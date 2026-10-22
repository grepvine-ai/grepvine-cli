package main

import (
	"archive/zip"
	"bytes"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type zipTestEntry struct {
	name    string
	content []byte
	method  uint16
	mode    os.FileMode
}

func writeTestZip(t *testing.T, entries []zipTestEntry) string {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	modified := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: entry.method, Modified: modified}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		w, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "corpus.zip")
	if err := os.WriteFile(name, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}

// An archive uploads as the tree it would extract to: every file under its
// path in the archive and the remote prefix, text in batches and binary or
// large files on their own, with the entry's own size and modification time.
// Directories and macOS resource forks are not files; links, names that
// escape the tree, and the earlier copies of a repeated path are skipped
// with a warning. Two uploads of the same archive send the same requests.
func TestUploadZipPublishesTheArchivedTree(t *testing.T) {
	entries := []zipTestEntry{
		{name: "docs/", mode: os.ModeDir | 0o755},
		{name: "docs/readme.md", content: []byte("first copy"), method: zip.Deflate},
		{name: "docs/stored.txt", content: []byte("stored <&> as is"), method: zip.Store},
		{name: "__MACOSX/docs/._readme.md", content: []byte{0, 5, 22, 7}},
		{name: "docs/link", content: []byte("readme.md"), mode: os.ModeSymlink | 0o777},
		{name: "../escape.txt", content: []byte("outside")},
		{name: "images/logo.gif", content: []byte{'G', 'I', 'F', 0xff, 0x00}, method: zip.Deflate},
		{name: "data/large.log", content: bytes.Repeat([]byte("a large line of text\n"), maxBatchContent/20), method: zip.Deflate},
	}
	for i := range 3000 {
		entries = append(entries, zipTestEntry{name: fmt.Sprintf("src/%04d.go", i), content: fmt.Appendf(nil, "package p // %d\n", i), method: zip.Deflate})
	}
	entries = append(entries, zipTestEntry{name: "docs/readme.md", content: []byte("# last copy\n"), method: zip.Deflate})
	archive := writeTestZip(t, entries)

	recorder, server := newUploadRecorder(t)
	a, stderr := s3TestApp(t, server.URL+"/v1", nil)
	if err := a.uploadZip(t.Context(), "acme", "corpus", archive, "release"); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batched) != 3002 {
		t.Fatalf("batched %d documents, want 3002", len(recorder.batched))
	}
	readme := recorder.batched["release/docs/readme.md"]
	if readme.Content != "# last copy\n" {
		t.Fatalf("readme = %q", readme.Content)
	}
	size, _ := readme.Metadata["size"].(map[string]any)
	mtime, _ := readme.Metadata["mtime"].(map[string]any)
	if size["value"] != float64(12) || mtime["value"] != "2026-03-04T05:06:07Z" {
		t.Fatalf("readme metadata = %#v", readme.Metadata)
	}
	if got := recorder.batched["release/docs/stored.txt"].Content; got != "stored <&> as is" {
		t.Fatalf("stored.txt = %q", got)
	}
	if got := recorder.batched["release/src/2999.go"].Content; got != "package p // 2999\n" {
		t.Fatalf("src/2999.go = %q", got)
	}
	if got := recorder.single["release/images/logo.gif"]; !bytes.Equal(got, entries[6].content) {
		t.Fatalf("logo.gif = %v", got)
	}
	if got := recorder.single["release/data/large.log"]; !bytes.Equal(got, entries[7].content) {
		t.Fatalf("large.log has %d bytes, want %d", len(got), len(entries[7].content))
	}
	if len(recorder.single) != 2 {
		t.Fatalf("published %d documents on their own, want 2", len(recorder.single))
	}
	for _, want := range []string{
		"skipping " + archive + ":docs/link: not a regular file",
		"skipping " + archive + ":../escape.txt: path ",
		archive + ":docs/readme.md appears more than once",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr %q lacks %q", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "MACOSX") {
		t.Fatalf("warned about a resource fork: %q", stderr.String())
	}

	keys := append([]string(nil), recorder.idempotency...)
	recorder.idempotency = nil
	if err := a.uploadZip(t.Context(), "acme", "corpus", archive, "release"); err != nil {
		t.Fatal(err)
	}
	again := append([]string(nil), recorder.idempotency...)
	sort.Strings(keys)
	sort.Strings(again)
	if len(keys) != 3 || fmt.Sprint(keys) != fmt.Sprint(again) {
		t.Fatalf("idempotency keys %v, then %v", keys, again)
	}
}

