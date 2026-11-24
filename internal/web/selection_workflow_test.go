package web

import (
	"html"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

var renderedFormRE = regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`)

func TestSelectedTagWorkflowRendersSummaryAlternativesAndEvidence(t *testing.T) {
	observed := release.Evidence{Known: true, At: time.Now()}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), Generation: 23, WritesEnabled: true,
		Choices: release.Choices{
			Tags: map[string]string{"fact": "3.25.1", "collector": "3.25.4"},
			PRs:  map[release.Target]release.Choice{{Repo: "acme/fact", PR: 11}: release.Included},
		},
		Assessment: release.Assessment{ProposedTags: map[string]string{"fact": "3.25.2"}, Conditions: []release.Condition{{
			Name: "tag fact", Status: release.No, Reason: "Current tag does not contain merge-1", TagIssue: release.TagMissingMerge,
		}}},
		Plans: []release.Plan{
			{Target: release.Target{Repo: "acme/fact", Tag: "3.25.1"}, SHA: "fact-current-sha",
				Validation: release.Validation{SHA: "fact-current-sha", Evidence: observed,
					Checks: []release.ValidationCheck{{Check: release.Check{Name: "Fact tag smoke test", Conclusion: "success"}}}}},
			{Target: release.Target{Repo: "acme/collector", Tag: "3.25.4"}, SHA: "another-sha",
				Validation: release.Validation{SHA: "another-sha", Evidence: observed,
					Checks: []release.ValidationCheck{{Check: release.Check{Name: "Collector test for another commit"}}}}},
		},
		Repositories: []release.Repository{
			{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25", Stream: "3.25"}, Tags: []release.Tag{
				{Name: "3.25.1", SHA: "fact-current-sha", Belongs: release.Yes, Contains: map[string]release.Truth{"merge-1": release.No}, Evidence: observed},
				{Name: "3.25.2", SHA: "fact-next-sha", Belongs: release.Yes, Evidence: observed},
			}, PRs: []release.PR{{Target: release.Target{Repo: "acme/fact", PR: 11}, SHA: "pr-head", MergeSHA: "merge-1", Merged: true}}},
			{Source: release.Source{Name: "collector", Repo: "acme/collector", Branch: "release-3.25", Stream: "3.25"}, Tags: []release.Tag{
				{Name: "3.25.4", SHA: "collector-current-sha", Belongs: release.Yes, Evidence: observed},
			}},
		},
	}}
	b := newBrowser(t, application)
	body := b.getBody(t)

	tagDetails := between(t, body, `<div id="tag-selection">`, `<section class="panel" id="component-images-panel">`)
	for _, want := range []string{
		"Component tags", "2 of 2 selected · 1 coverage issue · 1 unverified",
		"<code>3.25.1</code>", "<code>3.25.4</code>", "Missing an included merge", "Coverage unverified",
		"GitHub checks for the selected tag", "Fact tag smoke test · success", "Waiting for GitHub check results.",
	} {
		if !strings.Contains(tagDetails, want) {
			t.Errorf("tag selection missing %q", want)
		}
	}
	if !strings.Contains(body, `<div id="tag-selection">`) {
		t.Fatal("selected tag status must not automatically fold the section")
	}
	if strings.Contains(tagDetails, "Collector test for another commit") {
		t.Fatal("selected Collector tag displayed checks from another commit")
	}
	for _, forbidden := range []string{"Confirm fact 3.25.1", "Confirm collector 3.25.4"} {
		if strings.Contains(tagDetails, forbidden) {
			t.Errorf("current selection offers redundant confirmation %q", forbidden)
		}
	}
	if !strings.Contains(tagDetails, `<table class="tag-options"`) || !strings.Contains(tagDetails, "Switch to fact 3.25.2") ||
		!strings.Contains(tagDetails, "Newer tag available <code>3.25.2</code>") || application.snapshot.Choices.Tags["fact"] != "3.25.1" {
		t.Fatal("tag alternatives should be available through an explicit switch action")
	}
	var switchForm string
	for _, form := range renderedFormRE.FindAllString(tagDetails, -1) {
		if strings.Contains(form, "Switch to fact 3.25.2") {
			switchForm = form
			break
		}
	}
	if switchForm == "" {
		t.Fatal("alternative tag switch form missing")
	}
	values := url.Values{}
	for _, field := range []string{"kind", "generation", "component", "tag", "sha", "tab"} {
		values.Set(field, formInputValue(t, switchForm, field))
	}
	if values.Get("kind") != "tag" || values.Get("generation") != "23" || values.Get("component") != "fact" || values.Get("tag") != "3.25.2" || values.Get("sha") != "fact-next-sha" {
		t.Fatalf("switch form changed alternative tag identity: %v", values)
	}
	if response := b.post(values); response.Code != 303 || response.Header().Get("Location") != "/?tab=component-releases" {
		t.Fatalf("rendered tag switch rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(application.actions) != 1 {
		t.Fatalf("switch action not recorded: %+v", application.actions)
	}
	switchAction := application.actions[0]
	if switchAction.Kind != "tag" || switchAction.Generation != 23 || switchAction.Component != "fact" || switchAction.Tag != "3.25.2" || switchAction.SHA != "fact-next-sha" {
		t.Fatalf("switch action=%+v", switchAction)
	}
	current := tagRowContaining(tagDetails, "fact-current-sha")
	if current == "" {
		t.Fatal("selected Fact tag row missing")
	}
	details := between(t, current, `data-fold-key="tag-evidence:fact:acme/fact@3.25.1"`, "</details>")
	if !strings.Contains(details, "merge-1") || !strings.Contains(details, "fact-current-sha") {
		t.Fatal("current tag SHA and merge coverage should be inside collapsed evidence")
	}
	var tagGrantForm string
	for _, form := range renderedFormRE.FindAllString(details, -1) {
		if formInputValue(t, form, "kind") == "grant" {
			tagGrantForm = form
			break
		}
	}
	if tagGrantForm == "" || !strings.Contains(tagGrantForm, `data-autosubmit`) || strings.Contains(tagGrantForm, "Save RW setting") {
		t.Fatal("selected tag grant should autosubmit without a visible save button")
	}
	if formInputValue(t, tagGrantForm, "repo") != "acme/fact" || formInputValue(t, tagGrantForm, "tag") != "3.25.1" || formInputValue(t, tagGrantForm, "sha") != "fact-current-sha" || formInputValue(t, tagGrantForm, "generation") != "23" || formInputValue(t, tagGrantForm, "csrf") != b.csrf {
		t.Fatal("tag grant form lost repository/tag/SHA identity, generation, or CSRF token")
	}
	if !strings.Contains(body, "Fact: 3.25.1") || !strings.Contains(body, "unknown") || strings.Contains(body, "Release prerequisites met") {
		t.Fatal("unknown selected-tag coverage must not be reported as ready")
	}
	if strings.Contains(activityPanel(t, body, "component-releases"), "Choose component tags") {
		t.Fatal("recommendations should not repeat the completed tag-selection task")
	}
	nav := regexp.MustCompile(`(?s)<nav\b[^>]*aria-label="Release work"[^>]*>.*?</nav>`).FindString(body)
	if nav == "" {
		t.Fatal("release sections navigation missing")
	}
	if !strings.Contains(nav, `href="/?tab=component-releases"`) || !strings.Contains(nav, `href="/?tab=component-prs"`) {
		t.Fatal("release navigation should link to Tags and PRs")
	}
}

func TestPRChoiceControlsSubmitOneExplicitDecisionAndKeepGrantSeparate(t *testing.T) {
	excluded := release.Target{Repo: "acme/fact", PR: 17}
	undecided := release.Target{Repo: "acme/fact", PR: 18}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), Generation: 31, WritesEnabled: true,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{excluded: release.Excluded}},
		Repositories: []release.Repository{{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25"}, PRs: []release.PR{
			{Target: excluded, SHA: "excluded-head-sha", Title: "Excluded PR", State: "open", Evidence: release.Evidence{Known: true, At: time.Now()}},
			{Target: undecided, SHA: "undecided-head-sha", Title: "Undecided PR", State: "open", Evidence: release.Evidence{Known: true, At: time.Now()}},
		}}},
		Authority: []dispatch.View{{Target: excluded, SHA: "excluded-head-sha", Granted: true}},
	}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	card := prCardContaining(body, "Excluded PR")
	if card == "" {
		t.Fatal("excluded PR card missing")
	}
	forms := renderedFormRE.FindAllString(card, -1)
	var choiceForm, grantForm string
	for _, form := range forms {
		switch formInputValue(t, form, "kind") {
		case "choice":
			if choiceForm != "" {
				t.Fatal("PR card has more than one choice form")
			}
			choiceForm = form
		case "grant":
			grantForm = form
		}
	}
	if choiceForm == "" || grantForm == "" {
		t.Fatal("PR card must expose separate choice and write-authority forms")
	}
	buttons := regexp.MustCompile(`<button\b[^>]*name="choice"[^>]*>`).FindAllString(choiceForm, -1)
	if len(buttons) != 3 {
		t.Fatalf("choice form has %d decision buttons, want 3", len(buttons))
	}
	wantButtons := map[string]string{"included": "Include", "excluded": "Exclude", "ignored": "Ignore &amp; hide"}
	for value, label := range wantButtons {
		if !strings.Contains(choiceForm, `name="choice" value="`+value+`"`) || !strings.Contains(choiceForm, ">"+label+"</button>") {
			t.Errorf("choice form lacks %q decision labeled %q", value, html.UnescapeString(label))
		}
	}
	if strings.Contains(card, "Save inclusion") || strings.Contains(card, "Undecided") || strings.Contains(card, `type="checkbox" name="choice"`) {
		t.Fatal("choice card contains obsolete checkbox or undecided controls")
	}
	if !strings.Contains(choiceForm, `value="excluded" aria-pressed="true"`) || strings.Contains(choiceForm, `value="included" aria-pressed="true"`) || strings.Contains(choiceForm, `value="ignored" aria-pressed="true"`) {
		t.Fatal("excluded choice should be the only active pressed decision")
	}
	undecidedCard := prCardContaining(body, "Undecided PR")
	if undecidedCard == "" || strings.Contains(undecidedCard, `aria-pressed="true"`) {
		t.Fatal("missing choice should remain undecided with no pressed decision")
	}
	if !strings.Contains(grantForm, `data-autosubmit`) || strings.Contains(grantForm, "Save RW setting") {
		t.Fatal("grant form should autosubmit without a visible save button")
	}
	if formInputValue(t, grantForm, "repo") != excluded.Repo || formInputValue(t, grantForm, "pr") != "17" || formInputValue(t, grantForm, "sha") != "excluded-head-sha" || formInputValue(t, grantForm, "generation") != "31" || formInputValue(t, grantForm, "csrf") != b.csrf {
		t.Fatal("grant form lost its PR identity, generation, or CSRF token")
	}
	if !strings.Contains(grantForm, `name="enabled" value="false"`) || !strings.Contains(grantForm, `name="enabled" value="true"`) {
		t.Fatal("grant form must submit the unchecked false value and checked true value")
	}

	for _, decision := range []struct {
		name  string
		value string
	}{
		{name: "include", value: "included"},
		{name: "exclude", value: "excluded"},
		{name: "ignore", value: "ignored"},
	} {
		t.Run(decision.name, func(t *testing.T) {
			values := url.Values{}
			for _, field := range []string{"kind", "generation", "repo", "pr", "sha"} {
				values.Set(field, formInputValue(t, choiceForm, field))
			}
			values.Set("choice", decision.value)
			if response := b.post(values); response.Code != 303 {
				t.Fatalf("rendered choice rejected: status=%d body=%s", response.Code, response.Body.String())
			}
			got := application.actions[len(application.actions)-1]
			if got.Kind != "choice" || got.Target != excluded || got.SHA != "excluded-head-sha" || got.Generation != 31 || string(got.Choice) != decision.value {
				t.Fatalf("choice action=%+v", got)
			}
		})
	}
	if len(application.actions) != 3 {
		t.Fatalf("choice submissions should not apply grant actions: %+v", application.actions)
	}
}

func (b browser) getBody(t *testing.T) string {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequestWithContext(t.Context(), "GET", "http://127.0.0.1:8080/", nil)
	r.Host = "127.0.0.1:8080"
	b.handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("GET status=%d body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func between(t *testing.T, text, start, end string) string {
	t.Helper()
	_, remainder, ok := strings.Cut(text, start)
	if !ok {
		t.Fatalf("opening marker %q missing", start)
	}
	content, _, ok := strings.Cut(remainder, end)
	if !ok {
		t.Fatalf("closing marker %q missing", end)
	}
	return content
}

func formInputValue(t *testing.T, form, name string) string {
	t.Helper()
	return inputValue(t, form, name)
}

func prCardContaining(body, title string) string {
	for _, card := range regexp.MustCompile(`(?s)<article class="pr-card"[^>]*>.*?</article>`).FindAllString(body, -1) {
		if strings.Contains(card, title) {
			return card
		}
	}
	return ""
}

func tagRowContaining(body, marker string) string {
	for _, row := range regexp.MustCompile(`(?s)<article class="component-tag-row"[^>]*>.*?</article>`).FindAllString(body, -1) {
		if strings.Contains(row, marker) {
			return row
		}
	}
	return ""
}
