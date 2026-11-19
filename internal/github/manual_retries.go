package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"release-assistant/internal/release"
)

// openshiftCIReporterID identifies the openshift-ci[bot] GitHub status creator.
const openshiftCIReporterID = 75433959

type (
	// ManualRetryReport holds human-executable offers for the observed target.
	ManualRetryReport struct {
		URL            string
		Jobs           []ManualJob
		Prow, Warnings []string
	}
	// ManualJob links to the exact Actions job whose rerun control the engineer can use.
	ManualJob struct{ Name, URL string }

	retryBranch struct {
		SHA, Ref string
		Repo     struct{ ID int64 }
	}
	retryPR struct {
		Number     int
		State      string
		Base, Head retryBranch
	}
	retryRun struct {
		workflowRun
		Status, Path string
		Attempt      int                               `json:"run_attempt"`
		Number       int                               `json:"run_number"`
		Created      time.Time                         `json:"created_at"`
		PRs          []retryPR                         `json:"pull_requests"`
		References   []struct{ Path, Ref, SHA string } `json:"referenced_workflows"`
	}
	retryJob struct {
		ID                 int64
		Status, Conclusion string
		CheckURL           string `json:"check_run_url"`
	}
)

// ValidRetryTag accepts a literal Git tag, including namespaced tags, in a CLI target list.
func ValidRetryTag(tag string) bool {
	if strings.HasPrefix(tag, "-") || strings.HasSuffix(tag, ".") || strings.Contains(tag, "..") ||
		strings.Contains(tag, "@{") || strings.ContainsAny(tag, "~^:?*[\\") ||
		strings.ContainsFunc(tag, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return false
	}
	for part := range strings.SplitSeq(tag, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func (c *Client) retryTarget(ctx context.Context, t release.Target) (retryPR, error) {
	var pr retryPR
	if t.PR > 0 {
		err := c.get(ctx, fmt.Sprintf("%s/pulls/%d", repoPath(t.Repo), t.PR), &pr)
		if err == nil && (pr.Number != t.PR || pr.Head.SHA == "" || pr.Base.Ref == "" || pr.Base.Repo.ID == 0 || pr.Head.Repo.ID == 0) {
			err = errors.New("incomplete PR identity")
		}
		return pr, err
	}
	var commit struct{ SHA string }
	err := c.get(ctx, repoPath(t.Repo)+"/commits/"+segment("refs/tags/"+t.Tag), &commit)
	pr.Head.SHA = commit.SHA
	if err == nil && commit.SHA == "" {
		err = errors.New("missing tag commit")
	}
	return pr, err
}

// ManualRetries collects Actions job links and the failed presubmits covered by one PR-wide comment.
// Read errors can accompany independent offers for the same verified target.
func (c *Client) ManualRetries(ctx context.Context, t release.Target) (ManualRetryReport, error) {
	var report ManualRetryReport
	if !release.ValidRepo(t.Repo) || !(t.PR > 0 && t.Tag == "" || t.PR == 0 && ValidRetryTag(t.Tag)) {
		return report, errors.New("invalid retry target")
	}
	base := "https://github.com/" + t.Repo
	report.URL = base + "/releases/tag/" + segment(t.Tag)
	if t.PR > 0 {
		report.URL = fmt.Sprintf("%s/pull/%d", base, t.PR)
	}
	pr, err := c.retryTarget(ctx, t)
	if err != nil {
		return report, err
	}
	if t.Tag != "" {
		report.URL = base + "/commit/" + pr.Head.SHA
	} else if pr.State != "open" {
		report.Warnings = append(report.Warnings, "PR is not open")
		return report, nil
	}
	var prowErr error
	if t.PR > 0 {
		var statuses []commitStatus
		statuses, prowErr = c.readCommitStatuses(ctx, t.Repo, pr.Head.SHA)
		if prowErr == nil {
			report.Prow, report.Warnings = failedPresubmits(t, statuses)
		}
	}
	checks, actionsErr := c.readCommitCheckRuns(ctx, t.Repo, pr.Head.SHA)
	if actionsErr == nil {
		actionsErr = c.manualActions(ctx, t, pr, checks, &report)
	}
	if len(report.Jobs)+len(report.Prow) > 0 {
		current, err := c.retryTarget(ctx, t)
		if err != nil || current != pr {
			report.Jobs, report.Prow = nil, nil
			return report, errors.Join(prowErr, actionsErr, err, errors.New("target identity could not be reconfirmed"))
		}
	}
	return report, errors.Join(prowErr, actionsErr)
}

func failedPresubmits(t release.Target, statuses []commitStatus) (failed, warnings []string) {
	// The statuses API returns newest first. Another reporter can supersede the failure.
	seen := make(map[string]bool)
	prefix := fmt.Sprintf("/view/gs/test-platform-results-public/pr-logs/pull/%s/%d/", strings.ReplaceAll(strings.ToLower(t.Repo), "/", "_"), t.PR)
	for _, s := range statuses {
		if seen[s.Context] {
			continue
		}
		seen[s.Context] = true
		if s.State != "failure" || !strings.HasPrefix(s.Context, "ci/prow/") {
			continue
		}
		if s.Creator.ID != openshiftCIReporterID {
			warnings = append(warnings, s.Context+": Prow reporter unverified")
			continue
		}
		if !prowBuildURL(s.URL, prefix) {
			warnings = append(warnings, s.Context+": build URL does not identify this PR")
			continue
		}
		failed = append(failed, s.Context)
	}
	slices.Sort(failed)
	slices.Sort(warnings)
	return failed, warnings
}

func prowBuildURL(raw, prefix string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "prow.ci.openshift.org" || !strings.HasPrefix(u.Path, prefix) {
		return false
	}
	job, build, ok := strings.Cut(strings.TrimPrefix(u.Path, prefix), "/")
	id, err := strconv.ParseUint(build, 10, 64)
	return ok && job != "" && err == nil && id > 0
}

// retryPages requires every page before using a collection to resolve a job or superseding run.
func retryPages[T any](ctx context.Context, c *Client, endpoint, key string, limit int) ([]T, error) {
	var items []T
	for page := 1; page <= limit; page++ {
		var data map[string]json.RawMessage
		sep := "?"
		if strings.Contains(endpoint, "?") {
			sep = "&"
		}
		err := c.get(ctx, fmt.Sprintf("%s%sper_page=100&page=%d", endpoint, sep, page), &data)
		if err != nil {
			return nil, err
		}
		var rows []T
		var total int
		if json.Unmarshal(data[key], &rows) != nil || rows == nil || json.Unmarshal(data["total_count"], &total) != nil || total < 0 {
			return nil, fmt.Errorf("incomplete %s response", key)
		}
		items = append(items, rows...)
		if len(items) == total && !(key == "workflow_runs" && total >= 1000) {
			return items, nil
		}
		if len(rows) < 100 {
			break
		}
	}
	return nil, fmt.Errorf("incomplete %s pagination", key)
}

func (c *Client) manualActions(ctx context.Context, t release.Target, pr retryPR, checks []commitCheckRun, report *ManualRetryReport) error {
	failed := make(map[int64][]commitCheckRun)
	for _, check := range checks {
		if check.App.Slug == "github-actions" && check.HeadSHA == pr.Head.SHA && check.Status == "completed" && check.Conclusion == "failure" {
			failed[check.Suite.ID] = append(failed[check.Suite.ID], check)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	root := repoPath(t.Repo) + "/actions"
	runs, err := retryPages[retryRun](ctx, c, root+"/runs?head_sha="+url.QueryEscape(pr.Head.SHA), "workflow_runs", 10)
	if err != nil {
		return err
	}
	var problems []error
	countBySuite := make(map[int64]int)
	for _, run := range runs {
		countBySuite[run.SuiteID]++
	}
	for _, run := range runs {
		selected := failed[run.SuiteID]
		if len(selected) == 0 || run.SuiteID == 0 {
			continue
		}
		delete(failed, run.SuiteID)
		if countBySuite[run.SuiteID] != 1 {
			report.Warnings = append(report.Warnings, fmt.Sprintf("check suite %d matches multiple Actions runs", run.SuiteID))
			continue
		}
		jobs, reason, err := c.retryRunJobs(ctx, t, pr, run, runs, selected)
		report.Jobs = append(report.Jobs, jobs...)
		if reason != "" {
			report.Warnings = append(report.Warnings, fmt.Sprintf("%s (run %d): %s", run.Name, run.ID, reason))
		}
		if err != nil {
			problems = append(problems, err)
		}
	}
	for _, group := range failed {
		for _, check := range group {
			report.Warnings = append(report.Warnings, check.Name+": no matching Actions run")
		}
	}
	slices.Sort(report.Warnings)
	slices.SortFunc(report.Jobs, func(a, b ManualJob) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.URL, b.URL)) })
	return errors.Join(problems...)
}

func associatedPR(run retryRun, pr retryPR) bool {
	return run.Event == "pull_request" && slices.ContainsFunc(run.PRs, func(a retryPR) bool {
		return a.Number == pr.Number && a.Head.SHA == pr.Head.SHA && a.Base.Ref == pr.Base.Ref && a.Base.Repo.ID == pr.Base.Repo.ID && a.Head.Repo.ID == pr.Head.Repo.ID
	})
}

func (c *Client) retryRunJobs(ctx context.Context, t release.Target, pr retryPR, run retryRun, runs []retryRun, checks []commitCheckRun) ([]ManualJob, string, error) {
	if run.SHA != pr.Head.SHA || run.ID <= 0 || run.WorkflowID <= 0 || run.Number <= 0 {
		return nil, "missing run identity", nil
	}
	if t.PR > 0 && !associatedPR(run, pr) {
		return nil, "PR association unproven", nil
	}
	if t.Tag != "" && (run.Event != "push" || run.Ref != t.Tag) {
		return nil, "run belongs to another ref or event", nil
	}
	if run.Status != "completed" {
		return nil, "run is still active", nil
	}
	if run.Created.IsZero() || !time.Now().Before(run.Created.Add(30*24*time.Hour)) {
		return nil, "outside the 30-day rerun window", nil
	}
	if run.Attempt < 1 || run.Attempt > 50 {
		return nil, "attempt unavailable or rerun limit reached", nil
	}
	for _, other := range runs {
		if other.WorkflowID != run.WorkflowID || other.ID == run.ID || other.Number <= run.Number || other.Event != run.Event {
			continue
		}
		if associatedPR(other, pr) || (other.Ref == run.Ref && (t.Tag != "" || len(other.PRs) == 0)) {
			return nil, "newer run may supersede this failure", nil
		}
	}
	root := repoPath(t.Repo) + "/actions"
	endpoint := fmt.Sprintf("%s/runs/%d", root, run.ID)
	var workflow struct{ State string }
	if err := c.get(ctx, fmt.Sprintf("%s/workflows/%d", root, run.WorkflowID), &workflow); err != nil {
		return nil, "", err
	}
	if workflow.State != "active" {
		return nil, "workflow is not active", nil
	}
	jobs, err := retryPages[retryJob](ctx, c, fmt.Sprintf("%s/attempts/%d/jobs", endpoint, run.Attempt), "jobs", maxPages)
	if err != nil {
		return nil, "", err
	}
	var current retryRun
	if err := c.get(ctx, endpoint, &current); err != nil {
		return nil, "", err
	}
	if current.ID != run.ID || current.SuiteID != run.SuiteID || current.WorkflowID != run.WorkflowID || current.Attempt != run.Attempt || current.Status != "completed" || current.SHA != pr.Head.SHA {
		return nil, "run changed during collection", nil
	}
	if t.PR > 0 && !associatedPR(current, pr) {
		return nil, "PR association unproven", nil
	}
	if t.Tag != "" {
		verified, err := c.tagCaller(ctx, t, current)
		if err != nil || !verified {
			return nil, "tag trigger unproven", err
		}
	}
	var offers []ManualJob
	var missing []string
	for _, check := range checks {
		checkURL := fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", t.Repo, check.ID)
		var matches []retryJob
		for _, job := range jobs {
			if strings.EqualFold(job.CheckURL, checkURL) {
				matches = append(matches, job)
			}
		}
		if len(matches) != 1 || matches[0].ID <= 0 || matches[0].Status != "completed" || matches[0].Conclusion != "failure" {
			missing = append(missing, check.Name)
			continue
		}
		offers = append(offers, ManualJob{check.Name, fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", t.Repo, run.ID, matches[0].ID)})
	}
	if len(missing) > 0 {
		return offers, "no unique failed job in current attempt for " + strings.Join(missing, ", "), nil
	}
	return offers, "", nil
}

// tagCaller proves the triggering ref from a tag-only push filter or a local workflow call.
func (c *Client) tagCaller(ctx context.Context, t release.Target, run retryRun) (bool, error) {
	if run.Event != "push" || run.Ref != t.Tag || path.Dir(run.Path) != ".github/workflows" {
		return false, nil
	}
	file, err := c.File(ctx, t.Repo, run.SHA, run.Path)
	if err != nil {
		return false, err
	}
	var workflow struct {
		On   yaml.Node
		Jobs map[string]struct{ Uses string }
	}
	if err := yaml.Unmarshal(file, &workflow); err != nil {
		return false, fmt.Errorf("decode workflow %s: %w", run.Path, err)
	}
	// A push trigger limited to tags also disambiguates a tag from a same-name branch.
	var triggers struct {
		Push map[string]yaml.Node
	}
	if workflow.On.Decode(&triggers) == nil {
		push := triggers.Push
		if (len(push["tags"].Content) > 0 || len(push["tags-ignore"].Content) > 0) &&
			push["branches"].Kind == 0 && push["branches-ignore"].Kind == 0 {
			return true, nil
		}
	}
	for _, job := range workflow.Jobs {
		if job.Uses != "" && !strings.HasPrefix(job.Uses, "./.github/workflows/") {
			return false, nil
		}
	}
	// With mixed local and pinned calls the referenced workflow could belong to a
	// different call chain. Accept only evidence that agrees on the inherited ref.
	for _, ref := range run.References {
		if ref.SHA != run.SHA || ref.Ref != "refs/tags/"+t.Tag {
			return false, nil
		}
	}
	for _, job := range workflow.Jobs {
		local, ok := strings.CutPrefix(job.Uses, "./.github/workflows/")
		if !ok {
			continue
		}
		fullPath := t.Repo + "/.github/workflows/" + local + "@" + run.SHA
		for _, ref := range run.References {
			if ref.Path == fullPath {
				return true, nil
			}
		}
	}
	return false, nil
}
