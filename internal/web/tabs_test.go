package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestActivityTabsWorkWithoutComponentTags(t *testing.T) {
	config := validACS5Config()
	component := release.Target{Repo: config.Components[0].Repo, PR: 17}
	acs := release.Target{Repo: config.ACS().Repo, PR: 18}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Generation: 7,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{component: release.Included, acs: release.Included}},
		Repositories: []release.Repository{
			{Source: config.Components[0], PRs: []release.PR{{
				Target: component, Title: "Component change", State: "open", Base: config.Components[0].Branch, SHA: "head",
			}}},
			{Source: config.ACS(), PRs: []release.PR{{Target: acs, Title: "ACS change", State: "open"}}},
		},
		Plans:     []release.Plan{{Target: component, SHA: "head", Message: "component-build-observation"}, {Target: acs, Message: "acs-build-observation"}},
		Authority: []dispatch.View{{Target: component, SHA: "head", Message: "component-write-offer"}, {Target: acs, Message: "acs-write-offer"}},
	}}
	browser := newBrowser(t, application)
	for _, tab := range []string{"component-prs", "component-releases", "acs-prs", "acs-rcs", "activity", "settings"} {
		response := httptest.NewRecorder()
		browser.handler.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:8080/?tab="+tab, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || !strings.Contains(body, `data-active-tab="`+tab+`"`) {
			t.Fatalf("tab %s is inaccessible without tags: %d", tab, response.Code)
		}
		for _, other := range []string{"component-prs", "component-releases", "acs-prs", "acs-rcs", "activity", "settings"} {
			if !strings.Contains(body, `href="/?tab=`+other+`"`) {
				t.Fatalf("tab %s is missing a link to %s", tab, other)
			}
			panel := `<section id="panel-` + other + `" class="workflow-panel" role="tabpanel" aria-labelledby="tab-` + other + `" data-workflow-panel="` + other + `" tabindex="0"`
			if other != tab {
				panel += " hidden"
			}
			if !strings.Contains(body, panel+">") {
				t.Fatalf("tab %s has wrong visibility for %s", tab, other)
			}
		}
		seen := map[string]bool{}
		for _, match := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(body, -1) {
			if seen[match[1]] {
				t.Fatalf("duplicate DOM id %s across activity panels", match[1])
			}
			seen[match[1]] = true
		}
		for _, form := range renderedFormRE.FindAllString(body, -1) {
			if formInputValue(t, form, "tab") == "" {
				t.Fatal("action has no presentation destination")
			}
		}
		componentView := activityPanel(t, body, "component-prs")
		acsView := activityPanel(t, body, "acs-prs")
		for _, marker := range []string{"Component change", "component-build-observation", "component-write-offer"} {
			if !strings.Contains(componentView, marker) || strings.Contains(acsView, marker) {
				t.Fatalf("component activity crossed into ACS view: %s", marker)
			}
		}
		for _, marker := range []string{"ACS change", "acs-build-observation", "acs-write-offer"} {
			if !strings.Contains(acsView, marker) || strings.Contains(componentView, marker) {
				t.Fatalf("ACS activity crossed into component view: %s", marker)
			}
		}
		if strings.Contains(body, "finish-selection") || strings.Contains(body, "Confirm component tags and wait for PR inventories") {
			t.Fatal("tab presentation retained a global selection prerequisite")
		}
	}
	if len(application.actions) != 0 {
		t.Fatal("viewing a tab changed the release session")
	}
}

func TestComponentReleasesShowsSavedTagWhileItIsBeingVerified(t *testing.T) {
	browser := newBrowser(t, &fakeApplication{snapshot: app.Snapshot{
		Config:      validACS5Config(),
		PendingTags: map[string]string{"fact": "v0.4.7"},
	}})
	panel := activityPanel(t, browser.getBody(t), "component-releases")
	for _, want := range []string{`href="https://github.com/acme/fact/tree/refs/tags/v0.4.7"`, "<code>v0.4.7</code>", "1 of 2 selected", "Waiting for tag verification", "Clear selection"} {
		if !strings.Contains(panel, want) {
			t.Errorf("component release panel missing %q", want)
		}
	}
}

