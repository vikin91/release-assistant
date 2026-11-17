package github

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

type fakeServer struct {
	URL     string
	handler http.Handler
}

func newFakeServer(handler http.Handler) *fakeServer {
	return &fakeServer{URL: "https://api.github.test", handler: handler}
}
func (s *fakeServer) Client() *http.Client {
	return &http.Client{Transport: fakeTransport{handler: s.handler}}
}
func (*fakeServer) Close() {}

type fakeTransport struct{ handler http.Handler }

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	response.Request = req
	return response, nil
}

type errorTransport struct{ err error }

func (t errorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

type timeoutError struct{}

func (timeoutError) Error() string   { return "secret transport detail" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestGetReportsSafeHTTPDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		header   string
		value    string
		wantHint string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantHint: "Authentication failed"},
		{name: "forbidden", status: http.StatusForbidden, wantHint: "Access denied"},
		{name: "forbidden rate limit", status: http.StatusForbidden, header: "X-RateLimit-Remaining", value: "0", wantHint: "rate limit"},
		{name: "not found", status: http.StatusNotFound, wantHint: "Not found or inaccessible"},
		{name: "too many requests", status: http.StatusTooManyRequests, wantHint: "rate limit"},
		{name: "server error", status: http.StatusInternalServerError, wantHint: "GitHub server error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.header != "" {
					w.Header().Set(tc.header, tc.value)
				}
				http.Error(w, "secret response body", tc.status)
			}))
			c := New("https://url-user:url-password@api.github.test", "secret-token", srv.Client())
			err := c.get(context.Background(), "/repos/acme/repo/secret-token?access_token=secret-query", new(map[string]any))
			if err == nil {
				t.Fatal("get succeeded, want error")
			}
			message := err.Error()
			for _, forbidden := range []string{"secret-token", "secret-query", "secret response body", "access_token", "url-user", "url-password"} {
				if strings.Contains(message, forbidden) {
					t.Errorf("diagnostic contains %q: %s", forbidden, message)
				}
			}
			if !strings.Contains(message, "/repos/acme/repo") || !strings.Contains(message, "HTTP "+strconv.Itoa(tc.status)) || !strings.Contains(message, tc.wantHint) {
				t.Errorf("diagnostic missing endpoint, status, or hint: %s", message)
			}
		})
	}
}

func TestGetReportsSafeMalformedResponse(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret malformed response"))
	}))
	c := New(srv.URL, "secret-token", srv.Client())
	err := c.get(context.Background(), "/repos/acme/repo?token=secret-query", new(map[string]any))
	if err == nil || !strings.Contains(err.Error(), "/repos/acme/repo") || !strings.Contains(err.Error(), "response malformed") {
		t.Fatalf("unexpected malformed response diagnostic: %v", err)
	}
	for _, forbidden := range []string{"secret-token", "secret-query", "secret malformed", "?token"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("diagnostic contains %q: %v", forbidden, err)
		}
	}
}

func TestGetClassifiesSafeTransportFailures(t *testing.T) {
	tests := []struct {
		name   string
		cause  error
		want   string
		wantIs error
	}{
		{name: "timeout", cause: timeoutError{}, want: "network timeout"},
		{name: "cancellation", cause: context.Canceled, want: "context canceled", wantIs: context.Canceled},
		{name: "deadline", cause: context.DeadlineExceeded, want: "context deadline exceeded", wantIs: context.DeadlineExceeded},
		{name: "DNS", cause: &net.DNSError{Err: "secret DNS detail", Name: "secret.example"}, want: "DNS lookup failed"},
		{name: "TLS", cause: &tls.RecordHeaderError{Msg: "secret TLS detail"}, want: "TLS handshake failed"},
		{name: "TLS value", cause: tls.RecordHeaderError{Msg: "secret TLS detail"}, want: "TLS handshake failed"},
		{name: "network", cause: errors.New("secret network detail"), want: "network failure"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := New("https://api.github.test", "secret-token", &http.Client{Transport: errorTransport{err: tc.cause}})
			err := c.get(context.Background(), "/repos/acme/repo?token=secret-query", new(map[string]any))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not preserve %v", err, tc.wantIs)
			}
			for _, forbidden := range []string{"secret-token", "secret-query", "secret DNS detail", "secret.example", "secret network detail"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Errorf("diagnostic contains %q: %v", forbidden, err)
				}
			}
		})
	}
}

