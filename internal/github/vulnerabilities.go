package github

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/release"
)

const (
	scanWorkflowPath = "/actions/workflows/scan-image-vulnerabilities.yaml/runs"
	maxScanArchive   = 1 << 20
	maxScanReport    = 8 << 20
)

type (
	scanRun struct {
		ID            int64
		SHA           string `json:"head_sha"`
		Ref           string `json:"head_branch"`
		Status, Event string
	}
	scanArtifact struct {
		ID        int64
		Name      string
		Expired   bool
		CreatedAt time.Time `json:"created_at"`
	}
)

// ReadRCVulnerabilities reads reports from the tag-push scan workflow for the exact RC.
func (c *Client) ReadRCVulnerabilities(ctx context.Context, candidate release.Candidate, descendant string, prior release.RCVulnerabilities, validation release.Validation) release.RCVulnerabilities {
	review := release.RCVulnerabilities{Target: candidate.Target, SHA: candidate.SHA, DescendantSHA: descendant}
	if !candidate.Target.Valid() || candidate.Target.Tag == "" || candidate.SHA == "" {
		return review
	}
	base := repoPath(candidate.Target.Repo)
	if descendant != "" && descendant != candidate.SHA {
		var comparison commitRangeComparison
		endpoint := base + "/compare/" + segment(candidate.SHA) + "..." + segment(descendant) + "?per_page=1"
		if err := c.get(ctx, endpoint, &comparison); err != nil || comparison.Status != "ahead" {
			return release.RCVulnerabilities{}
		}
	}
	tag, sha := candidate.Target.Tag, candidate.SHA
	var latest release.RCScanWorkflow
	if validation.SHA == sha {
		latest = validation.ScanWorkflow
	}
	if latest.ID == 0 {
		var err error
		latest, err = c.findRCScanWorkflow(ctx, base, tag, sha)
		if err != nil {
			review.Problem = "Unable to read the vulnerability scan workflow. " + requestProblem(err)
			return review
		}
	}
	if latest.ID == 0 {
		review.Status = "Waiting for the RC vulnerability scan workflow."
		return review
	}
	runPath := fmt.Sprintf("%s/actions/runs/%d", base, latest.ID)
	review.WorkflowURL = fmt.Sprintf("https://github.com/%s/actions/runs/%d", candidate.Target.Repo, latest.ID)
	type scanJob struct {
		ID           int64
		Name, Status string
		URL          string    `json:"html_url"`
		StartedAt    time.Time `json:"started_at"`
	}
	jobs := make(map[string]scanJob)
	for page := 1; page <= maxPages; page++ {
		var result struct {
			Total int `json:"total_count"`
			Jobs  []scanJob
		}
		if err := c.get(ctx, fmt.Sprintf("%s/jobs?filter=all&per_page=100&page=%d", runPath, page), &result); err != nil {
			review.Problem = "Unable to read the complete vulnerability scan job list. " + requestProblem(err)
			return review
		}
		for _, job := range result.Jobs {
			if job.ID > jobs[job.Name].ID {
				jobs[job.Name] = job
			}
		}
		if page*100 >= result.Total {
			break
		}
		if page == maxPages {
			review.Problem = fmt.Sprintf("Unable to read the complete vulnerability scan job list. GitHub reported %d jobs, exceeding the %d-job read limit.", result.Total, maxPages*100)
			return review
		}
	}
	var artifacts struct {
		Total     int `json:"total_count"`
		Artifacts []scanArtifact
	}
	if err := c.get(ctx, runPath+"/artifacts?per_page=100", &artifacts); err != nil {
		review.Problem = "Unable to read the complete vulnerability scan artifact list. " + requestProblem(err)
	} else if artifacts.Total > len(artifacts.Artifacts) {
		review.Problem = fmt.Sprintf("Unable to read the complete vulnerability scan artifact list. GitHub returned %d of %d artifacts.", len(artifacts.Artifacts), artifacts.Total)
	}
	// A partial rerun leaves earlier jobs in place. Match each artifact to the
	// latest job for its image so a rerun cannot display a superseded report.
	available := make(map[string]scanArtifact)
	for _, a := range artifacts.Artifacts {
		if !a.Expired && a.ID > available[a.Name].ID {
			available[a.Name] = a
		}
	}
	for _, job := range jobs {
		image, ok := strings.CutPrefix(job.Name, "Konflux: ")
		image, tagged := strings.CutSuffix(image, ":"+tag)
		if !ok || !tagged || !strings.HasPrefix(image, "release-") {
			continue
		}
		status := "Pending"
		if job.Status == "completed" || latest.Status == "completed" {
			status = "Unavailable"
		}
		review.Images = append(review.Images, release.ImageVulnerabilities{Image: image, Status: status, ScanURL: job.URL})
		name := scanArtifactName(image, tag)
		if artifact := available[name]; artifact.CreatedAt.Before(job.StartedAt) {
			delete(available, name)
		}
	}
	slices.SortFunc(review.Images, func(a, b release.ImageVulnerabilities) int { return strings.Compare(a.Image, b.Image) })
	if len(review.Images) == 0 && review.Problem == "" {
		if latest.Status == "completed" {
			review.Status = "No Konflux image scan reports found."
		} else {
			review.Status = "Waiting for Konflux image scan jobs."
		}
	}
	// The same commit can contain branch and tag workflows. Conforma links
	// describe the commit's developer policy checks, independently of scan results.
	if validation.SHA != sha && !prior.Target.Valid() && descendant != "" {
		checks, _ := c.readCommitCheckRuns(ctx, candidate.Target.Repo, sha)
		validation.Checks = validationChecks(checks, nil, nil, sha, "https://github.com/"+candidate.Target.Repo, "")
		validation.SHA = sha
	}
	addConformaOnlyImages(candidate, validation, prior, &review)
	var downloads sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i := range review.Images {
		img := &review.Images[i]
		id := available[scanArtifactName(img.Image, tag)].ID
		addConformaCheck(candidate, validation, prior, img)
		if id <= 0 || img.Status == "Not scheduled" {
			continue
		}
		previous := slices.IndexFunc(prior.Images, func(existing release.ImageVulnerabilities) bool {
			return existing.Image == img.Image && existing.ArtifactID == id && existing.Status == "Available"
		})
		if prior.Target == candidate.Target && prior.SHA == sha && previous >= 0 {
			img.Status, img.ArtifactID, img.Findings = "Available", id, prior.Images[previous].Findings
			continue
		}
		downloads.Go(func() {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			case <-ctx.Done():
				return
			}
			findings, err := c.downloadScanReport(ctx, fmt.Sprintf("%s/actions/artifacts/%d/zip", base, id))
			if err != nil {
				img.Status = "Unavailable"
				return
			}
			img.Status, img.ArtifactID, img.Findings = "Available", id, findings
		})
	}
	downloads.Wait()
	return review
}

