package release

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAssessTagScenarios(t *testing.T) {
	tests := map[string]struct {
		mutate       func(*Input)
		wantStatus   Truth
		wantReason   string
		wantIssue    TagCoverageIssue
		wantProposed string
	}{
		"tag at branch tip is suitable without selected PRs": {
			wantStatus: Yes,
			wantReason: "belongs to the configured branch",
		},
		"unselected later changes do not invalidate tag": {
			mutate: func(in *Input) {
				in.Repositories[1].Head = "component-head-2"
				in.Repositories[1].Tags[0].SHA = "component-head-1"
				in.Repositories[1].Tags[0].Belongs = Yes
			},
			wantStatus: Yes,
			wantReason: "covers every included merged PR",
		},
		"selected merged PR after confirmed tag proposes higher eligible tag without switching": {
			mutate: func(in *Input) {
				pr := mergedPR("fact", 11, "merge-11")
				in.Repositories[1].PRs = append(in.Repositories[1].PRs, pr)
				in.Choices.PRs[pr.Target] = Included
				in.Repositories[1].Tags[0].Contains = map[string]Truth{"merge-11": No}
				newer := freshTag("v0.3.80", "component-head-2", Yes)
				newer.Contains = map[string]Truth{"merge-11": Yes}
				highest := freshTag("v0.3.100", "component-head-3", Yes)
				highest.Contains = map[string]Truth{"merge-11": Yes}
				wrongStream := freshTag("v0.4.99", "component-head-4", Yes)
				wrongStream.Contains = map[string]Truth{"merge-11": Yes}
				in.Repositories[1].Tags = append(in.Repositories[1].Tags, newer, highest, wrongStream)
				in.Repositories[1].Head = "component-head-2"
			},
			wantStatus:   No,
			wantReason:   "does not contain included PR merge",
			wantIssue:    TagMissingMerge,
			wantProposed: "v0.3.100",
		},
		"ACS PRs are selected against the configured ACS branch": {
			mutate: func(in *Input) {
				pr := PR{Target: Target{Repo: "stackrox/stackrox", PR: 31}, Base: "release-5.0", State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PRs = append(in.Repositories[0].PRs, pr)
				in.Choices.PRs[pr.Target] = Included
			},
			wantStatus: No,
			wantReason: "is open",
		},
		"merged ACS PR satisfies its own branch condition": {
			mutate: func(in *Input) {
				pr := PR{Target: Target{Repo: "stackrox/stackrox", PR: 32}, Base: "release-5.0", State: "closed", Merged: true, MergeSHA: "acs-merge-32", Evidence: freshEvidence()}
				in.Repositories[0].PRs = append(in.Repositories[0].PRs, pr)
				in.Choices.PRs[pr.Target] = Included
			},
			wantStatus: Yes,
			wantReason: "merged into its configured ACS release branch",
		},
		"selected open PR requires merge": {
			mutate: func(in *Input) {
				pr := PR{Target: Target{Repo: "stackrox/fact", PR: 12}, Base: "release-0.3", State: "open", Evidence: freshEvidence()}
				in.Repositories[1].PRs = append(in.Repositories[1].PRs, pr)
				in.Choices.PRs[pr.Target] = Included
			},
			wantStatus: No,
			wantReason: "still open",
			wantIssue:  TagPRUnmerged,
		},
		"same stream tag outside branch is ineligible": {
			mutate:     func(in *Input) { in.Repositories[1].Tags[0].Belongs = No },
			wantStatus: No,
			wantReason: "not an ancestor",
		},
		"unknown ancestry remains unknown": {
			mutate:     func(in *Input) { in.Repositories[1].Tags[0].Belongs = "" },
			wantStatus: "",
			wantReason: "ancestry for this tag is unknown",
		},
		"stale branch evidence makes tag unknown": {
			mutate:     func(in *Input) { in.Repositories[1].Evidence = Evidence{Known: true} },
			wantStatus: "",
			wantReason: "not fresh",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			a := Assess(in)
			conditionName := "tag fact"
			for target := range in.Choices.PRs {
				if target.Repo == "stackrox/stackrox" {
					conditionName = "included PR " + target.String()
				}
			}
			condition := conditionByName(t, a, conditionName)
			if condition.Status != tt.wantStatus {
				t.Fatalf("tag status = %q, want %q (%s)", condition.Status, tt.wantStatus, condition.Reason)
			}
			if !strings.Contains(condition.Reason, tt.wantReason) {
				t.Fatalf("tag reason %q does not contain %q", condition.Reason, tt.wantReason)
			}
			if condition.TagIssue != tt.wantIssue {
				t.Errorf("tag coverage issue = %q, want %q", condition.TagIssue, tt.wantIssue)
			}
			if tt.wantProposed != "" && a.ProposedTags["fact"] != tt.wantProposed {
				t.Fatalf("proposed tag = %q, want %q", a.ProposedTags["fact"], tt.wantProposed)
			}
			if tt.wantProposed != "" && in.Choices.Tags["fact"] != "v0.3.79" {
				t.Fatalf("confirmed tag changed to %q", in.Choices.Tags["fact"])
			}
			if strings.Contains(name, "selected merged PR after") && a.PinningRecommendation != "" {
				t.Fatalf("pinning was recommended despite unsuitable confirmed tag: %s", a.PinningRecommendation)
			}
		})
	}
}

