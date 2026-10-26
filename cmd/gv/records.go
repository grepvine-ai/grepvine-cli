package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

const (
	// recordReaders is how many record files an import reads at once. One
	// reader decompresses and decodes at well over a hundred megabytes a
	// second, so a few keep any index fed while leaving the processor to
	// other work.
	recordReaders = 4
	// maxRecordLine is the longest record the service reads in a batch.
	maxRecordLine = 64 << 20
)

// recordSuffixes name the files a directory or prefix of records holds.
var recordSuffixes = []string{".ndjson", ".jsonl", ".ndjson.gz", ".jsonl.gz"}

func isRecordFile(name string) bool {
	for _, suffix := range recordSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// uploadRecords imports documents from NDJSON files of records in the form a
// batch request takes, one {"key", "content", "metadata"} object per line,
// read as they are and gzip-compressed or not. source is a local path or an
// S3 location; a directory or prefix contributes its .ndjson and .jsonl
// files, compressed or not, in key order. With archive, source is a local
// zip archive, whose record files are read in archive order without being
// extracted.
//
// Each file is cut into batches of its own, so the requests an import sends
// depend only on the files and not on how their reads interleave, and an
// interrupted import resumes onto the batches it already stored.
func (a *app) uploadRecords(ctx context.Context, organization, index, source, prefix string, recursive, archive bool) error {
	prefix, err := normalizeRemotePath(prefix, true)
	if err != nil {
		return fmt.Errorf("remote prefix: %w", err)
	}
	status := a.newStatus()
	status.noun = "document"
	a.status.Store(status)
	defer a.status.Store(nil)
	status.start()
	defer status.finish()
	warn := func(message string) { status.printf("gv: warning: %s\n", message) }

	var files []uploadFile
	if archive {
		reader, info, err := openZip(source)
		if err != nil {
			return err
		}
		defer reader.Close()
		for _, file := range zipEntries(source, reader.File, "", info, warn) {
			if keepRecordFile(file, false, warn) {
				files = append(files, file)
			}
		}
	} else if files, err = a.recordFiles(ctx, source, recursive, warn); err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("source contains no .ndjson or .jsonl files")
	}
	var total int64
	for _, file := range files {
		total += file.size
	}
	status.measureSource(total)
	return a.uploadFiles(ctx, organization, index, status, true, func(ctx context.Context, emit func(uploadFile) error) error {
		for _, file := range files {
			if err := emit(file); err != nil {
				return err
			}
		}
		return nil
	}, recordEncoder(prefix))
}

// keepRecordFile reports whether an import reads file: a file named on its
// own whatever its name, and otherwise a record file, warning of the rest.
func keepRecordFile(file uploadFile, named bool, warn func(string)) bool {
	if named || isRecordFile(file.key) {
		return true
	}
	warn(fmt.Sprintf("skipping %s: not an .ndjson or .jsonl file", file.name()))
	return false
}

// recordFiles lists the record files of source with their sizes. A file named
// on its own is read whatever its name.
func (a *app) recordFiles(ctx context.Context, source string, recursive bool, warn func(string)) ([]uploadFile, error) {
	var files []uploadFile
	keep := func(file uploadFile, named bool) {
		if keepRecordFile(file, named, warn) {
			files = append(files, file)
		}
	}
	location, remote, err := parseS3URI(source)
	if err != nil {
		return nil, err
	}
	if remote {
		client, err := a.s3Client(ctx, location.bucket)
		if err != nil {
			return nil, err
		}
		s3, err := resolveS3Source(ctx, client, location, recursive)
		if err != nil {
			return nil, err
		}
		_, err = s3.walk(ctx, "", func(file uploadFile) error {
			keep(file, s3.object != nil)
			return nil
		}, warn, nil)
		return files, err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("inspect source: %w", err)
	}
	err = walkSource(ctx, source, "", func(file uploadFile) error {
		measured, err := os.Stat(file.localPath)
		if err != nil {
			return fmt.Errorf("inspect %s: %w", file.localPath, err)
		}
		file.size, file.mtime = measured.Size(), measured.ModTime()
		keep(file, !info.IsDir())
		return nil
	}, nil)
	return files, err
}

