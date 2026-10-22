package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

func TestUploadSurvivesBriefConnectionRefusalWithSameRequest(t *testing.T) {
	for _, recover := range []bool{true, false} {
		t.Run(map[bool]string{true: "recovers", false: "bounded_failure"}[recover], func(t *testing.T) {
			attempts := 0
			var id, body string
			transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
				attempts++
				encoded, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				_ = request.Body.Close()
				if attempts == 1 {
					id, body = request.Header.Get("Idempotency-Key"), string(encoded)
				}
				if id == "" || id != request.Header.Get("Idempotency-Key") || body != string(encoded) {
					t.Fatal("retry changed request identity or body")
				}
				if !recover || attempts < uploadRetryAttempts {
					return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"results":[{"key":"a","revision":"r1"}]}`))}, nil
			})
			client, err := httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: transport}))
			if err != nil {
				t.Fatal(err)
			}
			var waited time.Duration
			a := &app{sleep: func(_ context.Context, delay time.Duration) error {
				if delay > 10*time.Second {
					t.Fatal("unbounded retry delay")
				}
				waited += delay
				return nil
			}}
			_, err = a.sendUploadJob(t.Context(), client, "acme", "index", uploadJob{data: []byte("{\"key\":\"a\",\"content\":\"value\"}\n")})
			if (err == nil) != recover || attempts != uploadRetryAttempts || waited != 45*time.Second {
				t.Fatalf("attempts=%d waited=%s err=%v", attempts, waited, err)
			}
		})
	}
}

// TestUploadJobReturnsTheDocumentsItDidNotStore checks that a batch whose
// results report a document as not stored returns that document without
// retrying: the rest of the batch was stored.
func TestUploadJobReturnsTheDocumentsItDidNotStore(t *testing.T) {
	requests := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body := `{"results":[{"key":"a","revision":"r1"},{"key":"b","error":{"code":"precondition_failed","reason":"precondition","error":"the document's precondition failed"}}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client, err := httpapi.NewClient("http://api.test/v1", credentials{key: "test-key"}, httpapi.WithClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	a := &app{sleep: func(context.Context, time.Duration) error { return nil }}
	job := uploadJob{
		files: []uploadFile{{key: "a", size: 1}, {key: "b", size: 1}},
		data:  []byte("{\"key\":\"a\",\"content\":\"1\"}\n{\"key\":\"b\",\"content\":\"2\"}\n"), bytes: 2,
	}
	failed, err := a.sendUploadJob(t.Context(), client, "acme", "index", job)
	if err != nil || requests != 1 || len(failed) != 1 || failed[0].Key != "b" || failed[0].Error.Value.Code != "precondition_failed" {
		t.Fatalf("failed=%+v err=%v requests=%d", failed, err, requests)
	}
}

// TestUploadReportsEveryDocumentNotStoredAndFails uploads a directory into an
// index that refuses two of its documents. The other documents are stored,
// each refused one is reported by key, and the command fails at the end.
func TestUploadReportsEveryDocumentNotStoredAndFails(t *testing.T) {
	dir := plainDir(t)
	writeFiles(t, dir, map[string]string{"a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n", "d.txt": "d\n"})
	stack := newSyncStack()
	stack.refuse = map[string]bool{"b.txt": true, "d.txt": true}
	a, stdout, stderr := startStack(t, stack)
	err := runGv(a, stdout, stderr, "upload", "--recursive", dir, "--index", "acme/docs")
	if err == nil || err.Error() != "2 documents were not uploaded" {
		t.Fatalf("err = %v", err)
	}
	for _, key := range []string{"b.txt", "d.txt"} {
		if !strings.Contains(stderr.String(), "gv: "+key+" was not uploaded: the document's precondition failed (precondition_failed)\n") {
			t.Fatalf("stderr = %q", stderr.String())
		}
	}
	if got := sortedKeys(stack.objects); strings.Join(got, ",") != "a.txt,c.txt" {
		t.Fatalf("objects = %v", got)
	}
}
