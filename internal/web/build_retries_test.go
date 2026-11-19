package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestBuildRetriesMergeOnlyExactComments(t *testing.T) {
	config := validACS5Config()
	for tab, target := range map[string]release.Target{
		"component-releases": {Repo: config.Components[0].Repo, Tag: "3.25.7"},
		"acs-prs":            {Repo: config.ACS().Repo, PR: 82},
	} {
		t.Run(tab, func(t *testing.T) {
			verified := release.Proposal{Target: target, SHA: "head-sha", Job: "build", RunID: "123", Payload: "verified retry comment"}
			copyOnly := verified
			copyOnly.Payload = "manual-only retry comment for the same job and run"
			waiting := verified
			waiting.RunID = "124"
			waiting.Payload = "retry comment in cooldown"
			until := time.Date(2026, 11, 18, 10, 45, 0, 0, time.UTC)
			choices := release.Choices{
				Tags: map[string]string{config.Components[0].Name: "3.25.7"},
				PRs:  map[release.Target]release.Choice{target: release.Included},
			}
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: config, Choices: choices, WritesEnabled: true, Interactive: true, Generation: 12,
				Plans: []release.Plan{{
					Target: target, SHA: verified.SHA, Message: "Observed build failures",
					Proposals: []release.Proposal{verified, copyOnly},
					Warnings:  []string{"A run observation is incomplete"},
				}},
				Authority: []dispatch.View{{
					Target: target, SHA: verified.SHA, Granted: true,
					Offers: []dispatch.Offer{
						{ID: dispatch.ID(verified), Proposal: verified, CanApprove: true},
						{ID: dispatch.ID(waiting), Proposal: waiting, Reason: "waiting for comment rate limit", WaitingUntil: until},
					},
				}},
			}}
			b := newBrowser(t, application)
			retrySection := "<section class=\"panel\" id=\"" + tab + "-retry\">"
			body := between(t, activityPanel(t, b.getBody(t), tab), retrySection, "</section>")
			heading := "Konflux check retries"
			if tab == "acs-prs" {
				heading = "Checks of Included PRs"
			}
			for _, want := range []string{heading, "Check details", "A run observation is incomplete"} {
				if !strings.Contains(body, want) {
					t.Errorf("Build retries missing %q", want)
				}
			}
			if got := strings.Count(body, `class="build-retry-row"`); got != 3 {
				t.Fatalf("got %d retry rows, want one per exact comment", got)
			}
			for _, proposal := range []release.Proposal{verified, copyOnly, waiting} {
				if got := strings.Count(body, "<pre><code>"+proposal.Payload+"</code></pre>"); got != 1 {
					t.Errorf("comment %q appeared %d times, want one", proposal.Payload, got)
				}
			}
			row := func(proposal release.Proposal) string {
				return between(t, body, `data-fold-key="build-retry:`+dispatch.ID(proposal)+`"`, "</details>")
			}
			send := formContaining(t, row(verified), "Send one comment")
			if formInputValue(t, send, "id") != dispatch.ID(verified) || formInputValue(t, send, "sha") != verified.SHA ||
				formInputValue(t, send, "tab") != tab || !strings.Contains(send, `name="confirm" value="yes" required`) {
				t.Fatal("the exact confirmation lost its offer identity or tab")
			}
			if manual := row(copyOnly); !strings.Contains(manual, "Copy only") || strings.Contains(manual, "<form") {
				t.Fatal("the unmatched proposal inherited an offer's actions")
			}
			if cooldown := row(waiting); !strings.Contains(cooldown, "waiting for comment rate limit · until") ||
				strings.Contains(cooldown, "Send one comment") {
				t.Fatal("the cooldown status or actions are incorrect")
			}
			application.snapshot.Interactive = false
			auto := between(t, activityPanel(t, b.getBody(t), tab), retrySection, "</section>")
			if !strings.Contains(auto, "Eligible for automatic retry") {
				t.Fatal("automatic mode should explain that an eligible retry can run automatically")
			}
		})
	}
}

func TestKonfluxRetryNextAction(t *testing.T) {
	evidence := release.Evidence{Known: true, At: time.Now()}
	for name, tc := range map[string]struct {
		plan release.Plan
		want string
	}{
		"admission denied":     {release.Plan{Action: "Wait", StartFailures: []release.StartFailure{{Category: "admission_policy", Evidence: evidence}}}, "Investigate the admission policy"},
		"endpoint unavailable": {release.Plan{KonfluxAPIUnavailable: true}, "Wait for the Konflux API to recover"},
		"waiting for check":    {release.Plan{Action: "Wait", Runs: []release.Run{{ID: "123", Job: "main", Status: "in_progress"}}}, "Wait for the main check to finish"},
		"no matching run":      {release.Plan{Action: "Wait", Message: "No verified build checks match the requested tag or PR."}, "Wait for a matching Konflux build check"},
	} {
		t.Run(name, func(t *testing.T) {
			g := buildRetryTarget{Plan: &planModel{Plan: tc.plan}}
			if got := g.nextAction(true); !strings.HasPrefix(got, tc.want) {
				t.Errorf("next action = %q, want %q", got, tc.want)
			}
		})
	}
	group := buildRetryTarget{Candidates: []*buildRetryCandidate{{Offer: &dispatch.Offer{CommentSent: true}}}}
	if got := group.nextAction(true); !strings.Contains(got, "after the retry comment") {
		t.Errorf("next action after delivery = %q", got)
	}
}

