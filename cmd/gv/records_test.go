package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func recordLines(t *testing.T, first, count, contentSize int) []byte {
	t.Helper()
	var out bytes.Buffer
	for i := first; i < first+count; i++ {
		record := map[string]any{
			"key":     fmt.Sprintf("doc/%06d", i),
			"content": fmt.Sprintf("%06d <&> ", i) + strings.Repeat("x", contentSize),
			"metadata": map[string]any{
				"score": map[string]any{"type": "integer", "value": i},
			},
		}
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func gzipped(t *testing.T, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := gzip.NewWriter(&out)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// An import stores each record as the document it describes, with its own
// metadata, from compressed and plain files alike, and leaves other files
// alone. Each file is cut into batches of its own by the batch limits, and
// two imports of the same files send the same requests.
func TestUploadRecordsFromDirectory(t *testing.T) {
	root := t.TempDir()
	files := map[string][]byte{
		"part-00000.ndjson.gz": gzipped(t, recordLines(t, 0, 3000, 10)),
		"part-00001.jsonl":     recordLines(t, 3000, 20, 1<<20),
		"empty.ndjson":         nil,
		"manifest.json":        []byte(`{"documents": 3020}`),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recorder, server := newUploadRecorder(t)
	a, stderr := s3TestApp(t, server.URL+"/v1", nil)
	if err := a.uploadRecords(t.Context(), "acme", "corpus", root, "", true, false); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batched) != 3020 {
		t.Fatalf("stored %d documents, want 3020", len(recorder.batched))
	}
	first := recorder.batched["doc/000007"]
	score, _ := first.Metadata["score"].(map[string]any)
	if first.Content != "000007 <&> xxxxxxxxxx" || score["value"] != float64(7) {
		t.Fatalf("doc/000007 = %#v", first)
	}
	if large := recorder.batched["doc/003019"]; len(large.Content) != 1<<20+11 {
		t.Fatalf("doc/003019 has %d bytes", len(large.Content))
	}
	sizes := make([]int, len(recorder.batches))
	for i, batch := range recorder.batches {
		sizes[i] = len(batch)
		for _, key := range batch {
			if key < "doc/003000" != (batch[0] < "doc/003000") {
				t.Fatalf("a batch spans two files: %v", batch)
			}
		}
	}
	sort.Ints(sizes)
	// 3,000 small records cut at 2,048; twenty 1 MiB records cut at the
	// 16 MiB content limit.
	if want := []int{5, 15, 952, 2048}; fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
	if !strings.Contains(stderr.String(), "manifest.json: not an .ndjson or .jsonl file") {
		t.Fatalf("no warning for the manifest: %q", stderr.String())
	}

	keys := append([]string(nil), recorder.idempotency...)
	recorder.idempotency = nil
	if err := a.uploadRecords(t.Context(), "acme", "corpus", root, "", true, false); err != nil {
		t.Fatal(err)
	}
	again := append([]string(nil), recorder.idempotency...)
	sort.Strings(keys)
	sort.Strings(again)
	if fmt.Sprint(keys) != fmt.Sprint(again) {
		t.Fatal("a second import of the same files sent different requests")
	}
}

// A remote path prefixes every key; a record too large for a batch is
// reported and left out; a line that is not a record fails the import.
func TestUploadRecordsKeysAndLimits(t *testing.T) {
	root := t.TempDir()
	large, _ := json.Marshal(map[string]any{"key": "huge", "content": strings.Repeat("y", maxBatchContent+1)})
	content := append(recordLines(t, 0, 2, 1), large...)
	if err := os.WriteFile(filepath.Join(root, "docs.ndjson"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, server := newUploadRecorder(t)
	a, stderr := s3TestApp(t, server.URL+"/v1", nil)
	if err := a.uploadRecords(t.Context(), "acme", "corpus", filepath.Join(root, "docs.ndjson"), "imported", false, false); err != nil {
		t.Fatal(err)
	}
	record, ok := recorder.batched["imported/doc/000001"]
	if !ok || len(recorder.batched) != 2 || record.Metadata["score"] == nil {
		t.Fatalf("stored %#v", recorder.batched)
	}
	if !strings.Contains(stderr.String(), "docs.ndjson line 3: huge exceeds the 16 MiB a batch holds") {
		t.Fatalf("no warning for the large record: %q", stderr.String())
	}

	if err := os.WriteFile(filepath.Join(root, "bad.ndjson"), []byte("{\"key\":\"a\",\"content\":\"a\"}\n{\"content\":\"no key\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := a.uploadRecords(t.Context(), "acme", "corpus", filepath.Join(root, "bad.ndjson"), "", false, false)
	if err == nil || !strings.Contains(err.Error(), "bad.ndjson line 2: not a document record") {
		t.Fatalf("import of an invalid record = %v", err)
	}
}

// Records import from an S3 prefix as from a directory.
func TestUploadRecordsFromS3(t *testing.T) {
	bucket := newFakeS3(map[string][]byte{
		"corpus/r1/manifest.json":        []byte(`{}`),
		"corpus/r1/part-00000.ndjson.gz": gzipped(t, recordLines(t, 0, 5, 3)),
		"corpus/r1/part-00001.ndjson.gz": gzipped(t, recordLines(t, 5, 5, 3)),
	})
	recorder, server := newUploadRecorder(t)
	a, _ := s3TestApp(t, server.URL+"/v1", bucket)
	if err := a.uploadRecords(t.Context(), "acme", "corpus", "s3://b/corpus/r1/", "", true, false); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batched) != 10 || len(recorder.batches) != 2 || bucket.gets["corpus/r1/manifest.json"] != 0 {
		t.Fatalf("stored %d documents in %d batches; gets %v", len(recorder.batched), len(recorder.batches), bucket.gets)
	}
	if err := a.uploadRecords(t.Context(), "acme", "corpus", "s3://b/corpus/r1", "", false, false); err == nil || !strings.Contains(err.Error(), "use --recursive") {
		t.Fatalf("import of a prefix without --recursive = %v", err)
	}
}
