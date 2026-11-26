package github

import (
	"archive/zip"
	"bytes"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"release-assistant/internal/release"
)

func TestReadRCVulnerabilitiesUsesExactTagAndLatestJob(t *testing.T) {
	const tag = "4.10.12-rc.2"
	var downloads atomic.Int32
	archive := scanZip(t, `{"result":{"vulnerabilities":[{"cveId":"CVE-2026-99","cveSeverity":"IMPORTANT","cveCVSS":7.8,"componentName":"sqlite-libs","componentFixedVersion":"2"}]}}`)
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/repos/stackrox/stackrox") {
		case scanWorkflowPath:
			if r.URL.Query().Get("head_sha") != "candidate" {
				t.Errorf("unexpected SHA query: %s", r.URL)
			}
			writeCheckFixture(t, w, `{"workflow_runs":[
    {"id":40,"head_sha":"candidate","head_branch":"release-4.10","event":"push"},
    {"id":30,"head_sha":"wrong","head_branch":"4.10.12-rc.2","event":"push"},
    {"id":20,"head_sha":"candidate","head_branch":"4.10.12-rc.2","event":"workflow_dispatch"},
    {"id":10,"head_sha":"candidate","head_branch":"4.10.12-rc.2","event":"push","status":"in_progress"}]}`)
		case "/actions/runs/10/jobs":
			writeCheckFixture(t, w, `{"total_count":5,"jobs":[
    {"id":1,"name":"Konflux: release-main:4.10.12-rc.2","status":"completed"},
    {"id":2,"name":"Konflux: release-main:4.10.12-rc.2","status":"in_progress","started_at":"2026-11-26T10:00:00Z"},
    {"id":3,"name":"Konflux: release-scanner-v4:4.10.12-rc.2","status":"completed"},
    {"id":4,"name":"Konflux: release-roxctl:4.10.12-rc.2","status":"completed"},
    {"id":5,"name":"Upstream: main:4.10.12-rc.2","status":"completed"}]}`)
		case "/actions/runs/10/artifacts":
			writeCheckFixture(t, w, `{"total_count":2,"artifacts":[
    {"id":6,"name":"konflux_quay_io_rhacs-eng_release-main_4.10.12-rc.2.json","created_at":"2026-11-26T09:00:00Z"},
    {"id":7,"name":"konflux_quay_io_rhacs-eng_release-scanner-v4_4.10.12-rc.2.json","created_at":"2026-11-26T09:00:00Z"}]}`)
		case "/actions/artifacts/7/zip":
			downloads.Add(1)
			if _, err := w.Write(archive); err != nil {
				t.Error(err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	c := New(server.URL, "", server.Client())
	rc := release.Candidate{Target: release.Target{Repo: "stackrox/stackrox", Tag: tag}, SHA: "candidate"}
	validation := release.Validation{SHA: rc.SHA, Checks: []release.ValidationCheck{
		{ID: 8, App: "red-hat-konflux", ConformaReported: true, Check: release.Check{Name: "Red Hat Konflux / acs-conforma-dev-4-10 / scanner-v4-4-10", Conclusion: "failure"}},
	}}
	review := c.ReadRCVulnerabilities(t.Context(), rc, "", release.RCVulnerabilities{}, validation)
	if review.Problem != "" || len(review.Images) != 3 || review.Complete() || !review.ConformaReported() {
		t.Fatalf("review = %+v", review)
	}
	if got := []string{review.Images[0].Status, review.Images[1].Status, review.Images[2].Status}; !slices.Equal(got, []string{"Pending", "Unavailable", "Available"}) {
		t.Fatalf("scan states = %v", got)
	}
	if findings := review.Images[2].Findings; len(findings) != 1 || !findings[0].SuggestsHold() {
		t.Fatalf("findings = %+v", findings)
	}
	c.ReadRCVulnerabilities(t.Context(), rc, "", review, validation)
	if downloads.Load() != 1 {
		t.Fatalf("downloaded the same artifact %d times", downloads.Load())
	}
}

func TestRCVulnerabilityJobReadFailureShowsCause(t *testing.T) {
	server := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/actions/runs/10/jobs") {
			if r.URL.Query().Get("page") == "1" {
				writeCheckFixture(t, w, `{"total_count":101,"jobs":[]}`)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		t.Errorf("unexpected request %s", r.URL)
		http.NotFound(w, r)
	}))
	c := New(server.URL, "", server.Client())
	rc := release.Candidate{Target: release.Target{Repo: "stackrox/stackrox", Tag: "4.10.12-rc.2"}, SHA: "candidate"}
	validation := release.Validation{SHA: rc.SHA, ScanWorkflow: release.RCScanWorkflow{ID: 10, Status: "in_progress"}}
	review := c.ReadRCVulnerabilities(t.Context(), rc, "", release.RCVulnerabilities{}, validation)
	if !strings.Contains(review.Problem, "503") || review.Status != "" {
		t.Fatalf("expected job-list read failure with HTTP status, got %+v", review)
	}
}

func scanZip(t *testing.T, body string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	file, err := archive.Create("scan-result.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err = archive.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestScanReportDistinguishesEmptyAndInvalidEvidence(t *testing.T) {
	for name, test := range map[string]struct {
		report string
		valid  bool
	}{
		"empty result":            {`{"result":{"summary":{"TOTAL-VULNERABILITIES":0},"vulnerabilities":null}}`, true},
		"missing result":          {`{"error":"scan failed"}`, false},
		"missing vulnerabilities": {`{"result":{"summary":{"TOTAL-VULNERABILITIES":3}}}`, false},
		"trailing data":           {`{"result":{"vulnerabilities":[]}} {}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseScanArchive(scanZip(t, test.report))
			if (err == nil) != test.valid {
				t.Fatalf("unexpected parse result: %v", err)
			}
		})
	}
}
