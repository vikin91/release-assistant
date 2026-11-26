package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func TestImagePanelsUseTheSameRefreshAction(t *testing.T) {
	for tab, anchor := range map[string]string{
		"acs-rcs": "release-images", "component-releases": "component-images-panel",
	} {
		t.Run(tab, func(t *testing.T) {
			snapshot := workflowStatusSnapshot()
			application := &fakeApplication{snapshot: snapshot}
			browser := newBrowser(t, application)
			panel := activityPanel(t, browser.getBody(t), tab)
			if !strings.Contains(panel, `name="kind" value="refresh-images"`) ||
				!strings.Contains(panel, `name="tab" value="`+tab+`"`) {
				t.Fatal("image panel is missing the shared refresh action")
			}
			response := browser.post(url.Values{
				"kind": {"refresh-images"}, "tab": {tab}, "generation": {strconv.FormatUint(snapshot.Generation, 10)},
			})
			if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/?tab="+tab+"#"+anchor ||
				len(application.actions) != 1 || application.actions[0].Kind != "refresh-images" {
				t.Fatalf("unexpected image refresh response %d and actions %v", response.Code, application.actions)
			}
		})
	}
}

func TestRCTabShowsBuildActionsBesideMissingImages(t *testing.T) {
	s := workflowStatusSnapshot()
	target := s.Candidate().Target
	proposal := release.Proposal{
		Target: target, SHA: "rc-commit", Job: "main-on-push", RunID: "17",
		Payload: "/retest main-on-push tag:" + target.Tag,
	}
	s.Plans = []release.Plan{{
		Target: target, SHA: proposal.SHA, Evidence: s.Inventory.Evidence,
		Runs: []release.Run{
			{Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "failure",
				Eligible: true, URL: "https://konflux.example.com/main-17"},
			{Job: "roxctl-on-push", ID: "18", Status: "in_progress", URL: "https://konflux.example.com/roxctl-18"},
			{Job: "scanner-v4-on-push", ID: "19", Status: "completed", Outcome: "success",
				URL: "https://konflux.example.com/scanner-19"},
		},
		Proposals: []release.Proposal{proposal},
	}}
	for i := range s.Inventory.ReleaseImages {
		if s.Inventory.ReleaseImages[i].Family == "konflux" {
			s.Inventory.ReleaseImages[i].Present = release.No
		}
	}
	assessWorkflow(&s)
	assertTabStatus(t, s, "acs-rcs", tabActionNeeded, "can be retested")

	for name, writes := range map[string]bool{"read-only": false, "writes allowed": true} {
		t.Run(name, func(t *testing.T) {
			s := s
			s.WritesEnabled, s.Interactive = writes, true
			if writes {
				s.Authority = []dispatch.View{{
					Target: target, SHA: proposal.SHA, Granted: true,
					Offers: []dispatch.Offer{{ID: dispatch.ID(proposal), Proposal: proposal, CanApprove: true, Queueable: true}},
				}}
			}
			body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
			panel := activityPanel(t, body, "acs-rcs")
			for _, want := range []string{"3 Konflux images missing", "Open RC.2 tag", "View tag checks"} {
				if !strings.Contains(panel, want) {
					t.Errorf("missing %q", want)
				}
			}
			main := between(t, panel, "<code>main</code></th>", "</tr>")
			for _, want := range []string{"Build failed", "Open in Konflux", proposal.Payload, "Copy comment"} {
				if !strings.Contains(main, want) {
					t.Errorf("missing %q in main image row", want)
				}
			}
			if writes {
				if !strings.Contains(main, "Open Retest Queue") || strings.Contains(main, "<form") {
					t.Error("authorized row should link to the queue for confirmation")
				}
			} else if !strings.Contains(main, "View retest comment") || strings.Contains(main, "Open Retest Queue") {
				t.Error("read-only row should offer a copyable command")
			}
			roxctl := between(t, panel, "<code>roxctl</code></th>", "</tr>")
			scanner := between(t, panel, "<code>scanner-v4</code></th>", "</tr>")
			if !strings.Contains(roxctl, "Build running") || !strings.Contains(scanner, "Check build output") {
				t.Error("running and successful build evidence is missing")
			}
			if strings.Contains(roxctl+scanner, "View retest comment") {
				t.Error("running or successful build unexpectedly offers a retry")
			}
			if strings.Contains(activityPanel(t, body, "acs-prs"), proposal.Payload) {
				t.Error("RC retry crossed into the ACS PRs tab")
			}
		})
	}
}

