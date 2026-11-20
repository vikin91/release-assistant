package web

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestCreationFailuresShowTypedCommitEvidenceWithoutGrantingRetries(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	target := release.Target{Repo: source.Repo, PR: 41}
	readAt := time.Date(2026, time.November, 18, 9, 15, 0, 0, time.UTC)
	startedAt := readAt.Add(-time.Minute)
	failure := release.StartFailure{
		CheckID: 731, Job: "fact-build", Cluster: "konflux.example", Category: "admission_policy",
		ExternalID: "fact-build-<check>", CheckStartedAt: startedAt,
		Reason: `creating pipelinerun fact-build- in namespace test-tenant has failed:
admission webhook denied <script>alert("x")</script>`,
		Evidence: release.Evidence{Known: true, At: readAt, URL: "https://github.com/acme/fact/runs/731"},
	}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
		Repositories: []release.Repository{{Source: source, PRs: []release.PR{{Target: target, State: "open"}}}},
		Plans:        []release.Plan{{Target: target, SHA: "fetched-sha", StartFailures: []release.StartFailure{failure}, Evidence: release.Evidence{Error: "another check read failed"}}},
	}}
	body := newBrowser(t, application).getBody(t)
	panel := activityPanel(t, body, "component-prs")
	section := between(t, panel, `<section class="panel" id="component-prs-start-failures-panel">`, `</section>`)
	for _, want := range []string{
		"Reported build creation failures", "konflux.example", "1 reported check", "fact-build", "Stopped before build could run.", "PR or tag unknown",
		`<pre><code>creating pipelinerun fact-build- in namespace test-tenant has failed:
admission webhook denied &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;</code></pre>`,
		"Check #731 on GitHub", `href="https://github.com/acme/fact/runs/731"`,
		`href="https://github.com/acme/fact/commit/fetched-sha"`, "<code>fetched-sha</code></a>",
		"<dt>Last update:</dt>", formatTime(readAt), "<dt>Failure:</dt>", "<dt>Monitored PR</dt>", "<code>fact#41</code>",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("creation evidence missing %q: %s", want, section)
		}
	}
	for _, unwanted := range []string{"Technical details", "GitHub check started", "GitHub check external ID", "raw GitHub check JSON", "Admission policy rejection", "<dt>Repository</dt>", "<script>alert", "Retained evidence", "Recovered", "Cluster is healthy", "Cluster is down", `name="kind"`, "/konflux-retest"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("creation evidence invented authority or status %q", unwanted)
		}
	}
	if len(application.actions) != 0 || len(application.snapshot.Plans[0].Proposals) != 0 {
		t.Fatal("showing diagnostic evidence changed retry authority")
	}
}

func TestACSStartFailuresGroupedByMonitoredPR(t *testing.T) {
	config := validACS5Config()
	first := release.Target{Repo: config.ACS().Repo, PR: 9}
	second := release.Target{Repo: first.Repo, PR: 101}
	shared := release.StartFailure{Job: "retag-fact", CheckID: 15, Cluster: "shared-cluster", Reason: "shared failure"}
	old := shared
	old.Superseded = true
	other := release.StartFailure{Job: "retag-collector", CheckID: 16, Cluster: "second-cluster", Reason: "collector failure"}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config:  config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{first: release.Included, second: release.Included}},
		Repositories: []release.Repository{{
			Source: config.ACS(),
			PRs:    []release.PR{{Target: first, Title: "Update component versions"}},
		}},
		Plans: []release.Plan{
			{Target: second, SHA: "second-sha", StartFailures: []release.StartFailure{old}},
			{Target: first, SHA: "first-sha", StartFailures: []release.StartFailure{shared, shared, other}},
		},
	}}
	panel := activityPanel(t, newBrowser(t, application).getBody(t), "acs-prs")
	section := between(t, panel, `id="acs-prs-start-failures-panel"`, "</section>")
	firstGroup := between(t, section, `data-live-key="creation-report:acs-prs-9"`, `data-live-key="creation-report:acs-prs-101"`)
	_, secondGroup, _ := strings.Cut(section, `data-live-key="creation-report:acs-prs-101"`)
	firstHeading := between(t, firstGroup, "<header", "</header>")
	secondHeading := between(t, secondGroup, "<header", "</header>")
	if !strings.Contains(firstHeading, "<h3>Update component versions</h3>") ||
		!strings.Contains(firstHeading, `href="https://github.com/`+first.Repo+`/pull/9"`) ||
		!strings.Contains(firstHeading, ">"+first.String()+"</a>") {
		t.Errorf("unexpected PR title and link: %s", firstHeading)
	}
	if !strings.Contains(secondHeading, "<h3>"+second.String()+"</h3>") || !strings.Contains(secondHeading, ">Open PR</a>") {
		t.Errorf("unexpected heading for a PR without a title: %s", secondHeading)
	}
	if strings.Count(firstGroup, "shared failure") != 1 ||
		!strings.Contains(firstGroup, "collector failure") || !strings.Contains(firstGroup, "second-cluster") ||
		strings.Contains(firstGroup, "Earlier creation attempts") || strings.Contains(firstGroup, "second-sha") {
		t.Fatalf("wrong current failures for %s: %s", first, firstGroup)
	}
	if !strings.Contains(secondGroup, "Earlier creation attempts") ||
		!strings.Contains(secondGroup, "<pre><code>shared failure</code></pre>") || strings.Contains(secondGroup, "first-sha") {
		t.Fatalf("wrong earlier failure: %s", secondGroup)
	}
}

