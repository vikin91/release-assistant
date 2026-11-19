package githubread

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	ctx, usage := Track(t.Context(), 2)
	for range 2 {
		if err := read(t, limiter, ctx, "discovery"); err != nil {
			t.Fatal(err)
		}
	}
	if err := read(t, limiter, ctx, "planner"); err == nil {
		t.Fatal("planner bypassed source allowance")
	}
	if usage.Calls() != 2 || calls.Load() != 2 {
		t.Fatal("deferred reads consumed network calls")
	}
	for range 2 {
		ctx, _ := Track(t.Context(), 80)
		_ = read(t, limiter, ctx, "next-cycle")
	}
	if calls.Load() != 3 {
		t.Fatal("new cycle reset hourly allowance")
	}
	now = now.Add(time.Hour)
	if err := read(t, limiter, t.Context(), "later"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 4 {
		t.Fatal("rolling allowance did not recover")
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
	if err := read(t, limiter, t.Context(), "planner"); err != nil {
		t.Fatal(err)
	}
	err := read(t, limiter, t.Context(), "discovery")
	deferred, ok := errors.AsType[*DeferredError](err)
	if !ok || !deferred.Until.Equal(now.Add(2*time.Minute)) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
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
		if err := transport.SetStartupLimit(5); err != nil {
			t.Fatal(err)
		}
		check(5)
		if err := transport.SetStartupLimit(1); err != nil {
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
		if err := transport.SetStartupLimit(5); err != nil {
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
