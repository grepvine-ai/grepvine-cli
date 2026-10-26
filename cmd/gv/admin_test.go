package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// adminServer answers organization, member and API key calls with canned
// JSON keyed by "METHOD path?query" and records each request as
// "METHOD path body".
type adminServer struct {
	mu        sync.Mutex
	responses map[string]adminResponse
	requests  []string
}

type adminResponse struct {
	status int
	body   string
}

func newAdminServer(t *testing.T, responses map[string]adminResponse) (*adminServer, *app, func(stdin string, args ...string) (string, string, error)) {
	t.Helper()
	s := &adminServer{responses: responses}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		s.mu.Lock()
		s.requests = append(s.requests, strings.TrimSpace(r.Method+" "+path+" "+string(body)))
		s.mu.Unlock()
		key := r.Method + " " + path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		response, ok := responses[key]
		if !ok {
			t.Errorf("unexpected request %s", key)
			response = adminResponse{http.StatusNotFound, `{"code":"not_found","reason":"not_found","error":"not found"}`}
		}
		w.Header().Set("X-Request-Id", "req-1")
		if response.body != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(response.status)
		io.WriteString(w, response.body)
	}))
	t.Cleanup(server.Close)
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	execute := func(stdin string, args ...string) (string, string, error) {
		stdout.Reset()
		stderr.Reset()
		s.mu.Lock()
		s.requests = nil
		s.mu.Unlock()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		err := command.Execute()
		return stdout.String(), stderr.String(), err
	}
	return s, a, execute
}