// An entry whose content does not match its checksum stops the upload
// rather than publishing what was read.
func TestUploadZipRefusesACorruptEntry(t *testing.T) {
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	content := []byte("corrupted text")
	w, err := writer.CreateRaw(&zip.FileHeader{
		Name: "bad.txt", Method: zip.Store, CRC32: crc32.ChecksumIEEE(content) + 1,
		CompressedSize64: uint64(len(content)), UncompressedSize64: uint64(len(content)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "bad.zip")
	if err := os.WriteFile(archive, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	recorder, server := newUploadRecorder(t)
	a, _ := s3TestApp(t, server.URL+"/v1", nil)
	err = a.uploadZip(t.Context(), "acme", "corpus", archive, "")
	if err == nil || !strings.Contains(err.Error(), "bad.zip:bad.txt") || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("err = %v", err)
	}
	if len(recorder.batched) != 0 {
		t.Fatalf("published %v", recorder.batched)
	}
}

func TestUploadZipFlagValidation(t *testing.T) {
	archive := writeTestZip(t, []zipTestEntry{{name: "a.txt", content: []byte("a")}})
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"upload", "--zip", "s3://bucket/corpus.zip", "--index", "acme/corpus"}, want: "download it first"},
		{args: []string{"upload", "--zip", "-r", filepath.Dir(archive), "--index", "acme/corpus"}, want: "needs a zip archive"},
	} {
		a, _, _ := testApp(t, "http://127.0.0.1:1/v1")
		command := newRootCommand(a)
		command.SetArgs(test.args)
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		if err := command.Execute(); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("%v: err = %v, want %q", test.args, err, test.want)
		}
	}
}

// With --records, an archive's record files are imported as the documents
// they hold, gzip-compressed or not, under the remote prefix; other files in
// it are left alone with a warning. Two imports send the same requests.
func TestUploadZipRecordsImportsTheArchivedRecordFiles(t *testing.T) {
	archive := writeTestZip(t, []zipTestEntry{
		{name: "parts/", mode: os.ModeDir | 0o755},
		{name: "parts/part-00000.ndjson.gz", content: gzipped(t, recordLines(t, 0, 3000, 10)), method: zip.Store},
		{name: "parts/part-00001.jsonl", content: recordLines(t, 3000, 20, 1<<20), method: zip.Deflate},
		{name: "manifest.json", content: []byte(`{"documents": 3020}`), method: zip.Deflate},
	})
	recorder, server := newUploadRecorder(t)
	a, stderr := s3TestApp(t, server.URL+"/v1", nil)
	command := newRootCommand(a)
	command.SetArgs([]string{"upload", "--zip", "--records", archive, "imported", "--index", "acme/corpus"})
	if err := command.Execute(); err != nil {
		t.Fatal(err)
	}
	if len(recorder.batched) != 3020 {
		t.Fatalf("stored %d documents, want 3020", len(recorder.batched))
	}
	if got := recorder.batched["imported/doc/000007"].Content; got != "000007 <&> xxxxxxxxxx" {
		t.Fatalf("imported/doc/000007 = %q", got)
	}
	if large := recorder.batched["imported/doc/003019"]; len(large.Content) != 1<<20+11 {
		t.Fatalf("imported/doc/003019 has %d bytes", len(large.Content))
	}
	sizes := make([]int, len(recorder.batches))
	for i, batch := range recorder.batches {
		sizes[i] = len(batch)
	}
	sort.Ints(sizes)
	if want := []int{5, 15, 952, 2048}; fmt.Sprint(sizes) != fmt.Sprint(want) {
		t.Fatalf("batch sizes = %v, want %v", sizes, want)
	}
	if want := archive + ":manifest.json: not an .ndjson or .jsonl file"; !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr %q lacks %q", stderr.String(), want)
	}

	keys := append([]string(nil), recorder.idempotency...)
	recorder.idempotency = nil
	if err := a.uploadRecords(t.Context(), "acme", "corpus", archive, "imported", false, true); err != nil {
		t.Fatal(err)
	}
	again := append([]string(nil), recorder.idempotency...)
	sort.Strings(keys)
	sort.Strings(again)
	if len(keys) != 4 || fmt.Sprint(keys) != fmt.Sprint(again) {
		t.Fatalf("idempotency keys %v, then %v", keys, again)
	}
}
