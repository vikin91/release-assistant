package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

func windowCacheFixture(tb testing.TB) (*Client, app.DiscoveryRequest, map[string]int, http.Handler) {
	tb.Helper()
	counts := make(map[string]int)
	sha := func(i int) string { return fmt.Sprintf("%040x", i) }
	windows := make(map[string][]byte)
	for root := 301; root > 0; root -= 100 {
		window := make([]map[string]any, 0, 100)
		for i := root; i > max(root-100, 0); i-- {
			parents := []map[string]string{}
			if i > 1 {
				parents = append(parents, map[string]string{"sha": sha(i - 1)})
			}
			window = append(window, map[string]any{"sha": sha(i), "parents": parents})
		}
		data, err := json.Marshal(window)
		if err != nil {
			tb.Fatal(err)
		}
		windows[sha(root)] = data
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counts[r.URL.Path]++
		switch r.URL.Path {
		case "/repos/acme/fact/branches/release":
			_, _ = fmt.Fprintf(w, `{"commit":{"sha":%q}}`, sha(302))
		case "/repos/acme/fact/git/ref/tags/3.25.1":
			_, _ = fmt.Fprintf(w, `{"object":{"type":"commit","sha":%q}}`, sha(301))
		case "/repos/acme/fact/pulls/1":
			_, _ = fmt.Fprintf(w, `{"number":1,"state":"closed","merged":true,"base":{"ref":"release"},"merge_commit_sha":%q}`, sha(1))
		case "/repos/acme/fact/commits":
			window, ok := windows[r.URL.Query().Get("sha")]
			if !ok {
				tb.Fatalf("invalid window root: %s", r.URL)
			}
			_, _ = w.Write(window)
		default:
			switch {
			case strings.Contains(r.URL.Path, "/compare/"):
				_, _ = fmt.Fprint(w, `{"status":"ahead"}`)
			case strings.HasPrefix(r.URL.Path, "/repos/acme/fact/commits/"):
				_, _ = fmt.Fprint(w, `{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`)
			default:
				tb.Errorf("unexpected request: %s", r.URL)
				http.NotFound(w, r)
			}
		}
	})
	client := New("", "fixture-token", &http.Client{Transport: githubread.New(fakeTransport{handler: handler})})
	request := app.DiscoveryRequest{
		Source:       release.Source{Name: "fact", Repo: "acme/fact", Branch: "release", Stream: "3.25"},
		ConfirmedTag: "3.25.1", Tracked: []release.Target{{Repo: "acme/fact", PR: 1}},
		Scope: app.ReadTags | app.ReadTargets | app.ReadCoverage,
	}
	return client, request, counts, handler
}

func TestCommitWindowCacheDiscovery(t *testing.T) {
	client, request, counts, _ := windowCacheFixture(t)
	var hits int
	var console bytes.Buffer
	log := app.NewLog(&console, true)
	cache := NewCommitWindowCache(func(repo string, n int) {
		if repo != request.Source.Repo {
			t.Errorf("cache hit repository = %q", repo)
		}
		hits += n
		log.CommitWindowCacheHits(repo, n)
	})
	client.WithCommitWindowCache(cache)
	discover := func(limit int) (map[string]release.Truth, int64) {
		t.Helper()
		ctx, usage := githubread.Track(t.Context(), 80)
		result, err := client.discover(ctx, request, limit)
		if err != nil || len(result.Tags) != 1 || len(result.Tags[0].Contains) != 1 {
			t.Fatalf("coverage=%+v, error=%v", result.Tags, err)
		}
		return result.Tags[0].Contains, usage.Calls()
	}

	cold, coldCalls := discover(80)
	warm, warmCalls := discover(int(coldCalls) - 4)
	if coldCalls-warmCalls != 4 || hits != 4 || !maps.Equal(cold, warm) {
		t.Fatalf("cold calls=%d, warm calls=%d, hits=%d, cold=%v, warm=%v", coldCalls, warmCalls, hits, cold, warm)
	}
	cache.Clear()
	_, clearedCalls := discover(80)
	cache.SetEnabled(false)
	_, disabledCalls := discover(80)
	_, stillDisabledCalls := discover(80)
	cache.SetEnabled(true)
	_, reenabledCalls := discover(80)
	if clearedCalls != coldCalls || disabledCalls != coldCalls || stillDisabledCalls != coldCalls || reenabledCalls != coldCalls || hits != 4 {
		t.Fatalf("cold=%d, cleared=%d, disabled=%d/%d, reenabled=%d, hits=%d", coldCalls, clearedCalls, disabledCalls, stillDisabledCalls, reenabledCalls, hits)
	}
	if counts["/repos/acme/fact/branches/release"] != 6 || counts["/repos/acme/fact/git/ref/tags/3.25.1"] != 6 {
		t.Fatalf("mutable references lost live reads: %v", counts)
	}
	records := log.Recent()
	if len(records) != 1 || records[0].Level() != "Debug" || !strings.Contains(console.String(), "Debug: GitHub discovery for acme/fact reused 4 commit history windows") {
		t.Fatalf("unexpected cache activity: %s", &console)
	}
}

func TestCommitWindowCacheSkipsUnsuitableResponses(t *testing.T) {
	const root = "0123456789abcdef0123456789abcdef01234567"
	valid := fmt.Sprintf(`[{"sha":%q,"parents":[]}]`, root)
	for name, tc := range map[string]struct {
		sha, cacheControl, body string
		wantError               bool
	}{
		"should skip no-store":     {root, "max-age=0, No-Store", valid, false},
		"should skip extra JSON":   {root, "", valid + `{}`, false},
		"should skip invalid data": {root, "", `[{"sha":"different","parents":[]}]`, true},
		"should skip symbolic ref": {"short", "", `[{"sha":"short","parents":[]}]`, false},
	} {
		t.Run(name, func(t *testing.T) {
			var calls int
			server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.Header().Set("Cache-Control", tc.cacheControl)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			cache := NewCommitWindowCache(nil)
			client := New(server.URL, "", server.Client())
			for range 2 {
				graph := newCommitGraph()
				graph.cache = cache
				err := graph.load(t.Context(), client, "acme/fact", []string{tc.sha})
				if (err != nil) != tc.wantError {
					t.Fatalf("error = %v, want error = %t", err, tc.wantError)
				}
			}
			if calls != 2 {
				t.Fatalf("made %d live calls, want 2", calls)
			}
		})
	}
}

func TestCommitWindowCacheClearDiscardsOutstandingFetches(t *testing.T) {
	cache := NewCommitWindowCache(nil)
	key := commitWindowKey{repo: "acme/fact"}
	_, epoch := cache.lookup(key)
	cache.Clear()
	cache.save(key, epoch, []graphCommit{{SHA: "discarded"}})
	if got, _ := cache.lookup(key); got != nil {
		t.Fatalf("cache retained a write from before clear: %v", got)
	}
}

func BenchmarkCommitWindowCache(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("enabled=%t", enabled), func(b *testing.B) {
			client, request, counts, handler := windowCacheFixture(b)
			client.http.Transport = fakeTransport{handler: handler}
			cache := NewCommitWindowCache(nil)
			cache.SetEnabled(enabled)
			client.WithCommitWindowCache(cache)
			if _, err := client.Discover(b.Context(), request); err != nil {
				b.Fatal(err)
			}
			clear(counts)
			for b.Loop() {
				if _, err := client.Discover(b.Context(), request); err != nil {
					b.Fatal(err)
				}
			}
			var upstream int
			for _, count := range counts {
				upstream += count
			}
			b.ReportMetric(float64(upstream)/float64(b.N), "requests/op")
		})
	}
}