func TestCreationFailuresDeduplicateChecksWithinTheActivity(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	first := release.Target{Repo: source.Repo, PR: 42}
	second := release.Target{Repo: source.Repo, PR: 43}
	observed := release.Evidence{Known: true, At: time.Now()}
	failure := release.StartFailure{CheckID: 732, Job: "shared-job", Cluster: "konflux.example", Category: "other", Reason: "creation transport failure", Evidence: observed}
	verified := release.Run{ID: "731", CheckID: 731, Job: failure.Job, Status: "completed", Outcome: "success", URL: "https://example.com/verified-run"}
	unverified := release.Run{CheckID: 730, Job: failure.Job, Status: "unknown"}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{first: release.Included, second: release.Included}},
		Plans: []release.Plan{
			{Target: first, SHA: "shared-commit", StartFailures: []release.StartFailure{failure}, Runs: []release.Run{verified}},
			{Target: second, SHA: "shared-commit", StartFailures: []release.StartFailure{failure}, Runs: []release.Run{unverified}},
		},
	}}
	panel := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
	section := between(t, panel, `<section class="panel" id="component-prs-start-failures-panel">`, `</section>`)
	if strings.Count(section, "creation transport failure") != 1 || !strings.Contains(section, "1 reported check") {
		t.Fatal("the same historical check was counted more than once across PRs", section)
	}
	if !strings.Contains(section, "fact#42") || !strings.Contains(section, "fact#43") {
		t.Fatal("deduplication discarded fetched monitoring contexts")
	}
	firstContext := between(t, section, "<code>fact#42</code>", "</dd>")
	if !strings.Contains(firstContext, "Last verified run") || !strings.Contains(firstContext, `href="https://example.com/verified-run"`) ||
		!strings.Contains(firstContext, ">#731</a> · completed · success") || strings.Count(section, "Last verified run") != 1 {
		t.Fatal("the last verified run was not tied to its monitored PR", section)
	}
	before := application.snapshot.Plans[0]
	if before.StartFailures[0] != failure {
		t.Fatal("view grouping mutated the source observation")
	}
	groups, _ := groupStartFailures([]planModel{{Plan: before}}, "component-prs")
	key := groups[0].Failures[0].Key
	before.StartFailures = []release.StartFailure{failure}
	before.StartFailures[0].Evidence.At = observed.At.Add(time.Hour)
	refreshed, _ := groupStartFailures([]planModel{{Plan: before}}, "component-prs")
	if refreshed[0].Failures[0].Key != key {
		t.Fatal("rereading the same check changed its fold identity")
	}
}

