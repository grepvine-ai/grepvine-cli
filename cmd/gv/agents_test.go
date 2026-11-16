// Copyright 2026 Treeverse, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

const ciAgent = `{"id":"0198f13d-f525-7f84-a98e-3c0264919c42","name":"ci-bot","description":"CI searches","indexes":["docs"],"permissions":["search","read"],"managers":["alice"],"created_by":"alice","created_at":"2026-11-01T00:00:00Z","disabled":false,"needs_review":true}`

func TestAgentCommands(t *testing.T) {
	secrets := `{"secrets":[{"id":"11111111-1111-4111-8111-111111111111","prefix":"gvs_aaaa","created_at":"2026-11-01T00:00:00Z"},{"id":"22222222-2222-4222-8222-222222222222","prefix":"gvs_bbbb","created_at":"2026-11-02T00:00:00Z"}]}`
	s, _, execute := newAdminServer(t, map[string]adminResponse{
		"GET /organizations/acme/agents":                                                        {200, `{"results":[` + ciAgent + `],"pagination":{"next_page_id":"p2","has_next":true}}`},
		"GET /organizations/acme/agents?page=p2":                                                {200, `{"results":[` + strings.Replace(strings.Replace(ciAgent, "ci-bot", "zeta-bot", 1), "0198f13d", "0198f13e", 1) + `],"pagination":{"next_page_id":"","has_next":false}}`},
		"GET /organizations/acme/agents/ci-bot":                                                 {200, ciAgent},
		"POST /organizations/acme/agents":                                                       {201, ciAgent},
		"PATCH /organizations/acme/agents/ci-bot":                                               {200, ciAgent},
		"DELETE /organizations/acme/agents/ci-bot":                                              {204, ""},
		"PUT /organizations/acme/agents/ci-bot/managers/bob":                                    {200, ciAgent},
		"DELETE /organizations/acme/agents/ci-bot/managers/bob":                                 {200, ciAgent},
		"GET /organizations/acme/agents/ci-bot/secrets":                                         {200, secrets},
		"DELETE /organizations/acme/agents/ci-bot/secrets/22222222-2222-4222-8222-222222222222": {204, ""},
	})

	stdout, _, err := execute("", "agent", "ls", "acme")
	if err != nil || !strings.Contains(stdout, "ci") || !strings.Contains(stdout, "needs review") || !strings.Contains(stdout, "search, read") || !strings.Contains(stdout, "zeta-bot") {
		t.Fatalf("agent ls = %q, %v", stdout, err)
	}
	expectRequests(t, s, "GET /organizations/acme/agents", "GET /organizations/acme/agents")

	_, _, err = execute("", "agent", "create", "acme/ci-bot", "--permission", "search", "--permission", "read", "--index", "docs")
	if err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, `POST /organizations/acme/agents {"name":"ci-bot","indexes":["docs"],"permissions":["search","read"]}`)
	if _, _, err := execute("", "agent", "create", "acme/ci-bot", "--permission", "manage_members"); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("create with manage_members: %v", err)
	}

	// --permission replaces the permissions and keeps the indexes.
	if _, _, err := execute("", "agent", "update", "acme/ci-bot", "--permission", "search"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, "GET /organizations/acme/agents/ci-bot", `PATCH /organizations/acme/agents/ci-bot {"ceiling":{"indexes":["docs"],"permissions":["search"]}}`)
	// No flags saves the ceiling as it is, which clears a review mark.
	if _, _, err := execute("", "agent", "update", "acme/ci-bot", "--all-indexes"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, "GET /organizations/acme/agents/ci-bot", `PATCH /organizations/acme/agents/ci-bot {"ceiling":{"permissions":["search","read"]}}`)

	if _, _, err := execute("", "agent", "disable", "acme/ci-bot"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, `PATCH /organizations/acme/agents/ci-bot {"disabled":true}`)

	if _, _, err := execute("", "agent", "manager", "add", "acme/ci-bot", "bob"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, "PUT /organizations/acme/agents/ci-bot/managers/bob")

	// A secret is revoked by its prefix, as gv agent secret ls shows it.
	if _, _, err := execute("", "agent", "secret", "rm", "acme/ci-bot", "gvs_bb"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, "GET /organizations/acme/agents/ci-bot/secrets", "DELETE /organizations/acme/agents/ci-bot/secrets/22222222-2222-4222-8222-222222222222")
	if _, _, err := execute("", "agent", "secret", "rm", "acme/ci-bot", "gvs_"); err == nil || !strings.Contains(err.Error(), "more than one secret matches") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	expectRequests(t, s, "GET /organizations/acme/agents/ci-bot/secrets")

	if _, _, err := execute("x\n", "agent", "rm", "acme/ci-bot"); err == nil || !strings.HasPrefix(err.Error(), "not deleted") {
		t.Fatalf("agent rm without confirmation: %v", err)
	}
	expectRequests(t, s)
	if _, _, err := execute("ci-bot\n", "agent", "rm", "acme/ci-bot"); err != nil {
		t.Fatal(err)
	}
	expectRequests(t, s, "DELETE /organizations/acme/agents/ci-bot")
}