func (s *adminServer) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func expectRequests(t *testing.T, s *adminServer, want ...string) {
	t.Helper()
	got := s.sent()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const (
	acme    = `{"id":"7a1c0a52-1d43-4b6e-9d38-0c2b7b0e9f10","name":"acme","role":"owner","created_at":"2026-01-02T03:04:05Z"}`
	globex  = `{"id":"2f0e4a8b-5c61-4d7a-8e93-1b2c3d4e5f60","name":"globex","role":"reader","created_at":"2026-02-03T04:05:06Z"}`
	alice   = `{"id":"0b6f3a3e-8c4d-4f0e-a1b2-c3d4e5f60718","name":"alice","email":"alice@example.com","created_at":"2026-01-01T00:00:00Z"}`
	keyID   = "5d9e8f7a-6b5c-4d3e-8f1a-2b3c4d5e6f70"
	apiKey  = `{"id":"` + keyID + `","name":"ci","prefix":"gv_ab12","created_at":"2026-03-04T05:06:07Z"}`
	errBody = `{"code":"forbidden","reason":"forbidden","error":"you cannot manage members of this organization"}`
)

func TestWhoamiPrintsTheSignedInUser(t *testing.T) {
	_, _, execute := newAdminServer(t, map[string]adminResponse{"GET /auth/me": {200, alice}})
	stdout, _, err := execute("", "whoami")
	if err != nil || stdout != "alice <alice@example.com>\n" {
		t.Fatalf("whoami = %q, %v", stdout, err)
	}
}

func TestWhoamiJSON(t *testing.T) {
	_, _, execute := newAdminServer(t, map[string]adminResponse{"GET /auth/me": {200, alice}})
	stdout, _, err := execute("", "whoami", "--json")
	if err != nil || stdout != alice+"\n" {
		t.Fatalf("whoami --json = %q, %v", stdout, err)
	}
}

func TestOrgCommands(t *testing.T) {
	s, _, execute := newAdminServer(t, map[string]adminResponse{
		"GET /organizations":            {200, `{"results":[` + acme + `],"pagination":{"next_page_id":"p2","has_next":true}}`},
		"GET /organizations?page=p2":    {200, `{"results":[` + globex + `],"pagination":{"next_page_id":"","has_next":false}}`},
		"GET /organizations/acme":       {200, acme},
		"POST /organizations":           {201, acme},
		"DELETE /organizations/acme":    {202, ""},
		"PUT /organizations/acme/owner": {204, ""},
		"GET /organizations/missing":    {404, `{"code":"not_found","reason":"not_found","error":"organization not found"}`},
	})

	stdout, _, err := execute("", "org", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ORGANIZATION", "acme", "owner", "2026-01-02T03:04:05Z", "globex", "reader"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("org ls missing %q:\n%s", want, stdout)
		}
	}
	expectRequests(t, s, "GET /organizations", "GET /organizations")

	stdout, _, err = execute("", "org", "ls", "--json")
	if err != nil || stdout != acme+"\n"+globex+"\n" {
		t.Fatalf("org ls --json = %q, %v", stdout, err)
	}

	stdout, _, err = execute("", "org", "info", "acme")
	if err != nil || !strings.Contains(stdout, "Your role:    owner") {
		t.Fatalf("org info = %q, %v", stdout, err)
	}
	if _, _, err = execute("", "org", "info", "missing"); err == nil || !strings.Contains(err.Error(), "organization not found (not_found; request req-1)") {
		t.Fatalf("org info missing error = %v", err)
	}

	stdout, _, err = execute("", "org", "create", "acme")
	if err != nil || stdout != "Created organization acme\n" {
		t.Fatalf("org create = %q, %v", stdout, err)
	}
	expectRequests(t, s, `POST /organizations {"name":"acme"}`)

	for _, answer := range []string{"", "globex\n"} {
		_, stderr, err := execute(answer, "org", "rm", "acme")
		if err == nil || !strings.HasPrefix(err.Error(), "not deleted") || !strings.Contains(stderr, "Type the organization name to confirm") {
			t.Fatalf("org rm with answer %q: err=%v stderr=%q", answer, err, stderr)
		}
		expectRequests(t, s)
	}
	stdout, _, err = execute("acme\n", "org", "rm", "acme")
	if err != nil || stdout != "Deleting organization acme\n" {
		t.Fatalf("org rm = %q, %v", stdout, err)
	}
	expectRequests(t, s, "DELETE /organizations/acme")

	if _, _, err = execute("", "org", "transfer", "acme", "alice"); err == nil || !strings.HasPrefix(err.Error(), "not transferred") {
		t.Fatalf("org transfer without confirmation error = %v", err)
	}
	expectRequests(t, s)
	stdout, _, err = execute("", "org", "transfer", "-f", "acme", "alice")
	if err != nil || stdout != "alice now owns acme\n" {
		t.Fatalf("org transfer = %q, %v", stdout, err)
	}
	expectRequests(t, s, `PUT /organizations/acme/owner {"user":"alice"}`)
}

