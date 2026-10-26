package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

// The tags `gv sync` keeps on an index. They are ordinary tags; these are the
// names the web UI and the next sync look for.
const (
	gitCommitTag = "git.commit"
	gitBranchTag = "git.branch"
)

// deleteBatchKeys is the public batch limit for deletions.
const deleteBatchKeys = 2048

type syncOptions struct {
	dryRun bool
	json   bool
}

func newSyncCommand(a *app) *cobra.Command {
	var options syncOptions
	command := &cobra.Command{
		Use:   "sync DIRECTORY ORGANIZATION/INDEX",
		Short: "Make an index mirror a directory",
		Long: `Make an index mirror a directory.

In a git work tree, sync compares the commit the index was last synced from,
recorded in its git.commit tag, with HEAD. It uploads the tracked files that
range added or changed, deletes the documents of the files it removed, and
then records HEAD in the git.commit and git.branch tags. The first sync, a
directory outside git, and an index whose recorded commit this repository does
not have compare the whole directory with the whole index instead: files that
are new or changed are uploaded and documents without a file are deleted.

Files are read from the working tree, so tracked files with uncommitted
changes stop the sync before anything is written.

If a document is not stored or not deleted, sync reports it and finishes that
step, then stops with a non-zero exit status. It deletes nothing after a
document that was not stored, and it changes no tags after either.`,
		Args: cobra.ExactArgs(2),
		RunE: func(command *cobra.Command, args []string) error {
			organization, index, err := a.parseIndexReference(args[1])
			if err != nil {
				return err
			}
			a.jsonOutput, a.totals = options.json, transferSummary{}
			err = a.sync(command.Context(), organization, index, args[0], options)
			if options.dryRun {
				return err
			}
			return a.reportTotals(err)
		},
	}
	command.Flags().BoolVar(&options.dryRun, "dry-run", false, "print what would change without writing to the index")
	command.Flags().BoolVar(&options.json, "json", false, "print refused documents and the totals, or the --dry-run plan, as JSON")
	return command
}

