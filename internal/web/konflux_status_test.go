package web

import (
	"net/http"
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
		Plans:         []release.Plan{{Target: target, SHA: "head", StartFailures: []release.StartFailure{failure, earlier, superseded, unknownAge}}},
		KonfluxHealth: &konflux.Health{CheckedAt: now, APIReadable: true, AdmittedAt: now},
		KonfluxReadIssues: []konflux.ReadIssue{{
			Cluster: failure.Cluster, Endpoint: "Tekton Results", Reason: "request timed out",
			LastFailure: now, RetryAt: now.Add(time.Minute),
		}},
	}}
	body := newBrowser(t, application).getBody(t)
	header := between(t, body, "<header", "</header>")
	if !strings.Contains(header, "Konflux · Healthy</summary>") || strings.Contains(header, failure.Reason) || !strings.Contains(header, "View Konflux diagnostics") || strings.Contains(header, "View creation failure") || strings.Contains(header, "release-context") {
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
		health konflux.Health
		label  string
		class  string
	}{
		"recent admission":         {konflux.Health{APIReadable: true, AdmittedAt: now}, "Healthy", "accepted"},
		"reachable without proof":  {konflux.Health{APIReadable: true}, "Unverified", "unverified"},
		"missing credentials":      {konflux.Health{APIStatus: http.StatusUnauthorized}, "Sign-in required", "unavailable"},
		"insufficient permissions": {konflux.Health{APIStatus: http.StatusForbidden}, "API access denied", "unavailable"},
		"server unavailable":       {konflux.Health{APIStatus: http.StatusServiceUnavailable}, "API unavailable", "unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			tc.health.CheckedAt, tc.health.Next = now, now.Add(30*time.Second)
			page := pageModel{Snapshot: app.Snapshot{KonfluxHealth: &tc.health}}
			status := page.KonfluxAdmission()
			if status.Label != tc.label || status.Class != tc.class || status.Last != tc.health.CheckedAt || status.Next != tc.health.Next {
				t.Fatalf("unexpected status: %+v", status)
			}
		})
	}
}

func TestKonfluxPillShowsOnlyCurrentHealth(t *testing.T) {
	now := time.Now()
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), KonfluxHealth: &konflux.Health{},
	}}
	browser := newBrowser(t, application)
	header := func() string { return between(t, browser.getBody(t), "<header", "</header>") }
	if got := header(); !strings.Contains(got, `class="konflux-pill pending">Konflux · Unknown`) {
		t.Fatalf("unexpected initial header: %s", got)
	}

	application.snapshot.KonfluxHealth = &konflux.Health{
		CheckedAt: now, Next: now.Add(30 * time.Second), Problem: "PipelineRun API: request timed out",
	}
	got := header()
	for _, want := range []string{`class="konflux-pill unavailable">Konflux · API unavailable`, "request timed out", formatTime(now), formatTime(now.Add(30 * time.Second))} {
		if !strings.Contains(got, want) {
			t.Errorf("header missing %q: %s", want, got)
		}
	}

	application.snapshot.KonfluxHealth = &konflux.Health{
		CheckedAt: now, APIStatus: http.StatusUnauthorized, Cluster: "example.openshiftapps.com",
	}
	got = header()
	if !strings.Contains(got, "Konflux · Sign-in required") ||
		!strings.Contains(got, "Run <code>oc login</code> for <code>example.openshiftapps.com</code>, then restart Release Assistant.") {
		t.Fatalf("missing sign-in instructions: %s", got)
	}
}