func TestDiscoverReturnsPartialRepositoryAndSafeCauses(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches/release"):
			http.Error(w, "private body", http.StatusUnauthorized)
		case strings.HasSuffix(r.URL.Path, "/tags"), strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	c := New(srv.URL, "secret-token", srv.Client())
	repo, err := c.Discover(context.Background(), app.DiscoveryRequest{
		Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release", Stream: "3.25"},
	})
	if err == nil || repo.Source.Repo != "acme/fact" || repo.Evidence.Known {
		t.Fatalf("expected partial repository and discovery error, got repo=%+v err=%v", repo, err)
	}
	if !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "/branches/release") {
		t.Errorf("discovery error lost branch failure: %v", err)
	}
	for _, forbidden := range []string{"secret-token", "private body"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("discovery error contains %q: %v", forbidden, err)
		}
	}
}

func TestPagesPaginates(t *testing.T) {
	requests := 0
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("page") == "1" {
			items := make([]map[string]int, 100)
			for i := range items {
				items[i] = map[string]int{"n": i}
			}
			_ = json.NewEncoder(w).Encode(items)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]int{{"n": 100}})
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	count := 0
	if err := c.pages(context.Background(), "/things", func(json.RawMessage) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || count != 101 {
		t.Fatalf("requests=%d items=%d, want 2 and 101", requests, count)
	}
}