func TestOpenACSPRsDoNotPromptToCutAnRC(t *testing.T) {
	s := workflowStatusSnapshot()
	repo := &s.Repositories[len(s.Repositories)-1]
	for _, n := range []int{41, 42} {
		target := release.Target{Repo: repo.Source.Repo, PR: n}
		repo.PRs = append(repo.PRs, release.PR{Target: target, State: "open", Base: s.Config.Branch, Evidence: repo.Evidence})
		s.Choices.PRs[target] = release.Included
	}
	assessWorkflow(&s)
	panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-prs")
	if strings.Contains(panel, "Recommended next steps") || strings.Contains(panel, "Merge included PRs and cut new RC") ||
		strings.Contains(panel, "Unverified included PRs") {
		t.Fatal("verified open PRs should be tracked in the PR list")
	}
}

func TestACSPatchNoticeRequiresNoOpenPRs(t *testing.T) {
	s := workflowStatusSnapshot()
	acs := &s.Repositories[len(s.Repositories)-1]
	acs.PreviousPins = acs.Pins
	acs.PreviousPublished = release.Yes
	assessWorkflow(&s)
	application := &fakeApplication{snapshot: s}
	browser := newBrowser(t, application)
	const notice = "A new patch may be unnecessary."
	if panel := activityPanel(t, browser.getBody(t), "acs-prs"); !strings.Contains(panel, notice) {
		t.Fatal("ACS phase lost the previous patch notice")
	}

	acs.PRs = []release.PR{{
		Target: release.Target{Repo: acs.Source.Repo, PR: 41},
		Base:   acs.Source.Branch, State: "open", Evidence: acs.Evidence,
	}}
	assessWorkflow(&s)
	application.snapshot = s
	if panel := activityPanel(t, browser.getBody(t), "acs-prs"); strings.Contains(panel, notice) {
		t.Fatal("the previous patch notice ignored an open ACS PR")
	}
}

func TestTagOptionShowsCoverageBeforeConfirmation(t *testing.T) {
	for name, tt := range map[string]struct {
		coverage release.Truth
		want     string
	}{
		"missing merge":       {coverage: release.No, want: "Missing included merge"},
		"incomplete coverage": {want: "Coverage unverified for"},
	} {
		t.Run(name, func(t *testing.T) {
			s := workflowStatusSnapshot()
			repo := &s.Repositories[0]
			delete(s.Choices.Tags, repo.Source.Name)
			target := release.Target{Repo: repo.Source.Repo, PR: 17}
			repo.PRs = []release.PR{{
				Target: target, Base: repo.Source.Branch,
				Merged: true, MergeSHA: "included-merge", Evidence: repo.Evidence,
			}}
			s.Choices.PRs[target] = release.Included
			repo.Tags[0].Contains = map[string]release.Truth{"included-merge": tt.coverage}
			assessWorkflow(&s)
			body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
			panel := activityPanel(t, body, "component-releases")
			row := between(t, panel, `data-live-key="tag:`+repo.Source.Name+":"+repo.Source.Repo+"@", "</tr>")
			if !strings.Contains(row, tt.want+" <code>included-merge</code>") || !strings.Contains(row, "Confirm "+repo.Source.Name) {
				t.Errorf("expected coverage evidence beside the confirmation action: %s", row)
			}
		})
	}
}

