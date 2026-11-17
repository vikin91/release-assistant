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
func read(t *testing.T, transport http.RoundTripper, ctx context.Context, name string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/"+name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return transport.RoundTrip(req)
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
		r, err := read(t, limiter, ctx, "discovery")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	if _, err := read(t, limiter, ctx, "planner"); err == nil {
		t.Fatal("planner bypassed source allowance")
	}
	if usage.Calls() != 2 || calls.Load() != 2 {
		t.Fatal("deferred reads consumed network calls")
	}
	for range 2 {
		ctx, _ := Track(t.Context(), 80)
		r, err := read(t, limiter, ctx, "next-cycle")
		if err == nil {
			r.Body.Close()
		}
	}
	if calls.Load() != 3 {
		t.Fatal("new cycle reset hourly allowance")
	}
	now = now.Add(time.Hour)
	r, err := read(t, limiter, t.Context(), "later")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
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
	r, err := read(t, limiter, t.Context(), "planner")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	_, err = read(t, limiter, t.Context(), "discovery")
	deferred, ok := errors.AsType[*DeferredError](err)
	if !ok || !deferred.Until.Equal(now.Add(2*time.Minute)) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
	now = now.Add(2 * time.Minute)
	r, err = read(t, limiter, t.Context(), "retry")
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if calls != 2 {
		t.Fatal("reset was ignored")
	}
}

func TestPriorityFairnessAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		order := make(chan string, 10)
		limiter := New(roundTripFunc(func(r *http.Request) (*http.Response, error) { order <- r.URL.Path; return response(), nil }))
		first, err := read(t, limiter, WithPriority(t.Context(), Tags, "a"), "first")
		if err != nil {
			t.Fatal(err)
		}
		<-order
		start := func(ctx context.Context, name string) {
			go func() {
				r, err := read(t, limiter, ctx, name)
				if err == nil {
					r.Body.Close()
				}
			}()
			synctest.Wait()
		}
		start(WithPriority(t.Context(), Background, "acs"), "background")
		start(WithPriority(t.Context(), Tags, "a"), "same-source")
		start(WithPriority(t.Context(), Tags, "b"), "other-source")
		ctx, cancel := context.WithCancel(WithPriority(t.Context(), Tags, "c"))
		start(ctx, "canceled")
		cancel()
		synctest.Wait()
		first.Body.Close()
		synctest.Wait()
		for _, want := range []string{"/other-source", "/same-source", "/background"} {
			if got := <-order; got != want {
				t.Fatalf("got %s want %s", got, want)
			}
		}
		if len(limiter.pending) != 0 || limiter.busy {
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
		r.Body.Close()
	}
	if calls != 2 {
		t.Fatal("non-GitHub reads or writes were blocked")
	}
}
