package main

import (
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// memberRoles are the roles a member can be given. An organization has one
// owner, which only `gv org transfer` changes.
var memberRoles = []string{"admin", "developer", "reader"}

func checkMemberRole(role string) error {
	if !slices.Contains(memberRoles, role) {
		return fmt.Errorf("role must be admin, developer or reader, not %q", role)
	}
	return nil
}

func newMemberCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "member",
		Short: "List, add, change and remove organization members",
	}
	command.AddCommand(
		newMemberListCommand(a),
		newMemberAddCommand(a),
		newMemberSetRoleCommand(a),
		newMemberRemoveCommand(a),
	)
	return command
}

func newMemberListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls ORGANIZATION",
		Aliases: []string{"list"},
		Short:   "List the members of an organization and their roles",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			if !asJSON {
				fmt.Fprintln(table, "USER\tEMAIL\tROLE\tADDED")
			}
			params := httpapi.ListOrganizationMembersParams{Organization: httpapi.Name(args[0])}
			for {
				page, err := client.ListOrganizationMembers(command.Context(), params)
				if err != nil {
					return fmt.Errorf("list members: %w", friendlyAPIError(err))
				}
				for i := range page.Results {
					member := &page.Results[i]
					if asJSON {
						if err := writeJSONLine(a.stdout, member); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", member.User.Name, member.User.Email, member.Role, formatTime(member.CreatedAt))
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
	command.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per member instead of a table")
	return command
}

func newMemberAddCommand(a *app) *cobra.Command {
	var role string
	command := &cobra.Command{
		Use:   "add ORGANIZATION USER --role ROLE",
		Short: "Add a user to an organization",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			if err := checkMemberRole(role); err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			request := &httpapi.AddMemberRequest{User: httpapi.Name(args[1]), Role: httpapi.AddMemberRequestRole(role)}
			member, err := client.AddOrganizationMember(command.Context(), request, httpapi.AddOrganizationMemberParams{Organization: httpapi.Name(args[0])})
			if err != nil {
				return fmt.Errorf("add member: %w", friendlyAPIError(err))
			}
			fmt.Fprintf(a.stdout, "Added %s to %s as %s\n", member.User.Name, args[0], member.Role)
			return nil
		},
	}
	command.Flags().StringVar(&role, "role", "", "the member's role: admin, developer or reader")
	_ = command.MarkFlagRequired("role")
	return command
}

func newMemberSetRoleCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "set-role ORGANIZATION USER ROLE",
		Short: "Change a member's role to admin, developer or reader",
		Args:  cobra.ExactArgs(3),
		RunE: func(command *cobra.Command, args []string) error {
			organization, user, role := args[0], args[1], args[2]
			if err := checkMemberRole(role); err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			request := &httpapi.UpdateMemberRequest{Role: httpapi.UpdateMemberRequestRole(role)}
			member, err := client.UpdateOrganizationMember(command.Context(), request, httpapi.UpdateOrganizationMemberParams{Organization: httpapi.Name(organization), User: httpapi.Name(user)})
			if err != nil {
				return fmt.Errorf("change member role: %w", friendlyAPIError(err))
			}
			fmt.Fprintf(a.stdout, "%s is now %s in %s\n", member.User.Name, member.Role, organization)
			return nil
		},
	}
}

func newMemberRemoveCommand(a *app) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:     "rm ORGANIZATION USER",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a member from an organization",
		Long: `Remove a member from an organization. The owner cannot be removed.

Without --force, gv asks you to type the user name to confirm.`,
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, user := args[0], args[1]
			if !force {
				prompt := fmt.Sprintf("This removes %s from %s.\nType the user name to confirm: ", user, organization)
				if err := a.confirm(prompt, user, "not removed"); err != nil {
					return err
				}
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			if err := client.RemoveOrganizationMember(command.Context(), httpapi.RemoveOrganizationMemberParams{Organization: httpapi.Name(organization), User: httpapi.Name(user)}); err != nil {
				return fmt.Errorf("remove member: %w", friendlyAPIError(err))
			}
			fmt.Fprintf(a.stdout, "Removed %s from %s\n", user, organization)
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "remove without asking for confirmation")
	return command
}
