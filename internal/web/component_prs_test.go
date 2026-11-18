package web

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestComponentPRTriageShowsSelectionAndObservedChecks(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	fresh := release.Evidence{Known: true, At: time.Now()}
	pr := func(number int, state, conclusion string) release.PR {
		return release.PR{
			Target: release.Target{Repo: source.Repo, PR: number},
			Title:  fmt.Sprintf("change-%d", number), State: state, Evidence: fresh, ChecksEvidence: fresh,
			Checks: []release.Check{{Required: release.Yes, Conclusion: conclusion}},
		}
	}
	open := pr(1, "open", "failure")
	closed := pr(2, "closed", "success")
	candidate := pr(3, "open", "failure")
	stale := pr(4, "open", "failure")
	stale.ChecksEvidence.Error = "read timed out"
	clean := pr(5, "open", "success")
	pending := pr(6, "open", "")
	pending.Checks[0].Status = "in_progress"
	merged := pr(7, "closed", "success")
	merged.Merged = true
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{
			open.Target: release.Included, closed.Target: release.Included, merged.Target: release.Included,
		}},
		Repositories: []release.Repository{{
			Source: source, PRs: []release.PR{open, candidate, closed, stale, clean, pending, merged},
		}},
	}}
	body := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
	if !strings.Contains(body, "1 awaiting merge · 1 included and closed · 4 undecided · 1 included and merged") {
		t.Fatal("summary did not reflect the user's PR selection")
	}
	if strings.Index(body, "change-2") > strings.Index(body, "change-1") {
		t.Fatal("closed included PR should appear first within its component")
	}
	for _, tt := range []struct {
		title, choice, checks string
		attention             bool
	}{
		{"change-1", "included", "1 required check failed", true},
		{"change-2", "included", "Closed without merging", true},
		{"change-3", "undecided", "1 required check failed", false},
		{"change-4", "undecided", "Checks unverified", false},
		{"change-5", "undecided", "Required checks passed", false},
		{"change-6", "undecided", "Required checks pending", false},
	} {
		card := prCardContaining(body, tt.title)
		if !strings.Contains(card, `data-release-choice="`+tt.choice+`"`) ||
			!strings.Contains(card, tt.checks) || strings.Contains(card, "data-pr-attention") != tt.attention {
			t.Errorf("PR %q has the wrong selection, status, or filter category", tt.title)
		}
	}
	if len(application.actions) != 0 {
		t.Fatal("rendering triage changed the release selection")
	}
}