func TestAssessPinsImagesAndSelection(t *testing.T) {
	tests := map[string]struct {
		mutate      func(*Input)
		condition   string
		wantStatus  Truth
		wantReason  string
		wantPinning string
	}{
		"covered inclusion preserves matching pins": {
			mutate: func(in *Input) {
				pr := mergedPR("fact", 21, "merge-21")
				in.Repositories[1].PRs = append(in.Repositories[1].PRs, pr)
				in.Choices.PRs[pr.Target] = Included
				in.Repositories[1].Tags[0].Contains = map[string]Truth{"merge-21": Yes}
			},
			condition: "ACS release branch pins", wantStatus: Yes,
			wantReason: "Actual branch pins match selected versions",
		},
		"open correct pinning PR waits for merge": {
			mutate: func(in *Input) {
				acs := &in.Repositories[0]
				acs.PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				acs.PinningPins = Pins{Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			condition: "ACS release branch pins", wantStatus: Yes,
			wantReason: "Actual branch pins match selected versions",
		},
		"open correct pinning PR is distinct from actual pins": {
			mutate: func(in *Input) {
				acs := &in.Repositories[0]
				acs.Pins.Versions["fact"] = "0.3.78"
				acs.PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				acs.PinningPins = Pins{Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			condition: "ACS release branch pins", wantStatus: No,
			wantReason: "fact actual 0.3.78, selected 0.3.79",
		},
		"update guidance names the open PR and changed pin": {
			mutate: func(in *Input) {
				acs := &in.Repositories[0]
				acs.Pins.Versions["fact"] = "0.3.78"
				acs.PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				acs.PinningPins = Pins{Versions: map[string]string{"fact": "0.3.78", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			condition: "ACS release branch pins", wantStatus: No,
			wantReason:  "fact actual 0.3.78, selected 0.3.79",
			wantPinning: "Update ACS pinning PR #99 to set pkg/fact/version to 0.3.79.",
		},
		"merged pinning PR with matching actual pins is already complete": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 100}, Base: in.Config.Branch, State: "closed", Merged: true, Evidence: freshEvidence()}
			},
			condition: "ACS release branch pins", wantStatus: Yes,
			wantReason: "Actual branch pins match selected versions",
		},
		"missing image blocks pin update": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Versions["fact"] = "0.3.78"
				in.Inventory.Images[0].Present = No
			},
			condition: "image fact ci", wantStatus: No,
			wantReason: "missing",
		},
		"inaccessible image is unknown": {
			mutate:    func(in *Input) { in.Inventory.Images[0].Evidence = Evidence{Known: true, Error: "unauthorized"} },
			condition: "image fact ci", wantStatus: "",
			wantReason: "unavailable",
		},
		"empty image observations are unknown": {
			mutate:    func(in *Input) { in.Inventory.Images = nil },
			condition: "image fact ci", wantStatus: "",
			wantReason: "no current registry result",
		},
		"images for another ACS release are unknown": {
			mutate:    func(in *Input) { in.Inventory.ReleaseVersion = "5.0.2" },
			condition: "image fact ci", wantStatus: "",
			wantReason: "configured ACS release version",
		},
		"new PR guidance names the release branch and changed pin": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Paths = map[string]string{"fact": "pkg/fact/version", "collector": "pkg/collector/version"}
				in.Repositories[0].Pins.Versions["fact"] = "0.3.78"
			},
			condition: "ACS release branch pins", wantStatus: No,
			wantReason:  "fact actual 0.3.78, selected 0.3.79",
			wantPinning: "Create an ACS pinning PR against release-5.0 to set pkg/fact/version to 0.3.79.",
		},
		"invalid pin path blocks pinning guidance": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Paths = map[string]string{"fact": "../../bad;echo", "collector": "pkg/collector/version"}
				in.Repositories[0].Pins.Versions["fact"] = "0.3.78"
			},
			condition: "ACS release branch pins", wantStatus: No,
			wantReason: "fact actual 0.3.78, selected 0.3.79",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			a := Assess(in)
			condition := conditionByName(t, a, tt.condition)
			if condition.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q (%s)", condition.Status, tt.wantStatus, condition.Reason)
			}
			if !strings.Contains(condition.Reason, tt.wantReason) {
				t.Fatalf("reason %q does not contain %q", condition.Reason, tt.wantReason)
			}
			if a.PinningRecommendation != tt.wantPinning {
				t.Fatalf("pinning recommendation = %q, want %q", a.PinningRecommendation, tt.wantPinning)
			}
		})
	}
}

