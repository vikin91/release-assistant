package web

import (
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestWorkflowPRStatuses(t *testing.T) {
	tests := map[string]struct {
		choice           release.Choice
		change           func(*release.PR)
		anotherUndecided bool
		want             tabState
		wantText         string
	}{
		"should ask for a decision": {
			want: tabActionNeeded, wantText: "Decide whether to include 1 PR.",
		},
		"should allow an empty release scope after exclusion": {
			choice: release.Excluded, want: tabComplete,
		},
		"should ignore a candidate no longer returned by discovery": {
			change: func(pr *release.PR) { pr.Evidence.Known = false }, want: tabComplete,
		},
		"should wait for CI": {
			choice: release.Included, want: tabInProgress, wantText: "Waiting for required checks",
		},
		"should prioritize decisions over running checks": {
			choice: release.Included, anotherUndecided: true, want: tabActionNeeded,
			wantText: "Decide whether to include 1 PR.",
		},
		"should prioritize a failed required check over an undecided PR": {
			choice:           release.Included,
			change:           func(pr *release.PR) { pr.Checks[0].Conclusion = "failure" },
			anotherUndecided: true,
			want:             tabNeedsAttention, wantText: "PR #1 has failed required checks.",
		},
		"should wait for a requested review": {
			choice: release.Included,
			change: func(pr *release.PR) {
				pr.Checks[0].Conclusion = "success"
				pr.ReviewRequested = true
			},
			want: tabInProgress, wantText: "Waiting for a requested review",
		},
		"should ask the operator to review a green PR for merge": {
			choice: release.Included,
			change: func(pr *release.PR) { pr.Checks[0].Conclusion = "success" },
			want:   tabActionNeeded, wantText: "Review PR #1 for merge.",
		},
		"should report a closed included PR": {
			choice: release.Included,
			change: func(pr *release.PR) { pr.State = "closed" },
			want:   tabNeedsAttention, wantText: "closed without merging",
		},
		"should report an included PR on the wrong branch": {
			choice: release.Included,
			change: func(pr *release.PR) { pr.Base = "other" },
			want:   tabNeedsAttention, wantText: "targets a different branch",
		},
		"should accept a verified merge": {
			choice: release.Included,
			change: func(pr *release.PR) {
				pr.State, pr.Merged, pr.MergeSHA = "merged", true, "merge"
			},
			want: tabComplete,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			for _, tab := range []string{"component-prs", "acs-prs"} {
				t.Run(tab, func(t *testing.T) {
					s := workflowStatusSnapshot()
					repo := &s.Repositories[0]
					if tab == "acs-prs" {
						repo = &s.Repositories[len(s.Config.Components)]
					}
					pr := release.PR{
						Target: release.Target{Repo: repo.Source.Repo, PR: 1}, Base: repo.Source.Branch,
						State: "open", SHA: "pr-head", Evidence: repo.Evidence, ChecksEvidence: repo.Evidence,
						Checks: []release.Check{{Required: release.Yes, Status: "in_progress"}},
					}
					s.Choices.PRs[pr.Target] = tt.choice
					if tt.change != nil {
						tt.change(&pr)
					}
					if tt.anotherUndecided {
						undecided := pr
						undecided.Target.PR = 2
						repo.PRs = append(repo.PRs, undecided)
					}
					repo.PRs = append(repo.PRs, pr)
					assertTabStatus(t, s, tab, tt.want, tt.wantText)
				})
			}
		})
	}
}

func TestWorkflowKeepsVerifiedMergeWhenRepositoryReadIsIncomplete(t *testing.T) {
	s := workflowStatusSnapshot()
	repo := &s.Repositories[1]
	repo.PRListEvidence = repo.Evidence
	repo.Evidence.Error = "one or more GitHub observations are incomplete"
	pr := release.PR{
		Target: release.Target{Repo: repo.Source.Repo, PR: 4339},
		Base:   repo.Source.Branch, State: "merged", Merged: true, MergeSHA: "merge",
		Evidence: repo.PRListEvidence,
	}
	repo.PRs = []release.PR{pr}
	s.Choices.PRs[pr.Target] = release.Included
	assertTabStatus(t, s, "collector-prs", tabComplete, "")

	repo.PRListEvidence.Known = false
	repo.PRListEvidence.Error = "open PR list failed"
	assertTabStatus(t, s, "collector-prs", tabNeedsAttention, "Could not list open PRs for "+repo.Source.Repo+".")
	assessWorkflow(&s)
	body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
	panel := activityPanel(t, body, "collector-prs")
	if !strings.Contains(panel, "Included PRs merged (1)") || strings.Contains(panel, "Unverified included PRs") {
		t.Fatalf("expected the verified merge in the Collector PR list")
	}
}

