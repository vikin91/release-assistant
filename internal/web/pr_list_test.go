package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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
		"fact-prs": config.Components[0],
		"acs-prs":  config.ACS(),
	} {
		t.Run(tab, func(t *testing.T) {
			checkPRList(t, config, source, tab)
		})
	}
}

func TestComponentPRViewsHaveIndependentStatusesAndControls(t *testing.T) {
	config := prefilledConfig("4.10.12")
	config.Branch = "release-4.10"
	snapshot := app.Snapshot{Config: config, Choices: release.Choices{PRs: make(map[release.Target]release.Choice)}}
	fresh := release.Evidence{Known: true, At: time.Now()}
	for _, source := range config.Components {
		target := release.Target{Repo: source.Repo, PR: 42}
		pr := release.PR{
			Target: target, Title: "Change " + source.Name, State: "open", Base: source.Branch, Evidence: fresh,
		}
		switch source.Name {
		case "collector":
			snapshot.Choices.PRs[target] = release.Included
			pr.ChecksEvidence = fresh
			pr.Checks = []release.Check{{Required: release.Yes, Conclusion: "failure"}}
		case "scanner":
			snapshot.Choices.PRs[target] = release.Excluded
		}
		snapshot.Repositories = append(snapshot.Repositories, release.Repository{
			Source: source, PRs: []release.PR{pr}, Evidence: fresh,
		})
	}
	browser := newBrowser(t, &fakeApplication{snapshot: snapshot})
	body := browser.getBody(t)
	parent := activityPanel(t, body, "component-prs")
	sidebar := between(t, body, `class="component-pr-tabs"`, "</div>")
	if strings.Contains(parent, `class="pr-list-table"`) || strings.Count(parent, `data-open-section`) != 3 {
		t.Fatal("the parent should show an overview linking to each component")
	}
	for component, want := range map[string]struct{ heading, status string }{
		"collector": {"Collector PRs", "Needs attention"},
		"fact":      {"Fact PRs", "Action needed"},
		"scanner":   {"Scanner PRs", "Complete"},
	} {
		t.Run(component, func(t *testing.T) {
			tab := component + "-prs"
			if !strings.Contains(sidebar, `aria-label="`+want.heading+" · "+want.status) {
				t.Fatal("sidebar link is missing the component's status")
			}
			response := httptest.NewRecorder()
			browser.handler.ServeHTTP(response, httptest.NewRequestWithContext(
				t.Context(), http.MethodGet, "http://127.0.0.1:8080/?tab="+tab, nil,
			))
			view := response.Body.String()
			if response.Code != http.StatusOK || !strings.Contains(view, `data-active-tab="`+tab+`"`) {
				t.Fatal("direct link did not select the component")
			}
			panel := activityPanel(t, view, tab)
			for _, marker := range []string{
				"<strong>" + want.status + "</strong>",
				`name="` + componentPRSection(component) + `-filter"`,
				`name="tab" value="` + tab + `"`,
				`name="component" value="` + component + `"`,
				"Change " + component,
			} {
				if !strings.Contains(panel, marker) {
					t.Errorf("missing %q in %s", marker, tab)
				}
			}
			if strings.Count(panel, "Change ") != 1 || strings.Count(panel, `class="pr-list-table"`) != 1 ||
				strings.Count(panel, `name="kind" value="component-pr-choices"`) != 1 || strings.Contains(panel, "Checks of Included PRs") {
				t.Fatal("the component view contains an extra table, rows or controls")
			}
		})
	}
}

func TestScannerPRViewRequiresAConfiguredScanner(t *testing.T) {
	browser := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config()}})
	response := httptest.NewRecorder()
	browser.handler.ServeHTTP(response, httptest.NewRequestWithContext(
		t.Context(), http.MethodGet, "http://127.0.0.1:8080/?tab=scanner-prs", nil,
	))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `data-active-tab="component-prs"`) ||
		strings.Contains(body, `data-workflow-tab="scanner-prs"`) || strings.Contains(body, `id="panel-scanner-prs"`) {
		t.Fatal("the Scanner PR view should be unavailable for ACS 5")
	}
}