func TestAssessUnknownConfigurationAndMissingObservations(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Input)
		condition string
		want      Truth
	}{
		{"invalid configuration", func(in *Input) { in.Config.Components = nil }, "configuration", No},
		{"missing selected PR", func(in *Input) { target := Target{Repo: "stackrox/fact", PR: 500}; in.Choices.PRs[target] = Included }, "included PR stackrox/fact#500", ""},
		{"missing confirmed tag", func(in *Input) { in.Choices.Tags["fact"] = "v0.3.99" }, "tag fact", ""},
		{"missing actual pin is unknown", func(in *Input) { delete(in.Repositories[0].Pins.Versions, "fact") }, "ACS release branch pins", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := readyInput()
			tt.mutate(&in)
			a := Assess(in)
			if got := conditionByName(t, a, tt.condition).Status; got != tt.want {
				t.Fatalf("status = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAssessDoesNotSelectMissingTag(t *testing.T) {
	in := readyInput()
	in.Choices.Tags["fact"] = ""
	a := Assess(in)
	if a.ProposedTags["fact"] != "v0.3.79" {
		t.Fatalf("proposed tag = %q, want v0.3.79", a.ProposedTags["fact"])
	}
	if in.Choices.Tags["fact"] != "" {
		t.Fatalf("assessment selected tag %q without confirmation", in.Choices.Tags["fact"])
	}
}

func TestAssessReadinessDoesNotRequireSelectionConfirmation(t *testing.T) {
	in := readyInput()
	in.Choices.PRs = map[Target]Choice{}
	a := Assess(in)
	if !a.Ready {
		t.Fatalf("fresh complete input should be ready without a selection confirmation: %v", a.Conditions)
	}
}

func TestAssessReportsIndependentReleaseEvidence(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Input)
		check  func(*testing.T, Assessment)
	}{
		"should assess component merges before any tag exists": {
			mutate: func(in *Input) {
				in.Choices.Tags = nil
				in.Repositories[1].Tags = nil
				pr := mergedPR("fact", 40, "")
				pr.State, pr.Merged = "open", false
				in.Repositories[1].PRs = []PR{pr}
				in.Choices.PRs[pr.Target] = Included
			},
			check: func(t *testing.T, a Assessment) {
				pr := conditionByName(t, a, "included PR stackrox/fact#40")
				if pr.Status != No || !strings.Contains(pr.Reason, "open on the component release branch") {
					t.Fatalf("unexpected component PR condition: %+v", pr)
				}
				if a.Ready || a.PinningRecommendation != "" {
					t.Fatal("missing tag choices allowed release readiness or a pinning recommendation")
				}
			},
		},
		"should propose a tag when none is selected": {
			mutate: func(in *Input) { delete(in.Choices.Tags, "fact") },
			check: func(t *testing.T, a Assessment) {
				if a.ProposedTags["fact"] == "" || conditionByName(t, a, "tag fact").Status != "" {
					t.Fatal("expected a proposed tag and no selected tag")
				}
			},
		},
		"should assess missing images independently of another component's tag selection": {
			mutate: func(in *Input) {
				delete(in.Choices.Tags, "collector")
				in.Inventory.Images[0].Present = No
			},
			check: func(t *testing.T, a Assessment) {
				if a.ProposedTags["collector"] == "" || conditionByName(t, a, "tag collector").Status != "" {
					t.Fatal("expected a proposed Collector tag and no selected tag")
				}
				if image := conditionByName(t, a, "image fact ci"); image.Status != No {
					t.Fatalf("expected a missing Fact CI image: %+v", image)
				}
			},
		},
		"should assess ACS merges independently of component releases": {
			mutate: func(in *Input) {
				in.Choices.Tags = nil
				pr := PR{Target: Target{Repo: in.Config.ACS().Repo, PR: 41}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				second := pr
				second.Target.PR++
				in.Repositories[0].PRs = []PR{pr, second}
				in.Choices.PRs[pr.Target], in.Choices.PRs[second.Target] = Included, Included
			},
			check: func(t *testing.T, a Assessment) {
				condition := conditionByName(t, a, "included PR stackrox/stackrox#41")
				if strings.Contains(condition.Reason, "tag") || strings.Contains(condition.Reason, "component") {
					t.Fatalf("ACS merge condition refers to component releases: %s", condition.Reason)
				}
			},
		},
		"should keep release guides available for already selected tags": {
			check: func(t *testing.T, a Assessment) {
				if a.ReleaseGuides["fact"] == "" || a.ReleaseGuides["collector"] == "" {
					t.Fatal("selected tags hid manual release instructions")
				}
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			tt.check(t, Assess(in))
		})
	}
}

func TestAssessComponentBaseline(t *testing.T) {
	tests := map[string]struct {
		pin, tag, head   string
		mutate           func(*Input)
		relation, reason string
		ahead, openPR    bool
		allNoTag         bool
	}{
		"should find no tag with a complete inventory": {
			mutate: func(in *Input) {
				in.Repositories[1].Tags = []Tag{
					freshTag("0.4.0", "other-stream", Yes),
					freshTag("0.3.80-rc.1", "prerelease", Yes),
					freshTag("0.3.81", "other-branch", No),
				}
				in.Repositories[2].Tags = nil
			},
			relation: "no-tag", reason: "FACT_VERSION 0.3.79 was observed on stackrox/fact/release-0.3", allNoTag: true,
		},
		"should wait for a pending inventory": {
			mutate:   func(in *Input) { in.Repositories[1].TagsPending = true },
			relation: "unknown", reason: "complete, current tag inventory",
		},
		"should treat a truncated inventory as unknown": {
			mutate:   func(in *Input) { in.Repositories[1].Evidence.Error = "GitHub pagination limit reached" },
			relation: "unknown", reason: "fresh branch and tag inventory evidence",
		},
		"should treat a deferred inventory as unknown": {
			mutate:   func(in *Input) { in.Repositories[1].Evidence.Error = "GitHub tag inventory deferred" },
			relation: "unknown", reason: "fresh branch and tag inventory evidence",
		},
		"should treat a rate-limited inventory as unknown": {
			mutate:   func(in *Input) { in.Repositories[1].Evidence.Error = "GitHub rate limited" },
			relation: "unknown", reason: "fresh branch and tag inventory evidence",
		},
		"should treat an inventory with stale tags as unknown": {
			mutate: func(in *Input) {
				tag := freshTag("0.3.79", "tag-sha", Yes)
				tag.Evidence.Known = false
				in.Repositories[1].Tags = []Tag{tag}
			},
			relation: "unknown", reason: "complete, current tag inventory",
		},
		"should treat an inventory with an unresolved tag as unknown": {
			mutate:   func(in *Input) { in.Repositories[1].Tags = []Tag{freshTag("0.3.79", "", Yes)} },
			relation: "unknown", reason: "complete, current tag inventory",
		},
		"should treat an inventory with unknown ancestry as unknown": {
			mutate:   func(in *Input) { in.Repositories[1].Tags = []Tag{freshTag("0.3.79", "tag-sha", "")} },
			relation: "unknown", reason: "complete, current tag inventory",
		},
		"should require an observed branch head": {
			mutate:   func(in *Input) { in.Repositories[1].Head = "" },
			relation: "unknown", reason: "current evidence for branch release-0.3",
		},
		"should require fresh branch evidence": {
			mutate:   func(in *Input) { in.Repositories[1].Evidence.Known = false },
			relation: "unknown", reason: "fresh branch and tag inventory evidence",
		},
		"should reject a stale previous pin": {
			mutate:   func(in *Input) { in.Repositories[0].PreviousPins.Evidence.Known = false },
			relation: "unknown", reason: "previous ACS pin evidence for FACT_VERSION 0.3.79",
		},
		"should reject an errored previous pin": {
			mutate:   func(in *Input) { in.Repositories[0].PreviousPins.Evidence.Error = "Previous release unavailable" },
			relation: "unknown", reason: "previous ACS pin evidence for FACT_VERSION 0.3.79",
		},
		"should use semantic ordering with a prefixed pin and plain tag": {
			pin: "v0.3.9", tag: "0.3.10", relation: "newer", reason: "newer than the previous ACS pin FACT_VERSION 0.3.9",
			mutate: func(in *Input) {
				in.Repositories[1].Tags = append(in.Repositories[1].Tags, freshTag("v0.3.9", "previous", Yes))
			},
		},
		"should recognize a newer prefixed tag and plain pin": {
			pin: "0.3.78", tag: "v0.3.79", relation: "newer", reason: "newer than the previous ACS pin FACT_VERSION 0.3.78",
		},
		"should identify a matching tag behind the tip": {
			tag: "v0.3.79", head: "after-tag", relation: "equal", reason: "has advanced or has open PRs", ahead: true,
		},
		"should count only open PRs targeting the configured component branch": {
			tag: "v0.3.79", relation: "equal", reason: "has advanced or has open PRs", openPR: true,
			mutate: func(in *Input) {
				fact := &in.Repositories[1]
				fact.PRs = []PR{
					{Target: Target{Repo: fact.Source.Repo, PR: 1}, Base: fact.Source.Branch, State: "open", Evidence: freshEvidence()},
					{Target: Target{Repo: fact.Source.Repo, PR: 2}, Base: "master", State: "open"},
					{Target: Target{Repo: "stackrox/other", PR: 3}, Base: fact.Source.Branch, State: "open"},
					{Target: Target{Repo: fact.Source.Repo, PR: 4}, Base: fact.Source.Branch, State: "closed"},
				}
			},
		},
		"should recognize the previous version at the current branch tip": {
			pin: "v0.3.79", tag: "0.3.79", relation: "equal", reason: "is at the branch tip",
		},
		"should identify a newest tag older than the pin": {
			pin: "0.3.80", tag: "v0.3.79", relation: "older", reason: "older than the previous ACS pin",
		},
		"should identify a pin outside the configured stream": {
			pin: "0.2.80", tag: "v0.3.79", relation: "untagged-pin", reason: "has no matching eligible Fact tag",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			acs, fact := &in.Repositories[0], &in.Repositories[1]
			acs.PreviousPins = Pins{
				Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"},
				Paths:    map[string]string{"fact": "FACT_VERSION"}, Evidence: freshEvidence(),
			}
			acs.PreviousPublished = Yes
			if tt.pin != "" {
				acs.PreviousPins.Versions["fact"] = tt.pin
			}
			fact.Tags = nil
			if tt.tag != "" {
				fact.Tags = []Tag{freshTag(tt.tag, fact.Head, Yes)}
				in.Choices.Tags["fact"] = tt.tag
			}
			if tt.head != "" {
				fact.Head = tt.head
			}
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			a := Assess(in)
			if len(a.ComponentBaseline) != 2 || a.ComponentBaseline[1].Component != "collector" {
				t.Fatalf("expected Fact and Collector only for ACS 5.0, got %+v", a.ComponentBaseline)
			}
			record := a.ComponentBaseline[0]
			if record.Component != "fact" || record.PinFile != "FACT_VERSION" || record.Relation != tt.relation ||
				record.PinnedVersion != strings.TrimPrefix(acs.PreviousPins.Versions["fact"], "v") ||
				record.NewestTag != tt.tag || record.BranchAhead != tt.ahead || record.OpenPRs > 1 || (record.OpenPRs == 1) != tt.openPR {
				t.Fatalf("unexpected Fact comparison: %+v", record)
			}
			if tt.tag != "" && (record.NewestSHA != "fact-head" || !strings.Contains(record.Reason, tt.tag)) {
				t.Fatalf("missing tag identity in %+v", record)
			}
			if !strings.HasSuffix(record.Reason, ".") || len(record.Evidence) < 2 || record.Evidence[0] != acs.PreviousPins.Evidence {
				t.Fatalf("missing reason or evidence: %+v", record)
			}
			if !strings.Contains(record.Reason, tt.reason) {
				t.Fatalf("reason does not contain %q: %s", tt.reason, record.Reason)
			}
			if got := a.ComponentPRPreparation() != ""; got != tt.allNoTag {
				t.Fatalf("PR preparation notice present = %t, want %t", got, tt.allNoTag)
			}
			allAtTip := tt.relation == "equal" && !tt.ahead && !tt.openPR
			if a.PatchMayBeUnnecessary != allAtTip {
				t.Fatalf("previous patch notice = %t, want %t", a.PatchMayBeUnnecessary, allAtTip)
			}
			acs.PreviousPins = Pins{}
			withoutBaseline := Assess(in)
			if a.Ready != withoutBaseline.Ready || !reflect.DeepEqual(a.Conditions, withoutBaseline.Conditions) {
				t.Fatalf("baseline changed readiness or conditions: ready=%t vs %t", a.Ready, withoutBaseline.Ready)
			}
		})
	}
}

func TestAssessScannerBaseline(t *testing.T) {
	for name, pin := range map[string]string{
		"should assess Scanner for ACS 4.11":                         "4.11.2",
		"should keep Fact and Collector known without a Scanner pin": "",
	} {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			in.Config.Version, in.Config.Branch = "4.11.3", "release-4.11"
			scanner := Source{Name: "scanner", Repo: "stackrox/scanner", Branch: "release-4.11", Stream: "4.11"}
			in.Config.Components = append(in.Config.Components, scanner)
			acs := &in.Repositories[0]
			acs.Source = in.Config.ACS()
			acs.PreviousPins = Pins{Versions: map[string]string{"fact": "0.3.78", "collector": "0.4.12"}, Evidence: freshEvidence()}
			if pin != "" {
				acs.PreviousPins.Versions["scanner"] = pin
			} else {
				acs.PreviousPins.Evidence.Known = false
				acs.PreviousPins.Evidence.Error = "SCANNER_VERSION: file not found"
				acs.PreviousPins.Failures = map[string]string{"scanner": "file not found"}
			}
			in.Repositories = append(in.Repositories, Repository{Source: scanner, Head: "scanner-head", Evidence: freshEvidence(), Tags: []Tag{freshTag("4.11.3", "scanner-head", Yes)}})
			a := Assess(in)
			if len(a.ComponentBaseline) != 3 || a.ComponentBaseline[0].Relation != "newer" || a.ComponentBaseline[1].Relation != "equal" {
				t.Fatalf("unexpected ACS 4.11 comparison: %+v", a.ComponentBaseline)
			}
			want := "newer"
			if pin == "" {
				want = "unknown"
			}
			record := a.ComponentBaseline[2]
			if record.Component != "scanner" || record.Relation != want || record.PinnedVersion != pin || record.NewestTag != "4.11.3" || !strings.Contains(record.Reason, "SCANNER_VERSION") {
				t.Fatalf("unexpected Scanner comparison: %+v", record)
			}
			in.Repositories[0].PreviousPins = Pins{}
			withoutBaseline := Assess(in)
			if a.Ready != withoutBaseline.Ready || !reflect.DeepEqual(a.Conditions, withoutBaseline.Conditions) {
				t.Fatalf("Scanner baseline changed readiness or conditions: ready=%t vs %t", a.Ready, withoutBaseline.Ready)
			}
		})
	}
}

