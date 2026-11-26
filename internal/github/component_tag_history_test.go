package github

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestReadComponentTagPRs(t *testing.T) {
	for name, tc := range map[string]struct {
		comparison string
		numbers    []int
		wantError  bool
	}{
		"only PRs merged into the branch within the range": {
			comparison: `{"status":"ahead","ahead_by":2,"commits":[{"sha":"first"},{"sha":"selected"}]}`,
			numbers:    []int{11, 14},
		},
		"comparison is incomplete": {
			comparison: `{"status":"ahead","ahead_by":2,"commits":[{"sha":"first"}]}`,
			wantError:  true,
		},
		"tags diverged": {comparison: `{"status":"diverged","ahead_by":2}`, wantError: true},
	} {
		t.Run(name, func(t *testing.T) {
			client := New("https://api.github.test", "", &http.Client{Transport: fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch strings.TrimPrefix(r.URL.Path, "/repos/acme/collector") {
				case "/compare/previous...selected":
					fmt.Fprint(w, tc.comparison)
				case "/commits/first/pulls":
					fmt.Fprint(w, `[
						{"number":11,"merge_commit_sha":"first","base":{"ref":"release-3.24"},"merged_at":"2026-11-17T12:00:00Z"},
						{"number":12,"merge_commit_sha":"later","base":{"ref":"release-3.24"},"merged_at":"2026-11-18T12:00:00Z"},
						{"number":13,"merge_commit_sha":"first","base":{"ref":"release-3.25"},"merged_at":"2026-11-17T12:00:00Z"}
					]`)
				case "/commits/selected/pulls":
					fmt.Fprint(w, `[
						{"number":11,"merge_commit_sha":"first","base":{"ref":"release-3.24"},"merged_at":"2026-11-17T12:00:00Z"},
						{"number":14,"merge_commit_sha":"selected","base":{"ref":"release-3.24"},"merged_at":"2026-11-17T12:00:00Z"},
						{"number":15,"merge_commit_sha":"selected","base":{"ref":"release-3.24"}}
					]`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			})}})
			source := release.Source{Name: "collector", Repo: "acme/collector", Branch: "release-3.24", Stream: "3.24"}
			prs, err := client.ReadComponentTagPRs(t.Context(), source, "previous", "selected")
			var numbers []int
			for _, pr := range prs {
				numbers = append(numbers, pr.Number)
			}
			if (err != nil) != tc.wantError || !slices.Equal(numbers, tc.numbers) {
				t.Fatalf("PR numbers = %v, error = %v", numbers, err)
			}
		})
	}
}
