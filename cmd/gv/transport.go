package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// compressionMode is the --compress setting of an upload.
type compressionMode int32

const (
	// compressAuto compresses batch bodies except to a loopback endpoint,
	// where compressing costs CPU and saves no transfer.
	compressAuto compressionMode = iota
	compressAlways
	compressNever
)

func parseCompression(value string) (compressionMode, error) {
	switch value {
	case "", "auto":
		return compressAuto, nil
	case "always":
		return compressAlways, nil
	case "never":
		return compressNever, nil
	}
	return compressAuto, fmt.Errorf("--compress must be auto, always, or never")
}

// newHTTPClient returns the client every command uses. Its transport keeps a
// connection per concurrent upload open, where the default keeps two and
// redials the rest on every batch.
func newHTTPClient(mode *atomic.Int32) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 128
	return &http.Client{Timeout: 35 * time.Second, Transport: &batchCompressor{base: transport, mode: mode}}
}

// batchCompressor sends batch bodies with Content-Encoding: gzip. It
// works below the API client, after the idempotency key was taken from the
// uncompressed body, so a batch keeps its key whether or not it is
// compressed. A server that answers 415 gets the body uncompressed, then and
// for the rest of the run.
type batchCompressor struct {
	base        http.RoundTripper
	mode        *atomic.Int32
	unsupported atomic.Bool
}

var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return writer
}}

func (c *batchCompressor) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/objects:batch") || request.Body == nil {
		return c.base.RoundTrip(request)
	}
	body, err := io.ReadAll(request.Body)
	_ = request.Body.Close()
	if err != nil {
		return nil, err
	}
	if !c.compress(request) {
		return c.base.RoundTrip(withBody(request, body, ""))
	}
	var compressed bytes.Buffer
	compressed.Grow(len(body) / 3)
	writer := gzipWriters.Get().(*gzip.Writer)
	writer.Reset(&compressed)
	_, writeErr := writer.Write(body)
	closeErr := writer.Close()
	gzipWriters.Put(writer)
	if writeErr != nil || closeErr != nil {
		return c.base.RoundTrip(withBody(request, body, ""))
	}
	response, err := c.base.RoundTrip(withBody(request, compressed.Bytes(), "gzip"))
	if err == nil && response.StatusCode == http.StatusUnsupportedMediaType {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		c.unsupported.Store(true)
		return c.base.RoundTrip(withBody(request, body, ""))
	}
	return response, err
}

func (c *batchCompressor) compress(request *http.Request) bool {
	if c.unsupported.Load() || c.mode == nil {
		return false
	}
	switch compressionMode(c.mode.Load()) {
	case compressAlways:
		return true
	case compressNever:
		return false
	}
	host := request.URL.Hostname()
	if host == "localhost" {
		return false
	}
	address := net.ParseIP(host)
	return address == nil || !address.IsLoopback()
}

// withBody clones request with body, sized so it is not sent chunked.
func withBody(request *http.Request, body []byte, encoding string) *http.Request {
	clone := request.Clone(request.Context())
	clone.Body = io.NopCloser(bytes.NewReader(body))
	clone.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	clone.ContentLength = int64(len(body))
	clone.Header = request.Header.Clone()
	clone.Header.Del("Content-Length")
	if encoding != "" {
		clone.Header.Set("Content-Encoding", encoding)
	} else {
		clone.Header.Del("Content-Encoding")
	}
	return clone
}
