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
		Category: "admission_policy", CheckStartedAt: now.Add(-time.Minute), Evidence: evidence,
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