func TestPRListsShowSavedHiddenPRsWithoutAnObservation(t *testing.T) {
	config := validACS5Config()
	for tab, source := range map[string]release.Source{"fact-prs": config.Components[0], "acs-prs": config.ACS()} {
		t.Run(tab, func(t *testing.T) {
			target := release.Target{Repo: source.Repo, PR: 123}
			browser := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{
				Config:  config,
				Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Ignored}},
			}})
			panel := activityPanel(t, browser.getBody(t), tab)
			card := prCardContaining(panel, "https://github.com/"+source.Repo+"/pull/123")
			if !strings.Contains(panel, `-filter" value="ignored"`) ||
				!strings.Contains(card, `data-release-choice="ignored"`) ||
				!strings.Contains(card, `name="choice" value="included"`) {
				t.Fatalf("missing restorable PR %s in %s", target, tab)
			}
		})
	}
}

func checkPRList(t *testing.T, config release.Config, source release.Source, tab string) {
	t.Helper()
	fresh := release.Evidence{Known: true, At: time.Now()}
	pr := func(number int, state, conclusion string) release.PR {
		return release.PR{
			Target: release.Target{Repo: source.Repo, PR: number},
			Title:  fmt.Sprintf("change-%d", number), State: state, Base: source.Branch, Evidence: fresh, ChecksEvidence: fresh,
			Checks: []release.Check{{Required: release.Yes, Conclusion: conclusion}},
			URL:    fmt.Sprintf("https://github.com/%s/pull/%d", source.Repo, number),
		}
	}
	open := pr(1, "open", "failure")
	open.MergeState = "blocked"
	open.Checks = append(open.Checks,
		release.Check{Required: release.Yes, Status: "in_progress"},
		release.Check{Required: release.Yes, Status: "queued"},
		release.Check{Required: release.Yes, Conclusion: "success"},
		release.Check{Required: release.No, Status: "in_progress"},
		release.Check{Name: "GitHub Actions tests", Required: release.No, Conclusion: "failure"},
		release.Check{Name: "Konflux tests", Required: release.No, Conclusion: "failure"},
	)
	closed := pr(2, "closed", "success")
	candidate := pr(3, "open", "failure")
	candidate.Checks[0].Required = release.No
	candidate.Checks = append(candidate.Checks, release.Check{Required: release.Yes, Status: "in_progress"})
	stale := pr(4, "open", "failure")
	stale.ChecksEvidence.Error = "read timed out"
	clean := pr(5, "open", "success")
	clean.MergeState = "clean"
	excluded := pr(6, "open", "success")
	merged := pr(7, "closed", "success")
	merged.Merged = true
	repo := release.Repository{Source: source, PRs: []release.PR{open, candidate, closed, stale, clean, excluded, merged}}
	section := componentPRSection(source.Name)
	if tab == "acs-prs" {
		config.PinningPR = 8
		pin := pr(8, "open", "success")
		pin.Base = source.Branch
		repo.PinningPR = &pin
		repo.PRs = append(repo.PRs, pin)
		section = "acs-prs-pr-selection"
	}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, WritesEnabled: true,
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
	if tab == "acs-prs" && (strings.Count(tabBody, "Open pinning PR #8</a>") != 1 || strings.Count(body, "change-8") != 1 || !strings.Contains(body, "Confirmed pinning PR")) {
		t.Fatal("expected one confirmed pinning PR row")
	}
	hasIgnoredFilter := strings.Contains(body, `-filter" value="ignored"`)
	if hasIgnoredFilter {
		t.Fatal("the ignored filter should appear once there are hidden PRs")
	}
	undecided := 2
	if tab == "acs-prs" {
		undecided++
	}
	if !strings.Contains(body, fmt.Sprintf("2 awaiting merge · 1 included and closed · %d undecided · 1 included and merged", undecided)) {
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
		{"change-1", "included", "3 checks failed, including 1 required", true},
		{"change-2", "included", "Closed without merging", true},
		{"change-3", "undecided", "", false},
		{"change-4", "included", "Checks unavailable · read timed out", false},
		{"change-5", "undecided", "", false},
		{"change-6", "excluded", "", false},
	} {
		card := prCardContaining(body, tt.title)
		if !strings.Contains(card, `data-release-choice="`+tt.choice+`"`) ||
			!strings.Contains(card, tt.checks) || strings.Contains(card, "data-pr-attention") != tt.attention {
			t.Errorf("PR %q has the wrong selection, status, or filter category", tt.title)
		}
		if strings.Contains(card, "View checks") != (tt.choice == "included") {
			t.Errorf("PR %q shows a check summary for the wrong release choice", tt.title)
		}
		controls, _, _ := strings.Cut(card, "<details")
		for _, value := range []string{"included", "excluded", "ignored"} {
			if !strings.Contains(controls, `name="choice" value="`+value+`"`) {
				t.Errorf("PR %q lacks a visible %s action", tt.title, value)
			}
		}
		if !strings.Contains(controls, `type="checkbox" name="enabled"`) ||
			!strings.Contains(controls, retryCommentPermissionLabel) {
			t.Errorf("PR %q lacks a visible retry-comment checkbox", tt.title)
		}
		if strings.Contains(card, "Evidence and checks") || strings.Contains(card, "Retry comment permission") {
			t.Errorf("PR %q contains a disclosure for checks or permissions", tt.title)
		}
	}
	mixed := prCardContaining(body, "change-1")
	for _, want := range []string{
		"❌", "3 checks failed, including 1 required", "2 required checks running",
		"/pull/1/checks", `<span class="pill no">Merge blocked</span>`,
	} {
		if !strings.Contains(mixed, want) {
			t.Errorf("mixed check summary missing %q", want)
		}
	}
	if ready := prCardContaining(body, "change-5"); !strings.Contains(ready, `<span class="pill yes">Ready to merge</span>`) {
		t.Error("the ready PR needs a merge status pill even before inclusion")
	}
	if strings.Contains(mixed, "GitHub Actions tests") || strings.Contains(mixed, "Konflux tests") {
		t.Error("the PR row should summarize failures without listing individual checks")
	}
	if len(application.actions) != 0 {
		t.Fatal("rendering triage changed the release selection")
	}
	application.snapshot.Choices.PRs[open.Target] = release.Excluded
	application.snapshot.Choices.PRs[candidate.Target] = release.Included
	updated := activityPanel(t, browser.getBody(t), tab)
	assertPROrder(between(t, updated, `id="`+section+`-panel"`, "</section>"))
	included := prCardContaining(updated, "change-3")
	for _, want := range []string{"1 check failed, including 0 required", "1 required check running"} {
		if !strings.Contains(included, want) {
			t.Errorf("PR with only optional failures missing %q", want)
		}
	}
	application.snapshot.Choices.PRs[closed.Target] = release.Ignored
	updated = activityPanel(t, browser.getBody(t), tab)
	if !strings.Contains(updated, `-filter" value="ignored"`) ||
		!strings.Contains(prCardContaining(updated, "change-2"), `data-release-choice="ignored"`) {
		t.Fatal("the ignored filter should show closed PRs")
	}
}

