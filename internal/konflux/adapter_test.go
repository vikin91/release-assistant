package konflux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"release-assistant/internal/release"
	"strings"
	"testing"

	retry "github.com/vikin91/konflux-retest"
)

func TestConversion(t *testing.T) {
	const footer = "*posted from release-assistant v1.2.3*"
	for name, tc := range map[string]struct {
		target  release.Target
		result  retry.Result
		err     error
		payload string
	}{
		"PR":      {release.Target{Repo: "stackrox/fact", PR: 3}, retry.Result{SHA: "abc", Jobs: []string{"fact-on-push"}}, nil, "/konflux-retest fact-on-push"},
		"tag":     {release.Target{Repo: "stackrox/fact", Tag: "v0.3.77"}, retry.Result{SHA: "abc", Tag: "v0.3.77", Jobs: []string{"fact-on-push"}}, nil, "/retest fact-on-push tag:v0.3.77"},
		"partial": {release.Target{Repo: "stackrox/fact", PR: 3}, retry.Result{SHA: "abc", Jobs: []string{"fact-on-push"}, Warnings: []string{"incomplete input"}}, errors.New("partial"), "/konflux-retest fact-on-push"},
		"none":    {release.Target{Repo: "stackrox/fact", PR: 3}, retry.Result{Action: retry.ActionNone}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(Config{CommentFooter: footer})
			a.pr = func(context.Context, retry.Config, string, int) (retry.Result, error) { return tc.result, tc.err }
			a.tag = func(context.Context, retry.Config, string, string) (retry.Result, error) { return tc.result, tc.err }
			p, err := a.Plan(t.Context(), tc.target)
			if (err != nil) != (tc.err != nil) || p.SHA != tc.result.SHA || len(p.Warnings) != len(tc.result.Warnings) {
				t.Fatal(p, err)
			}
			if tc.payload != "" {
				if len(p.Proposals) != 1 || p.Proposals[0].Payload != tc.payload || p.Proposals[0].CommentFooter != footer {
					t.Fatalf("expected a copyable command and a comment footer, got %+v", p.Proposals)
				}
			}
			if tc.payload == "" && len(p.Proposals) != 0 {
				t.Fatal(p)
			}
		})
	}
}