func TestListTagsResolvesAnnotatedTagAndKeepsAncestryUnknownOnFailure(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/repos/acme/fact/tags"):
			_, _ = w.Write([]byte(`[{"name":"3.25.1","commit":{"sha":"tag-object"}}]`))
		case strings.HasSuffix(r.URL.Path, "/git/ref/tags/3.25.1"):
			_, _ = w.Write([]byte(`{"object":{"sha":"tag-object","type":"tag"}}`))
		case strings.HasSuffix(r.URL.Path, "/git/tags/tag-object"):
			_, _ = w.Write([]byte(`{"object":{"sha":"commit-sha","type":"commit"}}`))
		case strings.HasSuffix(r.URL.Path, "/commits/commit-sha"):
			_, _ = w.Write([]byte(`{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`))
		case strings.HasSuffix(r.URL.Path, "/branches/release"):
			_, _ = w.Write([]byte(`{"commit":{"sha":"branch-sha"}}`))
		case strings.Contains(r.URL.Path, "/compare/"):
			http.Error(w, "secret body must not escape", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	tags, err := c.listTags(context.Background(), release.Source{Repo: "acme/fact", Branch: "release", Stream: "3.25"}, "branch-sha")
	if err == nil || len(tags) != 1 {
		t.Fatalf("expected one partial tag and an ancestry error, got %d, %v", len(tags), err)
	}
	if tags[0].observation.SHA != "commit-sha" || tags[0].observation.Belongs != "" {
		t.Fatalf("unexpected resolved tag: %+v", tags[0].observation)
	}
}

func TestListRelevantPRsIncludesMergedAndClosedHistoryAfterBaseline(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[{"number":11,"title":"open PR","state":"open","head":{"sha":"head-11"},"base":{"ref":"release"}}]`))
		case strings.HasSuffix(r.URL.Path, "/commits/base-sha"):
			_, _ = w.Write([]byte(`{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`))
		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			_, _ = w.Write([]byte(`{"total_count":2,"items":[{"number":12},{"number":13}]}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/12"):
			_, _ = w.Write([]byte(`{"number":12,"title":"merged PR","state":"closed","merged":true,"merge_commit_sha":"merge-12","head":{"sha":"head-12"},"base":{"ref":"release"}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/13"):
			_, _ = w.Write([]byte(`{"number":13,"title":"second merged PR","state":"closed","merged":true,"head":{"sha":"head-13"},"base":{"ref":"release"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	prs, err := c.listRelevantPRs(context.Background(), release.Source{Repo: "acme/fact", Branch: "release"}, []release.Tag{{Name: "3.25.1", SHA: "base-sha", Belongs: release.Yes}}, 0, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	states := map[int]string{}
	for _, p := range prs {
		states[p.Number] = p.State
	}
	if len(states) != 3 || states[11] != "open" || states[12] != "closed" || states[13] != "closed" {
		t.Fatalf("discovered PR states: %#v", states)
	}
}

func TestDiscoverPreservesPullRequestAuthorAndLeavesMissingAuthorEmpty(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/branches/release"):
			_, _ = w.Write([]byte(`{"commit":{"sha":"branch-sha"}}`))
		case strings.HasSuffix(r.URL.Path, "/tags"):
			_, _ = w.Write([]byte(`[{"name":"3.25.1"}]`))
		case strings.HasSuffix(r.URL.Path, "/git/ref/tags/3.25.1"):
			_, _ = w.Write([]byte(`{"object":{"sha":"tag-sha","type":"commit"}}`))
		case strings.HasSuffix(r.URL.Path, "/commits/tag-sha"):
			_, _ = w.Write([]byte(`{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`))
		case strings.HasSuffix(r.URL.Path, "/compare/tag-sha...branch-sha"):
			_, _ = w.Write([]byte(`{"status":"ahead"}`))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			_, _ = w.Write([]byte(`{"total_count":2,"items":[{"number":17},{"number":18}]}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/17"):
			_, _ = w.Write([]byte(`{"number":17,"title":"with author","state":"closed","html_url":"https://github.com/acme/fact/pull/17","user":{"login":"github-user"},"head":{"sha":"head-17"},"base":{"ref":"release"}}`))
		case strings.HasSuffix(r.URL.Path, "/pulls/18"):
			_, _ = w.Write([]byte(`{"number":18,"title":"without author","state":"closed","head":{"sha":"head-18"},"base":{"ref":"release"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "", srv.Client())
	repo, err := c.Discover(context.Background(), app.DiscoveryRequest{
		Source:  release.Source{Name: "fact", Repo: "acme/fact", Branch: "release", Stream: "3.25"},
		Version: "3.25.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.PRs) != 2 {
		t.Fatalf("discovered %d PRs, want 2", len(repo.PRs))
	}
	authors := map[int]string{}
	for _, pr := range repo.PRs {
		authors[pr.Target.PR] = pr.Author
	}
	if authors[17] != "github-user" {
		t.Errorf("PR 17 author = %q, want github-user", authors[17])
	}
	if authors[18] != "" {
		t.Errorf("PR 18 author = %q, want empty", authors[18])
	}
}

func TestListRelevantPRsUsesConfirmedOlderTagAsBaseline(t *testing.T) {
	usedOlder := false
	queriedFromOlderDate := false
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/commits/older-sha"):
			usedOlder = true
			_, _ = w.Write([]byte(`{"commit":{"committer":{"date":"2026-01-02T00:00:00Z"}}}`))
		case strings.HasSuffix(r.URL.Path, "/commits/newer-sha"):
			t.Errorf("newer eligible tag used despite older confirmation")
		case strings.HasPrefix(r.URL.Path, "/search/issues"):
			queriedFromOlderDate = strings.Contains(r.URL.Query().Get("q"), "merged:>=2026-01-02")
			_, _ = w.Write([]byte(`{"total_count":0,"items":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	c := New(srv.URL, "", srv.Client())
	tags := []release.Tag{{Name: "3.25.1", SHA: "older-sha", Belongs: release.Yes}, {Name: "3.25.9", SHA: "newer-sha", Belongs: release.Yes}}
	if _, err := c.listRelevantPRs(context.Background(), release.Source{Repo: "acme/fact", Branch: "release"}, tags, 1, "3.25.1", nil); err != nil {
		t.Fatal(err)
	}
	if !usedOlder || !queriedFromOlderDate {
		t.Fatalf("older baseline was not used (commit=%t query=%t)", usedOlder, queriedFromOlderDate)
	}
}

func TestReadChecksPaginatesRunsAndReadsRequiredStatuses(t *testing.T) {
	requests := 0
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			requests++
			if r.URL.Query().Get("page") == "2" {
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "check_runs": []map[string]string{{"name": "second-page", "status": "completed", "conclusion": "failure"}}})
				return
			}
			runs := make([]map[string]string, 0, 100)
			for i := 0; i < 100; i++ {
				runs = append(runs, map[string]string{"name": "required-check", "status": "completed", "conclusion": "success", "html_url": "https://checks.test/run"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "check_runs": runs})
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]string{{"context": "legacy-check", "state": "success", "target_url": "https://checks.test/legacy"}})
		case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
			_ = json.NewEncoder(w).Encode(map[string]any{"contexts": []string{"required-check"}})
		default:
			http.NotFound(w, r)
		}
	}))
	c := New(srv.URL, "", srv.Client())
	pr := release.PR{SHA: "head-sha"}
	c.readChecks(context.Background(), release.Source{Repo: "acme/rox", Branch: "release"}, &pr)
	if requests != 2 || len(pr.Checks) != 102 || pr.Evidence.Error != "" {
		t.Fatalf("requests=%d checks=%d evidence=%+v", requests, len(pr.Checks), pr.Evidence)
	}
	if pr.Checks[0].Required != release.Yes || pr.Checks[101].Required != release.No {
		t.Fatalf("required statuses not mapped: %+v %+v", pr.Checks[0], pr.Checks[101])
	}
}

func TestReadPinsReportsMissingRequiredPin(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	pins, err := c.readPins(context.Background(), "acme/stackrox", "sha", "5.0.1")
	if err == nil || pins.Evidence.Known {
		t.Fatalf("missing pins must remain unknown: %+v, %v", pins, err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") || !strings.Contains(err.Error(), "version pin") {
		t.Errorf("version pin error lost file-read causes: %v", err)
	}
}

func TestDiscoverReadsMergedPinningPRAtMergeCommit(t *testing.T) {
	refs := map[string]bool{}
	values := map[string]map[string]string{
		"branch-sha": {"FACT_VERSION": "fact-current", "COLLECTOR_VERSION": "collector-current", "SCANNER_VERSION": "scanner-current"},
		"head-sha":   {"FACT_VERSION": "fact-head", "COLLECTOR_VERSION": "collector-head", "SCANNER_VERSION": "scanner-head"},
		"merge-sha":  {"FACT_VERSION": "fact-merged", "COLLECTOR_VERSION": "collector-merged", "SCANNER_VERSION": "scanner-merged"},
	}
	srv := pinningDiscoveryServer(t, `{"number":44,"title":"pin versions","state":"closed","merged":true,"merge_commit_sha":"merge-sha","head":{"sha":"head-sha"},"base":{"ref":"release"}}`, refs, values)
	defer srv.Close()

	repo, _ := New(srv.URL, "", srv.Client()).Discover(t.Context(), app.DiscoveryRequest{
		Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "4.8.0", PinningPR: 44,
	})
	if !repo.Pins.Evidence.Known || repo.Pins.Versions["fact"] != "fact-current" {
		t.Fatalf("current branch pins = %+v", repo.Pins)
	}
	want := map[string]string{"fact": "fact-merged", "collector": "collector-merged", "scanner": "scanner-merged"}
	if !repo.PinningPins.Evidence.Known || !maps.Equal(repo.PinningPins.Versions, want) {
		t.Fatalf("merged PR pins = %+v, want versions %v", repo.PinningPins, want)
	}
	if refs["head-sha"] {
		t.Fatal("merged pinning PR files were read from its head SHA")
	}
	if !refs["merge-sha"] || !refs["branch-sha"] {
		t.Fatalf("refs read = %v, want branch and merge commit", refs)
	}
}

func TestDiscoverReadsOpenPinningPRAtHead(t *testing.T) {
	refs := map[string]bool{}
	values := map[string]map[string]string{
		"branch-sha": {"FACT_VERSION": "fact-current", "COLLECTOR_VERSION": "collector-current"},
		"head-sha":   {"FACT_VERSION": "fact-head", "COLLECTOR_VERSION": "collector-head"},
	}
	srv := pinningDiscoveryServer(t, `{"number":44,"title":"pin versions","state":"open","head":{"sha":"head-sha"},"base":{"ref":"release"}}`, refs, values)
	defer srv.Close()

	repo, _ := New(srv.URL, "", srv.Client()).Discover(t.Context(), app.DiscoveryRequest{
		Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "5.0.0", PinningPR: 44,
	})
	if got := repo.PinningPins.Versions["fact"]; got != "fact-head" {
		t.Fatalf("open PR fact pin = %q, want fact-head", got)
	}
	if !repo.PinningPins.Evidence.Known || !refs["head-sha"] {
		t.Fatalf("open PR head was not used: pins=%+v refs=%v", repo.PinningPins, refs)
	}
}

func TestDiscoverDoesNotUseWrongBaseOrMissingMergeSHA(t *testing.T) {
	tests := []struct {
		name string
		pr   string
	}{
		{name: "wrong base", pr: `{"number":44,"merged":true,"merge_commit_sha":"merge-sha","head":{"sha":"head-sha"},"base":{"ref":"other"}}`},
		{name: "missing merge SHA", pr: `{"number":44,"merged":true,"head":{"sha":"head-sha"},"base":{"ref":"release"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			refs := map[string]bool{}
			values := map[string]map[string]string{
				"branch-sha": {"FACT_VERSION": "fact-current", "COLLECTOR_VERSION": "collector-current"},
				"head-sha":   {"FACT_VERSION": "fact-head", "COLLECTOR_VERSION": "collector-head"},
				"merge-sha":  {"FACT_VERSION": "fact-merged", "COLLECTOR_VERSION": "collector-merged"},
			}
			srv := pinningDiscoveryServer(t, tc.pr, refs, values)
			defer srv.Close()

			repo, _ := New(srv.URL, "", srv.Client()).Discover(t.Context(), app.DiscoveryRequest{
				Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "5.0.0", PinningPR: 44,
			})
			if tc.name == "wrong base" {
				if len(repo.PinningPins.Versions) != 0 || refs["head-sha"] || refs["merge-sha"] {
					t.Fatalf("wrong-base PR pins were used: pins=%+v refs=%v", repo.PinningPins, refs)
				}
				return
			}
			if repo.PinningPins.Evidence.Error == "" || repo.PinningPins.Evidence.Known {
				t.Fatalf("missing merge SHA did not preserve uncertainty: %+v", repo.PinningPins)
			}
			if refs["head-sha"] || refs["merge-sha"] {
				t.Fatalf("missing merge SHA caused a fallback read: refs=%v", refs)
			}
		})
	}
}

func pinningDiscoveryServer(t *testing.T, pr string, refs map[string]bool, values map[string]map[string]string) *fakeServer {
	t.Helper()
	return newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/acs/branches/release":
			_, _ = w.Write([]byte(`{"commit":{"sha":"branch-sha"}}`))
		case r.URL.Path == "/repos/acme/acs/tags", r.URL.Path == "/repos/acme/acs/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/acme/acs/pulls/44":
			_, _ = w.Write([]byte(pr))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/acs/contents/"):
			ref := r.URL.Query().Get("ref")
			refs[ref] = true
			file := strings.TrimPrefix(r.URL.Path, "/repos/acme/acs/contents/")
			content, ok := values[ref][file]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(content)),
			})
		case r.URL.Path == "/repos/stackrox/stackrox/tags":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestWriterUsesPRIssueAndCommitCommentEndpoints(t *testing.T) {
	tests := []struct {
		name              string
		target            release.Target
		wantPath, wantSHA string
	}{
		{name: "PR", target: release.Target{Repo: "acme/rox", PR: 17}, wantPath: "/repos/acme/rox/issues/17/comments"},
		{name: "tag", target: release.Target{Repo: "acme/rox", Tag: "4.8.1"}, wantPath: "/repos/acme/rox/commits/commit-sha/comments", wantSHA: "commit-sha"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				if r.Method != http.MethodPost || r.URL.Path != tt.wantPath {
					t.Errorf("request %s %s", r.Method, r.URL.Path)
				}
				if tt.wantSHA != "" && r.URL.Path != "/repos/acme/rox/commits/"+tt.wantSHA+"/comments" {
					t.Errorf("wrong commit destination")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["body"] != "/konflux-retest job-a" {
					t.Errorf("payload=%q", body["body"])
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"html_url":"https://github.com/acme/rox/comment/1"}`))
			}))
			defer srv.Close()
			c := New(srv.URL, "test-secret", srv.Client())
			delivery, err := NewWriter(c).Write(context.Background(), release.Proposal{Target: tt.target, SHA: "commit-sha", Payload: "/konflux-retest job-a"})
			if err != nil || delivery.Status != "sent" || delivery.URL == "" || !called {
				t.Fatalf("delivery=%+v err=%v called=%t", delivery, err, called)
			}
		})
	}
}

func TestWriterDistinguishesRejectedFromUncertain(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{{http.StatusBadRequest, "failed"}, {http.StatusInternalServerError, "uncertain"}} {
		srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "private response content", tc.status) }))
		c := New(srv.URL, "", srv.Client())
		delivery, err := NewWriter(c).Write(context.Background(), release.Proposal{Target: release.Target{Repo: "acme/rox", PR: 1}, SHA: "sha", Payload: "command"})
		srv.Close()
		if err == nil || delivery.Status != tc.want || strings.Contains(err.Error(), "private") {
			t.Fatalf("status=%d delivery=%+v err=%v", tc.status, delivery, err)
		}
	}
}

func TestReadTargetDetectsOptOutAndCurrentHead(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"number":4,"state":"open","html_url":"https://github.com/acme/rox/pull/4","head":{"sha":"fresh-head"},"labels":[{"name":"no-release-assistant"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	got, err := c.ReadTarget(context.Background(), release.Target{Repo: "acme/rox", PR: 4})
	if err != nil || got.SHA != "fresh-head" || !got.Open || !got.OptOut {
		t.Fatalf("identity=%+v err=%v", got, err)
	}
}

func TestFileDecodesBase64Contents(t *testing.T) {
	want := []byte("3.25.4\n")
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ref") != "immutable-sha" {
			t.Errorf("ref=%q", r.URL.Query().Get("ref"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString(want)})
	}))
	defer srv.Close()
	got, err := New(srv.URL, "", srv.Client()).File(context.Background(), "acme/rox", "immutable-sha", "FACT_VERSION")
	if err != nil || string(got) != string(want) {
		t.Fatalf("file=%q err=%v", got, err)
	}
}

type failedBody struct{ err error }

func (b failedBody) Read([]byte) (int, error) { return 0, b.err }
func (failedBody) Close() error               { return nil }

type bodyFailureTransport struct{ err error }

func (t bodyFailureTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: failedBody(t), Header: make(http.Header)}, nil
}

func TestGetPreservesDeadlineDuringResponseRead(t *testing.T) {
	client := New("https://api.github.test", "secret-token", &http.Client{Transport: bodyFailureTransport{err: context.DeadlineExceeded}})
	err := client.get(t.Context(), "/repos/acme/repo?token=secret-query", new(map[string]any))
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "malformed") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("body read timeout was lost or unsafe: %v", err)
	}
}
