package release

import (
	"strings"
	"testing"
)

func TestPinningWorkflowDistinguishesPendingMergeFromBranchProblems(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate          func(*Input)
		wantBranchIssue bool
		wantReady       bool
		wantGuidance    string
	}{
		"should wait for an open matching PR to merge": {
			wantGuidance: "wait for it to merge",
		},
		"should name a mismatched PR version": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPins.Versions["fact"] = "0.3.78"
			},
			wantGuidance: "The pinning PR sets FACT_VERSION to 0.3.78; your selected Fact version is 0.3.79.",
		},
		"should still report PR differences when branch pins already match": {
			mutate: func(in *Input) {
				in.Repositories[0].Pins.Versions["fact"] = "0.3.79"
				in.Repositories[0].PinningPins.Versions["fact"] = "0.3.78"
			},
			wantReady:    true,
			wantGuidance: "The pinning PR sets FACT_VERSION to 0.3.78; your selected Fact version is 0.3.79.",
		},
		"should retain mismatches after merge": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR.State = "closed"
				in.Repositories[0].PinningPR.Merged = true
			},
			wantBranchIssue: true,
		},
		"should become ready after merged pins reach the branch": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR.State = "closed"
				in.Repositories[0].PinningPR.Merged = true
				in.Repositories[0].Pins.Versions["fact"] = "0.3.79"
			},
			wantReady: true,
		},
		"should retain branch problems for an unverified PR": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningEvidence = Evidence{Error: "Request timed out."}
			},
			wantBranchIssue: true,
			wantGuidance:    "Request timed out.",
		},
		"should retain branch problems for another target branch": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR.Base = "master"
			},
			wantBranchIssue: true,
		},
		"should retain branch problems when the PR is ignored": {
			mutate: func(in *Input) {
				in.Choices.PRs[in.Config.PinningTarget()] = Ignored
			},
			wantBranchIssue: true,
		},
		"should retain branch problems when the PR closes without merge": {
			mutate: func(in *Input) {
				in.Repositories[0].PinningPR.State = "closed"
			},
			wantBranchIssue: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := readyInput()
			in.Config.PinningPR = 99
			acs := &in.Repositories[0]
			acs.Pins.Versions["fact"] = "0.3.78"
			acs.PinningPR = &PR{Target: in.Config.PinningTarget(), Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
			acs.PinningEvidence = freshEvidence()
			acs.PinningPins = Pins{
				Versions: map[string]string{"fact": "0.3.79", "collector": "0.4.12"},
				Paths:    map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}, Evidence: freshEvidence(),
			}
			if tc.mutate != nil {
				tc.mutate(&in)
			}
			a := Assess(in)
			if a.Ready != tc.wantReady {
				t.Errorf("RC.0 readiness = %t, want %t", a.Ready, tc.wantReady)
			}
			guidance := strings.Join(a.ACSRecommendations, "\n")
			if got := strings.Contains(guidance, "Review ACS release branch pins:"); got != tc.wantBranchIssue {
				t.Errorf("branch issue = %t, want %t: %s", got, tc.wantBranchIssue, guidance)
			}
			if !strings.Contains(guidance, tc.wantGuidance) {
				t.Errorf("missing guidance %q: %s", tc.wantGuidance, guidance)
			}
		})
	}
}

func TestPinningPRMismatchNamesOnlyFilesNeedingUpdates(t *testing.T) {
	in := readyInput()
	in.Config = Config{Version: "4.10.12", Branch: "release-4.10", PinningPR: 23958, Components: []Source{
		{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.2", Stream: "0.2"},
		{Name: "collector", Repo: "stackrox/collector", Branch: "release-3.24", Stream: "3.24"},
		{Name: "scanner", Repo: "stackrox/scanner", Branch: "release-2.39", Stream: "2.39"},
	}}
	in.Choices.Tags = map[string]string{"fact": "0.2.18", "collector": "3.24.17", "scanner": "2.39.28"}
	acs := &in.Repositories[0]
	acs.Source = in.Config.ACS()
	acs.PinningPR = &PR{Target: in.Config.PinningTarget(), Base: in.Config.Branch, State: "open", Evidence: freshEvidence()}
	acs.PinningPins = Pins{Versions: map[string]string{"fact": "0.2.17", "collector": "3.24.17", "scanner": "2.39.28"}, Evidence: freshEvidence()}
	a := Assess(in)
	want := "The pinning PR sets FACT_VERSION to 0.2.17; your selected Fact version is 0.2.18."
	if len(a.Pins.PRProblems) != 1 || a.Pins.PRProblems[0] != want {
		t.Fatalf("PR problems = %q, want only %q", a.Pins.PRProblems, want)
	}
	if a.Pins.PRStatus != No || !strings.Contains(strings.Join(a.ACSRecommendations, "\n"), want) {
		t.Fatal("PR version mismatch is not actionable without component image evidence", a.Pins, a.ACSRecommendations)
	}
}
