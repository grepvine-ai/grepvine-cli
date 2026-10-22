package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// maxDescriptionBytes mirrors the contract's IndexDescription limit, so a
// description read from a file is refused here with a clear message rather
// than by the service with a generic one.
const maxDescriptionBytes = 16384

// descriptionColumnWidth is how much of a description the listing shows:
// its first line, cut so the table stays inside a typical terminal.
const descriptionColumnWidth = 60

// Tag limits, mirroring the contract's IndexTags rules so a bad tag is refused
// here with a message that names it rather than by the service with a generic
// one.
const (
	maxTagCount      = 64
	maxTagKeyChars   = 128
	maxTagValueChars = 1024
)

func newIndexCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "index",
		Short: "List, inspect, create, describe, tag and delete indexes",
	}
	command.AddCommand(
		newIndexListCommand(a),
		newIndexInfoCommand(a),
		newIndexCreateCommand(a),
		newIndexUpdateCommand(a),
		newIndexRemoveCommand(a),
		newIndexTagCommand(a),
	)
	return command
}

type indexListOptions struct {
	json       bool
	prefix     string
	amount     int
	maxResults int
}

func newIndexListCommand(a *app) *cobra.Command {
	var options indexListOptions
	command := &cobra.Command{
		Use:     "ls ORGANIZATION",
		Aliases: []string{"list"},
		Short:   "List the indexes of an organization",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return a.listIndexes(command.Context(), args[0], options)
		},
	}
	command.Flags().BoolVar(&options.json, "json", false, "print one JSON object per index instead of a table")
	command.Flags().StringVar(&options.prefix, "prefix", "", "list only indexes whose names start with this prefix, in any case")
	command.Flags().IntVar(&options.amount, "amount", 200, "indexes requested per page")
	command.Flags().IntVar(&options.maxResults, "max-results", 0, "stop after this many indexes (zero means all)")
	return command
}

// listIndexes prints each page as it arrives rather than after the last one,
// so an organization with many indexes starts printing at once and
// --max-results stops the listing without reading the rest. The table is
// aligned one page at a time.
func (a *app) listIndexes(ctx context.Context, organization string, options indexListOptions) error {
	if options.amount < 1 || options.amount > 200 {
		return fmt.Errorf("amount must be between 1 and 200")
	}
	if options.maxResults < 0 {
		return fmt.Errorf("max-results must not be negative")
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	amount := options.amount
	if options.maxResults > 0 {
		amount = min(amount, options.maxResults)
	}
	params := httpapi.ListIndexesParams{Organization: httpapi.Name(organization), Amount: httpapi.NewOptInt(amount)}
	if options.prefix != "" {
		params.Prefix = httpapi.NewOptString(options.prefix)
	}
	table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
	if !options.json {
		fmt.Fprintln(table, "INDEX\tSTATE\tDOCUMENTS\tSIZE\tDESCRIPTION")
	}
	total := 0
	for {
		page, err := client.ListIndexes(ctx, params)
		if err != nil {
			return fmt.Errorf("list indexes: %w", friendlyAPIError(err))
		}
		for i := range page.Results {
			index := &page.Results[i]
			if options.json {
				if err := writeJSONLine(a.stdout, index); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(table, "%s/%s\t%s\t%d\t%s\t%s\n", index.Organization, index.Name, index.State, index.DocumentCount, formatSize(float64(index.ContentBytes)), descriptionSummary(string(index.Description), descriptionColumnWidth))
			}
			total++
			if options.maxResults > 0 && total >= options.maxResults {
				return table.Flush()
			}
		}
		if err := table.Flush(); err != nil {
			return err
		}
		if !page.Pagination.HasNext {
			return nil
		}
		params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
	}
}

type indexInfoOptions struct{ json bool }

func newIndexInfoCommand(a *app) *cobra.Command {
	var options indexInfoOptions
	command := &cobra.Command{
		Use:     "info ORGANIZATION/INDEX",
		Aliases: []string{"show", "get"},
		Short:   "Show an index, including its description",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.showIndex(command.Context(), organization, index, options)
		},
	}
	command.Flags().BoolVar(&options.json, "json", false, "print the index as JSON")
	return command
}

func (a *app) showIndex(ctx context.Context, organization, index string, options indexInfoOptions) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	value, err := fetchIndex(ctx, client, organization, index)
	if err != nil {
		return err
	}
	return a.printIndex(value, options.json)
}

