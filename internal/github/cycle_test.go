package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestDiscoverDeduplicatesOnlyWithinOneCycle(t *testing.T) {
	client, request, counts := discoveryFixture(t, 1, 0, 3)
	handler := client.http.Transport.(fakeTransport).handler
	protectionReads := 0
	requiredName := "first-required-check"
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/required_status_checks") {
			protectionReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"contexts": []string{requiredName}})
			return
		}
		handler.ServeHTTP(w, r)
	})}
	for cycle := 1; cycle <= 2; cycle++ {
		clear(counts)
		repo, err := client.Discover(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if protectionReads != cycle || counts["commit_metadata"] != 1 || counts["checks"] != 3 || counts["statuses"] != 3 {
			t.Fatalf("protection=%d cycle=%d requests=%v", protectionReads, cycle, counts)
		}
		for _, pr := range repo.PRs {
			if !pr.ChecksEvidence.Fresh() || len(pr.Checks) != 1 || pr.Checks[0].Name != requiredName || pr.Checks[0].Required != release.Yes {
				t.Fatalf("stale required-check settings: %+v", pr)
			}
		}
		requiredName = "second-required-check"
	}
}

func TestDiscoveryCacheKeepsIdentitiesFreshForResourceNamedRepositories(t *testing.T) {
	for _, repo := range []string{"commits/fact", "acme/contents"} {
		t.Run(repo, func(t *testing.T) {
			reads := 0
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads++
				_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": fmt.Sprintf("head-%d", reads)}})
			}))
			client := New(srv.URL, "", srv.Client())
			cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: 10, remaining: 10}
			ctx := context.WithValue(t.Context(), discoveryCycleKey{}, cycle)
			for n := 1; n <= 2; n++ {
				var pr prResponse
				if err := client.get(ctx, repoPath(repo)+"/pulls/44", &pr); err != nil || pr.Head.SHA != fmt.Sprintf("head-%d", n) {
					t.Fatalf("head=%s error=%v", pr.Head.SHA, err)
				}
			}
			if reads != 2 {
				t.Fatalf("reads=%d, want 2", reads)
			}
		})
	}
}

func TestDiscoverRequestBudgetPreservesPartialResults(t *testing.T) {
	for name, tc := range map[string]struct {
		tags, merged, open, limit int
	}{
		"should bound a large inventory":                         {100, 500, 20, maxDiscoveryRequests},
		"should leave unobserved merge coverage unknown":         {1, 2, 0, 9},
		"should join budget failures encountered only in checks": {1, 0, 1, 9},
	} {
		t.Run(name, func(t *testing.T) {
			client, request, counts := discoveryFixture(t, tc.tags, tc.merged, tc.open)
			ctx := t.Context()
			repo, err := client.discover(ctx, request, tc.limit)
			total := 0
			for _, n := range counts {
				total += n
			}
			if total != tc.limit || err == nil || !strings.Contains(err.Error(), "request budget limit reached") || ctx.Err() != nil {
				t.Fatalf("requests=%d limit=%d error=%v context=%v", total, tc.limit, err, ctx.Err())
			}
			if repo.Head != "branch-sha" || repo.Evidence.Fresh() || len(repo.Tags) == 0 {
				t.Fatalf("lost partial results or confirmed incomplete inventory: %+v", repo)
			}
			for _, tag := range repo.Tags {
				for _, truth := range tag.Contains {
					if truth != "" {
						t.Fatalf("confirmed unread coverage: %+v", tag)
					}
				}
			}
			t.Logf("requests=%d tags=%d PRs=%d incomplete=true", total, len(repo.Tags), len(repo.PRs))
		})
	}
}

func TestDiscoverReservesReadsForACSPins(t *testing.T) {
	client, request, counts := discoveryFixture(t, 100, 500, 20)
	request.Source.Name, request.Version, request.PinningPR = "acs", "4.10.1", 44
	handler := client.http.Transport.(fakeTransport).handler
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/contents/"):
			counts["pins"]++
			_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("0.3.1"))})
		case strings.HasSuffix(r.URL.Path, "/pulls/44"):
			counts["pinning_pr"]++
			_, _ = w.Write([]byte(pinningPRJSON("open", false, "pin-head", "release", "pin versions", false)))
		case strings.Contains(r.URL.Path, "/git/matching-refs/tags/"):
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/stackrox/stackrox/tags":
			counts["previous_tags"]++
			_, _ = w.Write([]byte(`[]`))
		default:
			handler.ServeHTTP(w, r)
		}
	})}
	repo, err := client.Discover(t.Context(), request)
	total := 0
	for _, n := range counts {
		total += n
	}
	if err == nil || !strings.Contains(err.Error(), "request budget limit reached") || total > maxDiscoveryRequests {
		t.Fatalf("requests=%d error=%v", total, err)
	}
	if !repo.Pins.Evidence.Fresh() || !repo.PinningEvidence.Fresh() || !repo.PinningPins.Evidence.Fresh() || len(repo.PinningPins.Versions) != 3 {
		t.Fatalf("inventory exhausted the pinning reserve: %+v", repo)
	}
	t.Logf("requests=%d inventory_incomplete=true branch_pins_fresh=true PR_pins_fresh=true", total)
}