func TestPublicAdapterSuppressesActiveRunsAndScopesTags(t *testing.T) {
	for name, tc := range map[string]struct {
		status, outcome, branch string
		want                    bool
	}{
		"failed tag":            {"completed", "failure", "refs/tags/v0.3.77", true},
		"cancelled":             {"completed", "cancelled", "refs/tags/v0.3.77", true},
		"timeout":               {"completed", "timed_out", "refs/tags/v0.3.77", true},
		"pending":               {"pending", "", "refs/tags/v0.3.77", false},
		"queued":                {"queued", "", "refs/tags/v0.3.77", false},
		"running":               {"in_progress", "", "refs/tags/v0.3.77", false},
		"successful":            {"completed", "success", "refs/tags/v0.3.77", false},
		"branch same commit":    {"completed", "failure", "release-0.3", false},
		"other tag same commit": {"completed", "failure", "refs/tags/v0.3.78", false},
	} {
		t.Run(name, func(t *testing.T) {
			transport := roundTrip(func(req *http.Request) (*http.Response, error) {
				var value any
				switch req.URL.Host {
				case "api.github.com":
					if strings.HasSuffix(req.URL.Path, "check-runs") {
						value = map[string]any{"check_runs": []any{map[string]any{"id": 17, "name": "Red Hat Konflux / fact-on-push", "app": map[string]int{"id": 296509}, "status": tc.status, "conclusion": tc.outcome, "details_url": "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"}}}
					} else {
						value = map[string]string{"sha": "head"}
					}
				case "api.example.openshiftapps.com":
					const prefix = "pipelinesascode.tekton.dev/"
					value = map[string]any{"metadata": map[string]any{"name": "run", "namespace": "builds", "annotations": map[string]string{prefix + "check-run-id": "17", prefix + "sha": "head", prefix + "repo-url": "https://github.com/stackrox/fact", prefix + "event-type": "push", prefix + "branch": tc.branch}}}
				default:
					t.Fatalf("unexpected request %s", req.URL)
				}
				data, _ := json.Marshal(value)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: http.Header{}}, nil
			})
			a := New(Config{HTTPClient: &http.Client{Transport: transport}, KonfluxToken: func(context.Context, string) (string, error) { return "fixture", nil }})
			p, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/fact", Tag: "v0.3.77"})
			if err != nil {
				t.Fatal(err)
			}
			if (len(p.Proposals) > 0) != tc.want {
				t.Fatal(p)
			}
			if tc.want && p.Proposals[0].RunID != "17" {
				t.Fatal("run identity missing", p)
			}
		})
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestImageOutputsStaySeparateFromRetryEligibility(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		url, digest string
		known       bool
		id          int64
		wantCurrent bool
	}{
		"verified tag output":    {"quay.io/rhacs-eng/release-main:build-fast", digest, true, 17, true},
		"verified digest output": {"quay.io/rhacs-eng/release-main@" + digest, digest, true, 17, true},
		"embedded digest only":   {"quay.io/rhacs-eng/release-main@" + digest, "", true, 17, true},
		"digest without repo":    {"", digest, true, 17, false},
		"unknown run identity":   {"quay.io/rhacs-eng/release-main:build-fast", digest, false, 17, false},
		"unknown stable run ID":  {"quay.io/rhacs-eng/release-main:build-fast", digest, true, 0, false},
		"missing digest":         {"quay.io/rhacs-eng/release-main:build-fast", "", true, 17, false},
		"invalid digest":         {"quay.io/rhacs-eng/release-main:build-fast", "sha256:short", true, 17, false},
		"untrusted image host":   {"https://example.com/private", digest, true, 17, false},
		"conflicting digest":     {"quay.io/rhacs-eng/release-main@sha256:" + strings.Repeat("b", 64), digest, true, 17, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(Config{})
			a.pr = func(context.Context, retry.Config, string, int) (retry.Result, error) {
				return retry.Result{SHA: "head", Jobs: []string{"main-on-push"}, Runs: []retry.Run{{ID: tc.id, Job: "main-on-push", Status: "completed", Conclusion: "failure", IdentityKnown: tc.known, Eligible: true, ImageURL: tc.url, ImageDigest: tc.digest}}}, errors.New("unrelated input observation unavailable")
			}
			plan, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/stackrox", PR: 123})
			if err == nil || len(plan.Proposals) != 1 || plan.Proposals[0].Payload != "/konflux-retest main-on-push" {
				t.Fatal("output observations changed library retry decision", plan, err)
			}
			image := plan.Runs[0].Image
			if image.Evidence.Fresh() != tc.wantCurrent || image.Present != "" {
				t.Fatalf("reported output was confused with registry availability: %+v", image)
			}
			if tc.wantCurrent && image.Reference != "quay.io/rhacs-eng/release-main@"+digest {
				t.Fatalf("output not bound to digest: %+v", image)
			}
			if tc.url == "" && image.Evidence.Error == "" {
				t.Fatalf("partial output has no explanation: %+v", image)
			}
			if (!tc.known || tc.id <= 0) && image.Reference != "" {
				t.Fatal("unverified output was exposed")
			}
		})
	}
}

func TestObservationFailuresExposeCauseWithoutCredentials(t *testing.T) {
	a := New(Config{GitHubToken: "private-github", QuayPassword: "private-quay"})
	a.pr = func(context.Context, retry.Config, string, int) (retry.Result, error) {
		return retry.Result{Warnings: []string{"Authentication failed private-github private-quay"}}, errors.New("GitHub API returned HTTP 401 private-github private-quay")
	}
	plan, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/stackrox", PR: 123})
	if err == nil || strings.Contains(err.Error(), "private-") || !strings.Contains(plan.Evidence.Error, "HTTP 401") || strings.Contains(plan.Evidence.Error, "private-") || strings.Contains(plan.Warnings[0], "private-") {
		t.Fatal("failure cause missing or credentials exposed", plan)
	}
}

