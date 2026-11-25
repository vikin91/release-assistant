package githubread

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func startRead(t *testing.T, transport http.RoundTripper, ctx context.Context, name string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return transport.RoundTrip(req)
}

func read(t *testing.T, transport http.RoundTripper, ctx context.Context, name string) error {
	t.Helper()
	resp, err := startRead(t, transport, ctx, name)
	if resp != nil {
		if closeErr := resp.Body.Close(); closeErr != nil {
			t.Errorf("close response for %s: %v", name, closeErr)
		}
	}
	return err
}

func response() *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
}

func TestRollingAndSourceAllowancesShareActualReads(t *testing.T) {
	var calls atomic.Int64
	limiter := New(roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return response(), nil }))
	now := time.Now()
	limiter.now = func() time.Time { return now }
	limiter.allowance = 3
	discovery := WithWork(t.Context(), "example/fact", "PR discovery")
	ctx, usage := Track(discovery, 2)
	for range 2 {
		if err := read(t, limiter, ctx, "discovery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := read(t, limiter, ctx, "planner"); err == nil || !strings.Contains(err.Error(), "internal limit of 2 reads per cycle") {
		t.Fatalf("source allowance: %v", err)
	}
	if usage.Calls() != 2 || calls.Load() != 2 {
		t.Fatal("deferred reads consumed network calls")
	}
	if got := limiter.Stats(); len(got) != 1 || got[0].Reads != 2 || got[0].CycleDeferred != 1 ||
		got[0].Repository != "example/fact" || got[0].Work != "PR discovery" {
		t.Fatalf("discovery statistics = %+v", got)
	}
	for range 2 {
		ctx, _ := Track(t.Context(), 80)
		_ = read(t, limiter, ctx, "next-cycle")
	}
	if calls.Load() != 3 {
		t.Fatal("new cycle reset hourly allowance")
	}
	budget := limiter.Budget()
	if budget.Remaining != 0 || budget.Limit != 3 || !budget.NextSlot.Equal(now.Add(time.Hour)) {
		t.Fatalf("exhausted budget = %+v", budget)
	}
	err := read(t, limiter, WithWork(t.Context(), "example/fact", "CI Retest scheduled"), "deferred")
	if err == nil || !strings.Contains(err.Error(), "before contacting GitHub. Its internal limit is 3 reads per rolling hour") {
		t.Fatalf("hourly allowance: %v", err)
	}
	if got := limiter.Stats(); len(got) != 3 || got[1].Work != "CI Retest scheduled" ||
		got[1].Reads != 0 || got[1].HourlyDeferred != 1 || got[1].CycleDeferred != 0 {
		t.Fatalf("statistics for the hourly allowance = %+v", got)
	}
	now = now.Add(time.Hour)
	if got := limiter.Budget(); got.Remaining != 3 || !got.NextSlot.IsZero() {
		t.Fatalf("idle budget after an hour = %+v", got)
	}
	if err := read(t, limiter, t.Context(), "later"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("rolling allowance did not recover")
	}
}

func TestBudgetUsesReportedCoreQuotaUntilReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	reset := now.Add(time.Hour)
	headers := make(http.Header)
	limiter := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: headers.Clone()}, nil
	}))
	limiter.now = func() time.Time { return now }
	if got := limiter.Budget(); got.Remaining != 3000 || !got.GitHub.ObservedAt.IsZero() {
		t.Fatalf("initial budget = %+v", got)
	}
	check := func(want Quota) {
		t.Helper()
		if err := read(t, limiter, t.Context(), "repos/example/project"); err != nil {
			t.Fatal(err)
		}
		if got := limiter.Budget().GitHub; got != want {
			t.Fatalf("reported quota = %+v, want %+v", got, want)
		}
	}
	headers.Set("X-RateLimit-Resource", "core")
	headers.Set("X-RateLimit-Limit", "5000")
	headers.Set("X-RateLimit-Remaining", "4123")
	check(Quota{}) // An incomplete report cannot establish the quota.
	headers.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	want := Quota{Remaining: 4123, Limit: 5000, ObservedAt: now, ResetAt: reset}
	check(want)
	now = now.Add(time.Minute)
	headers.Set("X-RateLimit-Resource", "search")
	headers.Set("X-RateLimit-Remaining", "19")
	check(want)
	headers.Set("X-RateLimit-Resource", "core")
	headers.Set("X-RateLimit-Remaining", "invalid")
	check(want)
	headers.Set("X-RateLimit-Remaining", "0")
	want.Remaining, want.ObservedAt = 0, now
	check(want)
	now = reset
	if got := limiter.Budget().GitHub; !got.ObservedAt.IsZero() {
		t.Fatalf("expired quota = %+v", got)
	}
}

