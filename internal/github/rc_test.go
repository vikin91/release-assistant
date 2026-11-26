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
	prDiscovery := false
	client := New("https://api.github.test", "", &http.Client{Transport: fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox"); path {
		case "/branches/release-5.0":
			fmt.Fprint(w, `{"commit":{"sha":"head"}}`)
		case "/pulls":
			fmt.Fprint(w, `[]`)
		case "/git/matching-refs/tags/5.0.1":
			fmt.Fprint(w, `[
				{"ref":"refs/tags/5.0.1","object":{"sha":"published","type":"commit"}},
				{"ref":"refs/tags/5.0.10","object":{"sha":"other-patch","type":"commit"}},
				{"ref":"refs/tags/5.0.1-rc.9","object":{"sha":"old","type":"commit"}},
				{"ref":"refs/tags/5.0.1-rc.11","object":{"sha":"wrong-branch","type":"commit"}},
				{"ref":"refs/tags/5.0.1-rc.100-other","object":{"sha":"invalid","type":"commit"}}
			]`)
		case "/git/matching-refs/tags/v5.0.1":
			fmt.Fprint(w, `[{"ref":"refs/tags/v5.0.1-rc.10","object":{"sha":"annotation","type":"tag"}}]`)
		case "/git/tags/annotation":
			fmt.Fprint(w, `{"object":{"sha":"candidate","type":"commit"}}`)
		case "/compare/wrong-branch...head":
			fmt.Fprint(w, `{"status":"diverged"}`)
		case "/compare/candidate...head":
			fmt.Fprint(w, `{"status":"ahead","ahead_by":2,"commits":[{"sha":"first"},{"sha":"second"}]}`)
		case "/commits/first/pulls":
			if !prDiscovery {
				t.Error("tag discovery requested associated PRs")
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
	if err != nil || !published || !rc.PRsPending || !rc.Evidence.Fresh() || rc.Target.Tag != "v5.0.1-rc.10" || rc.Number != 10 || rc.SHA != "candidate" || rc.Ahead != 2 || rc.PRError != "" {
		t.Fatalf("unexpected current candidate: %+v, err=%v", rc, err)
	}
	want := map[string]string{"5.0.1": "published", "5.0.1-rc.9": "old", "5.0.1-rc.11": "wrong-branch", "v5.0.1-rc.10": "candidate"}
	if len(result.Tags) != len(want) {
		t.Fatalf("patch tags = %+v", result.Tags)
	}
	for _, tag := range result.Tags {
		if want[tag.Name] != tag.SHA || !tag.Evidence.Fresh() {
			t.Fatalf("incorrect patch tag observation: %+v", tag)
		}
	}
	prDiscovery = true
	result, err = client.Discover(t.Context(), app.DiscoveryRequest{Source: result.Source, PreviousCandidate: rc, Scope: app.ReadPRs})
	rc = result.Candidate
	if err != nil || rc.PRsPending || rc.PRError != "" || len(rc.PRs) != 2 || rc.PRs[0].URL != "https://github.com/stackrox/stackrox/pull/41" || rc.PRs[1].Number != 44 {
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
		case path == "/pulls":
			fmt.Fprint(w, `[]`)
		case path == "/git/matching-refs/tags/5.0.1":
			fmt.Fprint(w, `[{"ref":"refs/tags/5.0.1-rc.2","object":{"sha":"candidate","type":"commit"}}]`)
		case path == "/git/matching-refs/tags/v5.0.1":
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
		case path == "/milestones":
			if r.URL.Query().Get("state") != "open" || r.Method != http.MethodGet {
				t.Errorf("unexpected milestone request: %s %s", r.Method, r.URL)
			}
			fmt.Fprint(w, `[
				{"title":"5.0.1-rc.2","state":"open","html_url":"https://github.com/stackrox/stackrox/milestone/572"},
				{"title":"5.0.1-rc.3","state":"closed","html_url":"https://github.com/stackrox/stackrox/milestone/573"},
				{"title":"5.0.1-rc.3","state":"open","html_url":"https://github.com/stackrox/stackrox/milestone/574"}
			]`)
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
		Version: "5.0.1", Scope: app.ReadRC | app.ReadPRs,
	}
	for i := range 10 {
		result, err := client.Discover(t.Context(), req)
		rc := result.Candidate
		if err != nil || !rc.Evidence.Fresh() || rc.Ahead != total || (i == 0 && rc.PRError == "") {
			t.Fatalf("invalid observation in poll %d: %+v, err=%v", i, rc, err)
		}
		if !rc.NextMilestone.Fresh() || rc.NextMilestone.URL != "https://github.com/stackrox/stackrox/milestone/574" {
			t.Fatalf("unexpected milestone: %+v", rc.NextMilestone)
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