func (a *app) sync(ctx context.Context, organization, index, directory string, options syncOptions) error {
	directory, err := resolveSyncDirectory(directory)
	if err != nil {
		return err
	}
	client, _, _, err := a.client(true)
	if err != nil {
		return err
	}
	current, err := fetchIndex(ctx, client, organization, index)
	if err != nil {
		return err
	}

	status := a.newStatus()
	a.status.Store(status)
	defer a.status.Store(nil)
	status.start()
	finished := false
	finish := func() {
		if !finished {
			finished = true
			status.finish()
		}
	}
	defer finish()
	warn := func(message string) { status.printf("gv: warning: %s\n", message) }
	notice := func(format string, args ...any) { status.printf("gv: "+format+"\n", args...) }

	var plan syncPlan
	var patch httpapi.IndexTagChanges
	var summary string
	repo, err := a.detectGit(ctx, directory, notice)
	if err != nil {
		return err
	}
	if repo == nil {
		plan, err = a.planTwoWay(ctx, client, organization, index, walkedFiles(directory, status.scanned), warn)
		if err != nil {
			return err
		}
		patch = removeGitTags(current.Tags)
		summary = "full comparison"
	} else {
		head, err := repo.head(ctx)
		if err != nil {
			return err
		}
		branch, err := repo.branch(ctx)
		if err != nil {
			return err
		}
		base := current.Tags[gitCommitTag]
		if base == head {
			finish()
			fmt.Fprintf(a.stderr, "gv: %s/%s is already up to date at %s\n", organization, index, shortCommit(head))
			return nil
		}
		// The bytes come from the working tree while the tag says HEAD, so a
		// tree that differs from HEAD would make the tag a lie and the next
		// sync miss what changed. Untracked files are not in question: they
		// are never synced.
		dirty, err := repo.dirty(ctx)
		if err != nil {
			return err
		}
		if dirty {
			return errors.New("the working tree has uncommitted changes to tracked files; commit or stash them first")
		}
		known := false
		if base != "" {
			if known, err = repo.hasCommit(ctx, base); err != nil {
				return err
			}
		}
		if known {
			changes, err := repo.changes(ctx, base, head)
			if err != nil {
				return err
			}
			if plan, err = planFromGitDiff(directory, changes, warn); err != nil {
				return err
			}
			summary = fmt.Sprintf("commits %s..%s", shortCommit(base), shortCommit(head))
		} else {
			if base != "" {
				notice("notice: the index was last synced from commit %s, which this repository does not have; comparing the whole directory", shortCommit(base))
			}
			plan, err = a.planTwoWay(ctx, client, organization, index, trackedFiles(repo, directory), warn)
			if err != nil {
				return err
			}
			summary = "commit " + shortCommit(head)
		}
		patch = gitTagPatch(current.Tags, head, branch)
	}
	status.scanFinished(int64(len(plan.uploads)))

	if options.dryRun {
		finish()
		if options.json {
			printSyncPlanJSON(a.stdout, plan, patch)
		} else {
			printSyncPlan(a.stdout, plan, patch)
		}
		fmt.Fprintf(a.stderr, "gv: dry run: %d to upload, %d to delete, %d tag %s; nothing written\n", len(plan.uploads), len(plan.deletes), len(patch), plural(int64(len(patch)), "change"))
		return nil
	}
	// Uploads first, then deletions, then the tags. A rename never leaves the
	// index without either name, and the tags move only after every write
	// succeeded, so an interrupted sync starts over from the same base: its
	// batches carry the same idempotency keys and are not written twice.
	//
	// A batch applies each document on its own, so a document the index
	// refused leaves the rest of its batch written. The uploads still run to
	// the end, but a refused upload stops the sync before the deletions, which
	// could otherwise remove the old name of a rename whose new name is not
	// there, and a refused deletion stops it before the tags, which would
	// otherwise record a commit the index does not match.
	if len(plan.uploads) > 0 {
		if err := a.uploadFiles(ctx, organization, index, status, true, plan.produce, encodeUploads); err != nil {
			if _, ok := errors.AsType[*rejectedError](err); ok {
				return fmt.Errorf("%w; no documents were deleted and the index tags were not changed", err)
			}
			return err
		}
	}
	if len(plan.deletes) > 0 {
		status.deleting(int64(len(plan.deletes)))
		if err := a.deleteKeys(ctx, client, organization, index, plan.deletes, status); err != nil {
			if _, ok := errors.AsType[*rejectedError](err); ok {
				return fmt.Errorf("%w; the index tags were not changed", err)
			}
			return err
		}
	}
	if len(patch) > 0 {
		if err := updateSyncTags(ctx, client, organization, index, patch); err != nil {
			return err
		}
	}
	finish()
	fmt.Fprintf(a.stderr, "gv: synced %s to %s/%s: %d uploaded, %d deleted, %d skipped (%s)\n", directory, organization, index, len(plan.uploads), len(plan.deletes), plan.skipped, summary)
	return nil
}

func resolveSyncDirectory(directory string) (string, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect directory: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", directory)
	}
	return resolved, nil
}

func shortCommit(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

// ---- git ------------------------------------------------------------------

// gitRepo runs git inside one directory. Every command is scoped to that
// directory, so a directory below the root of a repository syncs only what is
// under it, with keys relative to it.
type gitRepo struct{ dir string }

type gitError struct {
	args   []string
	code   int
	stderr string
}

func (e *gitError) Error() string {
	message := strings.TrimSpace(e.stderr)
	if message == "" {
		message = fmt.Sprintf("exit status %d", e.code)
	}
	return fmt.Sprintf("git %s: %s", strings.Join(e.args, " "), message)
}

func gitExitCode(err error) (int, bool) {
	if failure, ok := errors.AsType[*gitError](err); ok {
		return failure.code, true
	}
	return 0, false
}

func (g *gitRepo) run(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = g.dir
	// GIT_OPTIONAL_LOCKS keeps `git status` from rewriting the index file of a
	// repository it is only asked about; LC_ALL keeps its messages stable.
	command.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return stdout.Bytes(), &gitError{args: args, code: exit.ExitCode(), stderr: stderr.String()}
		}
		return nil, fmt.Errorf("run git: %w", err)
	}
	return stdout.Bytes(), nil
}

