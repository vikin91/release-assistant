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
			for _, want := range []string{"Konflux check retries", "Next action:", "Check details", "A run observation is incomplete"} {
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