func TestWorkflowComponentReleaseStatus(t *testing.T) {
	tests := map[string]struct {
		update   func(*app.Snapshot)
		want     tabState
		wantText string
	}{
		"should verify all selected tags and required images": {want: tabComplete},
		"should show progress before the first image inventory": {
			update: func(s *app.Snapshot) { s.Inventory = release.Inventory{} },
			want:   tabInProgress, wantText: "Waiting for image checks",
		},
		"should show progress while replacing unselected image placeholders": {
			update: func(s *app.Snapshot) {
				delete(s.Inventory.Versions, s.Inventory.Images[0].Component)
				s.Inventory.Images[0].Reference = ""
				s.Inventory.Images[0].Evidence.Error = "select a component tag"
				s.Inventory.Evidence.Known = false
			},
			want: tabInProgress, wantText: "Waiting for image checks",
		},
		"should ignore registry failures for the previous selected version": {
			update: func(s *app.Snapshot) {
				s.Inventory.Versions[s.Inventory.Images[0].Component] = "previous"
				s.Inventory.Images[0].Evidence.Error = "registry unavailable"
			},
			want: tabInProgress, wantText: "Waiting for image checks",
		},
		"should warn about a registry error for the current version while loading": {
			update: func(s *app.Snapshot) {
				s.Loading = true
				s.Inventory.Images[0].Evidence.Error = "registry unavailable"
			},
			want: tabNeedsAttention, wantText: "Unable to verify image",
		},
		"should warn when the inventory read failed before returning versions": {
			update: func(s *app.Snapshot) {
				s.Inventory = release.Inventory{Evidence: release.Evidence{Error: "registry unavailable"}}
			},
			want: tabNeedsAttention, wantText: "registry unavailable",
		},
		"should ask for tag selection": {
			update: func(s *app.Snapshot) { delete(s.Choices.Tags, "fact") },
			want:   tabActionNeeded, wantText: "Select a component tag for fact.",
		},
		"should quietly wait for a selected change to merge": {
			update: func(s *app.Snapshot) {
				repo := &s.Repositories[0]
				pr := release.PR{
					Target: release.Target{Repo: repo.Source.Repo, PR: 1},
					Base:   repo.Source.Branch, State: "open", Evidence: repo.Evidence,
				}
				repo.PRs = []release.PR{pr}
				s.Choices.PRs[pr.Target] = release.Included
			},
			want: tabPending,
		},
		"should warn when a tag misses an included merge": {
			update: func(s *app.Snapshot) {
				repo := &s.Repositories[0]
				pr := release.PR{
					Target: release.Target{Repo: repo.Source.Repo, PR: 1}, Base: repo.Source.Branch,
					State: "merged", Merged: true, MergeSHA: "later-merge", Evidence: repo.Evidence,
				}
				repo.PRs = []release.PR{pr}
				repo.Tags[0].Contains = map[string]release.Truth{pr.MergeSHA: release.No}
				s.Choices.PRs[pr.Target] = release.Included
			},
			want: tabNeedsAttention, wantText: "does not contain included PR merge",
		},
		"should keep the selected tag complete when only the commit graph is limited": {
			update: func(s *app.Snapshot) {
				repo := &s.Repositories[0]
				pr := release.PR{
					Target: release.Target{Repo: repo.Source.Repo, PR: 1}, Base: repo.Source.Branch,
					State: "merged", Merged: true, MergeSHA: "unknown-merge", Evidence: repo.Evidence,
				}
				repo.PRs = []release.PR{pr}
				repo.CommitGraphLimit = 20
				s.Choices.PRs[pr.Target] = release.Included
			},
			want: tabComplete,
		},
		"should wait for a running build on a selected tag": {
			update: func(s *app.Snapshot) { addWorkflowBuild(s, "in_progress", "") },
			want:   tabInProgress, wantText: "Waiting for builds",
		},
		"should surface a failed build over a running build": {
			update: func(s *app.Snapshot) {
				addWorkflowBuild(s, "in_progress", "")
				s.Plans[0].Runs = append(s.Plans[0].Runs, release.Run{
					ID: "2", Status: "completed", Outcome: "failure",
				})
			},
			want: tabNeedsAttention, wantText: "A build failed",
		},
		"should ignore a build from a different revision": {
			update: func(s *app.Snapshot) {
				addWorkflowBuild(s, "completed", "failure")
				s.Plans[0].SHA = "different"
			},
			want: tabPending,
		},
		"should allow time for a successful build to publish": {
			update: func(s *app.Snapshot) {
				addWorkflowBuild(s, "completed", "success")
				s.Inventory.Images[0].PublishingSince = s.Inventory.Images[0].Evidence.At
			},
			want: tabInProgress, wantText: "to publish",
		},
		"should report a missing image after the publication window": {
			update: func(s *app.Snapshot) {
				addWorkflowBuild(s, "completed", "success")
				image := &s.Inventory.Images[0]
				image.PublishingSince = image.Evidence.At.Add(-imagePublishingGrace)
			},
			want: tabNeedsAttention, wantText: "Image is still missing",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			s := workflowStatusSnapshot()
			if tt.update != nil {
				tt.update(&s)
			}
			assertTabStatus(t, s, "component-releases", tt.want, tt.wantText)
		})
	}
}

