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

func discoveryFixture(t testing.TB, tags, merged, open int) (*Client, app.DiscoveryRequest, map[string]int) {
	t.Helper()
	counts := map[string]int{}
	pageBounds := func(r *http.Request, n int) (int, int) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		start := min((page-1)*100, n)
		return start, min(start+100, n)
	}
	pr := func(n int, merged bool) map[string]any {
		state := "open"
		if merged {
			state = "closed"
		}
		return map[string]any{"number": n, "state": state, "merged": merged,
			"merge_commit_sha": fmt.Sprintf("merge-%d", n),
			"head":             map[string]string{"sha": fmt.Sprintf("head-%d", n)},
			"base":             map[string]string{"ref": "release"},
			"html_url":         fmt.Sprintf("https://github.com/acme/fact/pull/%d", n)}
	}
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		kind := ""
		var body any
		switch {
		case path == "/repos/acme/fact/branches/release":
			kind, body = "branch", map[string]any{"commit": map[string]string{"sha": "branch-sha"}}
		case path == "/repos/acme/fact/tags":
			kind = "tags"
			items := []map[string]any{}
			start, end := pageBounds(r, tags)
			for i := start; i < end; i++ {
				items = append(items, map[string]any{"name": fmt.Sprintf("3.25.%d", i+1), "commit": map[string]string{"sha": fmt.Sprintf("tag-%d", i+1)}})
			}
			body = items
		case strings.Contains(path, "/git/ref/tags/"):
			kind = "tag_ref"
			n, _ := strconv.Atoi(strings.TrimPrefix(path, "/repos/acme/fact/git/ref/tags/3.25."))
			body = map[string]any{"object": map[string]string{"type": "commit", "sha": fmt.Sprintf("tag-%d", n)}}
		case path == "/repos/acme/fact/pulls":
			kind = "open_list"
			items := []map[string]any{}
			start, end := pageBounds(r, open)
			for i := start; i < end; i++ {
				items = append(items, pr(i+1, false))
			}
			body = items
		case path == "/search/issues":
			kind = "merged_search"
			items := []map[string]int{}
			start, end := pageBounds(r, merged)
			for i := start; i < end; i++ {
				items = append(items, map[string]int{"number": i + 1001})
			}
			body = map[string]any{"total_count": merged, "incomplete_results": false, "items": items}
		case strings.Contains(path, "/pulls/"):
			kind = "pr_detail"
			n, _ := strconv.Atoi(strings.TrimPrefix(path, "/repos/acme/fact/pulls/"))
			body = pr(n, true)
		case strings.HasSuffix(path, "/check-runs"):
			kind, body = "checks", map[string]any{"total_count": 0, "check_runs": []any{}}
		case strings.HasSuffix(path, "/statuses"):
			kind, body = "statuses", []any{}
		case strings.Contains(path, "/rules/branches/"):
			kind, body = "rules", []any{}
		case strings.HasSuffix(path, "/required_status_checks"):
			kind, body = "protection", map[string]any{"contexts": []string{}}
		case strings.Contains(path, "/compare/"):
			kind = "tag_ancestry"
			body = map[string]string{"status": "ahead"}
		case strings.Contains(path, "/commits/"):
			kind, body = "commit_metadata", map[string]any{"commit": map[string]any{"committer": map[string]string{"date": "2026-01-01T00:00:00Z"}}}
		default:
			t.Errorf("unexpected request: %s", r.URL)
			http.NotFound(w, r)
			return
		}
		counts[kind]++
		_ = json.NewEncoder(w).Encode(body)
	}))
	return New(srv.URL, "", srv.Client()), app.DiscoveryRequest{Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release", Stream: "3.25"}}, counts
}

