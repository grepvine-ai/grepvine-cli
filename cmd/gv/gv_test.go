package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ogen-go/ogen/ogenerrors"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// collectUploadFiles drains the streaming walk the uploader feeds its reader
// pool from. The walk no longer measures a file, so a size limit is checked
// where the content is read; see the upload tests for that.
func collectUploadFiles(t *testing.T, source, prefix string) []uploadFile {
	t.Helper()
	files := make([]uploadFile, 0)
	if err := walkSource(t.Context(), source, prefix, func(file uploadFile) error {
		files = append(files, file)
		return nil
	}, nil); err != nil {
		t.Fatal(err)
	}
	return files
}

// makeUploadJobs runs the encoder over a fixed list of files, as the uploader
// does over the stream the walk produces.
func makeUploadJobs(files []uploadFile, recursive bool) ([]uploadJob, []string, error) {
	names := make(chan uploadFile, len(files)+1)
	for _, file := range files {
		names <- file
	}
	close(names)
	jobs := make([]uploadJob, 0)
	warnings := make([]string, 0)
	err := encodeUploads(context.Background(), names, recursive,
		func(message string) { warnings = append(warnings, message) },
		func(job uploadJob) error { jobs = append(jobs, job); return nil })
	return jobs, warnings, err
}

func testApp(t *testing.T, endpoint string) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	a := &app{
		stdout: stdout,
		stderr: stderr,
		stdin:  strings.NewReader(""),
		getenv: func(name string) string {
			if name == "GREPVINE_ENDPOINT" {
				return endpoint
			}
			return ""
		},
		configPath: filepath.Join(t.TempDir(), ".grepvine", "config.yaml"),
		httpClient: http.DefaultClient,
		open:       func(string) error { return nil },
		sleep:      func(context.Context, time.Duration) error { return nil },
	}
	return a, stdout, stderr
}

func TestConfigPrecedenceAndPermissions(t *testing.T) {
	a, _, _ := testApp(t, "https://environment.example/v1/")
	if err := saveConfig(a.configPath, config{Endpoint: "https://config.example/v1", APIKey: "saved"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	resolved, _, err := a.endpointAndKey()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "https://environment.example/v1" || resolved.APIKey != "saved" {
		t.Fatalf("unexpected resolved config: %+v", resolved)
	}
	a.getenv = func(name string) string {
		if name == "GREPVINE_API_KEY" {
			return "environment-key"
		}
		return ""
	}
	resolved, _, err = a.endpointAndKey()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "https://config.example/v1" || resolved.APIKey != "environment-key" {
		t.Fatalf("unexpected resolved config: %+v", resolved)
	}
}

// TestDefaultOrganization omits ORGANIZATION from index references and from
// commands that take only an organization, with the default from the
// configuration file or GREPVINE_ORGANIZATION.
func TestDefaultOrganization(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}, indexes: map[string]*httpapi.Index{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	execute := func(args ...string) error {
		stdout.Reset()
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}

	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := execute("index", "info", "docs"); err == nil || !strings.Contains(err.Error(), "set a default organization") {
		t.Fatalf("bare index without a default error = %v", err)
	}
	if err := execute("index", "ls"); err == nil || !strings.Contains(err.Error(), "pass ORGANIZATION") {
		t.Fatalf("ls without a default error = %v", err)
	}

	if err := saveConfig(a.configPath, config{APIKey: "test-key", Organization: "acme"}); err != nil {
		t.Fatal(err)
	}
	if err := execute("index", "create", "docs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Index:     acme/docs\n") {
		t.Fatalf("create output = %q", stdout.String())
	}
	if err := execute("index", "ls"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "acme/docs ") {
		t.Fatalf("ls output = %q", stdout.String())
	}

	// An explicit ORGANIZATION/INDEX wins over the default, and
	// GREPVINE_ORGANIZATION wins over the configuration file.
	for _, test := range []struct{ environment, reference, organization string }{
		{"", "docs", "acme"},
		{"", "other/docs", "other"},
		{"beta", "docs", "beta"},
	} {
		a.getenv = func(name string) string {
			if name == "GREPVINE_ORGANIZATION" {
				return test.environment
			}
			return ""
		}
		organization, index, err := a.parseIndexReference(test.reference)
		if err != nil || organization != test.organization || index != "docs" {
			t.Fatalf("%+v: got %q %q %v", test, organization, index, err)
		}
	}
	if _, _, err := a.parseIndexReference("acme/docs/extra"); err == nil {
		t.Fatal("parsed a reference with two slashes")
	}
}

func TestCLIAgainstInProcessService(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "café.txt")
	if err := os.WriteFile(source, []byte("first line\nfind this line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "code", source, "src"); err != nil {
		t.Fatal(err)
	}
	if err := a.getDocument(t.Context(), "acme", "code", "src/café.txt", getDocumentOptions{byteRange: "bytes=6-9"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "line" {
		t.Fatalf("document range = %q, want line", stdout.String())
	}
	stdout.Reset()
	if err := a.listDocuments(t.Context(), "acme", "code", listDocumentsOptions{delimiter: "/", amount: 200}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "src/\n" {
		t.Fatalf("delimiter listing = %q, want src/", stdout.String())
	}
	stdout.Reset()
	matches, err := a.search(t.Context(), "acme", "code", "find", searchOptions{fixed: true, pageSize: 200})
	if err != nil {
		t.Fatal(err)
	}
	if matches != 1 || stdout.String() != "src/café.txt:2:find this line\n" {
		t.Fatalf("matches=%d output=%q", matches, stdout.String())
	}
	stdout.Reset()
	if err := a.deleteDocuments(t.Context(), "acme", "code", []string{"src/café.txt"}, deleteDocumentOptions{ifMatch: "r1"}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "src/café.txt\tr-delete\n" {
		t.Fatalf("delete output = %q", stdout.String())
	}
}

// TestIndexCommandsListShowAndDescribe drives `gv index` through the root
// command against the in-process stack: a description set at creation, read
// back by info and ls, replaced from standard input, cleared with an empty
// flag, and refused when both flags are given.
func TestIndexCommandsListShowAndDescribe(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}, indexes: map[string]*httpapi.Index{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	execute := func(stdin string, args ...string) error {
		stdout.Reset()
		stderr.Reset()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}

	if err := execute("", "index", "create", "acme/docs", "--description", "# Docs\n\nEvery *guide* we ship."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Index:     acme/docs\n") || !strings.Contains(stdout.String(), "Description:\n  # Docs\n\n  Every *guide* we ship.\n") {
		t.Fatalf("create output = %q", stdout.String())
	}
	if err := execute("", "index", "create", "acme/bare"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Description: (none)\n") {
		t.Fatalf("create without a description output = %q", stdout.String())
	}
	if err := execute("", "index", "create", "acme/docs"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("duplicate create error = %v", err)
	}

	if err := execute("", "index", "ls", "acme"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "INDEX ") || !strings.Contains(lines[0], "DESCRIPTION") {
		t.Fatalf("ls output = %q", stdout.String())
	}
	if !strings.HasPrefix(lines[1], "acme/bare ") || !strings.Contains(lines[1], " ready ") {
		t.Fatalf("ls first row = %q", lines[1])
	}
	// The table shows the description's first line, without the heading mark.
	if !strings.HasPrefix(lines[2], "acme/docs ") || !strings.HasSuffix(lines[2], "  Docs") {
		t.Fatalf("ls second row = %q", lines[2])
	}

	// --max-results stops the listing after that many indexes.
	if err := execute("", "index", "ls", "acme", "--max-results", "1"); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "acme/bare ") {
		t.Fatalf("ls --max-results 1 output = %q", stdout.String())
	}

	// --prefix lists only the indexes whose names start with it.
	if err := execute("", "index", "ls", "acme", "--prefix", "do"); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "acme/docs ") {
		t.Fatalf("ls --prefix do output = %q", stdout.String())
	}

	if err := execute("", "index", "ls", "acme", "--json"); err != nil {
		t.Fatal(err)
	}
	var listed []map[string]any
	for line := range strings.SplitSeq(strings.TrimRight(stdout.String(), "\n"), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("ls --json line %q: %v", line, err)
		}
		listed = append(listed, record)
	}
	if len(listed) != 2 || listed[1]["name"] != "docs" || listed[1]["description"] != "# Docs\n\nEvery *guide* we ship." || listed[0]["description"] != "" {
		t.Fatalf("ls --json = %v", listed)
	}

	if err := execute("Guides, runbooks and the API reference.\n", "index", "update", "acme/docs", "--description-file", "-"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Description:\n  Guides, runbooks and the API reference.\n") {
		t.Fatalf("update output = %q", stdout.String())
	}
	if err := execute("", "index", "info", "acme/docs", "--json"); err != nil {
		t.Fatal(err)
	}
	var shown map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown["description"] != "Guides, runbooks and the API reference.\n" || shown["state"] != "ready" {
		t.Fatalf("info --json = %v", shown)
	}

	if err := execute("", "index", "update", "acme/docs", "--description", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Description: (none)\n") {
		t.Fatalf("clear output = %q", stdout.String())
	}
	if err := execute("", "index", "update", "acme/docs"); err == nil || !strings.Contains(err.Error(), "nothing to change") {
		t.Fatalf("update without flags error = %v", err)
	}
	if err := execute("", "index", "update", "acme/docs", "--description", "a", "--description-file", "-"); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("update with both flags error = %v", err)
	}
	if err := execute("", "index", "info", "acme/missing"); err == nil || !strings.Contains(err.Error(), "not_found") || !strings.Contains(err.Error(), "req-404") {
		t.Fatalf("info of a missing index error = %v", err)
	}
	if err := execute("", "index", "update", "acme/missing", "--description", "x"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("update of a missing index error = %v", err)
	}
}

// TestIndexRemoveConfirmsUnlessForced drives `gv index rm`: a wrong or
// missing confirmation keeps the index, the typed name deletes it, --force
// skips the prompt, and a missing index reports not found.
// Index names keep the case they were created with, and the service matches
// them without regard to it, so gv sends a mixed-case name as typed and
// accepts one in every response.
func TestIndexCommandsKeepMixedCaseNames(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}, indexes: map[string]*httpapi.Index{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	execute := func(stdin string, args ...string) error {
		stdout.Reset()
		stderr.Reset()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}

	if err := execute("", "index", "create", "Acme/MyIndex"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Index:     Acme/MyIndex\n") {
		t.Fatalf("create output = %q", stdout.String())
	}
	if err := execute("", "index", "create", "acme/myindeX"); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("create of another spelling error = %v", err)
	}
	if err := execute("", "index", "info", "acme/MYINDEX"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Index:     Acme/MyIndex\n") {
		t.Fatalf("info output = %q", stdout.String())
	}
	if err := execute("", "index", "ls", "Acme", "--prefix", "MY"); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n"); len(lines) != 2 || !strings.HasPrefix(lines[1], "Acme/MyIndex ") {
		t.Fatalf("ls --prefix MY output = %q", stdout.String())
	}
	if err := execute("", "index", "update", "acme/myindex", "--description", "Mixed case."); err != nil {
		t.Fatal(err)
	}
	if err := execute("myindex\n", "index", "rm", "acme/myindex"); err != nil {
		t.Fatal(err)
	}
	if err := execute("", "index", "info", "Acme/MyIndex"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("info after delete error = %v", err)
	}
}

