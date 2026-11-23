package konflux

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/release"
)

const (
	liveRead    = "https://api.example.openshiftapps.com/apis/tekton.dev/v1/namespaces/builds/pipelineruns/run"
	archiveRead = "https://tekton-results-tekton-results.apps.example.openshiftapps.com/apis/results.tekton.dev/v1alpha2/parents/builds/results/-/records"
)

func TestReadBackoffScopesFailuresAndProbes(t *testing.T) {
	now := time.Now()
	probeStarted := make(chan struct{})
	probeRelease := make(chan struct{})
	calls := 0
	var transitions []ReadIssue
	transport := &readBackoff{
		now:       func() time.Time { return now },
		endpoints: make(map[string]*endpointBackoff),
		onPauseChange: func(issue ReadIssue) {
			transitions = append(transitions, issue)
		},
		base: roundTrip(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() == liveRead {
				calls++
				switch calls {
				case 1, 2, 3, 4:
					return nil, context.DeadlineExceeded
				case 5:
					deadline, ok := req.Context().Deadline()
					if !ok || time.Until(deadline) > probeTimeout {
						t.Error("probe did not have its own short deadline")
					}
					close(probeStarted)
					<-probeRelease
				}
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}),
	}
	read := func(ctx context.Context, url string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			return err
		}
		_, err = io.ReadAll(resp.Body)
		return errors.Join(err, resp.Body.Close())
	}
	first := WithReadCycle(t.Context())
	for range 2 {
		if err := read(first, liveRead); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first refresh: %v", err)
		}
	}
	if issues := transport.issues(); len(issues) != 1 || !issues[0].RetryAt.IsZero() {
		t.Fatalf("one refresh must not open backoff: %+v", issues)
	}
	if err := read(WithReadCycle(t.Context()), liveRead); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second refresh: %v", err)
	}
	if issues := transport.issues(); len(issues) != 1 || issues[0].Reason != "request timed out" || !issues[0].RetryAt.Equal(now.Add(firstPause)) {
		t.Fatalf("expected timed-out endpoint to pause: %+v", issues)
	}
	if err := read(WithReadCycle(t.Context()), liveRead); err == nil || calls != 3 {
		t.Fatalf("paused read reached the endpoint: %v, calls=%d", err, calls)
	}
	for _, url := range []string{archiveRead, strings.Replace(liveRead, "api.example.", "api.other.", 1), "https://api.github.com/repos/stackrox/fact"} {
		if err := read(WithReadCycle(t.Context()), url); err != nil {
			t.Fatalf("unaffected read failed for %s: %v", url, err)
		}
	}
	now = now.Add(firstPause)
	if err := read(WithReadCycle(t.Context()), liveRead); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first probe: %v", err)
	}
	if issues := transport.issues(); len(issues) != 1 || !issues[0].RetryAt.Equal(now.Add(2*firstPause)) {
		t.Fatalf("failed probe should extend backoff: %+v", issues)
	}
	now = now.Add(2 * firstPause)
	done := make(chan error, 1)
	go func() { done <- read(WithReadCycle(t.Context()), liveRead) }()
	select {
	case <-probeStarted:
	case err := <-done:
		t.Fatalf("recovery probe did not reach the endpoint: %v", err)
	}
	err := read(WithReadCycle(t.Context()), liveRead)
	issues := transport.issues()
	close(probeRelease)
	if probeErr := <-done; probeErr != nil {
		t.Fatal(probeErr)
	}
	if err == nil || calls != 5 || len(issues) != 1 || !issues[0].Probing {
		t.Fatalf("expected a single in-flight probe: %v calls=%d issues=%+v", err, calls, issues)
	}
	if issues := transport.issues(); len(issues) != 0 {
		t.Fatalf("successful probe should clear the issue: %+v", issues)
	}
	if err := read(WithReadCycle(t.Context()), liveRead); err != nil || calls != 6 {
		t.Fatalf("normal requests should resume: %v calls=%d", err, calls)
	}
	if len(transitions) != 2 || !transitions[0].RetryAt.Equal(now.Add(-2*firstPause)) || !transitions[1].RetryAt.IsZero() {
		t.Fatalf("wanted one pause with the first probe time and one recovery, got %+v", transitions)
	}
}

