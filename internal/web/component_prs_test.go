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
	open.Checks = append(open.Checks,
		release.Check{Required: release.Yes, Status: "in_progress"},
		release.Check{Required: release.Yes, Status: "queued"},
		release.Check{Required: release.Yes, Conclusion: "success"},
		release.Check{Required: release.No, Conclusion: "failure"},
		release.Check{Conclusion: "failure"},
	)
	closed := pr(2, "closed", "success")
	candidate := pr(3, "open", "failure")
	stale := pr(4, "open", "failure")
	stale.ChecksEvidence.Error = "read timed out"
	clean := pr(5, "open", "success")
	excluded := pr(6, "open", "success")
	merged := pr(7, "closed", "success")
	merged.Merged = true
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{
			open.Target: release.Included, closed.Target: release.Included, stale.Target: release.Included,
			merged.Target: release.Included, excluded.Target: release.Excluded,
		}},
		Repositories: []release.Repository{{
			Source: source, PRs: []release.PR{open, candidate, closed, stale, clean, excluded, merged},
		}},
	}}
	body := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
	if !strings.Contains(body, "2 awaiting merge · 1 included and closed · 2 undecided · 1 included and merged") {
		t.Fatal("summary did not reflect the user's PR selection")
	}
	if strings.Index(body, "change-2") > strings.Index(body, "change-1") {
		t.Fatal("closed included PR should appear first within its component")
	}
	for _, tt := range []struct {
		title, choice, checks string
		attention             bool
	}{
		{"change-1", "included", "1 failed,", true},
		{"change-2", "included", "Closed without merging", true},
		{"change-3", "undecided", "", false},
		{"change-4", "included", "Unverified", false},
		{"change-5", "undecided", "", false},
		{"change-6", "excluded", "", false},
	} {
		card := prCardContaining(body, tt.title)
		if !strings.Contains(card, `data-release-choice="`+tt.choice+`"`) ||
			!strings.Contains(card, tt.checks) || strings.Contains(card, "data-pr-attention") != tt.attention {
			t.Errorf("PR %q has the wrong selection, status, or filter category", tt.title)
		}
		if strings.Contains(card, "Required checks:") != (tt.choice == "included") {
			t.Errorf("PR %q shows a check summary for the wrong release choice", tt.title)
		}
		controls, _, _ := strings.Cut(card, "<details")
		for _, value := range []string{"included", "excluded", "ignored"} {
			if !strings.Contains(controls, `name="choice" value="`+value+`"`) {
				t.Errorf("PR %q lacks a visible %s action", tt.title, value)
			}
		}
	}
	mixed := prCardContaining(body, "change-1")
	for _, want := range []string{"❌", "1 failed,", "⚠️", "2 running,", "✅", "1 passed", "1 unverified"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("mixed check summary missing %q", want)
		}
	}
	if len(application.actions) != 0 {
		t.Fatal("rendering triage changed the release selection")
	}
}
