package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// newSyncStack is an in-process stack with one ready index, acme/docs.
func newSyncStack() *fakeService {
	return &fakeService{
		objects: map[string]string{}, revisions: map[string]string{}, metadata: map[string]httpapi.Metadata{},
		indexes: map[string]*httpapi.Index{"docs": {
			ID: uuid.New(), Organization: "acme", Name: "docs", CanonicalID: "canonical", Tags: httpapi.IndexTags{},
			State: httpapi.IndexStateReady, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}},
	}
}

// startStack serves the stack and returns an app configured against it.
func startStack(t *testing.T, stack *fakeService) (*app, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	handler, err := httpapi.NewServer(stack, stack, httpapi.WithPathPrefix("/v1"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a, stdout, stderr := testApp(t, server.URL+"/v1")
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	return a, stdout, stderr
}

// runGv drives the root command the way a shell would.
func runGv(a *app, stdout, stderr *bytes.Buffer, args ...string) error {
	stdout.Reset()
	stderr.Reset()
	command := newRootCommand(a)
	command.SetArgs(args)
	return command.Execute()
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// plainDir is a temporary directory outside any repository, with the path
// symbolic links resolved the way sync resolves them.
func plainDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(dir))
	return dir
}

// gitRepoForTest creates an isolated repository on branch main and returns a
// helper that runs git in it, failing the test on any error.
func gitRepoForTest(t *testing.T) (string, func(args ...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := plainDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, "gitconfig"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, role := range []string{"AUTHOR", "COMMITTER"} {
		t.Setenv("GIT_"+role+"_NAME", "gv test")
		t.Setenv("GIT_"+role+"_EMAIL", "gv@example.test")
		t.Setenv("GIT_"+role+"_DATE", "2026-01-02T03:04:05Z")
	}
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = dir
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "commit.gpgsign", "false")
	git("config", "user.name", "gv test")
	git("config", "user.email", "gv@example.test")
	return dir, git
}

func sortedKeys(objects map[string]string) []string {
	keys := make([]string, 0, len(objects))
	for key := range objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func indexTags(stack *fakeService) httpapi.IndexTags {
	stack.mu.Lock()
	defer stack.mu.Unlock()
	copied := httpapi.IndexTags{}
	maps.Copy(copied, stack.indexes["docs"].Tags)
	return copied
}

func TestSyncGitFirstSyncUploadsTrackedFilesAndRecordsHead(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{
		".gitignore":    "build/\n",
		"README.md":     "# Docs\n",
		"docs/guide.md": "guide\n",
		"build/out.log": "ignored\n",
		"scratch.txt":   "untracked\n",
	})
	git("add", ".gitignore", "README.md", "docs/guide.md")
	git("commit", "-q", "-m", "initial")
	head := git("rev-parse", "HEAD")

	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{".gitignore", "README.md", "docs/guide.md"}) {
		t.Fatalf("objects after the first sync = %v", got)
	}
	if stack.objects["docs/guide.md"] != "guide\n" {
		t.Fatalf("content = %q", stack.objects["docs/guide.md"])
	}
	if tags := indexTags(stack); tags[gitCommitTag] != head || tags[gitBranchTag] != "main" {
		t.Fatalf("tags = %v, want commit %s on main", tags, head)
	}
	if !strings.Contains(stderr.String(), "3 uploaded, 0 deleted, 0 skipped (commit "+head[:7]+")") {
		t.Fatalf("summary = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout.String())
	}

	// A second sync of the same commit reads the index and nothing else.
	writes, listings := stack.writes, stack.listings
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "already up to date at "+head[:7]) || stack.writes != writes || stack.listings != listings {
		t.Fatalf("second sync: stderr=%q writes=%d->%d listings=%d->%d", stderr.String(), writes, stack.writes, listings, stack.listings)
	}
}

