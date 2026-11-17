package github

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

func TestPreviousPinsUseExactResolvedReleaseCommit(t *testing.T) {
	for name, prefix := range map[string]string{"final tag": "", "v-prefixed tag": "v"} {
		t.Run(name, func(t *testing.T) {
			var refs []string
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/git/ref/tags/"):
					refs = append(refs, r.URL.Path)
					if !strings.HasSuffix(r.URL.Path, "/"+prefix+"4.11.3") {
						http.NotFound(w, r)
						return
					}
					_, _ = w.Write([]byte(`{"object":{"type":"tag","sha":"annotated"}}`))
				case strings.HasSuffix(r.URL.Path, "/git/tags/annotated"):
					_, _ = w.Write([]byte(`{"object":{"type":"commit","sha":"previous-commit"}}`))
				case strings.Contains(r.URL.Path, "/contents/"):
					if r.URL.Query().Get("ref") != "previous-commit" {
						t.Fatalf("baseline did not use resolved commit: %s", r.URL)
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("baseline"))})
				default:
					t.Fatalf("unbounded or unrelated baseline lookup: %s", r.URL)
				}
			}))
			c := New(srv.URL, "", srv.Client())
			pins, published := c.previousPins(t.Context(), "acme/acs", "4.11.4")
			if !pins.Evidence.Fresh() || published != release.Yes || len(pins.Versions) != 3 || pins.Versions["scanner"] != "baseline" || !strings.HasSuffix(pins.Evidence.URL, "/previous-commit") {
				t.Fatalf("invalid baseline: %+v published=%s", pins, published)
			}
			wantRefs := 1
			if prefix != "" {
				wantRefs++
			}
			if len(refs) != wantRefs {
				t.Fatalf("unexpected tag lookups: %v", refs)
			}
		})
	}
}

func TestOptionalPreviousPinsFailurePreservesCurrentObservations(t *testing.T) {
	for name, status := range map[string]int{"missing prior tag": http.StatusNotFound, "rate limited prior tag": http.StatusTooManyRequests} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/branches/"):
					_, _ = w.Write([]byte(`{"commit":{"sha":"current"}}`))
				case strings.HasSuffix(r.URL.Path, "/pulls"):
					_, _ = w.Write([]byte(`[{"number":1,"state":"open","head":{"sha":"pr-head"},"base":{"ref":"release"}}]`))
				case strings.Contains(r.URL.Path, "/contents/"):
					_ = json.NewEncoder(w).Encode(map[string]string{"encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte("current-pin"))})
				case strings.Contains(r.URL.Path, "/git/ref/tags/"):
					http.Error(w, "private body", status)
				default:
					t.Fatalf("unexpected request: %s", r.URL)
				}
			}))
			c := New(srv.URL, "", srv.Client())
			repo, err := c.Discover(t.Context(), app.DiscoveryRequest{Source: release.Source{Name: "acs", Repo: "acme/acs", Branch: "release"}, Version: "5.0.1", Scope: app.ReadPRs | app.ReadPins | app.ReadPreviousPins})
			if err != nil || !repo.Evidence.Fresh() || !repo.Pins.Evidence.Fresh() || len(repo.PRs) != 1 || !repo.PRs[0].Evidence.Fresh() {
				t.Fatalf("optional baseline invalidated current work: err=%v repo=%+v", err, repo)
			}
			if repo.PreviousPins.Evidence.Fresh() || repo.PreviousPins.Evidence.Error == "" || repo.PreviousPublished != "" {
				t.Fatalf("missing baseline was treated as known: %+v", repo.PreviousPins)
			}
		})
	}
}

func TestScopedPRInventoryOnlyReadsSelectedChecks(t *testing.T) {
	client, request, counts := discoveryFixture(t, 100, 500, 20)
	request.Scope = app.ReadPRs | app.ReadTargets | app.ReadChecks
	request.Tracked = []release.Target{{Repo: request.Source.Repo, PR: 1}}
	repo, err := client.Discover(t.Context(), request)
	if err != nil || len(repo.PRs) != 20 || counts["checks"] != 1 || counts["statuses"] != 1 || counts["tag_ref"] != 0 {
		t.Fatalf("undecided PRs consumed detailed reads: err=%v counts=%v", err, counts)
	}
	for _, pr := range repo.PRs {
		if pr.ChecksEvidence.Fresh() != (pr.Target.PR == 1) {
			t.Fatalf("wrong check scope for %+v", pr)
		}
	}
}