func TestPRMergeStatusUsesGitHubVerdict(t *testing.T) {
	for name, tc := range map[string]struct {
		verdict      string
		draft, stale bool
		want         release.Truth
	}{
		"should report ready with non-blocking failures": {verdict: "unstable", want: release.Yes},
		"should report blocked for conflicts":            {verdict: "dirty", want: release.No},
		"should report blocked when out of date":         {verdict: "behind", want: release.No},
		"should report blocked for a draft":              {verdict: "clean", draft: true, want: release.No},
		"should wait for GitHub to calculate a verdict":  {verdict: "unknown"},
		"should omit a stale verdict":                    {verdict: "clean", stale: true},
	} {
		t.Run(name, func(t *testing.T) {
			pr := release.PR{State: "open", MergeState: tc.verdict, Draft: tc.draft,
				Evidence: release.Evidence{Known: !tc.stale, At: time.Now()}}
			if got := prMergeStatus(pr); got != tc.want {
				t.Errorf("merge status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBulkPRChoicesSubmitTheCurrentGeneration(t *testing.T) {
	config := validACS5Config()
	for tab, tc := range map[string]struct {
		source         release.Source
		kind, otherTab string
	}{
		"fact-prs": {config.Components[0], "component-pr-choices", "collector-prs"},
		"acs-prs":  {config.ACS(), "acs-pr-choices", "component-prs"},
	} {
		t.Run(tab, func(t *testing.T) {
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: config, Generation: 12,
				Repositories: []release.Repository{{Source: tc.source, PRs: []release.PR{{
					Target: release.Target{Repo: tc.source.Repo, PR: 1}, State: "open",
				}}}},
			}}
			browser := newBrowser(t, application)
			panel := activityPanel(t, browser.getBody(t), tab)
			form := formContaining(t, panel, `class="control-row pr-list-bulk"`)
			values := url.Values{}
			for _, field := range []string{"tab", "kind", "generation"} {
				values.Set(field, formInputValue(t, form, field))
			}
			var component, fragment string
			if tab == "fact-prs" {
				component = tc.source.Name
				fragment = "#" + componentPRSection(component)
				values.Set("component", formInputValue(t, form, "component"))
			}
			for choice, label := range map[release.Choice]string{
				release.Included: "Include all", release.Excluded: "Exclude all", release.Ignored: "Ignore &amp; hide all",
			} {
				if !strings.Contains(form, fmt.Sprintf(`name="choice" value="%s">%s</button>`, choice, label)) {
					t.Fatalf("missing enabled %s button", label)
				}
				values.Set("choice", string(choice))
				if response := browser.post(values); response.Code != http.StatusSeeOther ||
					response.Header().Get("Location") != "/?tab="+tab+fragment {
					t.Fatal("bulk action failed or lost its tab")
				}
				action := application.actions[len(application.actions)-1]
				if action.Kind != tc.kind || action.Choice != choice || action.Generation != 12 || action.Component != component {
					t.Fatalf("unexpected bulk action: %+v", action)
				}
			}
			values.Set("tab", tc.otherTab)
			if response := browser.post(values); response.Code != http.StatusBadRequest {
				t.Fatal("bulk action was accepted from the wrong list")
			}
		})
	}
}

func TestACSPRRowShowsObservedFailuresWithoutRequiredSettings(t *testing.T) {
	s := workflowStatusSnapshot()
	source := s.Config.ACS()
	target := release.Target{Repo: source.Repo, PR: 24102}
	fresh := s.Inventory.Evidence
	pr := release.PR{
		Target: target, Title: "Update V6 vulnerability bundles", State: "open", Base: source.Branch, SHA: "head",
		Evidence: fresh, CheckResultsEvidence: fresh,
		ChecksEvidence: release.Evidence{Known: true, At: fresh.At, Error: "Required-check settings: Access denied."},
		Checks: []release.Check{
			{Name: "GitHub Actions tests", Conclusion: "failure"},
			{Name: "Konflux startup check", Conclusion: "startup_failure"},
		},
	}
	s.Repositories[len(s.Repositories)-1].PRs = []release.PR{pr}
	s.Choices.PRs[target] = release.Included
	s.Plans = []release.Plan{{Target: target, Proposals: []release.Proposal{{
		Target: target, Job: "checks", SHA: pr.SHA, RunID: "12", Payload: "/konflux-retest checks",
	}}}}
	body := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-prs")
	card := prCardContaining(body, pr.Title)
	for _, want := range []string{
		"❌", "2 checks failed", "· couldn’t verify which checks are required", "View checks", "/pull/24102/checks",
		"data-pr-attention", "Refresh checks", prRetryAnchor(target),
	} {
		if !strings.Contains(card, want) {
			t.Errorf("missing %q in PR card", want)
		}
	}
	if !strings.Contains(body, `id="acs-pr-retries-24102"`) {
		t.Fatal("check details should include the matching PR")
	}
}

func TestPinDiscoveryRows(t *testing.T) {
	for name, tc := range map[string]struct {
		matches    int
		incomplete bool
		want       string
	}{
		"one candidate":      {1, false, "Suggested pinning PR"},
		"several candidates": {2, false, "Several PRs change component pin files"},
		"no candidates":      {0, false, "No open PR changes a component pin file"},
		"partial discovery":  {1, true, "Pinning PR discovery incomplete"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := validACS5Config()
			fresh := release.Evidence{Known: true, At: time.Now()}
			repo := release.Repository{Source: cfg.ACS(), PinDiscovery: release.PinDiscovery{Files: map[int]release.PinFileMatch{}, Evidence: fresh}}
			for n := 1; n <= 2; n++ {
				repo.PRs = append(repo.PRs, release.PR{Target: release.Target{Repo: repo.Source.Repo, PR: n}, Base: cfg.Branch, SHA: "reviewed", State: "open", Evidence: fresh})
				if n <= tc.matches {
					repo.PinDiscovery.Files[n] = release.PinFileMatch{Head: "reviewed", Base: "base", Reason: "Changes FACT_VERSION."}
				}
			}
			if tc.incomplete {
				repo.PinDiscovery.Evidence.Error = "incomplete file inventory"
			}
			body := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: cfg, Repositories: []release.Repository{repo}}}).getBody(t), "acs-prs")
			if !strings.Contains(body, tc.want) || strings.Count(body, "Use as pinning PR") != 2 || !strings.Contains(body, "2 open ·") || strings.Count(body, "Changes FACT_VERSION.") != tc.matches {
				t.Fatal("unexpected pinning roles or choices", body)
			}
			if tc.want != "Suggested pinning PR" && strings.Contains(body, "Suggested pinning PR") {
				t.Fatal("suggested without unique complete evidence")
			}
		})
	}
}