func TestIndexRemoveConfirmsUnlessForced(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}, indexes: map[string]*httpapi.Index{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	execute := func(stdin string, args ...string) error {
		stdout.Reset()
		stderr.Reset()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}
	exists := func(name string) bool {
		stack.mu.Lock()
		defer stack.mu.Unlock()
		_, ok := stack.indexes[name]
		return ok
	}
	for _, name := range []string{"acme/docs", "acme/scratch"} {
		if err := execute("", "index", "create", name); err != nil {
			t.Fatal(err)
		}
	}

	for _, answer := range []string{"", "scratch\n", "acme/docs\n"} {
		if err := execute(answer, "index", "rm", "acme/docs"); err == nil || !strings.Contains(err.Error(), "not deleted") || !strings.Contains(err.Error(), "--force") {
			t.Fatalf("rm with answer %q error = %v", answer, err)
		}
		if !strings.Contains(stderr.String(), "Type the index name to confirm") {
			t.Fatalf("rm prompt = %q", stderr.String())
		}
		if !exists("docs") {
			t.Fatalf("rm with answer %q deleted the index", answer)
		}
	}
	if err := execute("docs\n", "index", "rm", "acme/docs"); err != nil {
		t.Fatal(err)
	}
	if exists("docs") || stdout.String() != "Deleting acme/docs\n" {
		t.Fatalf("confirmed rm: exists=%v output=%q", exists("docs"), stdout.String())
	}
	if err := execute("", "index", "rm", "--force", "acme/scratch"); err != nil {
		t.Fatal(err)
	}
	if exists("scratch") || stderr.String() != "" {
		t.Fatalf("forced rm: exists=%v stderr=%q", exists("scratch"), stderr.String())
	}
	if err := execute("", "index", "rm", "-f", "acme/missing"); err == nil || !strings.Contains(err.Error(), "not_found") || !strings.Contains(err.Error(), "req-404") {
		t.Fatalf("rm of a missing index error = %v", err)
	}
}

func TestDescriptionSummaryTakesTheFirstLine(t *testing.T) {
	for _, test := range []struct{ in, want string }{
		{"", ""},
		{"\n\n# Docs\n\nMore.", "Docs"},
		{"plain text", "plain text"},
		{"   ##   Heading with spaces  ", "Heading with spaces"},
		{strings.Repeat("é", 70), strings.Repeat("é", 59) + "…"},
	} {
		if got := descriptionSummary(test.in, 60); got != test.want {
			t.Errorf("descriptionSummary(%q) = %q, want %q", test.in, got, test.want)
		}
	}
}

func TestDeviceLoginCapturesEnvironmentEndpoint(t *testing.T) {
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/device/authorizations":
			fmt.Fprintf(w, `{"device_code":"device","user_code":"ABCD-EFGH","verification_uri":%q,"verification_uri_complete":%q,"expires_in":600,"interval":5}`,
				serverURL(r)+"/device", serverURL(r)+"/device?user_code=ABCD-EFGH")
		case "/v1/device/token":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"code":"authorization_pending","reason":"device","error":"authorization is pending"}`)
				return
			}
			io.WriteString(w, `{"id":"0198f13d-f525-7f84-a98e-3c0264919c42","name":"gv","prefix":"gv_test","created_at":"2026-08-27T12:00:00Z","key":"secret-device-key"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "old-key", Organization: "acme"}); err != nil {
		t.Fatal(err)
	}
	if err := a.login(t.Context(), "gv"); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := a.loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	// Login replaces the key and keeps the default organization.
	if cfg.Endpoint != server.URL+"/v1" || cfg.APIKey != "secret-device-key" || cfg.Organization != "acme" {
		t.Fatalf("saved config = %+v", cfg)
	}
	if !strings.Contains(stdout.String(), "ABCD-EFGH") || polls != 2 {
		t.Fatalf("output=%q polls=%d", stdout.String(), polls)
	}
}

