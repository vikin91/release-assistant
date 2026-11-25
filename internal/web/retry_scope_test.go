package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestRetryAndCommentSectionsUseExplicitCurrentScope(t *testing.T) {
	included := release.Target{Repo: "acme/fact", PR: 21}
	excluded := release.Target{Repo: "acme/fact", PR: 22}
	undecided := release.Target{Repo: "acme/fact", PR: 23}
	ignored := release.Target{Repo: "acme/fact", PR: 24}
	merged := release.Target{Repo: "acme/fact", PR: 25}
	closed := release.Target{Repo: "acme/fact", PR: 26}
	factTag := release.Target{Repo: "acme/fact", Tag: "3.25.2"}
	collectorTag := release.Target{Repo: "acme/collector", Tag: "3.25.4"}
	oldFactTag := release.Target{Repo: "acme/fact", Tag: "3.25.1"}
	unacceptedTag := release.Target{Repo: "acme/fact", Tag: "3.25.9"}

	proposal := func(target release.Target, sha, marker string) release.Plan {
		return release.Plan{
			Target: target, SHA: sha, Message: marker, Evidence: release.Evidence{Error: "partial plan evidence"},
			Proposals: []release.Proposal{{Target: target, SHA: sha, Job: marker, RunID: "run-" + marker, Payload: marker}},
		}
	}
	offer := func(target release.Target, sha, marker string) dispatch.View {
		comment := marker
		if target.Tag != "" {
			comment += "-marker"
		}
		return dispatch.View{Target: target, SHA: sha, Granted: marker == "included-open" || marker == "excluded", Offers: []dispatch.Offer{{
			ID:       "offer-" + marker,
			Proposal: release.Proposal{Target: target, SHA: sha, Job: comment, RunID: "run-" + comment, Payload: comment},
			Allowed:  true, CanApprove: true,
		}}}
	}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), Generation: 44, WritesEnabled: true,
		Choices: release.Choices{
			Tags: map[string]string{"fact": "3.25.2", "collector": "3.25.4"},
			PRs: map[release.Target]release.Choice{
				included: release.Included, excluded: release.Excluded, ignored: release.Ignored, merged: release.Included, closed: release.Included,
			},
		},
		Repositories: []release.Repository{{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25"}, PRs: []release.PR{
			{Target: included, SHA: "included-sha", Title: "Included open PR", State: "open", Base: "release-3.25", Evidence: release.Evidence{Error: "identity evidence partial"}},
			{Target: excluded, SHA: "excluded-sha", Title: "Excluded open PR", State: "open"},
			{Target: undecided, SHA: "undecided-sha", Title: "Undecided open PR", State: "open"},
			{Target: ignored, SHA: "ignored-sha", Title: "Ignored open PR", State: "open"},
			{Target: merged, SHA: "merged-sha", Title: "Merged PR", State: "merged", Merged: true},
			{Target: closed, SHA: "closed-sha", Title: "Closed PR", State: "closed"},
		}}},
		RetestQueue: []app.RetestItem{{ID: 1, Target: included, Kind: "konflux", Payload: "included-open-marker"}},
		Plans: []release.Plan{
			proposal(included, "included-sha", "included-open-marker"),
			proposal(excluded, "excluded-sha", "excluded-marker"),
			proposal(undecided, "undecided-sha", "undecided-marker"),
			proposal(ignored, "ignored-sha", "ignored-marker"),
			proposal(merged, "merged-sha", "merged-marker"),
			proposal(closed, "closed-sha", "closed-marker"),
			proposal(factTag, "fact-tag-sha", "accepted-fact-tag-marker"),
			proposal(collectorTag, "collector-tag-sha", "accepted-collector-tag-marker"),
			proposal(oldFactTag, "old-tag-sha", "old-tag-marker"),
			proposal(unacceptedTag, "unaccepted-tag-sha", "unaccepted-tag-marker"),
		},
		Authority: []dispatch.View{
			offer(included, "included-sha", "included-open"),
			offer(excluded, "excluded-sha", "excluded"),
			offer(undecided, "undecided-sha", "undecided"),
			offer(ignored, "ignored-sha", "ignored"),
			offer(merged, "merged-sha", "merged"),
			offer(closed, "closed-sha", "closed"),
			offer(factTag, "fact-tag-sha", "accepted-fact-tag"),
			offer(collectorTag, "collector-tag-sha", "accepted-collector-tag"),
			offer(oldFactTag, "old-tag-sha", "old-tag"),
			offer(unacceptedTag, "unaccepted-tag-sha", "unaccepted-tag"),
		},
	}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	inline := prCardContaining(activityPanel(t, body, "fact-prs"), "Included open PR")
	for _, want := range []string{"Refresh checks", "<pre><code>included-open-marker</code></pre>", "Open Retest Queue"} {
		if strings.Count(inline, want) != 1 {
			t.Errorf("included PR should show %q once", want)
		}
	}
	if strings.Contains(inline, "Review retry") || strings.Contains(inline, "#new_comment_field") {
		t.Fatal("partial identity should show queue access and inline commands")
	}
	builds := between(t, body, `<section class="panel" id="component-releases-retry">`, `</section>`)
	for _, includedMarker := range []string{"accepted-fact-tag-marker", "accepted-collector-tag-marker"} {
		if !strings.Contains(builds, includedMarker) {
			t.Errorf("Build retries omitted scoped target marker %q", includedMarker)
		}
	}
	for _, link := range []string{
		`href="https://github.com/acme/fact/commit/fact-tag-sha" target="_blank"`,
		`href="https://github.com/acme/collector/commit/collector-tag-sha" target="_blank"`,
	} {
		if got := strings.Count(builds, link); got != 1 {
			t.Errorf("Build retries rendered %d instances of the manual comment link %s, want one", got, link)
		}
	}
	if !strings.Contains(builds, "partial plan evidence") || !strings.Contains(inline, "partial plan evidence") {
		t.Error("partial plan evidence was hidden")
	}
	for _, excludedMarker := range []string{
		"excluded-marker", "undecided-marker", "ignored-marker", "merged-marker", "closed-marker",
		"old-tag-marker", "unaccepted-tag-marker", "offer-excluded", "offer-undecided", "offer-ignored",
		"offer-merged", "offer-closed", "offer-old-tag", "offer-unaccepted-tag",
	} {
		if strings.Contains(inline+builds, excludedMarker) {
			t.Errorf("retry view contains out-of-scope marker %q", excludedMarker)
		}
	}
	for _, title := range []string{"Excluded open PR", "Undecided open PR"} {
		card := prCardContaining(body, title)
		if card == "" {
			t.Errorf("ordinary PR selection omitted %q", title)
			continue
		}
		if !strings.Contains(card, `name="choice" value="excluded" aria-pressed="true"`) && title == "Excluded open PR" {
			t.Errorf("excluded PR %q lost its checked choice state", title)
		}
		if strings.Contains(card, `name="choice" value="included" aria-pressed="true"`) && title == "Undecided open PR" {
			t.Errorf("undecided PR %q rendered as included", title)
		}
	}
	includedCard := prCardContaining(body, "Included open PR")
	if includedCard == "" || !strings.Contains(includedCard, `name="enabled" value="true" checked`) {
		t.Fatal("included PR lost its independently granted RW checked state")
	}
	excludedCard := prCardContaining(body, "Excluded open PR")
	if excludedCard == "" || !strings.Contains(excludedCard, `name="enabled" value="true" checked`) {
		t.Fatal("excluded PR lost its independent RW checked state when retry panels hide it")
	}
	ignoredCard := prCardContaining(body, "Ignored open PR")
	if !strings.Contains(ignoredCard, `data-release-choice="ignored"`) || strings.Contains(ignoredCard, `name="kind" value="grant"`) {
		t.Fatal("ignored PR should be recoverable from the ignored filter without retry permission controls")
	}
}

