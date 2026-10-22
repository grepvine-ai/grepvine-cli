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
	command := &cobra.Command{
		Use:   "login",
		Short: "Authorize this device in a browser",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return a.login(command.Context(), name)
		},
	}
	command.Flags().StringVar(&name, "name", name, "name for the API key created by this login")
	return command
}

func (a *app) login(ctx context.Context, name string) error {
	client, resolved, path, err := a.client(false)
	if err != nil {
		return err
	}
	authorization, err := client.StartDeviceAuthorization(ctx, &httpapi.DeviceAuthorizationRequest{APIKeyName: name})
	if err != nil {
		return fmt.Errorf("start device authorization: %w", friendlyAPIError(err))
	}
	complete := authorization.VerificationURIComplete.String()
	fmt.Fprintf(a.stdout, "Open %s\n", complete)
	fmt.Fprintf(a.stdout, "Enter code: %s\n", authorization.UserCode)
	if a.open != nil {
		_ = a.open(complete)
	}

	interval := max(time.Duration(authorization.Interval)*time.Second, 5*time.Second)
	expires := time.Now().Add(time.Duration(authorization.ExpiresIn) * time.Second)
	for {
		if !time.Now().Before(expires) {
			return errors.New("device authorization expired")
		}
		if err := a.sleep(ctx, interval); err != nil {
			return err
		}
		response, err := client.PollDeviceToken(ctx, &httpapi.DeviceTokenRequest{DeviceCode: authorization.DeviceCode})
		if err != nil {
			return fmt.Errorf("poll device authorization: %w", friendlyAPIError(err))
		}
		switch result := response.(type) {
		case *httpapi.APIKeyCreated:
			stored := config{APIKey: result.Key}
			if resolved.Endpoint != defaultEndpoint {
				stored.Endpoint = resolved.Endpoint
			}
			if err := saveConfig(path, stored); err != nil {
				return err
			}
			fmt.Fprintf(a.stdout, "Logged in as API key %s.\n", result.Prefix)
			return nil
		case *httpapi.ErrorStatusCodeWithHeaders:
			switch result.Response.Code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			case "access_denied":
				return errors.New("device authorization was denied")
			case "expired_token":
				return errors.New("device authorization expired")
			default:
				return publicAPIError(result)
			}
		default:
			return fmt.Errorf("unexpected device authorization response %T", response)
		}
	}
}

func newLogoutCommand(a *app) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the saved API key",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, path, err := a.loadConfig()
			if err != nil {
				return err
			}
			cfg.APIKey = ""
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