func TestRecursiveUploadPreservesNamesMetadataEmptyFilesAndRetries(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "café.txt"), []byte("bonjour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nested", "café.txt"), filepath.Join(root, "ignored-link")); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	requests := 0
	revisions := map[string]string{}
	committedIDs := map[string]bool{}
	revisionWrites := 0
	keys := map[string]uploadRecord{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/objects:batch") {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing bearer key")
		}
		idempotency := r.Header.Get("Idempotency-Key")
		scanner := bufio.NewScanner(r.Body)
		var records []uploadRecord
		for scanner.Scan() {
			var record uploadRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				t.Error(err)
			}
			records = append(records, record)
		}
		mu.Lock()
		requests++
		if !committedIDs[idempotency] {
			committedIDs[idempotency] = true
			for _, record := range records {
				keys[record.Key] = record
				revisionWrites++
				revisions[record.Key] = fmt.Sprintf("revision-%d", revisionWrites)
			}
		}
		first := requests == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if first {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"code":"unavailable","reason":"upstream","error":"retry"}`)
			return
		}
		io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()
	a, _, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "code", root, "archive/"); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "code", root, "archive/"); err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || len(keys) != 2 {
		t.Fatalf("revisions=%v keys=%v", revisions, keys)
	}
	if revisionWrites != 2 {
		t.Fatalf("retry/resume wrote %d revisions, want 2", revisionWrites)
	}
	unicodeRecord := keys["archive/nested/café.txt"]
	if unicodeRecord.Content != "bonjour\n" || unicodeRecord.IfNoneMatch != "" {
		t.Fatalf("unicode record = %+v", unicodeRecord)
	}
	if keys["archive/empty.txt"].Content != "" {
		t.Fatal("empty file content was not preserved")
	}
	if _, present := keys["archive/ignored-link"]; present {
		t.Fatal("symbolic link was followed")
	}
	if unicodeRecord.Metadata["path"] == nil || unicodeRecord.Metadata["size"] == nil || unicodeRecord.Metadata["mtime"] == nil {
		t.Fatalf("metadata not preserved: %#v", unicodeRecord.Metadata)
	}
	if stderr.Len() != 0 {
		t.Fatalf("non-TTY progress output = %q", stderr.String())
	}
}

func TestRecursiveUploadUsesLatencyBoundedBatches(t *testing.T) {
	root := t.TempDir()
	files := make([]uploadFile, uploadBatchFiles+1)
	for ordinal := range files {
		name := fmt.Sprintf("file-%03d.txt", ordinal)
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files[ordinal] = uploadFile{localPath: path, key: name}
	}
	jobs, warnings, err := makeUploadJobs(files, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if len(jobs) != 2 || len(jobs[0].files) != uploadBatchFiles || len(jobs[1].files) != 1 {
		t.Fatalf("batch sizes = %v, want [%d 1]", []int{len(jobs[0].files), len(jobs[1].files)}, uploadBatchFiles)
	}
}

func TestRecursiveUploadHonorsGitIgnoreAndPrunesGitDirectory(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{".git", "generated/cache", "generated/deep/cache", "ignored", "nested"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		".git/config":                   "private repository data",
		".gitignore":                    "*.log\n!important.log\nignored/\n/root-only.txt\ngenerated/**/cache/*.bin\n",
		"debug.log":                     "ignored",
		"important.log":                 "included",
		"ignored/value.txt":             "ignored",
		"keep.txt":                      "included",
		"generated/cache/drop.bin":      "ignored",
		"generated/cache/keep.txt":      "included",
		"generated/deep/cache/drop.bin": "ignored",
		"nested/.gitignore":             "*.tmp\n!keep.tmp\n",
		"nested/debug.log":              "ignored",
		"nested/drop.tmp":               "ignored",
		"nested/important.log":          "included",
		"nested/keep.tmp":               "included",
		"nested/root-only.txt":          "included",
		"root-only.txt":                 "ignored",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	largePath := filepath.Join(root, "large.bin")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(maxDocumentSize + 1); err != nil {
		large.Close()
		t.Fatal(err)
	}
	if err = large.Close(); err != nil {
		t.Fatal(err)
	}
	limitPath := filepath.Join(root, "limit.bin")
	limit, err := os.Create(limitPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = limit.Truncate(maxDocumentSize); err != nil {
		limit.Close()
		t.Fatal(err)
	}
	if err = limit.Close(); err != nil {
		t.Fatal(err)
	}

	collected := collectUploadFiles(t, root, "archive")
	keys := make([]string, len(collected))
	for i, file := range collected {
		keys[i] = file.key
	}
	want := []string{
		"archive/.gitignore",
		"archive/generated/cache/keep.txt",
		"archive/important.log",
		"archive/keep.txt",
		"archive/large.bin",
		"archive/limit.bin",
		"archive/nested/.gitignore",
		"archive/nested/important.log",
		"archive/nested/keep.tmp",
		"archive/nested/root-only.txt",
	}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Fatalf("collected keys = %v, want %v", keys, want)
	}
	_, warnings, err := makeUploadJobs(collected, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "large.bin") || !strings.Contains(warnings[0], "1 GiB") {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestRecursiveUploadWithOnlyOversizedFilesWarnsWithoutFailing(t *testing.T) {
	root := t.TempDir()
	largePath := filepath.Join(root, "large.bin")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(maxDocumentSize + 1); err != nil {
		large.Close()
		t.Fatal(err)
	}
	if err = large.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &app{stderr: &stderr}
	if err = a.upload(t.Context(), "acme", "code", root, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "gv: warning:") || !strings.Contains(stderr.String(), "large.bin") {
		t.Fatalf("warning output = %q", stderr.String())
	}
}

func TestSingleOversizedFileStillFails(t *testing.T) {
	largePath := filepath.Join(t.TempDir(), "large.bin")
	large, err := os.Create(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(maxDocumentSize + 1); err != nil {
		large.Close()
		t.Fatal(err)
	}
	if err = large.Close(); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &app{stderr: &stderr}
	err = a.upload(t.Context(), "acme", "code", largePath, "")
	if err == nil || !strings.Contains(err.Error(), "1 GiB") {
		t.Fatalf("error = %v", err)
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Fatalf("a single oversized file warned instead of failing: %q", stderr.String())
	}
}

func TestRecursiveUploadStoresBinaryFilesOnTheirOwn(t *testing.T) {
	// A binary file is not skipped: the service stores content it cannot
	// search. It cannot travel in the JSON batch, so it gets a streamed request
	// of its own, after the batch of the text files that preceded it so the
	// requests leave in source order.
	root := t.TempDir()
	textPath := filepath.Join(root, "Documentation", "README.md")
	binaryPath := filepath.Join(root, "Documentation", "images", "logo.gif")
	if err := os.MkdirAll(filepath.Dir(binaryPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(textPath, []byte("![logo](images/logo.gif)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binaryPath, []byte{'G', 'I', 'F', 0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, warnings, err := makeUploadJobs([]uploadFile{
		{localPath: textPath, key: "Documentation/README.md"},
		{localPath: binaryPath, key: "Documentation/images/logo.gif"},
	}, true)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings = %v, error = %v", warnings, err)
	}
	if len(jobs) != 2 || jobs[0].data == nil || len(jobs[0].files) != 1 || jobs[0].files[0].key != "Documentation/README.md" {
		t.Fatalf("jobs = %#v", jobs)
	}
	if jobs[1].data != nil || len(jobs[1].files) != 1 || jobs[1].files[0].key != "Documentation/images/logo.gif" || jobs[1].bytes != 5 {
		t.Fatalf("binary job = %#v", jobs[1])
	}
}

func TestLargeBinaryFileIsStreamedOnItsOwn(t *testing.T) {
	root := t.TempDir()
	binaryPath := filepath.Join(root, "large.dat")
	binaryFile, err := os.Create(binaryPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = binaryFile.Truncate(maxBatchContent + 1); err != nil {
		binaryFile.Close()
		t.Fatal(err)
	}
	if _, err = binaryFile.WriteAt([]byte{0xff}, 0); err != nil {
		binaryFile.Close()
		t.Fatal(err)
	}
	if err = binaryFile.Close(); err != nil {
		t.Fatal(err)
	}
	jobs, warnings, err := makeUploadJobs([]uploadFile{{localPath: binaryPath, key: "large.dat"}}, true)
	if err != nil || len(jobs) != 1 || len(warnings) != 0 || jobs[0].data != nil || jobs[0].bytes != maxBatchContent+1 {
		t.Fatalf("jobs = %v, warnings = %v, error = %v", jobs, warnings, err)
	}
}

func TestSingleBinaryFileIsUploadedOnItsOwn(t *testing.T) {
	binaryPath := filepath.Join(t.TempDir(), "logo.gif")
	if err := os.WriteFile(binaryPath, []byte{'G', 'I', 'F', 0xff, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	jobs, warnings, err := makeUploadJobs([]uploadFile{{localPath: binaryPath, key: "logo.gif"}}, false)
	if err != nil || len(jobs) != 1 || len(warnings) != 0 || jobs[0].data != nil {
		t.Fatalf("jobs = %v, warnings = %v, error = %v", jobs, warnings, err)
	}
}

func TestSearchPaginationFlagsAndExitCodes(t *testing.T) {
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		if request["mode"] != "literal" || request["case_sensitive"] != false || request["whole_word"] != true {
			t.Errorf("query request = %#v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		page++
		if page == 1 {
			io.WriteString(w, `{"results":[{"key":"café.txt","revision":"r1","line_number":2,"last_line_number":2,"line_start_offset":4,"line_end_offset":9,"text_start_offset":4,"text_line_number":2,"text":"Hello","matches":[],"text_truncated":false,"matches_truncated":false}],"pagination":{"next_page_id":"next","has_next":true}}`)
			return
		}
		io.WriteString(w, `{"results":[{"key":"empty.txt","revision":"r2","line_number":1,"last_line_number":1,"line_start_offset":0,"line_end_offset":5,"text_start_offset":0,"text_line_number":1,"text":"hello","matches":[],"text_truncated":false,"matches_truncated":false}],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme", "code", "hello", "-F", "-i", "-w"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != "café.txt:2:Hello\nempty.txt:1:hello\n" {
		t.Fatalf("output = %q", got)
	}

	emptyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer emptyServer.Close()
	a, _, _ = testApp(t, emptyServer.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.search(t.Context(), "acme", "code", "missing", searchOptions{pageSize: 200}); err != nil {
		t.Fatal(err)
	}
	a.exitCode = 1
	if a.exitCode != 1 {
		t.Fatal("no-match exit code is not 1")
	}
}