func TestDiscoveryBudgetDoesNotHidePaginationLimits(t *testing.T) {
	client, request, _ := discoveryFixture(t, 1, 0, 0)
	handler := client.http.Transport.(fakeTransport).handler
	pages := 0
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/fact/tags" {
			pages++
			tags := make([]map[string]string, 100)
			for i := range tags {
				tags[i] = map[string]string{"name": "9.9.1"}
			}
			_ = json.NewEncoder(w).Encode(tags)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	repo, err := client.Discover(t.Context(), request)
	if pages != maxPages || err == nil || !strings.Contains(err.Error(), "exceeded pagination limit") || repo.Evidence.Fresh() {
		t.Fatalf("pages=%d error=%v evidence=%+v", pages, err, repo.Evidence)
	}
}

func TestRateLimitCooldownStopsSubsequentReads(t *testing.T) {
	for name, tc := range map[string]struct {
		status    int
		header    string
		value     string
		wantReads int
	}{
		"should stop after HTTP 429":                       {429, "Retry-After", "60", 1},
		"should stop after primary quota exhaustion":       {403, "X-RateLimit-Remaining", "0", 1},
		"should keep ordinary access failures independent": {403, "", "", 2},
	} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads++
				if tc.header != "" {
					w.Header().Set(tc.header, tc.value)
				}
				w.WriteHeader(tc.status)
			}))
			client := New(srv.URL, "", srv.Client())
			for range 2 {
				err := client.get(t.Context(), "/repos/acme/fact/branches/release", new(branchResponse))
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", tc.status)) {
					t.Fatalf("error=%v", err)
				}
			}
			if reads != tc.wantReads {
				t.Fatalf("reads=%d, want %d", reads, tc.wantReads)
			}
		})
	}
	t.Run("should share cooldown across discovery cycles", func(t *testing.T) {
		reads := 0
		srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reads++
			if reads == 1 {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"commit":{"sha":"recovered"}}`))
		}))
		client := New(srv.URL, "", srv.Client())
		_, request, _ := discoveryFixture(t, 1, 1, 0)
		for range 2 {
			repo, err := client.Discover(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "HTTP 429") || repo.Evidence.Fresh() {
				t.Fatalf("error=%v evidence=%+v", err, repo.Evidence)
			}
		}
		if reads != 1 {
			t.Fatalf("reads=%d, want 1", reads)
		}
		client.backoff.until = time.Now().Add(-time.Second)
		var branch branchResponse
		if err := client.get(t.Context(), "/repos/acme/fact/branches/release", &branch); err != nil || reads != 2 || branch.Commit.SHA != "recovered" {
			t.Fatalf("expired cooldown did not resume reads: reads=%d branch=%+v error=%v", reads, branch, err)
		}
	})
}

func TestReadBackoffHonorsHeadersAndResumes(t *testing.T) {
	for name, tc := range map[string]struct {
		headers http.Header
		minimum time.Duration
	}{
		"retry seconds":   {http.Header{"Retry-After": {"120"}}, 110 * time.Second},
		"retry date":      {http.Header{"Retry-After": {time.Now().Add(2 * time.Minute).UTC().Format(http.TimeFormat)}}, 110 * time.Second},
		"primary reset":   {http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {strconv.FormatInt(time.Now().Add(2*time.Minute).Unix(), 10)}}, 110 * time.Second},
		"missing headers": {http.Header{}, 50 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			var backoff readBackoff
			backoff.record(&http.Response{Header: tc.headers}, fmt.Errorf("rate limited"))
			if backoff.problem() == nil || time.Until(backoff.until) < tc.minimum {
				t.Fatalf("until=%v error=%v", backoff.until, backoff.problem())
			}
			until := backoff.until
			backoff.record(&http.Response{Header: http.Header{"Retry-After": {"1"}}}, fmt.Errorf("another source was rate limited"))
			if !backoff.until.Equal(until) {
				t.Fatal("a shorter cooldown overrode the longer wait")
			}
			backoff.until = time.Now().Add(-time.Second)
			if backoff.problem() != nil {
				t.Fatal("expired cooldown prevented reads")
			}
		})
	}
}

func TestRateLimitCooldownIsSafeAcrossSources(t *testing.T) {
	var reads atomic.Int64
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	client := New(srv.URL, "", srv.Client())
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() { _ = client.get(t.Context(), "/repos/acme/fact/branches/release", new(branchResponse)) })
	}
	workers.Wait()
	before := reads.Load()
	for range 4 {
		_ = client.get(t.Context(), "/repos/acme/fact/branches/release", new(branchResponse))
	}
	if reads.Load() != before {
		t.Fatal("sent requests during shared cooldown")
	}
}

func TestDiscoverSharesRequiredCheckFailuresWithoutConfirmingHealth(t *testing.T) {
	client, request, _ := discoveryFixture(t, 1, 0, 3)
	handler := client.http.Transport.(fakeTransport).handler
	protectionReads := 0
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/required_status_checks") {
			protectionReads++
			http.Error(w, "private response", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	for cycle := 1; cycle <= 2; cycle++ {
		repo, _ := client.Discover(t.Context(), request)
		if protectionReads != cycle {
			t.Fatalf("protection reads=%d, want %d", protectionReads, cycle)
		}
		for _, pr := range repo.PRs {
			if pr.ChecksStatus() != "" || pr.ChecksEvidence.Fresh() || !strings.Contains(pr.ChecksEvidence.Error, "HTTP 403") || strings.Contains(pr.ChecksEvidence.Error, "private response") {
				t.Fatalf("failed protection read confirmed health: %+v", pr)
			}
		}
	}
}
