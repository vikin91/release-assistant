package app

import (
	"context"
	"reflect"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type writeCounter struct{ calls int }

func (w *writeCounter) Write(context.Context, release.Proposal) (dispatch.Delivery, error) {
	w.calls++
	return dispatch.Delivery{Status: "sent"}, nil
}

func TestAdvisoryReviewIsSessionLocalMetadata(t *testing.T) {
	s, f := newFixture()
	writer := &writeCounter{}
	s.dispatcher = dispatch.New(f, f, writer, dispatch.Options{EnableWrites: true})

	target := release.Target{Repo: "stackrox/fact", PR: 7}
	sha := "reviewed-head"
	id := "conforma-policy"
	proposal := release.Proposal{
		Target:  target,
		SHA:     sha,
		Job:     "unit",
		RunID:   "failed-run",
		Payload: "/konflux-retest unit",
	}
	plan := release.Plan{
		Target:     target,
		SHA:        sha,
		Runs:       []release.Run{{Job: "unit", ID: "failed-run", Status: "completed", Outcome: "failure", Eligible: true}},
		Proposals:  []release.Proposal{proposal},
		Advisories: []release.Advisory{{ID: id, Message: "Policy review required", URL: "https://example.com/advisory"}},
		Evidence:   observed(),
	}
	s.config = configuration()
	s.repos["fact"] = release.Repository{
		Source:   s.config.Components[0],
		Head:     sha,
		Evidence: observed(),
		PRs: []release.PR{{
			Target:         target,
			SHA:            sha,
			State:          "open",
			Base:           s.config.Components[0].Branch,
			Evidence:       observed(),
			Checks:         []release.Check{{Name: "unit", Status: "completed", Conclusion: "failure", Required: release.Yes}},
			ChecksEvidence: observed(),
		}},
	}
	s.plans[target] = plan
	s.choices.PRs[target] = release.Included
	s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target, SHA: sha, Open: true, Evidence: observed()})
	if err := s.dispatcher.Grant(target, sha, true); err != nil {
		t.Fatal(err)
	}
	s.dispatcher.ObservePlan(plan)
	s.drain()

	before := s.Snapshot()
	if len(before.Authority) != 1 || !before.Authority[0].Granted || len(before.Authority[0].Offers) != 1 || !before.Authority[0].Offers[0].CanApprove {
		t.Fatalf("fixture lacks an available retry offer and grant: %+v", before.Authority)
	}
	if len(before.Plans) != 1 || len(before.Plans[0].Advisories) != 1 || len(before.Plans[0].Runs) != 1 || before.Plans[0].Runs[0].Outcome != "failure" {
		t.Fatalf("fixture lacks failing plan and advisory: %+v", before.Plans)
	}

	for _, enabled := range []bool{true, false} {
		apply(t, s, Action{Kind: "acknowledge", Target: target, SHA: sha, ID: id, Enabled: enabled})
		after := s.Snapshot()
		if got := after.Choices.Acknowledged[id]; got != enabled {
			t.Fatalf("acknowledgement = %t, want %t", got, enabled)
		}
		if !reflect.DeepEqual(before.Choices.PRs, after.Choices.PRs) || !reflect.DeepEqual(before.Choices.Tags, after.Choices.Tags) {
			t.Fatal("advisory review changed release selection choices")
		}
		if !reflect.DeepEqual(before.Assessment, after.Assessment) {
			t.Fatalf("advisory review changed release assessment: before=%+v after=%+v", before.Assessment, after.Assessment)
		}
		if !reflect.DeepEqual(before.Repositories[0].PRs[0].Checks, after.Repositories[0].PRs[0].Checks) || !reflect.DeepEqual(before.Repositories[0].PRs[0].ChecksEvidence, after.Repositories[0].PRs[0].ChecksEvidence) {
			t.Fatal("advisory review changed observed check results")
		}
		if !reflect.DeepEqual(before.Plans, after.Plans) {
			t.Fatalf("advisory review changed plan evidence: before=%+v after=%+v", before.Plans, after.Plans)
		}
		if before.Interactive != after.Interactive || !reflect.DeepEqual(before.Authority, after.Authority) {
			t.Fatalf("advisory review changed write authority or retry offers: before=%+v after=%+v", before.Authority, after.Authority)
		}
		after.Choices.Acknowledged = before.Choices.Acknowledged
		after.LogEntries = before.LogEntries
		if !reflect.DeepEqual(before, after) {
			t.Fatal("advisory review changed snapshot state outside its acknowledgement metadata")
		}
		if writer.calls != 0 {
			t.Fatalf("advisory review invoked writer %d times", writer.calls)
		}
	}

	records := s.log.Recent()[len(before.LogEntries):]
	if len(records) != 2 {
		t.Fatalf("got %d audit records, want one for each review action", len(records))
	}
	for i, enabled := range []bool{true, false} {
		record := records[i]
		wantResult := "false"
		if enabled {
			wantResult = "true"
		}
		if record.Decision != "conforma-acknowledged" || record.Result != wantResult || record.Origin != "user" || record.Target != target || record.SHA != sha {
			t.Fatalf("unexpected audit record for enabled=%t: %+v", enabled, record)
		}
	}

	fresh, _ := newFixture()
	if len(fresh.Snapshot().Choices.Acknowledged) != 0 {
		t.Fatal("fresh session inherited advisory review metadata")
	}
}
