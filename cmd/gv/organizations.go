// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

func newWhoamiCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:   "whoami",
		Short: "Show the signed-in user",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			response, err := client.GetCurrentUser(command.Context())
			if err != nil {
				return fmt.Errorf("get user: %w", friendlyAPIError(err))
			}
			switch value := response.(type) {
			case *httpapi.GetCurrentUserOKHeaders:
				if asJSON {
					return writeJSONLine(a.stdout, &value.Response)
				}
				fmt.Fprintf(a.stdout, "%s <%s>\n", value.Response.Name, value.Response.Email)
				return nil
			case *httpapi.UnauthorizedHeaders:
				return responseError(value.Response, value.XRequestID)
			default:
				return fmt.Errorf("get user: unexpected response %T", response)
			}
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the user as JSON")
	return command
}

// An API key cannot create an organization or transfer its ownership: only a
// person signed in to the console can, so gv leaves both to the console.
func newOrgCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "org",
		Short: "List, inspect and delete organizations; create and transfer them in the console",
		Long: `List, inspect and delete organizations.

Create an organization and transfer its ownership in the console.`,
	}
	command.AddCommand(
		newOrgListCommand(a),
		newOrgInfoCommand(a),
		newOrgRemoveCommand(a),
	)
	return command
}

func newOrgListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the organizations you belong to",
		Args:    cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			if !asJSON {
				fmt.Fprintln(table, "ORGANIZATION\tROLE\tCREATED")
			}
			var params httpapi.ListOrganizationsParams
			for {
				page, err := client.ListOrganizations(command.Context(), params)
				if err != nil {
					return fmt.Errorf("list organizations: %w", friendlyAPIError(err))
				}
				for i := range page.Results {
					organization := &page.Results[i]
					if asJSON {
						if err := writeJSONLine(a.stdout, organization); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintf(table, "%s\t%s\t%s\n", organization.Name, organization.Role, formatTime(organization.CreatedAt))
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
	command.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per organization instead of a table")
	return command
}

func newOrgInfoCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "info [ORGANIZATION]",
		Aliases: []string{"show", "get"},
		Short:   "Show an organization",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, err := a.organizationArgument(args)
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			response, err := client.GetOrganization(command.Context(), httpapi.GetOrganizationParams{Organization: httpapi.Name(organization)})
			if err != nil {
				return fmt.Errorf("get organization: %w", friendlyAPIError(err))
			}
			switch value := response.(type) {
			case *httpapi.Organization:
				return a.printOrganization(value, asJSON)
			case *httpapi.NotFoundHeaders:
				return responseError(value.Response, value.XRequestID)
			default:
				return fmt.Errorf("get organization: unexpected response %T", response)
			}
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the organization as JSON")
	return command
}

func (a *app) printOrganization(organization *httpapi.Organization, asJSON bool) error {
	if asJSON {
		return writeJSONLine(a.stdout, organization)
	}
	table := tabwriter.NewWriter(a.stdout, 0, 8, 1, ' ', 0)
	fmt.Fprintf(table, "Organization:\t%s\n", organization.Name)
	fmt.Fprintf(table, "ID:\t%s\n", organization.ID)
	fmt.Fprintf(table, "Your role:\t%s\n", organization.Role)
	fmt.Fprintf(table, "Created:\t%s\n", formatTime(organization.CreatedAt))
	return table.Flush()
}

func newOrgRemoveCommand(a *app) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:     "rm ORGANIZATION",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an organization with all of its indexes and documents",
		Long: `Delete an organization with all of its indexes and documents. This cannot be undone.

Without --force, gv asks you to type the organization name to confirm.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization := args[0]
			if !force {
				prompt := fmt.Sprintf("This deletes %s with all of its indexes and documents. It cannot be undone.\nType the organization name to confirm: ", organization)
				if err := a.confirm(prompt, organization, "not deleted"); err != nil {
					return err
				}
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			response, err := client.DeleteOrganization(command.Context(), httpapi.DeleteOrganizationParams{Organization: httpapi.Name(organization)})
			if err != nil {
				return fmt.Errorf("delete organization: %w", friendlyAPIError(err))
			}
			switch value := response.(type) {
			case *httpapi.DeleteOrganizationAccepted:
				fmt.Fprintf(a.stdout, "Deleting organization %s\n", organization)
				return nil
			case *httpapi.NotFoundHeaders:
				return responseError(value.Response, value.XRequestID)
			default:
				return fmt.Errorf("delete organization: unexpected response %T", response)
			}
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "delete without asking for confirmation")
	return command
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}
