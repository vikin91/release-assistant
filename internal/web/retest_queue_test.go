package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestRetestQueueRendersOnlySupportedActions(t *testing.T) {
	config := validACS5Config()
	pr := release.Target{Repo: config.ACS().Repo, PR: 17}
	tag := release.Target{Repo: config.ACS().Repo, Tag: "5.0.1-rc.1"}
	snapshot := app.Snapshot{
		Config: config, Generation: 8, Interactive: true, WritesEnabled: true,
		RetestQueue: []app.RetestItem{
			{ID: 1, Target: tag, Kind: "konflux", Payload: "/retest main tag:5.0.1-rc.1", Job: "main", URL: "https://example.com/main"},
			{ID: 2, Target: pr, Kind: "actions", Payload: "gh api --method POST repos/acme/rox/actions/jobs/91/rerun", Job: "E2E / Upgrade", URL: "https://example.com/job/91", Title: "Backport <fix>", Branch: config.Branch},
			{ID: 3, Target: pr, Kind: "prow", Payload: "/test gke", Job: "ci/prow/gke", URL: "https://example.com/prow", Title: "Backport <fix>", Branch: config.Branch},
		},
		RetestLog: []app.RetestDecision{{
			At: time.Now(), ID: 4, Target: tag, Payload: "/retest sensor tag:5.0.1-rc.1", Result: "Sent",
			Detail: "GitHub confirmed the comment.", URL: "https://example.com/comment/4",
		}},
	}
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	panel := activityPanel(t, body, "retest-queue")
	for _, needle := range []string{
		"Backport &lt;fix&gt;", "https://example.com/main", "https://example.com/job/91", "https://example.com/prow",
		"https://example.com/comment/4", "GitHub confirmed the comment.", "<td>Sent</td>",
		`name="queue_id" value="1"`, `<button type="submit">Send</button>`,
		`<code data-copy-value>gh api --method POST repos/acme/rox/actions/jobs/91/rerun</code>`,
		`<button type="button" disabled>Execute</button>`, "Post manually on GitHub",
	} {
		if !strings.Contains(panel, needle) {
			t.Errorf("queue is missing %q", needle)
		}
	}
	if strings.Index(panel, "ACS tags") > strings.Index(panel, "ACS PRs against the release branch") {
		t.Fatal("the candidate should be ahead of ACS PRs")
	}
	if strings.Count(panel, `name="kind" value="queue-send"`) != 1 || strings.Count(panel, `name="kind" value="queue-skip"`) != 3 {
		t.Fatal("only the Konflux comment should have Send, and every item should have Skip")
	}
}
