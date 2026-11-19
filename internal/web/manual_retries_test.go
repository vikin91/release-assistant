package web

import (
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestManualRetriesRenderCopyOnlyInTargetTab(t *testing.T) {
	s := workflowStatusSnapshot()
	pr := release.Target{Repo: s.Config.ACS().Repo, PR: 7}
	s.Choices.PRs[pr] = release.Included
	rc := s.Candidate().Target
	s.ManualRetries = []release.ManualRetryReport{
		{Target: pr, SHA: "reviewed", URL: "https://github.com/" + pr.Repo + "/pull/7", Prow: []release.ProwRetry{{Context: "ci/prow/test", Command: "/test test"}, {Context: "ci/prow/build", Command: "/test build"}}, Warnings: []string{"Actions read unavailable"}},
		{Target: rc, SHA: s.Candidate().SHA, Jobs: []release.ManualJob{{ID: 99, Name: "Konflux: release-main", WorkflowName: "Scan images for vulnerabilities", URL: "https://github.com/" + rc.Repo + "/actions/runs/22/job/99"}}},
	}
	for name, mode := range map[string]struct{ writes, interactive bool }{"read only": {false, true}, "writes with confirmation": {true, true}, "automatic writes": {true, false}} {
		t.Run(name, func(t *testing.T) {
			s.WritesEnabled, s.Interactive = mode.writes, mode.interactive
			f := &fakeApplication{snapshot: s}
			body := newBrowser(t, f).getBody(t)
			for tab, want := range map[string][]string{
				"acs-prs": {"ci/prow/test", "<pre><code>/test test\n/test build</code></pre>", s.ManualRetries[0].URL + "#new_comment_field", "Copy comment", "Actions read unavailable"},
				"acs-rcs": {"Scan images for vulnerabilities / Konflux: release-main", s.ManualRetries[1].Jobs[0].URL, "gh api --method POST repos/" + rc.Repo + "/actions/jobs/99/rerun", "Copy command"},
			} {
				start := `aria-label="Manual CI retries"`
				if tab == "acs-prs" {
					start = `id="acs-pr-retries-7"`
				}
				panel := between(t, activityPanel(t, body, tab), start, "</section>")
				for _, text := range append(want, release.ManualRetryWarning, `type="button" data-copy-text`) {
					if !strings.Contains(panel, text) {
						t.Errorf("%s missing %q", tab, text)
					}
				}
				if strings.Contains(panel, "<form") || strings.Contains(panel, `type="submit"`) {
					t.Errorf("%s has an execution control: %s", tab, panel)
				}
			}
			if strings.Count(body, `aria-label="Manual CI retries"`) != 1 || strings.Contains(body, ">/retest</code>") || strings.Count(body, ">/test test\n/test build</code>") != 1 || strings.Count(body, "actions/jobs/99/rerun") != 1 || len(f.actions) != 0 {
				t.Fatal("manual results leaked to another tab or produced an action")
			}
		})
	}
}
