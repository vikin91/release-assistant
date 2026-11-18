package web

import (
	"strings"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestRCValidationShowsExceptionsAndReusesExactTagRetest(t *testing.T) {
	s := workflowStatusSnapshot()
	target := s.Config.RC0Target()
	sha := "candidate-sha"
	proposal := release.Proposal{
		Target: target, SHA: sha, Job: "checks", RunID: "18", Payload: "/retest checks tag:" + target.Tag,
	}
	s.Plans = []release.Plan{{
		Target: target, SHA: sha, Evidence: s.Inventory.Evidence,
		Runs: []release.Run{
			{Job: "checks", ID: "18", Status: "completed", Outcome: "cancelled", Eligible: true,
				URL: "https://konflux.example.com/checks"},
		},
		Proposals: []release.Proposal{proposal},
		Validation: release.Validation{
			SHA: sha, Evidence: s.Inventory.Evidence,
			Checks: []release.ValidationCheck{
				{ID: 11, App: "github-actions", Event: "push", Ref: target.Tag,
					WorkflowURL: "https://github.com/stackrox/stackrox/actions/runs/101",
					Check:       release.Check{Name: "ACS upgrade", Conclusion: "failure"}},
				{ID: 12, App: "github-actions", Event: "push", Ref: "release-5.0",
					Check: release.Check{Name: "gke-qa-e2e-tests-konflux", Status: "in_progress"}},
				{ID: 13, Check: release.Check{Name: "Red Hat Konflux / acs-conforma-prod-like / main", Conclusion: "failure"}},
				{ID: 14, Check: release.Check{Name: "Red Hat Konflux / acs-conforma-prod / main", Status: "queued"}},
				{ID: 15, Check: release.Check{Name: "Red Hat Konflux / acs-conforma-dev-5-0 / main", Conclusion: "failure"}},
				{ID: 16, Check: release.Check{Name: "passed-test-hidden", Conclusion: "success"}},
				{ID: 17, Check: release.Check{Name: "skipped-test-hidden", Conclusion: "skipped"}},
				{ID: 18, Check: release.Check{Name: "Red Hat Konflux / checks", Conclusion: "cancelled"}},
			},
		},
	}}
	assessWorkflow(&s)
	assertTabStatus(t, s, "acs-rcs", tabNeedsAttention, "An RC validation check failed or was canceled")

	for name, mode := range map[string]struct{ writes, interactive bool }{
		"read-only": {}, "interactive": {true, true}, "automatic": {true, false},
	} {
		t.Run(name, func(t *testing.T) {
			s := s
			s.WritesEnabled, s.Interactive = mode.writes, mode.interactive
			if mode.writes {
				s.Authority = []dispatch.View{{
					Target: target, SHA: sha, Granted: true,
					Offers: []dispatch.Offer{{ID: dispatch.ID(proposal), Proposal: proposal, CanApprove: true}},
				}}
			}
			panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
			if strings.Index(panel, `id="release-images-panel"`) > strings.Index(panel, `id="rc-validation"`) {
				t.Fatal("the image summary should appear above validation when the images are available")
			}
			validation := between(t, panel, `id="rc-validation"`, "</section>")
			for _, want := range []string{
				"All checks on GitHub", "candidate-sha", "ACS upgrade", "Canceled", "Running",
				"gke-qa-e2e-tests-konflux", "push · release-5.0", "acs-conforma-prod-like", "acs-conforma-prod /",
				"Review policy results", "Rerun in GitHub Actions", "actions/runs/101", proposal.Payload,
				"Copy comment", "Tag push · " + target.Tag,
			} {
				if !strings.Contains(validation, want) {
					t.Errorf("missing %q in validation", want)
				}
			}
			for _, hidden := range []string{"passed-test-hidden", "skipped-test-hidden", "acs-conforma-dev-5-0"} {
				if strings.Contains(validation, hidden) {
					t.Errorf("unexpected %q in validation", hidden)
				}
			}
			if strings.Count(validation, proposal.Payload) != 1 ||
				strings.Index(validation, "Canceled") > strings.Index(validation, "gke-qa-e2e-tests-konflux") {
				t.Error("show the exact tag retry once and place failures and cancellations first")
			}
			if strings.Contains(validation, `value="send"`) != mode.interactive {
				t.Error("only interactive mode should render a manual send form")
			}
			if strings.Contains(validation, `action="/action#rc-validation"`) != mode.writes {
				t.Error("tag permission should return to validation")
			}
		})
	}
}

func TestRCValidationRemainsVisibleWhileWaitingForEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		sha, want string
		observed  bool
	}{
		"tag unresolved":  {want: "Waiting to verify the RC.0 tag."},
		"checks loading":  {sha: "candidate", want: "Waiting for GitHub check results."},
		"no checks found": {sha: "candidate", observed: true, want: "No validation checks have been reported for this commit yet."},
	} {
		t.Run(name, func(t *testing.T) {
			s := workflowStatusSnapshot()
			if tc.sha != "" {
				plan := release.Plan{Target: s.Config.RC0Target(), SHA: tc.sha}
				if tc.observed {
					plan.Validation = release.Validation{SHA: tc.sha, Evidence: s.Inventory.Evidence}
				}
				s.Plans = []release.Plan{plan}
			}
			assessWorkflow(&s)
			panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
			validation := between(t, panel, `id="rc-validation"`, "</section>")
			if !strings.Contains(validation, "Pending") || !strings.Contains(validation, tc.want) {
				t.Fatalf("missing pending status or explanation %q", tc.want)
			}
			if strings.Contains(validation, "All checks on GitHub") != (tc.sha != "") {
				t.Fatal("the checks link should appear once the candidate commit is known")
			}
		})
	}
}