func TestRCTabShowsLastResultsWhileRecheckingBranch(t *testing.T) {
	s := workflowStatusSnapshot()
	rc := &s.Repositories[len(s.Repositories)-1].Candidate
	rc.Evidence.Known = false
	proposal := release.Proposal{
		Target: rc.Target, SHA: rc.SHA, Job: "main-on-push", RunID: "17",
		Payload: "/retest main-on-push tag:" + rc.Target.Tag,
	}
	s.Plans = []release.Plan{{
		Target: rc.Target, SHA: rc.SHA, Evidence: s.Inventory.Evidence,
		Runs: []release.Run{{
			Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "failure", Eligible: true,
		}},
		Proposals: []release.Proposal{proposal},
	}}
	assessWorkflow(&s)
	panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
	main := between(t, panel, "<code>main</code></th>", "</tr>")
	for _, want := range []string{"Build failed", "Available", "<time"} {
		if !strings.Contains(main, want) {
			t.Errorf("missing %q from retained image row", want)
		}
	}
	if !strings.Contains(panel, "Rechecking RC.2 against the updated release branch.") ||
		strings.Contains(panel, "No commits have landed") || strings.Contains(panel, proposal.Payload) {
		t.Fatal("the RC page did not distinguish retained results from the unverified comparison")
	}
}

func TestRCTabSeparatesMissingTagsFromAdditionalCheckTimeouts(t *testing.T) {
	s := workflowStatusSnapshot()
	target := s.Candidate().Target
	const message = "PipelineRun identity verification failed: extra-retag-job: context deadline exceeded"
	const digest = "quay.io/rhacs-eng/release-main@sha256:example"
	plan := release.Plan{
		Target: target, SHA: "rc-commit", Message: "No build retests needed.", Action: "None",
		Evidence: release.Evidence{At: s.Inventory.Evidence.At, Error: message},
		Runs:     []release.Run{{Job: "extra-retag-job", Status: "unknown", URL: "https://konflux.example.com/extra-retag-job"}},
	}
	for i := range s.Inventory.ReleaseImages {
		image := &s.Inventory.ReleaseImages[i]
		if image.Family != "konflux" {
			image.Reference = "quay.io/rhacs-eng/" + image.Name + ":" + target.Tag
			continue
		}
		image.Present = release.No
		image.Reference = "quay.io/rhacs-eng/release-" + image.Name + ":" + target.Tag
		image.Evidence.URL = "https://quay.io/repository/rhacs-eng/release-" + image.Name + "?tag=" + target.Tag
		plan.Runs = append(plan.Runs, release.Run{
			Job: image.Name + "-on-push", ID: image.Name, Status: "completed", Outcome: "success",
			URL:   "https://konflux.example.com/" + image.Name,
			Image: release.Image{Reference: digest, Evidence: s.Inventory.Evidence},
		})
	}
	latest := s.Inventory.Evidence.At.Add(time.Minute).Truncate(time.Second)
	s.Inventory.ReleaseImages = append(s.Inventory.ReleaseImages, release.Image{
		Component: "acs", Family: "ci", Name: "main",
		Reference: "quay.io/stackrox-io/main:" + target.Tag,
		Evidence:  release.Evidence{At: latest, Known: true}, Present: release.No,
	})
	s.Plans = []release.Plan{plan}
	assessWorkflow(&s)

	render := func() string {
		return activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
	}
	panel := render()
	table := between(t, panel, `<table class="rc-image-table"`, "</table>")
	main := between(t, table, "<code>main</code></th>", "</tr>")
	headers := between(t, table, "<thead>", "</thead>")
	for _, want := range []string{"Image", "CI rhacs-eng", "CI stackrox-io", "Konflux build", "Konflux image", "Last checked"} {
		_, rest, found := strings.Cut(headers, ">"+want+"</th>")
		if !found {
			t.Fatalf("missing or misordered column %q", want)
		}
		headers = rest
	}
	cells := strings.Split(main, "<td>")
	if len(cells) != 6 ||
		!strings.Contains(cells[1], "quay.io/rhacs-eng/main:"+target.Tag) || !strings.Contains(cells[1], "✓") ||
		!strings.Contains(cells[2], "quay.io/stackrox-io/main:"+target.Tag) || !strings.Contains(cells[2], "✗") ||
		!strings.Contains(cells[3], "Build succeeded") || !strings.Contains(cells[3], "Check build output") ||
		!strings.Contains(cells[4], "quay.io/rhacs-eng/release-main:"+target.Tag) || !strings.Contains(cells[4], "Open expected Quay tag") ||
		!strings.Contains(cells[5], latest.UTC().Format(time.RFC3339)) {
		t.Errorf("unexpected main image columns: %s", main)
	}
	if !strings.Contains(panel, "3 of 4 CI images available.") ||
		!strings.Contains(panel, "The corresponding Konflux builds succeeded, but their expected tags are missing in Quay.") ||
		strings.Contains(panel, plan.Message) {
		t.Error("missing focused image publication summary")
	}
	if strings.Count(table, "<code>main</code></th>") != 1 || strings.Contains(table, "<details") ||
		strings.Contains(panel, `<details id="release-images"`) || strings.Contains(panel, digest) {
		t.Error("the image table should show one compact row per image, without folds or reported output digests")
	}
	diagnostics := between(t, panel, `<details class="rc-diagnostics" data-fold-key="rc-build-diagnostics">`, "</details>")
	for _, want := range []string{"1 additional job could not be verified", "A Konflux lookup timed out", "Open check in Konflux", message} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("missing %q in collapsed diagnostics", want)
		}
	}
	if strings.Contains(table, "extra-retag-job") || strings.Count(panel, message) != 1 {
		t.Error("auxiliary check diagnostics leaked into the image rows")
	}

	s.Plans[0].Runs[1].ID, s.Plans[0].Runs[1].Status, s.Plans[0].Runs[1].Outcome = "", "unknown", ""
	panel = render()
	main = between(t, panel, "<code>main</code></th>", "</tr>")
	if !strings.Contains(main, "Build unverified") || !strings.Contains(main, "View unverified check") ||
		strings.Contains(panel, "The corresponding Konflux builds succeeded") {
		t.Error("an unverified image build must be identified in its row")
	}

	s.Inventory.ReleaseImages = append(s.Inventory.ReleaseImages, release.Image{
		Component: "acs", Family: "konflux", Name: "fact", BuildJob: "extra-retag-job",
		Reference: "quay.io/rhacs-eng/release-fact:" + target.Tag,
		Evidence:  s.Inventory.Evidence, Present: release.No,
	})
	assessWorkflow(&s)
	panel = render()
	fact := between(t, panel, "<code>fact</code></th>", "</tr>")
	if !strings.Contains(fact, "Build unverified") || !strings.Contains(fact, "https://konflux.example.com/extra-retag-job") ||
		strings.Contains(panel, "additional job could not be verified") {
		t.Error("the retag check should appear beside its configured RC image")
	}
}

