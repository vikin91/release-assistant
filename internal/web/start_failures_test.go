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
		Reason:   `creating pipelinerun fact-build- in namespace test-tenant has failed: admission webhook denied <script>alert("x")</script>`,
		Evidence: release.Evidence{Known: true, At: readAt, URL: "https://github.com/acme/fact/commit/fetched-sha/checks?check_run_id=731"},
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
		"Reported build creation failures", "konflux.example", "1 reported check", "fact-build", "Admission policy rejection",
		"denied &lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;", "Open source check #731 on GitHub", `href="https://github.com/acme/fact/commit/fetched-sha/checks?check_run_id=731"`,
		"Source commit: <code>fetched-sha</code>", "<dt>Check read at</dt>", formatTime(readAt),
		"<dt>GitHub check started</dt>", formatTime(startedAt),
		"GitHub check external ID: <code>fact-build-&lt;check&gt;</code>",
		`href="https://api.github.com/repos/acme/fact/check-runs/731"`, "Open raw GitHub check JSON",
		"PR/tag association is unverified", "Current cluster availability is unknown",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("creation evidence missing %q: %s", want, section)
		}
	}
	summary, technical, found := strings.Cut(section, "<summary>Technical details</summary>")
	if !found || strings.Contains(summary, "test-tenant") ||
		!strings.Contains(summary, "<dt>Repository</dt><dd><code>acme/fact</code></dd>") ||
		!strings.Contains(summary, "Open source check #731 on GitHub") || strings.Contains(summary, "admission webhook denied") ||
		!strings.Contains(technical, "admission webhook denied") {
		t.Fatal("the summary should expose failure context and the source link while the raw error stays in technical details")
	}
	for _, unwanted := range []string{"<script>alert", "Retained evidence", "Recovered", "Cluster is healthy", "Cluster is down", `name="kind"`, "/konflux-retest"} {
		if strings.Contains(section, unwanted) {
			t.Errorf("creation evidence invented authority or status %q", unwanted)
		}
	}
	if len(application.actions) != 0 || len(application.snapshot.Plans[0].Proposals) != 0 {
		t.Fatal("showing diagnostic evidence changed retry authority")
	}
}

func TestCreationFailuresDeduplicateChecksWithinTheActivity(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	first := release.Target{Repo: source.Repo, PR: 42}
	second := release.Target{Repo: source.Repo, PR: 43}
	observed := release.Evidence{Known: true, At: time.Now()}
	failure := release.StartFailure{CheckID: 732, Job: "shared-job", Cluster: "konflux.example", Category: "other", Reason: "creation transport failure", Evidence: observed}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Choices: release.Choices{PRs: map[release.Target]release.Choice{first: release.Included, second: release.Included}},
		Plans: []release.Plan{
			{Target: first, SHA: "shared-commit", StartFailures: []release.StartFailure{failure}},
			{Target: second, SHA: "shared-commit", StartFailures: []release.StartFailure{failure}},
		},
	}}
	panel := activityPanel(t, newBrowser(t, application).getBody(t), "component-prs")
	section := between(t, panel, `<section class="panel" id="component-prs-start-failures-panel">`, `</section>`)
	if strings.Count(section, "creation transport failure") != 1 || !strings.Contains(section, "1 reported check") || !strings.Contains(section, "Other creation failure") {
		t.Fatal("the same historical check was counted more than once across PRs", section)
	}
	if !strings.Contains(section, first.String()) || !strings.Contains(section, second.String()) {
		t.Fatal("deduplication discarded fetched monitoring contexts")
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
	for _, hidden := range []string{"excluded-report", "undecided-report", "ignored-report", "closed-report", "old-tag-report"} {
		if strings.Contains(body, hidden) {
			t.Errorf("out-of-scope evidence %q is visible", hidden)
		}
	}
	if !strings.Contains(pinningSection(t, body), `href="/?tab=acs-prs#acs-prs-start-failures"`) {
		t.Fatal("pinning PR has no path to its creation-attempt evidence")
	}
	if strings.Contains(activityPanel(t, body, "acs-rcs"), "start-failures-panel") {
		t.Fatal("RC image-only view gained creation-failure diagnostics")
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
	for _, want := range []string{"Cluster not reported", "Job not reported", "Retained evidence · current read unavailable", "<dt>Last retained check read at</dt>", formatTime(readAt), "previous-commit", "refresh failed &lt;tag&gt;"} {
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
	if len(model.StartFailureClusters) != 1 || len(model.EarlierStartFailureClusters) != 0 {
		t.Fatal("a successful run without typed supersession hid the failure")
	}
	application.snapshot.Plans[0].StartFailures[0].Superseded = true
	model = tabPage(makePage(application.snapshot, "csrf", ""), "component-prs")
	if len(model.StartFailureClusters) != 0 || len(model.EarlierStartFailureClusters) != 1 {
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