func TestReadBackoffReportsUnauthorizedUntilSuccessfulRead(t *testing.T) {
	now := time.Now()
	status := http.StatusUnauthorized
	transport := &readBackoff{
		now:       func() time.Time { return now },
		endpoints: make(map[string]*endpointBackoff),
		base: roundTrip(func(*http.Request) (*http.Response, error) {
			if status == 0 {
				return nil, context.DeadlineExceeded
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("{}"))}, nil
		}),
	}
	readArchive := func() error {
		t.Helper()
		now = now.Add(time.Second)
		req, err := http.NewRequestWithContext(WithReadCycle(t.Context()), http.MethodGet, archiveRead, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := transport.RoundTrip(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != status {
			t.Fatalf("got HTTP %d, want HTTP %d", resp.StatusCode, status)
		}
		return resp.Body.Close()
	}
	for range 2 {
		if err := readArchive(); err != nil {
			t.Fatal(err)
		}
		if issues := transport.issues(); len(issues) != 1 || !issues[0].Unauthorized || issues[0].Endpoint != "Tekton Results" ||
			issues[0].Reason != "HTTP 401 Unauthorized" || !issues[0].RetryAt.IsZero() || !issues[0].LastFailure.Equal(now) {
			t.Fatalf("expected authorization failure without backoff: %+v", issues)
		}
	}
	status = 0
	if err := readArchive(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a timeout, got %v", err)
	}
	if issues := transport.issues(); len(issues) != 2 {
		t.Fatalf("expected the timeout and the recent authorization failure: %+v", issues)
	}
	status = http.StatusOK
	if err := readArchive(); err != nil {
		t.Fatal(err)
	}
	if issues := transport.issues(); len(issues) != 0 {
		t.Fatalf("successful read should clear both issues: %+v", issues)
	}
}

func TestAdapterDefersLiveLookupsButStillReadsGitHubAndArchive(t *testing.T) {
	const apiAuthority = "api.example.openshiftapps.com:6443"
	lookups := Lookups{}
	if err := lookups.Add("296509,api.example.openshiftapps.com,builds"); err != nil {
		t.Fatal(err)
	}
	config := startupConfig(t.Context(), lookups, func(_ context.Context, _ string, args ...string) string {
		if args[1] == "--show-server" {
			return "https://" + apiAuthority
		}
		return "cluster-fixture"
	})
	config.GitHubToken = "github-fixture"
	var liveCalls, archiveCalls, checkReads int
	now := time.Now()
	config.HTTPClient = &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
		body := ""
		status := http.StatusOK
		switch req.URL.Host {
		case apiAuthority:
			if req.Host != apiAuthority || req.Header.Get("Authorization") != "Bearer cluster-fixture" {
				t.Fatal("live read missing the configured authority or cluster authentication")
			}
			liveCalls++
			if liveCalls < 3 {
				status = http.StatusServiceUnavailable
			} else {
				body = `{"metadata":{"name":"run","namespace":"builds","annotations":{
					"pipelinesascode.tekton.dev/check-run-id":"17",
					"pipelinesascode.tekton.dev/sha":"head",
					"pipelinesascode.tekton.dev/repo-url":"https://github.com/stackrox/fact",
					"pipelinesascode.tekton.dev/event-type":"push",
					"pipelinesascode.tekton.dev/branch":"refs/tags/v0.3.77"
				}}}`
			}
		case "tekton-results-tekton-results.apps.example.openshiftapps.com":
			archiveCalls++
			body = `{"records":[]}`
		case "api.github.com":
			body = `{"sha":"head"}`
			if strings.HasSuffix(req.URL.Path, "/check-runs") {
				checkReads++
				body = `{"check_runs":[{"id":17,"name":"Red Hat Konflux / fact-on-push","app":{"id":296509},"status":"completed","conclusion":"failure","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}]}`
			}
		default:
			t.Fatalf("unexpected request %s", req.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	a := New(config)
	a.reads.now = func() time.Time { return now }
	target := release.Target{Repo: "stackrox/fact", Tag: "v0.3.77"}
	for i := range 3 {
		plan, err := a.Plan(t.Context(), target)
		if err == nil || plan.SHA != "head" || len(plan.Runs) != 1 || plan.Runs[0].ID != "" || plan.Runs[0].CheckID != 17 || len(plan.Proposals) != 0 {
			t.Fatalf("unverified lookup %d unexpectedly produced verified evidence: %+v, %v", i, plan, err)
		}
		if !plan.KonfluxAPIUnavailable {
			t.Fatal("missing target API outage")
		}
		if i == 2 && !strings.Contains(plan.Evidence.Error, "deferred by endpoint backoff") {
			t.Fatalf("deferred read not explained: %s", plan.Evidence.Error)
		}
	}
	if liveCalls != 2 || archiveCalls != 3 || checkReads != 3 {
		t.Fatalf("expected only live reads to back off: live=%d archive=%d checks=%d", liveCalls, archiveCalls, checkReads)
	}
	now = now.Add(firstPause)
	plan, err := a.Plan(t.Context(), target)
	if err != nil || len(plan.Runs) != 1 || plan.Runs[0].ID != "17" || len(a.ReadIssues()) != 0 {
		t.Fatalf("recovery probe did not verify the live PipelineRun: %+v, %v", plan, err)
	}
	if liveCalls != 3 || archiveCalls != 3 {
		t.Fatalf("expected the recovery probe to use only the live API: live=%d archive=%d", liveCalls, archiveCalls)
	}
}
