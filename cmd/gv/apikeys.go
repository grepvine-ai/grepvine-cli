// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// An API key cannot create, list or revoke API keys: only a person signed in
// to the console can. So `gv key create` asks for a key through the browser,
// like `gv login`, and listing and revoking happen in the console.
func newKeyCommand(a *app) *cobra.Command {
	command := &cobra.Command{
		Use:   "key",
		Short: "Create an API key; list and revoke keys in the console",
		Long: `Create an API key that you approve in a browser.

List and revoke your API keys in the console, on the API keys page.`,
	}
	command.AddCommand(newKeyCreateCommand(a))
	return command
}

func newKeyCreateCommand(a *app) *cobra.Command {
	var scope scopeFlags
	return withScope(&scope, &cobra.Command{
		Use:   "create NAME",
		Short: "Create an API key that you approve in a browser, and print it",
		Long: `Create an API key and print it on standard output, so that
"gv key create ci > ci.key" saves it. The key is shown only once.

You approve the key in a browser, where you see what it asks for and may change
it. Without flags it asks to search and read every index of every organization
you belong to, for 90 days. A key never does more than your role allows.`,
		Example: `  gv key create ci --org acme --index docs --permission search --permission read --expires 30d > ci.key`,
		Args:    cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			request, err := scope.request(args[0], time.Now())
			if err != nil {
				return err
			}
			client, _, _, err := a.client(false)
			if err != nil {
				return err
			}
			created, err := a.deviceKey(command.Context(), client, request)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.stderr, "Created API key %s (%s): %s. Store it now; it is not shown again.\n",
				created.Name, created.ID, describeKeyScope(created.Organization, created.Indexes, created.Permissions, created.ExpiresAt))
			fmt.Fprintln(a.stdout, created.Key)
			return nil
		},
	})
}

func withScope(scope *scopeFlags, command *cobra.Command) *cobra.Command {
	scope.register(command)
	return command
}
