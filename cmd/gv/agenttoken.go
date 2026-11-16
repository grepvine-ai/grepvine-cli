// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// refreshBefore is how long before an agent token expires gv exchanges the
// secret again, so a request never goes out with a token about to lapse.
const refreshBefore = time.Minute

// agentTokens exchanges an agent secret for short-lived agent tokens at the
// token endpoint and hands out the current one, exchanging again shortly
// before it expires or after the API refuses it. Tokens stay in memory; the
// secret goes only to the token endpoint, never to the API.
//
// x/oauth2 makes the exchange. gv keeps the token itself rather than in an
// oauth2.ReuseTokenSource, which cannot drop a token the API refused.
type agentTokens struct {
	config clientcredentials.Config
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	token   string
	refresh time.Time
}

// newAgentTokens exchanges secret with the client_credentials grant. The token
// endpoint takes the secret in the form body as client_secret, or with HTTP
// Basic authentication and the agent's ID as the user name. gv sends it in the
// body, because the secret alone names the agent; Basic would need the agent's
// ID as a second setting. gv asks for the agent's full permissions and the
// default lifetime.
func newAgentTokens(secret, endpoint string, client *http.Client) *agentTokens {
	root := strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1")
	return &agentTokens{
		config: clientcredentials.Config{ClientSecret: secret, TokenURL: root + "/oauth/token", AuthStyle: oauth2.AuthStyleInParams},
		client: client,
		now:    time.Now,
	}
}

// tokenError is a failed exchange. It is the whole message the user sees.
type tokenError struct{ message string }

func (e *tokenError) Error() string { return e.message }

// current returns a token that is valid for at least a while longer. Callers
// in other goroutines wait for one exchange instead of starting their own.
func (t *agentTokens) current(ctx context.Context) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && t.now().Before(t.refresh) {
		return t.token, nil
	}
	token, lifetime, err := t.exchange(ctx)
	if err != nil {
		return "", err
	}
	// Refresh a minute early, or after three quarters of a short lifetime.
	t.token, t.refresh = token, t.now().Add(lifetime-min(refreshBefore, lifetime/4))
	return token, nil
}

// exchange trades the secret for a token and says how long it lasts.
func (t *agentTokens) exchange(ctx context.Context) (string, time.Duration, error) {
	token, err := t.config.Token(context.WithValue(ctx, oauth2.HTTPClient, t.client))
	failed, refused := errors.AsType[*oauth2.RetrieveError](err)
	switch {
	case err == nil && !token.Expiry.IsZero():
		return token.AccessToken, time.Until(token.Expiry), nil
	case err == nil:
		return "", 0, &tokenError{"exchange agent secret: the answer has no expires_in; retry"}
	case !refused:
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: %v; check your connection and retry", err)}
	case failed.Response.StatusCode == http.StatusTooManyRequests:
		wait := failed.Response.Header.Get("Retry-After")
		if wait == "" {
			wait = "a few"
		}
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: too many exchanges; slow down and retry in %s seconds", wait)}
	case failed.ErrorCode == "invalid_client":
		return "", 0, &tokenError{fmt.Sprintf("%s (invalid_client)\nCheck GREPVINE_AGENT_SECRET or agent_secret in the configuration file; a manager of the agent can create a new secret with gv agent secret create ORGANIZATION/AGENT", failed.ErrorDescription)}
	case failed.ErrorCode != "":
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: %s (%s)", failed.ErrorDescription, failed.ErrorCode)}
	default:
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: unexpected answer %s; retry", failed.Response.Status)}
	}
}

// forget drops token if it is still the current one, so the next request
// exchanges the secret again. A token another request already replaced stays
// replaced, so concurrent refusals lead to one exchange.
func (t *agentTokens) forget(token string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token == token {
		t.token = ""
	}
}

// retryRefused sends API requests made with an agent token, and when the API
// refuses one with 401, as it does for a token revoked when the secret is
// rotated, exchanges the secret again and sends the request once more. A
// request whose body cannot be sent again is not retried.
type retryRefused struct {
	base   http.RoundTripper
	tokens *agentTokens
}

func (r retryRefused) RoundTrip(request *http.Request) (*http.Response, error) {
	replayable := request.Body == nil || request.Body == http.NoBody || request.GetBody != nil
	response, err := r.base.RoundTrip(request)
	token, agent := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if err != nil || response.StatusCode != http.StatusUnauthorized || !agent || !replayable {
		return response, err
	}
	r.tokens.forget(token)
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	fresh, err := r.tokens.current(request.Context())
	if err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	if request.GetBody != nil {
		if retry.Body, err = request.GetBody(); err != nil {
			return nil, err
		}
	}
	retry.Header.Set("Authorization", "Bearer "+fresh)
	return r.base.RoundTrip(retry)
}