func TestTagPreparationKeepsExistingCandidatesAndReturnsToActivity(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	observed := release.Evidence{Known: true, At: time.Now()}
	application := &fakeApplication{snapshot: app.Snapshot{
		Config: config, Generation: 19,
		Choices: release.Choices{Tags: map[string]string{source.Name: "3.25.1"}},
		Repositories: []release.Repository{
			{Source: source, Head: "next-sha", Evidence: observed, Tags: []release.Tag{{Name: "3.25.1", SHA: "selected-sha", Belongs: release.Yes, Evidence: observed}, {Name: "3.25.2", SHA: "next-sha", Belongs: release.Yes, Evidence: observed}}},
			{Source: config.ACS(), PreviousPins: release.Pins{Versions: map[string]string{source.Name: "3.25.0"}, Evidence: observed}},
		},
	}}
	snapshot := &application.snapshot
	snapshot.Assessment = release.Assess(release.Input{Config: config, Choices: snapshot.Choices, Repositories: snapshot.Repositories})
	browser := newBrowser(t, application)
	body := activityPanel(t, browser.getBody(t), "component-releases")
	if !strings.Contains(body, `<p class="muted">The newest Fact tag 3.25.2 is newer than the previous ACS pin FACT_VERSION 3.25.0.</p>`) {
		t.Fatal("component release view lost baseline or existing candidate tags")
	}
	var form string
	for _, candidate := range renderedFormRE.FindAllString(body, -1) {
		if strings.Contains(candidate, "Clear selection") {
			form = candidate
			break
		}
	}
	if form == "" {
		t.Fatal("selected tag cannot be cleared while preparing a newer release")
	}
	values := url.Values{}
	for _, field := range []string{"kind", "generation", "component", "tab"} {
		values.Set(field, formInputValue(t, form, field))
	}
	response := browser.post(values)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab=component-releases" {
		t.Fatalf("tag preparation lost activity: %d %s", response.Code, response.Header().Get("Location"))
	}
	if len(application.actions) != 1 || application.actions[0].Kind != "clear-tag" || application.actions[0].Component != source.Name {
		t.Fatalf("tag preparation action = %+v", application.actions)
	}
}

func TestBaselineGuidanceInActivityTabs(t *testing.T) {
	tests := map[string]struct {
		tag     string
		pending bool
		want    string
	}{
		"should show PR preparation when all component inventories have no tags": {
			want: "No component tag newer than FACT_VERSION 3.25.0 was observed on acme/fact/release-3.25.",
		},
		"should omit PR preparation when a component has a tag": {
			tag:  "3.25.1",
			want: "The newest Fact tag 3.25.1 is newer than the previous ACS pin FACT_VERSION 3.25.0.",
		},
		"should omit PR preparation while a component inventory is incomplete": {
			pending: true,
			want:    "Fact comparison against FACT_VERSION 3.25.0 needs a complete, current tag inventory.",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			observed := release.Evidence{Known: true, At: time.Now()}
			repos := []release.Repository{
				{Source: config.ACS(), PreviousPins: release.Pins{Versions: map[string]string{"fact": "3.25.0", "collector": "3.25.0"}, Evidence: observed}},
				{Source: config.Components[0], Head: "fact-sha", TagsPending: tt.pending, Evidence: observed},
				{Source: config.Components[1], Head: "collector-sha", Evidence: observed},
			}
			if tt.tag != "" {
				repos[1].Tags = []release.Tag{{Name: tt.tag, SHA: "fact-sha", Belongs: release.Yes, Evidence: observed}}
			}
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: config, Repositories: repos, Assessment: release.Assess(release.Input{Config: config, Repositories: repos}),
			}}
			body := newBrowser(t, application).getBody(t)
			releases := activityPanel(t, body, "component-releases")
			if !strings.Contains(releases, `<p class="muted">`+tt.want+`</p>`) {
				t.Fatalf("missing component comparison %q", tt.want)
			}
			prs := activityPanel(t, body, "component-prs")
			const notice = "Component releases have not been cut for this ACS version yet, so PR preparation is the current step."
			if tt.tag == "" && !tt.pending {
				if strings.Count(prs, notice) != 1 || !strings.Contains(prs, `<p class="muted">`+notice+`</p>`) {
					t.Fatal("expected one muted PR preparation notice")
				}
			} else if strings.Contains(prs, notice) {
				t.Fatal("PR preparation notice appeared with a tag or incomplete evidence")
			}
			if strings.Contains(prs, "FACT_VERSION") || strings.Contains(activityPanel(t, body, "acs-prs"), notice) {
				t.Fatal("baseline guidance appeared outside its component activity")
			}
		})
	}
}

