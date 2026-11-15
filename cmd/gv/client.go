// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// credentials authenticate requests with an API key or, when an agent secret
// is set, with the agent tokens it is exchanged for.
type credentials struct {
	key    string
	tokens *agentTokens
}

//nolint:revive // implements the generated ogen interface
func (c credentials) ApiKeyAuth(ctx context.Context, _ httpapi.OperationName) (httpapi.ApiKeyAuth, error) {
	if c.tokens != nil {
		token, err := c.tokens.current(ctx)
		if err != nil {
			return httpapi.ApiKeyAuth{}, err
		}
		return httpapi.ApiKeyAuth{Token: token}, nil
	}
	if c.key == "" {
		return httpapi.ApiKeyAuth{}, ogenerrors.ErrSkipClientSecurity
	}
	return httpapi.ApiKeyAuth{Token: c.key}, nil
}

// AgentToken sends the agent token on operations an agent working alone may
// call. With an API key the operation is authenticated by ApiKeyAuth alone.
func (c credentials) AgentToken(ctx context.Context, _ httpapi.OperationName) (httpapi.AgentToken, error) {
	if c.tokens == nil {
		return httpapi.AgentToken{}, ogenerrors.ErrSkipClientSecurity
	}
	token, err := c.tokens.current(ctx)
	if err != nil {
		return httpapi.AgentToken{}, err
	}
	return httpapi.AgentToken{Token: token}, nil
}

func (credentials) CookieAuth(context.Context, httpapi.OperationName) (httpapi.CookieAuth, error) {
	return httpapi.CookieAuth{}, ogenerrors.ErrSkipClientSecurity
}

func (a *app) client(requireKey bool) (*httpapi.Client, config, string, error) {
	cfg, path, err := a.endpointAndKey()
	if err != nil {
		return nil, config{}, path, err
	}
	if requireKey && cfg.APIKey == "" && cfg.AgentSecret == "" {
		return nil, config{}, path, errors.New("not logged in; run `gv login`, or set GREPVINE_AGENT_SECRET to run as an agent")
	}
	creds := credentials{key: cfg.APIKey}
	if cfg.AgentSecret != "" {
		creds.tokens = newAgentTokens(cfg.AgentSecret, cfg.Endpoint, a.httpClient)
	}
	client, err := httpapi.NewClient(apiEndpoint(cfg.Endpoint), creds, httpapi.WithClient(a.httpClient))
	if err != nil {
		return nil, config{}, path, fmt.Errorf("create API client: %w", err)
	}
	return client, cfg, path, nil
}

func apiEndpoint(endpoint string) string {
	endpoint = strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(endpoint, "/v1") {
		return endpoint
	}
	return endpoint + "/v1"
}

func publicAPIError(response *httpapi.ErrorStatusCodeWithHeaders) error {
	return responseError(response.Response, response.XRequestID)
}
