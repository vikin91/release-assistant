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
	now := time.Now()
	snapshot := app.Snapshot{
		Config: config, Generation: 8, Interactive: true, WritesEnabled: true,
		RetestQueue: []app.RetestItem{
			{ID: 1, Target: tag, Kind: "konflux", Payload: "/retest main tag:5.0.1-rc.1", Job: "main", URL: "https://example.com/main"},
			{ID: 2, Target: pr, Kind: "actions", Payload: "gh api --method POST repos/acme/rox/actions/jobs/91/rerun", Job: "E2E / Upgrade", URL: "https://example.com/job/91", Title: "Backport <fix>", Branch: config.Branch},
			{ID: 3, Target: pr, Kind: "prow", Payload: "/test gke", Job: "ci/prow/gke", URL: "https://example.com/prow", Title: "Backport <fix>", Branch: config.Branch},
			{ID: 5, Target: pr, Kind: "konflux", Payload: "/konflux-retest main-on-push", Job: "main-on-push", MissingBuild: true},
		},
		RetestLog: []app.RetestDecision{{
			At: now.Add(-2 * time.Minute), ID: 2, Target: pr, Payload: "/konflux-retest main-on-push", Result: "Skipped",
		}, {
			At: now.Add(-time.Minute), ID: 4, Target: tag, Payload: "/retest sensor tag:5.0.1-rc.1", Result: "Skipped",
		}, {
			At: now, ID: 4, Target: tag, Payload: "/retest sensor tag:5.0.1-rc.1", Result: "Sent",
			Detail: "GitHub confirmed the comment.", URL: "https://example.com/comment/4",
		}},
	}
	body := newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t)
	tab := between(t, body, `id="tab-retest-queue"`, "</a>")
	if !strings.Contains(tab, "<span>Retest Queue (4)</span>") {
		t.Fatal("the navigation count should include pending rows only")
	}
	panel := activityPanel(t, body, "retest-queue")
	for _, needle := range []string{
		"Backport &lt;fix&gt;", "https://example.com/main", "https://example.com/job/91", "https://example.com/prow",
		"https://example.com/comment/4", "GitHub confirmed the comment.", "<td>Sent</td>",
		`name="queue_id" value="1"`, `<button type="submit">Send</button>`,
		`<code data-copy-value>gh api --method POST repos/acme/rox/actions/jobs/91/rerun</code>`,
		`<button type="button" disabled>Execute</button>`, "Post manually on GitHub",
		"main-on-push has not started",
	} {
		if !strings.Contains(panel, needle) {
			t.Errorf("queue is missing %q", needle)
		}
	}
	acs := between(t, panel, `id="retest-table-acs"`, "</table>")
	tagRow, prRow := strings.Index(acs, "Tag 5.0.1-rc.1"), strings.Index(acs, "PR #17")
	if tagRow < 0 || prRow < 0 || tagRow >= prRow || strings.Count(acs, "<thead>") != 1 {
		t.Fatal("the ACS table should show its tag before its PR under one column header")
	}
	if strings.Contains(panel, `id="retest-table-scanner"`) {
		t.Fatal("ACS 5 should not show a Scanner table")
	}
	if strings.Count(panel, `name="kind" value="queue-send"`) != 2 || strings.Count(panel, `name="kind" value="queue-skip"`) != 4 {
		t.Fatal("only Konflux comments should have Send, and every item should have Skip")
	}
	log := between(t, panel, `id="retest-log-heading"`, "</table>")
	if !strings.Contains(log, `<td title="stackrox/stackrox#17">stackrox#17</td>`) ||
		!strings.Contains(log, `<td>stackrox/stackrox@5.0.1-rc.1</td>`) {
		t.Fatal("the Queue log should shorten PR targets and retain full tag targets")
	}
	sent, skipped := strings.Index(log, "<td>Sent</td>"), strings.Index(log, "<td>Skipped</td>")
	if sent < 0 || skipped < 0 || sent >= skipped {
		t.Fatal("the queue log should list the latest decision first")
	}
}

func TestRetestQueueHasOneTablePerConfiguredRepository(t *testing.T) {
	config := prefilledConfig("4.10.12")
	config.Branch = "release-4.10"
	items := []app.RetestItem{
		{ID: 1, Target: release.Target{Repo: "stackrox/scanner", PR: 42}},
		{ID: 2, Target: release.Target{Repo: "stackrox/fact", Tag: "0.2.5"}},
		{ID: 3, Target: release.Target{Repo: "stackrox/fact", PR: 42}},
		{ID: 4, Target: release.Target{Repo: "stackrox/stackrox", PR: 42}},
	}
	for i := range items {
		items[i].Kind = "actions"
	}
	browser := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config, RetestQueue: items}})
	panel := activityPanel(t, browser.getBody(t), "retest-queue")
	if strings.Count(panel, `class="retest-items"`) != 4 {
		t.Fatal("expected four repository tables")
	}
	previous := -1
	for _, name := range []string{"acs", "collector", "fact", "scanner"} {
		start := `id="retest-table-` + name + `"`
		if index := strings.Index(panel, start); index <= previous {
			t.Fatalf("repository table %s is missing or out of order", name)
		} else {
			previous = index
		}
	}
	for repo, want := range map[string][]string{
		"acs":       {`name="queue_id" value="4"`},
		"collector": {"No retries are queued."},
		"fact":      {`name="queue_id" value="2"`, `name="queue_id" value="3"`},
		"scanner":   {`name="queue_id" value="1"`},
	} {
		table := between(t, panel, `id="retest-table-`+repo+`"`, "</table>")
		for _, marker := range want {
			if !strings.Contains(table, marker) {
				t.Errorf("%s table is missing %q", repo, marker)
			}
		}
	}
}
