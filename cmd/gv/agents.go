// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

func newAgentCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "agent",
		Short: "List, create, change and remove an organization's agents",
		Long: `An agent is a named identity for automation in one organization, such as a
CI job. Its ceiling, the indexes it reaches and its permissions, bounds what
it may do.

Listing and viewing agents works with any key of a member. Changing them needs
a key with the manage_agents permission (gv login --org ORGANIZATION
--permission manage_agents), and you must be one of the agent's managers or an
owner or admin.`,
	}
	command.AddCommand(
		newAgentListCommand(a),
		newAgentInfoCommand(a),
		newAgentCreateCommand(a),
		newAgentUpdateCommand(a),
		newAgentDisableCommand(a, true),
		newAgentDisableCommand(a, false),
		newAgentRemoveCommand(a),
		newAgentManagerCommand(a),
		newAgentSecretCommand(a),
	)
	return command
}

// agentAnswer returns the successful answer of an agent operation in
// organization, or an error that says what went wrong.
func agentAnswer[T any](organization, what string, response any, err error) (*T, error) {
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, friendlyAPIError(err))
	}
	hint := func(permission string) string { return agentPermissionHint(organization, permission) }
	switch value := response.(type) {
	case *T:
		return value, nil
	case *httpapi.NotFoundHeaders:
		return nil, responseErrorHint(value.Response, value.XRequestID, hint)
	case *httpapi.ForbiddenHeaders:
		return nil, responseErrorHint(value.Response, value.XRequestID, hint)
	case *httpapi.ConflictHeaders:
		return nil, responseErrorHint(value.Response, value.XRequestID, hint)
	default:
		return nil, fmt.Errorf("%s: unexpected response %T", what, response)
	}
}

// agentPermissionHint says how to get a key that can manage the agents of
// organization and has permission: a key holds every permission it gives an
// agent, and manage_agents needs one organization.
func agentPermissionHint(organization, permission string) string {
	if permission == "manage_agents" {
		return fmt.Sprintf("Sign in for a key that has it: gv login --org %s --permission manage_agents", organization)
	}
	return fmt.Sprintf("A key gives an agent only permissions it has itself. Sign in for one that has it: gv login --org %s --permission manage_agents --permission %s (repeat --permission for each one the agent needs)", organization, permission)
}

func agentParams(organization, agent string) (httpapi.Name, httpapi.Name) {
	return httpapi.Name(organization), httpapi.Name(agent)
}

func newAgentListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls [ORGANIZATION]",
		Aliases: []string{"list"},
		Short:   "List an organization's agents",
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
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			if !asJSON {
				fmt.Fprintln(table, "NAME\tSTATUS\tPERMISSIONS\tINDEXES\tLAST USED")
			}
			params := httpapi.ListAgentsParams{Organization: httpapi.Name(organization)}
			for {
				response, err := client.ListAgents(command.Context(), params)
				page, err := agentAnswer[httpapi.AgentPage](organization, "list agents", response, err)
				if err != nil {
					return err
				}
				for i := range page.Results {
					agent := &page.Results[i]
					if asJSON {
						if err := writeJSONLine(a.stdout, agent); err != nil {
							return err
						}
						continue
					}
					fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", agent.Name, agentStatus(agent), joinPermissions(agent.Permissions), describeIndexes(agent.Indexes), lastUsed(agent.LastUsedAt))
				}
				if !page.Pagination.HasNext {
					break
				}
				params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
			}
			return table.Flush()
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per agent instead of a table")
	return command
}

func newAgentInfoCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "info ORGANIZATION/AGENT",
		Aliases: []string{"get"},
		Short:   "Show an agent: its ceiling, managers and status",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			agent, err := fetchAgent(command.Context(), client, organization, name)
			if err != nil {
				return err
			}
			return a.printAgent(organization, agent, asJSON)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print the agent as JSON")
	return command
}

func fetchAgent(ctx context.Context, client *httpapi.Client, organization, agent string) (*httpapi.Agent, error) {
	org, name := agentParams(organization, agent)
	response, err := client.GetAgent(ctx, httpapi.GetAgentParams{Organization: org, Agent: name})
	return agentAnswer[httpapi.Agent](organization, "get agent", response, err)
}

// ceilingFlags are an agent's ceiling as flags: its permissions and the
// indexes it reaches.
type ceilingFlags struct {
	permissions []string
	indexes     []string
	allIndexes  bool
}

