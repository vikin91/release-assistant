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

func TestIncludedPRHasOneCombinedRetryCopyField(t *testing.T) {
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
	want := "/konflux-retest checks\ngh api --method POST repos/" + included.Repo + "/actions/jobs/201/rerun\n/test unit"
	if !strings.Contains(field, "<pre><code>"+want+"</code></pre>") || strings.Contains(field, "stale command") {
		t.Fatalf("combined retry copy field is incorrect: %s", field)
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
