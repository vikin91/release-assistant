package release

import (
	"reflect"
	"testing"
	"time"
)

func TestRetryPlanPreservesCIReport(t *testing.T) {
	target := Target{Repo: "stackrox/stackrox", PR: 7}
	report := ManualRetryReport{
		Target: target, SHA: "head", At: time.Now(), URL: "pr-url",
		Warnings: []string{"partial result"}, OtherChecks: []string{"unclassified check"},
		Jobs:       []ManualJob{{ID: 12, Name: "unit-test", URL: "job-url", WorkflowName: "CI"}},
		Prow:       []ProwRetry{{Context: "ci/prow/e2e", Command: "/test e2e", URL: "prow-url"}},
		Incomplete: true,
	}
	plan := report.RetryPlan()
	if len(plan.Comments) != 0 || plan.Evidence.Known {
		t.Fatalf("partial CI result must carry only verified manual offers: %+v", plan)
	}
	if got := plan.ManualReport(); !reflect.DeepEqual(got, report) {
		t.Fatalf("report = %+v, want %+v", got, report)
	}
}

func TestRetryPlanPreservesKonfluxEvidence(t *testing.T) {
	target := Target{Repo: "stackrox/stackrox", PR: 7}
	now := time.Now()
	plan := Plan{
		Target: target, SHA: "head", Action: "Retest", Message: "Retry available",
		Evidence:      Evidence{At: now, URL: "pr-url", Error: "partial result"},
		Warnings:      []string{"partial result"},
		Runs:          []Run{{ID: "run-12", Job: "build", Eligible: true, Image: Image{Reference: "image"}}},
		Proposals:     []Proposal{{Target: target, SHA: "head", RunID: "run-12", Job: "build", Payload: "/konflux-retest build", CommentFooter: "footer", At: now}},
		Advisories:    []Advisory{{ID: "cve", Message: "advisory"}},
		StartFailures: []StartFailure{{CheckID: 4, Job: "other", Reason: "cluster unavailable"}},
		Validation:    Validation{SHA: "head"}, KonfluxAPIUnavailable: true,
	}
	result := plan.RetryPlan()
	if len(result.Manual.Jobs)+len(result.Manual.Prow) != 0 {
		t.Fatalf("Konflux result must carry only comment proposals: %+v", result)
	}
	if got := result.KonfluxPlan(); !reflect.DeepEqual(got, plan) {
		t.Fatalf("plan = %+v, want %+v", got, plan)
	}
}