func TestPinDetectionSetting(t *testing.T) {
	cfg := validACS5Config()
	form := makePage(app.Snapshot{Config: cfg}, "", "").ConfigForm
	form.Set("kind", "configure")
	form.Set("pinning_pr", "46")
	form.Set("detect_pinning_pr", "true")
	a, err := parseAction(&http.Request{Form: form}, cfg)
	if err != nil || a.Config.PinningPR != 0 || a.Config.ManualPinningPR != 46 || !a.Config.DetectPinningPR {
		t.Fatalf("detection override: %+v %v", a, err)
	}
	saved := makePage(app.Snapshot{Config: a.Config}, "", "").ConfigForm
	saved.Set("kind", "configure")
	saved.Del("detect_pinning_pr")
	a, err = parseAction(&http.Request{Form: saved}, cfg)
	if err != nil || a.Config.PinningPR != 46 || a.Config.DetectPinningPR {
		t.Fatalf("restoring manual selection: %+v %v", a, err)
	}
}

func TestSettingsKeepsPendingPinningChoiceWithItsRelease(t *testing.T) {
	config := validACS5Config()
	snapshot := app.Snapshot{Config: config, PendingPinningPR: 46}
	application := &fakeApplication{snapshot: snapshot}
	browser := newBrowser(t, application)
	body := browser.getBody(t)
	if !strings.Contains(activityPanel(t, body, "acs-prs"), "Pin PR #46: unverified") {
		t.Fatal("saved pinning choice did not appear before verification")
	}
	form := makePage(snapshot, "", "").ConfigForm
	if form.Get("pinning_pr") != "46" {
		t.Fatal("settings did not show the saved pinning choice")
	}
	form.Set("kind", "configure")
	form.Set("version", "5.0.2")
	if response := browser.post(form); response.Code != http.StatusSeeOther {
		t.Fatalf("changing release returned %d", response.Code)
	}
	if got := application.actions[0].Config.PinningPR; got != 0 {
		t.Fatalf("carried pinning PR %d into another release", got)
	}
	form.Set("pinning_pr", "47")
	if response := browser.post(form); response.Code != http.StatusSeeOther {
		t.Fatalf("changing release with a new number returned %d", response.Code)
	}
	if got := application.actions[1].Config.PinningPR; got != 47 {
		t.Fatalf("lost explicitly entered pinning PR, got %d", got)
	}
}

