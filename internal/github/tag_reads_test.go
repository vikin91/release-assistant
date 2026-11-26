package github

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestReadComponentTagNamesUsesOnlyStreamRefs(t *testing.T) {
	var paths []string
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("If-None-Match") == `"tags"` && r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"tags"`)
		switch r.URL.Path {
		case "/repos/acme/fact/git/matching-refs/tags/0.4.":
			_, _ = fmt.Fprint(w, `[{"ref":"refs/tags/0.4.2"},{"ref":"refs/tags/0.4.11"},{"ref":"refs/tags/0.4.12-rc.1"},{"ref":"refs/tags/0.40.13"}]`)
		case "/repos/acme/fact/git/matching-refs/tags/v0.4.":
			_, _ = fmt.Fprint(w, `[{"ref":"refs/tags/v0.4.10"}]`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	client := New(server.URL, "fixture-token", server.Client())
	source := release.Source{Name: "fact", Repo: "acme/fact", Stream: "0.4"}
	for range 2 {
		got, err := client.ReadComponentTagNames(t.Context(), source)
		if want := []string{"0.4.11", "v0.4.10", "0.4.2"}; err != nil || !slices.Equal(got, want) {
			t.Fatalf("names=%v want=%v error=%v", got, want, err)
		}
	}
	if len(paths) != 4 || paths[0] != paths[2] || paths[1] != paths[3] {
		t.Fatalf("expected two stream lookups per click, got %v", paths)
	}
}

func TestVerifyRecentTagsReusesExactSHAsAndRevalidatesRefs(t *testing.T) {
	head, commit, object := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	counts := make(map[string]int)
	var validated int
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/acme/fact")
		counts[path]++
		switch path {
		case "/branches/release-0.4", "/git/ref/tags/0.4.11":
			if r.Header.Get("If-None-Match") == `"ref"` {
				validated++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `"ref"`)
			if path == "/branches/release-0.4" {
				_, _ = fmt.Fprintf(w, `{"commit":{"sha":%q}}`, head)
			} else {
				_, _ = fmt.Fprintf(w, `{"object":{"sha":%q,"type":"tag"}}`, object)
			}
		case "/git/tags/" + object:
			_, _ = fmt.Fprintf(w, `{"object":{"sha":%q,"type":"commit"}}`, commit)
		case "/commits/" + commit:
			_, _ = fmt.Fprint(w, `{"commit":{"committer":{"date":"2026-01-01T00:00:00Z"}}}`)
		case "/compare/" + commit + "..." + head:
			_, _ = fmt.Fprint(w, `{"status":"ahead"}`)
		case "/git/ref/tags/0.4.10":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	client := New(server.URL, "fixture-token", server.Client())
	request := app.DiscoveryRequest{
		Source: release.Source{Name: "fact", Repo: "acme/fact", Branch: "release-0.4", Stream: "0.4"},
		Scope:  app.ReadTags, TagCandidates: []string{"0.4.11", "0.4.10"},
	}
	for range 2 {
		repository, err := client.Discover(t.Context(), request)
		if err == nil || len(repository.Tags) != 2 {
			t.Fatalf("expected one failed candidate, got %+v error=%v", repository.Tags, err)
		}
		tag, failed := repository.Tags[0], repository.Tags[1]
		if tag.SHA != commit || tag.Belongs != release.Yes || !tag.Evidence.Fresh() ||
			failed.Name != "0.4.10" || failed.Belongs != "" || failed.Evidence.Fresh() {
			t.Fatalf("unexpected verification results: %+v", repository.Tags)
		}
	}
	if validated != 2 || counts["/git/tags/"+object] != 1 || counts["/commits/"+commit] != 1 ||
		counts["/compare/"+commit+"..."+head] != 1 || counts["/git/ref/tags/0.4.10"] != 2 {
		t.Fatalf("conditional reads=%d and calls=%v", validated, counts)
	}
}
