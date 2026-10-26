package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
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
			organization, index, err := parseIndexReference(args[0])
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
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.listDocuments(command.Context(), organization, index, options)
		},
	}
	command.Flags().StringVar(&options.prefix, "prefix", "", "only list keys with this prefix")
	command.Flags().StringVar(&options.delimiter, "delimiter", "", "fold keys into common prefixes at this delimiter")
	command.Flags().IntVar(&options.amount, "amount", 200, "entries requested per page")
	command.Flags().BoolVar(&options.json, "json", false, `print one JSON object per entry: a document, or {"prefix": ...} for a common prefix`)
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
				if err := writeJSONLine(a.stdout, prefixLine(prefix)); err != nil {
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

type deleteDocumentOptions struct{ ifMatch string }

func newDeleteDocumentCommand(a *app) *cobra.Command {
	var options deleteDocumentOptions
	command := &cobra.Command{
		Use:   "delete ORGANIZATION/INDEX KEY",
		Short: "Delete a document",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := parseIndexReference(args[0])
			if err != nil {
				return err
			}
			return a.deleteDocument(command.Context(), organization, index, args[1], options)
		},
	}
	command.Flags().StringVar(&options.ifMatch, "if-match", "", "delete only if the revision matches")
	return command
}

func (a *app) deleteDocument(ctx context.Context, organization, index, key string, options deleteDocumentOptions) error {
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	params := httpapi.DeleteDocumentParams{
		Organization:   httpapi.Name(organization),
		Index:          httpapi.IndexName(index),
		Key:            key,
		IdempotencyKey: uuid.NewString(),
	}
	if options.ifMatch != "" {
		params.IfMatch = httpapi.NewOptString(options.ifMatch)
	}
	response, err := client.DeleteDocument(ctx, params)
	if err != nil {
		return fmt.Errorf("delete document: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.MutationResult:
		revision, _ := value.Revision.Get()
		fmt.Fprintf(a.stdout, "%s\t%s\n", value.Key, revision)
		return nil
	case *httpapi.NotFoundHeaders:
		return responseError(value.Response, value.XRequestID)
	case *httpapi.PreconditionFailedHeaders:
		requestID, _ := value.XRequestID.Get()
		if requestID != "" {
			return fmt.Errorf("%s (%s; request %s)", value.Response.Error, value.Response.Code, requestID)
		}
		return fmt.Errorf("%s (%s)", value.Response.Error, value.Response.Code)
	default:
		return fmt.Errorf("delete document: unexpected response %T", response)
	}
}

func parseIndexReference(value string) (string, string, error) {
	organization, index, ok := strings.Cut(value, "/")
	if !ok || organization == "" || index == "" || strings.Contains(index, "/") {
		return "", "", fmt.Errorf("index must be ORGANIZATION/INDEX")
	}
	return organization, index, nil
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

// prefixLine is a common prefix in the JSON output of list.
type prefixLine string

func (p prefixLine) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"prefix": string(p)})
}
