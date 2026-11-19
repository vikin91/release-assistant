package github

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestDiscoverMonitorsSuppliedOpenPinningPR(t *testing.T) {
	tests := []struct {
		name          string
		listed        bool
		listedSHA     string
		supplied      string
		ignored       bool
		wantChecks    int
		wantListCount int
		wantSHA       string
		wantTitle     string
		wantReadHeads []string
	}{
		{
			name: "absent from open PR list", supplied: pinningPRJSON("open", false, "head-1", "release", "supplied", false),
			wantChecks: 1, wantListCount: 1, wantSHA: "head-1", wantTitle: "supplied", wantReadHeads: []string{"head-1"},
		},
		{
			name: "reuse checks for same head and refresh identity", listed: true, listedSHA: "head-1",
			supplied:   pinningPRJSON("open", false, "head-1", "release", "supplied", true),
			wantChecks: 1, wantListCount: 1, wantSHA: "head-1", wantTitle: "supplied", wantReadHeads: []string{"head-1"},
		},
		{
			name: "read fresh checks after head changes", listed: true, listedSHA: "old-head",
			supplied:   pinningPRJSON("open", false, "new-head", "release", "supplied", false),
			wantChecks: 2, wantListCount: 1, wantSHA: "new-head", wantTitle: "supplied", wantReadHeads: []string{"old-head", "new-head"},
		},
		{
			name: "ignored", supplied: pinningPRJSON("open", false, "head-1", "release", "supplied", false), ignored: true,
			wantChecks: 0, wantListCount: 1,
		},
		{
			name: "wrong base", supplied: pinningPRJSON("open", false, "head-1", "other", "supplied", false),
			wantChecks: 0, wantListCount: 0,
		},
		{
			name: "merged", supplied: pinningPRJSON("closed", true, "head-1", "release", "supplied", false),
			wantChecks: 0, wantListCount: 1,
		},
		{
			name: "closed without merge", supplied: pinningPRJSON("closed", false, "head-1", "release", "supplied", false),
			wantChecks: 0, wantListCount: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			checkReads := map[string]int{}
			var listJSON string
			if tc.listed {
				listJSON = `[ {"number":44,"title":"stale","state":"open","html_url":"https://github.com/acme/acs/pull/44","head":{"sha":"` + tc.listedSHA + `"},"base":{"ref":"release"}} ]`
			} else {
				listJSON = `[]`
			}
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/repos/acme/acs/branches/release":
					_, _ = w.Write([]byte(`{"commit":{"sha":"branch-sha"}}`))
				case r.URL.Path == "/repos/acme/acs/tags", r.URL.Path == "/repos/stackrox/stackrox/tags":
					_, _ = w.Write([]byte(`[]`))
				case r.URL.Path == "/repos/acme/acs/pulls":
					_, _ = w.Write([]byte(listJSON))
				case r.URL.Path == "/repos/acme/acs/pulls/44":
					_, _ = w.Write([]byte(tc.supplied))
				case strings.HasPrefix(r.URL.Path, "/repos/acme/acs/contents/"):
					_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("5.0.0")) + `"}`))
				case strings.Contains(r.URL.Path, "/check-runs"):
					head := strings.Split(strings.Split(r.URL.Path, "/commits/")[1], "/")[0]
					checkReads[head]++
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 1, "check_runs": []map[string]string{{"name": "build", "status": "completed", "conclusion": "success"}}})
				case strings.HasSuffix(r.URL.Path, "/statuses"):
					_, _ = w.Write([]byte(`[]`))
				case strings.Contains(r.URL.Path, "/rules/branches/"):
					_, _ = w.Write([]byte(`[]`))
				case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
					_, _ = w.Write([]byte(`{"contexts":["build"]}`))
				default:
					http.NotFound(w, r)
				}
			}))
			req := app.DiscoveryRequest{Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "5.0.0", PinningPR: 44}
			if tc.ignored {
				req.Ignored = []release.Target{{Repo: "acme/acs", PR: 44}}
			}
			repo, _ := New(srv.URL, "", srv.Client()).Discover(t.Context(), req)
			count := 0
			var listed *release.PR
			for i := range repo.PRs {
				if repo.PRs[i].Target.PR == 44 {
					count++
					listed = &repo.PRs[i]
				}
			}
			if count != tc.wantListCount {
				t.Fatalf("PR count = %d, want %d (PRs: %+v)", count, tc.wantListCount, repo.PRs)
			}
			gotReads := 0
			for _, n := range checkReads {
				gotReads += n
			}
			if gotReads != tc.wantChecks {
				t.Fatalf("check reads = %v, want %d", checkReads, tc.wantChecks)
			}
			for _, head := range tc.wantReadHeads {
				if checkReads[head] != 1 {
					t.Errorf("check reads for %q = %d, want 1 (all reads: %v)", head, checkReads[head], checkReads)
				}
			}
			if tc.wantChecks == 0 && tc.wantListCount == 1 {
				if listed == nil || repo.PinningPR == nil || listed.State != repo.PinningPR.State || len(listed.Checks) != 0 {
					t.Fatalf("invalid tracked identity: %+v", repo)
				}
				return
			}
			if tc.wantListCount == 0 {
				return
			}
			if listed == nil || listed.SHA != tc.wantSHA || listed.Title != tc.wantTitle || len(listed.Checks) != 1 || listed.ChecksEvidence.At.IsZero() {
				t.Fatalf("monitored PR = %+v", listed)
			}
			if repo.PinningPR == nil || repo.PinningPR.SHA != tc.wantSHA || repo.PinningPR.Title != tc.wantTitle || len(repo.PinningPR.Checks) != 1 {
				t.Fatalf("pinning PR observation = %+v", repo.PinningPR)
			}
			if tc.name == "reuse checks for same head and refresh identity" && (!listed.OptOut || !repo.PinningPR.OptOut) {
				t.Fatalf("fresh supplied label was not applied: listed=%+v pinning=%+v", listed, repo.PinningPR)
			}
		})
	}
}