// detectGit reports the repository the directory belongs to, or nil when it
// belongs to none or git is not installed.
func (a *app) detectGit(ctx context.Context, directory string, notice func(string, ...any)) (*gitRepo, error) {
	if _, err := exec.LookPath("git"); err != nil {
		if _, statErr := os.Stat(filepath.Join(directory, ".git")); statErr == nil {
			notice("notice: %s looks like a git repository but git is not installed; syncing it as a plain directory", directory)
		}
		return nil, nil
	}
	repo := &gitRepo{dir: directory}
	out, err := repo.run(ctx, "rev-parse", "--is-inside-work-tree", "--is-inside-git-dir")
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 128 {
			return nil, nil
		}
		return nil, err
	}
	answers := strings.Fields(string(out))
	if len(answers) == 2 && answers[1] == "true" {
		return nil, fmt.Errorf("%s is inside a .git directory", directory)
	}
	if len(answers) == 0 || answers[0] != "true" {
		return nil, nil
	}
	return repo, nil
}

func (g *gitRepo) head(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return "", errors.New("the repository has no commits; commit first")
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// branch is the short name of the branch HEAD is on, or empty when HEAD is
// detached.
func (g *gitRepo) branch(ctx context.Context) (string, error) {
	out, err := g.run(ctx, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil {
		if code, ok := gitExitCode(err); ok && code == 1 {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *gitRepo) hasCommit(ctx context.Context, hash string) (bool, error) {
	if !isCommitHash(hash) {
		return false, nil
	}
	if _, err := g.run(ctx, "cat-file", "-e", hash+"^{commit}"); err != nil {
		if _, ok := gitExitCode(err); ok {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func isCommitHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// dirty reports whether any tracked file under the directory differs from
// HEAD. Untracked files and submodules do not count: neither is synced.
func (g *gitRepo) dirty(ctx context.Context) (bool, error) {
	out, err := g.run(ctx, "status", "--porcelain", "-z", "--untracked-files=no", "--ignore-submodules=all", "--", ".")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

func (g *gitRepo) changes(ctx context.Context, base, head string) ([]gitChange, error) {
	out, err := g.run(ctx, "diff", "--raw", "-z", "--no-renames", "--relative", base, head, "--", ".")
	if err != nil {
		return nil, err
	}
	return parseRawDiff(out)
}

// trackedPaths calls emit for every regular file git tracks under the
// directory, relative to it.
func (g *gitRepo) trackedPaths(ctx context.Context, emit func(relative string) error) error {
	out, err := g.run(ctx, "ls-files", "-z", "-s", "--", ".")
	if err != nil {
		return err
	}
	return parseLsFiles(out, emit)
}

// gitChange is one record of `git diff --raw`.
type gitChange struct {
	oldMode, newMode uint32
	oldSHA, newSHA   string
	status           byte
	path             string
}

func isRegularBlob(mode uint32) bool { return mode == 0o100644 || mode == 0o100755 }

// parseRawDiff reads the NUL-separated raw diff format: for every change a
// record `:<old mode> <new mode> <old hash> <new hash> <status>` followed by
// the path, each NUL-terminated. Renames are disabled, so a record never
// carries a second path.
func parseRawDiff(output []byte) ([]gitChange, error) {
	fields := bytes.Split(output, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, errors.New("git diff output ended inside a record")
	}
	changes := make([]gitChange, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		meta := string(fields[i])
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("unexpected git diff record %q", meta)
		}
		parts := strings.Fields(meta[1:])
		if len(parts) != 5 || parts[4] == "" {
			return nil, fmt.Errorf("unexpected git diff record %q", meta)
		}
		oldMode, err := strconv.ParseUint(parts[0], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("unexpected git diff record %q", meta)
		}
		newMode, err := strconv.ParseUint(parts[1], 8, 32)
		if err != nil {
			return nil, fmt.Errorf("unexpected git diff record %q", meta)
		}
		status := parts[4][0]
		switch status {
		case 'A', 'M', 'D', 'T':
		default:
			return nil, fmt.Errorf("unsupported git diff status %q for %q", parts[4], fields[i+1])
		}
		changes = append(changes, gitChange{oldMode: uint32(oldMode), newMode: uint32(newMode), oldSHA: parts[2], newSHA: parts[3], status: status, path: string(fields[i+1])})
	}
	return changes, nil
}

// parseLsFiles reads `git ls-files -z -s`: `<mode> <hash> <stage>\t<path>`
// per NUL-terminated record. Only regular blobs at stage zero are files to
// sync; a symbolic link, a submodule and an unmerged entry are not.
func parseLsFiles(output []byte, emit func(relative string) error) error {
	for record := range bytes.SplitSeq(output, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		head, path, ok := strings.Cut(string(record), "\t")
		if !ok {
			return fmt.Errorf("unexpected git ls-files record %q", record)
		}
		parts := strings.Fields(head)
		if len(parts) != 3 {
			return fmt.Errorf("unexpected git ls-files record %q", record)
		}
		mode, err := strconv.ParseUint(parts[0], 8, 32)
		if err != nil {
			return fmt.Errorf("unexpected git ls-files record %q", record)
		}
		if !isRegularBlob(uint32(mode)) || parts[2] != "0" {
			continue
		}
		if err := emit(path); err != nil {
			return err
		}
	}
	return nil
}

// ---- planning -------------------------------------------------------------

// syncPlan is what a sync will do: the files to upload, in a deterministic
// order, the keys to delete, sorted, and how many files it left out.
type syncPlan struct {
	uploads []uploadFile
	deletes []string
	skipped int
}

// produce feeds the planned uploads to the upload pipeline.
func (p *syncPlan) produce(ctx context.Context, emit func(uploadFile) error) error {
	for _, file := range p.uploads {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(file); err != nil {
			return err
		}
	}
	return nil
}

// localFile describes one path git named, relative to the directory. It never
// follows a symbolic link: the walker skips links by their directory entry,
// and a path that came from git is checked the same way before it is read.
func localFile(directory, relative string) (uploadFile, os.FileInfo, error) {
	key, err := normalizeRemotePath(relative, false)
	if err != nil {
		return uploadFile{}, nil, err
	}
	local := filepath.Join(directory, filepath.FromSlash(relative))
	info, err := os.Lstat(local)
	if err != nil {
		return uploadFile{}, nil, err
	}
	if !info.Mode().IsRegular() {
		return uploadFile{}, nil, errors.New("not a regular file")
	}
	return uploadFile{localPath: local, key: key}, info, nil
}

// planFromGitDiff turns the changes between two commits into a plan. A path
// whose new state is a regular file is uploaded; a path that was a regular
// file and no longer is, or is gone, is deleted; a change between a symbolic
// link and a submodule concerns nothing the index holds.
func planFromGitDiff(directory string, changes []gitChange, warn func(string)) (syncPlan, error) {
	var plan syncPlan
	for _, change := range changes {
		switch {
		case isRegularBlob(change.newMode):
			if change.status == 'M' && isRegularBlob(change.oldMode) && change.oldSHA == change.newSHA {
				// Only the mode changed; the content the index holds did not.
				continue
			}
			file, info, err := localFile(directory, change.path)
			if err != nil {
				return syncPlan{}, fmt.Errorf("%s: %w", change.path, err)
			}
			if info.Size() > maxDocumentSize {
				warn(fmt.Sprintf("skipping %s: exceeds the 1 GiB document limit", change.path))
				plan.skipped++
				continue
			}
			plan.uploads = append(plan.uploads, file)
		case isRegularBlob(change.oldMode):
			key, err := normalizeRemotePath(change.path, false)
			if err != nil {
				// A path that cannot be a key was never uploaded.
				continue
			}
			plan.deletes = append(plan.deletes, key)
		}
	}
	slices.Sort(plan.deletes)
	return plan, nil
}

// remoteDocument is what a listing says about one document, as far as the
// comparison with a local file needs.
type remoteDocument struct {
	size     int64
	mtime    time.Time
	hasMtime bool
}

// localEnumerator names the regular files under a directory with what
// Lstat reports about each.
type localEnumerator func(ctx context.Context, emit func(file uploadFile, info os.FileInfo) error) error

// planTwoWay compares the whole index with the whole directory. It holds one
// entry per document in memory, about a hundred bytes each, so an index of a
// million documents costs on the order of a hundred megabytes to compare; the
// uploads it plans are held as well, so a first sync of a tree that size costs
// a few hundred megabytes in all.
func (a *app) planTwoWay(ctx context.Context, client *httpapi.Client, organization, index string, local localEnumerator, warn func(string)) (syncPlan, error) {
	remote := map[string]remoteDocument{}
	err := listIndexDocuments(ctx, client, organization, index, func(document httpapi.DocumentSummary) {
		remote[document.Key] = remoteFromSummary(document)
	})
	if err != nil {
		return syncPlan{}, err
	}
	var plan syncPlan
	err = local(ctx, func(file uploadFile, info os.FileInfo) error {
		document, known := remote[file.key]
		delete(remote, file.key)
		if info.Size() > maxDocumentSize {
			// Never uploaded, and whatever the index holds under the key is
			// left alone rather than deleted for a file that exists.
			warn(fmt.Sprintf("skipping %s: exceeds the 1 GiB document limit", file.key))
			plan.skipped++
			return nil
		}
		if !known || localChanged(document, info) {
			plan.uploads = append(plan.uploads, file)
		}
		return nil
	})
	if err != nil {
		return syncPlan{}, err
	}
	for key := range remote {
		plan.deletes = append(plan.deletes, key)
	}
	slices.Sort(plan.deletes)
	return plan, nil
}

func remoteFromSummary(document httpapi.DocumentSummary) remoteDocument {
	remote := remoteDocument{size: int64(document.ContentBytes)}
	if value, ok := document.Metadata["mtime"]; ok {
		if stamp, ok := value.GetTimestampMetadataValue(); ok {
			remote.mtime, remote.hasMtime = stamp.Value, true
		}
	}
	return remote
}

// localChanged decides whether a local file differs from the document under
// its key, from the size and the modification time the upload recorded, the
// way rsync decides by default. The listing reports the time in whole
// seconds, so the comparison does too.
func localChanged(remote remoteDocument, info os.FileInfo) bool {
	return !remote.hasMtime || remote.size != info.Size() ||
		!remote.mtime.Truncate(time.Second).Equal(info.ModTime().Truncate(time.Second))
}

func listIndexDocuments(ctx context.Context, client *httpapi.Client, organization, index string, visit func(httpapi.DocumentSummary)) error {
	params := httpapi.ListDocumentsParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index), Amount: httpapi.NewOptInt(200)}
	for {
		page, err := client.ListDocuments(ctx, params)
		if err != nil {
			return fmt.Errorf("list documents: %w", friendlyAPIError(err))
		}
		for _, document := range page.Results {
			visit(document)
		}
		if !page.Pagination.HasNext {
			return nil
		}
		params.Page = httpapi.NewOptString(page.Pagination.NextPageID)
	}
}

// trackedFiles enumerates the regular files git tracks under the directory.
// A tracked file that is missing or not a regular file on disk is an
// inconsistency between the repository and the tree, and stops the sync.
func trackedFiles(repo *gitRepo, directory string) localEnumerator {
	return func(ctx context.Context, emit func(uploadFile, os.FileInfo) error) error {
		return repo.trackedPaths(ctx, func(relative string) error {
			file, info, err := localFile(directory, relative)
			if err != nil {
				return fmt.Errorf("%s: %w", relative, err)
			}
			return emit(file, info)
		})
	}
}

// walkedFiles enumerates a directory outside git the way `gv upload` does:
// .git pruned, .gitignore honored, symbolic links skipped.
func walkedFiles(directory string, seen func(directoriesFound, directoriesDone, files int64)) localEnumerator {
	return func(ctx context.Context, emit func(uploadFile, os.FileInfo) error) error {
		return walkSource(ctx, directory, "", func(file uploadFile) error {
			info, err := os.Lstat(file.localPath)
			if err != nil {
				return fmt.Errorf("inspect %s: %w", file.localPath, err)
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			return emit(file, info)
		}, seen)
	}
}

// printSyncPlanJSON writes a dry run as one JSON line per upload, deletion and
// tag change; a removed tag has a null value.
func printSyncPlanJSON(w io.Writer, plan syncPlan, patch httpapi.IndexTagChanges) {
	for _, file := range plan.uploads {
		_ = writeJSONLine(w, map[string]string{"action": "upload", "key": file.key})
	}
	for _, key := range plan.deletes {
		_ = writeJSONLine(w, map[string]string{"action": "delete", "key": key})
	}
	for _, key := range sortedTagKeys(httpapi.IndexTags(tagPatchKeys(patch))) {
		var value *string
		if v, ok := patch[key].Get(); ok {
			value = &v
		}
		_ = writeJSONLine(w, map[string]any{"action": "tag", "key": key, "value": value})
	}
}

// printSyncPlan writes a dry run: one line per upload, deletion and tag change.
func printSyncPlan(w io.Writer, plan syncPlan, patch httpapi.IndexTagChanges) {
	for _, file := range plan.uploads {
		fmt.Fprintf(w, "+ %s\n", file.key)
	}
	for _, key := range plan.deletes {
		fmt.Fprintf(w, "- %s\n", key)
	}
	for _, key := range sortedTagKeys(httpapi.IndexTags(tagPatchKeys(patch))) {
		if value, ok := patch[key].Get(); ok {
			fmt.Fprintf(w, "~ tag %s=%s\n", key, value)
		} else {
			fmt.Fprintf(w, "~ tag %s (removed)\n", key)
		}
	}
}

func tagPatchKeys(patch httpapi.IndexTagChanges) map[string]string {
	keys := make(map[string]string, len(patch))
	for key := range patch {
		keys[key] = ""
	}
	return keys
}

// ---- deletions ------------------------------------------------------------

// deleteKeys removes the documents under keys in batches. A key that is
// already gone counts as deleted: an earlier interrupted sync may have removed
// it, or the file may never have been uploaded at all. Any other document a
// batch did not delete is reported, the remaining batches still run, and the
// deletion then ends with a *rejectedError that counts them.
func (a *app) deleteKeys(ctx context.Context, client *httpapi.Client, organization, index string, keys []string, status *progress) error {
	var rejected int64
	for start := 0; start < len(keys); start += deleteBatchKeys {
		batch := keys[start:min(start+deleteBatchKeys, len(keys))]
		failed, err := a.sendDeleteBatch(ctx, client, organization, index, batch)
		if err != nil {
			return err
		}
		for _, failure := range failed {
			reportRejected(status, failure, "deleted")
		}
		rejected += int64(len(failed))
		status.deleted(len(batch) - len(failed))
	}
	if rejected > 0 {
		return &rejectedError{count: rejected, action: "deleted"}
	}
	return nil
}

// deleteID is the idempotency key of one delete batch: a function of the keys
// it holds, so an interrupted sync that plans the same batch again presents
// the same request.
func deleteID(organization, index string, keys []string) string {
	var data bytes.Buffer
	data.WriteString("delete\x00")
	for _, key := range keys {
		data.WriteString(key)
		data.WriteByte(0)
	}
	return uploadID(organization, index, data.Bytes())
}

// sendDeleteBatch deletes one batch and returns the results of the documents
// it did not delete for a reason other than their being gone already.
func (a *app) sendDeleteBatch(ctx context.Context, client *httpapi.Client, organization, index string, keys []string) ([]httpapi.MutationResult, error) {
	records := make([]httpapi.DeleteObjectRecord, len(keys))
	for i, key := range keys {
		records[i] = httpapi.DeleteObjectRecord{Key: key}
	}
	request := &httpapi.DeleteObjectsRequest{Objects: records}
	params := httpapi.DeleteObjectsParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index), IdempotencyKey: deleteID(organization, index, keys)}
	var throttledWait time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		if err := a.awaitThrottle(ctx); err != nil {
			return nil, err
		}
		response, err := client.DeleteObjects(ctx, request, params)
		if err == nil {
			var failed []httpapi.MutationResult
			for _, result := range batchFailures(response.Results) {
				if result.Error.Value.Code != "not_found" {
					failed = append(failed, result)
				}
			}
			return failed, nil
		}
		if again, throttleErr := a.throttled(ctx, err, &throttledWait); throttleErr != nil {
			return nil, fmt.Errorf("delete batch: %w", throttleErr)
		} else if again {
			attempt--
			continue
		}
		retry, public := classifyUploadError(err)
		if !retry {
			return nil, public
		}
		err = public
		if attempt == 2 {
			return nil, fmt.Errorf("delete batch: %w", err)
		}
		if sleepErr := a.sleep(ctx, time.Duration(attempt+1)*100*time.Millisecond); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, nil
}

// ---- tags -----------------------------------------------------------------

// gitTagPatch records HEAD on the index: the commit, and the branch or its
// removal when HEAD is detached. Tags already equal are left out.
func gitTagPatch(current httpapi.IndexTags, head, branch string) httpapi.IndexTagChanges {
	patch := httpapi.IndexTagChanges{}
	if current[gitCommitTag] != head {
		patch[gitCommitTag] = nilString(head)
	}
	if branch != "" {
		if current[gitBranchTag] != branch {
			patch[gitBranchTag] = nilString(branch)
		}
	} else if _, ok := current[gitBranchTag]; ok {
		patch[gitBranchTag] = nullString()
	}
	return patch
}

// removeGitTags drops the git tags after a sync from a directory outside git,
// which they would otherwise misdescribe.
func removeGitTags(current httpapi.IndexTags) httpapi.IndexTagChanges {
	patch := httpapi.IndexTagChanges{}
	for _, key := range []string{gitCommitTag, gitBranchTag} {
		if _, ok := current[key]; ok {
			patch[key] = nullString()
		}
	}
	return patch
}

func updateSyncTags(ctx context.Context, client *httpapi.Client, organization, index string, patch httpapi.IndexTagChanges) error {
	response, err := client.UpdateIndex(ctx, &httpapi.UpdateIndexRequest{Tags: httpapi.NewOptIndexTagChanges(patch)}, httpapi.UpdateIndexParams{Organization: httpapi.Name(organization), Index: httpapi.IndexName(index)})
	if err != nil {
		return fmt.Errorf("update index tags: %w", friendlyAPIError(err))
	}
	switch value := response.(type) {
	case *httpapi.Index:
		return nil
	case *httpapi.NotFoundHeaders:
		return responseError(value.Response, value.XRequestID)
	default:
		return fmt.Errorf("update index tags: unexpected response %T", response)
	}
}
