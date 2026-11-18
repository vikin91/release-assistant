package web

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestConfiguredPinningPRMonitoringAndGrant(t *testing.T) {
	target := release.Target{Repo: "stackrox/stackrox", PR: 481}
	observed := release.Evidence{Known: true, At: time.Now()}
	unrelated := release.Target{Repo: "acme/fact", PR: 44}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: func() release.Config { c := validACS5Config(); c.PinningPR = target.PR; return c }(), Generation: 73, WritesEnabled: true,
		Repositories: []release.Repository{
			{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-3.25"}, PRs: []release.PR{{Target: unrelated, SHA: "unrelated-sha", State: "open", Title: "Unselected PR"}}},
			{Source: release.Source{Name: "acs", Repo: target.Repo, Branch: "release-5.0"},
				PRs:       []release.PR{{Target: target, SHA: "pin-sha", State: "open", Base: "release-5.0", Title: "Pin release components", URL: "https://github.com/stackrox/stackrox/pull/481", Evidence: observed, ChecksEvidence: release.Evidence{Error: "one check source failed"}, Checks: []release.Check{{Name: "unit", Status: "completed", Conclusion: "success", URL: "https://github.com/stackrox/stackrox/actions/runs/10"}}}},
				PinningPR: &release.PR{Target: target, SHA: "pin-sha", State: "open", Base: "release-5.0", Title: "Pin release components", URL: "https://github.com/stackrox/stackrox/pull/481", Evidence: observed, ChecksEvidence: release.Evidence{Error: "one check source failed"}, Checks: []release.Check{{Name: "unit", Status: "completed", Conclusion: "success", URL: "https://github.com/stackrox/stackrox/actions/runs/10"}}},
			},
		},
		Plans: []release.Plan{
			{Target: target, SHA: "stale-plan-sha", Message: "pin build result", Runs: []release.Run{{Job: "fact-build", URL: "https://github.com/stackrox/stackrox/actions/runs/11", Image: release.Image{Component: "acs", Family: "konflux", Name: "rox", Reference: "quay.io/stackrox/rox@sha256:abc123", Present: release.Yes, Evidence: observed}}}},
			{Target: unrelated, SHA: "unrelated-sha", Message: "must remain out of scope"},
		},
		Authority: []dispatch.View{{Target: target, SHA: "pin-sha", Granted: true}},
	}}
	b := newBrowser(t, application)
	body := b.getBody(t)
	section := pinningSection(t, body)
	for _, want := range []string{
		"Pin release components", "Unable to get current checks from pinning PR #481", "one check source failed",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("pinning PR section omitted %q", want)
		}
	}
	if strings.Contains(section, ">Included PR<") || strings.Contains(section, ">Not found<") || strings.Contains(section, ">Available<") {
		t.Error("stale-head image evidence was presented as current, or pinning PR was presented as Included")
	}
	if strings.Contains(section, "before RC.0") || strings.Contains(section, "ACS RC.0 build") {
		t.Error("pinning image rows contain phase labels")
	}
	if strings.Contains(section, "quay.io/stackrox/rox@sha256:abc123") || strings.Contains(section, ">unit</a>") || strings.Contains(section, "<table>") {
		t.Error("unverified checks or images clutter the pinning summary")
	}
	if strings.Contains(body, "must remain out of scope") {
		t.Error("unrelated undecided PR proposal appeared in retry proposals")
	}
	if strings.Count(body, "Pin release components") != 1 {
		t.Error("configured PR was duplicated in normal PR selection")
	}
	retries := between(t, body, `id="acs-prs-retry"`, "</section>")
	if !strings.Contains(retries, ">Pinning PR</span>") || !strings.Contains(retries, target.String()) {
		t.Error("retry proposal does not distinguish the configured pinning PR scope")
	}
	if !strings.Contains(body, `href="/?tab=acs-prs#acs-prs-retry-details" data-open-section`) {
		t.Error("open pinning PR has no retry proposal link")
	}

	var grantForm string
	for _, form := range renderedFormRE.FindAllString(body, -1) {
		if formInputValue(t, form, "kind") == "grant" && formInputValue(t, form, "repo") == target.Repo {
			grantForm = form
			break
		}
	}
	if grantForm == "" {
		t.Fatal("configured pinning PR has no RW permission form")
	}
	assertGrantFields(t, grantForm, b, target, "pin-sha", 73, true)
	formValues := url.Values{}
	for _, field := range []string{"kind", "generation", "repo", "pr", "sha", "enabled"} {
		formValues.Set(field, formInputValue(t, grantForm, field))
	}
	formValues.Add("enabled", "true")
	if response := b.post(formValues); response.Code != 303 {
		t.Fatalf("pinning grant form rejected: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(application.actions) != 1 || application.actions[0].Kind != "grant" || application.actions[0].Target != target || application.actions[0].SHA != "pin-sha" || application.actions[0].Generation != 73 || !application.actions[0].Enabled {
		t.Fatalf("pinning grant action=%+v", application.actions)
	}
}