func TestWorkflowPinningAndRCStatuses(t *testing.T) {
	s := workflowStatusSnapshot()
	acs := &s.Repositories[len(s.Config.Components)]
	assertTabStatus(t, s, "acs-prs", tabComplete, "the ACS branch pins match")
	assertTabStatus(t, s, "acs-rcs", tabInProgress, "Waiting for RC build checks")

	for i := range s.Inventory.ReleaseImages {
		s.Inventory.ReleaseImages[i].Present = release.No
	}
	assertTabStatus(t, s, "acs-rcs", tabPending, "")
	s.Inventory.ReleaseImages[0].Present = release.Yes
	assertTabStatus(t, s, "acs-rcs", tabInProgress, "Waiting for the remaining")
	missing := &s.Inventory.ReleaseImages[1]
	missing.PublishingSince = missing.Evidence.At.Add(-imagePublishingGrace)
	assertTabStatus(t, s, "acs-rcs", tabNeedsAttention, "Image is still missing")
	missing.Evidence.Error = "registry unavailable"
	assertTabStatus(t, s, "acs-rcs", tabNeedsAttention, "Unable to verify image")

	acs.PinningPR.Merged, acs.PinningPR.State = false, "open"
	acs.PinningPR.ReviewRequested = true
	acs.Pins.Versions["fact"] = "3.25.0"
	assertTabStatus(t, s, "acs-prs", tabInProgress, "Waiting for a requested review")
	assertTabStatus(t, s, "acs-rcs", tabNeedsAttention, "Unable to verify image")
	acs.PinningPR.Merged = true
	assertTabStatus(t, s, "acs-prs", tabNeedsAttention, "pins still differ")
}

func TestWorkflowTabStatusIsRenderedWithAccessibleReason(t *testing.T) {
	s := workflowStatusSnapshot()
	component := &s.Repositories[0]
	component.PRs = []release.PR{{
		Target: release.Target{Repo: component.Source.Repo, PR: 1},
		Base:   component.Source.Branch, State: "open", Evidence: component.Evidence,
	}}
	assessWorkflow(&s)
	body := newBrowser(t, &fakeApplication{snapshot: s}).getBody(t)
	for _, marker := range []string{
		`aria-label="1. Component PRs · Action needed · Decide whether to include 1 PR."`,
		`class="tab-status action-needed" aria-hidden="true">→`,
		`<strong>Action needed</strong> · Decide whether to include 1 PR.`,
		`aria-label="4. ACS RCs · In progress · Images are available. Waiting for RC build checks."`,
	} {
		if !strings.Contains(body, marker) {
			t.Errorf("missing %q", marker)
		}
	}
}