func TestUploadResumeAfterInterruptDoesNotDuplicateRevision(t *testing.T) {
	committed := map[string]bool{}
	commits := 0
	first := true
	ctx, cancel := context.WithCancel(t.Context())
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		idempotency := request.Header.Get("Idempotency-Key")
		if !committed[idempotency] {
			committed[idempotency] = true
			commits++
		}
		if first {
			first = false
			cancel()
			return nil, context.Canceled
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"results":[{"key":"resume.txt","revision":"r1"}]}`)),
			Request:    request,
		}, nil
	})
	a := &app{sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }}
	client, err := httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	job := uploadJob{files: []uploadFile{{key: "resume.txt"}}, data: []byte(`{"key":"resume.txt","content":"value","if_none_match":"*"}` + "\n"), bytes: 5}
	if _, err := a.sendUploadJob(ctx, client, "acme", "code", job); err == nil {
		t.Fatal("interrupted upload unexpectedly succeeded")
	}
	if failed, err := a.sendUploadJob(t.Context(), client, "acme", "code", job); err != nil || len(failed) != 0 {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	if commits != 1 || len(committed) != 1 {
		t.Fatalf("resume produced commits=%d idempotency_keys=%d", commits, len(committed))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type fakeService struct {
	httpapi.UnimplementedHandler
	mu        sync.Mutex
	objects   map[string]string
	revisions map[string]string
	// indexes holds the indexes the index commands manage, keyed by name.
	indexes map[string]*httpapi.Index
	// metadata holds what each stored document was described with, so a
	// listing reports it the way the service does.
	metadata map[string]httpapi.Metadata
	// refuse names the keys whose writes the stack reports as not applied, as
	// the service does for a document whose precondition failed; a batch
	// applies every other document it holds.
	refuse map[string]bool
	// Counters the sync tests assert on: every write the stack accepted, the
	// keys the batches stored, the sizes of the delete batches it applied, the
	// keys a delete batch found already gone, the single deletes, and how
	// often each delete idempotency key was presented.
	writes         int
	putKeys        []string
	batchSizes     []int
	missingDeletes int
	singleDeletes  int
	deleteIDs      map[string]int
	listings       int
}

// refusedResult is the result the stack reports for a document in refuse.
func refusedResult(key string) httpapi.MutationResult {
	return httpapi.MutationResult{Key: key, Error: httpapi.NewOptError(httpapi.Error{Code: "precondition_failed", Reason: "precondition", Error: "the document's precondition failed"})}
}

// NewError maps the stack's errors to the public error shape, as the
// service does. Without it the embedded handler answers status zero.
func (s *fakeService) NewError(_ context.Context, err error) *httpapi.ErrorStatusCodeWithHeaders {
	return &httpapi.ErrorStatusCodeWithHeaders{StatusCode: http.StatusInternalServerError, Response: httpapi.Error{Code: "internal_error", Reason: "internal", Error: err.Error()}}
}

// DeleteObjects applies each document of the batch on its own, as the
// service does: a missing key is reported as not found and the rest are
// deleted.
func (s *fakeService) DeleteObjects(_ context.Context, request *httpapi.DeleteObjectsRequest, params httpapi.DeleteObjectsParams) (*httpapi.MutationBatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteIDs == nil {
		s.deleteIDs = map[string]int{}
	}
	s.deleteIDs[params.IdempotencyKey]++
	results := make([]httpapi.MutationResult, 0, len(request.Objects))
	for _, object := range request.Objects {
		if s.refuse[object.Key] {
			results = append(results, refusedResult(object.Key))
			continue
		}
		if _, exists := s.objects[object.Key]; !exists {
			s.missingDeletes++
			results = append(results, httpapi.MutationResult{Key: object.Key, Error: httpapi.NewOptError(httpapi.Error{Code: "not_found", Reason: "resource", Error: "the document does not exist"})})
			continue
		}
		delete(s.objects, object.Key)
		delete(s.metadata, object.Key)
		results = append(results, httpapi.MutationResult{Key: object.Key, Revision: httpapi.NewOptString("r-delete")})
	}
	s.batchSizes = append(s.batchSizes, len(request.Objects))
	s.writes++
	return &httpapi.MutationBatchResult{Results: results}, nil
}

// metadataFromRecord rebuilds the typed metadata a batch record carried, so a
// listing reports the size and modification time the uploader wrote.
func metadataFromRecord(fields map[string]any) httpapi.Metadata {
	metadata := httpapi.Metadata{}
	for name, raw := range fields {
		field, _ := raw.(map[string]any)
		switch field["type"] {
		case "timestamp":
			if stamp, err := time.Parse(time.RFC3339Nano, fmt.Sprint(field["value"])); err == nil {
				metadata[name] = httpapi.NewTimestampMetadataValueMetadataValue(httpapi.TimestampMetadataValue{Type: "timestamp", Value: stamp})
			}
		case "integer":
			number, _ := field["value"].(float64)
			metadata[name] = httpapi.NewIntegerMetadataValueMetadataValue(httpapi.IntegerMetadataValue{Type: "integer", Value: int64(number)})
		case "string":
			metadata[name] = httpapi.NewStringMetadataValueMetadataValue(httpapi.StringMetadataValue{Type: "string", Value: fmt.Sprint(field["value"])})
		}
	}
	return metadata
}

func (s *fakeService) ListIndexes(_ context.Context, params httpapi.ListIndexesParams) (*httpapi.IndexPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.indexes))
	for name := range s.indexes {
		if strings.HasPrefix(name, strings.ToLower(params.Prefix.Or(""))) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	// One index per page, so the command's paging is exercised.
	cursor, _ := params.Page.Get()
	results := make([]httpapi.IndexSummary, 0, 1)
	page := httpapi.Pagination{}
	for i, name := range names {
		if name <= cursor {
			continue
		}
		v := s.indexes[name]
		results = append(results, httpapi.IndexSummary{ID: v.ID, Organization: v.Organization, Name: v.Name, CanonicalID: v.CanonicalID, Description: v.Description, Tags: v.Tags, State: v.State, DocumentCount: v.DocumentCount, ContentBytes: v.ContentBytes, CreatedAt: v.CreatedAt, LastUpdatedAt: v.LastUpdatedAt})
		if i+1 < len(names) {
			page = httpapi.Pagination{HasNext: true, NextPageID: name}
		}
		break
	}
	return &httpapi.IndexPage{Results: results, Pagination: page}, nil
}

func (s *fakeService) GetIndex(_ context.Context, params httpapi.GetIndexParams) (httpapi.GetIndexRes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.indexes[strings.ToLower(string(params.Index))]
	if !ok {
		return &httpapi.NotFoundHeaders{XRequestID: httpapi.NewOptString("req-404"), Response: httpapi.Error{Code: "not_found", Reason: "resource", Error: "the resource does not exist or is not visible"}}, nil
	}
	copied := *index
	return &copied, nil
}

func (s *fakeService) CreateIndex(_ context.Context, request *httpapi.CreateIndexRequest, params httpapi.CreateIndexParams) (httpapi.CreateIndexRes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.indexes[strings.ToLower(string(request.Name))]; exists {
		return &httpapi.ConflictHeaders{Response: httpapi.Error{Code: "conflict", Reason: "state", Error: "the request conflicts with current state"}}, nil
	}
	index := &httpapi.Index{
		ID: uuid.New(), Organization: params.Organization, Name: request.Name, CanonicalID: "canonical", Description: request.Description.Or(""),
		Tags: httpapi.IndexTags{}, State: httpapi.IndexStateReady, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	maps.Copy(index.Tags, request.Tags.Or(nil))
	s.indexes[strings.ToLower(string(request.Name))] = index
	copied := *index
	return &copied, nil
}

func (s *fakeService) UpdateIndex(_ context.Context, request *httpapi.UpdateIndexRequest, params httpapi.UpdateIndexParams) (httpapi.UpdateIndexRes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index, ok := s.indexes[strings.ToLower(string(params.Index))]
	if !ok {
		return &httpapi.NotFoundHeaders{Response: httpapi.Error{Code: "not_found", Reason: "resource", Error: "the resource does not exist or is not visible"}}, nil
	}
	if description, ok := request.Description.Get(); ok {
		index.Description = description
	}
	if changes, ok := request.Tags.Get(); ok {
		if index.Tags == nil {
			index.Tags = httpapi.IndexTags{}
		}
		for key, change := range changes {
			if value, ok := change.Get(); ok {
				index.Tags[key] = value
			} else {
				delete(index.Tags, key)
			}
		}
	}
	s.writes++
	copied := *index
	return &copied, nil
}

func (s *fakeService) DeleteIndex(_ context.Context, params httpapi.DeleteIndexParams) (httpapi.DeleteIndexRes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.indexes[strings.ToLower(string(params.Index))]; !ok {
		return &httpapi.NotFoundHeaders{XRequestID: httpapi.NewOptString("req-404"), Response: httpapi.Error{Code: "not_found", Reason: "resource", Error: "the resource does not exist or is not visible"}}, nil
	}
	delete(s.indexes, strings.ToLower(string(params.Index)))
	return &httpapi.DeleteIndexAccepted{}, nil
}

//nolint:revive // implements the generated ogen interface
func (s *fakeService) HandleApiKeyAuth(ctx context.Context, _ httpapi.OperationName, token httpapi.ApiKeyAuth) (context.Context, error) {
	if token.Token != "test-key" {
		return nil, errors.New("invalid test key")
	}
	return ctx, nil
}

func (*fakeService) HandleCookieAuth(ctx context.Context, _ httpapi.OperationName, _ httpapi.CookieAuth) (context.Context, error) {
	return ctx, ogenerrors.ErrSkipServerSecurity
}

func (s *fakeService) PutObjects(_ context.Context, request httpapi.PutObjectsReq, params httpapi.PutObjectsParams) (httpapi.PutObjectsRes, error) {
	scanner := bufio.NewScanner(request.Data)
	results := make([]httpapi.MutationResult, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	for scanner.Scan() {
		var record uploadRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		if s.refuse[record.Key] {
			results = append(results, refusedResult(record.Key))
			continue
		}
		revision := s.revisions[params.IdempotencyKey+record.Key]
		if revision == "" {
			revision = fmt.Sprintf("r%d", len(s.revisions)+1)
			s.revisions[params.IdempotencyKey+record.Key] = revision
			s.objects[record.Key] = record.Content
			if s.metadata == nil {
				s.metadata = map[string]httpapi.Metadata{}
			}
			s.metadata[record.Key] = metadataFromRecord(record.Metadata)
			s.putKeys = append(s.putKeys, record.Key)
			s.writes++
		}
		results = append(results, httpapi.MutationResult{Key: record.Key, Revision: httpapi.NewOptString(revision)})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return &httpapi.MutationBatchResult{Results: results}, nil
}

func (s *fakeService) QueryIndex(_ context.Context, request *httpapi.QueryRequest, _ httpapi.QueryIndexParams) (*httpapi.QueryPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results := make([]httpapi.QueryResult, 0)
	for key, content := range s.objects {
		offset := 0
		for lineIndex, line := range strings.Split(content, "\n") {
			match := strings.Index(line, request.Pattern)
			if match >= 0 {
				results = append(results, httpapi.QueryResult{Key: key, Revision: "r1", LineNumber: lineIndex + 1, LastLineNumber: lineIndex + 1, TextLineNumber: lineIndex + 1, LineStartOffset: offset, LineEndOffset: offset + len(line), TextStartOffset: offset, Text: line, Matches: []httpapi.Match{{Offset: offset + match, Length: len(request.Pattern)}}})
			}
			offset += len(line) + 1
		}
	}
	return &httpapi.QueryPage{Results: results, Pagination: httpapi.Pagination{}}, nil
}

func (s *fakeService) GetDocument(_ context.Context, params httpapi.GetDocumentParams) (httpapi.GetDocumentRes, error) {
	s.mu.Lock()
	content, exists := s.objects[params.Key]
	s.mu.Unlock()
	if !exists {
		return nil, errors.New("document not found")
	}
	if value, ok := params.Range.Get(); ok && value == "bytes=6-9" {
		return &httpapi.GetDocumentPartialContentHeaders{
			AcceptRanges: httpapi.NewOptString("bytes"), ContentLength: httpapi.NewOptInt(4),
			ContentRange: httpapi.NewOptString(fmt.Sprintf("bytes 6-9/%d", len(content))), ETag: httpapi.NewOptString("r1"),
			XGrepvineMetadata: httpapi.NewOptString("{}"), Response: httpapi.GetDocumentPartialContent{Data: strings.NewReader(content[6:10])},
		}, nil
	}
	return &httpapi.GetDocumentOKHeaders{
		AcceptRanges: httpapi.NewOptString("bytes"), ContentLength: httpapi.NewOptInt(len(content)), ETag: httpapi.NewOptString("r1"),
		XGrepvineMetadata: httpapi.NewOptString("{}"), Response: httpapi.GetDocumentOK{Data: strings.NewReader(content)},
	}, nil
}

// stackPageSize is how many documents one listing page of the stack holds, so
// a command's paging is exercised by a handful of files.
const stackPageSize = 2

func (s *fakeService) ListDocuments(_ context.Context, params httpapi.ListDocumentsParams) (*httpapi.DocumentPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listings++
	prefix, _ := params.Prefix.Get()
	delimiter, _ := params.Delimiter.Get()
	cursor, _ := params.Page.Get()
	common := map[string]struct{}{}
	documents := make([]httpapi.DocumentSummary, 0)
	for key, content := range s.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if delimiter != "" {
			if offset := strings.Index(strings.TrimPrefix(key, prefix), delimiter); offset >= 0 {
				common[prefix+strings.TrimPrefix(key, prefix)[:offset+len(delimiter)]] = struct{}{}
				continue
			}
		}
		if key <= cursor {
			continue
		}
		metadata := s.metadata[key]
		if metadata == nil {
			metadata = httpapi.Metadata{}
		}
		documents = append(documents, httpapi.DocumentSummary{
			Key: key, Revision: "r1", ContentBytes: len(content), Metadata: metadata, UpdatedAt: time.Unix(0, 0).UTC(), Searchable: true,
		})
	}
	prefixes := make([]string, 0, len(common))
	if cursor == "" {
		for value := range common {
			prefixes = append(prefixes, value)
		}
	}
	sort.Strings(prefixes)
	sort.Slice(documents, func(i, j int) bool { return documents[i].Key < documents[j].Key })
	page := httpapi.Pagination{}
	if len(documents) > stackPageSize {
		documents = documents[:stackPageSize]
		page = httpapi.Pagination{HasNext: true, NextPageID: documents[len(documents)-1].Key}
	}
	return &httpapi.DocumentPage{Results: documents, CommonPrefixes: prefixes, Pagination: page}, nil
}

// PutDocument stores one document with the metadata it carries, and refuses
// to replace one when the request asks for an absent key.
func (s *fakeService) PutDocument(_ context.Context, request *httpapi.PutDocumentFormMultipart, params httpapi.PutDocumentParams) (httpapi.PutDocumentRes, error) {
	content, err := io.ReadAll(request.Content.File)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.objects[request.Key]; exists && params.IfNoneMatch.Or("") == "*" {
		return &httpapi.PreconditionFailedHeaders{Response: httpapi.Error{Code: "precondition_failed", Reason: "precondition", Error: "the document's precondition failed"}}, nil
	}
	s.objects[request.Key] = string(content)
	if fields, ok := request.Metadata.Get(); ok {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(fields), &decoded); err != nil {
			return nil, err
		}
		if s.metadata == nil {
			s.metadata = map[string]httpapi.Metadata{}
		}
		s.metadata[request.Key] = metadataFromRecord(decoded)
	}
	s.writes++
	return &httpapi.MutationResult{Key: request.Key, Revision: httpapi.NewOptString("r-put")}, nil
}

func (s *fakeService) DeleteDocument(_ context.Context, params httpapi.DeleteDocumentParams) (httpapi.DeleteDocumentRes, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.singleDeletes++
	if _, exists := s.objects[params.Key]; !exists {
		return &httpapi.NotFoundHeaders{Response: httpapi.Error{Code: "not_found", Reason: "document", Error: "document not found"}}, nil
	}
	delete(s.objects, params.Key)
	delete(s.metadata, params.Key)
	s.writes++
	return &httpapi.MutationResult{Key: params.Key, Revision: httpapi.NewOptString("r-delete")}, nil
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func TestUploadSlowsDownOnTooManyRequestsWithoutFailing(t *testing.T) {
	root := t.TempDir()
	for ordinal := range 6 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file-%d.txt", ordinal)), []byte(fmt.Sprintf("content %d\n", ordinal)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	requests, rejected := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/objects:batch") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		requests++
		reject := rejected < uploadRetryAttempts+1
		if reject {
			rejected++
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if reject {
			// More rejections than the ordinary retry budget: a 429 must
			// never count as a failed attempt.
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":"write_backlog","reason":"capacity","error":"the index is busy absorbing recent writes; slow down and retry"}`)
			return
		}
		io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()
	a, _, stderr := testApp(t, server.URL+"/v1")
	var sleepMu sync.Mutex
	var sleeps []time.Duration
	a.sleep = func(_ context.Context, duration time.Duration) error {
		sleepMu.Lock()
		defer sleepMu.Unlock()
		sleeps = append(sleeps, duration)
		return nil
	}
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "code", root, "archive/"); err != nil {
		t.Fatalf("upload failed instead of slowing down: %v", err)
	}
	if rejected != uploadRetryAttempts+1 || requests < uploadRetryAttempts+2 {
		t.Fatalf("requests = %d, rejected = %d; want every rejected batch retried", requests, rejected)
	}
	honored := 0
	for _, duration := range sleeps {
		// Retry-After of two seconds, with up to 25% of jitter either way.
		if duration >= 1500*time.Millisecond && duration <= 2500*time.Millisecond {
			honored++
		}
	}
	if honored < uploadRetryAttempts+1 {
		t.Fatalf("Retry-After honored %d times (sleeps %v), want more than the ordinary retry budget", honored, sleeps)
	}
	if notices := strings.Count(stderr.String(), "gv: slowing down:"); notices != 1 {
		t.Fatalf("slow-down notices = %d, want exactly one per pause:\n%s", notices, stderr.String())
	}
}