// recordEncoder returns the encodeFunc of an import: up to recordReaders
// files are read at once, and each is cut into batches of its own.
func recordEncoder(prefix string) encodeFunc {
	return func(ctx context.Context, names <-chan uploadFile, _ bool, warn func(string), consume func(uploadJob) error) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		var readers sync.WaitGroup
		var failure error
		var failed sync.Once
		for range recordReaders {
			readers.Go(func() {
				for file := range names {
					if ctx.Err() != nil {
						continue
					}
					if err := encodeRecordFile(ctx, file, prefix, warn, consume); err != nil {
						failed.Do(func() { failure = err })
						cancel()
					}
				}
			})
		}
		readers.Wait()
		if failure != nil {
			return failure
		}
		return ctx.Err()
	}
}

// countingReader counts the bytes read through it.
type countingReader struct {
	reader io.Reader
	count  int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.count += int64(n)
	return n, err
}

// recordFields are the parts of a record the importer needs: the key to name
// it and the content to measure it against the batch limit.
type recordFields struct {
	Key     string  `json:"key"`
	Content *string `json:"content"`
}

// encodeRecordFile cuts one record file into batches. Each batch carries the
// share of the file it was read from, so the progress line can measure an
// import by the source it has consumed, the one total known before it starts.
func encodeRecordFile(ctx context.Context, file uploadFile, prefix string, warn func(string), consume func(uploadJob) error) error {
	handle, err := openUploadFile(ctx, file, nil)
	if err != nil {
		return err
	}
	defer handle.Close()
	raw := &countingReader{reader: handle}
	buffered := bufio.NewReaderSize(raw, 1<<20)
	var reader io.Reader = buffered
	if magic, _ := buffered.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		decompressed, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("read %s: %w", file.name(), err)
		}
		defer decompressed.Close()
		reader = decompressed
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1<<20), maxRecordLine)

	var body bytes.Buffer
	batch := make([]uploadFile, 0, uploadBatchFiles)
	var content, consumed int64
	// A batch is cut only when the next record does not fit, so the last
	// batch of a file is never empty unless the file holds no records; that
	// file is reported consumed with a job of no documents.
	flush := func(final bool) error {
		source := raw.count - consumed
		if final {
			source = max(file.size-consumed, 0)
		}
		if len(batch) == 0 && source == 0 {
			return nil
		}
		consumed += source
		job := uploadJob{files: batch, data: append([]byte(nil), body.Bytes()...), bytes: content, source: source}
		batch, content = make([]uploadFile, 0, uploadBatchFiles), 0
		body.Reset()
		return consume(job)
	}
	line := 0
	for scanner.Scan() {
		line++
		record := bytes.TrimSpace(scanner.Bytes())
		if len(record) == 0 {
			continue
		}
		var fields recordFields
		if err := json.Unmarshal(record, &fields); err != nil || fields.Key == "" || fields.Content == nil {
			return fmt.Errorf("%s line %d: not a document record with a key and content", file.name(), line)
		}
		size := int64(len(*fields.Content))
		if size > maxBatchContent {
			warn(fmt.Sprintf("skipping %s line %d: %s exceeds the 16 MiB a batch holds", file.name(), line, fields.Key))
			continue
		}
		key := fields.Key
		if prefix != "" {
			key = prefix + "/" + fields.Key
			if record, err = withKey(record, key); err != nil {
				return fmt.Errorf("%s line %d: %w", file.name(), line, err)
			}
		}
		if len(batch) == uploadBatchFiles || len(batch) > 0 && content+size > maxBatchContent {
			if err := flush(false); err != nil {
				return err
			}
		}
		body.Write(record)
		body.WriteByte('\n')
		batch = append(batch, uploadFile{key: key, size: size})
		content += size
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read %s after line %d: %w", file.name(), line, err)
	}
	return flush(true)
}

// withKey returns record with its key replaced, every other field as it was.
func withKey(record []byte, key string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		return nil, err
	}
	fields["key"] = encoded
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(fields); err != nil {
		return nil, err
	}
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}