func TestConfiguredPinningPRImagesShowPerRowStatusAndMissingObservations(t *testing.T) {
	target := release.Target{Repo: "stackrox/stackrox", PR: 482}
	observed := release.Evidence{Known: true, At: time.Now(), URL: "https://quay.io/repository/stackrox/rox?tag=latest"}
	pr := release.PR{Target: target, SHA: "pin-sha", State: "open", Base: "release-5.0", Evidence: observed, ChecksEvidence: observed, Checks: []release.Check{{Name: "healthy-build", Status: "completed", Conclusion: "success", Required: release.Yes}}}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config:       func() release.Config { c := validACS5Config(); c.PinningPR = target.PR; return c }(),
		Repositories: []release.Repository{{Source: release.Source{Name: "acs", Repo: target.Repo}, PinningPR: &pr}},
		Plans: []release.Plan{{Target: target, SHA: "pin-sha", Runs: []release.Run{
			{Job: "available-build", URL: "https://github.com/stackrox/stackrox/actions/runs/20", Image: release.Image{Name: "rox", Reference: "quay.io/stackrox/rox:1.2.3", Present: release.Yes, Evidence: observed}},
			{Job: "missing-build", Image: release.Image{Name: "scanner", Reference: "quay.io/stackrox/scanner:1.2.3", Present: release.No, Evidence: observed}},
			{Job: "unknown-build", Image: release.Image{Name: "collector", Reference: "quay.io/stackrox/collector:1.2.3", Evidence: release.Evidence{Error: "registry timeout"}}},
			{Job: "digest-only-build", ID: "22", Image: release.Image{Evidence: release.Evidence{Error: "The build has not reported a valid image reference."}}},
		}}},
	}}
	body := newBrowser(t, application).getBody(t)
	section := pinningSection(t, body)
	for _, want := range []string{"Available", "Not found", "Unable to check", "rox", "scanner", "collector", "quay.io/stackrox/rox:1.2.3", "quay.io/stackrox/scanner:1.2.3", "quay.io/stackrox/collector:1.2.3", "registry timeout", "Build evidence", "Image evidence", "digest-only-build", "The build has not reported a valid image reference."} {
		if !strings.Contains(section, want) {
			t.Errorf("pinning image observations omitted %q", want)
		}
	}
	for _, forbidden := range []string{"before RC.0", "ACS RC.0 build"} {
		if strings.Contains(section, forbidden) {
			t.Errorf("pinning image row includes phase text %q", forbidden)
		}
	}

	application.snapshot.Plans[0].SHA = "old-head"
	stale := pinningSection(t, newBrowser(t, application).getBody(t))
	if strings.Contains(stale, ">Available<") || !strings.Contains(stale, "Build belongs to commit old-head; current PR commit is pin-sha.") {
		t.Error("images from another head were presented as current")
	}
	application.snapshot.Plans = nil
	body = newBrowser(t, application).getBody(t)
	section = pinningSection(t, body)
	if !strings.Contains(section, "Required checks passed.") || !strings.Contains(section, "Pending build images.") {
		t.Error("healthy PR did not advance to image readiness")
	}
}

func TestConfiguredPinningPRGrantIsDisabledWhenIdentityOrWritesAreUnsafe(t *testing.T) {
	base := release.PR{Target: release.Target{Repo: "stackrox/stackrox", PR: 483}, SHA: "pin-sha", State: "open", Base: "release-5.0", Evidence: release.Evidence{Known: true, At: time.Now()}}
	for _, tc := range []struct {
		name     string
		pr       release.PR
		writes   bool
		wantText string
	}{
		{name: "startup read only", pr: base, writes: false},
		{name: "opt out", pr: func() release.PR { p := base; p.OptOut = true; return p }(), writes: true},
		{name: "wrong base", pr: func() release.PR { p := base; p.Base = "master"; return p }(), writes: true},
		{name: "unknown identity", pr: func() release.PR { p := base; p.Evidence.Known = false; return p }(), writes: true},
		{name: "merged", pr: func() release.PR { p := base; p.State = "closed"; p.Merged = true; return p }(), writes: true, wantText: "Merged."},
		{name: "closed", pr: func() release.PR { p := base; p.State = "closed"; return p }(), writes: true, wantText: "Closed without merging."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := tc.pr.Target
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: func() release.Config { c := validACS5Config(); c.PinningPR = target.PR; return c }(), WritesEnabled: tc.writes,
				Repositories: []release.Repository{{Source: release.Source{Name: "acs", Repo: target.Repo}, PinningPR: &tc.pr}},
				Authority:    []dispatch.View{{Target: target, SHA: tc.pr.SHA, Granted: true}},
			}}
			body := newBrowser(t, application).getBody(t)
			section := pinningSection(t, body)
			if tc.wantText != "" && !strings.Contains(section, tc.wantText) {
				t.Errorf("pinning section omitted status %q", tc.wantText)
			}
			grantFound := false
			for _, form := range renderedFormRE.FindAllString(section, -1) {
				if formInputValue(t, form, "kind") == "grant" && !strings.Contains(form, "disabled") {
					grantFound = true
					t.Error("unsafe pinning PR RW checkbox is enabled")
				} else if formInputValue(t, form, "kind") == "grant" {
					grantFound = true
				}
			}
			if !grantFound {
				t.Error("configured pinning PR has no RW permission checkbox")
			}
			if tc.pr.State != "open" && strings.Contains(section, "href=\"/?tab=acs-prs#acs-prs-retry-details\"") {
				t.Error("non-open pinning PR has a retry invitation")
			}
		})
	}
}

