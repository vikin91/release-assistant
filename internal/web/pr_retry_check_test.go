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

func TestIncludedPRLinksToItsSingleKonfluxComment(t *testing.T) {
	snapshot := workflowStatusSnapshot()
	repository := &snapshot.Repositories[len(snapshot.Repositories)-1]
	observed := release.Evidence{Known: true, At: time.Now()}
	included := release.Target{Repo: repository.Source.Repo, PR: 41}
	undecided := release.Target{Repo: repository.Source.Repo, PR: 42}
	for _, target := range []release.Target{included, undecided} {
		repository.PRs = append(repository.PRs, release.PR{
			Target: target, State: "open", Base: repository.Source.Branch, SHA: "head", Evidence: observed,
		})
	}
	snapshot.Choices.PRs[included] = release.Included
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
				Jobs: []release.ManualJob{{ID: 201}}, Prow: []release.ProwRetry{{Command: "/test unit"}}},
		},
	}
	application := &fakeApplication{snapshot: snapshot}
	browser := newBrowser(t, application)
	panel := activityPanel(t, browser.getBody(t), "acs-prs")
	field := between(t, panel, `id="`+prRetryAnchor(included)+`"`, "</article>")
	want := "gh api --method POST repos/" + included.Repo + "/actions/jobs/201/rerun\n/test unit"
	if !strings.Contains(field, "<pre><code>"+want+"</code></pre>") ||
		!strings.Contains(field, `href="/?tab=acs-prs#acs-pr-retries-41"`) || strings.Contains(field, "/konflux-retest") {
		t.Fatalf("retry checker did not link to the Konflux comment: %s", field)
	}
	comment := between(t, panel, `id="acs-pr-retries-41"`, "</article>")
	if !strings.Contains(comment, "<pre><code>/konflux-retest checks\n/konflux-retest main</code></pre>") ||
		strings.Count(panel, "/konflux-retest checks") != 1 || strings.Contains(panel, "stale command") {
		t.Fatalf("Konflux comment did not contain the unique current commands: %s", comment)
	}
	if strings.Count(panel, "Check retries now") != 1 || strings.Count(panel, "Copy retry commands") != 1 ||
		strings.Contains(panel, `id="`+prRetryAnchor(undecided)+`"`) {
		t.Fatal("expected a single action and copy field on the included PR only")
	}
	response := browser.post(url.Values{
		"kind": {"check-pr-retries"}, "tab": {"acs-prs"}, "repo": {included.Repo}, "sha": {"head"},
		"pr": {strconv.Itoa(included.PR)}, "generation": {strconv.FormatUint(snapshot.Generation, 10)},
	})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab=acs-prs#"+prRetryAnchor(included) ||
		len(application.actions) != 1 || application.actions[0].Kind != "check-pr-retries" || application.actions[0].Target != included {
		t.Fatalf("unexpected retry action response %d and actions %v", response.Code, application.actions)
	}
}