// fetchIndex reads one index, turning the not-found answer into the error the
// other commands report.
func fetchIndex(ctx context.Context, client *httpapi.Client, organization, index string) (*httpapi.Index, error) {
	response, err := client.GetIndex(ctx, httpapi.GetIndexParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index)})
	if err != nil {
		return nil, fmt.Errorf("get index: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.Index:
		return value, nil
	case *httpapi.NotFoundHeaders:
		return nil, responseError(value.Response, value.XRequestID)
	default:
		return nil, fmt.Errorf("get index: unexpected response %T", response)
	}
}

// descriptionFlags are the two ways a command takes a description: inline,
// or from a file, where `-` is standard input. Markdown of any length is
// awkward on a command line, so the file form is the one a longer description
// uses.
type descriptionFlags struct {
	text string
	file string
}

func (f *descriptionFlags) register(command *cobra.Command) {
	command.Flags().StringVar(&f.text, "description", "", "the index description, in Markdown")
	command.Flags().StringVar(&f.file, "description-file", "", "read the description from this file, or from standard input with -")
}

// given reports whether either flag was used. An empty --description still
// counts: that is how a description is cleared.
func (f *descriptionFlags) given(command *cobra.Command) bool {
	return command.Flags().Changed("description") || command.Flags().Changed("description-file")
}

func (f *descriptionFlags) resolve(command *cobra.Command, stdin io.Reader) (string, error) {
	if command.Flags().Changed("description") && command.Flags().Changed("description-file") {
		return "", errors.New("use either --description or --description-file, not both")
	}
	if !command.Flags().Changed("description-file") {
		return validateDescription(f.text)
	}
	var content []byte
	var err error
	if f.file == "-" {
		content, err = io.ReadAll(stdin)
	} else {
		content, err = os.ReadFile(f.file)
	}
	if err != nil {
		return "", fmt.Errorf("read description: %w", err)
	}
	if !utf8.Valid(content) {
		return "", errors.New("the description file is not UTF-8 text")
	}
	return validateDescription(string(content))
}

func validateDescription(value string) (string, error) {
	if utf8.RuneCountInString(value) > maxDescriptionBytes {
		return "", fmt.Errorf("the description is longer than %d characters", maxDescriptionBytes)
	}
	return value, nil
}

func newIndexCreateCommand(a *app) *cobra.Command {
	var description descriptionFlags
	var tagFlags []string
	command := &cobra.Command{
		Use:   "create ORGANIZATION/INDEX",
		Short: "Create an index, optionally with a description and tags",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			request := &httpapi.CreateIndexRequest{Name: httpapi.IndexName(index)}
			if description.given(command) {
				value, err := description.resolve(command, a.stdin)
				if err != nil {
					return err
				}
				request.Description = httpapi.NewOptIndexDescription(httpapi.IndexDescription(value))
			}
			if len(tagFlags) > 0 {
				tags, err := parseTagAssignments(tagFlags)
				if err != nil {
					return err
				}
				request.Tags = httpapi.NewOptIndexTags(tags)
			}
			return a.createIndex(command.Context(), organization, request)
		},
	}
	description.register(command)
	command.Flags().StringArrayVar(&tagFlags, "tag", nil, "a KEY=VALUE tag; repeat for several")
	return command
}

func (a *app) createIndex(ctx context.Context, organization string, request *httpapi.CreateIndexRequest) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	response, err := client.CreateIndex(ctx, request, httpapi.CreateIndexParams{Organization: httpapi.Name(organization)})
	if err != nil {
		return fmt.Errorf("create index: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.Index:
		return a.printIndex(value, false)
	case *httpapi.ConflictHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("create index: unexpected response %T", response)
	}
}

