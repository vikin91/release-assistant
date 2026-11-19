package github

import (
	"net/http"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestEffectiveRequiredChecks(t *testing.T) {
	const (
		classic = `{"contexts":["classic"]}`
		rules   = `[{"type":"pull_request"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":15368}]}}]`
	)
	for name, tc := range map[string]struct {
		classic, rules, enforcement string
		classicStatus, rulesStatus  int
		build, legacy               release.Truth
		health                      release.Truth
		incomplete                  bool
	}{
		"should use active rules when classic enforcement is off":           {classicStatus: 404, enforcement: "off", rules: rules, build: release.Yes, legacy: release.No, health: release.Yes},
		"should combine classic and ruleset checks":                         {classic: classic, rules: rules, build: release.Yes, legacy: release.Yes, health: release.Yes},
		"should preserve classic checks when there are no active rules":     {classic: classic, rules: "[]", build: release.No, legacy: release.Yes, health: release.Yes},
		"should verify a branch with no required checks":                    {classicStatus: 404, enforcement: "off", rules: "[]", build: release.No, legacy: release.No, health: release.Yes},
		"should keep ruleset requirements when classic settings are hidden": {classicStatus: 404, rules: rules, build: release.Yes, incomplete: true},
		"should keep classic requirements when rulesets are hidden":         {classic: classic, rulesStatus: 404, legacy: release.Yes, incomplete: true},
		"should retain uncertainty after a permission error":                {classicStatus: 403, enforcement: "off", rules: "[]", incomplete: true},
		"should retain uncertainty when ruleset parameters are missing":     {classic: classic, rules: `[{"type":"required_status_checks"}]`, legacy: release.Yes, incomplete: true},
		"should retain uncertainty when classic settings are missing":       {classic: "{}", rules: "[]", incomplete: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body string
				switch r.URL.Path {
				case "/repos/acme/acs/branches/release/protection/required_status_checks":
					if tc.classicStatus != 0 {
						w.WriteHeader(tc.classicStatus)
						return
					}
					body = tc.classic
				case "/repos/acme/acs/rules/branches/release":
					if tc.rulesStatus != 0 {
						w.WriteHeader(tc.rulesStatus)
						return
					}
					body = tc.rules
				case "/repos/acme/acs/commits/head/check-runs":
					body = `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success","app":{"id":15368}}]}`
				case "/repos/acme/acs/commits/head/statuses":
					body = `[{"context":"classic","state":"success"}]`
				default:
					t.Fatalf("unexpected request %s", r.URL)
				}
				writeCheckFixture(t, w, body)
			}))
			pr := release.PR{SHA: "head", Evidence: release.Evidence{At: time.Now(), Known: true}}
			New(srv.URL, "", srv.Client()).readChecks(t.Context(), release.Source{Repo: "acme/acs", Branch: "release"}, &pr, tc.enforcement)
			if !pr.CheckResultsEvidence.Fresh() || pr.ChecksEvidence.Fresh() == tc.incomplete || pr.ChecksStatus() != tc.health || len(pr.Checks) != 2 || pr.Checks[0].Required != tc.build || pr.Checks[1].Required != tc.legacy {
				t.Fatalf("effective checks = %+v", pr)
			}
		})
	}
}

func TestAppBoundRequiredCheckWaitsForExpectedApp(t *testing.T) {
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body string
		switch r.URL.Path {
		case "/repos/acme/acs/branches/release/protection/required_status_checks":
			body = `{"contexts":["build"],"checks":[{"context":"build","app_id":15368}]}`
		case "/repos/acme/acs/rules/branches/release":
			body = `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":15368}]}}]`
		case "/repos/acme/acs/commits/head/check-runs":
			body = `{"total_count":1,"check_runs":[{"name":"build","status":"completed","conclusion":"success","app":{"id":444}}]}`
		case "/repos/acme/acs/commits/head/statuses":
			body = `[{"context":"build","state":"success"}]`
		default:
			t.Fatalf("unexpected request %s", r.URL)
		}
		writeCheckFixture(t, w, body)
	}))
	pr := release.PR{SHA: "head", Evidence: release.Evidence{At: time.Now(), Known: true}}
	New(srv.URL, "", srv.Client()).readChecks(t.Context(), release.Source{Repo: "acme/acs", Branch: "release"}, &pr, "")
	if !pr.ChecksEvidence.Fresh() || pr.ChecksStatus() != "" || len(pr.Checks) != 3 || pr.Checks[0].Required != release.No || pr.Checks[1].Required != release.No || pr.Checks[2].Required != release.Yes || pr.Checks[2].Status != "pending" {
		t.Fatalf("app-bound requirement = %+v", pr)
	}
}