func TestRateLimitStopsOtherClientsUntilReset(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	calls := 0
	limiter := New(roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		r := response()
		r.StatusCode = 429
		r.Header.Set("Retry-After", "120")
		return r, nil
	}))
	limiter.now = func() time.Time { return now }
	if err := read(t, limiter, WithWork(t.Context(), "example/b", "CI Retest scheduled"), "planner"); err != nil {
		t.Fatal(err)
	}
	err := read(t, limiter, WithWork(t.Context(), "example/a", "PR discovery"), "discovery")
	deferred, ok := errors.AsType[*DeferredError](err)
	if !ok || !strings.Contains(deferred.Reason, "GitHub reported a rate limit") || !deferred.Until.Equal(now.Add(2*time.Minute)) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	if got := limiter.Stats(); len(got) != 2 || got[0].PauseDeferred != 1 || got[0].Reads != 0 ||
		got[1].RateLimited != 1 || got[1].Reads != 1 {
		t.Fatalf("provider pause statistics = %+v", got)
	}
	now = now.Add(2 * time.Minute)
	if err := read(t, limiter, t.Context(), "retry"); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("reset was ignored")
	}
}

func TestPriorityFairnessAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		order := make(chan string, 10)
		limiter := New(roundTripFunc(func(r *http.Request) (*http.Response, error) { order <- r.URL.Path; return response(), nil }))
		first, err := startRead(t, limiter, WithPriority(t.Context(), Tags, "a"), "first")
		if err != nil {
			t.Fatal(err)
		}
		<-order
		start := func(ctx context.Context, name string) {
			go func() { _ = read(t, limiter, ctx, name) }()
			synctest.Wait()
		}
		start(WithPriority(t.Context(), Background, "acs"), "background")
		start(WithPriority(t.Context(), Tags, "a"), "same-source")
		start(WithPriority(t.Context(), Tags, "b"), "other-source")
		ctx, cancel := context.WithCancel(WithPriority(t.Context(), Tags, "c"))
		start(ctx, "canceled")
		cancel()
		synctest.Wait()
		time.Sleep(125 * time.Millisecond)
		if err := first.Body.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		for _, want := range []string{"/other-source", "/same-source", "/background"} {
			if got := <-order; got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		}
		if len(limiter.pending) != 0 || limiter.active != 0 {
			t.Fatal("request slot leaked")
		}
		const wait = 125 * time.Millisecond
		if got := limiter.Stats(); len(got) != 4 || got[0].Repository != "a" || got[0].Reads != 2 ||
			got[0].HTTPWait != wait || got[0].MaxHTTPWait != wait ||
			got[3].Repository != "c" || got[3].Reads != 0 || got[3].Canceled != 1 {
			t.Fatalf("HTTP queue measurements = %+v", got)
		}
	})
}

func TestOtherServicesAndWritesBypassReadAllowance(t *testing.T) {
	calls := 0
	limiter := New(roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(), nil }))
	limiter.until = time.Now().Add(time.Hour)
	for _, entry := range []struct{ method, url string }{{"GET", "https://quay.io/v2/"}, {"POST", "https://api.github.com/comments"}} {
		req, _ := http.NewRequestWithContext(t.Context(), entry.method, entry.url, nil)
		r, err := limiter.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("non-GitHub reads or writes were blocked")
	}
}

func TestStartupReadLimitAppliesToQueuedReads(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := New(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(), nil }))
		responses := make(chan *http.Response, 8)
		for range cap(responses) {
			go func() {
				resp, err := startRead(t, transport, t.Context(), "discovery")
				if err != nil {
					t.Error(err)
					return
				}
				responses <- resp
			}()
		}
		check := func(want int) {
			t.Helper()
			synctest.Wait()
			if got := len(responses); got != want {
				t.Fatalf("open response bodies = %d, want %d", got, want)
			}
		}
		closeOne := func() {
			t.Helper()
			if err := (<-responses).Body.Close(); err != nil {
				t.Fatal(err)
			}
		}
		check(1)
		transport.StartMonitoring()
		check(3)
		if err := transport.SetReadLimits(new(5), nil); err != nil {
			t.Fatal(err)
		}
		check(5)
		if err := transport.SetReadLimits(new(1), nil); err != nil {
			t.Fatal(err)
		}
		closeOne()
		check(4)
		time.Sleep(4 * time.Minute)
		transport.StartMonitoring()
		time.Sleep(time.Minute)
		if transport.DuringStartup() {
			t.Fatal("monitor restart extended the startup window")
		}
		if err := transport.SetReadLimits(new(5), nil); err != nil {
			t.Fatal(err)
		}
		for want := 3; want >= 1; want-- {
			closeOne()
			check(want)
		}
		for range 3 {
			closeOne()
			check(1)
		}
		closeOne()
		check(0)
	})
}

func TestHigherOngoingLimitWakesQueuedReadsAtCutoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := New(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(), nil }))
		if err := transport.SetReadLimits(new(1), new(2)); err != nil {
			t.Fatal(err)
		}
		transport.StartMonitoring()
		responses := make(chan *http.Response, 4)
		for range cap(responses) {
			go func() {
				resp, err := startRead(t, transport, t.Context(), "queued")
				if err != nil {
					t.Error(err)
					return
				}
				responses <- resp
			}()
		}
		check := func(want int) {
			t.Helper()
			synctest.Wait()
			if got := len(responses); got != want {
				t.Fatalf("open response bodies = %d, want %d", got, want)
			}
		}
		time.Sleep(5*time.Minute - time.Nanosecond)
		check(1)
		time.Sleep(time.Nanosecond)
		check(2)
		if err := transport.SetReadLimits(nil, new(3)); err != nil {
			t.Fatal(err)
		}
		check(3)
		for range cap(responses) {
			if err := (<-responses).Body.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
}