// A refusal because the agent lacks a permission points to its managers,
// never to signing in again.
func TestAgentMissingPermissionHint(t *testing.T) {
	err := responseError(httpapi.Error{Code: "forbidden", Error: "this agent does not have the write permission; ask one of its managers",
		Details: []httpapi.ErrorDetail{{Type: "agent_missing_permission", Metadata: httpapi.ErrorDetailMetadata{"permission": "write"}}}}, httpapi.OptString{})
	if !strings.Contains(err.Error(), "ask one of its managers") || strings.Contains(err.Error(), "gv login") {
		t.Fatalf("error = %q", err)
	}
}

// The credential from the environment wins over the one from the file; two
// from the same place are refused.
func TestAgentSecretPrecedence(t *testing.T) {
	a, _, _ := testApp(t, "https://api.example")
	env := map[string]string{}
	a.getenv = func(name string) string { return env[name] }
	cases := []struct {
		file             config
		env              map[string]string
		key, secret, err string
	}{
		{file: config{APIKey: "k"}, env: map[string]string{"GREPVINE_AGENT_SECRET": "s"}, secret: "s"},
		{file: config{AgentSecret: "s"}, env: map[string]string{"GREPVINE_API_KEY": "k"}, key: "k"},
		{file: config{APIKey: "k", AgentSecret: "s"}, err: "gv config unset agent_secret"},
		{env: map[string]string{"GREPVINE_API_KEY": "k", "GREPVINE_AGENT_SECRET": "s"}, err: "GREPVINE_API_KEY and GREPVINE_AGENT_SECRET are set"},
	}
	for i, c := range cases {
		if err := saveConfig(a.configPath, c.file); err != nil {
			t.Fatal(err)
		}
		env = c.env
		resolved, _, err := a.endpointAndKey()
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("case %d: error = %v, want %q", i, err, c.err)
			}
			continue
		}
		if err != nil || resolved.APIKey != c.key || resolved.AgentSecret != c.secret {
			t.Fatalf("case %d: resolved %+v, %v", i, resolved, err)
		}
	}
}