func TestIncludedPinningPRHasNoChoiceControls(t *testing.T) {
	for name, merged := range map[string]bool{"awaiting merge": false, "merged": true} {
		t.Run(name, func(t *testing.T) {
			c := validACS5Config()
			c.PinningPR = 19
			pr := release.PR{Target: c.PinningTarget(), Base: c.Branch, State: "open", Merged: merged, Title: "Release pins", Evidence: release.Evidence{Known: true, At: time.Now()}}
			if merged {
				pr.State = "merged"
			}
			snapshot := app.Snapshot{Config: c, Choices: release.Choices{PRs: map[release.Target]release.Choice{pr.Target: release.Included}}, Repositories: []release.Repository{{Source: c.ACS(), PRs: []release.PR{pr}, PinningPR: &pr}}}
			page := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: snapshot}).getBody(t), "acs-prs")
			list := between(t, page, `id="acs-prs-pr-selection-panel"`, "</section>")
			count := "1 awaiting merge"
			if merged {
				count = "1 included and merged"
			}
			if !strings.Contains(list, count) || strings.Count(list, "Confirmed pinning PR") != 1 || strings.Contains(list, `name="kind" value="choice"`) {
				t.Fatal("incorrect pinning PR controls or count", list)
			}
			form := formContaining(t, list, `class="control-row pr-list-bulk"`)
			for _, label := range []string{"Include all", "Exclude all", "Ignore &amp; hide all"} {
				assertButtonTextDisabled(t, label, form, label, true)
			}
		})
	}
}