func BenchmarkDiscover(b *testing.B) {
	for _, tags := range []int{10, 100} {
		b.Run(fmt.Sprintf("tags=%d/merged=500/open=20", tags), func(b *testing.B) {
			client, request, counts := discoveryFixture(b, tags, 500, 20)
			for b.Loop() {
				repo, err := client.discover(b.Context(), request, 1000)
				if err != nil {
					b.Fatal(err)
				}
				if len(repo.Tags) != tags || len(repo.PRs) != 520 {
					b.Fatal("incomplete discovery")
				}
			}
			total := 0
			for _, n := range counts {
				total += n
			}
			b.ReportMetric(float64(total)/float64(b.N), "requests/op")
		})
	}
}

func TestDiscoverVerifiesTagAncestryAndRetainsMergedPRs(t *testing.T) {
	for _, tags := range []int{10, 100} {
		t.Run(fmt.Sprintf("tags=%d", tags), func(t *testing.T) {
			client, request, counts := discoveryFixture(t, tags, 500, 20)
			for cycle := 1; cycle <= 2; cycle++ {
				clear(counts)
				repo, err := client.discover(t.Context(), request, 1000)
				if err != nil {
					t.Fatal(err)
				}
				for _, tag := range repo.Tags {
					if !tag.Evidence.Fresh() {
						t.Fatalf("partial tag: %+v", tag)
					}

				}
				total := 0
				for _, n := range counts {
					total += n
				}
				if counts["tag_ancestry"] != tags || len(repo.PRs) != 520 || total > 1000 {
					t.Fatalf("excessive requests: %v", counts)
				}
				t.Logf("cycle=%d total=%d tag comparisons=%d protection=%d", cycle, total, counts["tag_ancestry"], counts["protection"])
			}
		})
	}
}

func BenchmarkDiscoverBudget(b *testing.B) {
	for name, size := range map[string]struct {
		tags, merged, open int
		incomplete         bool
	}{
		"small_complete": {10, 10, 3, false},
		"large_partial":  {100, 500, 20, true},
	} {
		b.Run(name, func(b *testing.B) {
			client, request, counts := discoveryFixture(b, size.tags, size.merged, size.open)
			for b.Loop() {
				repo, err := client.Discover(b.Context(), request)
				if (err != nil) != size.incomplete || repo.Evidence.Fresh() == size.incomplete {
					b.Fatalf("error=%v evidence=%+v", err, repo.Evidence)
				}
			}
			total := 0
			for _, n := range counts {
				total += n
			}
			b.ReportMetric(float64(total)/float64(b.N), "requests/op")
		})
	}
}

func TestDiscoverPublishesTagsBeforePRAndCheckReads(t *testing.T) {
	client, request, counts := discoveryFixture(t, 100, 500, 20)
	request.Scope = app.ReadTags
	firstRequests := 0
	request.Progress = func(repo release.Repository) {
		if firstRequests != 0 {
			return
		}
		for _, n := range counts {
			firstRequests += n
		}
		if len(repo.Tags) != 1 || !repo.Tags[0].Evidence.Fresh() || repo.Tags[0].Belongs != release.Yes || repo.Evidence.Fresh() {
			t.Fatalf("invalid partial tag inventory: %+v", repo)
		}
	}
	repo, err := client.Discover(t.Context(), request)
	if firstRequests != 5 || len(repo.Tags) == 0 || err != nil || !repo.TagsPending {
		t.Fatalf("first choice requests=%d tags=%d error=%v", firstRequests, len(repo.Tags), err)
	}
	for _, kind := range []string{"open_list", "merged_search", "pr_detail", "checks", "statuses", "protection"} {
		if counts[kind] != 0 {
			t.Fatalf("unrelated %s calls during tag selection: %v", kind, counts)
		}
	}
	t.Logf("first selectable tag after %d requests; PR and check calls=0", firstRequests)
}