func TestSyncGitSecondSyncSendsOnlyTheDelta(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"README.md": "one\n", "docs/guide.md": "guide\n", "old.md": "old\n", "keep.txt": "keep\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}

	git("mv", "docs/guide.md", "docs/intro.md")
	git("rm", "-q", "old.md")
	writeFiles(t, dir, map[string]string{"README.md": "two\n", "new.md": "new\n"})
	git("add", ".")
	git("commit", "-q", "-m", "second")
	head := git("rev-parse", "HEAD")

	stack.mu.Lock()
	stack.putKeys, stack.batchSizes = nil, nil
	stack.mu.Unlock()
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("second sync: %v\n%s", err, stderr.String())
	}
	sort.Strings(stack.putKeys)
	if fmt.Sprint(stack.putKeys) != fmt.Sprint([]string{"README.md", "docs/intro.md", "new.md"}) {
		t.Fatalf("uploaded keys = %v", stack.putKeys)
	}
	if fmt.Sprint(stack.batchSizes) != "[2]" || stack.singleDeletes != 0 || stack.missingDeletes != 0 {
		t.Fatalf("delete batches = %v, single deletes = %d, missing = %d", stack.batchSizes, stack.singleDeletes, stack.missingDeletes)
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{"README.md", "docs/intro.md", "keep.txt", "new.md"}) {
		t.Fatalf("objects = %v", got)
	}
	if stack.objects["README.md"] != "two\n" {
		t.Fatalf("README content = %q", stack.objects["README.md"])
	}
	if tags := indexTags(stack); tags[gitCommitTag] != head {
		t.Fatalf("tags = %v", tags)
	}
	if !strings.Contains(stderr.String(), "3 uploaded, 2 deleted, 0 skipped (commits ") {
		t.Fatalf("summary = %q", stderr.String())
	}
}

func TestSyncGitSubdirectoryScopesKeysAndChanges(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"README.md": "root\n", "docs/guide.md": "guide\n", "docs/api/ref.md": "ref\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", filepath.Join(dir, "docs"), "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{"api/ref.md", "guide.md"}) {
		t.Fatalf("objects = %v", got)
	}

	// A commit that touches nothing under the directory moves the tag and
	// nothing else.
	writeFiles(t, dir, map[string]string{"README.md": "changed\n"})
	git("add", ".")
	git("commit", "-q", "-m", "outside")
	head := git("rev-parse", "HEAD")
	writes := stack.writes
	if err := runGv(a, stdout, stderr, "sync", filepath.Join(dir, "docs"), "acme/docs"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "0 uploaded, 0 deleted, 0 skipped") || stack.writes != writes+1 || indexTags(stack)[gitCommitTag] != head {
		t.Fatalf("stderr=%q writes=%d->%d tags=%v", stderr.String(), writes, stack.writes, indexTags(stack))
	}
}

func TestSyncGitDetachedHeadRemovesTheBranchTag(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"a.txt": "a\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	if indexTags(stack)[gitBranchTag] != "main" {
		t.Fatalf("tags = %v", indexTags(stack))
	}
	git("checkout", "-q", "--detach")
	writeFiles(t, dir, map[string]string{"b.txt": "b\n"})
	git("add", ".")
	git("commit", "-q", "-m", "detached")
	head := git("rev-parse", "HEAD")
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	tags := indexTags(stack)
	if _, present := tags[gitBranchTag]; present || tags[gitCommitTag] != head {
		t.Fatalf("tags = %v", tags)
	}
}

func TestSyncGitUnknownBaseFallsBackToComparingTheTree(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"a.txt": "a\n", "b.txt": "b\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	head := git("rev-parse", "HEAD")
	stack := newSyncStack()
	stack.indexes["docs"].Tags[gitCommitTag] = strings.Repeat("0", 40)
	stack.objects["stale.txt"] = "stale\n"
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "which this repository does not have") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{"a.txt", "b.txt"}) {
		t.Fatalf("objects = %v", got)
	}
	if tags := indexTags(stack); tags[gitCommitTag] != head || tags[gitBranchTag] != "main" {
		t.Fatalf("tags = %v", tags)
	}
}

