package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// fakeS3 is an in-memory bucket with the listing semantics an upload relies
// on: "/" as the delimiter, common prefixes rolled up, and pages of a few
// keys so every walk crosses page boundaries. A directory bucket lists in a
// scrambled but stable order, as S3 Express does.
type fakeS3 struct {
	mu        sync.Mutex
	unordered bool
	pageSize  int
	objects   map[string]fakeObject
	gets      map[string]int
	lists     int
	// beforeGet runs before every GET, outside the lock.
	beforeGet func(key string)
}

type fakeObject struct {
	content  []byte
	mtime    time.Time
	class    types.ObjectStorageClass
	restored bool
}

func newFakeS3(objects map[string][]byte) *fakeS3 {
	fake := &fakeS3{pageSize: 3, objects: map[string]fakeObject{}, gets: map[string]int{}}
	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for key, content := range objects {
		fake.objects[key] = fakeObject{content: content, mtime: base.Add(time.Duration(len(key)) * time.Minute)}
	}
	return fake
}

func fakeETag(content []byte) string {
	sum := sha256.Sum256(content)
	return fmt.Sprintf(`"%x"`, sum[:8])
}

func fakeAPIError(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

func (f *fakeS3) ListObjectsV2(_ context.Context, input *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	prefix, delimiter := aws.ToString(input.Prefix), aws.ToString(input.Delimiter)
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	type entry struct {
		key    string
		prefix bool
	}
	var entries []entry
	for _, key := range keys {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if delimiter != "" {
			if at := strings.Index(key[len(prefix):], delimiter); at >= 0 {
				common := key[:len(prefix)+at+len(delimiter)]
				if len(entries) == 0 || entries[len(entries)-1].key != common {
					entries = append(entries, entry{key: common, prefix: true})
				}
				continue
			}
		}
		entries = append(entries, entry{key: key})
	}
	if f.unordered {
		sort.SliceStable(entries, func(i, j int) bool {
			left, right := sha256.Sum256([]byte(entries[i].key)), sha256.Sum256([]byte(entries[j].key))
			return bytes.Compare(left[:], right[:]) < 0
		})
	}
	start := 0
	if token := aws.ToString(input.ContinuationToken); token != "" {
		start, _ = strconv.Atoi(token)
	}
	size := f.pageSize
	if input.MaxKeys != nil && int(*input.MaxKeys) < size {
		size = int(*input.MaxKeys)
	}
	end := min(start+size, len(entries))
	output := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(end < len(entries))}
	if end < len(entries) {
		output.NextContinuationToken = aws.String(strconv.Itoa(end))
	}
	for _, item := range entries[start:end] {
		if item.prefix {
			output.CommonPrefixes = append(output.CommonPrefixes, types.CommonPrefix{Prefix: aws.String(item.key)})
			continue
		}
		object := f.objects[item.key]
		listed := types.Object{
			Key: aws.String(item.key), Size: aws.Int64(int64(len(object.content))), LastModified: aws.Time(object.mtime),
			ETag: aws.String(fakeETag(object.content)), StorageClass: object.class,
		}
		if object.restored {
			listed.RestoreStatus = &types.RestoreStatus{IsRestoreInProgress: aws.Bool(false), RestoreExpiryDate: aws.Time(object.mtime.Add(time.Hour))}
		}
		output.Contents = append(output.Contents, listed)
	}
	return output, nil
}

func (f *fakeS3) HeadObject(_ context.Context, input *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[aws.ToString(input.Key)]
	if !ok {
		return nil, fakeAPIError("NotFound")
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(object.content))), LastModified: aws.Time(object.mtime), ETag: aws.String(fakeETag(object.content)),
	}, nil
}

