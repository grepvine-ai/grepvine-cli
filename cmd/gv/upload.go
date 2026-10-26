package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	ogenhttp "github.com/ogen-go/ogen/http"
	"github.com/spf13/cobra"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

const (
	maxBatchContent = 16 << 20
	// maxDocumentSize is the largest document the service stores. Content up
	// to 100 MiB that is valid UTF-8 is searchable; anything else is stored
	// and served without search, which is what lets a documentation tree
	// carry its images and archives beside the text that links to them.
	maxDocumentSize = 1 << 30
	// Use the public batch limit: every batch pays a fixed per-request cost,
	// so small batches make large recursive uploads pay it far too often. The
	// 16 MiB content limit still bounds each batch.
	uploadBatchFiles = 2048
	// defaultUploadConcurrency is how many batches an upload keeps in flight.
	// Each waits for its durable acknowledgment, so the rate of a link is
	// roughly this many batches per acknowledgment time; four capped a
	// host-local upload near 8 MB/s.
	defaultUploadConcurrency = 16
	maxUploadConcurrency     = 64
	// Transient failures get 45 seconds of bounded backoff across eight attempts.
	uploadRetryAttempts = 8
	// uploadNameBuffer is how many names the walk may run ahead of the reader
	// pool. Names are cheap next to content, so a buffer this size costs a few
	// megabytes and keeps the pool fed across a slow directory.
	uploadNameBuffer = 32768
)

type uploadFile struct {
	localPath string
	key       string
	size      int64
	mtime     time.Time
	// object is set for a file read from S3 rather than from localPath.
	object *s3Object
	// entry is set for a file read from the zip archive at localPath.
	entry *zip.File
}

// name is how messages refer to the file.
func (f uploadFile) name() string {
	switch {
	case f.object != nil:
		return f.object.uri()
	case f.entry != nil:
		return f.localPath + ":" + f.entry.Name
	}
	return f.localPath
}

// uploadJob is one request. A batch carries its encoded body in data; a file
// published on its own has none, and carries its content only when it was
// already read, so a file that is not text is not read a second time.
type uploadJob struct {
	files   []uploadFile
	data    []byte
	content []byte
	bytes   int64
	// source is how much of a record file the job was read from, which an
	// import measures its progress by.
	source int64
}

type uploadRecord struct {
	Key         string         `json:"key"`
	Content     string         `json:"content"`
	Metadata    map[string]any `json:"metadata"`
	IfNoneMatch string         `json:"if_none_match,omitempty"`
}

