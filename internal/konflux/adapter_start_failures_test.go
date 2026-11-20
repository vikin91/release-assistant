package konflux

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/release"

	retry "github.com/vikin91/konflux-retest"
)

func TestStartFailureConversionPreservesCheckEvidenceWithoutRunIdentity(t *testing.T) {
	for name, partial := range map[string]bool{"complete result": false, "partial result": true} {
		t.Run(name, func(t *testing.T) {
			a := New(Config{GitHubToken: "private-github", QuayPassword: "private-quay"})
			a.tag = func(context.Context, retry.Config, string, string) (retry.Result, error) {
				result := retry.Result{SHA: "commit", Action: retry.ActionWait,
					StartFailures: []retry.StartFailure{{CheckID: 42, Job: "fact-on-push", Cluster: "example.openshiftapps.com", Category: retry.StartFailureAdmissionPolicy, Reason: "denied private-github private-quay"}},
					Runs:          []retry.Run{{ID: 42, Job: "fact-on-push", Status: "completed", Conclusion: "failure"}},
				}
				if partial {
					return result, errors.New("unrelated run identity unavailable")
				}
				return result, nil
			}
			plan, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/fact", Tag: "0.2.18"})
			if (err != nil) != partial || len(plan.StartFailures) != 1 || len(plan.Proposals) != 0 || plan.Runs[0].ID != "" {
				t.Fatalf("creation failure acquired retry identity: plan=%+v err=%v", plan, err)
			}
			failure := plan.StartFailures[0]
			if failure.CheckID != 42 || failure.Job != "fact-on-push" || failure.Cluster != "example.openshiftapps.com" || failure.Category != "admission_policy" || failure.Reason != "denied [redacted] [redacted]" || failure.Superseded || !failure.Evidence.Fresh() || !failure.Evidence.At.Equal(plan.Evidence.At) || failure.Evidence.URL != "https://github.com/stackrox/fact/runs/42" {
				t.Fatalf("lost typed commit check observation: %+v", failure)
			}
		})
	}
}

func TestNewerVerifiedJobMarksEarlierCreationFailureSuperseded(t *testing.T) {
	for name, tc := range map[string]struct {
		run            retry.Run
		wantSuperseded bool
	}{
		"verified newer attempt": {retry.Run{Job: "fact-on-push", ID: 43, IdentityKnown: true}, true},
		"unverified newer check": {retry.Run{Job: "fact-on-push", ID: 43}, false},
		"different job":          {retry.Run{Job: "collector-on-push", ID: 43, IdentityKnown: true}, false},
		"same check":             {retry.Run{Job: "fact-on-push", ID: 42, IdentityKnown: true}, false},
		"older verified check":   {retry.Run{Job: "fact-on-push", ID: 41, IdentityKnown: true}, false},
	} {
		t.Run(name, func(t *testing.T) {
			a := New(Config{})
			a.pr = func(context.Context, retry.Config, string, int) (retry.Result, error) {
				return retry.Result{SHA: "commit", StartFailures: []retry.StartFailure{{CheckID: 42, Job: "fact-on-push", Category: retry.StartFailureOther}}, Runs: []retry.Run{tc.run}}, nil
			}
			plan, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/fact", PR: 3})
			if err != nil || len(plan.StartFailures) != 1 || plan.StartFailures[0].Superseded != tc.wantSuperseded || len(plan.Proposals) != 0 {
				t.Fatalf("wrong supersession: plan=%+v err=%v", plan, err)
			}
		})
	}
}