func TestSyncGitRefusesADirtyWorkingTree(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"a.txt": "a\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	writeFiles(t, dir, map[string]string{"a.txt": "edited\n"})
	err := runGv(a, stdout, stderr, "sync", dir, "acme/docs")
	if err == nil || !strings.Contains(err.Error(), "commit or stash") {
		t.Fatalf("dirty tree error = %v", err)
	}
	if stack.writes != 0 {
		t.Fatalf("a refused sync wrote %d times", stack.writes)
	}
	// An untracked file is not a change to what is synced.
	git("checkout", "-q", "--", "a.txt")
	writeFiles(t, dir, map[string]string{"scratch.txt": "untracked\n"})
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync with an untracked file: %v", err)
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != "[a.txt]" {
		t.Fatalf("objects = %v", got)
	}
}

func TestSyncGitRefusesARepositoryWithoutCommits(t *testing.T) {
	dir, _ := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"a.txt": "a\n"})
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	err := runGv(a, stdout, stderr, "sync", dir, "acme/docs")
	if err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("error = %v", err)
	}
}

func TestSyncResumesAfterAnInterruptedDeletion(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"keep.txt": "keep\n", "old.md": "old\n", "gone.md": "gone\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	git("rm", "-q", "old.md", "gone.md")
	git("commit", "-q", "-m", "remove")
	// An earlier run removed gone.md and was interrupted before the tag moved.
	stack.mu.Lock()
	delete(stack.objects, "gone.md")
	stack.mu.Unlock()
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("resumed sync: %v\n%s", err, stderr.String())
	}
	// The key that is already gone comes back as not found and counts as
	// deleted; the batch still deletes the other one.
	if stack.missingDeletes != 1 || stack.singleDeletes != 0 || fmt.Sprint(stack.batchSizes) != "[2]" {
		t.Fatalf("missing=%d single=%d batches=%v", stack.missingDeletes, stack.singleDeletes, stack.batchSizes)
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != "[keep.txt]" {
		t.Fatalf("objects = %v", got)
	}
	if indexTags(stack)[gitCommitTag] != git("rev-parse", "HEAD") {
		t.Fatalf("tags = %v", indexTags(stack))
	}
}