func newUploadCommand(a *app) *cobra.Command {
	var target string
	var recursive, records, archive, asJSON bool
	var concurrency int
	var compress string
	command := &cobra.Command{
		Use:   "upload SOURCE [REMOTE_PATH] --index ORGANIZATION/INDEX",
		Short: "Upload a file, or a directory with --recursive",
		Long: `Upload a file, or a directory with --recursive.

SOURCE is a local path or an s3://BUCKET/KEY URI. An S3 source is read with
the standard AWS credentials: environment variables, the shared configuration
and credentials files, SSO, or an instance or container role. An S3 prefix
uploads every object under it, as a directory would, and needs --recursive.

With --records, SOURCE holds documents rather than being them: NDJSON files of
one {"key", "content", "metadata"} object per line, the form of a batch
request, gzip-compressed or not. A directory or prefix contributes its
.ndjson and .jsonl files, with or without .gz. REMOTE_PATH, when given,
prefixes every key.

With --zip, SOURCE is a local zip archive whose files are uploaded under
their paths in the archive, as if it were extracted to a directory, without
writing them to disk. REMOTE_PATH, when given, prefixes every key. With
--zip and --records together, the archive's .ndjson and .jsonl files, with
or without .gz, are imported as records.`,
		Args: cobra.RangeArgs(1, 4),
		RunE: func(command *cobra.Command, args []string) (err error) {
			a.jsonOutput, a.totals = asJSON, transferSummary{}
			defer func() { err = a.reportTotals(err) }()
			organization, index, source, prefix, err := uploadArguments(args, target)
			if err != nil {
				return err
			}
			if concurrency < 1 || concurrency > maxUploadConcurrency {
				return fmt.Errorf("--concurrency must be between 1 and %d", maxUploadConcurrency)
			}
			mode, err := parseCompression(compress)
			if err != nil {
				return err
			}
			location, remote, err := parseS3URI(source)
			if err != nil {
				return err
			}
			if archive && remote {
				return errors.New("--zip reads a local archive; download it first")
			}
			if !remote {
				info, err := os.Lstat(source)
				if err != nil {
					return fmt.Errorf("inspect source: %w", err)
				}
				if archive && !info.Mode().IsRegular() {
					return errors.New("--zip needs a zip archive as its source")
				}
				if info.IsDir() && !recursive {
					return errors.New("source is a directory; use --recursive")
				}
			}
			a.uploadConcurrency = concurrency
			a.compression.Store(int32(mode))
			if records {
				return a.uploadRecords(command.Context(), organization, index, source, prefix, recursive, archive)
			}
			if archive {
				return a.uploadZip(command.Context(), organization, index, source, prefix)
			}
			if remote {
				return a.uploadS3(command.Context(), organization, index, location, prefix, recursive)
			}
			return a.upload(command.Context(), organization, index, source, prefix)
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print refused documents and the totals as JSON")
	command.Flags().StringVar(&target, "index", "", "destination ORGANIZATION/INDEX")
	command.Flags().BoolVarP(&recursive, "recursive", "r", false, "recursively upload a directory or an S3 prefix")
	command.Flags().BoolVar(&records, "records", false, "read documents from NDJSON record files instead of uploading the files")
	command.Flags().BoolVar(&archive, "zip", false, "upload the files a zip archive holds instead of the archive itself")
	command.Flags().IntVar(&concurrency, "concurrency", defaultUploadConcurrency, "batches to keep in flight")
	command.Flags().StringVar(&compress, "compress", "auto", "compress batches: auto (not to a local endpoint), always, or never")
	return command
}

func uploadArguments(args []string, target string) (string, string, string, string, error) {
	if target == "" {
		if len(args) < 3 {
			return "", "", "", "", errors.New("--index ORGANIZATION/INDEX is required")
		}
		prefix := ""
		if len(args) == 4 {
			prefix = args[3]
		}
		return args[0], args[1], args[2], prefix, nil
	}
	if len(args) > 2 {
		return "", "", "", "", errors.New("upload accepts SOURCE and an optional REMOTE_PATH")
	}
	organization, index, ok := strings.Cut(target, "/")
	if !ok || organization == "" || index == "" || strings.Contains(index, "/") {
		return "", "", "", "", errors.New("index must be ORGANIZATION/INDEX")
	}
	prefix := ""
	if len(args) == 2 {
		prefix = args[1]
	}
	return organization, index, args[0], prefix, nil
}

func (a *app) upload(ctx context.Context, organization, index, source, prefix string) error {
	prefix, err := normalizeRemotePath(prefix, true)
	if err != nil {
		return fmt.Errorf("remote prefix: %w", err)
	}
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("inspect source: %w", err)
	}
	recursive := sourceInfo.IsDir()

	status := a.newStatus()
	a.status.Store(status)
	defer a.status.Store(nil)
	status.start()
	defer status.finish()

	// Count the source before uploading any of it. The pass costs seconds even
	// on a source of several million files, and it is what lets the upload that
	// follows report a total, a share done, and an estimate that mean something.
	total, err := countSource(ctx, source, status.scanned)
	if err != nil {
		return err
	}
	status.scanFinished(total)
	if total == 0 {
		return errors.New("source contains no regular files")
	}
	return a.uploadFiles(ctx, organization, index, status, recursive, func(ctx context.Context, emit func(uploadFile) error) error {
		return walkSource(ctx, source, prefix, emit, nil)
	}, encodeUploads)
}

// encodeFunc turns a stream of named files into request bodies; see
// encodeUploads.
type encodeFunc func(ctx context.Context, names <-chan uploadFile, recursive bool, warn func(string), consume func(uploadJob) error) error

// uploadFiles publishes the files produce names. The caller owns the progress
// line and has already told it how many files to expect; produce is the
// source of names, a directory walk for `gv upload` and a computed plan for
// `gv sync`, and it stops when the context it is given ends. encode reads the
// named files and turns them into requests.
//
// A document the service did not store is reported as it happens and the
// upload goes on with the rest; the upload then ends with a *rejectedError
// that counts them.
func (a *app) uploadFiles(ctx context.Context, organization, index string, status *progress, recursive bool, produce func(ctx context.Context, emit func(uploadFile) error) error, encode encodeFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	warn := func(message string) { status.printf("gv: warning: %s\n", message) }

	var client *httpapi.Client
	var clientErr error
	var clientOnce sync.Once
	getClient := func() (*httpapi.Client, error) {
		clientOnce.Do(func() { client, _, _, clientErr = a.client(true) })
		return client, clientErr
	}

	concurrency := a.uploadConcurrency
	if concurrency <= 0 {
		concurrency = defaultUploadConcurrency
	}
	failures := make(chan error, concurrency+2)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}

	// The producer names files, the encoder reads and batches them, and the
	// workers publish the batches. Running the three as one pipeline is what
	// lets the first batch reach the server while the source is still being
	// enumerated, rather than after a complete pass over it.
	// The producer runs ahead of the reader pool by at most this many names,
	// which is what keeps the memory of a source of several million files
	// bounded.
	names := make(chan uploadFile, uploadNameBuffer)
	queue := make(chan uploadJob, concurrency)

	var walk sync.WaitGroup
	walk.Go(func() {
		defer close(names)
		err := produce(ctx, func(file uploadFile) error {
			select {
			case names <- file:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			fail(err)
		}
	})

	var encoding sync.WaitGroup
	encoding.Go(func() {
		defer close(queue)
		err := encode(ctx, names, recursive, warn, func(job uploadJob) error {
			select {
			case queue <- job:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			fail(err)
		}
	})

	var rejected atomic.Int64
	var workers sync.WaitGroup
	for range concurrency {
		workers.Go(func() {
			for job := range queue {
				if len(job.files) == 0 {
					// A record file that held no documents.
					status.sent(0, 0, job.source)
					continue
				}
				client, err := getClient()
				if err != nil {
					fail(err)
					return
				}
				failed, err := a.sendUploadJob(ctx, client, organization, index, job)
				if err != nil {
					fail(err)
					return
				}
				// Only what was stored counts as sent, so the summary line
				// never reports a document that is not in the index.
				stored, storedBytes := len(job.files), job.bytes
				if len(failed) > 0 {
					rejected.Add(int64(len(failed)))
					failedKeys := make(map[string]bool, len(failed))
					for _, failure := range failed {
						failedKeys[failure.Key] = true
						reportRejected(status, failure, "uploaded")
					}
					for _, file := range job.files {
						if failedKeys[file.key] {
							stored--
							storedBytes -= file.size
						}
					}
				}
				status.sent(stored, storedBytes, job.source)
			}
		})
	}

	walk.Wait()
	encoding.Wait()
	workers.Wait()

	select {
	case err := <-failures:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if count := rejected.Load(); count > 0 {
		return &rejectedError{count: count, action: "uploaded"}
	}
	return nil
}

// rejectedError ends a write that left some documents unchanged. Each of
// them was reported when its batch returned, so the error only counts them.
type rejectedError struct {
	count  int64
	action string
}

func (e *rejectedError) Error() string {
	if e.count == 1 {
		return fmt.Sprintf("1 document was not %s", e.action)
	}
	return fmt.Sprintf("%d documents were not %s", e.count, e.action)
}

// batchFailures returns the results of a batch that report a document the
// batch did not change. Results are per document: the others were applied.
func batchFailures(results []httpapi.MutationResult) []httpapi.MutationResult {
	var failed []httpapi.MutationResult
	for _, result := range results {
		if result.Error.IsSet() {
			failed = append(failed, result)
		}
	}
	return failed
}

// reportRejected prints one document a batch did not change, with the
// service's reason for it.
func reportRejected(status *progress, result httpapi.MutationResult, action string) {
	reason := result.Error.Value
	status.printf("gv: %s was not %s: %s (%s)\n", result.Key, action, reason.Error, reason.Code)
	status.rejected(result)
}

func normalizeRemotePath(value string, allowEmpty bool) (string, error) {
	value = strings.ReplaceAll(value, "\\", "/")
	if allowEmpty {
		value = strings.TrimRight(value, "/")
	}
	if value == "" && allowEmpty {
		return "", nil
	}
	if strings.HasPrefix(value, "/") {
		return "", errors.New("must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned == "." && allowEmpty {
		return "", nil
	}
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("must not escape its root")
	}
	for component := range strings.SplitSeq(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", errors.New("contains an empty, dot, or dot-dot component")
		}
	}
	if len(cleaned) > 1024 {
		return "", errors.New("exceeds 1,024 UTF-8 bytes")
	}
	if !utf8.ValidString(cleaned) {
		return "", errors.New("is not valid UTF-8")
	}
	return cleaned, nil
}

// encodeUploads turns a stream of named files into request bodies. It reads
// and encodes one chunk at a time, so a recursive upload holds neither the
// corpus nor more than one batch of content, and the batches a run produces
// depend only on the order and the sizes of its files. Two runs over an
// unchanged source therefore publish the same idempotency keys, which is what
// lets an interrupted upload resume without writing a revision twice.
func encodeUploads(ctx context.Context, names <-chan uploadFile, recursive bool, warn func(string), consume func(uploadJob) error) error {
	pending := make([]uploadFile, 0, uploadBatchFiles)
	closed := false
	for !closed || len(pending) > 0 {
		// A chunk is always filled to the batch limit unless the stream ends
		// first, never to whatever happens to have arrived, so its boundaries
		// do not depend on how the walk and the uploads interleaved.
		for !closed && len(pending) < uploadBatchFiles {
			select {
			case file, ok := <-names:
				if !ok {
					closed = true
					break
				}
				pending = append(pending, file)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if len(pending) == 0 {
			break
		}
		results, consumed := readChunk(pending)
		if err := encodeChunk(results[:consumed], recursive, warn, consume); err != nil {
			return err
		}
		pending = append(pending[:0], pending[consumed:]...)
	}
	return nil
}

// encodeChunk writes one chunk of read files into request bodies. A chunk
// already fits inside one batch, so it produces one batch, plus a request of
// its own for every file too large to share one and for every file that is
// not text.
func encodeChunk(results []readResult, recursive bool, warn func(string), consume func(uploadJob) error) error {
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	// Content is text, and HTML escaping would spell every <, > and & in it
	// as six bytes.
	encoder.SetEscapeHTML(false)
	batch := make([]uploadFile, 0, len(results))
	var total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		job := uploadJob{files: batch, data: append([]byte(nil), body.Bytes()...), bytes: total}
		batch, total = make([]uploadFile, 0, len(results)), 0
		body.Reset()
		return consume(job)
	}
	for _, result := range results {
		if result.err != nil {
			return result.err
		}
		if result.deferred {
			// Unreachable: the budget refuses in batch order, so the first
			// file it defers is where the chunk ends. Publishing a deferred
			// file would publish it empty, so say so rather than do it.
			return fmt.Errorf("%s was read short of its content", result.file.name())
		}
		if result.irregular {
			continue
		}
		if result.skipped != "" {
			if !recursive {
				return fmt.Errorf("%s: %s", result.file.name(), result.skipped)
			}
			warn(fmt.Sprintf("skipping %s: %s", result.file.name(), result.skipped))
			continue
		}
		if result.oversize {
			if !recursive {
				return fmt.Errorf("%s exceeds the 1 GiB document limit", result.file.name())
			}
			warn(fmt.Sprintf("skipping %s: exceeds the 1 GiB document limit", result.file.name()))
			continue
		}
		if result.large || !utf8.Valid(result.content) {
			// A file too large to share a batch, or one that is not text, is
			// published on its own. The batch endpoint carries text as JSON
			// strings; the single-document endpoint streams any bytes, and the
			// service stores content it cannot search rather than refusing it.
			if err := flush(); err != nil {
				return err
			}
			if err := consume(uploadJob{files: []uploadFile{result.file}, content: result.content, bytes: result.file.size}); err != nil {
				return err
			}
			continue
		}
		record := uploadRecord{
			Key: result.file.key, Content: string(result.content),
			Metadata: map[string]any{
				"path":  map[string]any{"type": "string", "value": result.file.key},
				"size":  map[string]any{"type": "integer", "value": result.file.size},
				"mtime": map[string]any{"type": "timestamp", "value": result.file.mtime.UTC().Format(time.RFC3339Nano)},
			},
		}
		if err := encoder.Encode(record); err != nil {
			return err
		}
		total += result.file.size
		batch = append(batch, result.file)
	}
	return flush()
}

// readResult is one file the reader pool opened. Exactly one of err, the skip
// reasons, deferred, and content describes it.
type readResult struct {
	file      uploadFile
	content   []byte
	large     bool
	oversize  bool
	irregular bool
	deferred  bool
	// skipped is why a file that exists cannot be read, such as an archived
	// S3 object: a recursive upload goes on without it.
	skipped string
	err     error
}

// uploadReaders bounds the concurrent file reads of one chunk.
const uploadReaders = 16

// readChunk opens, measures, and reads one chunk of files concurrently.
//
// The size and the modification time come from the handle the content is read
// through, so enumeration never has to stat anything: one fstat on an open
// descriptor rides along with a read that has to happen anyway, whereas one
// lstat per name during the walk puts a syscall on the critical path of every
// file in the source before the first batch can leave.
//
// It returns the prefix of batch the content budget allowed. The caller offers
// the rest again in the next chunk.
func readChunk(batch []uploadFile) ([]readResult, int) {
	results := make([]readResult, len(batch))
	budget := newChunkBudget(len(batch))
	indexes := make(chan int)
	var readers sync.WaitGroup
	for range min(uploadReaders, len(batch)) {
		readers.Go(func() {
			for index := range indexes {
				results[index] = readOneFile(batch[index], index, budget)
			}
		})
	}
	for index := range batch {
		if budget.full() {
			break
		}
		indexes <- index
	}
	close(indexes)
	readers.Wait()
	return results, budget.consumed()
}

func readOneFile(file uploadFile, index int, budget *chunkBudget) readResult {
	if file.entry != nil {
		return readZipEntry(file, index, budget)
	}
	handle, err := os.Open(file.localPath)
	if err != nil {
		budget.reserve(index, 0)
		return readResult{file: file, err: fmt.Errorf("read %s: %w", file.localPath, err)}
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil {
		budget.reserve(index, 0)
		return readResult{file: file, err: fmt.Errorf("inspect %s: %w", file.localPath, err)}
	}
	if !info.Mode().IsRegular() {
		budget.reserve(index, 0)
		return readResult{file: file, irregular: true}
	}
	file.size, file.mtime = info.Size(), info.ModTime()
	if result, ok := admitFile(file, index, budget); !ok {
		return result
	}
	content := make([]byte, file.size)
	read, err := io.ReadFull(handle, content)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return readResult{file: file, err: fmt.Errorf("read %s: %w", file.localPath, err)}
	}
	// A file that shrank between the measurement and the read is uploaded, and
	// described, as what was actually read.
	file.size = int64(read)
	return readResult{file: file, content: content[:read]}
}

// admitFile takes the share of the batch budget a measured file needs to be
// read into the chunk. When it reports false, the result says why the file is
// not read now: it is published on its own, skipped, or offered again in the
// next chunk.
func admitFile(file uploadFile, index int, budget *chunkBudget) (readResult, bool) {
	switch {
	case file.size > maxDocumentSize:
		budget.reserve(index, 0)
		return readResult{file: file, oversize: true}, false
	case file.size > maxBatchContent:
		budget.reserve(index, 0)
		return readResult{file: file, large: true}, false
	}
	if !budget.reserve(index, file.size) {
		// The batch is full. This file, and every name the chunk was given
		// after it, is offered again in the next chunk.
		return readResult{file: file, deferred: true}, false
	}
	return readResult{}, true
}

// chunkBudget hands out the content budget of one batch in batch order. The
// first reader it refuses ends the chunk where that reader stands, so the
// files a chunk reads are always a prefix of the names it was given, whatever
// order the reads finished in.
type chunkBudget struct {
	mu       sync.Mutex
	turn     *sync.Cond
	size     int
	next     int
	ended    int
	reserved int64
}

func newChunkBudget(size int) *chunkBudget {
	budget := &chunkBudget{size: size, ended: size}
	budget.turn = sync.NewCond(&budget.mu)
	return budget
}

// reserve waits for the turn of index and then takes size bytes of the batch,
// reporting whether the batch had room. Granting in index order is what keeps
// a chunk a prefix, and it costs a reader nothing: it has already opened and
// measured its file by the time it asks.
func (b *chunkBudget) reserve(index int, size int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.next != index {
		b.turn.Wait()
	}
	granted := index < b.ended && (b.reserved == 0 || b.reserved+size <= maxBatchContent)
	if granted {
		b.reserved += size
	} else if index < b.ended {
		b.ended = index
	}
	b.next++
	b.turn.Broadcast()
	return granted
}

func (b *chunkBudget) full() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended < b.size
}

func (b *chunkBudget) consumed() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ended
}

// sendUploadJob publishes one job and returns the results of the documents
// it did not store. A retry presents the same idempotency key, which returns
// the results of an attempt that was applied and stores any document an
// interrupted attempt did not reach.
func (a *app) sendUploadJob(ctx context.Context, client *httpapi.Client, organization, index string, job uploadJob) ([]httpapi.MutationResult, error) {
	if job.data == nil {
		return a.sendLargeFile(ctx, client, organization, index, job.files[0], job.content)
	}
	idempotency := uploadID(organization, index, job.data)
	var throttledWait time.Duration
	for attempt := 0; attempt < uploadRetryAttempts; attempt++ {
		if err := a.awaitThrottle(ctx); err != nil {
			return nil, err
		}
		response, err := client.PutObjects(ctx, httpapi.PutObjectsReq{Data: bytes.NewReader(job.data)}, httpapi.PutObjectsParams{
			Organization: httpapi.Name(organization), Index: httpapi.IndexName(index), IdempotencyKey: idempotency,
		})
		if err == nil {
			switch result := response.(type) {
			case *httpapi.MutationBatchResult:
				return batchFailures(result.Results), nil
			case *httpapi.ConflictHeaders:
				return nil, fmt.Errorf("upload conflict: %s", result.Response.Error)
			default:
				return nil, fmt.Errorf("unexpected upload response %T", response)
			}
		}
		// A 429 asks the whole client to slow down; it never consumes one of
		// the ordinary attempts.
		if again, throttleErr := a.throttled(ctx, err, &throttledWait); throttleErr != nil {
			return nil, fmt.Errorf("upload batch: %w", throttleErr)
		} else if again {
			attempt--
			continue
		}
		retry, public := classifyUploadError(err)
		if !retry {
			return nil, public
		}
		err = public
		if attempt == uploadRetryAttempts-1 {
			return nil, fmt.Errorf("upload batch: %w", err)
		}
		if sleepErr := a.sleep(ctx, min(time.Second<<attempt, 10*time.Second)); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, nil
}

// sendLargeFile publishes one file on its own. content is the file when it was
// already read; otherwise every attempt reads it again from its source. A file
// the index refused is returned as a failed result, as a batch reports one.
func (a *app) sendLargeFile(ctx context.Context, client *httpapi.Client, organization, index string, file uploadFile, content []byte) ([]httpapi.MutationResult, error) {
	values := map[string]any{
		"path":  map[string]any{"type": "string", "value": file.key},
		"size":  map[string]any{"type": "integer", "value": file.size},
		"mtime": map[string]any{"type": "timestamp", "value": file.mtime.UTC().Format(time.RFC3339Nano)},
	}
	metadata, _ := json.Marshal(values)
	idempotency := uploadID(organization, index, []byte(file.key+fmt.Sprint(file.size, file.mtime.UnixNano())))
	var throttledWait time.Duration
	for attempt := 0; attempt < uploadRetryAttempts; attempt++ {
		if err := a.awaitThrottle(ctx); err != nil {
			return nil, err
		}
		handle, err := openUploadFile(ctx, file, content)
		if err != nil {
			return nil, err
		}
		response, callErr := client.PutDocument(ctx, &httpapi.PutDocumentFormMultipart{
			Key: file.key, Metadata: httpapi.NewOptString(string(metadata)),
			Content: ogenhttp.MultipartFile{Name: path.Base(file.key), File: handle, Size: file.size},
		}, httpapi.PutDocumentParams{
			Organization: httpapi.Name(organization), Index: httpapi.IndexName(index), IdempotencyKey: idempotency,
		})
		_ = handle.Close()
		if callErr == nil {
			switch result := response.(type) {
			case *httpapi.MutationResult:
				return nil, nil
			case *httpapi.ConflictHeaders:
				return nil, fmt.Errorf("upload conflict: %s", result.Response.Error)
			case *httpapi.PreconditionFailedHeaders:
				return []httpapi.MutationResult{{Key: file.key, Error: httpapi.NewOptError(result.Response)}}, nil
			default:
				return nil, fmt.Errorf("unexpected upload response %T", response)
			}
		}
		if again, throttleErr := a.throttled(ctx, callErr, &throttledWait); throttleErr != nil {
			return nil, fmt.Errorf("upload %s: %w", file.key, throttleErr)
		} else if again {
			attempt--
			continue
		}
		retry, public := classifyUploadError(callErr)
		if !retry {
			return nil, public
		}
		callErr = public
		if attempt == uploadRetryAttempts-1 {
			return nil, fmt.Errorf("upload %s: %w", file.key, callErr)
		}
		if err := a.sleep(ctx, min(time.Second<<attempt, 10*time.Second)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// openUploadFile opens the content of a file published on its own.
func openUploadFile(ctx context.Context, file uploadFile, content []byte) (io.ReadCloser, error) {
	switch {
	case content != nil:
		return io.NopCloser(bytes.NewReader(content)), nil
	case file.object != nil:
		return file.object.open(ctx)
	case file.entry != nil:
		return file.entry.Open()
	}
	return os.Open(file.localPath)
}

func classifyUploadError(err error) (bool, error) {
	if response, ok := errors.AsType[*httpapi.ErrorStatusCodeWithHeaders](err); ok {
		return response.StatusCode >= 500, publicAPIError(response)
	}
	return true, err
}

func uploadID(organization, index string, data []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(organization))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(index))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}