func (c *Client) findRCScanWorkflow(ctx context.Context, base, tag, sha string) (release.RCScanWorkflow, error) {
	endpoint := base + scanWorkflowPath + "?event=push&head_sha=" + url.QueryEscape(sha) + "&per_page=100"
	var runs struct {
		Runs []scanRun `json:"workflow_runs"`
	}
	if err := c.get(ctx, endpoint, &runs); err != nil {
		return release.RCScanWorkflow{}, err
	}
	var latest release.RCScanWorkflow
	for _, run := range runs.Runs {
		if run.ID > latest.ID && run.SHA == sha && run.Ref == tag && run.Event == "push" {
			latest = release.RCScanWorkflow{ID: run.ID, Status: run.Status}
		}
	}
	return latest, nil
}

func scanArtifactName(image, tag string) string {
	return "konflux_quay_io_rhacs-eng_" + image + "_" + tag + ".json"
}

func conformaCheckScope(tag string) (prefix, suffix string) {
	patch, _, _ := strings.Cut(tag, "-rc.")
	major, minor, _, _ := release.Version(patch)
	line := fmt.Sprintf("%d-%d", major, minor)
	return "Red Hat Konflux / acs-conforma-dev-" + line + " / ", "-" + line
}

func addConformaOnlyImages(candidate release.Candidate, validation release.Validation, prior release.RCVulnerabilities, review *release.RCVulnerabilities) {
	prefix, suffix := conformaCheckScope(candidate.Target.Tag)
	add := func(image string) {
		if !slices.ContainsFunc(review.Images, func(row release.ImageVulnerabilities) bool { return row.Image == image }) {
			review.Images = append(review.Images, release.ImageVulnerabilities{Image: image, Status: "Not scheduled"})
		}
	}
	if validation.SHA == candidate.SHA {
		for _, check := range validation.Checks {
			component, named := strings.CutPrefix(check.Name, prefix)
			name, matched := strings.CutSuffix(component, suffix)
			if named && matched && name != "" && strings.Contains(check.App, "konflux") {
				add("release-" + name)
			}
		}
	} else if prior.Target == candidate.Target && prior.SHA == candidate.SHA {
		for _, old := range prior.Images {
			if old.Status == "Not scheduled" {
				add(old.Image)
			}
		}
	}
}

