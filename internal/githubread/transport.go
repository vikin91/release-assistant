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
	// Budget reports the assistant's rolling allowance and GitHub's last core REST quota.
	Budget struct {
		Remaining, Limit      int
		NextSlot, PausedUntil time.Time
		PauseReason           string
		GitHub                Quota
	}
	// Quota records GitHub's response headers. A zero ObservedAt means the quota is unknown.
	Quota struct {
		Remaining, Limit    int
		ObservedAt, ResetAt time.Time
	}
	// Transport schedules GitHub reads fairly within each priority and caps hourly spending.
	Transport struct {
		base         http.RoundTripper
		mu           sync.Mutex
		pending      []*http.Request
		changed      chan struct{}
		active       int
		startupLimit int
		ongoingLimit int
		startupUntil time.Time
		last         string
		calls        []time.Time
		allowance    int
		quota        Quota
		until        time.Time
		blocked      string
		now          func() time.Time
	}
	releaseBody struct {
		io.ReadCloser
		once    sync.Once
		release func()
	}
)

const (
	// Tags prioritizes component and RC selection.
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
	return fmt.Sprintf("%s. Retry after %s", e.Reason, e.Until.UTC().Format(time.RFC3339))
}

// Calls excludes cached, deferred, and non-GitHub observations.
func (u *Usage) Calls() int64 { return u.calls.Load() }

// Remaining returns the unused allowance of a bounded source cycle.
func (u *Usage) Remaining() int64 { return int64(u.limit) - u.Calls() }

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
	return &Transport{base: base, startupLimit: 3, ongoingLimit: 1, changed: make(chan struct{}), allowance: 3000, now: time.Now}
}

// StartMonitoring opens the five-minute startup window once per process.
func (t *Transport) StartMonitoring() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.startupUntil.IsZero() {
		const startupDuration = 5 * time.Minute
		t.startupUntil = t.now().Add(startupDuration)
		// A higher ongoing limit can admit queued reads while every startup slot is occupied.
		time.AfterFunc(startupDuration, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.signal()
		})
		t.signal()
	}
}

// DuringStartup reports whether discovery can prioritize initial release choices.
func (t *Transport) DuringStartup() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now().Before(t.startupUntil)
}

// ReadLimits returns the configured concurrency limits during and after startup.
func (t *Transport) ReadLimits() (startup, ongoing int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.startupLimit, t.ongoingLimit
}

// Budget returns the available reads at the time of the call, including during idle periods.
func (t *Transport) Budget() Budget {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.expireCalls(now)
	b := Budget{Remaining: t.allowance - len(t.calls), Limit: t.allowance}
	if b.Remaining == 0 {
		b.NextSlot = t.calls[0].Add(time.Hour)
	}
	if now.Before(t.until) {
		b.PausedUntil, b.PauseReason = t.until, t.blocked
	}
	if now.Before(t.quota.ResetAt) {
		b.GitHub = t.quota
	}
	return b
}

// expireCalls removes reads outside the rolling hour. The caller holds t.mu.
func (t *Transport) expireCalls(now time.Time) {
	cutoff := now.Add(-time.Hour)
	for len(t.calls) > 0 && !t.calls[0].After(cutoff) {
		t.calls = t.calls[1:]
	}
}

// SetReadLimits applies the supplied limits together. A nil argument keeps its current value.
func (t *Transport) SetReadLimits(startup, ongoing *int) error {
	if startup != nil && (*startup < 1 || *startup > 5) {
		return fmt.Errorf("concurrent GitHub reads during startup must be between 1 and 5")
	}
	if ongoing != nil && (*ongoing < 1 || *ongoing > 3) {
		return fmt.Errorf("concurrent GitHub reads after startup must be between 1 and 3")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if startup != nil {
		t.startupLimit = *startup
	}
	if ongoing != nil {
		t.ongoingLimit = *ongoing
	}
	t.signal()
	return nil
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

// RoundTrip schedules github.com REST reads and queries sent to the GraphQL endpoint.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	graphql := req.Method == http.MethodPost && req.URL.Path == "/graphql"
	if req.URL.Hostname() != "api.github.com" || (req.Method != http.MethodGet && !graphql) {
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
		t.expireCalls(now)
		var deferred *DeferredError
		switch {
		case now.Before(t.until):
			deferred = &DeferredError{Reason: t.blocked, Until: t.until}
		case len(t.calls) >= t.allowance:
			deferred = &DeferredError{
				Reason: fmt.Sprintf("Release Assistant deferred this request before contacting GitHub. Its internal limit is %d reads per rolling hour", t.allowance),
				Until:  t.calls[0].Add(time.Hour),
			}
		case usage != nil && usage.limit > 0 && usage.Calls() >= int64(usage.limit):
			deferred = &DeferredError{
				Reason: fmt.Sprintf("Release Assistant deferred this request before contacting GitHub. This source reached its internal limit of %d reads per cycle", usage.limit),
				Until:  now.Add(time.Minute),
			}
		}
		if deferred != nil {
			remove()
			t.mu.Unlock()
			return nil, deferred
		}
		limit := 1
		if now.Before(t.startupUntil) {
			limit = t.startupLimit
		} else if !t.startupUntil.IsZero() {
			limit = t.ongoingLimit
		}
		if t.active < limit && t.chosen() == req {
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
	t.active++
	t.last = class(req).source
	t.calls = append(t.calls, t.now())
	if usage != nil {
		usage.calls.Add(1)
	}
	t.mu.Unlock()
	finish := func() { t.mu.Lock(); defer t.mu.Unlock(); t.active--; t.signal() }
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
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	if resp.Header.Get("X-RateLimit-Resource") == "core" {
		limit, limitErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
		remaining, remainingErr := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
		reset, resetErr := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
		if limitErr == nil && remainingErr == nil && resetErr == nil && limit > 0 &&
			remaining >= 0 && remaining <= limit && reset > now.Unix() {
			t.quota = Quota{Remaining: remaining, Limit: limit, ObservedAt: now, ResetAt: time.Unix(reset, 0)}
		}
	}
	limited := resp.StatusCode == 429 || resp.Header.Get("X-RateLimit-Remaining") == "0" || (resp.StatusCode == 403 && resp.Header.Get("Retry-After") != "")
	if !limited {
		return
	}
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
	if until.After(t.until) {
		t.until = until
		t.blocked = fmt.Sprintf("GitHub reported a rate limit (HTTP %d). Release Assistant paused GitHub reads", resp.StatusCode)
		t.signal()
	}
}