func TestRCTabLinksReleaseWorkflowWhenBranchIsReadyForANewCandidate(t *testing.T) {
	tests := map[string]struct {
		change   func(*app.Snapshot, *release.Repository)
		wantLink bool
	}{
		"first RC": {
			change: func(s *app.Snapshot, acs *release.Repository) {
				acs.Candidate = release.Candidate{Evidence: s.Inventory.Evidence}
			},
			wantLink: true,
		},
		"branch advanced after the existing RC": {
			change:   func(_ *app.Snapshot, acs *release.Repository) { acs.Candidate.Ahead = 2 },
			wantLink: true,
		},
		"existing RC at branch tip": {},
		"candidate evidence is unavailable": {
			change: func(_ *app.Snapshot, acs *release.Repository) {
				acs.Candidate.Ahead = 2
				acs.Candidate.Evidence.Error = "compare unavailable"
			},
		},
		"an ACS PR is undecided": {
			change: func(s *app.Snapshot, acs *release.Repository) {
				acs.Candidate.Ahead = 2
				acs.PRs = []release.PR{{
					Target: release.Target{Repo: acs.Source.Repo, PR: 41},
					Base:   s.Config.Branch, State: "open", Evidence: acs.Evidence,
				}}
			},
		},
		"the release already has a final tag": {
			change: func(s *app.Snapshot, acs *release.Repository) {
				acs.Candidate.Ahead = 2
				acs.Tags = []release.Tag{{Name: "v" + s.Config.Version}}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := workflowStatusSnapshot()
			acs := &s.Repositories[len(s.Repositories)-1]
			if tt.change != nil {
				tt.change(&s, acs)
			}
			assessWorkflow(&s)
			body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
			panel := activityPanel(t, body, "acs-rcs")
			for _, marker := range []string{"The release branch is ready for a new RC.", "Open release workflow", "/actions/workflows/start-release.yml"} {
				if present := strings.Contains(panel, marker); present != tt.wantLink {
					t.Errorf("RC handoff marker %q present = %t, want %t", marker, present, tt.wantLink)
				}
			}
			if acs.Candidate.Target.Tag == "" && !strings.Contains(panel, "No release candidate tag exists") {
				t.Error("RC panel lost the verified absence of a candidate")
			}
		})
	}
}

