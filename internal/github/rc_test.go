package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestDiscoverCandidateUsesHighestReachableRCAndLinksItsMergedPRs(t *testing.T) {
	published := false
	client := New("https://api.github.test", "", &http.Client{Transport: fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox"); path {
		case "/branches/release-5.0":
			fmt.Fprint(w, `{"commit":{"sha":"head"}}`)
		case "/git/matching-refs/tags/5.0.1-rc.":
			fmt.Fprint(w, `[
				{"ref":"refs/tags/5.0.1-rc.9","object":{"sha":"old","type":"commit"}},
				{"ref":"refs/tags/5.0.1-rc.11","object":{"sha":"wrong-branch","type":"commit"}},
				{"ref":"refs/tags/5.0.1-rc.100-other","object":{"sha":"invalid","type":"commit"}}
			]`)
		case "/git/matching-refs/tags/v5.0.1-rc.":
			fmt.Fprint(w, `[{"ref":"refs/tags/v5.0.1-rc.10","object":{"sha":"annotation","type":"tag"}}]`)
		case "/git/tags/annotation":
			fmt.Fprint(w, `{"object":{"sha":"candidate","type":"commit"}}`)
		case "/compare/wrong-branch...head":
			fmt.Fprint(w, `{"status":"diverged"}`)
		case "/compare/candidate...head":
			fmt.Fprint(w, `{"status":"ahead","ahead_by":2,"commits":[{"sha":"first"},{"sha":"second"}]}`)
		case "/commits/first/pulls":
			if !published {
				t.Error("post-RC PR lookup delayed the first verified RC")
			}
			fmt.Fprint(w, `[
				{"number":41,"title":"Backport one","base":{"ref":"release-5.0"},"merged_at":"2026-11-17T12:00:00Z"},
				{"number":42,"base":{"ref":"master"},"merged_at":"2026-11-17T12:00:00Z"},
				{"number":43,"base":{"ref":"release-5.0"},"merged_at":null}
			]`)
		case "/commits/second/pulls":
			fmt.Fprint(w, `[
				{"number":41,"title":"Backport one","base":{"ref":"release-5.0"},"merged_at":"2026-11-17T12:00:00Z"},
				{"number":44,"title":"Backport two","base":{"ref":"release-5.0"},"merged_at":"2026-11-17T12:00:00Z"}
			]`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})}})
	result, err := client.Discover(t.Context(), app.DiscoveryRequest{
		Source:  release.Source{Name: "acs", Repo: "stackrox/stackrox", Branch: "release-5.0"},
		Version: "5.0.1", Scope: app.ReadRC,
		CandidateProgress: func(head string, rc release.Candidate) {
			published = head == "head" && rc.SHA == "candidate" && rc.Evidence.Fresh() && rc.PRsPending && rc.Ahead == 2
		},
	})
	rc := result.Candidate
	if err != nil || !rc.Evidence.Fresh() || rc.Target.Tag != "v5.0.1-rc.10" || rc.Number != 10 || rc.SHA != "candidate" || rc.Ahead != 2 || rc.PRError != "" {
		t.Fatalf("unexpected current candidate: %+v, err=%v", rc, err)
	}
	if len(rc.PRs) != 2 || rc.PRs[0].URL != "https://github.com/stackrox/stackrox/pull/41" || rc.PRs[1].Number != 44 {
		t.Fatalf("unexpected post-RC PRs: %+v", rc.PRs)
	}
}

func TestDiscoverCandidateResumesPRLookupsAcrossPolls(t *testing.T) {
	const total = 101
	var lookups int
	client := New("https://api.github.test", "", &http.Client{Transport: fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox"); {
		case path == "/branches/release-5.0":
			fmt.Fprint(w, `{"commit":{"sha":"head"}}`)
		case path == "/git/matching-refs/tags/5.0.1-rc.":
			fmt.Fprint(w, `[{"ref":"refs/tags/5.0.1-rc.2","object":{"sha":"candidate","type":"commit"}}]`)
		case path == "/git/matching-refs/tags/v5.0.1-rc.":
			fmt.Fprint(w, `[]`)
		case path == "/compare/candidate...head":
			type commit struct{ SHA string }
			var commits []commit
			switch r.URL.Query().Get("page") {
			case "1":
				for i := range 100 {
					commits = append(commits, commit{SHA: strconv.Itoa(i)})
				}
			case "2":
				commits = append(commits, commit{SHA: "last"})
			default:
				t.Errorf("unexpected comparison page: %s", r.URL)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "ahead_by": total, "commits": commits})
		case path == "/commits/last/pulls":
			lookups++
			fmt.Fprint(w, `[{"number":44,"base":{"ref":"release-5.0"},"merged_at":"2026-11-17T12:00:00Z"}]`)
		case strings.HasPrefix(path, "/commits/") && strings.HasSuffix(path, "/pulls"):
			lookups++
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})}})
	req := app.DiscoveryRequest{
		Source:  release.Source{Name: "acs", Repo: "stackrox/stackrox", Branch: "release-5.0"},
		Version: "5.0.1", Scope: app.ReadRC,
	}
	for i := range 10 {
		result, err := client.Discover(t.Context(), req)
		rc := result.Candidate
		if err != nil || !rc.Evidence.Fresh() || rc.Ahead != total || (i == 0 && rc.PRError == "") {
			t.Fatalf("invalid observation in poll %d: %+v, err=%v", i, rc, err)
		}
		if rc.PRError == "" {
			if len(rc.PRs) != 1 || rc.PRs[0].Number != 44 || lookups != total {
				t.Fatalf("incomplete or repeated PR reads: prs=%+v lookups=%d", rc.PRs, lookups)
			}
			return
		}
	}
	t.Fatal("PR lookup did not finish after multiple polls")
}
