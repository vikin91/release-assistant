package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

func TestDashboardShowsRecentKonfluxEvidenceAcrossTabs(t *testing.T) {
	now := time.Now()
	config := validACS5Config()
	source := config.Components[0]
	target := release.Target{Repo: source.Repo, Tag: "1.2.3"}
	evidence := release.Evidence{Known: true, At: now}
	failure := release.StartFailure{
		CheckID: 12, Job: "build", Cluster: "example.openshiftapps.com",
		Category: "admission_policy", Reason: "Pipeline creation denied by policy.", CheckStartedAt: now.Add(-time.Minute), Evidence: evidence,
	}
	earlier := failure
	earlier.CheckID, earlier.Cluster, earlier.CheckStartedAt = 13, "earlier.openshiftapps.com", now.Add(-time.Hour)
	superseded := failure
	superseded.CheckID, superseded.Cluster, superseded.Superseded = 14, "superseded.openshiftapps.com", true
	unknownAge := failure
	unknownAge.CheckID, unknownAge.Cluster, unknownAge.CheckStartedAt = 15, "undated.openshiftapps.com", time.Time{}

	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{Tags: map[string]string{source.Name: target.Tag}},
		Plans: []release.Plan{{Target: target, SHA: "head", StartFailures: []release.StartFailure{failure, earlier, superseded, unknownAge}}},
		KonfluxReadIssues: []konflux.ReadIssue{{
			Cluster: failure.Cluster, Endpoint: "Tekton Results", Reason: "request timed out",
			LastFailure: now, RetryAt: now.Add(time.Minute),
		}},
	}}
	body := newBrowser(t, application).getBody(t)
	header := between(t, body, "<header", "</header>")
	if !strings.Contains(header, "Konflux · Unhealthy</summary>") || !strings.Contains(header, failure.Reason) || !strings.Contains(header, "View Konflux diagnostics") || strings.Contains(header, "View creation failure") || strings.Contains(header, "release-context") {
		t.Fatalf("unexpected header: %s", header)
	}
	banner := between(t, body, `<section id="konflux-status"`, `</section>`)
	for _, want := range []string{
		"Pipeline start rejected by an admission policy", "Tekton Results reads paused",
		"request timed out", "Next recovery check eligible at", formatTime(now),
		`href="/?tab=component-releases#component-releases-start-failures"`,
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q: %s", want, banner)
		}
	}
	if strings.Count(banner, "<strong>Konflux") != 1 {
		t.Fatalf("expected a single cluster group: %s", banner)
	}
}

func TestKonfluxAdmission(t *testing.T) {
	now := time.Now()
	for name, tc := range map[string]struct {
		run    release.Run
		age    time.Duration
		issues []konfluxClusterStatus
		want   string
	}{
		"verified active run":     {run: release.Run{ID: "12", Status: "in_progress"}, want: "accepted"},
		"unverified identity":     {run: release.Run{Status: "in_progress"}, want: "unverified"},
		"completed build failure": {run: release.Run{ID: "12", Status: "completed", Outcome: "failure"}, want: "unverified"},
		"old observation":         {run: release.Run{ID: "12", Status: "in_progress"}, age: time.Hour, want: "unverified"},
		"read issue":              {run: release.Run{ID: "12", Status: "in_progress"}, issues: []konfluxClusterStatus{{Reads: []konflux.ReadIssue{{}}}}, want: "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			page := pageModel{Plans: []planModel{{Plan: release.Plan{
				Evidence: release.Evidence{Known: true, At: now.Add(-tc.age)}, Runs: []release.Run{tc.run},
			}}}, KonfluxStatus: tc.issues}
			if got := page.KonfluxAdmission(); got.Class != tc.want {
				t.Errorf("got %q, want %q", got.Class, tc.want)
			}
		})
	}
}

func TestAdmissionKeepsLastReportedErrorAfterHealthEvidenceExpires(t *testing.T) {
	now := time.Now()
	next := now.Add(time.Minute)
	target := release.Target{Repo: "acme/fact", PR: 1}
	page := pageModel{
		Snapshot: app.Snapshot{NextSourceCheck: map[string]time.Time{target.Repo: next}},
		Plans: []planModel{{Plan: release.Plan{Target: target, StartFailures: []release.StartFailure{{
			Reason: "An admission policy denied PipelineRun creation.", CheckStartedAt: now.Add(-time.Hour),
			Evidence: release.Evidence{Known: true, At: now},
		}}}}},
	}
	status := page.KonfluxAdmission()
	if status.Label != "Unverified" || status.Detail != page.Plans[0].Plan.StartFailures[0].Reason || !status.Last.Equal(now) || !status.Next.Equal(next) {
		t.Fatalf("unexpected diagnostic: %+v", status)
	}
}

func TestKonfluxPillBeforeAndAfterTimeout(t *testing.T) {
	now := time.Now()
	issue := konflux.ReadIssue{Endpoint: "PipelineRun API", Reason: "request timed out", LastFailure: now, RetryAt: now.Add(time.Minute)}
	application := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config()}}
	browser := newBrowser(t, application)
	header := func() string { return between(t, browser.getBody(t), "<header", "</header>") }
	if got := header(); !strings.Contains(got, `class="konflux-pill pending">Konflux · Unknown`) {
		t.Fatalf("unexpected initial header: %s", got)
	}
	application.snapshot.KonfluxReadIssues = []konflux.ReadIssue{issue}
	got := header()
	for _, want := range []string{`class="konflux-pill unavailable">Konflux · API unavailable`, "PipelineRun API read failed: request timed out", formatTime(issue.LastFailure), formatTime(issue.RetryAt)} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q: %s", want, got)
		}
	}
}