// seedTwoWayStack stores three documents: one with no local file, one that
// matches its local file in size and modification time, and one whose size
// differs. It returns the stack with the git tags set, as after a git sync.
func seedTwoWayStack(t *testing.T, dir string) *fakeService {
	t.Helper()
	writeFiles(t, dir, map[string]string{"same.txt": "same\n", "changed.txt": "changed locally\n", "new.txt": "new\n"})
	info, err := os.Stat(filepath.Join(dir, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	stack := newSyncStack()
	stack.objects["stale.txt"] = "stale\n"
	stack.objects["same.txt"] = "same\n"
	stack.metadata["same.txt"] = httpapi.Metadata{"mtime": httpapi.NewTimestampMetadataValueMetadataValue(httpapi.TimestampMetadataValue{Type: "timestamp", Value: info.ModTime().UTC()})}
	stack.objects["changed.txt"] = "changed\n"
	stack.metadata["changed.txt"] = httpapi.Metadata{"mtime": httpapi.NewTimestampMetadataValueMetadataValue(httpapi.TimestampMetadataValue{Type: "timestamp", Value: info.ModTime().UTC()})}
	stack.indexes["docs"].Tags = httpapi.IndexTags{gitCommitTag: strings.Repeat("a", 40), gitBranchTag: "main", "team": "search"}
	return stack
}

func TestSyncOutsideGitComparesDirectoryAndIndex(t *testing.T) {
	dir := plainDir(t)
	stack := seedTwoWayStack(t, dir)
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	sort.Strings(stack.putKeys)
	if fmt.Sprint(stack.putKeys) != fmt.Sprint([]string{"changed.txt", "new.txt"}) {
		t.Fatalf("uploaded keys = %v", stack.putKeys)
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{"changed.txt", "new.txt", "same.txt"}) {
		t.Fatalf("objects = %v", got)
	}
	if stack.objects["changed.txt"] != "changed locally\n" {
		t.Fatalf("changed.txt = %q", stack.objects["changed.txt"])
	}
	if tags := indexTags(stack); len(tags) != 1 || tags["team"] != "search" {
		t.Fatalf("tags = %v, want only team", tags)
	}
	if !strings.Contains(stderr.String(), "2 uploaded, 1 deleted, 0 skipped (full comparison)") {
		t.Fatalf("summary = %q", stderr.String())
	}
	// The listing carries whole seconds; the files it just uploaded must
	// compare equal on the next run, so nothing is written twice.
	writes := stack.writes
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	if stack.writes != writes || !strings.Contains(stderr.String(), "0 uploaded, 0 deleted") {
		t.Fatalf("second run wrote %d times: %q", stack.writes-writes, stderr.String())
	}
}

func TestSyncDryRunPrintsThePlanAndWritesNothing(t *testing.T) {
	dir := plainDir(t)
	stack := seedTwoWayStack(t, dir)
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", "--dry-run", dir, "acme/docs"); err != nil {
		t.Fatalf("dry run: %v\n%s", err, stderr.String())
	}
	want := "+ changed.txt\n+ new.txt\n- stale.txt\n~ tag git.branch (removed)\n~ tag git.commit (removed)\n"
	if stdout.String() != want {
		t.Fatalf("dry run output = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(stderr.String(), "dry run: 2 to upload, 1 to delete, 2 tag changes; nothing written") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stack.writes != 0 || len(stack.objects) != 3 {
		t.Fatalf("dry run wrote: writes=%d objects=%v", stack.writes, sortedKeys(stack.objects))
	}
}

func TestSyncOutsideGitKeepsTheDocumentOfAnOversizeFile(t *testing.T) {
	dir := plainDir(t)
	large, err := os.Create(filepath.Join(dir, "large.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err = large.Truncate(maxDocumentSize + 1); err != nil {
		t.Fatal(err)
	}
	if err = large.Close(); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"small.txt": "small\n"})
	stack := newSyncStack()
	stack.objects["large.bin"] = "an earlier version"
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != fmt.Sprint([]string{"large.bin", "small.txt"}) {
		t.Fatalf("objects = %v", got)
	}
	if !strings.Contains(stderr.String(), "skipping large.bin") || !strings.Contains(stderr.String(), "1 uploaded, 0 deleted, 1 skipped") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSyncDeletesInBatchesOfTheContractLimit(t *testing.T) {
	dir := plainDir(t)
	stack := newSyncStack()
	for i := range deleteBatchKeys + 1 {
		stack.objects[fmt.Sprintf("doc-%05d.txt", i)] = "x"
	}
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("sync: %v\n%s", err, stderr.String())
	}
	if fmt.Sprint(stack.batchSizes) != fmt.Sprint([]int{deleteBatchKeys, 1}) || len(stack.objects) != 0 {
		t.Fatalf("batches = %v, remaining = %d", stack.batchSizes, len(stack.objects))
	}
	for id, presented := range stack.deleteIDs {
		if presented != 1 {
			t.Fatalf("idempotency key %s presented %d times", id, presented)
		}
	}
}

func TestSyncRefusesAPathThatIsNotADirectory(t *testing.T) {
	dir := plainDir(t)
	writeFiles(t, dir, map[string]string{"file.txt": "x\n"})
	a, stdout, stderr := startStack(t, newSyncStack())
	if err := runGv(a, stdout, stderr, "sync", filepath.Join(dir, "file.txt"), "acme/docs"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %v", err)
	}
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/missing"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("missing index error = %v", err)
	}
}