func TestMemberCommands(t *testing.T) {
	member := `{"user":` + alice + `,"role":"developer","created_at":"2026-04-05T06:07:08Z"}`
	s, _, execute := newAdminServer(t, map[string]adminResponse{
		"GET /organizations/acme/members":          {200, `{"results":[` + member + `],"pagination":{"next_page_id":"","has_next":false}}`},
		"POST /organizations/acme/members":         {201, member},
		"PUT /organizations/acme/members/alice":    {200, strings.Replace(member, "developer", "admin", 1)},
		"DELETE /organizations/acme/members/alice": {204, ""},
		"DELETE /organizations/acme/members/bob":   {403, errBody},
	})

	stdout, _, err := execute("", "member", "ls", "acme")
	if err != nil || !strings.Contains(stdout, "alice") || !strings.Contains(stdout, "alice@example.com") || !strings.Contains(stdout, "developer") {
		t.Fatalf("member ls = %q, %v", stdout, err)
	}

	stdout, _, err = execute("", "member", "add", "acme", "alice", "--role", "developer")
	if err != nil || stdout != "Added alice to acme as developer\n" {
		t.Fatalf("member add = %q, %v", stdout, err)
	}
	expectRequests(t, s, `POST /organizations/acme/members {"user":"alice","role":"developer"}`)

	for _, args := range [][]string{
		{"member", "add", "acme", "alice", "--role", "owner"},
		{"member", "set-role", "acme", "alice", "superuser"},
	} {
		if _, _, err := execute("", args...); err == nil || !strings.Contains(err.Error(), "role must be admin, developer or reader") {
			t.Fatalf("%v error = %v", args, err)
		}
		expectRequests(t, s)
	}
	if _, _, err := execute("", "member", "add", "acme", "alice"); err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatalf("member add without --role error = %v", err)
	}

	stdout, _, err = execute("", "member", "set-role", "acme", "alice", "admin")
	if err != nil || stdout != "alice is now admin in acme\n" {
		t.Fatalf("member set-role = %q, %v", stdout, err)
	}
	expectRequests(t, s, `PUT /organizations/acme/members/alice {"role":"admin"}`)

	if _, _, err = execute("bob\n", "member", "rm", "acme", "alice"); err == nil || !strings.HasPrefix(err.Error(), "not removed") {
		t.Fatalf("member rm mismatch error = %v", err)
	}
	expectRequests(t, s)
	stdout, _, err = execute("alice\n", "member", "rm", "acme", "alice")
	if err != nil || stdout != "Removed alice from acme\n" {
		t.Fatalf("member rm = %q, %v", stdout, err)
	}
	_, _, err = execute("", "member", "rm", "--force", "acme", "bob")
	if err == nil || !strings.Contains(err.Error(), "you cannot manage members of this organization (forbidden; request req-1)") {
		t.Fatalf("member rm forbidden error = %v", err)
	}
}

func TestKeyCommands(t *testing.T) {
	revoked := strings.Replace(apiKey, `"created_at"`, `"revoked_at":"2026-05-06T07:08:09Z","created_at"`, 1)
	s, _, execute := newAdminServer(t, map[string]adminResponse{
		"GET /api-keys":             {200, `{"results":[` + revoked + `],"pagination":{"next_page_id":"","has_next":false}}`},
		"POST /api-keys":            {201, strings.TrimSuffix(apiKey, "}") + `,"key":"gv_ab12secret"}`}, // gitleaks:allow (fake test key)
		"DELETE /api-keys/" + keyID: {204, ""},
	})

	stdout, _, err := execute("", "key", "ls")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{keyID, "ci", "gv_ab12", "2026-05-06T07:08:09Z"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("key ls missing %q:\n%s", want, stdout)
		}
	}
	// The key was never used, so LAST USED is a dash.
	if !strings.Contains(stdout, "  -  ") {
		t.Fatalf("key ls has no dash for an unused key:\n%s", stdout)
	}

	stdout, stderr, err := execute("", "key", "create", "ci")
	if err != nil || stdout != "gv_ab12secret\n" || !strings.Contains(stderr, "not shown again") {
		t.Fatalf("key create stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	expectRequests(t, s, `POST /api-keys {"name":"ci"}`)

	if _, _, err = execute("", "key", "rm", "not-a-uuid"); err == nil || !strings.Contains(err.Error(), "gv key ls") {
		t.Fatalf("key rm invalid ID error = %v", err)
	}
	expectRequests(t, s)
	stdout, _, err = execute("", "key", "rm", keyID)
	if err != nil || stdout != "Revoked API key "+keyID+"\n" {
		t.Fatalf("key rm = %q, %v", stdout, err)
	}
}

func TestSearchReportsAPIErrorsWithRequestID(t *testing.T) {
	_, _, execute := newAdminServer(t, map[string]adminResponse{
		"POST /organizations/acme/indexes/docs/query?amount=200": {404, `{"code":"not_found","reason":"not_found","error":"the resource does not exist or is not visible"}`},
	})
	_, _, err := execute("", "search", "acme/docs", "zebra")
	if err == nil || err.Error() != "search: the resource does not exist or is not visible (not_found; request req-1)" {
		t.Fatalf("search error = %v", err)
	}
}