func BenchmarkTagSelectionFirstChoice(b *testing.B) {
	client, request, counts := discoveryFixture(b, 100, 500, 20)
	request.Scope = app.ReadTags
	first, total := 0, 0
	for b.Loop() {
		clear(counts)
		client.inventories.Clear()
		published := false
		request.Progress = func(repo release.Repository) {
			if published || len(repo.Tags) == 0 || !repo.Tags[0].Evidence.Fresh() {
				return
			}
			published = true
			for _, n := range counts {
				first += n
			}
		}
		repo, err := client.Discover(b.Context(), request)
		if !published || len(repo.Tags) == 0 || err != nil || !repo.TagsPending {
			b.Fatalf("unexpected tag discovery result: tags=%d error=%v", len(repo.Tags), err)
		}
		for _, n := range counts {
			total += n
		}
	}
	b.ReportMetric(float64(first)/float64(b.N), "requests/first-choice")
	b.ReportMetric(float64(total)/float64(b.N), "requests/op")
}

func TestScopedDiscoveryAvoidsUnrelatedRequests(t *testing.T) {
	t.Run("PR inventory", func(t *testing.T) {
		client, request, counts := discoveryFixture(t, 100, 500, 20)
		request.Scope = app.ReadPRs
		repo, err := client.Discover(t.Context(), request)
		total := 0
		for _, n := range counts {
			total += n
		}
		if err != nil || total != 2 || len(repo.PRs) != 20 || len(repo.Tags) != 0 {
			t.Fatalf("requests=%v tags=%d PRs=%d error=%v", counts, len(repo.Tags), len(repo.PRs), err)
		}
		for _, pr := range repo.PRs {
			if pr.ChecksEvidence.Fresh() {
				t.Fatal("unread checks were confirmed")
			}
		}
		t.Log("20 PR choices in 2 requests; tag, check, and merged-search calls=0")
	})
	t.Run("selected tag and merge", func(t *testing.T) {
		client, request, counts := discoveryFixture(t, 100, 500, 20)
		request.Scope = app.ReadTags | app.ReadTargets
		request.ConfirmedTag = "3.25.100"
		request.Tracked = []release.Target{{Repo: request.Source.Repo, PR: 1001}}
		repo, err := client.Discover(t.Context(), request)
		if err != nil || len(repo.Tags) != 1 || len(repo.PRs) != 1 || repo.Tags[0].Belongs != release.Yes || repo.PRs[0].MergeSHA != "merge-1001" || counts["tag_ref"] != 1 || counts["pr_detail"] != 1 || counts["open_list"] != 0 || counts["merged_search"] != 0 {
			t.Fatalf("counts=%v repo=%+v error=%v", counts, repo, err)
		}
	})
	t.Run("PR polling with a selected tag needs no tag or history reads", func(t *testing.T) {
		client, request, counts := discoveryFixture(t, 100, 500, 20)
		request.Scope = app.ReadPRs | app.ReadTargets
		request.ConfirmedTag = "3.25.100"
		request.Tracked = []release.Target{{Repo: request.Source.Repo, PR: 1001}}
		repo, err := client.Discover(t.Context(), request)
		if err != nil || len(repo.Tags) != 0 || len(repo.PRs) != 21 || repo.PRs[20].MergeSHA != "merge-1001" ||
			counts["branch"] != 1 || counts["open_list"] != 1 || counts["pr_detail"] != 1 || len(counts) != 3 {
			t.Fatalf("counts=%v tags=%+v PRs=%d error=%v", counts, repo.Tags, len(repo.PRs), err)
		}
	})
	t.Run("excluded target", func(t *testing.T) {
		client, request, counts := discoveryFixture(t, 100, 500, 20)
		request.Scope = app.ReadTargets | app.ReadChecks
		request.Tracked = []release.Target{{Repo: request.Source.Repo, PR: 1}}
		request.Ignored = request.Tracked
		repo, err := client.Discover(t.Context(), request)
		if err != nil || len(repo.PRs) != 0 || counts["pr_detail"] != 0 || counts["checks"] != 0 {
			t.Fatalf("counts=%v error=%v", counts, err)
		}
	})
}