func TestComponentReleaseTagActionsAndEvidence(t *testing.T) {
	tests := map[string]struct {
		collectorTag      string
		collectorAncestor release.Truth
		wantGuidance      string
	}{
		"should show verified coverage beside a selected tag": {
			collectorTag: "3.25.5", collectorAncestor: release.Yes, wantGuidance: "Covers included changes",
		},
		"should prompt for the missing selection": {
			collectorAncestor: release.Yes, wantGuidance: "Confirm collector 3.25.5",
		},
		"should explain why a selected tag is unsuitable": {
			collectorTag: "3.25.5", collectorAncestor: release.No,
			wantGuidance: "tag is not an ancestor of the configured component branch",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			config := validACS5Config()
			observed := release.Evidence{Known: true, At: time.Now()}
			choices := release.Choices{Tags: map[string]string{"fact": "3.25.7", "collector": tt.collectorTag}}
			repos := []release.Repository{
				{Source: config.ACS(), PreviousPins: release.Pins{Versions: map[string]string{"fact": "3.25.2", "collector": "3.25.2"}, Evidence: observed}},
				{Source: config.Components[0], Head: "fact-later", Evidence: observed, Tags: []release.Tag{
					{Name: "3.25.7", SHA: "fact-selected", Belongs: release.Yes, Evidence: observed},
					{Name: "3.25.8", SHA: "fact-later", Belongs: release.Yes, Evidence: observed},
				}},
				{Source: config.Components[1], Head: "collector-head", Evidence: observed, Tags: []release.Tag{
					{Name: "3.25.5", SHA: "collector-tag", Belongs: tt.collectorAncestor, Evidence: observed},
				}},
			}
			inventory := release.Inventory{ReleaseVersion: config.Version, Versions: choices.Tags, Evidence: observed}
			for _, source := range config.Components {
				for _, family := range []string{"ci", "konflux"} {
					inventory.Images = append(inventory.Images, release.Image{
						Component: source.Name, Family: family, Reference: "quay.io/acme/" + source.Name + "-" + family,
						Present: release.Yes, Evidence: observed,
					})
				}
			}
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: config, Choices: choices, Repositories: repos,
				Assessment: release.Assess(release.Input{Config: config, Choices: choices, Repositories: repos, Inventory: inventory}),
			}}
			body := activityPanel(t, newBrowser(t, application).getBody(t), "component-releases")
			if !strings.Contains(body, tt.wantGuidance) || strings.Contains(body, "Recommended next steps") {
				t.Errorf("tag section is missing %q or renders a separate recommendations card", tt.wantGuidance)
			}
			for _, want := range []string{"Newer tag available <code>3.25.8</code>", "previous ACS pin FACT_VERSION 3.25.2", "Fact release guide"} {
				if !strings.Contains(body, want) {
					t.Errorf("component tag view missing %q", want)
				}
			}
		})
	}
}

func TestActivityDestinationIsValidated(t *testing.T) {
	browser := newBrowser(t, &fakeApplication{})
	response := browser.post(url.Values{"kind": {"interactive"}, "generation": {"0"}, "tab": {"https://untrusted.example"}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab=component-prs" {
		t.Fatal("presentation destination accepted an external redirect")
	}
}

func activityPanel(t *testing.T, body, tab string) string {
	t.Helper()
	start := strings.Index(body, `<section id="panel-`+tab+`"`)
	if start < 0 {
		t.Fatalf("activity panel %s missing", tab)
	}
	rest := body[start:]
	end := strings.Index(rest[len("<section"):], `<section id="panel-`)
	if end < 0 {
		end = strings.Index(rest, `<section class="panel" id="activity-console"`)
		if end < 0 {
			return rest
		}
		return rest[:end]
	}
	return rest[:end+len("<section")]
}

func TestRejectedActionKeepsItsActivityAndEscapesError(t *testing.T) {
	application := &fakeApplication{snapshot: app.Snapshot{Config: validACS5Config()}, err: errors.New("Cannot apply <script>external text</script>")}
	browser := newBrowser(t, application)
	response := browser.post(url.Values{"kind": {"interactive"}, "generation": {"0"}, "tab": {"acs-prs"}})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `data-active-tab="acs-prs"`) {
		t.Fatal("rejected action lost its activity")
	}
	if strings.Contains(response.Body.String(), "<script>external text</script>") || !strings.Contains(response.Body.String(), "&lt;script&gt;external text&lt;/script&gt;") {
		t.Fatal("action error was not escaped")
	}
}