func (f *ceilingFlags) register(command *cobra.Command) {
	command.Flags().StringSliceVar(&f.permissions, "permission", nil, "search, read, write, create_index or manage_index; repeatable")
	command.Flags().StringSliceVar(&f.indexes, "index", nil, "an index the agent reaches, repeatable (default: every index, including ones created later)")
}

func (f *ceilingFlags) agentPermissions() (httpapi.AgentPermissions, error) {
	permissions := make(httpapi.AgentPermissions, 0, len(f.permissions))
	for _, permission := range f.permissions {
		switch permission {
		case "search", "read", "write", "create_index", "manage_index":
			permissions = append(permissions, httpapi.Permission(permission))
		default:
			return nil, fmt.Errorf("--permission %q is not one of search, read, write, create_index, manage_index", permission)
		}
	}
	return permissions, nil
}

func (f *ceilingFlags) indexScope() httpapi.IndexScope {
	if len(f.indexes) == 0 {
		return nil
	}
	scope := make(httpapi.IndexScope, 0, len(f.indexes))
	for _, index := range f.indexes {
		scope = append(scope, httpapi.IndexName(index))
	}
	return scope
}

// merge replaces the parts of current that the flags set. The service
// replaces a ceiling as a whole, so a part left out keeps its value.
func (f *ceilingFlags) merge(command *cobra.Command, current httpapi.AgentCeiling) (httpapi.AgentCeiling, error) {
	if command.Flags().Changed("permission") {
		permissions, err := f.agentPermissions()
		if err != nil {
			return current, err
		}
		current.Permissions = permissions
	}
	switch {
	case f.allIndexes:
		current.Indexes = nil
	case command.Flags().Changed("index"):
		current.Indexes = f.indexScope()
	}
	return current, nil
}

func newAgentCreateCommand(a *app) *cobra.Command {
	var ceiling ceilingFlags
	var description string
	command := &cobra.Command{
		Use:   "create ORGANIZATION/AGENT --permission PERMISSION",
		Short: "Create an agent; you become its first manager",
		Long: `Create an agent with a ceiling: the indexes it reaches and its permissions.
You can give it only permissions your own role has. An agent limited to some
indexes cannot have create_index.`,
		Example: `  gv agent create acme/ci --permission search --permission read --index docs --description "CI searches"`,
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			permissions, err := ceiling.agentPermissions()
			if err != nil {
				return err
			}
			request := &httpapi.CreateAgentRequest{Name: httpapi.Name(name), Indexes: ceiling.indexScope(), Permissions: permissions}
			if description != "" {
				request.Description = httpapi.NewOptAgentDescription(httpapi.AgentDescription(description))
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			response, err := client.CreateAgent(command.Context(), request, httpapi.CreateAgentParams{Organization: httpapi.Name(organization)})
			agent, err := agentAnswer[httpapi.Agent](organization, "create agent", response, err)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Created agent %s/%s: %s · %s\n", organization, agent.Name, joinPermissions(agent.Permissions), describeIndexes(agent.Indexes))
			return nil
		},
	}
	ceiling.register(command)
	command.Flags().StringVar(&description, "description", "", "what the agent is for")
	_ = command.MarkFlagRequired("permission")
	return command
}

func newAgentUpdateCommand(a *app) *cobra.Command {
	var ceiling ceilingFlags
	var description string
	command := &cobra.Command{
		Use:   "update ORGANIZATION/AGENT",
		Short: "Change an agent's description or ceiling",
		Long: `Change an agent's description or ceiling. --permission replaces its
permissions and --index its indexes; what you leave out is kept. A ceiling
must stay within your own permissions and within every manager's role.

Saving an agent marked "needs review" clears the mark, even with no change.`,
		Example: `  gv agent update acme/ci --permission search --permission read --permission write
  gv agent update acme/ci --all-indexes`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			if ceiling.allIndexes && command.Flags().Changed("index") {
				return errors.New("use either --index or --all-indexes, not both")
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			current, err := fetchAgent(command.Context(), client, organization, name)
			if err != nil {
				return err
			}
			// The ceiling is always sent, so an update with no flags saves the
			// agent as it is, which clears a review mark.
			merged, err := ceiling.merge(command, httpapi.AgentCeiling{Indexes: current.Indexes, Permissions: current.Permissions})
			if err != nil {
				return err
			}
			request := &httpapi.UpdateAgentRequest{Ceiling: httpapi.NewOptAgentCeiling(merged)}
			if command.Flags().Changed("description") {
				request.Description = httpapi.NewOptAgentDescription(httpapi.AgentDescription(description))
			}
			agent, err := a.updateAgent(command.Context(), client, organization, name, request)
			if err != nil {
				return err
			}
			return a.printAgent(organization, agent, false)
		},
	}
	ceiling.register(command)
	command.Flags().BoolVar(&ceiling.allIndexes, "all-indexes", false, "let the agent reach every index, including ones created later")
	command.Flags().StringVar(&description, "description", "", "what the agent is for; empty clears it")
	return command
}