func newIndexUpdateCommand(a *app) *cobra.Command {
	var description descriptionFlags
	command := &cobra.Command{
		Use:   "update ORGANIZATION/INDEX",
		Short: "Change an index's description",
		Long: `Change an index's description.

The description is Markdown. Pass it inline with --description, or from a
file with --description-file (use - for standard input). An empty
--description clears it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			if !description.given(command) {
				return errors.New("nothing to change; pass --description or --description-file")
			}
			value, err := description.resolve(command, a.stdin)
			if err != nil {
				return err
			}
			request := &httpapi.UpdateIndexRequest{Description: httpapi.NewOptIndexDescription(httpapi.IndexDescription(value))}
			return a.updateIndex(command.Context(), organization, index, request)
		},
	}
	description.register(command)
	return command
}

// updateIndex sends one change to an index and prints the result. The request
// carries only the fields that change; the service leaves the others alone.
func (a *app) updateIndex(ctx context.Context, organization, index string, request *httpapi.UpdateIndexRequest) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	response, err := client.UpdateIndex(ctx, request, httpapi.UpdateIndexParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index)})
	if err != nil {
		return fmt.Errorf("update index: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.Index:
		return a.printIndex(value, false)
	case *httpapi.NotFoundHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("update index: unexpected response %T", response)
	}
}

func newIndexRemoveCommand(a *app) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:     "rm ORGANIZATION/INDEX",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an index and all of its documents",
		Long: `Delete an index and all of its documents. This cannot be undone.

Without --force, gv asks you to type the index name to confirm.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			if !force {
				prompt := fmt.Sprintf("This deletes %s/%s and all of its documents. It cannot be undone.\nType the index name to confirm: ", organization, index)
				if err := a.confirm(prompt, index, "not deleted"); err != nil {
					return err
				}
			}
			return a.removeIndex(command.Context(), organization, index)
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "delete without asking for confirmation")
	return command
}

// confirm writes prompt to standard error and reads one line of standard
// input. Anything but expected, including no answer at all, refuses with an
// error that starts with refusal, such as "not deleted".
func (a *app) confirm(prompt, expected, refusal string) error {
	fmt.Fprint(a.stderr, prompt)
	answer, err := bufio.NewReader(a.stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read confirmation: %w", err)
	}
	if strings.TrimSpace(answer) != expected {
		if answer == "" {
			fmt.Fprintln(a.stderr)
		}
		return fmt.Errorf("%s; the name did not match (pass --force to skip the confirmation)", refusal)
	}
	return nil
}

func (a *app) removeIndex(ctx context.Context, organization, index string) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	response, err := client.DeleteIndex(ctx, httpapi.DeleteIndexParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index)})
	if err != nil {
		return fmt.Errorf("delete index: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.DeleteIndexAccepted:
		fmt.Fprintf(a.stdout, "Deleting %s/%s\n", organization, index)
		return nil
	case *httpapi.NotFoundHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("delete index: unexpected response %T", response)
	}
}

// printIndex writes one index as a block of labeled lines, with the
// description last and verbatim, so it can be piped into a file and edited.
func (a *app) printIndex(index *httpapi.Index, asJSON bool) error {
	if asJSON {
		return writeJSONLine(a.stdout, index)
	}
	table := tabwriter.NewWriter(a.stdout, 0, 8, 1, ' ', 0)
	fmt.Fprintf(table, "Index:\t%s/%s\n", index.Organization, index.Name)
	fmt.Fprintf(table, "State:\t%s\n", index.State)
	fmt.Fprintf(table, "Documents:\t%d\n", index.DocumentCount)
	fmt.Fprintf(table, "Size:\t%s\n", formatSize(float64(index.ContentBytes)))
	fmt.Fprintf(table, "Created:\t%s\n", index.CreatedAt.UTC().Format(time.RFC3339))
	if updated, ok := index.LastUpdatedAt.Get(); ok {
		fmt.Fprintf(table, "Last written:\t%s\n", updated.UTC().Format(time.RFC3339))
	}
	if percent, ok := index.OptimizationProgressPercent.Get(); ok {
		fmt.Fprintf(table, "Optimized:\t%d%%\n", percent)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if len(index.Tags) == 0 {
		fmt.Fprintln(a.stdout, "Tags: (none)")
	} else {
		fmt.Fprintln(a.stdout, "Tags:")
		for _, key := range sortedTagKeys(index.Tags) {
			fmt.Fprintf(a.stdout, "  %s=%s\n", key, index.Tags[key])
		}
	}
	if index.Description == "" {
		fmt.Fprintln(a.stdout, "Description: (none)")
		return nil
	}
	fmt.Fprintln(a.stdout, "Description:")
	for line := range strings.SplitSeq(strings.TrimRight(string(index.Description), "\n"), "\n") {
		if line == "" {
			fmt.Fprintln(a.stdout)
			continue
		}
		fmt.Fprintf(a.stdout, "  %s\n", line)
	}
	return nil
}

func writeJSONLine(w io.Writer, value json.Marshaler) error {
	encoded, err := value.MarshalJSON()
	if err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
}

// descriptionSummary is the first non-empty line of a description, with
// Markdown heading marks removed and cut to width runes, for a table cell.
func descriptionSummary(description string, width int) string {
	for line := range strings.SplitSeq(description, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" {
			continue
		}
		if utf8.RuneCountInString(line) <= width {
			return line
		}
		runes := []rune(line)
		return strings.TrimSpace(string(runes[:width-1])) + "…"
	}
	return ""
}

