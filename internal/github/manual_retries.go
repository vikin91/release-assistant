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

const (
	// openshiftCIReporterID identifies the bot user in GitHub commit statuses.
	openshiftCIReporterID = 75433959
	// openshiftCIAppID identifies the same provider in GitHub branch rules.
	openshiftCIAppID = 91280
)

type (
	// ManualRetryReport holds human-executable offers for the observed target.
	ManualRetryReport = release.ManualRetryReport
	// ManualJob identifies an Actions job for a manual rerun.
	ManualJob = release.ManualJob

	retryBranch struct {
		SHA, Ref string
		Repo     struct{ ID int64 }
	}
	retryPR struct {
		Number     int
		State      string
		MergeSHA   string `json:"merge_commit_sha"`
		Base, Head retryBranch
	}
	retryRun struct {
		workflowRun
		Status, Conclusion, Path string
		Attempt                  int                               `json:"run_attempt"`
		Number                   int                               `json:"run_number"`
		Created                  time.Time                         `json:"created_at"`
		PRs                      []retryPR                         `json:"pull_requests"`
		References               []struct{ Path, Ref, SHA string } `json:"referenced_workflows"`
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

// ManualRetries classifies failed checks and offers verified Actions and Prow retries.
// Read errors can accompany independent offers for the same verified target.
func (c *Client) ManualRetries(ctx context.Context, t release.Target) (ManualRetryReport, error) {
	return c.ManualRetriesWithOptional(ctx, t, false)
}

// ManualRetriesWithOptional includes configured optional CI jobs when requested.
// A tag has no merge base branch and therefore has no required merge checks.
func (c *Client) ManualRetriesWithOptional(ctx context.Context, t release.Target, optional bool) (ManualRetryReport, error) {
	report := ManualRetryReport{Target: t}
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
	report.SHA = pr.Head.SHA
	scope := retryScope{optional: optional}
	var policyErr error
	if t.PR > 0 {
		var branch branchResponse
		// The enforcement level disambiguates a 404 from the classic protection API.
		_ = c.get(ctx, repoPath(t.Repo)+"/branches/"+segment(pr.Base.Ref), &branch)
		var problem string
		scope.required, problem = c.requiredChecks(ctx, release.Source{Repo: t.Repo, Branch: pr.Base.Ref}, branch.Protection.RequiredStatusChecks.EnforcementLevel)
		if problem != "" {
			policyErr = fmt.Errorf("required checks for %s: %s", pr.Base.Ref, problem)
		}
	}
	if len(scope.required) == 0 && !optional {
		return report, policyErr
	}
	statuses, prowErr := c.readCommitStatuses(ctx, t.Repo, pr.Head.SHA)
	if prowErr == nil {
		prowErr = c.prowRetries(ctx, &report, pr.Base.Ref, statuses, scope)
	}
	checks, actionsErr := c.readCommitCheckRuns(ctx, t.Repo, pr.Head.SHA)
	if actionsErr == nil {
		actionsErr = c.manualActions(ctx, t, pr, checks, &report, scope)
	}
	slices.Sort(report.OtherChecks)
	if len(report.Jobs)+len(report.Prow)+len(report.OtherChecks) > 0 {
		current, err := c.retryTarget(ctx, t)
		if err != nil || current != pr {
			report.SHA = ""
			report.Jobs, report.Prow, report.OtherChecks = nil, nil, nil
			return report, errors.Join(policyErr, prowErr, actionsErr, err, errors.New("target identity could not be reconfirmed"))
		}
	}
	return report, errors.Join(policyErr, prowErr, actionsErr)
}

func failedPresubmits(report *ManualRetryReport, statuses []commitStatus, scope retryScope) map[string]failedPresubmit {
	t := report.Target
	failed := make(map[string]failedPresubmit)
	// The statuses API returns newest first. Another reporter can supersede the failure.
	seen := make(map[string]bool)
	prefix := fmt.Sprintf("/view/gs/test-platform-results-public/pr-logs/pull/%s/%d/", strings.ReplaceAll(strings.ToLower(t.Repo), "/", "_"), t.PR)
	for _, s := range statuses {
		key := strings.ToLower(s.Context)
		if seen[key] {
			continue
		}
		seen[key] = true
		if s.State != "failure" && s.State != "error" {
			continue
		}
		source := retrySource{strings.ToLower(t.Repo), "Prow", ""}
		var appID int64
		if s.Creator.ID == openshiftCIReporterID {
			appID = openshiftCIAppID
		}
		optional := appID != 0 && scope.includesOptional(source, s.Context)
		if !scope.matches(s.Context, appID) && !optional {
			continue
		}
		if appID == 0 {
			provider := "commit status"
			if s.Creator.Login != "" {
				provider += " (" + s.Creator.Login + ")"
			}
			classifyRetry(report, provider, "", s.Context)
			continue
		}
		if !classifyRetry(report, "Prow", "", s.Context) {
			continue
		}
		if t.PR == 0 || s.State != "failure" {
			report.Warnings = append(report.Warnings, s.Context+": Prow retries require a failed PR presubmit")
			continue
		}
		job := prowBuildJob(s.URL, prefix)
		if job == "" {
			report.Warnings = append(report.Warnings, s.Context+": build URL does not identify this PR")
			continue
		}
		failed[s.Context] = failedPresubmit{Name: job, URL: s.URL}
	}
	return failed
}

func prowBuildJob(raw, prefix string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "prow.ci.openshift.org" || !strings.HasPrefix(u.Path, prefix) {
		return ""
	}
	job, build, ok := strings.Cut(strings.TrimPrefix(u.Path, prefix), "/")
	id, err := strconv.ParseUint(build, 10, 64)
	if ok && err == nil && id > 0 {
		return job
	}
	return ""
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

func (c *Client) manualActions(ctx context.Context, t release.Target, pr retryPR, checks []commitCheckRun, report *ManualRetryReport, scope retryScope) error {
	failed := make(map[int64][]commitCheckRun)
	for _, check := range checks {
		if check.HeadSHA != pr.Head.SHA || check.Status != "completed" || check.check().Result() != release.No {
			continue
		}
		if check.App.Slug != "github-actions" {
			if scope.matches(check.Name, check.App.ID) {
				classifyRetry(report, cmp.Or(check.App.Slug, "unknown reporter"), "", check.Name)
			}
			continue
		}
		failed[check.Suite.ID] = append(failed[check.Suite.ID], check)
	}
	if len(failed) == 0 {
		return nil
	}
	root := repoPath(t.Repo) + "/actions"
	runs, err := retryPages[retryRun](ctx, c, root+"/runs?head_sha="+url.QueryEscape(pr.Head.SHA), "workflow_runs", 10)
	var problems []error
	if err != nil {
		problems = append(problems, err)
	}
	countBySuite := make(map[int64]int)
	latestVerifiedPRRun := make(map[int64]int)
	for _, run := range runs {
		countBySuite[run.SuiteID]++
		if t.PR > 0 && run.targetProblem(t, pr) == "" {
			latestVerifiedPRRun[run.WorkflowID] = max(latestVerifiedPRRun[run.WorkflowID], run.Number)
		}
	}
	for _, run := range runs {
		selected := failed[run.SuiteID]
		if len(selected) == 0 || run.SuiteID == 0 {
			continue
		}
		if countBySuite[run.SuiteID] != 1 {
			continue
		}
		delete(failed, run.SuiteID)
		if run.targetProblem(t, pr) != "" || run.Number < latestVerifiedPRRun[run.WorkflowID] {
			continue
		}
		source := retrySource{strings.ToLower(t.Repo), "github-actions", run.Path}
		if retryDenyList.matches(source, "") {
			continue
		}
		observed := checks
		if run.Attempt > 1 {
			latest, reason, err := c.latestRetryChecks(ctx, t, pr, run, checks)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			if reason != "" {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s (run %d): %s", run.Name, run.ID, reason))
				continue
			}
			observed = latest
			selected = selected[:0]
			for _, check := range latest {
				if check.Status == "completed" && check.check().Result() == release.No {
					selected = append(selected, check)
				}
			}
		}
		selected, scopeErr := c.scopedRunChecks(ctx, t, pr, run, scope, selected, observed)
		if scopeErr != nil {
			problems = append(problems, scopeErr)
		}
		if len(selected) == 0 {
			continue
		}
		jobs, reason, err := c.retryRunJobs(ctx, report, pr, run, runs, selected)
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
			if !scope.matches(check.Name, check.App.ID) {
				continue
			}
			classifyRetry(report, check.App.Slug, "", check.Name)
			report.Warnings = append(report.Warnings, check.Name+": no unique matching Actions run")
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

func (run retryRun) targetProblem(t release.Target, pr retryPR) string {
	if run.SHA != pr.Head.SHA || run.ID <= 0 || run.WorkflowID <= 0 || run.Number <= 0 {
		return "missing run identity"
	}
	if t.PR > 0 && !associatedPR(run, pr) {
		return "PR association unproven"
	}
	if t.Tag != "" && (run.Event != "push" || run.Ref != t.Tag) {
		return "run belongs to another ref or event"
	}
	return ""
}

// latestRetryChecks limits rerun evidence to the checks GitHub links from its latest job list.
func (c *Client) latestRetryChecks(ctx context.Context, t release.Target, pr retryPR, run retryRun, checks []commitCheckRun) ([]commitCheckRun, string, error) {
	endpoint := fmt.Sprintf("%s/actions/runs/%d/jobs?filter=latest", repoPath(t.Repo), run.ID)
	jobs, err := retryPages[retryJob](ctx, c, endpoint, "jobs", maxPages)
	if err != nil {
		return nil, "", fmt.Errorf("reading latest jobs for run %d: %w", run.ID, err)
	}
	if _, reason, err := c.confirmRetryRun(ctx, t, pr, run); reason != "" || err != nil {
		return nil, reason, err
	}
	byCheckURL := make(map[string]retryJob, len(jobs))
	for _, job := range jobs {
		url := strings.ToLower(job.CheckURL)
		if _, exists := byCheckURL[url]; exists || job.ID <= 0 || url == "" || job.Status == "" {
			return nil, "latest jobs lack unique check identities", nil
		}
		byCheckURL[url] = job
	}
	var latest []commitCheckRun
	for _, check := range checks {
		if check.Suite.ID != run.SuiteID || check.HeadSHA != pr.Head.SHA || check.App.Slug != "github-actions" {
			continue
		}
		checkURL := fmt.Sprintf("https://api.github.com/repos/%s/check-runs/%d", t.Repo, check.ID)
		if job, ok := byCheckURL[strings.ToLower(checkURL)]; ok {
			check.Status, check.Conclusion = job.Status, job.Conclusion
			latest = append(latest, check)
		}
	}
	return latest, "", nil
}

// confirmRetryRun checks that the run still represents the observed attempt.
func (c *Client) confirmRetryRun(ctx context.Context, t release.Target, pr retryPR, run retryRun) (retryRun, string, error) {
	var current retryRun
	endpoint := fmt.Sprintf("%s/actions/runs/%d", repoPath(t.Repo), run.ID)
	if err := c.get(ctx, endpoint, &current); err != nil {
		return current, "", err
	}
	if current.ID != run.ID || current.SuiteID != run.SuiteID || current.WorkflowID != run.WorkflowID || current.Path != run.Path || current.Attempt != run.Attempt || current.Status != run.Status || current.SHA != pr.Head.SHA {
		return current, "run changed during collection", nil
	}
	if t.PR > 0 && !associatedPR(current, pr) {
		return current, "PR association unproven", nil
	}
	return current, "", nil
}

// actionsImageBuildProblem checks the workflow that publishes the images for e2e-dispatch.
func actionsImageBuildProblem(t release.Target, pr retryPR, runs []retryRun) string {
	const unverified = "cannot verify the Build run for this commit"
	var latest retryRun
	for _, run := range runs {
		if run.Path != stackroxBuildWorkflow || run.targetProblem(t, pr) != "" {
			continue
		}
		if latest.ID != 0 && (latest.WorkflowID != run.WorkflowID || latest.Number == run.Number) {
			return unverified
		}
		if run.Number > latest.Number {
			latest = run
		}
	}
	if latest.ID == 0 {
		return unverified
	}
	if latest.Status == "completed" && latest.Conclusion == "success" {
		return ""
	}
	return fmt.Sprintf("inspect the Build run for this commit (%s) at https://github.com/%s/actions/runs/%d",
		cmp.Or(latest.Conclusion, latest.Status, "unknown"), t.Repo, latest.ID)
}

func (c *Client) retryRunJobs(ctx context.Context, report *ManualRetryReport, pr retryPR, run retryRun, runs []retryRun, checks []commitCheckRun) ([]ManualJob, string, error) {
	t := report.Target
	var allowed []commitCheckRun
	for _, check := range checks {
		if !classifyRetry(report, check.App.Slug, run.Path, check.Name) {
			continue
		}
		if check.Conclusion != "failure" {
			report.Warnings = append(report.Warnings, check.Name+": ineligible Actions conclusion "+check.Conclusion)
			continue
		}
		if strings.EqualFold(t.Repo, "stackrox/stackrox") && run.Path == stackroxE2EDispatchWorkflow && check.Name == waitForActionsImagesJob {
			if problem := actionsImageBuildProblem(t, pr, runs); problem != "" {
				report.Warnings = append(report.Warnings, check.Name+": "+problem)
				continue
			}
		}
		allowed = append(allowed, check)
	}
	if len(allowed) == 0 {
		return nil, "", nil
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
	current, reason, err := c.confirmRetryRun(ctx, t, pr, run)
	if reason != "" || err != nil {
		return nil, reason, err
	}
	if t.Tag != "" {
		verified, err := c.tagCaller(ctx, t, current)
		if err != nil || !verified {
			return nil, "tag trigger unproven", err
		}
	}
	workflowName := current.Name
	if t.Tag != "" {
		workflowName = strings.TrimSuffix(workflowName, " ("+t.Tag+")")
	}
	var offers []ManualJob
	var missing []string
	for _, check := range allowed {
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
		jobID := matches[0].ID
		offers = append(offers, ManualJob{
			ID:           jobID,
			Name:         check.Name,
			WorkflowName: workflowName,
			URL:          fmt.Sprintf("https://github.com/%s/actions/runs/%d/job/%d", t.Repo, run.ID, jobID),
		})
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
