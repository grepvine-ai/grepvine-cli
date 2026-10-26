package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

type searchOptions struct {
	fixed      bool
	json       bool
	ignoreCase bool
	wholeWord  bool
	multiline  bool
	color      string
	filter     string
	pageSize   int
	maxResults int
	indexRef   string
	// before and after are the context lines printed around each match, as
	// grep's -B and -A. context is -C, which sets whichever of the two was
	// not given on its own.
	before  int
	after   int
	context int
}

// maxContextLines is the most lines the API returns before or after a match.
const maxContextLines = 20

func newSearchCommand(a *app) *cobra.Command {
	var options searchOptions
	command := &cobra.Command{
		Use:   "search ORGANIZATION/INDEX PATTERN",
		Short: "Search an index",
		Args:  cobra.RangeArgs(1, 3),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, pattern, err := searchArgumentsWithIndex(args, options.indexRef)
			if err != nil {
				return err
			}
			flags := command.Flags()
			if flags.Changed("context") {
				if !flags.Changed("before-context") {
					options.before = options.context
				}
				if !flags.Changed("after-context") {
					options.after = options.context
				}
			}
			matches, err := a.search(command.Context(), organization, index, pattern, options)
			if err != nil {
				return err
			}
			if matches == 0 {
				a.exitCode = 1
			}
			return nil
		},
	}
	flags := command.Flags()
	flags.BoolVarP(&options.fixed, "fixed-strings", "F", false, "interpret the pattern as a literal string")
	flags.BoolVarP(&options.ignoreCase, "ignore-case", "i", false, "ignore case distinctions")
	flags.BoolVarP(&options.wholeWord, "word-regexp", "w", false, "match whole Unicode words (requires -F)")
	flags.BoolVarP(&options.multiline, "multiline", "U", false, "let a match span lines")
	flags.StringVar(&options.filter, "filter", "", "metadata filter expression")
	flags.StringVar(&options.indexRef, "index", "", "index as ORGANIZATION/INDEX")
	flags.IntVarP(&options.after, "after-context", "A", 0, "print this many lines after each match (at most 20)")
	flags.IntVarP(&options.before, "before-context", "B", 0, "print this many lines before each match (at most 20)")
	flags.IntVarP(&options.context, "context", "C", 0, "print this many lines before and after each match (at most 20)")
	flags.IntVar(&options.pageSize, "amount", 200, "results requested per page")
	flags.IntVar(&options.maxResults, "max-results", 0, "stop after this many results (zero means all)")
	flags.BoolVar(&options.json, "json", false, "print one JSON object per result instead of text")
	flags.StringVar(&options.color, "color", "auto", "colorize output: auto, always or never (auto colors a terminal unless NO_COLOR is set)")
	return command
}

func searchArgumentsWithIndex(args []string, indexRef string) (string, string, string, error) {
	if indexRef == "" {
		return searchArguments(args)
	}
	if len(args) != 1 {
		return "", "", "", errors.New("with --index, pass exactly one PATTERN")
	}
	organization, index, err := parseIndexReference(indexRef)
	return organization, index, args[0], err
}

func searchArguments(args []string) (string, string, string, error) {
	if len(args) == 3 {
		return args[0], args[1], args[2], nil
	}
	organization, index, err := parseIndexReference(args[0])
	return organization, index, args[1], err
}

func (a *app) search(ctx context.Context, organization, index, pattern string, options searchOptions) (int, error) {
	if options.wholeWord && !options.fixed {
		return 0, fmt.Errorf("-w requires literal mode (-F)")
	}
	if options.pageSize < 1 || options.pageSize > 200 {
		return 0, fmt.Errorf("amount must be between 1 and 200")
	}
	if options.before < 0 || options.before > maxContextLines || options.after < 0 || options.after > maxContextLines {
		return 0, fmt.Errorf("context lines must be between 0 and %d", maxContextLines)
	}
	var printer *searchPrinter
	if !options.json {
		var err error
		if printer, err = a.newSearchPrinter(options); err != nil {
			return 0, err
		}
		// The last result's trailing context is held until the next result or
		// the end, whichever comes first, including an end forced by --max-results
		// or an error.
		defer printer.finish()
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return 0, err
	}
	mode := httpapi.QueryRequestModeRegex
	if options.fixed {
		mode = httpapi.QueryRequestModeLiteral
	}
	request := &httpapi.QueryRequest{
		Pattern:       pattern,
		Mode:          httpapi.NewOptQueryRequestMode(mode),
		CaseSensitive: httpapi.NewOptBool(!options.ignoreCase),
		WholeWord:     httpapi.NewOptBool(options.wholeWord),
		Multiline:     httpapi.NewOptBool(options.multiline),
	}
	if options.filter != "" {
		request.Filter = httpapi.NewOptString(options.filter)
	}
	if options.before > 0 {
		request.LinesBefore = httpapi.NewOptInt(options.before)
	}
	if options.after > 0 {
		request.LinesAfter = httpapi.NewOptInt(options.after)
	}
	params := httpapi.QueryIndexParams{
		Organization: httpapi.Name(organization),
		Index:        httpapi.IndexName(index),
		Amount:       httpapi.NewOptInt(options.pageSize),
	}
	total := 0
	var throttledWait time.Duration
	for {
		if err := a.awaitThrottle(ctx); err != nil {
			return total, err
		}
		page, err := client.QueryIndex(ctx, request, params)
		if err != nil {
			if again, throttleErr := a.throttled(ctx, err, &throttledWait); throttleErr != nil {
				return total, fmt.Errorf("search: %w", throttleErr)
			} else if again {
				continue
			}
			return total, fmt.Errorf("search: %w", friendlyAPIError(err))
		}
		for _, result := range page.Results {
			if options.json {
				if err := writeJSONLine(a.stdout, &result); err != nil {
					return total, err
				}
			} else {
				printer.print(result)
			}
			total++
			if options.maxResults > 0 && total >= options.maxResults {
				return total, nil
			}
		}
		if !page.Pagination.HasNext {
			return total, nil
		}
		params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
	}
}
