package github

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestDiscoverDeduplicatesOnlyWithinOneCycle(t *testing.T) {
	client, request, counts := discoveryFixture(t, 1, 0, 3)
	handler := client.http.Transport.(fakeTransport).handler
	protectionReads := 0
	requiredName := "first-required-check"
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/required_status_checks") {
			protectionReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"contexts": []string{requiredName}})
			return
		}
		handler.ServeHTTP(w, r)
	})}
	for cycle := 1; cycle <= 2; cycle++ {
		clear(counts)
		repo, err := client.Discover(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if protectionReads != cycle || counts["commit_metadata"] != 1 || counts["checks"] != 3 || counts["statuses"] != 3 {
			t.Fatalf("protection=%d cycle=%d requests=%v", protectionReads, cycle, counts)
		}
		for _, pr := range repo.PRs {
			if !pr.ChecksEvidence.Fresh() || len(pr.Checks) != 1 || pr.Checks[0].Name != requiredName || pr.Checks[0].Required != release.Yes {
				t.Fatalf("stale required-check settings: %+v", pr)
			}
		}
		requiredName = "second-required-check"
	}
}

func TestDiscoverSharesRequiredCheckFailuresWithoutConfirmingHealth(t *testing.T) {
	client, request, _ := discoveryFixture(t, 1, 0, 3)
	handler := client.http.Transport.(fakeTransport).handler
	protectionReads := 0
	client.http.Transport = fakeTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/required_status_checks") {
			protectionReads++
			http.Error(w, "private response", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})}
	for cycle := 1; cycle <= 2; cycle++ {
		repo, _ := client.Discover(t.Context(), request)
		if protectionReads != cycle {
			t.Fatalf("protection reads=%d, want %d", protectionReads, cycle)
		}
		for _, pr := range repo.PRs {
			if pr.ChecksStatus() != "" || pr.ChecksEvidence.Fresh() || !strings.Contains(pr.ChecksEvidence.Error, "HTTP 403") || strings.Contains(pr.ChecksEvidence.Error, "private response") {
				t.Fatalf("failed protection read confirmed health: %+v", pr)
			}
		}
	}
}