func (a *app) updateAgent(ctx context.Context, client *httpapi.Client, organization, agent string, request *httpapi.UpdateAgentRequest) (*httpapi.Agent, error) {
	org, name := agentParams(organization, agent)
	response, err := client.UpdateAgent(ctx, request, httpapi.UpdateAgentParams{Organization: org, Agent: name})
	return agentAnswer[httpapi.Agent](organization, "update agent", response, err)
}

func newAgentDisableCommand(a *app, disable bool) *cobra.Command {
	use, short, done := "enable", "Let a disabled agent work again", "Enabled"
	if disable {
		use, short, done = "disable", "Stop every key, connection and token that acts as an agent", "Disabled"
	}
	return &cobra.Command{
		Use:   use + " ORGANIZATION/AGENT",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			if _, err := a.updateAgent(command.Context(), client, organization, name, &httpapi.UpdateAgentRequest{Disabled: httpapi.NewOptBool(disable)}); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s agent %s/%s\n", done, organization, name)
			return nil
		},
	}
}

func newAgentRemoveCommand(a *app) *cobra.Command {
	var force bool
	command := &cobra.Command{
		Use:     "rm ORGANIZATION/AGENT",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an agent with its secrets and every key that acts as it",
		Long: `Delete an agent with its secrets and every key, connection and token that
acts as it. Only owners and admins can delete agents.

Without --force, gv asks you to type the agent name to confirm.`,
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			if !force {
				prompt := fmt.Sprintf("This deletes agent %s/%s, its secrets and every key that acts as it.\nType the agent name to confirm: ", organization, name)
				if err := a.confirm(prompt, name, "not deleted"); err != nil {
					return err
				}
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			org, agent := agentParams(organization, name)
			response, err := client.DeleteAgent(command.Context(), httpapi.DeleteAgentParams{Organization: org, Agent: agent})
			if _, err := agentAnswer[httpapi.DeleteAgentNoContent](organization, "delete agent", response, err); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Deleted agent %s/%s\n", organization, name)
			return nil
		},
	}
	command.Flags().BoolVarP(&force, "force", "f", false, "delete without asking for confirmation")
	return command
}

func newAgentManagerCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "manager",
		Short: "Add and remove an agent's managers",
		Long: `Managers can change an agent, manage its secrets and disable it. A manager
must be a member whose role allows everything the agent may do.`,
	}
	add := &cobra.Command{
		Use:   "add ORGANIZATION/AGENT USER",
		Short: "Make a member a manager of an agent",
		Args:  cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			org, agent := agentParams(organization, name)
			response, err := client.AddAgentManager(command.Context(), httpapi.AddAgentManagerParams{Organization: org, Agent: agent, User: httpapi.Name(args[1])})
			if _, err := agentAnswer[httpapi.Agent](organization, "add manager", response, err); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s now manages %s/%s\n", args[1], organization, name)
			return nil
		},
	}
	remove := &cobra.Command{
		Use:     "rm ORGANIZATION/AGENT USER",
		Aliases: []string{"remove"},
		Short:   "Stop a member from managing an agent",
		Args:    cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			org, agent := agentParams(organization, name)
			response, err := client.RemoveAgentManager(command.Context(), httpapi.RemoveAgentManagerParams{Organization: org, Agent: agent, User: httpapi.Name(args[1])})
			if _, err := agentAnswer[httpapi.Agent](organization, "remove manager", response, err); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "%s no longer manages %s/%s\n", args[1], organization, name)
			return nil
		},
	}
	command.AddCommand(add, remove)
	return command
}

