package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBatchCompressorCompressesAndFallsBack(t *testing.T) {
	t.Parallel()

	body := []byte(strings.Repeat(`{"key":"a","content":"text text text"}`+"\n", 200))
	var mode atomic.Int32
	cases := []struct {
		name     string
		mode     compressionMode
		host     string
		refuse   bool
		wantGzip bool
	}{
		{"auto remote", compressAuto, "api.example.com", false, true},
		{"auto loopback", compressAuto, "127.0.0.1", false, false},
		{"auto localhost", compressAuto, "localhost", false, false},
		{"always loopback", compressAlways, "127.0.0.1", false, true},
		{"never remote", compressNever, "api.example.com", false, false},
		{"refused", compressAlways, "api.example.com", true, false},
	}
	for _, test := range cases {
		mode.Store(int32(test.mode))
		var sawGzip, sawPlain int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			received, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-Encoding") == "gzip" {
				sawGzip++
				if test.refuse {
					w.WriteHeader(http.StatusUnsupportedMediaType)
					return
				}
				reader, err := gzip.NewReader(bytes.NewReader(received))
				if err != nil {
					t.Errorf("%s: %v", test.name, err)
					return
				}
				received, _ = io.ReadAll(reader)
			} else {
				sawPlain++
			}
			if r.ContentLength < 0 {
				t.Errorf("%s: body sent without a length", test.name)
			}
			if !bytes.Equal(received, body) {
				t.Errorf("%s: server received a different body", test.name)
			}
		}))
		compressor := &batchCompressor{base: rewriteHost{base: http.DefaultTransport, target: server.URL}, mode: &mode}
		request, _ := http.NewRequest(http.MethodPost, "http://"+test.host+"/v1/organizations/o/indexes/i/objects:batch", io.NopCloser(bytes.NewReader(body)))
		response, err := compressor.RoundTrip(request)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", test.name, response.StatusCode)
		}
		if (sawGzip > 0) != (test.wantGzip || test.refuse) || (test.refuse && sawPlain != 1) {
			t.Fatalf("%s: gzip requests %d, plain requests %d", test.name, sawGzip, sawPlain)
		}
		server.Close()
	}
}

// rewriteHost sends every request to target while leaving the request's own
// URL, which decides compression, untouched.
type rewriteHost struct {
	base   http.RoundTripper
	target string
}

func (r rewriteHost) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.target, "http://")
	return r.base.RoundTrip(clone)
}