func TestAssessPerImageStatuses(t *testing.T) {
	tests := map[string]struct {
		mutate     func(*Input)
		wantStatus Truth
		wantReason string
	}{
		"available": {wantStatus: Yes, wantReason: "available: quay.io/rox/fact-ci:v0.3.79"},
		"confirmed missing": {
			mutate:     func(in *Input) { in.Inventory.Images[0].Present = No },
			wantStatus: No, wantReason: "missing: quay.io/rox/fact-ci:v0.3.79",
		},
		"registry failure is unknown": {
			mutate:     func(in *Input) { in.Inventory.Images[0].Evidence = Evidence{Known: true, Error: "unauthorized"} },
			wantStatus: "", wantReason: "registry result for fact is unavailable",
		},
		"selected version missing is unknown": {
			mutate:     func(in *Input) { in.Choices.Tags["fact"] = "" },
			wantStatus: "", wantReason: "selected fact version is unavailable",
		},
		"component version binding mismatch is unknown": {
			mutate:     func(in *Input) { in.Inventory.Versions["fact"] = "0.3.78" },
			wantStatus: "", wantReason: "do not match selected version 0.3.79",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			a := Assess(in)
			image := imageStatusByRef(t, a, "quay.io/rox/fact-ci:v0.3.79")
			if image.Status != tt.wantStatus || !strings.Contains(image.Reason, tt.wantReason) {
				t.Fatalf("image status = %q, reason %q; want %q containing %q", image.Status, image.Reason, tt.wantStatus, tt.wantReason)
			}
			if !image.Required || image.SelectedTag != "v0.3.79" && name != "selected version missing is unknown" {
				t.Fatalf("component image binding = required %t, selected tag %q", image.Required, image.SelectedTag)
			}
		})
	}
}

