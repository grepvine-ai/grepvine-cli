package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	ogenhttp "github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

type getDocumentOptions struct {
	byteRange string
	output    string
}

func newGetDocumentCommand(a *app) *cobra.Command {
	var options getDocumentOptions
	command := &cobra.Command{
		Use:   "get ORGANIZATION/INDEX KEY",
		Short: "Download a document",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := a.parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.getDocument(command.Context(), organization, index, args[1], options)
		},
	}
	command.Flags().StringVar(&options.byteRange, "range", "", "HTTP byte range, for example bytes=0-1023")
	command.Flags().StringVarP(&options.output, "output", "o", "", "write to a file instead of standard output")
	return command
}

func (a *app) getDocument(ctx context.Context, organization, index, key string, options getDocumentOptions) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	params := httpapi.GetDocumentParams{
		Organization: httpapi.Name(organization),
		Index:        httpapi.IndexName(index),
		Key:          key,
	}
	if options.byteRange != "" {
		params.Range = httpapi.NewOptString(options.byteRange)
	}
	response, err := client.GetDocument(ctx, params)
	if err != nil {
		return fmt.Errorf("get document: %w", friendlyAPIError(err))
	}
	var content io.Reader
	switch value := response.(type) {
	case *httpapi.GetDocumentOKHeaders:
		content = value.Response
	case *httpapi.GetDocumentPartialContentHeaders:
		content = value.Response
	case *httpapi.NotFoundHeaders:
		return responseError(value.Response, value.XRequestID)
	case *httpapi.ErrorHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("get document: unexpected response %T", response)
	}

	destination := a.stdout
	var file *os.File
	if options.output != "" && options.output != "-" {
		file, err = os.Create(options.output)
		if err != nil {
			return fmt.Errorf("create output: %w", err)
		}
		defer file.Close()
		destination = file
	}
	if _, err := io.Copy(destination, content); err != nil {
		return fmt.Errorf("write document: %w", err)
	}
	return nil
}

type listDocumentsOptions struct {
	prefix    string
	delimiter string
	amount    int
	json      bool
}

func newListDocumentsCommand(a *app) *cobra.Command {
	var options listDocumentsOptions
	command := &cobra.Command{
		Use:   "list ORGANIZATION/INDEX",
		Short: "List documents and common prefixes",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := a.parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.listDocuments(command.Context(), organization, index, options)
		},
	}
	command.Flags().StringVar(&options.prefix, "prefix", "", "only list keys with this prefix")
	command.Flags().StringVar(&options.delimiter, "delimiter", "", "fold keys into common prefixes at this delimiter")
	command.Flags().IntVar(&options.amount, "amount", 200, "entries requested per page")
	command.Flags().BoolVar(&options.json, "json", false, "print one JSON object per prefix or document")
	return command
}

func (a *app) listDocuments(ctx context.Context, organization, index string, options listDocumentsOptions) error {
	if options.amount < 1 || options.amount > 200 {
		return fmt.Errorf("amount must be between 1 and 200")
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	params := httpapi.ListDocumentsParams{
		Organization: httpapi.Name(organization),
		Index:        httpapi.IndexName(index),
		Amount:       httpapi.NewOptInt(options.amount),
	}
	if options.prefix != "" {
		params.Prefix = httpapi.NewOptString(options.prefix)
	}
	if options.delimiter != "" {
		params.Delimiter = httpapi.NewOptString(options.delimiter)
	}
	for {
		page, err := client.ListDocuments(ctx, params)
		if err != nil {
			return fmt.Errorf("list documents: %w", friendlyAPIError(err))
		}
		if options.json {
			for _, prefix := range page.CommonPrefixes {
				if err := writeJSONLine(a.stdout, map[string]string{"prefix": prefix}); err != nil {
					return err
				}
			}
			for i := range page.Results {
				if err := writeJSONLine(a.stdout, &page.Results[i]); err != nil {
					return err
				}
			}
		} else {
			entries := append([]string(nil), page.CommonPrefixes...)
			for _, document := range page.Results {
				entries = append(entries, document.Key)
			}
			sort.Strings(entries)
			for _, entry := range entries {
				fmt.Fprintln(a.stdout, entry)
			}
		}
		if !page.Pagination.HasNext {
			return nil
		}
		params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
	}
}

type putDocumentOptions struct {
	metadata []string
	ifMatch  string
	ifAbsent bool
	json     bool
}

func newPutDocumentCommand(a *app) *cobra.Command {
	var options putDocumentOptions
	command := &cobra.Command{
		Use:   "put ORGANIZATION/INDEX KEY FILE",
		Short: "Store one document from a file, or from standard input with -",
		Long: `Store one document from a file, or from standard input with -.

Each --metadata flag sets one field: NAME=VALUE stores a string, and
NAME:TYPE=VALUE stores a value of TYPE, which is string, integer, number,
boolean or timestamp (RFC 3339).`,
		Args: cobra.ExactArgs(3),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := a.parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.putDocument(command.Context(), organization, index, args[1], args[2], options)
		},
	}
	command.Flags().StringArrayVarP(&options.metadata, "metadata", "m", nil, "metadata field NAME=VALUE or NAME:TYPE=VALUE; repeat for more")
	command.Flags().StringVar(&options.ifMatch, "if-match", "", "store only if the current revision matches")
	command.Flags().BoolVar(&options.ifAbsent, "if-absent", false, "store only if the key holds no document")
	command.Flags().BoolVar(&options.json, "json", false, "print the result as JSON")
	return command
}

