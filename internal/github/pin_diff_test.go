package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

const factPinDiff = `diff --git a/FACT_VERSION b/FACT_VERSION
index abcdef1..1234567 100644
--- a/FACT_VERSION
+++ b/FACT_VERSION
@@ -1 +1 @@
-0.3.76
+0.3.77
`

func TestReadPinningDiffsReusesUnchangedRevisions(t *testing.T) {
	req := release.PinningRequest{Repo: "stackrox/stackrox", Branch: "release-5.0", Head: "base", Version: "5.0.1",
		Changes: []release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\n", After: "0.3.77\n"}}}
	pr := prResponse{Number: 7, State: "open", ChangedFiles: new(1)}
	pr.Base.Ref, pr.Base.SHA, pr.Head.SHA = req.Branch, req.Head, "first"
	var fileReads, rawReads int
	rawDiff := factPinDiff
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_ = json.NewEncoder(w).Encode([]prResponse{pr})
		case strings.HasSuffix(r.URL.Path, "/files"):
			fileReads++
			fmt.Fprint(w, `[{"filename":"FACT_VERSION","status":"modified"}]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7"):
			if r.Header.Get("Accept") == "application/vnd.github.diff" {
				rawReads++
				fmt.Fprint(w, rawDiff)
			} else {
				_ = json.NewEncoder(w).Encode(pr)
			}
		case strings.HasSuffix(r.URL.Path, "/branches/release-5.0"):
			fmt.Fprint(w, `{"commit":{"sha":"base"}}`)
		default:
			t.Errorf("unexpected GitHub read: %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	c := New(srv.URL, "", srv.Client())
	var previous map[int]release.PinFileMatch
	read := func(wantMatch, wantReads int) {
		t.Helper()
		d, err := c.ReadPinningDiffs(t.Context(), req, previous)
		if err != nil || !d.Evidence.Fresh() || d.Duplicate(release.PinSignature(req.Changes)) != wantMatch ||
			fileReads != wantReads || rawReads != wantReads {
			t.Fatalf("discovery=%+v, err=%v, files=%d raw=%d", d, err, fileReads, rawReads)
		}
		previous = d.Files
	}
	read(7, 1)
	read(7, 1)
	pr.Head.SHA = "second"
	rawDiff = strings.ReplaceAll(rawDiff, "+0.3.77", "+0.3.78")
	read(0, 2)
	pr.Base.SHA = "moved-base"
	rawDiff = factPinDiff
	read(7, 3)
	req.Head = "new-base"
	if _, err := c.ReadPinningDiffs(t.Context(), req, previous); err == nil {
		t.Fatal("accepted a changed target branch")
	}
}

func TestExactPinDiffComparesFormattingAndMode(t *testing.T) {
	want := release.PinSignature([]release.PinChange{{Path: "FACT_VERSION", Before: "0.3.76\n", After: "0.3.77\n"}})
	for name, diff := range map[string]string{
		"same":             factPinDiff,
		"mode change":      strings.ReplaceAll(factPinDiff, "index abcdef1..1234567 100644", "old mode 100644\nnew mode 100755\nindex abcdef1..1234567"),
		"no final newline": factPinDiff + "\\ No newline at end of file\n",
		"extra file":       factPinDiff + strings.ReplaceAll(factPinDiff, "FACT_VERSION", "COLLECTOR_VERSION"),
	} {
		t.Run(name, func(t *testing.T) {
			if (exactPinDiff(diff, map[string]bool{"FACT_VERSION": true}) == want) != (name == "same") {
				t.Fatalf("wrong comparison for %q", diff)
			}
		})
	}
}