func (f *fakeS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	key := aws.ToString(input.Key)
	if f.beforeGet != nil {
		f.beforeGet(key)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets[key]++
	object, ok := f.objects[key]
	switch {
	case !ok:
		return nil, fakeAPIError("NoSuchKey")
	case input.IfMatch != nil && *input.IfMatch != fakeETag(object.content):
		return nil, fakeAPIError("PreconditionFailed")
	case (object.class == types.ObjectStorageClassGlacier || object.class == types.ObjectStorageClassDeepArchive) && !object.restored:
		return nil, fakeAPIError("InvalidObjectState")
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(object.content)), ContentLength: aws.Int64(int64(len(object.content)))}, nil
}

// uploadRecorder is a document service that records what each request
// stored: batch records by key, and documents sent on their own by key.
type uploadRecorder struct {
	mu           sync.Mutex
	batches      [][]string
	idempotency  []string
	batched      map[string]uploadRecord
	single       map[string][]byte
	singleFields map[string]map[string]any
}

func newUploadRecorder(t *testing.T) (*uploadRecorder, *httptest.Server) {
	t.Helper()
	recorder := &uploadRecorder{batched: map[string]uploadRecord{}, single: map[string][]byte{}, singleFields: map[string]map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/objects:batch"):
			scanner := bufio.NewScanner(r.Body)
			scanner.Buffer(make([]byte, 64<<10), 64<<20)
			var keys []string
			records := map[string]uploadRecord{}
			for scanner.Scan() {
				var record uploadRecord
				if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
					t.Error(err)
				}
				keys = append(keys, record.Key)
				records[record.Key] = record
			}
			recorder.mu.Lock()
			recorder.batches = append(recorder.batches, keys)
			recorder.idempotency = append(recorder.idempotency, r.Header.Get("Idempotency-Key"))
			maps.Copy(recorder.batched, records)
			recorder.mu.Unlock()
			io.WriteString(w, `{"results":[]}`)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/objects"):
			reader, err := r.MultipartReader()
			if err != nil {
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
			recorder.mu.Lock()
			recorder.single[key] = content
			recorder.singleFields[key] = metadata
			recorder.mu.Unlock()
			io.WriteString(w, `{"key":"`+key+`","revision":"r1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return recorder, server
}

func s3TestApp(t *testing.T, endpoint string, bucket *fakeS3) (*app, *bytes.Buffer) {
	t.Helper()
	a, _, stderr := testApp(t, endpoint)
	if err := saveConfig(a.configPath, config{APIKey: "test-key"}); err != nil {
		t.Fatal(err)
	}
	a.openS3 = func(context.Context, string) (s3API, error) { return bucket, nil }
	return a, stderr
}

func TestParseS3URI(t *testing.T) {
	for _, test := range []struct {
		source string
		want   s3Location
		remote bool
		fails  bool
	}{
		{source: "docs", remote: false},
		{source: "/tmp/s3://x", remote: false},
		{source: "s3://bucket", want: s3Location{bucket: "bucket"}, remote: true},
		{source: "s3://bucket/", want: s3Location{bucket: "bucket"}, remote: true},
		{source: "s3://bucket/a/b?c#d%20e", want: s3Location{bucket: "bucket", key: "a/b?c#d%20e"}, remote: true},
		{source: "s3://", remote: true, fails: true},
		{source: "s3:///key", remote: true, fails: true},
	} {
		location, remote, err := parseS3URI(test.source)
		if remote != test.remote || (err != nil) != test.fails || err == nil && location != test.want {
			t.Errorf("parseS3URI(%q) = %+v, %v, %v", test.source, location, remote, err)
		}
	}
}

// A source is one object or a directory by the same rules as a local path:
// a directory needs --recursive, and a plain upload of a name that is both
// an object and a prefix uploads the object.
func TestResolveS3Source(t *testing.T) {
	bucket := newFakeS3(map[string][]byte{"notes.txt": []byte("n"), "docs/a.txt": []byte("a"), "both": []byte("b"), "both/inner.txt": []byte("i")})
	for _, test := range []struct {
		key       string
		recursive bool
		object    string
		prefix    string
		err       string
	}{
		{key: "", recursive: false, err: "use --recursive"},
		{key: "", recursive: true, prefix: ""},
		{key: "docs", recursive: false, err: "use --recursive"},
		{key: "docs/", recursive: false, err: "use --recursive"},
		{key: "docs", recursive: true, prefix: "docs/"},
		{key: "docs/", recursive: true, prefix: "docs/"},
		{key: "notes.txt", recursive: false, object: "notes.txt"},
		{key: "notes.txt", recursive: true, object: "notes.txt"},
		{key: "both", recursive: false, object: "both"},
		{key: "both", recursive: true, prefix: "both/"},
		{key: "missing", recursive: false, err: "does not exist"},
		{key: "missing", recursive: true, err: "does not exist"},
	} {
		source, err := resolveS3Source(t.Context(), bucket, s3Location{bucket: "b", key: test.key}, test.recursive)
		switch {
		case test.err != "":
			if err == nil || !strings.Contains(err.Error(), test.err) {
				t.Errorf("resolve %q recursive=%v: error %v, want %q", test.key, test.recursive, err, test.err)
			}
		case err != nil:
			t.Errorf("resolve %q recursive=%v: %v", test.key, test.recursive, err)
		case test.object != "":
			if source.object == nil || source.object.key != test.object || source.size != 1 {
				t.Errorf("resolve %q recursive=%v = %+v, want object %q", test.key, test.recursive, source, test.object)
			}
		case source.object != nil || source.prefix != test.prefix:
			t.Errorf("resolve %q recursive=%v = %+v, want prefix %q", test.key, test.recursive, source, test.prefix)
		}
	}
}

// The walk visits keys in the order of one flat listing, even though it
// lists prefixes separately and concurrently, and a directory bucket, which
// lists in no order, is walked in the same order.
func TestS3WalkOrder(t *testing.T) {
	names := []string{"a-b", "a/x", "a/y/z", "a0", "b", "c/d/e/f", "c/d/g", "c/d0", "c/e", "z"}
	for i := range 40 {
		names = append(names, fmt.Sprintf("wide/%02d/file", i))
	}
	objects := map[string][]byte{}
	for _, name := range names {
		objects["root/"+name] = []byte(name)
	}
	objects["rootless"] = []byte("outside the prefix")
	sort.Strings(names)
	for _, unordered := range []bool{false, true} {
		bucket := newFakeS3(objects)
		bucket.unordered = unordered
		source := &s3Source{client: bucket, bucket: "b", prefix: "root/", unordered: unordered}
		var keys []string
		total, err := source.walk(t.Context(), "", func(file uploadFile) error {
			keys = append(keys, file.key)
			return nil
		}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(keys, names) || total != int64(len(names)) {
			t.Fatalf("unordered=%v: walk = %d %q, want %q", unordered, total, keys, names)
		}
		counted, err := source.walk(t.Context(), "", nil, nil, nil)
		if err != nil || counted != total {
			t.Fatalf("unordered=%v: count = %d, %v, want %d", unordered, counted, err, total)
		}
	}
}

// An S3 prefix uploads as a directory does: text in batches, binary and
// large objects on their own, each read once, with the same metadata, under
// the remote prefix. Objects a directory cannot hold, and archived objects,
// are reported and left out.
func TestUploadFromS3Prefix(t *testing.T) {
	gif := []byte{'G', 'I', 'F', '8', '9', 'a', 0xff, 0x00, 0xfe}
	large := bytes.Repeat([]byte("large text\n"), maxBatchContent/11+2)
	bucket := newFakeS3(map[string][]byte{
		"corpus/README.md":         []byte("# readme\n"),
		"corpus/docs/":             nil,
		"corpus/docs/guide.md":     []byte("guide\n"),
		"corpus/docs/deep/a.txt":   []byte("deep a\n"),
		"corpus/images/logo.gif":   gif,
		"corpus/data/large.txt":    large,
		"corpus/.git/config":       []byte("[core]\n"),
		"corpus/odd//empty.txt":    []byte("empty component\n"),
		"corpus/odd/../escape.txt": []byte("dot-dot\n"),
		"corpus/cold/frozen.txt":   []byte("frozen\n"),
		"corpus/cold/thawed.txt":   []byte("thawed\n"),
		"elsewhere.txt":            []byte("not under the prefix\n"),
	})
	frozen := bucket.objects["corpus/cold/frozen.txt"]
	frozen.class = types.ObjectStorageClassGlacier
	bucket.objects["corpus/cold/frozen.txt"] = frozen
	thawed := bucket.objects["corpus/cold/thawed.txt"]
	thawed.class, thawed.restored = types.ObjectStorageClassDeepArchive, true
	bucket.objects["corpus/cold/thawed.txt"] = thawed

	recorder, server := newUploadRecorder(t)
	a, stderr := s3TestApp(t, server.URL+"/v1", bucket)
	if err := a.uploadS3(t.Context(), "acme", "docs", s3Location{bucket: "b", key: "corpus"}, "mirror", true); err != nil {
		t.Fatal(err)
	}

	wantBatched := map[string]string{
		"mirror/README.md":       "# readme\n",
		"mirror/docs/guide.md":   "guide\n",
		"mirror/docs/deep/a.txt": "deep a\n",
		"mirror/cold/thawed.txt": "thawed\n",
	}
	if len(recorder.batched) != len(wantBatched) {
		t.Fatalf("batched %d records, want %d: %v", len(recorder.batched), len(wantBatched), recorder.batches)
	}
	for key, content := range wantBatched {
		record := recorder.batched[key]
		object := bucket.objects["corpus/"+strings.TrimPrefix(key, "mirror/")]
		if record.Content != content {
			t.Errorf("%s content = %q, want %q", key, record.Content, content)
		}
		size, _ := record.Metadata["size"].(map[string]any)
		mtime, _ := record.Metadata["mtime"].(map[string]any)
		if size["value"] != float64(len(content)) || mtime["value"] != object.mtime.Format(time.RFC3339Nano) {
			t.Errorf("%s metadata = %#v", key, record.Metadata)
		}
	}
	if len(recorder.single) != 2 || !bytes.Equal(recorder.single["mirror/images/logo.gif"], gif) || !bytes.Equal(recorder.single["mirror/data/large.txt"], large) {
		t.Fatalf("single uploads: %d, logo %d bytes, large %d bytes", len(recorder.single), len(recorder.single["mirror/images/logo.gif"]), len(recorder.single["mirror/data/large.txt"]))
	}
	for key, count := range bucket.gets {
		if count != 1 {
			t.Errorf("%s was read %d times", key, count)
		}
	}
	if bucket.gets["corpus/.git/config"] != 0 || bucket.gets["elsewhere.txt"] != 0 {
		t.Errorf("read objects outside the upload: %v", bucket.gets)
	}
	for _, warning := range []string{"s3://b/corpus/odd//: its path has", "s3://b/corpus/odd/../: its path has", "s3://b/corpus/cold/frozen.txt: it is archived"} {
		if !strings.Contains(stderr.String(), "gv: warning: skipping "+warning) {
			t.Errorf("no warning %q in %q", warning, stderr.String())
		}
	}
}

// Batches are cut where a local upload of the same tree cuts them, and two
// uploads of an unchanged source send the same requests, so an interrupted
// upload resumes onto the batches the first attempt stored.
func TestUploadFromS3BatchesLikeALocalUpload(t *testing.T) {
	root := t.TempDir()
	objects := map[string][]byte{}
	for i := range 3000 {
		name := fmt.Sprintf("small/%04d.txt", i)
		objects[name] = []byte(strings.Repeat("x", i%97) + "\n")
	}
	for i := range 40 {
		objects[fmt.Sprintf("medium/%02d.txt", i)] = bytes.Repeat([]byte("m"), 1<<20+i)
	}
	objects["medium/binary.bin"] = []byte{0xff, 0xfe, 0x00}
	for name, content := range objects {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	keysOf := func(jobs []uploadJob) [][]string {
		var batches [][]string
		for _, job := range jobs {
			keys := make([]string, len(job.files))
			for i, file := range job.files {
				keys[i] = file.key
			}
			batches = append(batches, keys)
		}
		return batches
	}
	localJobs, _, err := makeUploadJobs(collectUploadFiles(t, root, ""), true)
	if err != nil {
		t.Fatal(err)
	}

	bucket := newFakeS3(objects)
	bucket.pageSize = 1000
	source := &s3Source{client: bucket, bucket: "b"}
	encode := func() []uploadJob {
		names := make(chan uploadFile, uploadNameBuffer)
		var walked error
		go func() {
			defer close(names)
			_, walked = source.walk(t.Context(), "", func(file uploadFile) error { names <- file; return nil }, nil, nil)
		}()
		var jobs []uploadJob
		if err := source.encodeUploads(t.Context(), names, true, func(string) {}, func(job uploadJob) error {
			jobs = append(jobs, job)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if walked != nil {
			t.Fatal(walked)
		}
		return jobs
	}
	first, second := encode(), encode()
	if got, want := keysOf(first), keysOf(localJobs); !reflect.DeepEqual(got, want) {
		t.Fatalf("S3 batches differ from local batches: %d against %d", len(got), len(want))
	}
	if len(first) < 5 {
		t.Fatalf("only %d jobs; the test no longer crosses the batch limits", len(first))
	}
	for i := range first {
		if !bytes.Equal(first[i].data, second[i].data) || !bytes.Equal(first[i].content, second[i].content) {
			t.Fatalf("job %d differs between two uploads of the same source", i)
		}
	}
}

// A single object uploads under its base name, as a single file does.
func TestUploadFromS3Object(t *testing.T) {
	bucket := newFakeS3(map[string][]byte{"logs/2026/app.log": []byte("started\n")})
	recorder, server := newUploadRecorder(t)
	a, _ := s3TestApp(t, server.URL+"/v1", bucket)
	if err := a.uploadS3(t.Context(), "acme", "logs", s3Location{bucket: "b", key: "logs/2026/app.log"}, "", false); err != nil {
		t.Fatal(err)
	}
	if record, ok := recorder.batched["app.log"]; !ok || record.Content != "started\n" || len(recorder.batched) != 1 {
		t.Fatalf("batched = %#v", recorder.batched)
	}
}

// An object replaced after it was listed fails the upload rather than
// publishing content that does not match the size and time it was listed
// with; an empty prefix is an error, as an empty directory is.
func TestUploadFromS3Failures(t *testing.T) {
	bucket := newFakeS3(map[string][]byte{"dir/a.txt": []byte("a"), "dir/b.txt": []byte("b")})
	bucket.beforeGet = func(key string) {
		if key == "dir/b.txt" {
			bucket.mu.Lock()
			object := bucket.objects[key]
			object.content = []byte("rewritten")
			bucket.objects[key] = object
			bucket.mu.Unlock()
		}
	}
	_, server := newUploadRecorder(t)
	a, _ := s3TestApp(t, server.URL+"/v1", bucket)
	err := a.uploadS3(t.Context(), "acme", "docs", s3Location{bucket: "b", key: "dir/"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "s3://b/dir/b.txt changed during the upload") {
		t.Fatalf("upload of a changing object = %v", err)
	}

	empty := newFakeS3(map[string][]byte{"dir/.git/HEAD": []byte("ref")})
	a, _ = s3TestApp(t, server.URL+"/v1", empty)
	err = a.uploadS3(t.Context(), "acme", "docs", s3Location{bucket: "b", key: "dir/"}, "", true)
	if err == nil || !strings.Contains(err.Error(), "contains no files") {
		t.Fatalf("upload of an empty prefix = %v", err)
	}
}

func TestIsDirectoryBucket(t *testing.T) {
	for bucket, want := range map[string]bool{
		"logs--use1-az4--x-s3": true,
		"logs":                 false,
		"x-s3":                 false,
		"a--x-s3-b":            false,
	} {
		if got := isDirectoryBucket(bucket); got != want {
			t.Errorf("isDirectoryBucket(%q) = %v, want %v", bucket, got, want)
		}
	}
}
