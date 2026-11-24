package ciretest

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	retry "github.com/vikin91/ci-retest"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func TestPlannerUsesSharedReadBudget(t *testing.T) {
	const repo = "/repos/stackrox/stackrox"
	responses := map[string]string{
		repo + "/pulls/7":         `{"number":7,"state":"open","base":{"ref":"master","repo":{"id":1}},"head":{"sha":"head","repo":{"id":1}}}`,
		repo + "/branches/master": `{"protection":{"required_status_checks":{"enforcement_level":"everyone"}}}`,
		repo + "/branches/master/protection/required_status_checks": `{"contexts":["unit-tests"],"checks":[]}`,
		repo + "/rules/branches/master":                             `[]`,
		repo + "/commits/head/statuses":                             `[]`,
		repo + "/commits/head/check-runs":                           `{"total_count":0,"check_runs":[]}`,
	}
	target := release.Target{Repo: "stackrox/stackrox", PR: 7}
	for name, limit := range map[string]int{"six reads for an open PR with no failed checks": 6, "defers the sixth read when the source budget runs out": 5} {
		t.Run(name, func(t *testing.T) {
			seen := make(map[string]bool)
			transport := githubread.New(roundTripFunc(func(req *http.Request) (*http.Response, error) {
				path := req.URL.Path
				body, ok := responses[path]
				if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer fixture-token" || !ok || seen[path] {
					t.Fatalf("unexpected request %s %s", req.Method, path)
				}
				seen[path] = true
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
			}))
			planner := New(retry.Config{GitHubToken: "fixture-token", HTTPClient: &http.Client{Transport: transport}})
			ctx, usage := githubread.Track(t.Context(), limit)
			plan, err := planner.PlanRetries(ctx, target)
			if usage.Calls() != int64(limit) || len(seen) != limit {
				t.Fatalf("read count = %d with %d distinct requests, want %d", usage.Calls(), len(seen), limit)
			}
			if limit == len(responses) {
				if err != nil {
					t.Fatal(err)
				}
			} else if deferred, ok := errors.AsType[*githubread.DeferredError](err); !ok || deferred.Until.IsZero() {
				t.Fatalf("expected a typed deferral with the next retry time, got %v", err)
			}
			if plan.Target != target || plan.SHA != "head" || len(plan.Comments)+len(plan.Manual.Jobs)+len(plan.Manual.Prow) != 0 {
				t.Fatalf("unexpected plan %+v", plan)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
