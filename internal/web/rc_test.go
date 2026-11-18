package web

import (
	"strings"
	"testing"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestRCTabShowsBuildActionsBesideMissingImages(t *testing.T) {
	s := workflowStatusSnapshot()
	target := s.Config.RC0Target()
	proposal := release.Proposal{
		Target: target, SHA: "rc-commit", Job: "main-on-push", RunID: "17",
		Payload: "/retest main-on-push tag:" + target.Tag,
	}
	s.Plans = []release.Plan{{
		Target: target, SHA: proposal.SHA, Evidence: s.Inventory.Evidence,
		Runs: []release.Run{
			{Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "failure",
				Eligible: true, URL: "https://konflux.example.com/main-17"},
			{Job: "roxctl-on-push", ID: "18", Status: "in_progress", URL: "https://konflux.example.com/roxctl-18"},
			{Job: "scanner-v4-on-push", ID: "19", Status: "completed", Outcome: "success",
				URL: "https://konflux.example.com/scanner-19"},
		},
		Proposals: []release.Proposal{proposal},
	}}
	for i := range s.Inventory.ReleaseImages {
		if s.Inventory.ReleaseImages[i].Family == "konflux" {
			s.Inventory.ReleaseImages[i].Present = release.No
		}
	}
	assessWorkflow(&s)
	assertTabStatus(t, s, "acs-rcs", tabActionNeeded, "can be retested")

	for name, writes := range map[string]bool{"read-only": false, "writes allowed": true} {
		t.Run(name, func(t *testing.T) {
			s := s
			s.WritesEnabled, s.Interactive = writes, true
			if writes {
				s.Authority = []dispatch.View{{
					Target: target, SHA: proposal.SHA, Granted: true,
					Offers: []dispatch.Offer{{ID: dispatch.ID(proposal), Proposal: proposal, CanApprove: true}},
				}}
			}
			body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
			panel := activityPanel(t, body, "acs-rcs")
			for _, want := range []string{"3 Konflux images missing", "Open RC.0 tag", "View tag checks"} {
				if !strings.Contains(panel, want) {
					t.Errorf("missing %q", want)
				}
			}
			main := between(t, panel, "<code>main</code></th>", "</tr>")
			for _, want := range []string{"Build failed", "Open in Konflux", proposal.Payload, "Copy comment"} {
				if !strings.Contains(main, want) {
					t.Errorf("missing %q in main image row", want)
				}
			}
			if writes {
				if !strings.Contains(main, "Retest Konflux build") || !strings.Contains(main, "I confirm this exact comment") {
					t.Error("authorized row is missing an exact comment confirmation")
				}
			} else if !strings.Contains(main, "Copy retest command") || strings.Contains(main, `value="send"`) {
				t.Error("read-only row should offer a copyable command")
			}
			roxctl := between(t, panel, "<code>roxctl</code></th>", "</tr>")
			scanner := between(t, panel, "<code>scanner-v4</code></th>", "</tr>")
			if !strings.Contains(roxctl, "Build running") || !strings.Contains(scanner, "Check build output") {
				t.Error("running and successful build evidence is missing")
			}
			if strings.Contains(roxctl+scanner, "Copy retest command") || strings.Contains(roxctl+scanner, "Retest Konflux build") {
				t.Error("running or successful build unexpectedly offers a retry")
			}
			if strings.Contains(activityPanel(t, body, "acs-prs"), proposal.Payload) {
				t.Error("RC.0 retry crossed into the ACS PRs tab")
			}
		})
	}
}

func TestRCTabSeparatesMissingTagsFromAdditionalCheckTimeouts(t *testing.T) {
	s := workflowStatusSnapshot()
	target := s.Config.RC0Target()
	const message = "PipelineRun identity verification failed: retag-fact: context deadline exceeded"
	plan := release.Plan{
		Target: target, SHA: "rc-commit", Message: "No build retests needed.", Action: "None",
		Evidence: release.Evidence{At: s.Inventory.Evidence.At, Error: message},
		Runs:     []release.Run{{Job: "retag-fact", Status: "unknown", URL: "https://konflux.example.com/retag-fact"}},
	}
	for i := range s.Inventory.ReleaseImages {
		image := &s.Inventory.ReleaseImages[i]
		if image.Family != "konflux" {
			continue
		}
		image.Present = release.No
		image.Reference = "quay.io/rhacs-eng/release-" + image.Name + ":" + target.Tag + "-fast"
		image.Evidence.URL = "https://quay.io/repository/rhacs-eng/release-" + image.Name + "?tag=" + target.Tag + "-fast"
		plan.Runs = append(plan.Runs, release.Run{
			Job: image.Name + "-on-push", ID: image.Name, Status: "completed", Outcome: "success",
			URL: "https://konflux.example.com/" + image.Name,
		})
	}
	s.Plans = []release.Plan{plan}
	assessWorkflow(&s)

	render := func() string {
		return activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
	}
	panel := render()
	table := between(t, panel, "<table", "</table>")
	main := between(t, table, "<code>main</code></th>", "</tr>")
	for _, want := range []string{"Build succeeded", "Tag missing", "Check build output", "Open expected Quay tag", "Checked ", "<summary>Details</summary>"} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q in main image row", want)
		}
	}
	if !strings.Contains(panel, "All 3 CI images are available.") ||
		!strings.Contains(panel, "The corresponding Konflux builds succeeded, but their expected tags are missing in Quay.") ||
		strings.Contains(panel, plan.Message) {
		t.Error("missing focused image publication summary")
	}
	diagnostics := between(t, panel, `<details class="rc-diagnostics" data-fold-key="rc-build-diagnostics">`, "</details>")
	for _, want := range []string{"1 additional job could not be verified", "A Konflux lookup timed out", "Open check in Konflux", message} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("missing %q in collapsed diagnostics", want)
		}
	}
	if strings.Contains(table, "retag-fact") || strings.Count(panel, message) != 1 {
		t.Error("auxiliary check diagnostics leaked into the image rows")
	}

	s.Plans[0].Runs[1].ID, s.Plans[0].Runs[1].Status, s.Plans[0].Runs[1].Outcome = "", "unknown", ""
	panel = render()
	main = between(t, panel, "<code>main</code></th>", "</tr>")
	if !strings.Contains(main, "Build unverified") || !strings.Contains(main, "View unverified check") ||
		strings.Contains(panel, "The corresponding Konflux builds succeeded") {
		t.Error("an unverified image build must be identified in its row")
	}
}

func TestRCTabLinksReleaseWorkflowWhenTagCannotBeResolved(t *testing.T) {
	s := workflowStatusSnapshot()
	s.Plans = []release.Plan{{
		Target: s.Config.RC0Target(), Evidence: release.Evidence{Error: "GitHub API returned HTTP 404"},
	}}
	assessWorkflow(&s)
	body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
	panel := activityPanel(t, body, "acs-rcs")
	for _, want := range []string{"Unable to verify the RC.0 tag", "Open release workflow", "/actions/workflows/start-release.yml"} {
		if !strings.Contains(panel, want) {
			t.Errorf("missing %q", want)
		}
	}
}