func TestSearchRetriesAfterTooManyRequests(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query") {
			http.NotFound(w, r)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":"capacity_exhausted","reason":"capacity","error":"request capacity is exhausted; slow down and retry"}`)
			return
		}
		io.WriteString(w, `{"results":[{"key":"a.txt","revision":"r1","line_number":1,"last_line_number":1,"line_start_offset":0,"line_end_offset":5,"text_start_offset":0,"text_line_number":1,"text":"hello","matches":[],"text_truncated":false,"matches_truncated":false}],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	var slept time.Duration
	a.sleep = func(_ context.Context, duration time.Duration) error { slept += duration; return nil }
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	matches, err := a.search(t.Context(), "acme", "code", "hello", searchOptions{pageSize: 50})
	if err != nil || matches != 1 || calls != 2 {
		t.Fatalf("search = %d matches, %d calls, %v", matches, calls, err)
	}
	if stdout.String() != "a.txt:1:hello\n" || slept < 750*time.Millisecond || !strings.Contains(stderr.String(), "gv: slowing down:") {
		t.Fatalf("stdout = %q, slept = %s, stderr = %q", stdout.String(), slept, stderr.String())
	}
}

// The walk reads directories concurrently. It must still produce exactly what
// a plain filepath.WalkDir produces, because the batches an upload publishes,
// and therefore the idempotency keys an interrupted upload resumes onto, are
// cut from that order.
func TestWalkSourceMatchesWalkDirOrder(t *testing.T) {
	root := t.TempDir()
	// Names that sort around the directory names, so a walk that visited every
	// file before descending, or descended before its siblings, would show up.
	for _, directory := range []string{
		"alpha", "alpha/inner", "alpha/inner/deeper", "middle", "middle/one",
		"middle/two", "middle/two/three", "zulu", "zulu/last",
	} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{
		"", "alpha", "alpha/inner", "alpha/inner/deeper", "middle", "middle/one",
		"middle/two", "middle/two/three", "zulu", "zulu/last",
	} {
		for _, name := range []string{"aardvark.txt", "middle.txt", "zebra.txt"} {
			path := filepath.Join(root, filepath.FromSlash(directory), name)
			if err := os.WriteFile(path, []byte(directory+"/"+name), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := make([]string, 0)
	if err := filepath.WalkDir(root, func(local string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, local)
		if err != nil {
			return err
		}
		want = append(want, path.Join("archive", filepath.ToSlash(relative)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Repeated, because a read ahead that reordered would do so only sometimes.
	for attempt := range 8 {
		got := make([]string, 0)
		for _, file := range collectUploadFiles(t, root, "archive") {
			got = append(got, file.key)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("attempt %d walked\n %v\nwant\n %v", attempt, got, want)
		}
	}
}

// A chunk must read a prefix of the names it was given whatever order its
// readers finish in, so a batch depends only on the source and never on how
// the reads interleaved.
func TestChunkBudgetGrantsInOrderAndEndsOnce(t *testing.T) {
	const files = 64
	budget := newChunkBudget(files)
	granted := make([]bool, files)
	var wait sync.WaitGroup
	for index := range files {
		wait.Go(func() {
			// Half of one batch each, so the third file is refused.
			granted[index] = budget.reserve(index, maxBatchContent/2)
		})
	}
	wait.Wait()
	if consumed := budget.consumed(); consumed != 2 {
		t.Fatalf("consumed = %d, want 2", consumed)
	}
	for index, ok := range granted {
		if ok != (index < 2) {
			t.Fatalf("file %d granted = %v, want %v", index, ok, index < 2)
		}
	}
}

// The uploader measures a file on the handle it reads it through, so a batch
// is bounded by content the reader pool actually saw rather than by sizes the
// walk collected earlier.
func TestUploadBatchesStayInsideTheContentLimit(t *testing.T) {
	root := t.TempDir()
	const megabyte = 1 << 20
	content := bytes.Repeat([]byte("x"), megabyte)
	files := make([]uploadFile, maxBatchContent/megabyte+1)
	for ordinal := range files {
		name := fmt.Sprintf("file-%02d.txt", ordinal)
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
		files[ordinal] = uploadFile{localPath: filepath.Join(root, name), key: name}
	}
	jobs, warnings, err := makeUploadJobs(files, true)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings = %v, error = %v", warnings, err)
	}
	if len(jobs) != 2 || len(jobs[0].files) != maxBatchContent/megabyte || len(jobs[1].files) != 1 {
		t.Fatalf("batch sizes = %v, want [%d 1]", []int{len(jobs[0].files), len(jobs[1].files)}, maxBatchContent/megabyte)
	}
	for ordinal, job := range jobs {
		if job.bytes > maxBatchContent {
			t.Fatalf("batch %d carries %d bytes, over the %d limit", ordinal, job.bytes, maxBatchContent)
		}
	}
}

// A busy uploader is told to slow down once per deferred batch. On a terminal
// that must change the status line rather than print over it.
func TestUploadReportsThrottleOnTheStatusLineWithoutLogging(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rejected := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if rejected < 2 {
			rejected++
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"code":"write_backlog","reason":"capacity","error":"slow down"}`)
			return
		}
		io.WriteString(w, `{"results":[]}`)
	}))
	defer server.Close()
	a, _, stderr := testApp(t, server.URL+"/v1")
	a.isTTY = true
	a.sleep = func(context.Context, time.Duration) error { return nil }
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "code", root, ""); err != nil {
		t.Fatal(err)
	}
	if rejected != 2 {
		t.Fatalf("rejected = %d, want 2", rejected)
	}
	if strings.Contains(stderr.String(), "slowing down") {
		t.Fatalf("a pause was logged over the status line: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Uploaded 1 file ") {
		t.Fatalf("no upload summary: %q", stderr.String())
	}
}

