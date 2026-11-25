package release

import (
	"slices"
	"strings"
	"testing"
)

func TestPinningHeadBranchUsesShortRevisionAndDiffSuffix(t *testing.T) {
	req := PinningRequest{
		Version: "v4.10.12", Branch: "release-4.10", Head: "67b438b1607022279e8d0e65cf8c768e0abc35d2",
		Changes: []PinChange{
			{Path: "COLLECTOR_VERSION", Before: "3.24.18\n", After: "3.24.19\n"},
			{Path: "FACT_VERSION", Before: "0.2.18\n", After: "0.2.19\n"},
		},
	}
	branch := req.HeadBranch()
	suffix, ok := strings.CutPrefix(branch, "release-assistant/pin-4.10.12-")
	if !ok || len(suffix) != 8 || !ValidRef(branch) {
		t.Fatalf("invalid short branch name %q", branch)
	}
	slices.Reverse(req.Changes)
	if got := req.HeadBranch(); got != branch {
		t.Fatalf("file order changed the branch name to %q", got)
	}
	changedBase := req
	changedBase.Head = "67b438b1607022279e8d0e65cf8c768e0abc35d3"
	if changedBase.HeadBranch() == branch {
		t.Fatal("a different base commit must change the branch name")
	}
	req.Changes[0].After = "0.2.20\n"
	if req.HeadBranch() == branch {
		t.Fatal("a different pin must change the branch name")
	}
}