func TestParseRawDiff(t *testing.T) {
	output := strings.Join([]string{
		":000000 100644 0000000 1111111 A", "added.txt",
		":100644 100644 2222222 3333333 M", "changed with space.txt",
		":100644 100755 4444444 4444444 M", "mode-only.sh",
		":100644 000000 5555555 0000000 D", "removed.txt",
		":100644 120000 6666666 7777777 T", "now-a-link",
		":120000 100644 8888888 9999999 T", "now-a-file",
		":000000 160000 0000000 aaaaaaa A", "submodule",
		":100644 100644 bbbbbbb ccccccc M", "line\nbreak.txt",
	}, "\x00") + "\x00"
	changes, err := parseRawDiff([]byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 8 || changes[1].path != "changed with space.txt" || changes[7].path != "line\nbreak.txt" {
		t.Fatalf("changes = %+v", changes)
	}
	if changes[4].status != 'T' || changes[4].oldMode != 0o100644 || changes[4].newMode != 0o120000 {
		t.Fatalf("type change = %+v", changes[4])
	}
	if empty, err := parseRawDiff(nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty diff = %v, %v", empty, err)
	}
	for _, malformed := range []string{":100644 100644 1 2 M\x00", "100644 100644 1 2 M\x00a\x00", ":100644 100644 1 2 R100\x00a\x00b\x00", ":x 100644 1 2 M\x00a\x00"} {
		if _, err := parseRawDiff([]byte(malformed)); err == nil {
			t.Fatalf("malformed %q accepted", malformed)
		}
	}

	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"added.txt": "a", "changed with space.txt": "c", "mode-only.sh": "m", "now-a-file": "f", "line\nbreak.txt": "l"})
	warnings := 0
	plan, err := planFromGitDiff(dir, changes, func(string) { warnings++ })
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(plan.uploads))
	for _, file := range plan.uploads {
		keys = append(keys, file.key)
	}
	if fmt.Sprint(keys) != fmt.Sprint([]string{"added.txt", "changed with space.txt", "now-a-file", "line\nbreak.txt"}) {
		t.Fatalf("uploads = %v", keys)
	}
	if fmt.Sprint(plan.deletes) != fmt.Sprint([]string{"now-a-link", "removed.txt"}) || warnings != 0 || plan.skipped != 0 {
		t.Fatalf("deletes = %v warnings = %d skipped = %d", plan.deletes, warnings, plan.skipped)
	}
	if _, err := planFromGitDiff(dir, []gitChange{{newMode: 0o100644, status: 'A', path: "missing.txt"}}, func(string) {}); err == nil {
		t.Fatal("a tracked file missing from disk was not reported")
	}
}

func TestParseLsFiles(t *testing.T) {
	output := "100644 1111111111111111111111111111111111111111 0\tplain.txt\x00" +
		"100755 2222222222222222222222222222222222222222 0\tscript.sh\x00" +
		"120000 3333333333333333333333333333333333333333 0\tlink\x00" +
		"160000 4444444444444444444444444444444444444444 0\tsubmodule\x00" +
		"100644 5555555555555555555555555555555555555555 1\tconflict.txt\x00" +
		"100644 6666666666666666666666666666666666666666 0\twith space.txt\x00"
	var paths []string
	if err := parseLsFiles([]byte(output), func(path string) error { paths = append(paths, path); return nil }); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(paths) != fmt.Sprint([]string{"plain.txt", "script.sh", "with space.txt"}) {
		t.Fatalf("paths = %v", paths)
	}
	if err := parseLsFiles([]byte("garbage\x00"), func(string) error { return nil }); err == nil {
		t.Fatal("malformed record accepted")
	}
}

func TestLocalChangedComparesSizeAndWholeSeconds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		remote remoteDocument
		want   bool
	}{
		"same second, different nanoseconds": {remoteDocument{size: 5, mtime: stamp.Truncate(time.Second), hasMtime: true}, false},
		"identical":                          {remoteDocument{size: 5, mtime: stamp, hasMtime: true}, false},
		"size differs":                       {remoteDocument{size: 4, mtime: stamp, hasMtime: true}, true},
		"one second earlier":                 {remoteDocument{size: 5, mtime: stamp.Add(-time.Second), hasMtime: true}, true},
		"one second later":                   {remoteDocument{size: 5, mtime: stamp.Add(time.Second), hasMtime: true}, true},
		"no recorded time":                   {remoteDocument{size: 5}, true},
	} {
		if got := localChanged(test.remote, info); got != test.want {
			t.Fatalf("%s: changed = %v, want %v", name, got, test.want)
		}
	}
}