func TestCreationFailureScopeFollowsIncludedPRsSelectedTagsAndPinning(t *testing.T) {
	config := validACS5Config()
	config.PinningPR = 52
	source := config.Components[0]
	included := release.Target{Repo: source.Repo, PR: 44}
	excluded := release.Target{Repo: source.Repo, PR: 45}
	undecided := release.Target{Repo: source.Repo, PR: 46}
	ignored := release.Target{Repo: source.Repo, PR: 47}
	closed := release.Target{Repo: source.Repo, PR: 48}
	selectedTag := release.Target{Repo: source.Repo, Tag: "3.25.8"}
	oldTag := release.Target{Repo: source.Repo, Tag: "3.25.7"}
	pin := config.PinningTarget()
	plan := func(target release.Target, reason string) release.Plan {
		return release.Plan{Target: target, SHA: "commit", StartFailures: []release.StartFailure{{CheckID: 733, Job: "job", Reason: reason}}}
	}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{
			included: release.Included, excluded: release.Excluded, ignored: release.Ignored, closed: release.Included,
		}, Tags: map[string]string{source.Name: selectedTag.Tag}},
		Repositories: []release.Repository{
			{Source: source, PRs: []release.PR{{Target: closed, State: "closed"}}},
			{Source: config.ACS(), PinningPR: &release.PR{Target: pin, SHA: "commit", State: "open", Base: config.Branch}},
		},
		Plans: []release.Plan{
			plan(included, "included-component-report"), plan(excluded, "excluded-report"), plan(undecided, "undecided-report"),
			plan(ignored, "ignored-report"), plan(closed, "closed-report"), plan(selectedTag, "selected-tag-report"),
			plan(oldTag, "old-tag-report"), plan(pin, "pinning-creation-report"),
		},
	}}
	body := newBrowser(t, application).getBody(t)
	for tab, wanted := range map[string]string{"component-prs": "included-component-report", "component-releases": "selected-tag-report", "acs-prs": "pinning-creation-report"} {
		panel := activityPanel(t, body, tab)
		if !strings.Contains(panel, wanted) {
			t.Errorf("%s omitted its scoped failure", tab)
		}
		for other, marker := range map[string]string{"component-prs": "included-component-report", "component-releases": "selected-tag-report", "acs-prs": "pinning-creation-report"} {
			if other != tab && strings.Contains(panel, marker) {
				t.Errorf("%s included %s evidence", tab, other)
			}
		}
	}
	tagPanel := activityPanel(t, body, "component-releases")
	if !strings.Contains(tagPanel, "<dt>Monitored tag</dt>") || !strings.Contains(tagPanel, "<code>fact@3.25.8</code>") {
		t.Error("tag failure should identify the monitored tag")
	}
	for _, hidden := range []string{"excluded-report", "undecided-report", "ignored-report", "closed-report", "old-tag-report"} {
		if strings.Contains(body, hidden) {
			t.Errorf("out-of-scope evidence %q is visible", hidden)
		}
	}
	if !strings.Contains(pinningSection(t, body), `href="/?tab=acs-prs#acs-prs-start-failures"`) {
		t.Fatal("pinning PR has no path to its creation-attempt evidence")
	}
	rcPanel := activityPanel(t, body, "acs-rcs")
	if !strings.Contains(rcPanel, "Waiting for RC build creation checks.") || strings.Contains(rcPanel, "pinning-creation-report") {
		t.Fatal("RC creation failures should wait for RC-specific evidence")
	}
}

func TestRetainedCreationFailuresKeepUncertaintyAndSafeSources(t *testing.T) {
	config := validACS5Config()
	target := release.Target{Repo: config.Components[0].Repo, PR: 49}
	readAt := time.Date(2026, time.November, 17, 12, 0, 0, 0, time.UTC)
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
		Plans: []release.Plan{{Target: target, SHA: "previous-commit", StartFailures: []release.StartFailure{{
			CheckID: 734, Category: "other", Reason: "unable to create PipelineRun",
			Evidence: release.Evidence{At: readAt, URL: "javascript:alert(1)", Error: "refresh failed <tag>"},
		}}}},
	}}
	panel := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
	for _, want := range []string{"Cluster not reported", "Job not reported", "Retained evidence · current read unavailable", "<dt>Last update:</dt>", formatTime(readAt), "previous-commit", "refresh failed &lt;tag&gt;"} {
		if !strings.Contains(panel, want) {
			t.Errorf("retained diagnostic missing %q", want)
		}
	}
	if strings.Contains(panel, `href="javascript:`) || strings.Contains(panel, "Cluster is healthy") || strings.Contains(panel, "Recovered") ||
		strings.Contains(panel, "Namespace in error") {
		t.Fatal("retained diagnostic supplied an unsafe source or an invented recovery")
	}
}

func TestOnlyExplicitSupersessionMovesCreationFailuresToEarlierAttempts(t *testing.T) {
	config := validACS5Config()
	target := release.Target{Repo: config.Components[0].Repo, PR: 50}
	failure := release.StartFailure{CheckID: 735, Job: "build", Cluster: "konflux.example", Category: "admission_policy", Reason: "admission-policy-marker"}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
		Plans: []release.Plan{{Target: target, SHA: "commit", StartFailures: []release.StartFailure{failure}, Runs: []release.Run{{Job: "build", Status: "completed", Outcome: "success"}}}},
	}}
	model := tabPage(makePage(application.snapshot, "csrf", ""), "component-prs")
	if len(model.StartFailureReports[0].Current) != 1 || len(model.StartFailureReports[0].Earlier) != 0 {
		t.Fatal("a successful run without typed supersession hid the failure")
	}
	application.snapshot.Plans[0].StartFailures[0].Superseded = true
	model = tabPage(makePage(application.snapshot, "csrf", ""), "component-prs")
	if len(model.StartFailureReports[0].Current) != 0 || len(model.StartFailureReports[0].Earlier) != 1 {
		t.Fatal("verified supersession did not separate earlier-attempt evidence")
	}
	body := newBrowser(t, application).getBody(t)
	if !strings.Contains(body, "Earlier creation attempts") || !strings.Contains(body, "Newer verified run observed") ||
		!strings.Contains(body, "admission-policy-marker") || !strings.Contains(body, "Current cluster availability is unknown") {
		t.Fatal("superseded facts were lost or presented as cluster recovery")
	}
	if !reflect.DeepEqual(application.snapshot.Plans[0].Runs, []release.Run{{Job: "build", Status: "completed", Outcome: "success"}}) {
		t.Fatal("diagnostic grouping changed retry evidence")
	}
}