func TestComponentImageTagSelectionNotice(t *testing.T) {
	in := readyInput()
	in.Inventory.Evidence = Evidence{}
	if got := Assess(in).ImageProblem; got != "Component image checks do not have up-to-date results." {
		t.Fatalf("all tags selected: %q", got)
	}
	delete(in.Choices.Tags, "collector")
	if got := Assess(in).ImageProblem; got != "Select component tags first. 1 selected, 1 missing." {
		t.Fatalf("one tag selected: %q", got)
	}
	in.Choices.Tags = nil
	if got := Assess(in).ImageProblem; got != "Select component tags first. 0 selected, 2 missing." {
		t.Fatalf("no tags selected: %q", got)
	}
}

func TestAssessPreservesPartialImagesAndReportsIncompleteResolution(t *testing.T) {
	in := readyInput()
	in.Inventory.Images[0].Evidence = Evidence{Known: true, Error: "unauthorized"}
	a := Assess(in)
	failed := imageStatusByRef(t, a, "quay.io/rox/fact-ci:v0.3.79")
	available := imageStatusByRef(t, a, "quay.io/rox/fact-konflux:v0.3.79")
	if failed.Status != "" || available.Status != Yes {
		t.Fatalf("partial image results were not retained: failed=%+v available=%+v", failed, available)
	}
	if a.ImageProblem == "" {
		t.Fatal("incomplete image results have no ImageProblem")
	}
}

func TestAssessIncludesInformationalACSReleaseImages(t *testing.T) {
	in := readyInput()
	in.Inventory.ReleaseImages = []Image{{
		Component: "acs", Family: imageCI, Name: "main", Reference: "quay.io/rhacs-eng/main:5.0.1-rc.0",
		Present: Yes, Evidence: freshEvidence(),
	}}
	in.Inventory.Versions["acs"] = "5.0.1-rc.0"
	in.Inventory.ReleaseImages[0].Evidence = Evidence{Known: true, Error: "registry unavailable"}
	a := Assess(in)
	if !a.Ready {
		t.Fatal("an unavailable later ACS build must not block component prerequisites")
	}
	image := imageStatusByRef(t, a, "quay.io/rhacs-eng/main:5.0.1-rc.0")
	if image.Required || image.Status != "" || image.SelectedTag != "5.0.1-rc.0" {
		t.Fatalf("ACS release image was not retained as informational: %+v", image)
	}
}

