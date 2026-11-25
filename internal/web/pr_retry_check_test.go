package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestIncludedPRShowsInlineRetryCommands(t *testing.T) {
	for _, tab := range []string{"fact-prs", "acs-prs"} {
		t.Run(tab, func(t *testing.T) {
			checkIncludedPRRetryCommands(t, tab)
		})
	}
}

func checkIncludedPRRetryCommands(t *testing.T, tab string) {
	t.Helper()
	snapshot := workflowStatusSnapshot()
	repository := &snapshot.Repositories[len(snapshot.Repositories)-1]
	if tab == "fact-prs" {
		repository = &snapshot.Repositories[0]
	}
	observed := release.Evidence{Known: true, At: time.Now()}
	included := release.Target{Repo: repository.Source.Repo, PR: 41}
	undecided := release.Target{Repo: repository.Source.Repo, PR: 42}
	for _, target := range []release.Target{included, undecided} {
		repository.PRs = append(repository.PRs, release.PR{
			Target: target, State: "open", Base: repository.Source.Branch, SHA: "head", Evidence: observed,
		})
	}
	snapshot.Choices.PRs[included] = release.Included
	const contract = "unsupported retag input contract: invalid .tekton/retag-pipeline.yaml: unsupported pinned task implementation"
	const milestone = "Milestone 1 is not ready: main-on-push (queued), retag-scanner (input unavailable)."
	snapshot.PRRetryChecks = map[release.Target]app.PRRetryCheck{
		included: {
			SHA: "head", Started: observed.At,
			Konflux: release.Plan{Target: included, SHA: "head", Evidence: observed, Proposals: []release.Proposal{
				{Target: included, SHA: "head", Payload: "/konflux-retest checks"},
				{Target: included, SHA: "head", Payload: "/konflux-retest main"},
				{Target: included, SHA: "head", Payload: "/konflux-retest checks"},
				{Target: included, SHA: "old-head", Payload: "stale command"},
			}},
			CI: release.ManualRetryReport{Target: included, SHA: "head", At: observed.At,
				Jobs: []release.ManualJob{{ID: 201}}, Prow: []release.ProwRetry{{Command: "/test unit"}},
				Incomplete: true, Warnings: []string{"GitHub reported a rate limit (HTTP 403). Release Assistant paused GitHub reads"}},
		},
	}
	check := snapshot.PRRetryChecks[included]
	check.Konflux.Evidence.Error = contract
	check.Konflux.Evidence.At = observed.At.Add(-2 * time.Minute)
	check.CI.At = observed.At.Add(-3 * time.Minute)
	check.Konflux.Warnings = []string{milestone, "retag-scanner: cannot resolve input requirements: " + contract + "."}
	snapshot.PRRetryChecks[included] = check
	jobCommand := "gh api --method POST repos/" + included.Repo + "/actions/jobs/201/rerun"
	snapshot.RetestQueue = []app.RetestItem{
		{ID: 1, Target: included, Kind: "konflux", Payload: "/konflux-retest main"},
		{ID: 2, Target: included, Kind: "konflux", Payload: "/konflux-retest checks"},
		{ID: 3, Target: included, Kind: "prow", Payload: "/test unit"},
		{ID: 4, Target: included, Kind: "actions", Payload: jobCommand},
	}
	application := &fakeApplication{snapshot: snapshot}
	browser := newBrowser(t, application)
	panel := activityPanel(t, browser.getBody(t), tab)
	field := between(t, panel, `id="`+prRetryAnchor(included)+`"`, "</article>")
	statusForm := formContaining(t, field, `class="pr-check-status muted"`)
	if !strings.Contains(statusForm, "Konflux checked <time") || !strings.Contains(statusForm, "CI checked <time") ||
		strings.Count(statusForm, "minutes ago</time> (incomplete)") != 2 ||
		!strings.Contains(statusForm, `title="`+formatTime(check.Konflux.Evidence.At)+`"`) ||
		!strings.Contains(statusForm, `<button type="submit">Refresh checks</button>`) {
		t.Fatalf("expected check ages, timestamp and refresh action in one status line: %s", statusForm)
	}
	wantComment := "<pre><code>/konflux-retest checks\n/konflux-retest main\n/test unit</code></pre>"
	if !strings.Contains(field, wantComment) || !strings.Contains(field, "<pre><code>"+jobCommand+"</code></pre>") ||
		!strings.Contains(field, "Open Retest Queue") || strings.Contains(panel, "Review retry") {
		t.Fatalf("retry checker did not show its inline fields: %s", field)
	}
	if !strings.Contains(field, `class="error-text"><strong>CI Retest · Error =</strong> Rate limited by GitHub`) ||
		strings.Contains(field, "Retry diagnostics") {
		t.Fatal("the on-demand checker should show the external rate limit inline")
	}
	if strings.Count(field, contract) != 1 ||
		!strings.Contains(field, "Konflux Retest · Retag input validation · Error =</strong> retag-scanner: "+contract) ||
		!strings.Contains(field, "Konflux Retest · Planner · Warning =</strong> "+milestone) {
		t.Fatalf("expected one sourced contract error and a separate planner warning: %s", field)
	}
	if strings.Count(panel, "/konflux-retest checks") != 1 || strings.Contains(panel, "stale command") ||
		strings.Count(panel, "Refresh checks") != 1 || strings.Count(panel, "Copy comments") != 1 ||
		strings.Contains(panel, `id="`+prRetryAnchor(undecided)+`"`) {
		t.Fatal("expected one inline action and comment field on the included PR only")
	}
	response := browser.post(url.Values{
		"kind": {"check-pr-retries"}, "tab": {tab}, "repo": {included.Repo}, "sha": {"head"},
		"pr": {strconv.Itoa(included.PR)}, "generation": {strconv.FormatUint(snapshot.Generation, 10)},
	})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab="+tab+"#"+prRetryAnchor(included) ||
		len(application.actions) != 1 || application.actions[0].Kind != "check-pr-retries" || application.actions[0].Target != included {
		t.Fatalf("unexpected retry action response %d and actions %v", response.Code, application.actions)
	}
}