func TestIncludedClosedPRCanBeExcludedFromItsActivity(t *testing.T) {
	config := validACS5Config()
	for _, source := range []release.Source{config.Components[0], config.ACS()} {
		t.Run(source.Name, func(t *testing.T) {
			target := release.Target{Repo: source.Repo, PR: 36}
			observed := release.Evidence{Known: true, At: time.Now()}
			application := &fakeApplication{snapshot: app.Snapshot{
				Config: config, Generation: 23, WritesEnabled: true,
				Choices: release.Choices{PRs: map[release.Target]release.Choice{target: release.Included}},
				Repositories: []release.Repository{{Source: source, Evidence: observed, PRs: []release.PR{{
					Target: target, SHA: "closed-head", Title: "Closed selected fix", State: "closed", Base: source.Branch, Evidence: observed,
				}}}},
				Plans:     []release.Plan{{Target: target, Message: "closed-pr-retry-plan"}},
				Authority: []dispatch.View{{Target: target, Granted: true, Message: "closed-pr-offer", Offers: []dispatch.Offer{{CanApprove: true}}}},
			}}
			browser := newBrowser(t, application)
			tab := "component-prs"
			if source.Name == "acs" {
				tab = "acs-prs"
			}
			body := activityPanel(t, browser.getBody(t), tab)
			card := prCardContaining(body, "Closed selected fix")
			if card == "" || !strings.Contains(card, "Closed without merging") || !strings.Contains(card, `name="choice" value="excluded"`) {
				t.Fatal("included closed PR has no visible recovery control")
			}
			if strings.Contains(body, "closed-pr-retry-plan") || strings.Contains(body, "closed-pr-offer") {
				t.Fatal("closed PR retained retry or write offers")
			}
			var choiceForm string
			for _, form := range renderedFormRE.FindAllString(card, -1) {
				if formInputValue(t, form, "kind") == "grant" && !strings.Contains(form, `disabled> `+retryCommentPermissionLabel) {
					t.Fatal("closed PR retained an enabled RW control")
				}
				if formInputValue(t, form, "kind") == "choice" {
					choiceForm = form
				}
			}
			values := url.Values{"choice": {"excluded"}}
			for _, field := range []string{"kind", "generation", "repo", "pr", "sha", "tab"} {
				values.Set(field, formInputValue(t, choiceForm, field))
			}
			response := browser.post(values)
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab="+tab {
				t.Fatal("excluding closed PR lost its activity")
			}
			if len(application.actions) != 1 || application.actions[0].Choice != release.Excluded || application.actions[0].Target != target {
				t.Fatalf("closed PR exclusion action = %+v", application.actions)
			}
			application.snapshot.Choices.PRs[target] = release.Excluded
			if strings.Contains(browser.getBody(t), "Closed selected fix") {
				t.Fatal("excluded closed PR remained in the actionable inventory")
			}
			assessment := release.Assess(release.Input{Config: config, Choices: application.snapshot.Choices, Repositories: application.snapshot.Repositories})
			for _, condition := range assessment.Conditions {
				if condition.Name == "included PR "+target.String() {
					t.Fatal("excluded closed PR still blocks release assessment")
				}
			}
		})
	}
}

func TestPRGrantRequiresCurrentOpenIdentity(t *testing.T) {
	config := validACS5Config()
	source := config.Components[0]
	for name, mutate := range map[string]func(*release.PR){
		"stale identity": func(pr *release.PR) { pr.Evidence.Known = false },
		"wrong base":     func(pr *release.PR) { pr.Base = "master" },
		"missing SHA":    func(pr *release.PR) { pr.SHA = "" },
		"opt out":        func(pr *release.PR) { pr.OptOut = true },
	} {
		t.Run(name, func(t *testing.T) {
			pr := release.PR{Target: release.Target{Repo: source.Repo, PR: 37}, SHA: "head", Title: "Unsafe grant", State: "open", Base: source.Branch, Evidence: release.Evidence{Known: true, At: time.Now()}}
			mutate(&pr)
			application := &fakeApplication{snapshot: app.Snapshot{Config: config, WritesEnabled: true, Repositories: []release.Repository{{Source: source, PRs: []release.PR{pr}}}}}
			card := prCardContaining(newBrowser(t, application).getBody(t), pr.Title)
			if card == "" || !strings.Contains(card, `disabled> `+retryCommentPermissionLabel) {
				t.Fatal("unsafe PR identity has an enabled RW control")
			}
		})
	}
}