func TestAssessPinSummarySeparatesActualPinsFromPRProposal(t *testing.T) {
	tests := map[string]struct {
		mutate         func(*Input)
		wantBranch     Truth
		wantPR         Truth
		wantBranchText string
		wantPRText     string
	}{
		"matching branch and open matching PR": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PinningPins = Pins{Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			wantBranch: Yes, wantPR: "", wantBranchText: "match selected versions", wantPRText: "The open pinning PR sets all selected component versions.",
		},
		"matching branch and wrong proposal": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PinningPins = Pins{Versions: map[string]string{"fact": "0.3.78", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			wantBranch: Yes, wantPR: No, wantBranchText: "match selected versions", wantPRText: "The pinning PR sets FACT_VERSION to 0.3.78; your selected Fact version is 0.3.79.",
		},
		"mismatched actual branch names versions": {
			mutate:     func(in *Input) { in.Repositories[0].Pins.Versions["fact"] = "0.3.78" },
			wantBranch: No, wantBranchText: "fact actual 0.3.78, selected 0.3.79",
		},
		"merged PR requires matching actual branch": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 100}, Base: in.Config.Branch, State: "closed", Merged: true, Evidence: freshEvidence()}
			},
			wantBranch: Yes, wantPR: Yes, wantPRText: "Merged; actual ACS branch pins match",
		},
		"missing actual pins are unknown": {
			mutate:     func(in *Input) { delete(in.Repositories[0].Pins.Versions, "fact") },
			wantBranch: "", wantBranchText: "pkg/fact/version is empty or missing",
		},
		"wrong PR target branch is no": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 101}, Base: "master", State: "open", Evidence: freshEvidence()}
			},
			wantBranch: Yes, wantPR: No, wantPRText: "Expected stackrox/stackrox on release-5.0; got stackrox/stackrox on master",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			pins := Assess(in).Pins
			if pins.BranchStatus != tt.wantBranch || pins.PRStatus != tt.wantPR {
				t.Fatalf("pin summary statuses = branch %q, PR %q; want %q, %q", pins.BranchStatus, pins.PRStatus, tt.wantBranch, tt.wantPR)
			}
			if !strings.Contains(pins.BranchReason, tt.wantBranchText) || !strings.Contains(pins.PRReason, tt.wantPRText) {
				t.Fatalf("pin summary reasons = branch %q, PR %q", pins.BranchReason, pins.PRReason)
			}
			if name == "matching branch and open matching PR" && (pins.PRNumber != 99 || !strings.Contains(pins.PRURL, "github.com/stackrox/stackrox/pull/99")) {
				t.Fatalf("pinning PR link was not preserved: %+v", pins)
			}
		})
	}
}

func TestAssessPinSummaryDiagnostics(t *testing.T) {
	tests := map[string]struct {
		mutate            func(*Input)
		wantBranch        string
		wantBranchSummary string
		wantPRSummary     string
		wantPRReason      string
		branchStatus      Truth
		prStatus          Truth
		branchUnknown     bool
		prUnknown         bool
		absentBranch      string
	}{
		"initial reads are pending": {
			mutate: func(in *Input) {
				in.Config.PinningPR = 99
				in.Repositories[0].Pins = Pins{}
			},
			wantBranch:    "Pending: reading ACS release branch version files",
			wantPRSummary: "Pending: reading pinning PR #99",
			branchUnknown: true,
			prUnknown:     true,
		},
		"branch request failure is not pending": {
			mutate: func(in *Input) {
				in.Repositories[0].Head = ""
				in.Repositories[0].Evidence = Evidence{Error: "Authentication failed (HTTP 401)"}
			},
			wantBranchSummary: "Unable to get the ACS release branch head.",
			wantBranch:        "Authentication failed (HTTP 401)",
		},
		"pinning version file failure names file and cause": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Versions["fact"] = "0.3.78"
				in.Config.PinningPR = 99
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PinningEvidence = freshEvidence()
				in.Repositories[0].PinningPins = Pins{Paths: map[string]string{"fact": "FACT_VERSION"}, Failures: map[string]string{"fact": "Access denied (HTTP 403)"}, Evidence: Evidence{Error: "Access denied (HTTP 403)"}}
			},
			wantPRSummary: "Unable to get FACT_VERSION",
			wantPRReason:  "FACT_VERSION: Access denied (HTTP 403)",
		},
		"pin file authentication failure names cause and file": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Evidence = Evidence{Known: true, Error: "Authentication failed (HTTP 401)"}
				in.Repositories[0].Pins.Failures = map[string]string{"fact": "Authentication failed (HTTP 401)"}
			},
			wantBranch: "pkg/fact/version: Authentication failed (HTTP 401)",
		},
		"pinning PR not found retains configured link": {
			mutate: func(in *Input) {
				in.Config.PinningPR = 99
				in.Repositories[0].PinningEvidence = Evidence{Known: true, Error: "PR not found (HTTP 404)"}
			},
			wantPRSummary: "Unable to get PR status and component versions from pinning PR #99",
			wantPRReason:  "PR not found (HTTP 404)",
		},
		"pinning PR rate limit failure": {
			mutate: func(in *Input) {
				in.Config.PinningPR = 99
				in.Repositories[0].PinningEvidence = Evidence{Known: true, Error: "Rate limit exceeded (HTTP 429)"}
			},
			wantPRSummary: "Unable to get PR status and component versions from pinning PR #99",
			wantPRReason:  "Rate limit exceeded (HTTP 429)",
		},
		"pinning PR timeout does not report stale open state": {
			mutate: func(in *Input) {
				in.Config.PinningPR = 99
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PinningEvidence = Evidence{Known: true, Error: "Request timed out"}
			},
			wantPRSummary: "Unable to get PR status and component versions from pinning PR #99",
			wantPRReason:  "Request timed out",
			prUnknown:     true,
		},
		"failed component tag discovery does not ask to reselect an existing tag": {
			mutate: func(in *Input) {
				in.Repositories[1].Tags = nil
			},
			branchStatus: Yes,
			wantBranch:   "Actual branch pins match selected versions",
			absentBranch: "Choose Fact tag",
		},
		"partial branch pins identify missing file": {
			mutate: func(in *Input) {
				delete(in.Repositories[0].Pins.Versions, "fact")
			},
			wantBranch: "pkg/fact/version is empty or missing",
		},
		"empty branch version file is named": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Versions["fact"] = ""
			},
			wantBranch: "pkg/fact/version is empty or missing",
		},
		"missing local tags name the exact selection action": {
			mutate: func(in *Input) {
				in.Config.PinningPR = 99
				in.Choices.Tags["fact"] = ""
				in.Choices.Tags["collector"] = ""
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 99}, Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
				in.Repositories[0].PinningEvidence = freshEvidence()
				in.Repositories[0].PinningPins = Pins{Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"}, Evidence: freshEvidence()}
			},
			wantBranch:    "Choose Fact and Collector tags",
			wantPRSummary: "component tag selection required",
			wantPRReason:  "Choose Fact and Collector tags",
		},
		"merged PR remains verified despite unrelated discovery error": {
			mutate: func(in *Input) {
				in.Repositories[0].Evidence.Error = "unrelated tag discovery failed"
				in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 100}, Base: in.Config.Branch, State: "closed", Merged: true, Evidence: freshEvidence()}
				in.Repositories[0].PinningEvidence = freshEvidence()
			},
			branchStatus:  Yes,
			prStatus:      Yes,
			wantBranch:    "match selected versions",
			wantPRSummary: "Merged; actual ACS branch pins match selected versions",
		},
		"actual mismatch stays red": {
			mutate:       func(in *Input) { in.Repositories[0].Pins.Versions["fact"] = "0.3.78" },
			branchStatus: No,
			wantBranch:   "fact actual 0.3.78, selected 0.3.79",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			if tt.mutate != nil {
				tt.mutate(&in)
			}
			assessment := Assess(in)
			pins := assessment.Pins
			if tt.wantBranchSummary != "" && pins.BranchSummary != tt.wantBranchSummary {
				t.Fatalf("branch summary = %q, want %q", pins.BranchSummary, tt.wantBranchSummary)
			}
			if (tt.branchStatus != "" && tt.branchStatus != pins.BranchStatus) || (tt.branchUnknown && pins.BranchStatus != "") {
				t.Fatalf("branch status = %q, want %q (%s)", pins.BranchStatus, tt.branchStatus, pins.BranchReason)
			}
			if (tt.prStatus != "" && tt.prStatus != pins.PRStatus) || (tt.prUnknown && pins.PRStatus != "") {
				t.Fatalf("PR status = %q, want %q (%s)", pins.PRStatus, tt.prStatus, pins.PRReason)
			}
			if tt.wantBranch != "" && !strings.Contains(pins.BranchReason, tt.wantBranch) {
				t.Fatalf("branch reason %q does not contain %q", pins.BranchReason, tt.wantBranch)
			}
			if tt.absentBranch != "" && strings.Contains(pins.BranchReason, tt.absentBranch) {
				t.Fatalf("branch reason %q unexpectedly contains %q", pins.BranchReason, tt.absentBranch)
			}
			if tt.wantPRSummary != "" && !strings.Contains(pins.PRSummary, tt.wantPRSummary) {
				t.Fatalf("PR summary %q does not contain %q", pins.PRSummary, tt.wantPRSummary)
			}
			if tt.wantPRReason != "" && !strings.Contains(pins.PRReason, tt.wantPRReason) {
				t.Fatalf("PR reason %q does not contain %q", pins.PRReason, tt.wantPRReason)
			}
			if strings.Contains(pins.BranchSummary, "have not been compared") || strings.Contains(pins.PRSummary, "not verified") {
				t.Fatalf("pin summary contains vague diagnostics: %+v", pins)
			}
			if in.Config.PinningPR > 0 && pins.PRURL != "https://github.com/stackrox/stackrox/pull/"+fmt.Sprint(in.Config.PinningPR) {
				t.Fatalf("configured pinning PR URL was not retained: %q", pins.PRURL)
			}
		})
	}
}