// newIndexTagCommand groups the tag operations. Tags are key/value labels on
// an index; `gv sync` records the commit it synced in git.commit and
// git.branch, and anything else is whatever the organization finds useful.
func newIndexTagCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "tag",
		Short: "List, set and remove an index's tags",
	}
	command.AddCommand(
		newIndexTagListCommand(a),
		newIndexTagSetCommand(a),
		newIndexTagRemoveCommand(a),
	)
	return command
}

func newIndexTagListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls ORGANIZATION/INDEX",
		Aliases: []string{"list"},
		Short:   "List an index's tags",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.listTags(command.Context(), organization, index, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the tags as one JSON object")
	return command
}

func (a *app) listTags(ctx context.Context, organization, index string, asJSON bool) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	value, err := fetchIndex(ctx, client, organization, index)
	if err != nil {
		return err
	}
	if asJSON {
		encoded, err := json.Marshal(map[string]string(value.Tags))
		if err != nil {
			return fmt.Errorf("encode tags: %w", err)
		}
		_, err = fmt.Fprintf(a.stdout, "%s\n", encoded)
		return err
	}
	table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(table, "KEY\tVALUE")
	for _, key := range sortedTagKeys(value.Tags) {
		fmt.Fprintf(table, "%s\t%s\n", key, value.Tags[key])
	}
	return table.Flush()
}

func newIndexTagSetCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set ORGANIZATION/INDEX KEY=VALUE [KEY=VALUE...]",
		Short: "Set tags on an index, leaving the others as they are",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			tags, err := parseTagAssignments(args[1:])
			if err != nil {
				return err
			}
			changes := httpapi.IndexTagChanges{}
			for key, value := range tags {
				changes[key] = nilString(value)
			}
			return a.updateIndex(command.Context(), organization, index, &httpapi.UpdateIndexRequest{Tags: httpapi.NewOptIndexTagChanges(changes)})
		},
	}
}

func newIndexTagRemoveCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rm ORGANIZATION/INDEX KEY [KEY...]",
		Aliases: []string{"remove", "unset"},
		Short:   "Remove tags from an index",
		Args:    cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			changes := httpapi.IndexTagChanges{}
			for _, key := range args[1:] {
				if err := validateTagKey(key); err != nil {
					return err
				}
				changes[key] = nullString()
			}
			return a.updateIndex(command.Context(), organization, index, &httpapi.UpdateIndexRequest{Tags: httpapi.NewOptIndexTagChanges(changes)})
		},
	}
}

// parseTagAssignments turns KEY=VALUE arguments into tags. The first `=`
// splits, so a value may contain one.
func parseTagAssignments(values []string) (httpapi.IndexTags, error) {
	tags := httpapi.IndexTags{}
	for _, value := range values {
		key, tagValue, ok := strings.Cut(value, "=")
		if !ok {
			return nil, fmt.Errorf("tag %q must be KEY=VALUE", value)
		}
		if err := validateTagKey(key); err != nil {
			return nil, err
		}
		if err := validateTagValue(tagValue); err != nil {
			return nil, fmt.Errorf("tag %q: %w", key, err)
		}
		tags[key] = tagValue
	}
	if len(tags) > maxTagCount {
		return nil, fmt.Errorf("an index has at most %d tags", maxTagCount)
	}
	return tags, nil
}

func validateTagKey(key string) error {
	if key == "" || !utf8.ValidString(key) || utf8.RuneCountInString(key) > maxTagKeyChars {
		return fmt.Errorf("tag key %q must be 1 to %d characters", key, maxTagKeyChars)
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("tag key %q must not contain whitespace or control characters", key)
		}
	}
	return nil
}

func validateTagValue(value string) error {
	if !utf8.ValidString(value) || utf8.RuneCountInString(value) > maxTagValueChars {
		return fmt.Errorf("the value is longer than %d characters", maxTagValueChars)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return errors.New("the value contains a control character")
		}
	}
	return nil
}

func sortedTagKeys(tags httpapi.IndexTags) []string {
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func nilString(value string) httpapi.NilString {
	var v httpapi.NilString
	v.SetTo(value)
	return v
}

func nullString() httpapi.NilString {
	var v httpapi.NilString
	v.SetToNull()
	return v
}
