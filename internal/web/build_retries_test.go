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
			if target.PR > 0 {
				verified.Job, verified.Payload = "central-db-on-push", "/konflux-retest central-db-on-push"
				copyOnly.Job, copyOnly.Payload = "retag-collector", "/konflux-retest retag-collector"
				waiting.Job, waiting.Payload = "retag-scanner", "/konflux-retest retag-scanner"
			}
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
					Advisories: []release.Advisory{
						{ID: "dev", Name: "Red Hat Konflux / acs-conforma-dev-5-0 / main", Message: "developer finding"},
						{ID: "prod", Name: "Red Hat Konflux / acs-conforma-prod / main", Message: "production finding"},
					},
				}},
				Authority: []dispatch.View{{
					Target: target, SHA: verified.SHA, Granted: true,
					Offers: []dispatch.Offer{
						{ID: dispatch.ID(verified), Proposal: verified, CanApprove: true, Queueable: true},
						{ID: dispatch.ID(waiting), Proposal: waiting, Queueable: true, Reason: "waiting for comment rate limit", WaitingUntil: until},
					},
				}},
			}}
			b := newBrowser(t, application)
			retrySection := "<section class=\"panel\" id=\"" + tab + "-retry\">"
			body := between(t, activityPanel(t, b.getBody(t), tab), retrySection, "</section>")
			if target.PR > 0 {
				comment := strings.Join([]string{verified.Payload, copyOnly.Payload, waiting.Payload}, "\n")
				for _, want := range []string{
					"Checks of Included PRs", "<pre><code>" + comment + "</code></pre>", "Copy comment", "Open Retest Queue",
					"Comment on PR #82 on GitHub", "<summary>Conforma dev checks</summary>", "developer finding", `name="id" value="dev"`,
				} {
					if strings.Count(body, want) != 1 {
						t.Errorf("expected one occurrence of %q in the PR retry card", want)
					}
				}
				for _, absent := range []string{`class="build-retry-row"`, "Check details", "A run observation is incomplete", "production finding"} {
					if strings.Contains(body, absent) {
						t.Errorf("PR retry card still shows %q", absent)
					}
				}
				return
			}
			for _, want := range []string{"Konflux check retries", "Check details", "A run observation is incomplete", "production finding"} {
				if !strings.Contains(body, want) {
					t.Errorf("Build retries missing %q", want)
				}
			}
			if got := strings.Count(body, `class="build-retry-row"`); got != 3 {
				t.Fatalf("got %d retry rows, want one per exact comment", got)
			}
			for _, proposal := range []release.Proposal{verified, copyOnly, waiting} {
				markup := "<pre><code>" + proposal.Payload + "</code></pre>"
				if got := strings.Count(body, markup); got != 1 {
					t.Errorf("comment %q appeared %d times, want one", proposal.Payload, got)
				}
			}
			row := func(proposal release.Proposal) string {
				return between(t, body, `data-fold-key="build-retry:`+dispatch.ID(proposal)+`"`, "</details>")
			}
			if queued := row(verified); !strings.Contains(queued, "Open Retest Queue") || strings.Contains(queued, "<form") {
				t.Fatal("the verified proposal should link to the queue for confirmation")
			}
			if manual := row(copyOnly); !strings.Contains(manual, "Copy only") || strings.Contains(manual, "<form") {
				t.Fatal("the unmatched proposal inherited an offer's actions")
			}
			if cooldown := row(waiting); !strings.Contains(cooldown, "waiting for comment rate limit · until") ||
				!strings.Contains(cooldown, "Open Retest Queue") || strings.Contains(cooldown, "<form") {
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
		"admission denied":     {release.Plan{Action: "Wait", StartFailures: []release.StartFailure{{Category: "admission_policy", Evidence: evidence}}}, "A PipelineRun failed to start"},
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
			Prow: []release.ProwRetry{{Context: "ci/prow/unit", Command: "/test unit"}}},
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
	for _, action := range []string{"/konflux-retest build", "actions/jobs/201/rerun", "ci/prow/unit"} {
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
		{Target: pr(2), Incomplete: true, Warnings: []string{"Actions read deferred"}, Prow: []release.ProwRetry{{Context: "ci/prow/unit", Command: "/test unit"}}},
		{Target: pr(3)},
		{Target: pr(4), Jobs: []release.ManualJob{{ID: 201}}},
	}
	for _, r := range s.ManualRetries {
		s.Choices.PRs[r.Target] = release.Included
	}
	s.Plans = []release.Plan{{Target: pr(1), StartFailures: []release.StartFailure{{Category: "admission_policy", Evidence: release.Evidence{Known: true, At: time.Now()}}}}}
	panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-prs")
	for id, want := range map[string][]string{
		"1": {"Loading GitHub Actions and Prow results", "A PipelineRun failed to start"},
		"2": {"results are incomplete", "ci/prow/unit", "Actions read deferred"},
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

func TestACSShowsRequiredKonfluxCreationFailuresByExactCheck(t *testing.T) {
	s := workflowStatusSnapshot()
	acs := &s.Repositories[len(s.Repositories)-1]
	target := release.Target{Repo: s.Config.ACS().Repo, PR: 24152}
	observed := release.Evidence{Known: true, At: time.Now()}
	pr := release.PR{
		Target: target, SHA: "head", State: "open", Base: acs.Source.Branch,
		Evidence: observed, ChecksEvidence: observed,
	}
	plan := release.Plan{Target: target, SHA: pr.SHA, Action: "Wait", Evidence: observed}
	for i, name := range []string{"checks", "create-custom-snapshot", "operator-index-on-push", "operator-index-on-push", "optional-job", "ordinary-failure"} {
		id := int64(i + 1)
		check := release.Check{CheckRunID: id, Name: "Red Hat Konflux / " + name, Required: release.Yes, Conclusion: "failure"}
		if name == "optional-job" {
			check.Required = release.No
		}
		pr.Checks = append(pr.Checks, check)
		if name != "ordinary-failure" {
			plan.StartFailures = append(plan.StartFailures, release.StartFailure{CheckID: id, Job: name, Category: "admission_policy", Evidence: observed})
		}
	}
	acs.PRs = append(acs.PRs, pr)
	s.Choices.PRs[target] = release.Included
	s.Plans = []release.Plan{plan}
	application := &fakeApplication{snapshot: s}
	browser := newBrowser(t, application)
	row := func() string {
		return between(t, browser.getBody(t), `id="acs-pr-retries-24152"`, `</article>`)
	}
	want := "3 required checks failed to start (checks, create-custom-snapshot, operator-index-on-push). Waiting for new attempts."
	if got := row(); !strings.Contains(got, want) || strings.Contains(got, "Investigate the admission policy") {
		t.Fatalf("missing required failure summary: %s", got)
	}
	for i := range application.snapshot.Plans[0].StartFailures {
		application.snapshot.Plans[0].StartFailures[i].Superseded = true
	}
	if got := row(); strings.Contains(got, "failed to start") {
		t.Fatalf("superseded failures still shown as the next action: %s", got)
	}
}