func imageStatusByRef(t *testing.T, assessment Assessment, ref string) ImageStatus {
	t.Helper()
	for _, image := range assessment.Images {
		if image.Image.Reference == ref {
			return image
		}
	}
	t.Fatalf("image %q not found in assessment", ref)
	return ImageStatus{}
}

func TestAssessReportsOutstandingConditions(t *testing.T) {
	for name, tt := range map[string]struct {
		mutate    func(*Input)
		condition string
		reason    string
		status    Truth
	}{
		"missing component image": {
			mutate:    func(in *Input) { in.Inventory.Images[0].Present = No },
			condition: "image fact ci",
			reason:    "The ci image is missing: quay.io/rox/fact-ci:v0.3.79",
			status:    No,
		},
		"unavailable registry observation": {
			mutate: func(in *Input) {
				in.Inventory.Images[0].Evidence = Evidence{Known: true, Error: "unauthorized"}
			},
			condition: "image fact ci",
			reason:    "registry result for fact is unavailable",
		},
		"unavailable ACS pin observations": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Evidence = Evidence{Known: true, Error: "discovery unavailable"}
			},
			condition: "ACS release branch pins",
			reason:    "discovery unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			tt.mutate(&in)
			a := Assess(in)
			if a.Ready {
				t.Fatal("assessment is ready with an outstanding condition")
			}
			condition := conditionByName(t, a, tt.condition)
			if condition.Status != tt.status || !strings.Contains(condition.Reason, tt.reason) {
				t.Fatalf("unexpected condition: %+v", condition)
			}
		})
	}
}