func TestPublicAdapterPreservesActionableIdentityErrors(t *testing.T) {
	const checkURL = "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/main-17"
	transport := roundTrip(func(req *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, ""
		switch {
		case req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/pulls/24065"):
			body = `{"state":"open","head":{"sha":"head"}}`
		case req.URL.Host == "api.github.com" && strings.HasSuffix(req.URL.Path, "/check-runs"):
			data, _ := json.Marshal(map[string]any{"check_runs": []map[string]any{{"id": 17, "name": "Red Hat Konflux / main-on-push", "app": map[string]int{"id": 296509}, "status": "completed", "conclusion": "failure", "details_url": checkURL}}})
			body = string(data)
		case req.URL.Host == "api.example.openshiftapps.com":
			status, body = http.StatusForbidden, `{"message":"User builder cannot get pipelineruns; cluster-secret"}`
		case req.URL.Host == "tekton-results-tekton-results.apps.example.openshiftapps.com":
			status, body = http.StatusNotFound, `{"message":"archived PipelineRun main-17 was not found"}`
		default:
			t.Fatalf("unexpected request %s", req.URL)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	adapter := New(Config{HTTPClient: &http.Client{Transport: transport}, KonfluxToken: func(context.Context, string) (string, error) { return "cluster-secret", nil }})
	plan, err := adapter.Plan(t.Context(), release.Target{Repo: "stackrox/stackrox", PR: 24065})
	if err == nil || plan.Evidence.Fresh() || len(plan.Proposals) != 0 {
		t.Fatalf("identity failure authorized a retry: plan=%+v error=%v", plan, err)
	}
	for _, want := range []string{"identity verification failed for 1 check", `check "Red Hat Konflux / main-on-push" (run 17`, checkURL, "HTTP 403 (Forbidden)", "HTTP 404 (Not Found)"} {
		if !strings.Contains(plan.Evidence.Error, want) || !strings.Contains(err.Error(), want) {
			t.Fatalf("lost %q in %v", want, err)
		}
	}
	for _, unexpected := range []string{"cluster-secret", "User builder cannot get pipelineruns", "archived PipelineRun main-17 was not found"} {
		if strings.Contains(err.Error(), unexpected) || strings.Contains(strings.Join(plan.Warnings, "\n"), unexpected) {
			t.Fatalf("upstream response data leaked in diagnostics: %q", unexpected)
		}
	}
}

func TestIsUnsupportedRetagContract(t *testing.T) {
	contract := fmt.Errorf("%w: invalid .tekton/retag-pipeline.yaml", retry.ErrUnsupportedRetagContract)
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"joined duplicates in adapter diagnostic": {
			&observationError{message: contract.Error() + "\nSome Konflux reads were deferred by endpoint backoff.", cause: errors.Join(errors.Join(contract, contract))},
			true,
		},
		"a wrapped joined timeout still requires backoff": {fmt.Errorf("planning: %w", errors.Join(contract, context.DeadlineExceeded)), false},
		"an error with the same text":                     {errors.New(contract.Error()), false},
		"no error":                                        {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := IsUnsupportedRetagContract(tc.err); got != tc.want {
				t.Fatalf("IsUnsupportedRetagContract(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}

func TestPublicAdapterFiltersChecksBeforeStartupPolicy(t *testing.T) {
	for name, tc := range map[string]struct {
		appField       string
		wantPolicyCall bool
	}{
		"Konflux check in a denied namespace": {`,"app":{"id":296509}`, true},
		"check from another App":              {`,"app":{"id":42}`, false},
		"check without an App":                {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			lookups := make(Lookups)
			if err := lookups.Add("296509,api.example.openshiftapps.com,other-namespace"); err != nil {
				t.Fatal(err)
			}
			transport := roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					t.Errorf("denied lookup contacted %s", r.URL.Host)
					return nil, errors.New("unexpected lookup")
				}
				body := `{"state":"open","head":{"sha":"head"}}`
				if strings.HasSuffix(r.URL.Path, "/check-runs") {
					if r.URL.Query().Get("app_id") != "296509" {
						t.Error("check-runs request omitted the Konflux App filter")
					}
					body = `{"check_runs":[{"id":17,"name":"Red Hat Konflux / fact-on-push","status":"completed","conclusion":"failure","details_url":"https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/run"` + tc.appField + `}]}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			policyCalled := false
			a := New(Config{
				HTTPClient: &http.Client{Transport: transport},
				AllowKonfluxLookup: func(ctx context.Context, appID int64, host, namespace string) bool {
					policyCalled = true
					if appID != 296509 || host != "api.example.openshiftapps.com" || namespace != "builds" {
						t.Errorf("unexpected lookup scope: %d %s %s", appID, host, namespace)
					}
					return lookups.allows(ctx, appID, host, namespace)
				},
				KonfluxToken: func(context.Context, string) (string, error) {
					t.Error("denied lookup requested a token")
					return "", errors.New("unexpected token request")
				},
			})
			p, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/fact", PR: 1})
			if policyCalled != tc.wantPolicyCall || p.SHA != "head" || p.Action != "Wait" || len(p.Proposals) != 0 {
				t.Fatalf("unexpected lookup or plan: policyCalled=%t plan=%+v error=%v", policyCalled, p, err)
			}
			if !tc.wantPolicyCall {
				if err != nil || len(p.Runs) != 0 {
					t.Fatalf("check from another App became Konflux evidence: %+v (%v)", p, err)
				}
				return
			}
			if err == nil || p.Evidence.Fresh() || len(p.Warnings) == 0 || len(p.Runs) != 1 || p.Runs[0].Status != "unknown" || p.Runs[0].ID != "" || p.Runs[0].Eligible {
				t.Fatalf("denied run was lost or became retryable: %+v (%v)", p, err)
			}
		})
	}
}