func TestNoCreationFailureEvidenceDoesNotClaimClusterHealth(t *testing.T) {
	for name, plan := range map[string]release.Plan{
		"missing failure data": {},
		"successful old run":   {Runs: []release.Run{{Job: "build", Outcome: "success"}}},
		"failed current read":  {Evidence: release.Evidence{Error: "network unavailable"}},
	} {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			target := release.Target{Repo: config.Components[0].Repo, PR: 51}
			plan.Target = target
			application := &fakeApplication{snapshot: app.Snapshot{Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}}, Plans: []release.Plan{plan}}}
			panel := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
			if strings.Contains(panel, "start-failures-panel") || strings.Contains(panel, "Cluster is healthy") || strings.Contains(panel, "Recovered") {
				t.Fatal("absence of diagnostic facts was presented as a health observation")
			}
		})
	}
}

func TestCreationFailureGroupingKeepsUnresolvedTargetEvidence(t *testing.T) {
	for name, states := range map[string][2]bool{
		"first target has a newer verified run":  {true, false},
		"second target has a newer verified run": {false, true},
		"both targets have newer verified runs":  {true, true},
	} {
		t.Run(name, func(t *testing.T) {
			plans := make([]planModel, 0, 2)
			for i, superseded := range states {
				plans = append(plans, planModel{Plan: release.Plan{
					Target: release.Target{Repo: "acme/fact", PR: i + 1}, SHA: "shared-commit",
					StartFailures: []release.StartFailure{{CheckID: 736, Job: "build", Cluster: "konflux.example", Superseded: superseded}},
				}})
			}
			reported, earlier := groupStartFailures(plans, "component-prs")
			if states[0] && states[1] {
				if len(reported) != 0 || len(earlier) != 1 {
					t.Fatal("all-target supersession did not move the check to earlier evidence")
				}
			} else if len(reported) != 1 || len(earlier) != 0 || len(reported[0].Failures[0].Targets) != 2 {
				t.Fatal("one target's verified newer run hid another target's unresolved report")
			}
		})
	}
}

func TestCreationFailureGroupingPrefersFreshReadBeforeObservationTime(t *testing.T) {
	observed := time.Date(2026, time.November, 18, 9, 15, 0, 0, time.UTC)
	fresh := release.StartFailure{CheckID: 737, Reason: "positive check read", Evidence: release.Evidence{At: observed, Known: true}}
	stale := release.StartFailure{CheckID: 737, Reason: "retained after failed read", Evidence: release.Evidence{At: observed.Add(time.Hour), Error: "refresh failed"}}
	for _, observations := range [][]release.StartFailure{{fresh, stale}, {stale, fresh}} {
		plans := []planModel{}
		for i, failure := range observations {
			plans = append(plans, planModel{Plan: release.Plan{Target: release.Target{Repo: "acme/fact", PR: i + 1}, SHA: "commit", StartFailures: []release.StartFailure{failure}}})
		}
		reported, _ := groupStartFailures(plans, "component-prs")
		if len(reported) != 1 || len(reported[0].Failures) != 1 || reported[0].Failures[0].Failure.Reason != fresh.Reason || !reported[0].Failures[0].Failure.Evidence.Fresh() {
			t.Fatal("a newer failed read displaced positive check evidence")
		}
	}
}

func TestCreationFailureGroupingDoesNotMergeDifferentSourceChecks(t *testing.T) {
	base := release.StartFailure{CheckID: 738, Cluster: "cluster-a", Job: "build", Reason: "first"}
	otherCluster := base
	otherCluster.Cluster = "cluster-b"
	unknownOne := release.StartFailure{Job: "one", Reason: "no check ID"}
	unknownTwo := release.StartFailure{Job: "two", Reason: "no check ID"}
	plans := []planModel{
		{Plan: release.Plan{Target: release.Target{Repo: "acme/fact", PR: 1}, StartFailures: []release.StartFailure{base, otherCluster, unknownOne, unknownTwo}}},
		{Plan: release.Plan{Target: release.Target{Repo: "acme/collector", PR: 1}, StartFailures: []release.StartFailure{base}}},
	}
	reported, _ := groupStartFailures(plans, "component-prs")
	count := 0
	keys := map[string]bool{}
	for _, cluster := range reported {
		for _, failure := range cluster.Failures {
			count++
			if keys[failure.Key] {
				t.Fatal("distinct checks shared a UI evidence identity")
			}
			keys[failure.Key] = true
		}
	}
	if count != 5 {
		t.Fatal("source checks from different clusters, repositories, or unknown identities were merged")
	}
}