func assertTabStatus(t *testing.T, s app.Snapshot, tab string, want tabState, reason string) {
	t.Helper()
	assessWorkflow(&s)
	got := makePage(s, "", "").TabStatuses[tab]
	if got.State != want || !strings.Contains(got.Reason, reason) {
		t.Fatalf("%s status = %+v, want state %d with %q", tab, got, want, reason)
	}
}

func assessWorkflow(s *app.Snapshot) {
	s.Assessment = release.Assess(release.Input{
		Config: s.Config, Choices: s.Choices, Repositories: s.Repositories, Inventory: s.Inventory,
	})
}

func addWorkflowBuild(s *app.Snapshot, status, outcome string) {
	repo := s.Repositories[0]
	tag := s.Choices.Tags[repo.Source.Name]
	s.Inventory.Images[0].Present = release.No
	s.Plans = []release.Plan{{
		Target: release.Target{Repo: repo.Source.Repo, Tag: tag}, SHA: repo.Head,
		Evidence: repo.Evidence, Runs: []release.Run{{ID: "1", Status: status, Outcome: outcome}},
	}}
}

func workflowStatusSnapshot() app.Snapshot {
	config := validACS5Config()
	config.PinningPR = 53
	evidence := release.Evidence{At: time.Now(), Known: true}
	s := app.Snapshot{
		Config:  config,
		Choices: release.Choices{PRs: map[release.Target]release.Choice{}, Tags: map[string]string{}},
		Inventory: release.Inventory{
			ReleaseVersion: config.Version, Evidence: evidence, Versions: map[string]string{"acs": config.Version + "-rc.2"},
		},
	}
	pins := release.Pins{Evidence: evidence, Versions: map[string]string{}, Paths: map[string]string{}}
	for _, source := range config.Components {
		version := source.Stream + ".1"
		s.Choices.Tags[source.Name] = version
		s.Inventory.Versions[source.Name] = version
		pins.Versions[source.Name] = version
		s.Repositories = append(s.Repositories, release.Repository{
			Source: source, Evidence: evidence, Head: source.Name + "-head",
			Tags: []release.Tag{{Name: version, SHA: source.Name + "-head", Belongs: release.Yes, Evidence: evidence}},
		})
		for _, family := range []string{"ci", "konflux"} {
			s.Inventory.Images = append(s.Inventory.Images, release.Image{
				Component: source.Name, Family: family, Reference: source.Name + ":" + version,
				Evidence: evidence, Present: release.Yes,
			})
		}
	}
	s.Repositories = append(s.Repositories, release.Repository{
		Source: config.ACS(), Head: "acs-head", Evidence: evidence, Pins: pins,
		Candidate: release.Candidate{
			Target: release.Target{Repo: config.ACS().Repo, Tag: config.Version + "-rc.2"},
			SHA:    "rc-commit", Number: 2, Evidence: evidence,
		},
		PinningEvidence: evidence,
		PinningPins:     release.Pins{Evidence: evidence, Versions: map[string]string{"fact": "3.25.1", "collector": "3.25.1"}},
		PinningPR: &release.PR{
			Target: config.PinningTarget(), Base: config.Branch, SHA: "pin-head", State: "merged", Merged: true,
			MergeSHA: "pin-merge", Evidence: evidence, ChecksEvidence: evidence,
		},
	})
	for _, name := range []string{"main", "roxctl", "scanner-v4"} {
		for _, family := range []string{"ci", "konflux"} {
			s.Inventory.ReleaseImages = append(s.Inventory.ReleaseImages, release.Image{
				Component: "acs", Name: name, Family: family, Reference: name + ":" + config.Version + "-rc.2",
				Evidence: evidence, Present: release.Yes,
			})
		}
	}
	return s
}