// The source is counted before any of it is uploaded, so the two phases of the
// line are distinct: a spinner with a running tally while the total is unknown,
// then an upload bar with a total it can trust.
func TestProgressShowsScanningThenUploading(t *testing.T) {
	status := newProgress(io.Discard, true)
	status.scanned(200, 50, 1_234_567)
	line := status.line(time.Now())
	if !strings.Contains(line, "Scanning") || !strings.Contains(line, "1,234,567 files found") ||
		strings.Contains(line, "%") || strings.Contains(line, "ETA") || strings.ContainsAny(line, "█░") {
		t.Fatalf("scanning line = %q", line)
	}
	if first, next := status.line(status.started), status.line(status.started.Add(progressInterval)); first == next {
		t.Fatalf("spinner did not advance: %q", first)
	}
	status.scanned(0, 0, 1)
	if line := status.line(time.Now()); !strings.Contains(line, " 1 file found") {
		t.Fatalf("single file line = %q", line)
	}
	status.scanFinished(100_000)
	status.sent(50_000, 1<<20, 0)
	line = status.line(time.Now())
	if !strings.Contains(line, "Uploading") || !strings.Contains(line, " 50%") || !strings.Contains(line, "50K/100K") {
		t.Fatalf("uploading line = %q", line)
	}
	status.sentRate = 0.000001
	status.reportPause(time.Now().Add(time.Minute))
	if line := status.line(time.Now()); !strings.Contains(line, "Throttled") || !strings.Contains(line, "ETA --") {
		t.Fatalf("throttled line = %q", line)
	}
	// A source that grew after it was counted must not report past a whole.
	status.sent(60_000, 1<<20, 0)
	if line := status.line(time.Now()); !strings.Contains(line, "110K/110K") {
		t.Fatalf("overrun line = %q", line)
	}
}

