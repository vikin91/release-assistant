// Package githubread shares GitHub read allowance and scheduling across adapters.
package githubread

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type (
	// Priority orders work waiting for the next GitHub request slot.
	Priority     uint8
	requestKey   struct{}
	usageKey     struct{}
	requestClass struct {
		priority Priority
		source   string
	}
	// Usage counts actual reads across discovery and planner calls in one source cycle.
	Usage struct {
		calls atomic.Int64
		limit int
	}
	// DeferredError records when a read can be attempted without exceeding an allowance.
	DeferredError struct {
		Reason string
		Until  time.Time
	}
	// Transport schedules GitHub GETs fairly within each priority and caps hourly spending.
	Transport struct {
		base      http.RoundTripper
		mu        sync.Mutex
		pending   []*http.Request
		changed   chan struct{}
		busy      bool
		last      string
		calls     []time.Time
		allowance int
		until     time.Time
		blocked   string
		now       func() time.Time
	}
	releaseBody struct {
		io.ReadCloser
		once    sync.Once
		release func()
	}
)

const (
	// Tags prioritizes component selection.
	Tags Priority = iota
	// PRs prioritizes PR inventory during scope selection.
	PRs
	// Builds prioritizes the selected build and pinning targets.
	Builds
	// Background reserves inexpensive revalidation for after active selection work.
	Background
)

// Error preserves the scheduling reason and the earliest useful retry time.
func (e *DeferredError) Error() string {
	return fmt.Sprintf("%s; retry after %s", e.Reason, e.Until.UTC().Format(time.RFC3339))
}

// Calls excludes cached, deferred, and non-GitHub observations.
func (u *Usage) Calls() int64 { return u.calls.Load() }

// Track shares a source-cycle cap across all HTTP clients using the returned context.
func Track(ctx context.Context, limit int) (context.Context, *Usage) {
	u := &Usage{limit: limit}
	return context.WithValue(ctx, usageKey{}, u), u
}

// WithPriority identifies the release work competing for GitHub reads.
func WithPriority(ctx context.Context, priority Priority, source string) context.Context {
	return context.WithValue(ctx, requestKey{}, requestClass{priority: priority, source: source})
}

// New leaves 2,000 reads of a typical 5,000/hour quota for writes and other clients.
// GitHub's remaining-quota and retry headers can stop reads sooner.
func New(base http.RoundTripper) *Transport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &Transport{base: base, changed: make(chan struct{}), allowance: 3000, now: time.Now}
}

func class(req *http.Request) requestClass {
	if c, ok := req.Context().Value(requestKey{}).(requestClass); ok {
		return c
	}
	return requestClass{priority: Background, source: "other"}
}

func (t *Transport) signal() { close(t.changed); t.changed = make(chan struct{}) }

func (t *Transport) chosen() *http.Request {
	var best *http.Request
	for _, req := range t.pending {
		if req.Context().Err() != nil {
			continue
		}
		if best == nil || class(req).priority < class(best).priority || (class(req).priority == class(best).priority && class(best).source == t.last && class(req).source != t.last) {
			best = req
		}
	}
	return best
}

// RoundTrip applies scheduling only to github.com's REST reads; other services proceed independently.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet || req.URL.Hostname() != "api.github.com" {
		return t.base.RoundTrip(req)
	}
	t.mu.Lock()
	t.pending = append(t.pending, req)
	remove := func() {
		t.pending = slices.DeleteFunc(t.pending, func(p *http.Request) bool { return p == req })
		t.signal()
	}
	usage, _ := req.Context().Value(usageKey{}).(*Usage)
	for {
		if err := req.Context().Err(); err != nil {
			remove()
			t.mu.Unlock()
			return nil, err
		}
		now := t.now()
		for len(t.calls) > 0 && !t.calls[0].After(now.Add(-time.Hour)) {
			t.calls = t.calls[1:]
		}
		var deferred *DeferredError
		switch {
		case now.Before(t.until):
			deferred = &DeferredError{Reason: t.blocked, Until: t.until}
		case len(t.calls) >= t.allowance:
			deferred = &DeferredError{Reason: "shared GitHub hourly read allowance exhausted", Until: t.calls[0].Add(time.Hour)}
		case usage != nil && usage.limit > 0 && usage.Calls() >= int64(usage.limit):
			deferred = &DeferredError{Reason: "source-cycle GitHub read allowance exhausted", Until: now.Add(time.Minute)}
		}
		if deferred != nil {
			remove()
			t.mu.Unlock()
			return nil, deferred
		}
		if !t.busy && t.chosen() == req {
			break
		}
		changed := t.changed
		t.mu.Unlock()
		select {
		case <-changed:
		case <-req.Context().Done():
		}
		t.mu.Lock()
	}
	remove()
	t.busy = true
	t.last = class(req).source
	t.calls = append(t.calls, t.now())
	if usage != nil {
		usage.calls.Add(1)
	}
	t.mu.Unlock()
	finish := func() { t.mu.Lock(); defer t.mu.Unlock(); t.busy = false; t.signal() }
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		finish()
		return resp, err
	}
	if resp == nil {
		finish()
		return nil, fmt.Errorf("GitHub transport returned no response")
	}
	t.record(resp)
	if resp.Body == nil {
		resp.Body = http.NoBody
	}
	resp.Body = &releaseBody{ReadCloser: resp.Body, release: finish}
	return resp, nil
}

func (b *releaseBody) Close() error { defer b.once.Do(b.release); return b.ReadCloser.Close() }

func (t *Transport) record(resp *http.Response) {
	limited := resp.StatusCode == 429 || resp.Header.Get("X-RateLimit-Remaining") == "0" || (resp.StatusCode == 403 && resp.Header.Get("Retry-After") != "")
	if !limited {
		return
	}
	now := t.now()
	var until time.Time
	if seconds, err := strconv.ParseUint(resp.Header.Get("Retry-After"), 10, 32); err == nil {
		until = now.Add(time.Duration(seconds) * time.Second)
	} else if date, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
		until = date
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if seconds, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(seconds, 0).After(until) {
			until = time.Unix(seconds, 0)
		}
	}
	if !until.After(now) {
		until = now.Add(time.Minute)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if until.After(t.until) {
		t.until = until
		t.blocked = fmt.Sprintf("GitHub reads deferred after rate limit (HTTP %d)", resp.StatusCode)
	}
}