func TestDeleteIDIsDeterministicAndOrderSensitive(t *testing.T) {
	first := deleteID("acme", "docs", []string{"a", "b"})
	if first != deleteID("acme", "docs", []string{"a", "b"}) {
		t.Fatal("the same batch produced different idempotency keys")
	}
	if first == deleteID("acme", "docs", []string{"b", "a"}) || first == deleteID("acme", "other", []string{"a", "b"}) {
		t.Fatal("different batches produced the same idempotency key")
	}
	if first == uploadID("acme", "docs", []byte("a\x00b\x00")) {
		t.Fatal("a delete batch collides with an upload body")
	}
}

func TestGitTagPatchRecordsOnlyWhatChanges(t *testing.T) {
	head := strings.Repeat("c", 40)
	patch := gitTagPatch(httpapi.IndexTags{gitCommitTag: head, gitBranchTag: "main"}, head, "main")
	if len(patch) != 0 {
		t.Fatalf("unchanged tags produced %v", patch)
	}
	patch = gitTagPatch(httpapi.IndexTags{gitCommitTag: "old", gitBranchTag: "main"}, head, "")
	if value, _ := patch[gitCommitTag].Get(); value != head || !patch[gitBranchTag].IsNull() {
		t.Fatalf("detached patch = %v", patch)
	}
	patch = gitTagPatch(httpapi.IndexTags{}, head, "")
	if _, present := patch[gitBranchTag]; present {
		t.Fatalf("a branch that was never set is removed: %v", patch)
	}
	if removed := removeGitTags(httpapi.IndexTags{"team": "x"}); len(removed) != 0 {
		t.Fatalf("removing absent git tags produced %v", removed)
	}
	if removed := removeGitTags(httpapi.IndexTags{gitCommitTag: "x", "team": "y"}); len(removed) != 1 || !removed[gitCommitTag].IsNull() {
		t.Fatalf("removal patch = %v", removed)
	}
}

// TestDeleteBatchRetriesAndSlowsDown drives the delete batch through a 429, a
// 503 and then success, as the upload batch does.
func TestDeleteBatchRetriesAndSlowsDown(t *testing.T) {
	statuses := []int{http.StatusTooManyRequests, http.StatusServiceUnavailable, http.StatusOK}
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		status := statuses[min(requests, len(statuses)-1)]
		requests++
		header := http.Header{"Content-Type": []string{"application/json"}}
		body := `{"results":[{"key":"a","revision":"r1"},{"key":"b","revision":"r2"}]}`
		if status != http.StatusOK {
			body = `{"code":"busy","reason":"capacity","error":"slow down"}`
			if status == http.StatusTooManyRequests {
				header.Set("Retry-After", "1")
			}
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	slept := 0
	a := &app{stderr: &bytes.Buffer{}, sleep: func(context.Context, time.Duration) error { slept++; return nil }}
	client, err := httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := a.sendDeleteBatch(t.Context(), client, "acme", "docs", []string{"a", "b"})
	if err != nil || len(failed) != 0 {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	// The 429 costs a pause without an attempt, the 503 costs an attempt; the
	// stubbed clock never advances, so the pause is waited out again before
	// each later request.
	if requests != 3 || slept < 2 {
		t.Fatalf("requests=%d slept=%d", requests, slept)
	}

	// A 404 for the whole request is final, reported at once rather than
	// retried.
	notFoundRequests := 0
	notFound := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		notFoundRequests++
		return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":"not_found","reason":"resource","error":"gone"}`)), Request: request}, nil
	})
	client, err = httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: notFound}))
	if err != nil {
		t.Fatal(err)
	}
	if failed, err = a.sendDeleteBatch(t.Context(), client, "acme", "docs", []string{"a"}); err == nil || notFoundRequests != 1 {
		t.Fatalf("failed=%v err=%v requests=%d", failed, err, notFoundRequests)
	}
}