func addConformaCheck(candidate release.Candidate, validation release.Validation, prior release.RCVulnerabilities, img *release.ImageVulnerabilities) {
	if validation.SHA != candidate.SHA {
		for _, old := range prior.Images {
			if prior.Target == candidate.Target && prior.SHA == candidate.SHA && old.Image == img.Image {
				img.ConformaURL, img.ConformaStatus, img.ConformaReported = old.ConformaURL, old.ConformaStatus, old.ConformaReported
				img.ConformaReport = old.ConformaReport
			}
		}
		return
	}
	prefix, suffix := conformaCheckScope(candidate.Target.Tag)
	name := prefix + strings.TrimPrefix(img.Image, "release-") + suffix
	var latest release.ValidationCheck
	for _, check := range validation.Checks {
		if check.Name == name && strings.Contains(check.App, "konflux") && check.ID > latest.ID {
			latest = check
		}
	}
	img.ConformaURL = latest.URL
	img.ConformaReported = latest.ConformaReported
	img.ConformaDetails, img.ConformaAppID = latest.ConformaDetails, latest.AppID
	img.ConformaStatus = latest.Conclusion
	if latest.ID > 0 && latest.Conclusion == "" {
		img.ConformaStatus = "pending"
	}
}

func (c *Client) downloadScanReport(ctx context.Context, endpoint string) ([]release.Vulnerability, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusFound {
		_ = resp.Body.Close()
		destination, err := resp.Location()
		if err != nil || !scanDownloadHost(destination) {
			return nil, fmt.Errorf("unexpected scan artifact download location")
		}
		unsigned, err := http.NewRequestWithContext(ctx, http.MethodGet, destination.String(), nil)
		if err != nil {
			return nil, err
		}
		resp, err = c.http.Do(unsigned)
		if err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scan artifact unavailable")
	}
	archive, err := io.ReadAll(io.LimitReader(resp.Body, maxScanArchive+1))
	if err != nil || len(archive) > maxScanArchive {
		return nil, fmt.Errorf("scan archive is incomplete or too large")
	}
	return parseScanArchive(archive)
}

func scanDownloadHost(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Port() == "" &&
		(strings.HasSuffix(u.Hostname(), ".blob.core.windows.net") ||
			strings.HasSuffix(u.Hostname(), ".actions.githubusercontent.com"))
}

func parseScanArchive(data []byte) ([]release.Vulnerability, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if len(archive.File) != 1 || archive.File[0].Name != "scan-result.json" || archive.File[0].UncompressedSize64 > maxScanReport {
		return nil, fmt.Errorf("unexpected scan report archive")
	}
	file, err := archive.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var report struct {
		Result *struct {
			Vulnerabilities *[]struct {
				ID          string   `json:"cveId"`
				CVSS        *float64 `json:"cveCVSS"`
				Severity    string   `json:"cveSeverity"`
				Package     string   `json:"componentName"`
				Installed   string   `json:"componentVersion"`
				Fixed       string   `json:"componentFixedVersion"`
				URL         string   `json:"cveInfo"`
				AdvisoryURL string   `json:"advisoryInfo"`
			}
			Summary map[string]int
		}
	}
	decoder := json.NewDecoder(io.LimitReader(file, maxScanReport+1))
	if err := decoder.Decode(&report); err != nil || report.Result == nil {
		return nil, fmt.Errorf("invalid scan report")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("incomplete scan report")
	}
	total, ok := report.Result.Summary["TOTAL-VULNERABILITIES"]
	if report.Result.Vulnerabilities == nil {
		if ok && total == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("scan report has no vulnerability results")
	}
	var findings []release.Vulnerability
	for _, value := range *report.Result.Vulnerabilities {
		if strings.TrimSpace(value.ID) == "" {
			return nil, fmt.Errorf("scan finding has no identifier")
		}
		severity := strings.ToLower(value.Severity)
		if severity != "" {
			severity = strings.ToUpper(severity[:1]) + severity[1:]
		}
		findings = append(findings, release.Vulnerability{ID: value.ID, CVSS: value.CVSS, Severity: severity, Package: value.Package,
			Installed: value.Installed, Fixed: strings.TrimSpace(value.Fixed), URL: value.URL, AdvisoryURL: value.AdvisoryURL})
	}
	release.SortVulnerabilities(findings)
	return findings, nil
}