func (a *app) printAgent(organization string, agent *httpapi.Agent, asJSON bool) error {
	if asJSON {
		return writeJSONLine(a.stdout, agent)
	}
	managers := make([]string, 0, len(agent.Managers))
	for _, manager := range agent.Managers {
		managers = append(managers, string(manager))
	}
	table := tabwriter.NewWriter(a.stdout, 0, 8, 1, ' ', 0)
	fmt.Fprintf(table, "Agent:\t%s/%s\n", organization, agent.Name)
	fmt.Fprintf(table, "ID:\t%s\n", agent.ID)
	fmt.Fprintf(table, "Status:\t%s\n", agentStatus(agent))
	fmt.Fprintf(table, "Permissions:\t%s\n", joinPermissions(agent.Permissions))
	fmt.Fprintf(table, "Indexes:\t%s\n", describeIndexes(agent.Indexes))
	fmt.Fprintf(table, "Managers:\t%s\n", orNone(strings.Join(managers, ", ")))
	created := formatTime(agent.CreatedAt)
	if by, ok := agent.CreatedBy.Get(); ok {
		created += " by " + string(by)
	}
	fmt.Fprintf(table, "Created:\t%s\n", created)
	fmt.Fprintf(table, "Last used:\t%s\n", lastUsed(agent.LastUsedAt))
	fmt.Fprintf(table, "Description:\t%s\n", orNone(string(agent.Description)))
	return table.Flush()
}

func agentStatus(agent *httpapi.Agent) string {
	var status []string
	if agent.Disabled {
		status = append(status, "disabled")
	}
	if agent.NeedsReview {
		status = append(status, "needs review")
	}
	return orDefault(strings.Join(status, ", "), "active")
}

func joinPermissions(permissions []httpapi.Permission) string {
	names := make([]string, 0, len(permissions))
	for _, permission := range permissions {
		names = append(names, string(permission))
	}
	return strings.Join(names, ", ")
}

// describeIndexes names the indexes of a scope; no list means every index.
func describeIndexes(indexes httpapi.IndexScope) string {
	if indexes == nil {
		return "all indexes"
	}
	names := make([]string, 0, len(indexes))
	for _, index := range indexes {
		names = append(names, string(index))
	}
	return orNone(strings.Join(names, ", "))
}

func lastUsed(at httpapi.OptDateTime) string {
	if value, ok := at.Get(); ok {
		return formatTime(value)
	}
	return "never"
}

func orNone(value string) string { return orDefault(value, "(none)") }

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func newAgentSecretCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "secret",
		Short: "Create, list and revoke an agent's secrets",
		Long: `A job that runs without a person, such as CI, signs in as an agent with an
agent secret: set GREPVINE_AGENT_SECRET, or agent_secret in the configuration
file. gv exchanges it for short-lived tokens and never sends the secret
itself with API requests.

An agent holds at most two secrets, so you can rotate one: create a second,
deploy it, then revoke the first.`,
	}
	command.AddCommand(newAgentSecretCreateCommand(a), newAgentSecretListCommand(a), newAgentSecretRemoveCommand(a))
	return command
}

func newAgentSecretCreateCommand(a *app) *cobra.Command {
	var expires string
	var save bool
	command := &cobra.Command{
		Use:   "create ORGANIZATION/AGENT",
		Short: "Create an agent secret that you approve in a browser, and print it",
		Long: `Create an agent secret and print it on standard output. It is shown only
once. You approve it in a browser, as a manager of the agent or an owner or
admin.

With --save, gv also saves it to the configuration file, replacing the saved
API key: gv then runs as the agent. Run gv login to act as yourself again.`,
		Example: `  gv agent secret create acme/ci --expires 90d > ci.secret`,
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			wanted := httpapi.DeviceAgentSecret{Organization: httpapi.Name(organization), Agent: httpapi.Name(name)}
			if expires != "never" {
				at, err := parseExpiry(orDefault(expires, defaultLifetime), time.Now())
				if err != nil {
					return err
				}
				wanted.ExpiresAt = httpapi.NewOptDateTime(at)
			}
			client, _, path, err := a.client(false)
			if err != nil {
				return err
			}
			answer, err := a.deviceToken(command.Context(), client, &httpapi.DeviceAuthorizationRequest{AgentSecret: httpapi.NewOptDeviceAgentSecret(wanted)})
			if err != nil {
				return err
			}
			created, ok := answer.AgentSecret.Get()
			if !ok {
				return errors.New("device authorization returned no agent secret; retry")
			}
			until := "never expires"
			if at, ok := created.ExpiresAt.Get(); ok {
				until = "until " + at.UTC().Format(time.DateOnly)
			}
			fmt.Fprintf(a.stderr, "Created secret %s for agent %s/%s (%s). Store it now; it is not shown again.\n", created.Prefix, organization, name, until)
			fmt.Fprintln(a.stdout, created.Secret)
			if !save {
				return nil
			}
			stored, _, err := a.loadConfig()
			if err != nil {
				return err
			}
			// The file holds one identity: the secret replaces a saved API key.
			stored.AgentSecret, stored.APIKey = created.Secret, ""
			if err := saveConfig(path, stored); err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "Saved to %s; gv now runs as agent %s. Run gv login to act as yourself again.\n", path, name)
			return nil
		},
	}
	command.Flags().StringVar(&expires, "expires", "", "when the secret expires: a number of days such as 30d, a date such as 2027-01-31, or never (default: 90 days, at most a year)")
	command.Flags().BoolVar(&save, "save", false, "also save the secret to the configuration file, so gv runs as the agent")
	return command
}