func TestDiscoverPreservesPartialPinningPRChecks(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/acme/acs/branches/release":
			_, _ = w.Write([]byte(`{"commit":{"sha":"branch-sha"}}`))
		case r.URL.Path == "/repos/acme/acs/tags", r.URL.Path == "/repos/stackrox/stackrox/tags":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/acme/acs/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/repos/acme/acs/pulls/44":
			_, _ = w.Write([]byte(pinningPRJSON("open", false, "head-1", "release", "supplied", false)))
		case strings.HasPrefix(r.URL.Path, "/repos/acme/acs/contents/"):
			_, _ = w.Write([]byte(`{"encoding":"base64","content":"` + base64.StdEncoding.EncodeToString([]byte("5.0.0")) + `"}`))
		case strings.Contains(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success"}]}`))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			http.Error(w, "private API response", http.StatusForbidden)
		case strings.Contains(r.URL.Path, "/rules/branches/"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
			_, _ = w.Write([]byte(`{"contexts":["build"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	repo, _ := New(srv.URL, "", srv.Client()).Discover(t.Context(), app.DiscoveryRequest{
		Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "5.0.0", PinningPR: 44,
	})
	if repo.PinningPR == nil || len(repo.PinningPR.Checks) != 1 || repo.PinningPR.ChecksEvidence.Error == "" {
		t.Fatalf("partial pinning PR checks were lost: %+v", repo.PinningPR)
	}
	if len(repo.PRs) != 1 || len(repo.PRs[0].Checks) != 1 || repo.PRs[0].ChecksEvidence.Error == "" {
		t.Fatalf("partial monitored PR checks were lost: %+v", repo.PRs)
	}
}

func pinningPRJSON(state string, merged bool, sha, base, title string, optOut bool) string {
	label := ""
	if optOut {
		label = `,"labels":[{"name":"no-release-assistant"}]`
	}
	return `{"number":44,"title":"` + title + `","state":"` + state + `","merged":` + strconv.FormatBool(merged) + `,"html_url":"https://github.com/acme/acs/pull/44","head":{"sha":"` + sha + `"},"base":{"ref":"` + base + `"}` + label + `}`
}

func TestPinningHealthUsesLatestStatusesAndWaitsForMissingRequiredChecks(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			_, _ = w.Write([]byte(`{"total_count":0,"check_runs":[]}`))
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_, _ = w.Write([]byte(`[{"context":"build","state":"success"},{"context":"build","state":"failure"}]`))
		case strings.Contains(r.URL.Path, "/rules/branches/"):
			_, _ = w.Write([]byte(`[]`))
		case strings.HasSuffix(r.URL.Path, "/required_status_checks"):
			_, _ = w.Write([]byte(`{"contexts":["build","not-yet-reported"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	pr := release.PR{SHA: "head", Evidence: release.Evidence{Known: true, At: time.Now()}}
	New(srv.URL, "", srv.Client()).readChecks(t.Context(), release.Source{Repo: "acme/acs", Branch: "release"}, &pr, "")
	if len(pr.Checks) != 2 || pr.Checks[0].Status != "success" || pr.Checks[1].Name != "not-yet-reported" || pr.Checks[1].Required != release.Yes || pr.ChecksStatus() != "" {
		t.Fatalf("incorrect health evidence: %+v", pr)
	}
}
