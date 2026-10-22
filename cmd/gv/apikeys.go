package main

import (
	"fmt"
	"text/tabwriter"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

func newKeyCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "key",
		Short: "List, create and revoke your API keys",
	}
	command.AddCommand(
		newKeyListCommand(a),
		newKeyCreateCommand(a),
		newKeyRemoveCommand(a),
	)
	return command
}

func newKeyListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List your API keys",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			if !asJSON {
				fmt.Fprintln(table, "ID\tNAME\tPREFIX\tCREATED\tLAST USED\tREVOKED")
			}
			var params httpapi.ListAPIKeysParams
			for {
				page, err := client.ListAPIKeys(command.Context(), params)
				if err != nil {
					return fmt.Errorf("list API keys: %w", friendlyAPIError(err))
				}
				for i := range page.Results {
					key := &page.Results[i]
					if asJSON {
						if err := writeJSONLine(a.stdout, key); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\n", key.ID, key.Name, key.Prefix, formatTime(key.CreatedAt), optionalTime(key.LastUsedAt), optionalTime(key.RevokedAt))
				}
				if err := table.Flush(); err != nil {
					return err
				}
				if !page.Pagination.HasNext {
					return nil
				}
				params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
			}
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per key instead of a table")
	return command
}

func optionalTime(value httpapi.OptDateTime) string {
	if t, ok := value.Get(); ok {
		return formatTime(t)
	}
	return "-"
}

func newKeyCreateCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "create NAME",
		Short: "Create an API key and print it",
		Long: `Create an API key and print it on standard output, so that
"gv key create ci > ci.key" saves it. The key is shown only once.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			created, err := client.CreateAPIKey(command.Context(), &httpapi.CreateAPIKeyRequest{Name: args[0]}, httpapi.CreateAPIKeyParams{})
			if err != nil {
				return fmt.Errorf("create API key: %w", friendlyAPIError(err))
			}
			fmt.Fprintf(a.stderr, "Created API key %s (%s). Store it now; it is not shown again.\n", created.Name, created.ID)
			fmt.Fprintln(a.stdout, created.Key)
			return nil
		},
	}
}

func newKeyRemoveCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rm ID",
		Aliases: []string{"remove", "delete", "revoke"},
		Short:   "Revoke an API key",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil {
				return fmt.Errorf("API key ID %q is not valid; `gv key ls` lists the IDs", args[0])
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			response, err := client.RevokeAPIKey(command.Context(), httpapi.RevokeAPIKeyParams{APIKeyID: id})
			if err != nil {
				return fmt.Errorf("revoke API key: %w", friendlyAPIError(err))
			}
			switch value := response.(type) {
			case *httpapi.RevokeAPIKeyNoContent:
				fmt.Fprintf(a.stdout, "Revoked API key %s\n", id)
				return nil
			case *httpapi.NotFoundHeaders:
				return responseError(value.Response, value.XRequestID)
			default:
				return fmt.Errorf("revoke API key: unexpected response %T", response)
			}
		},
	}
}
