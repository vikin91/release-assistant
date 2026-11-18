package app

import (
	"bytes"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestIncludedPRLogsCheckStatusChanges(t *testing.T) {
	s, f := newFixture()
	s.config = configuration()
	source := s.config.Components[0]
	target := release.Target{Repo: source.Repo, PR: 7}
	s.choices.PRs[target] = release.Included
	var output bytes.Buffer
	s.log = NewLog(&output)
	pr := release.PR{
		Target: target, SHA: "pr-head", State: "open", Base: source.Branch,
		Evidence: observed(), ChecksEvidence: observed(), CheckResultsEvidence: observed(),
		Checks: []release.Check{{Required: release.Yes, Status: "in_progress"}},
	}
	refresh := func() {
		t.Helper()
		repo := f.repos[source.Name]
		repo.PRs = []release.PR{pr}
		f.repos[source.Name] = repo
		if err := s.refreshPlanned(t.Context(), s.generation, source, false); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	pr.Checks[0].Conclusion = "failure"
	refresh()
	refresh()
	pr.ChecksEvidence.Error = "required settings are unavailable"
	refresh()
	pr.ChecksEvidence.Error = ""
	pr.Checks[0].Conclusion = "success"
	refresh()
	for _, want := range []string{
		"Info: Required GitHub checks are pending for fact#7",
		"Warn: Required GitHub checks are failing for fact#7",
		"Warn: GitHub checks are failing for fact#7 (pull request head commit pr-head). Required checks could not be verified.",
		"Info: Required GitHub checks are passing for fact#7",
	} {
		if strings.Count(output.String(), want) != 1 {
			t.Fatalf("expected one occurrence of %q in\n%s", want, output.String())
		}
	}
}

func TestRCLogsValidationStatusChanges(t *testing.T) {
	s, _ := newFixture()
	target := configuration().RC0Target()
	var output bytes.Buffer
	s.log = NewLog(&output)
	plan := release.Plan{SHA: "rc-head", Validation: release.Validation{
		SHA: "rc-head", Evidence: observed(),
		Checks: []release.ValidationCheck{{Check: release.Check{Conclusion: "failure"}}},
	}}
	s.observeRCPlan(target, plan)
	s.observeRCPlan(target, plan)
	plan.Validation.Checks = []release.ValidationCheck{{Check: release.Check{Conclusion: "success"}}}
	s.observeRCPlan(target, plan)
	for _, want := range []string{
		"Warn: GitHub validation checks are failing for stackrox/5.0.1-rc.0",
		"Info: GitHub validation checks are passing for stackrox/5.0.1-rc.0",
	} {
		if strings.Count(output.String(), want) != 1 {
			t.Fatalf("expected one occurrence of %q in\n%s", want, output.String())
		}
	}
}
