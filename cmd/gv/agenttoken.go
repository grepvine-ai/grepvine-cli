// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// refreshBefore is how long before an agent token expires gv exchanges the
// secret again, so a request never goes out with a token about to lapse.
const refreshBefore = time.Minute

// agentTokens exchanges an agent secret for short-lived agent tokens at the
// token endpoint and hands out the current one, exchanging again shortly
// before it expires. Tokens stay in memory; the secret goes only to the token
// endpoint, never to the API.
type agentTokens struct {
	secret   string
	tokenURL string
	client   *http.Client
	now      func() time.Time

	mu      sync.Mutex
	token   string
	refresh time.Time
}

func newAgentTokens(secret, endpoint string, client *http.Client) *agentTokens {
	root := strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/v1")
	return &agentTokens{secret: secret, tokenURL: root + "/oauth/token", client: client, now: time.Now}
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

// exchange trades the secret for a token with the client_credentials grant.
//
// The token endpoint takes the secret in one of two ways: in the form body as
// client_secret, or with HTTP Basic authentication, the agent's ID as the user
// name and the secret as the password. gv sends it in the body, because the
// secret alone names the agent; Basic would need the agent's ID as a second
// setting. gv asks for the agent's full permissions and the default lifetime.
func (t *agentTokens) exchange(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{"grant_type": {"client_credentials"}, "client_secret": {t.secret}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("exchange agent secret: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := t.client.Do(request)
	if err != nil {
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: %v; check your connection and retry", err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: %v; retry", err)}
	}
	var answer struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &answer)
	switch {
	case response.StatusCode == http.StatusOK && answer.AccessToken != "" && answer.ExpiresIn > 0:
		return answer.AccessToken, time.Duration(answer.ExpiresIn) * time.Second, nil
	case response.StatusCode == http.StatusTooManyRequests:
		wait := response.Header.Get("Retry-After")
		if wait == "" {
			wait = "a few"
		}
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: too many exchanges; slow down and retry in %s seconds", wait)}
	case answer.Error == "invalid_client":
		return "", 0, &tokenError{fmt.Sprintf("%s (invalid_client)\nCheck GREPVINE_AGENT_SECRET or agent_secret in the configuration file; a manager of the agent can create a new secret with gv agent secret create ORGANIZATION/AGENT", answer.Description)}
	case answer.Error != "":
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: %s (%s)", answer.Description, answer.Error)}
	default:
		return "", 0, &tokenError{fmt.Sprintf("exchange agent secret: unexpected answer %s; retry", response.Status)}
	}
}