// The counting pass must apply the same rules the upload does, or the total it
// reports would not be the total that gets published.
func TestCountSourceMatchesTheWalk(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{"kept", "skipped"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, content := range map[string]string{
		".gitignore":    "skipped/\n*.log\n",
		"a.txt":         "a",
		"noise.log":     "ignored",
		"kept/b.txt":    "b",
		"skipped/c.txt": "ignored",
	} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	counted, err := countSource(t.Context(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	walked := int64(len(collectUploadFiles(t, root, "archive")))
	if counted != walked || counted != 3 {
		t.Fatalf("counted %d, walked %d, want 3 each", counted, walked)
	}
}

// On a terminal, search results are grouped under their document key and the
// matched spans are highlighted, as ripgrep does. Match offsets arrive
// absolute within the document, so they are translated against the text's
// start offset, clipped to a truncated text, and merged when they overlap.
func TestSearchTerminalOutputGroupsAndHighlights(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[`+
			`{"key":"a.txt","revision":"r1","line_number":2,"last_line_number":2,"line_start_offset":100,"line_end_offset":114,"text_start_offset":100,"text_line_number":2,"text":"find this line","matches":[{"offset":100,"length":4}],"text_truncated":false,"matches_truncated":false},`+
			`{"key":"a.txt","revision":"r1","line_number":7,"last_line_number":7,"line_start_offset":200,"line_end_offset":230,"text_start_offset":210,"text_line_number":7,"text":"xfind me, find","matches":[{"offset":220,"length":4},{"offset":205,"length":6},{"offset":221,"length":2},{"offset":215,"length":0}],"text_truncated":true,"matches_truncated":false},`+
			`{"key":"b.txt","revision":"r2","line_number":1,"last_line_number":1,"line_start_offset":0,"line_end_offset":5,"text_start_offset":0,"text_line_number":1,"text":"hello","matches":[],"text_truncated":false,"matches_truncated":false}`+
			`],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()

	newTerminalApp := func(env map[string]string) (*app, *bytes.Buffer) {
		a, stdout, _ := testApp(t, server.URL+"/v1")
		if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
			t.Fatal(err)
		}
		a.stdoutTTY = true
		inner := a.getenv
		a.getenv = func(name string) string {
			if value, ok := env[name]; ok {
				return value
			}
			return inner(name)
		}
		return a, stdout
	}

	a, stdout := newTerminalApp(nil)
	matches, err := a.search(t.Context(), "acme", "code", "find", searchOptions{pageSize: 200})
	if err != nil || matches != 3 {
		t.Fatalf("search = %d matches, %v", matches, err)
	}
	want := "\x1b[35ma.txt\x1b[0m\n" +
		"\x1b[32m2\x1b[0m:\x1b[1;31mfind\x1b[0m this line\n" +
		"\x1b[32m7\x1b[0m:\x1b[1;31mx\x1b[0mfind me, \x1b[1;31mfind\x1b[0m\n" +
		"\n" +
		"\x1b[35mb.txt\x1b[0m\n" +
		"\x1b[32m1\x1b[0m:hello\n"
	if got := stdout.String(); got != want {
		t.Fatalf("terminal output = %q, want %q", got, want)
	}

	plain := "a.txt\n2:find this line\n7:xfind me, find\n\nb.txt\n1:hello\n"
	for _, env := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}} {
		a, stdout = newTerminalApp(env)
		if _, err := a.search(t.Context(), "acme", "code", "find", searchOptions{pageSize: 200}); err != nil {
			t.Fatal(err)
		}
		if got := stdout.String(); got != plain {
			t.Fatalf("output with %v = %q, want %q", env, got, plain)
		}
	}
	a, stdout = newTerminalApp(nil)
	if _, err := a.search(t.Context(), "acme", "code", "find", searchOptions{pageSize: 200, color: "never"}); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != plain {
		t.Fatalf("output with --color=never = %q, want %q", got, plain)
	}

	// Off a terminal the layout stays one key:line:text per result; --color=always
	// still colors it, for a pager.
	a, stdout, _ = testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme/code", "find", "--color=always"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want = "\x1b[35ma.txt\x1b[0m:\x1b[32m2\x1b[0m:\x1b[1;31mfind\x1b[0m this line\n" +
		"\x1b[35ma.txt\x1b[0m:\x1b[32m7\x1b[0m:\x1b[1;31mx\x1b[0mfind me, \x1b[1;31mfind\x1b[0m\n" +
		"\x1b[35mb.txt\x1b[0m:\x1b[32m1\x1b[0m:hello\n"
	if got := stdout.String(); got != want {
		t.Fatalf("piped --color=always output = %q, want %q", got, want)
	}

	root = newRootCommand(a)
	root.SetArgs([]string{"search", "acme/code", "find", "--color=sometimes"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--color must be") {
		t.Fatalf("invalid --color error = %v", err)
	}
}

// With -A, -B or -C the CLI prints context lines the way grep does: joined
// with `-` rather than `:`, each line once even where two results' context
// overlaps or a result sits inside another's context, and `--` between groups
// that are not adjacent. The request carries the counts, and -C fills in
// whichever of -A and -B was not given.
func TestSearchPrintsContextLinesLikeGrep(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		line := func(n int, text string, truncated bool) string {
			return fmt.Sprintf(`{"line_number":%d,"text":%q,"text_truncated":%t}`, n, text, truncated)
		}
		result := func(key string, n int, text string, before, after []string) string {
			return fmt.Sprintf(`{"key":%q,"revision":"r1","line_number":%d,"last_line_number":%d,"line_start_offset":0,"line_end_offset":0,"text_start_offset":0,"text_line_number":%d,"text":%q,"matches":[],"text_truncated":false,"matches_truncated":false,"lines_before":[%s],"lines_after":[%s]}`,
				key, n, n, n, text, strings.Join(before, ","), strings.Join(after, ","))
		}
		io.WriteString(w, `{"results":[`+
			// Line 5's trailing context reaches line 7, which is itself a
			// result: 6 prints once as context and 7 prints as a match.
			result("a.txt", 5, "five hit", []string{line(3, "three", false), line(4, "four", false)}, []string{line(6, "six", false), line(7, "seven hit", false)})+","+
			result("a.txt", 7, "seven hit", []string{line(5, "five hit", false), line(6, "six", false)}, []string{line(8, "eight", false), line(9, "nine", false)})+","+
			// A gap before line 20, and a trailing context line the window cut.
			result("a.txt", 20, "twenty hit", []string{line(18, "eighteen", false), line(19, "nineteen", false)}, []string{line(21, "twenty-one", true)})+","+
			// The first line of the next document has nothing above it.
			result("b.txt", 1, "one hit", nil, []string{line(2, "two", false)})+
			`],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()

	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme/code", "hit", "-C", "2", "-A", "1"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "a.txt-3-three\n" +
		"a.txt-4-four\n" +
		"a.txt:5:five hit\n" +
		"a.txt-6-six\n" +
		"a.txt:7:seven hit\n" +
		"a.txt-8-eight\n" +
		"a.txt-9-nine\n" +
		"--\n" +
		"a.txt-18-eighteen\n" +
		"a.txt-19-nineteen\n" +
		"a.txt:20:twenty hit\n" +
		"a.txt-21-twenty-one\n" +
		"--\n" +
		"b.txt:1:one hit\n" +
		"b.txt-2-two\n"
	if got := stdout.String(); got != want {
		t.Fatalf("piped output =\n%s\nwant\n%s", got, want)
	}
	if len(requests) != 1 || requests[0]["lines_before"] != float64(2) || requests[0]["lines_after"] != float64(1) {
		t.Fatalf("request = %#v, want lines_before 2 from -C and lines_after 1 from -A", requests)
	}

	// On a terminal the groups sit under their document heading and `--`
	// separates only the groups within one document.
	a, stdout, _ = testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	a.stdoutTTY = true
	if _, err := a.search(t.Context(), "acme", "code", "hit", searchOptions{pageSize: 200, before: 2, after: 1, color: "never"}); err != nil {
		t.Fatal(err)
	}
	want = "a.txt\n3-three\n4-four\n5:five hit\n6-six\n7:seven hit\n8-eight\n9-nine\n--\n18-eighteen\n19-nineteen\n20:twenty hit\n21-twenty-one\n\nb.txt\n1:one hit\n2-two\n"
	if got := stdout.String(); got != want {
		t.Fatalf("terminal output =\n%s\nwant\n%s", got, want)
	}

	// Without context flags nothing is asked for and no separator appears,
	// so scripts that parse key:line:text are unaffected.
	requests = nil
	a, stdout, _ = testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root = newRootCommand(a)
	root.SetArgs([]string{"search", "acme/code", "hit"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, asked := requests[0]["lines_before"]; asked {
		t.Fatalf("request without -B carries lines_before: %#v", requests[0])
	}
	if got := stdout.String(); strings.Contains(got, "--") {
		t.Fatalf("output without context has separators:\n%s", got)
	}

	// The bound is the API's, and is refused before a request is made.
	if _, err := a.search(t.Context(), "acme", "code", "hit", searchOptions{pageSize: 200, before: maxContextLines + 1}); err == nil || !strings.Contains(err.Error(), "context lines") {
		t.Fatalf("search with %d context lines = %v, want a refusal", maxContextLines+1, err)
	}
}

func TestUploadStoresBinaryAndLargeFilesThroughTheDocumentEndpoint(t *testing.T) {
	// A binary file, however small, and any file over the batch content limit
	// travel as their own multipart request, byte for byte, with the same
	// metadata the batch records carry; the text beside them still batches.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	gif := []byte{'G', 'I', 'F', '8', '9', 'a', 0xff, 0x00, 0xfe, '\n', 0x80}
	large := bytes.Repeat([]byte{0xff, 0x00, 'x', '\n'}, (maxBatchContent+1)/4+1)
	files := map[string][]byte{
		"README.md":       []byte("![logo](images/logo.gif)\n"),
		"images/logo.gif": gif,
		"data/large.bin":  large,
	}
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	var mu sync.Mutex
	batched := map[string]string{}
	single := map[string][]byte{}
	singleMetadata := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objects:batch"):
			scanner := bufio.NewScanner(r.Body)
			scanner.Buffer(make([]byte, 64<<10), 64<<20)
			mu.Lock()
			for scanner.Scan() {
				var record uploadRecord
				if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
					t.Error(err)
				}
				batched[record.Key] = record.Content
			}
			mu.Unlock()
			io.WriteString(w, `{"results":[]}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/objects"):
			if r.Header.Get("Idempotency-Key") == "" {
				t.Error("multipart upload without an idempotency key")
			}
			reader, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var key string
			var content []byte
			var metadata map[string]any
			for {
				part, err := reader.NextPart()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Error(err)
					break
				}
				value, _ := io.ReadAll(part)
				switch part.FormName() {
				case "key":
					key = string(value)
				case "content":
					content = value
				case "metadata":
					if err := json.Unmarshal(value, &metadata); err != nil {
						t.Error(err)
					}
				}
			}
			mu.Lock()
			single[key] = content
			singleMetadata[key] = metadata
			mu.Unlock()
			io.WriteString(w, `{"key":"`+key+`","revision":"r1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	a, _, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	if err := a.upload(t.Context(), "acme", "docs", root, ""); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("a stored-only file produced output: %q", stderr.String())
	}
	if len(batched) != 1 || batched["README.md"] != string(files["README.md"]) {
		t.Fatalf("batched records = %#v", batched)
	}
	if len(single) != 2 || !bytes.Equal(single["images/logo.gif"], gif) || !bytes.Equal(single["data/large.bin"], large) {
		t.Fatalf("single uploads: %d, logo %d bytes, large %d bytes", len(single), len(single["images/logo.gif"]), len(single["data/large.bin"]))
	}
	for key, content := range single {
		metadata := singleMetadata[key]
		path, _ := metadata["path"].(map[string]any)
		size, _ := metadata["size"].(map[string]any)
		if path["value"] != key || size["value"] != float64(len(content)) || metadata["mtime"] == nil {
			t.Fatalf("metadata of %s = %#v", key, metadata)
		}
	}
}

// A result whose match spans lines prints one `key:line:text` line per line
// it covers, numbered in turn, with the match highlighted on each; context
// lines and a following result that shares a context line print once.
func TestSearchPrintsMultilineResults(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/query") {
			http.NotFound(w, r)
			return
		}
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"results":[`+
			// "alpha\nmiddle\nneedle" occupies bytes 10-29; the match covers it whole.
			`{"key":"a.txt","revision":"r1","line_number":3,"last_line_number":5,"line_start_offset":10,"line_end_offset":29,"text_start_offset":10,"text_line_number":3,"text":"alpha\nmiddle\nneedle","matches":[{"offset":10,"length":19}],"text_truncated":false,"matches_truncated":false,"lines_before":[{"line_number":2,"text":"two","text_truncated":false}],"lines_after":[{"line_number":6,"text":"six","text_truncated":false}]},`+
			`{"key":"a.txt","revision":"r1","line_number":7,"last_line_number":7,"line_start_offset":40,"line_end_offset":49,"text_start_offset":40,"text_line_number":7,"text":"needle 7!","matches":[{"offset":40,"length":6}],"text_truncated":false,"matches_truncated":false,"lines_before":[{"line_number":6,"text":"six","text_truncated":false}],"lines_after":[{"line_number":8,"text":"eight","text_truncated":false}]}`+
			`],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()

	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme/code", "alpha\\nneedle", "-U", "-C", "1"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := "a.txt-2-two\n" +
		"a.txt:3:alpha\n" +
		"a.txt:4:middle\n" +
		"a.txt:5:needle\n" +
		"a.txt-6-six\n" +
		"a.txt:7:needle 7!\n" +
		"a.txt-8-eight\n"
	if got := stdout.String(); got != want {
		t.Fatalf("piped output =\n%s\nwant\n%s", got, want)
	}
	if len(requests) != 1 || requests[0]["multiline"] != true {
		t.Fatalf("requests = %#v, want one with multiline set", requests)
	}

	a, stdout, _ = testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	a.stdoutTTY = true
	if _, err := a.search(t.Context(), "acme", "code", "alpha\\nneedle", searchOptions{pageSize: 200, multiline: true, color: "always", before: 1, after: 1}); err != nil {
		t.Fatal(err)
	}
	wantTerminal := "\x1b[35ma.txt\x1b[0m\n" +
		"\x1b[32m2\x1b[0m-two\n" +
		"\x1b[32m3\x1b[0m:\x1b[1;31malpha\x1b[0m\n" +
		"\x1b[32m4\x1b[0m:\x1b[1;31mmiddle\x1b[0m\n" +
		"\x1b[32m5\x1b[0m:\x1b[1;31mneedle\x1b[0m\n" +
		"\x1b[32m6\x1b[0m-six\n" +
		"\x1b[32m7\x1b[0m:\x1b[1;31mneedle\x1b[0m 7!\n" +
		"\x1b[32m8\x1b[0m-eight\n"
	if got := stdout.String(); got != wantTerminal {
		t.Fatalf("terminal output = %q, want %q", got, wantTerminal)
	}
}

func TestProgressDoesNotExtrapolateAStoppedRate(t *testing.T) {
	for _, rate := range []float64{0, 0.000001, 0.49} {
		if got := remaining(6_400_000, rate); got != -1 {
			t.Fatalf("rate %g: ETA %s", rate, got)
		}
	}
	if got := remaining(100, 2); got != 50*time.Second {
		t.Fatalf("active ETA: %s", got)
	}
	if got := remaining(1<<62, 1); got != -1 {
		t.Fatalf("overflowing ETA: %s", got)
	}
}

func TestSearchAndListJSON(t *testing.T) {
	const result = `{"key":"a.txt","revision":"r1","line_number":1,"last_line_number":1,"line_start_offset":0,"line_end_offset":5,"text_start_offset":0,"text_line_number":1,"text":"hello","matches":[],"text_truncated":false,"matches_truncated":false}`
	const document = `{"key":"a.txt","revision":"r1","content_bytes":5,"metadata":{},"updated_at":"2026-03-04T05:06:07Z","searchable":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/query") {
			io.WriteString(w, `{"results":[`+result+`],"pagination":{"next_page_id":"","has_next":false}}`)
			return
		}
		io.WriteString(w, `{"results":[`+document+`],"common_prefixes":["dir/"],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme/docs", "hello", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != result+"\n" {
		t.Fatalf("search --json = %q", got)
	}
	stdout.Reset()
	root = newRootCommand(a)
	root.SetArgs([]string{"list", "acme/docs", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != `{"prefix":"dir/"}`+"\n"+document+"\n" {
		t.Fatalf("list --json = %q", got)
	}
}

func TestSearchJSONNoMatchContextAndMaxResults(t *testing.T) {
	result := func(key string) string {
		return `{"key":"` + key + `","revision":"r1","line_number":2,"last_line_number":2,"line_start_offset":4,"line_end_offset":9,"text_start_offset":4,"text_line_number":2,"text":"hello","matches":[],"text_truncated":false,"matches_truncated":false,"lines_before":[{"line_number":1,"text":"before","text_truncated":false}]}`
	}
	var requests []map[string]any
	empty := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		if empty {
			io.WriteString(w, `{"results":[],"pagination":{"next_page_id":"","has_next":false}}`)
			return
		}
		io.WriteString(w, `{"results":[`+result("a.txt")+`,`+result("b.txt")+`],"pagination":{"next_page_id":"","has_next":false}}`)
	}))
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}

	root := newRootCommand(a)
	root.SetArgs([]string{"search", "acme/docs", "hello", "--json", "-B", "1", "--max-results", "1"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := stdout.String(); got != result("a.txt")+"\n" {
		t.Fatalf("search --json -B 1 --max-results 1 = %q", got)
	}
	if requests[0]["lines_before"] != float64(1) {
		t.Fatalf("query request = %#v", requests[0])
	}
	if a.exitCode != 0 {
		t.Fatalf("exit code = %d", a.exitCode)
	}

	empty = true
	stdout.Reset()
	root = newRootCommand(a)
	root.SetArgs([]string{"search", "acme/docs", "nothing", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 || a.exitCode != 1 {
		t.Fatalf("no match: output %q, exit code %d", stdout.String(), a.exitCode)
	}
}

func TestListJSONAcrossPages(t *testing.T) {
	const first = `{"key":"a.txt","revision":"r1","content_bytes":1,"metadata":{},"updated_at":"2026-03-04T05:06:07Z","searchable":true}`
	const second = `{"key":"z/b.txt","revision":"r2","content_bytes":2,"metadata":{},"updated_at":"2026-03-04T05:06:07Z","searchable":true}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "p2" {
			io.WriteString(w, `{"results":[`+second+`],"common_prefixes":["y/"],"pagination":{"next_page_id":"","has_next":false}}`)
			return
		}
		io.WriteString(w, `{"results":[`+first+`],"common_prefixes":["x/"],"pagination":{"next_page_id":"p2","has_next":true}}`)
	}))
	defer server.Close()
	a, stdout, _ := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand(a)
	root.SetArgs([]string{"list", "acme/docs", "--delimiter", "/", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	want := `{"prefix":"x/"}` + "\n" + first + "\n" + `{"prefix":"y/"}` + "\n" + second + "\n"
	if got := stdout.String(); got != want {
		t.Fatalf("list --json = %q, want %q", got, want)
	}
}
func TestConfigFileFromEnvironment(t *testing.T) {
	a, _, _ := testApp(t, "")
	flagPath := a.configPath
	envPath := filepath.Join(t.TempDir(), "stage.yaml")
	a.getenv = func(name string) string {
		if name == "GREPVINE_CONFIG" {
			return envPath
		}
		return ""
	}
	if path, _ := a.resolvedConfigPath(); path != flagPath {
		t.Fatalf("--config should win, got %s", path)
	}
	a.configPath = ""
	if path, _ := a.resolvedConfigPath(); path != envPath {
		t.Fatalf("GREPVINE_CONFIG ignored, got %s", path)
	}
}

// TestPutAndDeleteSeveralDocuments stores documents from a file and from
// standard input with metadata, then deletes several keys in one call, one of
// them missing.
func TestPutAndDeleteSeveralDocuments(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}}
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	execute := func(stdin string, args ...string) error {
		stdout.Reset()
		stderr.Reset()
		a.stdin = strings.NewReader(stdin)
		command := newRootCommand(a)
		command.SetArgs(args)
		return command.Execute()
	}
	source := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(source, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := execute("", "put", "acme/docs", "a.txt", source, "-m", "team=search", "-m", "pages:integer=3"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "a.txt\tr-put\n" || stack.objects["a.txt"] != "from a file\n" {
		t.Fatalf("put output = %q, stored %q", stdout.String(), stack.objects["a.txt"])
	}
	if team, _ := stack.metadata["a.txt"]["team"].GetStringMetadataValue(); team.Value != "search" {
		t.Fatalf("team metadata = %+v", stack.metadata["a.txt"])
	}
	if pages, _ := stack.metadata["a.txt"]["pages"].GetIntegerMetadataValue(); pages.Value != 3 {
		t.Fatalf("pages metadata = %+v", stack.metadata["a.txt"])
	}
	if err := execute("from stdin", "put", "acme/docs", "b.txt", "-", "--json"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != `{"key":"b.txt","revision":"r-put"}`+"\n" || stack.objects["b.txt"] != "from stdin" {
		t.Fatalf("put --json output = %q, stored %q", stdout.String(), stack.objects["b.txt"])
	}
	if err := execute("again", "put", "acme/docs", "b.txt", "-", "--if-absent"); err == nil || !strings.Contains(err.Error(), "precondition_failed") {
		t.Fatalf("put --if-absent over a document = %v", err)
	}
	for _, field := range []string{"pages:integer=many", "size:bytes=3", "=x", "nothing"} {
		if err := execute("", "put", "acme/docs", "c.txt", "-", "-m", field); err == nil || !strings.Contains(err.Error(), "metadata") {
			t.Fatalf("put -m %q = %v", field, err)
		}
	}

	if err := execute("", "delete", "acme/docs", "a.txt", "b.txt", "--if-match", "r1"); err == nil || !strings.Contains(err.Error(), "--if-match") {
		t.Fatalf("delete --if-match with two keys = %v", err)
	}
	err = execute("", "delete", "acme/docs", "a.txt", "missing.txt", "b.txt")
	if _, ok := errors.AsType[*rejectedError](err); !ok || err.Error() != "1 document was not deleted" {
		t.Fatalf("delete with a missing key = %v", err)
	}
	if stdout.String() != "a.txt\tr-delete\nb.txt\tr-delete\n" || !strings.Contains(stderr.String(), "missing.txt was not deleted") {
		t.Fatalf("delete stdout = %q stderr = %q", stdout.String(), stderr.String())
	}
	if len(stack.objects) != 0 || len(stack.batchSizes) != 1 {
		t.Fatalf("objects left = %v, batches = %v", stack.objects, stack.batchSizes)
	}
	err = execute("", "delete", "acme/docs", "gone.txt", "--json")
	if _, ok := errors.AsType[*rejectedError](err); !ok {
		t.Fatalf("delete --json of a missing key = %v", err)
	}
	if !strings.HasPrefix(stdout.String(), `{"key":"gone.txt","error":{"code":"not_found"`) {
		t.Fatalf("delete --json output = %q", stdout.String())
	}
}
