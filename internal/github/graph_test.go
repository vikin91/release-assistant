package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestGraphAncestors(t *testing.T) {
	for name, tc := range map[string]struct {
		parents  map[string][]string
		ancestor string
		want     release.Truth
	}{
		"should follow the second merge parent":                   {map[string][]string{"tag": {"left", "right"}, "left": {}, "right": {"merge"}, "merge": {}}, "merge", release.Yes},
		"should reject an unrelated commit with complete history": {map[string][]string{"tag": {"root"}, "root": {}}, "other", release.No},
		"should leave a missing parent unknown":                   {map[string][]string{"tag": {"missing"}}, "other", ""},
		"should retain a proven path with incomplete history":     {map[string][]string{"tag": {"merge", "missing"}, "merge": {}}, "merge", release.Yes},
		"should include the tag commit itself":                    {map[string][]string{"tag": {}}, "tag", release.Yes},
	} {
		t.Run(name, func(t *testing.T) {
			graph := newCommitGraph()
			graph.parents = tc.parents
			reachable, complete := graph.ancestors("tag")
			if got := graphContains(reachable, complete, tc.ancestor); got != tc.want {
				t.Fatalf("contains = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestGraphLoadFollowsWindowBoundariesAndSharesParents(t *testing.T) {
	reads := map[string]int{}
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		root := r.URL.Query().Get("sha")
		reads[root]++
		var window []map[string]any
		switch root {
		case "tag":
			window = []map[string]any{{"sha": "tag", "parents": []map[string]string{{"sha": "left"}, {"sha": "right"}}}}
		case "left", "right":
			window = []map[string]any{{"sha": root, "parents": []map[string]string{{"sha": "shared"}}}}
		case "shared":
			window = []map[string]any{{"sha": "shared", "parents": []any{}}}
		default:
			t.Fatalf("unexpected graph root: %s", root)
		}
		_ = json.NewEncoder(w).Encode(window)
	}))
	graph := newCommitGraph()
	client := New(srv.URL, "", srv.Client())
	if err := graph.load(t.Context(), client, "acme/fact", []string{"tag", "right"}); err != nil {
		t.Fatal(err)
	}
	if err := graph.load(t.Context(), client, "acme/fact", []string{"tag"}); err != nil {
		t.Fatal(err)
	}
	_, complete := graph.ancestors("tag")
	if !complete || len(reads) != 4 {
		t.Fatalf("complete=%t reads=%v", complete, reads)
	}
	for root, n := range reads {
		if n != 1 {
			t.Errorf("root %s read %d times", root, n)
		}
	}
}

func TestGraphLoadPreservesFailuresAndLimits(t *testing.T) {
	for name, tc := range map[string]struct {
		response string
		status   int
		want     string
	}{
		"should reject omitted parent metadata": {`[{"sha":"tag"}]`, 200, "malformed commit metadata"},
		"should reject an omitted root":         {`[]`, 200, "omitted the requested commit"},
		"should reject an empty parent SHA":     {`[{"sha":"tag","parents":[{"sha":""}]}]`, 200, "malformed parent metadata"},
		"should retain access errors":           {`{}`, 403, "HTTP 403"},
	} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			graph := newCommitGraph()
			client := New(srv.URL, "", srv.Client())
			for range 2 {
				err := graph.load(t.Context(), client, "acme/fact", []string{"tag"})
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %q", err, tc.want)
				}
			}
			reachable, complete := graph.ancestors("tag")
			if reads != 1 || graphContains(reachable, complete, "other") != "" {
				t.Fatalf("reads=%d complete=%t", reads, complete)
			}
		})
	}
	t.Run("should report the acquisition limit", func(t *testing.T) {
		reads := 0
		srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reads++
			_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": r.URL.Query().Get("sha"), "parents": []map[string]string{{"sha": fmt.Sprintf("parent-%d", reads)}}}})
		}))
		graph := newCommitGraph()
		err := graph.load(t.Context(), New(srv.URL, "", srv.Client()), "acme/fact", []string{"tag"})
		if err == nil || !strings.Contains(err.Error(), "limit reached") || reads != maxPages {
			t.Fatalf("reads=%d error=%v", reads, err)
		}
		reachable, complete := graph.ancestors("tag")
		if graphContains(reachable, complete, "other") != "" || graphContains(reachable, complete, "parent-1") != release.Yes {
			t.Fatal("truncated graph lost the distinction between proven and unknown ancestry")
		}
	})
}

func TestDiscoverCoveragePreservesPartialEvidence(t *testing.T) {
	for name, incomplete := range map[string]bool{"should prove absence with complete history": false, "should keep absence unknown after a read failure": true} {
		t.Run(name, func(t *testing.T) {
			client, request, _ := discoveryFixture(t, 1, 2, 0)
			handler := client.http.Transport.(fakeTransport).handler
			client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/fact/commits" {
					handler.ServeHTTP(w, r)
					return
				}
				if r.URL.Query().Get("sha") != "tag-1" {
					http.Error(w, "private body", http.StatusForbidden)
					return
				}
				parents := []map[string]string{{"sha": "merge-1001"}}
				if incomplete {
					parents = append(parents, map[string]string{"sha": "missing"})
				}
				_ = json.NewEncoder(w).Encode([]map[string]any{{"sha": "tag-1", "parents": parents}, {"sha": "merge-1001", "parents": []any{}}})
			})}
			repo, err := client.Discover(t.Context(), request)
			if (err != nil) != incomplete {
				t.Fatalf("error=%v", err)
			}
			tag := repo.Tags[0]
			want := release.No
			if incomplete {
				want = ""
			}
			if tag.Contains["merge-1001"] != release.Yes || tag.Contains["merge-1002"] != want || tag.Evidence.Fresh() == incomplete {
				t.Fatalf("unexpected coverage: %+v", tag)
			}
			if tag.Evidence.At.IsZero() || tag.Evidence.URL != "https://github.com/acme/fact/tree/3.25.1" {
				t.Fatalf("lost provenance: %+v", tag.Evidence)
			}
			if incomplete && (tag.Evidence.Error != "merge coverage unavailable" || !strings.Contains(err.Error(), "HTTP 403") || strings.Contains(err.Error(), "private body")) {
				t.Fatalf("lost safe failure evidence: tag=%+v error=%v", tag, err)
			}
		})
	}
}
