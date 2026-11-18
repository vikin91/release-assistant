package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPRListsShowSelectionAndObservedChecks(t *testing.T) {
	config := validACS5Config()
	for tab, source := range map[string]release.Source{
		"component-prs": config.Components[0],
		"acs-prs":       config.ACS(),
	} {
		t.Run(tab, func(t *testing.T) {
			checkPRList(t, config, source, tab)
		})
	}
}

func checkPRList(t *testing.T, config release.Config, source release.Source, tab string) {
	t.Helper()
	fresh := release.Evidence{Known: true, At: time.Now()}
	pr := func(number int, state, conclusion string) release.PR {
		return release.PR{
			Target: release.Target{Repo: source.Repo, PR: number},
			Title:  fmt.Sprintf("change-%d", number), State: state, Evidence: fresh, ChecksEvidence: fresh,
			Checks: []release.Check{{Required: release.Yes, Conclusion: conclusion}},
			URL:    fmt.Sprintf("https://github.com/%s/pull/%d", source.Repo, number),
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
	repo := release.Repository{Source: source, PRs: []release.PR{open, candidate, closed, stale, clean, excluded, merged}}
	section := "pr-selection"
	if tab == "acs-prs" {
		config.PinningPR = 8
		pin := pr(8, "open", "success")
		pin.Base = source.Branch
		repo.PinningPR = &pin
		repo.PRs = append(repo.PRs, pin)
		section = "acs-prs-pr-selection"
	}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{
			open.Target: release.Included, closed.Target: release.Included, stale.Target: release.Included,
			merged.Target: release.Included, excluded.Target: release.Excluded,
		}},
		Repositories: []release.Repository{repo},
	}}
	browser := newBrowser(t, application)
	tabBody := activityPanel(t, browser.getBody(t), tab)
	body := between(t, tabBody, `id="`+section+`-panel"`, "</section>")
	if !strings.Contains(body, `name="`+section+`-filter" value="attention"`) {
		t.Fatal("the PR list needs its own filter group")
	}
	if tab == "acs-prs" && (!strings.Contains(tabBody, "Pinning PR #8") || strings.Contains(body, "change-8")) {
		t.Fatal("the pinning PR belongs in its separate panel")
	}
	hasBulkAction := strings.Contains(body, "Ignore &amp; hide all")
	hasIgnoredFilter := strings.Contains(body, `-filter" value="ignored"`)
	if hasBulkAction != (tab == "component-prs") || hasIgnoredFilter != (tab == "component-prs") {
		t.Fatal("bulk ignore and the ignored filter belong on Component PRs")
	}
	if !strings.Contains(body, "2 awaiting merge · 1 included and closed · 2 undecided · 1 included and merged") {
		t.Fatal("summary did not reflect the user's PR selection")
	}
	assertPROrder := func(body string) {
		last := -1
		for number := 1; number <= 6; number++ {
			pos := strings.Index(body, fmt.Sprintf(">#%d change-%d</a>", number, number))
			if pos <= last {
				t.Fatalf("PR #%d is missing or out of numerical order", number)
			}
			last = pos
		}
	}
	assertPROrder(body)
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
		if !strings.Contains(controls, `type="checkbox" name="enabled"`) ||
			!strings.Contains(controls, "Allow retry comments") {
			t.Errorf("PR %q lacks a visible retry-comment checkbox", tt.title)
		}
		if strings.Contains(card, "Evidence and checks") || strings.Contains(card, "Retry comment permission") {
			t.Errorf("PR %q contains a disclosure for checks or permissions", tt.title)
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
	application.snapshot.Choices.PRs[open.Target] = release.Excluded
	application.snapshot.Choices.PRs[candidate.Target] = release.Included
	updated := activityPanel(t, browser.getBody(t), tab)
	assertPROrder(between(t, updated, `id="`+section+`-panel"`, "</section>"))
}

func TestIgnoreAllComponentPRsSubmitsTheCurrentGeneration(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Generation: 12,
		Repositories: []release.Repository{{Source: source, PRs: []release.PR{{
			Target: release.Target{Repo: source.Repo, PR: 1}, State: "open",
		}}}},
	}}
	browser := newBrowser(t, application)
	form := formContaining(t, browser.getBody(t), "Ignore &amp; hide all")
	values := url.Values{}
	for _, field := range []string{"tab", "kind", "generation"} {
		values.Set(field, formInputValue(t, form, field))
	}
	if response := browser.post(values); response.Code != http.StatusSeeOther ||
		response.Header().Get("Location") != "/?tab=component-prs" {
		t.Fatal("bulk action failed or lost its tab")
	}
	if len(application.actions) != 1 || application.actions[0].Kind != "ignore-component-prs" ||
		application.actions[0].Generation != 12 {
		t.Fatalf("unexpected bulk action: %+v", application.actions)
	}
	values.Set("tab", "acs-prs")
	if response := browser.post(values); response.Code != http.StatusBadRequest {
		t.Fatal("bulk ignore was accepted from the ACS list")
	}
}