func TestRCTabShowsPostCandidateCommitsAndPRsWithoutPinning(t *testing.T) {
	for name, tc := range map[string]struct {
		ahead   int
		prError string
		want    []string
	}{
		"branch and candidate match": {want: []string{"No commits have landed on the release branch since RC.2."}},
		"two commits landed": {ahead: 2, want: []string{
			">2 commits</a> landed on the release branch since RC.2.",
			`href="https://github.com/stackrox/stackrox/compare/rc-commit...acs-head"`,
			`href="https://github.com/stackrox/stackrox/pull/17"`, "#17 Backport fix",
		}},
		"PR results are incomplete": {ahead: 1, prError: "GitHub rate limit", want: []string{
			">1 commit</a> landed on the release branch since RC.2.",
			"Some PR links are unavailable.",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			s := workflowStatusSnapshot()
			s.Config.PinningPR = 0
			rc := &s.Repositories[len(s.Repositories)-1].Candidate
			rc.Ahead, rc.PRError = tc.ahead, tc.prError
			if tc.ahead == 2 {
				rc.NextMilestone = release.Evidence{Known: true, At: rc.Evidence.At, URL: "https://github.com/stackrox/stackrox/milestone/575"}
			}
			rc.CompareURL = "https://github.com/stackrox/stackrox/compare/rc-commit...acs-head"
			if tc.ahead == 2 {
				rc.PRs = []release.CandidatePR{{Number: 17, Title: "Backport fix", URL: "https://github.com/stackrox/stackrox/pull/17"}}
			}
			panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: s}).getBody(t), "acs-rcs")
			const milestone = `href="https://github.com/stackrox/stackrox/milestone/575"`
			if tc.ahead == 2 {
				link := strings.Index(panel, milestone)
				if link <= strings.Index(panel, "#17 Backport fix") ||
					link >= strings.Index(panel, `class="rc-image-table"`) ||
					!strings.Contains(panel, "Close it manually on GitHub to cut RC.3.") {
					t.Fatal("missing or misplaced Cut RC link and help text")
				}
			} else if strings.Contains(panel, "Cut RC.") {
				t.Fatal("unexpected Cut RC link without a verified milestone")
			}
			for _, want := range append(tc.want, "Open RC.2 tag") {
				if !strings.Contains(panel, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}

func TestRCFieldsRemainVisibleAcrossObservations(t *testing.T) {
	observed := workflowStatusSnapshot()
	rc := observed.Candidate()
	observed.Plans = []release.Plan{{Target: rc.Target, SHA: rc.SHA, Evidence: rc.Evidence}}
	observed.ManualRetries = []release.ManualRetryReport{{Target: rc.Target, SHA: rc.SHA}}
	assessWorkflow(&observed)
	pending := app.Snapshot{Config: observed.Config}
	unavailable := pending
	unavailable.Repositories = []release.Repository{{Source: pending.Config.ACS(), Candidate: release.Candidate{
		Evidence: release.Evidence{Error: "GitHub unavailable"},
	}}}
	for name, tc := range map[string]struct {
		snapshot app.Snapshot
		want     []string
	}{
		"before first poll": {pending, []string{"Waiting to discover release candidate tags", "Waiting for GitHub to verify the current RC tag", "Pending registry checks"}},
		"discovery failed":  {unavailable, []string{"GitHub unavailable", "Unavailable · The current RC could not be verified"}},
		"no manual retries": {observed, []string{"No manual CI retries available for this RC"}},
	} {
		t.Run(name, func(t *testing.T) {
			panel := activityPanel(t, newBrowser(t, &fakeApplication{snapshot: tc.snapshot}).getBody(t), "acs-rcs")
			rest := panel
			for _, field := range []string{`id="release-images-panel"`, `class="rc-image-table"`, `id="rc-validation"`, `class="rc-validation-checks"`, `aria-label="Manual CI retries"`} {
				var ok bool
				_, rest, ok = strings.Cut(rest, field)
				if !ok {
					t.Fatalf("missing or misplaced %s", field)
				}
			}
			if !strings.Contains(panel, "<code>operator-index</code></th>") {
				t.Error("expected image disappeared")
			}
			for _, message := range tc.want {
				if !strings.Contains(panel, message) {
					t.Errorf("missing %q", message)
				}
			}
			if !tc.snapshot.Candidate().Target.Valid() {
				row := between(t, panel, "<code>main</code></th>", "</tr>")
				if strings.Count(row, ">Pending</span>") != 5 || strings.Contains(row, "href=") {
					t.Errorf("unobserved image row: %s", row)
				}
				if strings.Contains(panel, "actions/jobs/") || strings.Contains(panel, `value="send"`) {
					t.Fatal("pending fields offer an action")
				}
			}
		})
	}
}
