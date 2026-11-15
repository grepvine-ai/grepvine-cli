// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

func newLoginCommand(a *app) *cobra.Command {
	name := "gv"
	var scope scopeFlags
	command := &cobra.Command{
		Use:   "login",
		Short: "Authorize this device in a browser",
		Long: `Authorize this device in a browser and save the API key it receives.

The key asks for what the flags say; without them it asks to search and read
every index of every organization you belong to, for 90 days. You see the
request in the browser and may change it before you approve it. A key never
does more than your role allows.

With --agent, the key acts as that agent of --org: it does what both you and
the agent may do.`,
		Example: `  gv login
  gv login --org acme --index docs --permission search --permission read --expires 30d
  gv login --org acme --agent ci --permission search --permission read`,
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			request, err := scope.request(name, time.Now())
			if err != nil {
				return err
			}
			return a.login(command.Context(), request)
		},
	}
	command.Flags().StringVar(&name, "name", name, "name for the API key created by this login")
	scope.register(command)
	return command
}

func (a *app) login(ctx context.Context, request *httpapi.DeviceAuthorizationRequest) error {
	client, resolved, path, err := a.client(false)
	if err != nil {
		return err
	}
	created, key, err := a.deviceKey(ctx, client, request)
	if err != nil {
		return err
	}
	stored, _, err := a.loadConfig()
	if err != nil {
		return err
	}
	// The file holds one identity: signing in replaces a saved agent secret.
	stored.APIKey, stored.AgentSecret, stored.Endpoint = key, "", ""
	if resolved.Endpoint != defaultEndpoint {
		stored.Endpoint = resolved.Endpoint
	}
	if err := saveConfig(path, stored); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Logged in as API key %s: %s.\n", created.Prefix.Or(""), describeKeyScope(created))
	return nil
}

// deviceKey asks for an API key through the browser and returns it with the
// secret key itself.
func (a *app) deviceKey(ctx context.Context, client *httpapi.Client, request *httpapi.DeviceAuthorizationRequest) (*httpapi.DeviceToken, string, error) {
	created, err := a.deviceToken(ctx, client, request)
	if err != nil {
		return nil, "", err
	}
	key, ok := created.Key.Get()
	if !ok {
		return nil, "", errors.New("device authorization returned no API key; retry")
	}
	return created, key, nil
}

// deviceToken asks for a credential through the browser: it prints where to
// approve the request, waits for the decision, and returns what the person
// approved. Messages go to standard error, so a caller may print the
// credential on standard output.
func (a *app) deviceToken(ctx context.Context, client *httpapi.Client, request *httpapi.DeviceAuthorizationRequest) (*httpapi.DeviceToken, error) {
	authorization, err := client.StartDeviceAuthorization(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("start device authorization: %w", friendlyAPIError(err))
	}
	complete := authorization.VerificationURIComplete.String()
	fmt.Fprintf(a.stderr, "Open %s\n", complete)
	fmt.Fprintf(a.stderr, "Enter code: %s\n", authorization.UserCode)
	if a.open != nil {
		_ = a.open(complete)
	}

	interval := max(time.Duration(authorization.Interval)*time.Second, 5*time.Second)
	expires := time.Now().Add(time.Duration(authorization.ExpiresIn) * time.Second)
	for {
		if !time.Now().Before(expires) {
			return nil, errors.New("device authorization expired")
		}
		if err := a.sleep(ctx, interval); err != nil {
			return nil, err
		}
		response, err := client.PollDeviceToken(ctx, &httpapi.DeviceTokenRequest{DeviceCode: authorization.DeviceCode})
		if err != nil {
			return nil, fmt.Errorf("poll device authorization: %w", friendlyAPIError(err))
		}
		switch result := response.(type) {
		case *httpapi.DeviceToken:
			return result, nil
		case *httpapi.ErrorStatusCodeWithHeaders:
			switch result.Response.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				// The service says why: denied by the person, or refused, such
				// as an agent that already has two secrets.
				return nil, fmt.Errorf("device authorization: %s", result.Response.Error)
			case "expired_token":
				return nil, errors.New("device authorization expired")
			default:
				return nil, publicAPIError(result)
			}
		default:
			return nil, fmt.Errorf("unexpected device authorization response %T", response)
		}
	}
}

func newLogoutCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved API key and agent secret",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			cfg.APIKey, cfg.AgentSecret = "", ""
			if strings.TrimRight(cfg.Endpoint, "/") == defaultEndpoint {
				cfg.Endpoint = ""
			}
			if err := saveConfig(path, cfg); err != nil {
				return err
			}
			fmt.Fprintln(a.stdout, "Logged out.")
			return nil
		},
	}
}
