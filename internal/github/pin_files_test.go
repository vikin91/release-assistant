package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestDiscoverPinFiles(t *testing.T) {
	for name, tc := range map[string]struct {
		version, path, want string
		fail                bool
	}{
		"single Fact pin":           {"5.0.1", "FACT_VERSION", "Changes FACT_VERSION.", false},
		"ACS 4 scanner pin":         {"4.11.1", "SCANNER_VERSION", "Changes SCANNER_VERSION.", false},
		"ACS 5 ignores scanner pin": {"5.0.1", "SCANNER_VERSION", "", false},
		"unrelated path":            {"5.0.1", "docs/FACT_VERSION", "", false},
		"incomplete second page":    {"5.0.1", "FACT_VERSION", "", true},
	} {
		t.Run(name, func(t *testing.T) {
			raw := prResponse{Number: 7, State: "open"}
			raw.Base.Ref = "release"
			raw.Base.SHA = "base"
			raw.Head.SHA = "head"
			fileReads := 0
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/branches/release"):
					fmt.Fprint(w, `{"commit":{"sha":"base"}}`)
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					_ = json.NewEncoder(w).Encode([]prResponse{raw})
				case strings.HasSuffix(r.URL.Path, "/files"):
					fileReads++
					if r.URL.Query().Get("page") == "1" {
						fmt.Fprint(w, "["+strings.Repeat(`{"filename":"other"},`, 99)+`{"filename":"more"}]`)
						return
					}
					if tc.fail {
						http.Error(w, "unavailable", http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode([]map[string]string{{"filename": tc.path}})
				case strings.HasSuffix(r.URL.Path, "/7"):
					_ = json.NewEncoder(w).Encode(raw)
				default:
					t.Fatalf("unexpected read %s", r.URL)
				}
			}))
			c := New(srv.URL, "", srv.Client())
			req := app.DiscoveryRequest{Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: tc.version, Scope: app.ReadPRs, DetectPinningPR: true}
			read := func() release.PinDiscovery { r, _ := c.Discover(t.Context(), req); return r.PinDiscovery }
			d := read()
			if d.Evidence.Fresh() == tc.fail || d.Files[7].Reason != tc.want || fileReads != 2 {
				t.Fatalf("discovery %+v, reads %d", d, fileReads)
			}
			req.PreviousPinFiles = d.Files
			read()
			if tc.fail {
				if fileReads != 4 {
					t.Fatal("incomplete evidence cached")
				}
				return
			}
			if fileReads != 2 {
				t.Fatal("unchanged comparison reread")
			}
			raw.Base.SHA = "new base"
			read()
			raw.Base.SHA = "base"
			raw.Head.SHA = "new head"
			read()
			if fileReads != 6 {
				t.Fatal("changed revision reused cached comparison")
			}
		})
	}
}