func TestPublicAdapterPreservesCreationFailuresAndConformaAdvisories(t *testing.T) {
	for name, target := range map[string]release.Target{
		"PR":  {Repo: "stackrox/fact", PR: 3},
		"tag": {Repo: "stackrox/fact", Tag: "0.2.18"},
	} {
		t.Run(name, func(t *testing.T) {
			var paths []string
			startedAt := time.Date(2026, time.November, 18, 9, 43, 15, 0, time.UTC)
			transport := roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.Host != "api.github.com" {
					t.Fatalf("creation failure or developer advisory triggered unrelated read: %s %s", req.Method, req.URL)
				}
				paths = append(paths, req.URL.Path)
				var value any
				switch {
				case strings.HasSuffix(req.URL.Path, "/pulls/3"):
					value = map[string]any{"state": "open", "head": map[string]string{"sha": "commit"}}
				case strings.HasSuffix(req.URL.Path, "/commits/refs/tags/0.2.18"):
					value = map[string]string{"sha": "commit"}
				case strings.HasSuffix(req.URL.Path, "/check-runs"):
					value = map[string]any{"check_runs": []any{
						map[string]any{
							"id": 42, "name": "Red Hat Konflux / fact-on-push", "app": map[string]int{"id": 296509}, "status": "completed", "conclusion": "failure",
							"details_url": "https://konflux-ui.apps.example.openshiftapps.com/",
							"external_id": "fact-on-push-private-token", "started_at": startedAt,
							"output": map[string]string{
								"title": "pipelinerun start failure", "text": "ValidatingAdmissionPolicy denied request; token=private-token password=private-quay",
							},
						},
						map[string]any{"id": 43, "name": "Red Hat Konflux / operator-on-push", "app": map[string]int{"id": 296509}, "status": "completed", "conclusion": "startup_failure", "details_url": "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelineruns", "output": map[string]string{"title": "pipelinerun start failure", "text": "PipelineRun quota exceeded"}},
						map[string]any{"id": 44, "name": "Red Hat Konflux / acs-conforma-dev-4-10 / fact-4-10", "app": map[string]int{"id": 296509}, "status": "completed", "conclusion": "failure", "details_url": "https://konflux-ui.apps.example.openshiftapps.com/ns/builds/pipelinerun/conforma-dev"},
					}}
				default:
					t.Fatalf("unexpected request %s", req.URL)
				}
				data, _ := json.Marshal(value)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data))), Header: http.Header{}}, nil
			})
			a := New(Config{GitHubToken: "private-token", QuayPassword: "private-quay", HTTPClient: &http.Client{Transport: transport}, KonfluxToken: func(context.Context, string) (string, error) {
				t.Fatal("creation failure or developer Conforma requested PipelineRun identity")
				return "", nil
			}})
			plan, err := a.Plan(t.Context(), target)
			if err != nil || !plan.Evidence.Fresh() || len(plan.StartFailures) != 2 || len(plan.Advisories) != 1 || len(plan.Proposals) != 0 || len(paths) != 2 {
				t.Fatalf("normal observations became read failures or retries: plan=%+v err=%v requests=%v", plan, err, paths)
			}
			if !slices.EqualFunc(plan.StartFailures, []release.StartFailure{{CheckID: 42, Category: "admission_policy"}, {CheckID: 43, Category: "other"}}, func(a, b release.StartFailure) bool { return a.CheckID == b.CheckID && a.Category == b.Category }) {
				t.Fatalf("typed failure categories were lost: %+v", plan.StartFailures)
			}
			if failure := plan.StartFailures[0]; failure.ExternalID != "fact-on-push-[redacted]" || !failure.CheckStartedAt.Equal(startedAt) {
				t.Fatalf("GitHub check metadata was lost or not redacted: %+v", failure)
			}
			for _, run := range plan.Runs {
				if run.ID != "" || run.Eligible {
					t.Fatalf("unverified creation check obtained retry authority: %+v", run)
				}
			}
			if !strings.Contains(plan.Advisories[0].Message, "acs-conforma-dev-4-10") || strings.Contains(plan.StartFailures[0].Reason, "private-") {
				t.Fatalf("lost advisory or redaction: %+v", plan)
			}
		})
	}
}

func TestCreationFailureDoesNotSuppressIndependentEligibleRetry(t *testing.T) {
	a := New(Config{})
	a.pr = func(context.Context, retry.Config, string, int) (retry.Result, error) {
		return retry.Result{SHA: "commit", Action: retry.ActionRetest, Jobs: []string{"checks"},
			StartFailures: []retry.StartFailure{{CheckID: 42, Job: "main-on-push", Category: retry.StartFailureAdmissionPolicy}},
			Runs: []retry.Run{
				{Job: "main-on-push", ID: 42, Status: "completed", Conclusion: "failure"},
				{Job: "checks", ID: 43, Status: "completed", Conclusion: "failure", IdentityKnown: true, Eligible: true},
			},
		}, errors.New("unrelated input unavailable")
	}
	plan, err := a.Plan(t.Context(), release.Target{Repo: "stackrox/stackrox", PR: 3})
	if err == nil || len(plan.StartFailures) != 1 || !plan.StartFailures[0].Evidence.Fresh() || len(plan.Proposals) != 1 || plan.Proposals[0].Job != "checks" || plan.Proposals[0].RunID != "43" || plan.Proposals[0].Payload != "/konflux-retest checks" || plan.Runs[0].ID != "" {
		t.Fatalf("creation diagnostics changed independent library retry: plan=%+v err=%v", plan, err)
	}
}