func (a *app) putDocument(ctx context.Context, organization, index, key, source string, options putDocumentOptions) error {
	if options.ifMatch != "" && options.ifAbsent {
		return errors.New("use --if-match or --if-absent, not both")
	}
	metadata, err := parseMetadata(options.metadata)
	if err != nil {
		return err
	}
	content := ogenhttp.MultipartFile{Name: path.Base(key), File: a.stdin}
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return fmt.Errorf("open document: %w", err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return fmt.Errorf("inspect document: %w", err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory; use gv upload --recursive", source)
		}
		if info.Size() > maxDocumentSize {
			return fmt.Errorf("%s exceeds the 1 GiB document limit", source)
		}
		content.File, content.Size = file, info.Size()
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	form := &httpapi.PutDocumentFormMultipart{Key: key, Content: content}
	if metadata != "" {
		form.Metadata = httpapi.NewOptString(metadata)
	}
	params := httpapi.PutDocumentParams{
		Organization:   httpapi.Name(organization),
		Index:          httpapi.IndexName(index),
		IdempotencyKey: uuid.NewString(),
	}
	if options.ifMatch != "" {
		params.IfMatch = httpapi.NewOptString(options.ifMatch)
	}
	if options.ifAbsent {
		params.IfNoneMatch = httpapi.NewOptString("*")
	}
	response, err := client.PutDocument(ctx, form, params)
	if err != nil {
		return fmt.Errorf("put document: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.MutationResult:
		if options.json {
			return writeJSONLine(a.stdout, value)
		}
		revision, _ := value.Revision.Get()
		fmt.Fprintf(a.stdout, "%s\t%s\n", value.Key, revision)
		return nil
	case *httpapi.ConflictHeaders:
		return responseError(value.Response, value.XRequestID)
	case *httpapi.PreconditionFailedHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("put document: unexpected response %T", response)
	}
}

// parseMetadata turns NAME=VALUE and NAME:TYPE=VALUE flags into the JSON
// metadata object a stored document carries, or "" when there are none.
func parseMetadata(fields []string) (string, error) {
	if len(fields) == 0 {
		return "", nil
	}
	metadata := map[string]any{}
	for _, field := range fields {
		name, text, ok := strings.Cut(field, "=")
		if !ok || name == "" {
			return "", fmt.Errorf("metadata %q must be NAME=VALUE or NAME:TYPE=VALUE", field)
		}
		kind := "string"
		if before, after, typed := strings.Cut(name, ":"); typed {
			name, kind = before, after
		}
		var value any
		var err error
		switch kind {
		case "string":
			value = text
		case "integer":
			value, err = strconv.ParseInt(text, 10, 64)
		case "number":
			var number float64
			number, err = strconv.ParseFloat(text, 64)
			if err == nil && (math.IsNaN(number) || math.IsInf(number, 0)) {
				err = errors.New("not finite")
			}
			value = number
		case "boolean":
			value, err = strconv.ParseBool(text)
		case "timestamp":
			_, err = time.Parse(time.RFC3339Nano, text)
			value = text
		default:
			return "", fmt.Errorf("metadata type %q must be string, integer, number, boolean or timestamp", kind)
		}
		if err != nil {
			return "", fmt.Errorf("metadata %s: %q is not a valid %s", name, text, kind)
		}
		metadata[name] = map[string]any{"type": kind, "value": value}
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	return string(encoded), nil
}

type deleteDocumentOptions struct {
	ifMatch string
	json    bool
}

func newDeleteDocumentCommand(a *app) *cobra.Command {
	var options deleteDocumentOptions
	command := &cobra.Command{
		Use:   "delete ORGANIZATION/INDEX KEY...",
		Short: "Delete one or more documents",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := a.parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.deleteDocuments(command.Context(), organization, index, args[1:], options)
		},
	}
	command.Flags().StringVar(&options.ifMatch, "if-match", "", "delete only if the revision matches; needs exactly one KEY")
	command.Flags().BoolVar(&options.json, "json", false, "print one JSON result per document")
	return command
}

// deleteDocuments deletes keys in batches. Each document is deleted on its
// own: one the index did not delete is reported, the others are deleted, and
// the command then fails with a count of those it did not delete.
func (a *app) deleteDocuments(ctx context.Context, organization, index string, keys []string, options deleteDocumentOptions) error {
	if options.ifMatch != "" && len(keys) > 1 {
		return errors.New("--if-match takes a single KEY")
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	var rejected int64
	for start := 0; start < len(keys); start += deleteBatchKeys {
		batch := keys[start:min(start+deleteBatchKeys, len(keys))]
		records := make([]httpapi.DeleteObjectRecord, len(batch))
		for i, key := range batch {
			records[i] = httpapi.DeleteObjectRecord{Key: key}
			if options.ifMatch != "" {
				records[i].IfMatch = httpapi.NewOptString(options.ifMatch)
			}
		}
		response, err := client.DeleteObjects(ctx, &httpapi.DeleteObjectsRequest{Objects: records}, httpapi.DeleteObjectsParams{
			Organization:   httpapi.Name(organization),
			Index:          httpapi.IndexName(index),
			IdempotencyKey: uuid.NewString(),
		})
		if err != nil {
			return fmt.Errorf("delete documents: %w", friendlyAPIError(err))
		}
		for i := range response.Results {
			result := &response.Results[i]
			if reason, failed := result.Error.Get(); failed {
				rejected++
				fmt.Fprintf(a.stderr, "gv: %s was not deleted: %s (%s)\n", result.Key, reason.Error, reason.Code)
			}
			if options.json {
				if err := writeJSONLine(a.stdout, result); err != nil {
					return err
				}
			} else if !result.Error.IsSet() {
				revision, _ := result.Revision.Get()
				fmt.Fprintf(a.stdout, "%s\t%s\n", result.Key, revision)
			}
		}
	}
	if rejected > 0 {
		return &rejectedError{count: rejected, action: "deleted"}
	}
	return nil
}

// parseIndexReference reads ORGANIZATION/INDEX, or a bare INDEX in the
// default organization.
func (a *app) parseIndexReference(value string) (string, string, error) {
	organization, index, ok := strings.Cut(value, "/")
	if !ok {
		cfg, _, err := a.endpointAndKey()
		if err != nil {
			return "", "", err
		}
		if cfg.Organization == "" {
			return "", "", errors.New("index must be ORGANIZATION/INDEX, or set a default organization to pass only INDEX")
		}
		organization, index = cfg.Organization, value
	}
	if organization == "" || index == "" || strings.Contains(index, "/") {
		return "", "", errors.New("index must be ORGANIZATION/INDEX")
	}
	return organization, index, nil
}

// organizationArgument returns the ORGANIZATION argument, or the default
// organization when it is omitted.
func (a *app) organizationArgument(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	cfg, _, err := a.endpointAndKey()
	if err != nil {
		return "", err
	}
	if cfg.Organization == "" {
		return "", errors.New("pass ORGANIZATION, or set a default with `organization` in the configuration file or GREPVINE_ORGANIZATION")
	}
	return cfg.Organization, nil
}

func friendlyAPIError(err error) error {
	if response, ok := errors.AsType[*httpapi.ErrorStatusCodeWithHeaders](err); ok {
		return publicAPIError(response)
	}
	return err
}

func responseError(response httpapi.Error, requestID httpapi.OptString) error {
	id, _ := requestID.Get()
	if id != "" {
		return fmt.Errorf("%s (%s; request %s)", response.Error, response.Code, id)
	}
	return fmt.Errorf("%s (%s)", response.Error, response.Code)
}
