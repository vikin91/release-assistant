package app

import (
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestRetainedBuildObservationStaysDisplayOnly(t *testing.T) {
	now := time.Now()
	previous := release.Plan{
		SHA: "head", Evidence: release.Evidence{At: now.Add(-time.Minute), Known: true},
		Runs: []release.Run{{CheckID: 17, Job: "main-on-push", ID: "17", Status: "completed", Outcome: "failure", Eligible: true}},
	}
	current := release.Plan{
		SHA: "head", Evidence: release.Evidence{At: now, Error: "reads deferred"},
		Runs: []release.Run{
			{CheckID: 17, Job: "main-on-push", Status: "unknown"},
			{CheckID: 18, Job: "main-on-push", Status: "unknown"},
		},
	}
	want := release.BuildObservation{Status: "completed", Outcome: "failure", At: previous.Evidence.At}
	retainVerifiedBuilds(previous, &current)
	assertObservation := func(plan release.Plan) {
		t.Helper()
		run := plan.Runs[0]
		if run.LastVerified != want || run.ID != "" || run.Status != "unknown" || run.Eligible || len(plan.Proposals) != 0 {
			t.Fatalf("retained evidence changed current authority or observation: %+v", plan)
		}
		if !plan.Runs[1].LastVerified.At.IsZero() {
			t.Fatalf("retained evidence attached to a different check: %+v", plan)
		}
	}
	assertObservation(current)
	next := clonePlan(current)
	next.Runs[0].LastVerified = release.BuildObservation{}
	retainVerifiedBuilds(current, &next)
	assertObservation(next)
	next.SHA = "new-head"
	next.Runs[0].LastVerified = release.BuildObservation{}
	retainVerifiedBuilds(current, &next)
	if !next.Runs[0].LastVerified.At.IsZero() {
		t.Fatal("retained evidence crossed a commit boundary")
	}
}