func readyInput() Input {
	config := Config{Version: "5.0.1", Branch: "release-5.0", Components: []Source{
		{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.3", Stream: "0.3"},
		{Name: "collector", Repo: "stackrox/collector", Branch: "release-0.4", Stream: "0.4"},
	}}
	acs := Repository{Source: config.ACS(), Head: "acs-head", Evidence: freshEvidence(), Pins: Pins{
		Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"},
		Paths:    map[string]string{"fact": "pkg/fact/version", "collector": "pkg/collector/version"}, Evidence: freshEvidence(),
	}}
	factSource, collectorSource := config.Components[0], config.Components[1]
	fact := Repository{Source: factSource, Head: "fact-head", Evidence: freshEvidence(), Tags: []Tag{freshTag("v0.3.79", "fact-head", Yes)}}
	collector := Repository{Source: collectorSource, Head: "collector-head", Evidence: freshEvidence(), Tags: []Tag{freshTag("v0.4.12", "collector-head", Yes)}}
	images := []Image{
		freshImage("fact", imageCI, "quay.io/rox/fact-ci:v0.3.79"),
		freshImage("fact", imageKonflux, "quay.io/rox/fact-konflux:v0.3.79"),
		freshImage("collector", imageCI, "quay.io/rox/collector-ci:v0.4.12"),
		freshImage("collector", imageKonflux, "quay.io/rox/collector-konflux:v0.4.12"),
	}
	return Input{Config: config, Choices: Choices{Tags: map[string]string{"fact": "v0.3.79", "collector": "v0.4.12"}, PRs: map[Target]Choice{}},
		Repositories: []Repository{acs, fact, collector}, Inventory: Inventory{ReleaseVersion: config.Version, Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"}, Images: images, Evidence: freshEvidence()}}
}

func freshEvidence() Evidence {
	return Evidence{Known: true, At: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
}

func freshTag(name, sha string, belongs Truth) Tag {
	return Tag{Name: name, SHA: sha, Belongs: belongs, Contains: map[string]Truth{}, Evidence: freshEvidence()}
}

func freshImage(component, family, reference string) Image {
	return Image{Component: component, Family: family, Reference: reference, Present: Yes, Evidence: freshEvidence()}
}

func mergedPR(component string, number int, mergeSHA string) PR {
	branch := "release-0.3"
	if component == "collector" {
		branch = "release-0.4"
	}
	return PR{Target: Target{Repo: "stackrox/" + component, PR: number}, Base: branch, State: "closed", Merged: true, MergeSHA: mergeSHA, Evidence: freshEvidence()}
}

func conditionByName(t *testing.T, assessment Assessment, name string) Condition {
	t.Helper()
	for _, condition := range assessment.Conditions {
		if condition.Name == name || strings.HasPrefix(condition.Name, name+" ") {
			return condition
		}
	}
	t.Fatalf("condition %q not found", name)
	return Condition{}
}

func TestReleaseGuideForSelectedChangesWithoutCoveringTag(t *testing.T) {
	in := readyInput()
	pr := mergedPR("fact", 71, "later-merge")
	in.Choices.PRs[pr.Target] = Included
	in.Repositories[1].PRs = append(in.Repositories[1].PRs, pr)
	in.Repositories[1].Tags[0].Contains[pr.MergeSHA] = No
	a := Assess(in)
	if !strings.Contains(a.ReleaseGuides["fact"], "/docs/release.md") {
		t.Fatal("component release instructions missing")
	}
}
func TestPinningRecommendationCanReplaceCurrentPrereleasePins(t *testing.T) {
	in := readyInput()
	in.Repositories[0].Pins.Versions["fact"] = "0.3.80-1-gabcdef"
	a := Assess(in)
	if !strings.Contains(a.PinningRecommendation, "pkg/fact/version to 0.3.79") {
		t.Fatal("recommendation omitted the replacement for the prerelease pin")
	}
}

func TestChangedPinningPRInputDoesNotReuseEarlierProposal(t *testing.T) {
	in := readyInput()
	in.Config.PinningPR = 200
	in.Repositories[0].Pins.Versions["fact"] = "0.3.78"
	in.Repositories[0].PinningPR = &PR{Target: Target{Repo: "stackrox/stackrox", PR: 100}, State: "open", Base: in.Config.Branch, Evidence: freshEvidence()}
	a := Assess(in)
	if a.PinningRecommendation != "" || !strings.Contains(a.Pins.PRSummary, "Pending: reading pinning PR #200") {
		t.Fatal("old pinning target reused", a)
	}
}

func TestAssessIncompletePatternSetPreservesIndependentImageResults(t *testing.T) {
	in := readyInput()
	in.Inventory.Evidence.Known = false
	in.Inventory.Evidence.Error = "One expected image reference is unresolved"
	in.Inventory.Images[1].Evidence.Known = false
	in.Inventory.Images[1].Evidence.Error = in.Inventory.Evidence.Error
	a := Assess(in)
	if imageStatusByRef(t, a, in.Inventory.Images[0].Reference).Status != Yes || imageStatusByRef(t, a, in.Inventory.Images[1].Reference).Status != "" || a.Ready || a.PinningRecommendation != "" {
		t.Fatalf("partial image resolution lost independent results or permitted readiness: %+v", a)
	}
	// An incomplete expected image set cannot be accepted even if every returned row is available.
	in.Inventory.Images[1].Evidence = freshEvidence()
	if Assess(in).Ready {
		t.Fatal("incomplete image set allowed readiness")
	}
}

func TestAssessUnreadPinningPRRetainsSuppliedLink(t *testing.T) {
	in := readyInput()
	in.Config.PinningPR = 123
	a := Assess(in)
	if a.Pins.PRNumber != 123 || a.Pins.PRURL != "https://github.com/stackrox/stackrox/pull/123" || a.Pins.PRStatus != "" {
		t.Fatalf("unread PR summary: %+v", a.Pins)
	}
}

func TestImageAvailabilityDoesNotDependOnGitHubObservations(t *testing.T) {
	for name, mutate := range map[string]func(*Input){
		"no repository observations":        func(in *Input) { in.Repositories = nil },
		"component repository inaccessible": func(in *Input) { in.Repositories[1].Evidence = Evidence{Error: "GitHub unavailable"} },
		"ACS repository inaccessible":       func(in *Input) { in.Repositories[0].Evidence = Evidence{Error: "GitHub unavailable"} },
	} {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			mutate(&in)
			result := Assess(in)
			for _, image := range result.Images {
				if image.Required && image.Status != Yes {
					t.Errorf("registry result depends on source availability: %+v", image)
				}
			}
			if result.Ready || result.PinningRecommendation != "" {
				t.Fatal("image availability bypassed unknown GitHub readiness prerequisites")
			}
		})
	}
}