// gv exchanges the secret once, sends only the token to the API, and
// exchanges again shortly before the token expires.
func TestAgentSecretExchange(t *testing.T) {
	var exchanges atomic.Int32
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/token" {
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_secret") != "gvs_secret" {
				w.WriteHeader(http.StatusUnauthorized)
				io.WriteString(w, `{"error":"invalid_client","error_description":"the agent secret is not valid; create a new one in the console"}`)
				return
			}
			n := exchanges.Add(1)
			fmt.Fprintf(w, `{"access_token":"gvt_%d","token_type":"Bearer","expires_in":900,"scope":"search read"}`, n)
			return
		}
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		io.WriteString(w, `{"results":[],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()

	a, _, _ := testApp(t, server.URL+"/v1")
	a.getenv = func(name string) string {
		return map[string]string{"GREPVINE_API_ENDPOINT": server.URL + "/v1", "GREPVINE_AGENT_SECRET": "gvs_secret"}[name]
	}
	client, _, _, err := a.client(true)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := client.ListAgents(t.Context(), httpapi.ListAgentsParams{Organization: "acme"}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if exchanges.Load() != 1 {
		t.Fatalf("exchanges = %d, want 1", exchanges.Load())
	}
	for _, header := range seen {
		if header != "Bearer gvt_1" {
			t.Fatalf("API saw Authorization %q", header)
		}
	}

	tokens := newAgentTokens("gvs_secret", server.URL, http.DefaultClient)
	now := time.Now()
	tokens.now = func() time.Time { return now }
	first, _ := tokens.current(t.Context())
	now = now.Add(13 * time.Minute)
	if again, _ := tokens.current(t.Context()); again != first {
		t.Fatal("exchanged again before the refresh time")
	}
	now = now.Add(time.Minute + time.Second)
	if next, _ := tokens.current(t.Context()); next == first {
		t.Fatal("did not exchange again a minute before expiry")
	}

	bad := newAgentTokens("gvs_wrong", server.URL+"/v1", http.DefaultClient)
	if _, err := bad.current(t.Context()); err == nil || !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "gv agent secret create") {
		t.Fatalf("invalid secret error = %v", err)
	}

	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer busy.Close()
	if _, err := newAgentTokens("gvs_secret", busy.URL, http.DefaultClient).current(t.Context()); err == nil || !strings.Contains(err.Error(), "slow down and retry in 7 seconds") {
		t.Fatalf("too many exchanges error = %v", err)
	}
}

// secret create asks for the secret through the browser, prints it once, and
// with --save keeps it as the one identity in the configuration file.
func TestAgentSecretCreate(t *testing.T) {
	s, a, execute := newAdminServer(t, map[string]adminResponse{
		"POST /device/authorizations": {200, `{"device_code":"device","user_code":"ABCD-EFGH","verification_uri":"https://console.example/device","verification_uri_complete":"https://console.example/device?user_code=ABCD-EFGH","expires_in":600,"interval":5}`},
		"POST /device/token":          {200, `{"agent_secret":{"id":"11111111-1111-4111-8111-111111111111","prefix":"gvs_aaaa","created_at":"2026-11-01T00:00:00Z","secret":"gvs_aaaa_full","client_id":"22222222-2222-4222-8222-222222222222"}}`},
	})
	stdout, stderr, err := execute("", "agent", "secret", "create", "acme/ci-bot", "--expires", "never", "--save")
	if err != nil || stdout != "gvs_aaaa_full\n" || !strings.Contains(stderr, "ABCD-EFGH") || !strings.Contains(stderr, "never expires") || !strings.Contains(stderr, "Client ID: 22222222-2222-4222-8222-222222222222") {
		t.Fatalf("secret create = %q, %q, %v", stdout, stderr, err)
	}
	if got := s.sent()[0]; got != `POST /device/authorizations {"agent_secret":{"organization":"acme","agent":"ci-bot","expires_at":null}}` {
		t.Fatalf("request = %s", got)
	}
	cfg, _, err := a.loadConfig()
	if err != nil || cfg.AgentSecret != "gvs_aaaa_full" || cfg.APIKey != "" {
		t.Fatalf("saved config = %+v, %v", cfg, err)
	}
}

// A key refused a permission it would give an agent is told to sign in for
// one in the agent's organization that keeps manage_agents.
func TestAgentKeyPermissionHint(t *testing.T) {
	refusal := `{"code":"forbidden","reason":"forbidden","error":"this API key does not have the create_index permission","details":[{"type":"missing_permission","metadata":{"permission":"create_index"}}]}`
	_, _, execute := newAdminServer(t, map[string]adminResponse{
		"GET /organizations/acme/agents/ci-bot":   {200, ciAgent},
		"PATCH /organizations/acme/agents/ci-bot": {403, refusal},
	})
	_, _, err := execute("", "agent", "update", "acme/ci-bot", "--permission", "create_index", "--all-indexes")
	if err == nil || !strings.Contains(err.Error(), "gv login --org acme --permission manage_agents --permission create_index") {
		t.Fatalf("error = %v", err)
	}
}

// A refused secret says why, such as the two-secret limit.
func TestAgentSecretCreateRefused(t *testing.T) {
	_, _, execute := newAdminServer(t, map[string]adminResponse{
		"POST /device/authorizations": {200, `{"device_code":"device","user_code":"ABCD-EFGH","verification_uri":"https://console.example/device","verification_uri_complete":"https://console.example/device?user_code=ABCD-EFGH","expires_in":600,"interval":5}`},
		"POST /device/token":          {400, `{"code":"access_denied","reason":"device_authorization","error":"the agent already has two secrets; remove one first"}`},
	})
	if _, _, err := execute("", "agent", "secret", "create", "acme/ci-bot"); err == nil || !strings.Contains(err.Error(), "already has two secrets; remove one first") {
		t.Fatalf("error = %v", err)
	}
}

// An API key and an agent token each reach the API as one bearer header, on an
// operation that accepts both.
func TestOneAuthorizationHeader(t *testing.T) {
	for _, env := range []map[string]string{
		{"GREPVINE_API_KEY": "gvk_key"},
		{"GREPVINE_AGENT_SECRET": "gvs_secret"},
	} {
		var seen []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/oauth/token" {
				io.WriteString(w, `{"access_token":"gvt_1","token_type":"Bearer","expires_in":900}`)
				return
			}
			seen = r.Header.Values("Authorization")
			io.WriteString(w, `{"results":[],"pagination":{"next_page_id":"","has_next":false}}`)
		}))
		a, _, _ := testApp(t, server.URL+"/v1")
		env["GREPVINE_API_ENDPOINT"] = server.URL + "/v1"
		a.getenv = func(name string) string { return env[name] }
		client, _, _, err := a.client(true)
		if err != nil {
			t.Fatal(err)
		}
		_, err = client.ListIndexes(t.Context(), httpapi.ListIndexesParams{Organization: "acme"})
		server.Close()
		want := "Bearer gvk_key"
		if env["GREPVINE_AGENT_SECRET"] != "" {
			want = "Bearer gvt_1"
		}
		if err != nil || len(seen) != 1 || seen[0] != want {
			t.Fatalf("%v: Authorization = %q, %v; want one %q", env, seen, err, want)
		}
	}
}

// When the API refuses an agent token, as after the secret is rotated, gv
// exchanges the secret again and sends the request once more, body and all.
// It retries once, only a request it can send again, and sends the secret
// only to the token endpoint.
func TestAgentTokenRefused(t *testing.T) {
	var exchanges atomic.Int32
	var mu sync.Mutex
	var seen []string // Authorization and body of every API request
	refuseAll := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/oauth/token" {
			fmt.Fprintf(w, `{"access_token":"gvt_%d","token_type":"Bearer","expires_in":900}`, exchanges.Add(1))
			return
		}
		body, _ := io.ReadAll(r.Body)
		authorization := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, authorization+" "+string(body))
		mu.Unlock()
		if strings.Contains(authorization+string(body), "gvs_") {
			t.Errorf("API saw the secret: %q %q", authorization, body)
		}
		if refuseAll || authorization == "Bearer gvt_1" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"the token was revoked","code":"unauthenticated"}`)
			return
		}
		if r.Method == http.MethodPost {
			io.WriteString(w, `{"results":[],"pagination":{"next_page_id":"","has_next":false}}`)
			return
		}
		io.WriteString(w, acme)
	}))
	defer server.Close()
	reset := func() {
		exchanges.Store(0)
		seen = nil
	}

	a, _, _ := testApp(t, server.URL+"/v1")
	a.getenv = func(name string) string {
		return map[string]string{"GREPVINE_API_ENDPOINT": server.URL + "/v1", "GREPVINE_AGENT_SECRET": "gvs_secret"}[name]
	}
	newClient := func() *httpapi.Client {
		client, _, _, err := a.client(true)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	if _, err := newClient().GetOrganization(t.Context(), httpapi.GetOrganizationParams{Organization: "acme"}); err != nil {
		t.Fatalf("get after a refused token: %v", err)
	}
	if exchanges.Load() != 2 || len(seen) != 2 || seen[1] != "Bearer gvt_2 " {
		t.Fatalf("get: exchanges = %d, requests = %q", exchanges.Load(), seen)
	}

	reset()
	query := &httpapi.QueryRequest{Pattern: "Grepvine"}
	if _, err := newClient().QueryIndex(t.Context(), query, httpapi.QueryIndexParams{Organization: "acme", Index: "docs"}); err != nil {
		t.Fatalf("query after a refused token: %v", err)
	}
	if len(seen) != 2 || !strings.HasPrefix(seen[1], "Bearer gvt_2 {") || strings.TrimPrefix(seen[0], "Bearer gvt_1") != strings.TrimPrefix(seen[1], "Bearer gvt_2") {
		t.Fatalf("query: requests = %q, want the same body sent again", seen)
	}

	reset()
	refuseAll = true
	if _, err := newClient().GetOrganization(t.Context(), httpapi.GetOrganizationParams{Organization: "acme"}); err == nil {
		t.Fatal("get succeeded though every token is refused")
	}
	if exchanges.Load() != 2 || len(seen) != 2 {
		t.Fatalf("refused twice: exchanges = %d, requests = %q, want one retry", exchanges.Load(), seen)
	}

	reset()
	tokens := newAgentTokens("gvs_secret", server.URL, http.DefaultClient)
	streamed, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/organizations/acme/indexes/docs/query", io.NopCloser(strings.NewReader("{}")))
	streamed.Header.Set("Authorization", "Bearer gvt_1")
	response, err := retryRefused{base: http.DefaultTransport, tokens: tokens}.RoundTrip(streamed)
	if err != nil || response.StatusCode != http.StatusUnauthorized || len(seen) != 1 || exchanges.Load() != 0 {
		t.Fatalf("a body that cannot be sent again was retried: %v, %v, %q", response, err, seen)
	}
	_ = response.Body.Close()
}