func assertGrantFields(t *testing.T, form string, b browser, target release.Target, sha string, generation uint64, enabled bool) {
	t.Helper()
	for field, want := range map[string]string{
		"kind": "grant", "generation": strconv.FormatUint(generation, 10), "repo": target.Repo, "pr": strconv.Itoa(target.PR), "sha": sha,
		"enabled": "false", "csrf": b.csrf,
	} {
		if got := formInputValue(t, form, field); got != want {
			t.Errorf("grant form %s=%q, want %q", field, got, want)
		}
	}
	if enabled && !strings.Contains(form, `name="enabled" value="true" checked`) {
		t.Error("pinning PR RW permission did not retain its granted state")
	}
}

func pinningSection(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<details class="panel" id="pinning-details" open>`)
	if start < 0 {
		t.Fatal("pinning details section missing")
	}
	depth := 0
	for offset := start; offset < len(body); {
		nextOpen := strings.Index(body[offset:], "<details")
		nextClose := strings.Index(body[offset:], "</details>")
		if nextClose < 0 {
			t.Fatal("pinning details section is not closed")
		}
		if nextOpen >= 0 && nextOpen < nextClose {
			depth++
			offset += nextOpen + len("<details")
			continue
		}
		depth--
		offset += nextClose + len("</details>")
		if depth == 0 {
			return body[start:offset]
		}
	}
	t.Fatal("pinning details section is not balanced")
	return ""
}

func TestPinningPRShowsNextActionInsteadOfCheckInventory(t *testing.T) {
	for name, tc := range map[string]struct {
		check    release.Check
		proposal bool
		partial  bool
		want     string
	}{
		"should show the job to retest on failure":              {check: release.Check{Name: "failing-build", Status: "completed", Conclusion: "failure", Required: release.Yes}, proposal: true, want: "failing-build: view retest comment"},
		"should keep a verified retry after a partial read":     {check: release.Check{Name: "failing-build", Status: "completed", Conclusion: "failure", Required: release.Yes}, proposal: true, partial: true, want: "failing-build: view retest comment"},
		"should link to failure when no verified retest exists": {check: release.Check{Name: "failing-build", Status: "failure", Required: release.Yes}, want: "No verified retest is available"},
		"should wait for running checks":                        {check: release.Check{Name: "running-build", Status: "in_progress", Required: release.Yes}, want: "Wait for their results before retesting"},
	} {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			config.PinningPR = 484
			target := config.PinningTarget()
			observed := release.Evidence{Known: true, At: time.Now()}
			pr := release.PR{Target: target, SHA: "head", State: "open", Base: config.Branch, Evidence: observed, ChecksEvidence: observed, Checks: []release.Check{
				{Name: "passing-unit", Status: "success", Required: release.Yes},
				{Name: "optional-failure", Status: "failure", Required: release.No}, tc.check,
			}}
			plan := release.Plan{Target: target, SHA: pr.SHA, Evidence: observed, Runs: []release.Run{{Job: "passing-run", Status: "success"}}}
			if tc.partial {
				plan.Evidence = release.Evidence{At: observed.At, Error: "An unrelated build lookup failed."}
			}
			if tc.proposal {
				plan.Proposals = []release.Proposal{{Target: target, SHA: pr.SHA, Job: "failing-build", RunID: "17", Payload: "/konflux-retest failing-build"}}
			}
			body := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{Config: config, Repositories: []release.Repository{{Source: config.ACS(), PinningPR: &pr}}, Plans: []release.Plan{plan}}}).getBody(t)
			section := pinningSection(t, body)
			if !strings.Contains(section, tc.want) {
				t.Fatalf("missing next action %q: %s", tc.want, section)
			}
			if tc.partial && !strings.Contains(section, plan.Evidence.Error) {
				t.Fatal("pinning summary omitted the partial result warning")
			}
			for _, unwanted := range []string{"passing-unit", "optional-failure", "Pending build images", "<table>"} {
				if strings.Contains(section, unwanted) {
					t.Errorf("unhelpful detail %q shown", unwanted)
				}
			}
			retries := between(t, body, `<section class="panel" id="acs-prs-retry">`, `</section>`)
			if strings.Contains(retries, "passing-run") {
				t.Error("pinning runs duplicated in retries")
			}
			if tc.proposal && !strings.Contains(retries, "/konflux-retest failing-build") {
				t.Error("exact retest comment missing")
			}
		})
	}
}