func TestRetryProposalsDoNotRequireSelectionConfirmation(t *testing.T) {
	application := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config(), Generation: 12}}
	body := newBrowser(t, application).getBody(t)
	prs := activityPanel(t, body, "fact-prs")
	if strings.Contains(body, "Finish PR selection") || strings.Contains(body, "finish-selection") {
		t.Fatal("activity tabs retained the global selection gate")
	}
	if !strings.Contains(prs, "No open PR observations") || strings.Contains(prs, "Review retry") {
		t.Fatal("empty component PR view displayed a retry action")
	}
}

func TestAdvisoryReviewAndUndoFormsSubmitBoundActions(t *testing.T) {
	config := validACS5Config()
	source := config.ACS()
	target := release.Target{Repo: source.Repo, PR: 31}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: validACS5Config(), Generation: 57,
		Choices:      release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}, Acknowledged: map[string]bool{"advisory-31": true}},
		Repositories: []release.Repository{{Source: source, PRs: []release.PR{{Target: target, SHA: "review-sha", Base: source.Branch, State: "open"}}}},
		Plans: []release.Plan{{
			Target: target, SHA: "review-sha", Evidence: release.Evidence{Known: false, Error: "retry evidence unavailable"},
			Advisories: []release.Advisory{{ID: "advisory-31", Name: "Red Hat Konflux / acs-conforma-dev-5-0 / main", Message: "Conforma review finding", URL: "https://evidence.example/advisory-31"}},
		}},
	}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	if !strings.Contains(body, "Reviewed for this session") ||
		!strings.Contains(body, "retry evidence unavailable") || !strings.Contains(body, "Conforma review finding") ||
		!strings.Contains(body, "https://evidence.example/advisory-31") {
		t.Fatal("reviewed advisory must retain its failure message and evidence")
	}
	for _, help := range []string{"for this session", "action-log entry", "check results", "retry eligibility", "write permissions", "release readiness", "Reviews reset on restart"} {
		if !strings.Contains(body, help) {
			t.Errorf("advisory help does not explain session-local review scope: missing %q", help)
		}
	}
	if strings.Contains(body, "Acknowledge advisory") || strings.Contains(body, "Save acknowledgement") {
		t.Fatal("advisory review UI retained checkbox/save acknowledgement controls")
	}

	var undoForm string
	for _, form := range renderedFormRE.FindAllString(body, -1) {
		if formInputValue(t, form, "kind") == "acknowledge" && strings.Contains(form, "Mark as unreviewed") {
			undoForm = form
			break
		}
	}
	if undoForm == "" {
		t.Fatal("reviewed advisory does not offer a Mark as unreviewed form")
	}
	assertReviewFormFields(t, undoForm, b, target, "review-sha", "advisory-31", 57, false, "Mark as unreviewed")
	if response := b.post(reviewFormValues(t, undoForm)); response.Code != 303 {
		t.Fatalf("rendered undo form rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(application.actions) != 1 || application.actions[0].Kind != "acknowledge" || application.actions[0].Target != target ||
		application.actions[0].SHA != "review-sha" || application.actions[0].ID != "advisory-31" ||
		application.actions[0].Generation != 57 || application.actions[0].Enabled {
		t.Fatalf("undo action=%+v", application.actions)
	}

	application.snapshot.Choices.Acknowledged["advisory-31"] = false
	body = b.getBody(t)
	var reviewForm string
	for _, form := range renderedFormRE.FindAllString(body, -1) {
		if formInputValue(t, form, "kind") == "acknowledge" && strings.Contains(form, "Mark as reviewed") {
			reviewForm = form
			break
		}
	}
	if reviewForm == "" {
		t.Fatal("unreviewed advisory does not offer a Mark as reviewed form")
	}
	assertReviewFormFields(t, reviewForm, b, target, "review-sha", "advisory-31", 57, true, "Mark as reviewed")
	if response := b.post(reviewFormValues(t, reviewForm)); response.Code != 303 {
		t.Fatalf("rendered review form rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(application.actions) != 2 || application.actions[1].Kind != "acknowledge" || application.actions[1].Target != target ||
		application.actions[1].SHA != "review-sha" || application.actions[1].ID != "advisory-31" ||
		application.actions[1].Generation != 57 || !application.actions[1].Enabled {
		t.Fatalf("review action=%+v", application.actions)
	}
}

func assertReviewFormFields(t *testing.T, form string, b browser, target release.Target, sha, id string, generation uint64, enabled bool, button string) {
	t.Helper()
	for field, want := range map[string]string{
		"kind": "acknowledge", "generation": strconv.FormatUint(generation, 10), "repo": target.Repo, "pr": "31", "tag": "", "sha": sha, "id": id, "enabled": map[bool]string{true: "true", false: "false"}[enabled], "csrf": b.csrf,
	} {
		if got := formInputValue(t, form, field); got != want {
			t.Errorf("review form field %s=%q, want %q", field, got, want)
		}
	}
	if strings.Contains(form, `type="checkbox"`) || !strings.Contains(form, ">"+button+"</button>") {
		t.Errorf("review form has wrong control for %q: %s", button, form)
	}
	if got := strings.Count(form, `<button`); got != 1 {
		t.Errorf("review form has %d buttons, want one", got)
	}
	if got := strings.Count(form, `name="enabled"`); got != 1 {
		t.Errorf("review form has %d enabled fields, want one hidden value", got)
	}
}

func reviewFormValues(t *testing.T, form string) url.Values {
	t.Helper()
	values := url.Values{}
	for _, field := range []string{"kind", "generation", "repo", "pr", "tag", "sha", "id", "enabled"} {
		values.Set(field, formInputValue(t, form, field))
	}
	return values
}