// TestDeleteBatchReportsDocumentsItDidNotDelete checks the per-document
// results of a delete batch: a key that is already gone counts as deleted, a
// document whose precondition failed is returned, and neither is retried.
func TestDeleteBatchReportsDocumentsItDidNotDelete(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body := `{"results":[` +
			`{"key":"a","revision":"r1"},` +
			`{"key":"b","error":{"code":"not_found","reason":"resource","error":"the document does not exist"}},` +
			`{"key":"c","error":{"code":"precondition_failed","reason":"precondition","error":"the document's precondition failed"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	a := &app{stderr: &bytes.Buffer{}, sleep: func(context.Context, time.Duration) error { return nil }}
	client, err := httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := a.sendDeleteBatch(t.Context(), client, "acme", "docs", []string{"a", "b", "c"})
	if err != nil || requests != 1 || len(failed) != 1 || failed[0].Key != "c" || failed[0].Error.Value.Code != "precondition_failed" {
		t.Fatalf("failed=%+v err=%v requests=%d", failed, err, requests)
	}
}

// TestSyncStopsBeforeDeletionsWhenADocumentIsNotStored renames a file and
// changes another, and has the index refuse the new name. The other upload
// is still written, but the old name stays, nothing is deleted, the tags do
// not move, and the command fails naming the document.
func TestSyncStopsBeforeDeletionsWhenADocumentIsNotStored(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"guide.md": "guide\n", "README.md": "one\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	first := git("rev-parse", "HEAD")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}

	git("mv", "guide.md", "intro.md")
	writeFiles(t, dir, map[string]string{"README.md": "two\n"})
	git("add", ".")
	git("commit", "-q", "-m", "second")
	stack.mu.Lock()
	stack.refuse = map[string]bool{"intro.md": true}
	stack.batchSizes = nil
	stack.mu.Unlock()

	err := runGv(a, stdout, stderr, "sync", dir, "acme/docs")
	var rejected *rejectedError
	if !errors.As(err, &rejected) || rejected.count != 1 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "1 document was not uploaded") || !strings.Contains(err.Error(), "no documents were deleted") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr.String(), "gv: intro.md was not uploaded: the document's precondition failed (precondition_failed)") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if stack.objects["README.md"] != "two\n" {
		t.Fatalf("README content = %q", stack.objects["README.md"])
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != "[README.md guide.md]" || len(stack.batchSizes) != 0 {
		t.Fatalf("objects = %v, delete batches = %v", got, stack.batchSizes)
	}
	if tags := indexTags(stack); tags[gitCommitTag] != first {
		t.Fatalf("tags = %v, want commit %s", tags, first)
	}

	// Once the index takes the document, the same sync completes.
	stack.mu.Lock()
	stack.refuse = nil
	stack.mu.Unlock()
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatalf("second attempt: %v\n%s", err, stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != "[README.md intro.md]" {
		t.Fatalf("objects = %v", got)
	}
	if tags := indexTags(stack); tags[gitCommitTag] != git("rev-parse", "HEAD") {
		t.Fatalf("tags = %v", tags)
	}
}

// TestSyncKeepsTheTagsWhenADocumentIsNotDeleted has the index refuse one of
// two deletions: the other is applied, and the tags stay where they were.
func TestSyncKeepsTheTagsWhenADocumentIsNotDeleted(t *testing.T) {
	dir, git := gitRepoForTest(t)
	writeFiles(t, dir, map[string]string{"keep.txt": "keep\n", "old.md": "old\n", "stuck.md": "stuck\n"})
	git("add", ".")
	git("commit", "-q", "-m", "initial")
	first := git("rev-parse", "HEAD")
	stack := newSyncStack()
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "sync", dir, "acme/docs"); err != nil {
		t.Fatal(err)
	}
	git("rm", "-q", "old.md", "stuck.md")
	git("commit", "-q", "-m", "remove")
	stack.mu.Lock()
	stack.refuse = map[string]bool{"stuck.md": true}
	stack.mu.Unlock()

	err := runGv(a, stdout, stderr, "sync", dir, "acme/docs")
	if err == nil || !strings.Contains(err.Error(), "1 document was not deleted; the index tags were not changed") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(stderr.String(), "gv: stuck.md was not deleted: the document's precondition failed (precondition_failed)") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := sortedKeys(stack.objects); fmt.Sprint(got) != "[keep.txt stuck.md]" {
		t.Fatalf("objects = %v", got)
	}
	if tags := indexTags(stack); tags[gitCommitTag] != first {
		t.Fatalf("tags = %v, want commit %s", tags, first)
	}
}

// TestIndexTagCommandsSetListAndRemove drives `gv index tag` and `--tag`
// against the in-process stack.
func TestIndexTagCommandsSetListAndRemove(t *testing.T) {
	stack := &fakeService{objects: map[string]string{}, revisions: map[string]string{}, indexes: map[string]*httpapi.Index{}}
	a, stdout, stderr := startStack(t, stack)
	if err := runGv(a, stdout, stderr, "index", "create", "acme/repo", "--tag", "team=search", "--tag", "env=dev"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Tags:\n  env=dev\n  team=search\n") {
		t.Fatalf("create output = %q", stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "create", "acme/bare"); err != nil || !strings.Contains(stdout.String(), "Tags: (none)\n") {
		t.Fatalf("create without tags: %v %q", err, stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "tag", "set", "acme/repo", "env=prod", "git.commit=abc=def"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Tags:\n  env=prod\n  git.commit=abc=def\n  team=search\n") {
		t.Fatalf("set output = %q", stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "tag", "ls", "acme/repo"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "KEY ") || !strings.HasPrefix(lines[1], "env ") || !strings.HasSuffix(lines[1], "prod") || !strings.HasPrefix(lines[2], "git.commit ") {
		t.Fatalf("tag ls output = %q", stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "tag", "ls", "acme/repo", "--json"); err != nil || stdout.String() != `{"env":"prod","git.commit":"abc=def","team":"search"}`+"\n" {
		t.Fatalf("tag ls --json: %v %q", err, stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "tag", "rm", "acme/repo", "env", "git.commit"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Tags:\n  team=search\n") {
		t.Fatalf("rm output = %q", stdout.String())
	}
	// A description change leaves the tags alone.
	if err := runGv(a, stdout, stderr, "index", "update", "acme/repo", "--description", "The repository."); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Tags:\n  team=search\n") || !strings.Contains(stdout.String(), "Description:\n  The repository.\n") {
		t.Fatalf("update output = %q", stdout.String())
	}
	if err := runGv(a, stdout, stderr, "index", "info", "acme/repo", "--json"); err != nil || !strings.Contains(stdout.String(), `"tags":{"team":"search"}`) {
		t.Fatalf("info --json: %v %q", err, stdout.String())
	}

	// Bad tags are refused before any request.
	writes := stack.writes
	for _, args := range [][]string{
		{"index", "tag", "set", "acme/repo", "novalue"},
		{"index", "tag", "set", "acme/repo", "=x"},
		{"index", "tag", "set", "acme/repo", "has space=x"},
		{"index", "tag", "set", "acme/repo", "k=" + strings.Repeat("v", 1025)},
		{"index", "tag", "rm", "acme/repo", "has space"},
		{"index", "create", "acme/next", "--tag", "k=line\nbreak"},
	} {
		if err := runGv(a, stdout, stderr, args...); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	if stack.writes != writes {
		t.Fatalf("refused tags reached the service: writes %d -> %d", writes, stack.writes)
	}
	if err := runGv(a, stdout, stderr, "index", "tag", "ls", "acme/missing"); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("missing index error = %v", err)
	}
}
