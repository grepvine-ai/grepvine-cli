package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

type credentials struct{ key string }

//nolint:revive // implements the generated ogen interface
func (c credentials) ApiKeyAuth(context.Context, httpapi.OperationName) (httpapi.ApiKeyAuth, error) {
	if c.key == "" {
		return httpapi.ApiKeyAuth{}, ogenerrors.ErrSkipClientSecurity
	}
	return httpapi.ApiKeyAuth{Token: c.key}, nil
}

func (credentials) CookieAuth(context.Context, httpapi.OperationName) (httpapi.CookieAuth, error) {
	return httpapi.CookieAuth{}, ogenerrors.ErrSkipClientSecurity
}

func (a *app) client(requireKey bool) (*httpapi.Client, config, string, error) {
	cfg, path, err := a.endpointAndKey()
	if err != nil {
		return nil, config{}, path, err
	}
	if requireKey && cfg.APIKey == "" {
		return nil, config{}, path, errors.New("not logged in; run `gv login`")
	}
	client, err := httpapi.NewClient(apiEndpoint(cfg.Endpoint), credentials{key: cfg.APIKey}, httpapi.WithClient(a.httpClient))
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
