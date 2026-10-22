package main

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/grepvine-ai/grepvine-cli/internal/httpapi"
)

const (
	// defaultRetryAfter applies when a 429 carries no usable Retry-After.
	defaultRetryAfter = time.Second
	// maxRetryAfter bounds how long one server hint can pause the client.
	maxRetryAfter = time.Minute
	// maxThrottledWait bounds the total time one request spends honoring
	// Retry-After before it is reported as a failure.
	maxThrottledWait = 10 * time.Minute
)

// throttle is the process-wide pause every request honors after the server
// answered 429 with Retry-After. One request being told to slow down slows
// down every concurrent worker, so a busy uploader backs off as a whole
// instead of each worker hammering the same index.
type throttle struct {
	mu    sync.Mutex
	until time.Time
}

// slow extends the pause to at least now+delay and reports whether this call
// started a new pause, so the caller prints one notice per episode.
func (t *throttle) slow(now time.Time, delay time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	target := now.Add(delay)
	fresh := !t.until.After(now)
	if target.After(t.until) {
		t.until = target
	}
	return fresh
}

// remaining returns how long the pause still lasts.
func (t *throttle) remaining(now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.until.After(now) {
		return 0
	}
	return t.until.Sub(now)
}

// retryAfterJitter spreads retries of clients that were told to wait the
// same interval, so they do not return in one burst.
const retryAfterJitter = 0.25

// retryAfter extracts the server's interval from a 429 response and applies
// a random adjustment of up to retryAfterJitter in either direction.
func retryAfter(response *httpapi.ErrorStatusCodeWithHeaders) (time.Duration, bool) {
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	delay := defaultRetryAfter
	if seconds, ok := response.RetryAfter.Get(); ok && seconds > 0 {
		delay = time.Duration(seconds) * time.Second
	}
	delay = min(delay, maxRetryAfter)
	jitter := time.Duration((rand.Float64()*2 - 1) * retryAfterJitter * float64(delay))
	return delay + jitter, true
}

// slowDown records a 429 and reports it once per pause episode.
//
// A command drawing a progress line reports the pause on that line instead: a
// busy uploader is told to slow down once per deferred batch, and a notice for
// each one would scroll the line away and say nothing the falling rate does
// not already say.
func (a *app) slowDown(response *httpapi.ErrorStatusCodeWithHeaders, delay time.Duration) {
	now := time.Now()
	fresh := a.throttle.slow(now, delay)
	if status := a.status.Load(); status != nil && status.reportPause(now.Add(delay)) {
		return
	}
	if fresh {
		fmt.Fprintf(a.stderr, "gv: slowing down: %s; retrying in %s\n", response.Response.Error, delay)
	}
}

// awaitThrottle waits out the current pause before a request is sent.
func (a *app) awaitThrottle(ctx context.Context) error {
	remaining := a.throttle.remaining(time.Now())
	if remaining <= 0 {
		return nil
	}
	return a.sleep(ctx, remaining)
}

// throttled handles one failed API call. It reports whether the caller should
// try again: a 429 is retried after the server's interval without consuming an
// ordinary attempt; other outcomes are left to the caller's retry policy.
func (a *app) throttled(ctx context.Context, err error, waited *time.Duration) (bool, error) {
	var response *httpapi.ErrorStatusCodeWithHeaders
	if !errors.As(err, &response) {
		return false, nil
	}
	delay, ok := retryAfter(response)
	if !ok {
		return false, nil
	}
	if *waited+delay > maxThrottledWait {
		return false, fmt.Errorf("%w (slowed down for %s without success)", publicAPIError(response), waited.Round(time.Second))
	}
	*waited += delay
	a.slowDown(response, delay)
	if err := a.sleep(ctx, delay); err != nil {
		return false, err
	}
	return true, nil
}