func (a *app) listAgentSecrets(ctx context.Context, client *httpapi.Client, organization, agent string) ([]httpapi.AgentSecret, error) {
	org, name := agentParams(organization, agent)
	response, err := client.ListAgentSecrets(ctx, httpapi.ListAgentSecretsParams{Organization: org, Agent: name})
	page, err := agentAnswer[httpapi.AgentSecretPage](organization, "list agent secrets", response, err)
	if err != nil {
		return nil, err
	}
	return page.Secrets, nil
}

func newAgentSecretListCommand(a *app) *cobra.Command {
	var asJSON bool
	command := &cobra.Command{
		Use:     "ls ORGANIZATION/AGENT",
		Aliases: []string{"list"},
		Short:   "List an agent's secrets, without the secrets themselves",
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			secrets, err := a.listAgentSecrets(command.Context(), client, organization, name)
			if err != nil {
				return err
			}
			if asJSON {
				for i := range secrets {
					if err := writeJSONLine(a.stdout, &secrets[i]); err != nil {
						return err
					}
				}
				return nil
			}
			table := tabwriter.NewWriter(a.stdout, 0, 8, 2, ' ', 0)
			fmt.Fprintln(table, "PREFIX\tID\tCREATED\tEXPIRES\tLAST USED")
			for _, secret := range secrets {
				expires := "never"
				if at, ok := secret.ExpiresAt.Get(); ok {
					expires = formatTime(at)
				}
				fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", secret.Prefix, secret.ID, formatTime(secret.CreatedAt), expires, lastUsed(secret.LastUsedAt))
			}
			return table.Flush()
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print one JSON object per secret instead of a table")
	return command
}

func newAgentSecretRemoveCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:     "rm ORGANIZATION/AGENT ID|PREFIX",
		Aliases: []string{"remove", "revoke"},
		Short:   "Revoke an agent secret and end the tokens exchanged for it",
		Long: `Revoke an agent secret, named by its ID or by its prefix as gv agent secret
ls shows them. Tokens exchanged for it stop working at once; the agent's other
secret keeps working.`,
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, name, err := a.parseReference(args[0], "agent")
			if err != nil {
				return err
			}
			client, _, _, err := a.client(true)
			if err != nil {
				return err
			}
			id, err := uuid.Parse(args[1])
			if err != nil {
				secrets, err := a.listAgentSecrets(command.Context(), client, organization, name)
				if err != nil {
					return err
				}
				if id, err = matchSecret(secrets, args[1]); err != nil {
					return err
				}
			}
			org, agent := agentParams(organization, name)
			response, err := client.RevokeAgentSecret(command.Context(), httpapi.RevokeAgentSecretParams{Organization: org, Agent: agent, SecretID: id})
			if _, err := agentAnswer[httpapi.RevokeAgentSecretNoContent](organization, "revoke agent secret", response, err); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Revoked secret %s of agent %s/%s\n", args[1], organization, name)
			return nil
		},
	}
}

// matchSecret finds the one secret whose prefix matches value: value is the
// prefix, the start of it, or the secret itself.
func matchSecret(secrets []httpapi.AgentSecret, value string) (uuid.UUID, error) {
	if value == "" {
		return uuid.Nil, errors.New("name the secret by its ID or prefix")
	}
	var found []httpapi.AgentSecret
	for _, secret := range secrets {
		if strings.HasPrefix(secret.Prefix, value) || strings.HasPrefix(value, secret.Prefix) {
			found = append(found, secret)
		}
	}
	if len(found) == 1 {
		return found[0].ID, nil
	}
	prefixes := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		prefixes = append(prefixes, secret.Prefix)
	}
	problem := "no secret matches"
	if len(found) > 1 {
		problem = "more than one secret matches"
	}
	return uuid.Nil, fmt.Errorf("%s %q; the agent's secrets are: %s", problem, value, orNone(strings.Join(prefixes, ", ")))
}