func TestACSChecksGroupActionsByIncludedPR(t *testing.T) {
	s := workflowStatusSnapshot()
	acs := &s.Repositories[len(s.Repositories)-1]
	target := func(n int) release.Target { return release.Target{Repo: acs.Source.Repo, PR: n} }
	for _, n := range []int{42, 41, 7} {
		s.Choices.PRs[target(n)] = release.Included
	}
	acs.PRs = []release.PR{
		{Target: target(7), State: "closed", Merged: true},
		{Target: target(41), State: "open", Title: "Update release images"},
	}
	s.Choices.PRs[target(43)] = release.Excluded
	s.Plans = []release.Plan{{Target: target(41), Proposals: []release.Proposal{
		{Target: target(41), SHA: "head", RunID: "2", Job: "build", Payload: "/konflux-retest build"},
	}}}
	s.ManualRetries = []release.ManualRetryReport{
		{Target: target(41), SHA: "head", Jobs: []release.ManualJob{{ID: 201}},
			Prow: []release.ProwRetry{{Command: "/test unit"}}},
		{Target: target(43), Warnings: []string{"excluded result"}},
	}
	panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-prs")
	checks := between(t, panel, `id="acs-prs-retry"`, "</section>")
	if strings.Count(checks, "<article ") != 3 || strings.Contains(panel, "Manual CI retries") || strings.Contains(panel, "Konflux check retries") || strings.Contains(checks, "excluded result") {
		t.Fatalf("unexpected ACS check groups: %s", checks)
	}
	active := between(t, checks, `id="acs-pr-retries-41"`, "</article>")
	heading := between(t, active, "<header", "</header>")
	if !strings.Contains(heading, "<h3>Update release images</h3>") ||
		!strings.Contains(heading, `href="https://github.com/`+target(41).Repo+`/pull/41"`) ||
		!strings.Contains(heading, ">"+target(41).String()+"</a>") {
		t.Errorf("unexpected PR heading: %s", heading)
	}
	for _, action := range []string{"/konflux-retest build", "actions/jobs/201/rerun", "/test unit"} {
		if strings.Count(active, action) != 1 || strings.Count(checks, action) != 1 {
			t.Errorf("expected %q only under PR 41", action)
		}
	}
	merged := between(t, checks, `id="acs-pr-retries-7"`, "</article>")
	pending := between(t, checks, `id="acs-pr-retries-42"`, "</article>")
	if !strings.Contains(pending, "<h3>"+target(42).String()+"</h3>") || !strings.Contains(pending, ">Open PR</a>") {
		t.Errorf("missing fallback PR heading: %s", pending)
	}
	if !strings.Contains(merged, `<span class="pill yes">Merged</span>`) || !strings.Contains(pending, "Wait for the first Konflux check observation") {
		t.Fatal("missing merged or pending PR status")
	}
}

func TestACSChecksShowProgressPerPR(t *testing.T) {
	s := workflowStatusSnapshot()
	pr := func(n int) release.Target { return release.Target{Repo: s.Config.ACS().Repo, PR: n} }
	s.ManualRetries = []release.ManualRetryReport{
		{Target: pr(1), Pending: true},
		{Target: pr(2), Incomplete: true, Warnings: []string{"Actions read deferred"}, Prow: []release.ProwRetry{{Command: "/test unit"}}},
		{Target: pr(3)},
		{Target: pr(4), Jobs: []release.ManualJob{{ID: 201}}},
	}
	for _, r := range s.ManualRetries {
		s.Choices.PRs[r.Target] = release.Included
	}
	s.Plans = []release.Plan{{Target: pr(1), StartFailures: []release.StartFailure{{Category: "admission_policy", Evidence: release.Evidence{Known: true, At: time.Now()}}}}}
	panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-prs")
	for id, want := range map[string][]string{
		"1": {"Loading GitHub Actions and Prow results", "Investigate the admission policy"},
		"2": {"results are incomplete", "/test unit", "Actions read deferred"},
		"3": {"No verified GitHub Actions or Prow retries"},
		"4": {"actions/jobs/201/rerun"},
	} {
		card := between(t, panel, `id="acs-pr-retries-`+id+`"`, "</article>")
		for _, fragment := range want {
			if !strings.Contains(card, fragment) {
				t.Errorf("PR %s is missing %q", id, fragment)
			}
		}
		if id != "1" && strings.Contains(card, "Loading GitHub Actions and Prow results") {
			t.Errorf("PR %s still shows a loading message", id)
		}
	}
}
