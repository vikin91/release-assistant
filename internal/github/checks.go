package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/release"
)

type (
	commitCheckRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		URL        string `json:"html_url"`
		HeadSHA    string `json:"head_sha"`
		App        struct {
			ID   int64  `json:"id"`
			Slug string `json:"slug"`
		} `json:"app"`
		Suite struct {
			ID int64 `json:"id"`
		} `json:"check_suite"`
	}
	commitStatus struct {
		Context string `json:"context"`
		State   string `json:"state"`
		URL     string `json:"target_url"`
		Creator struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"creator"`
	}
	workflowRun struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		WorkflowID int64  `json:"workflow_id"`
		SuiteID    int64  `json:"check_suite_id"`
		SHA        string `json:"head_sha"`
		Event      string `json:"event"`
		Ref        string `json:"head_branch"`
	}
)

func (run commitCheckRun) check() release.Check {
	return release.Check{Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, URL: run.URL}
}

func (c *Client) readCommitCheckRuns(ctx context.Context, repo, sha string) ([]commitCheckRun, error) {
	var runs []commitCheckRun
	for page := 1; page <= maxPages; page++ {
		var response struct {
			Total int              `json:"total_count"`
			Runs  []commitCheckRun `json:"check_runs"`
		}
		endpoint := fmt.Sprintf("%s/commits/%s/check-runs?per_page=100&page=%d", repoPath(repo), segment(sha), page)
		if err := c.get(ctx, endpoint, &response); err != nil {
			return runs, err
		}
		runs = append(runs, response.Runs...)
		if page*100 >= response.Total {
			return runs, nil
		}
	}
	return runs, fmt.Errorf("GitHub check run pagination limit reached")
}

func (c *Client) readCommitStatuses(ctx context.Context, repo, sha string) ([]commitStatus, error) {
	var statuses []commitStatus
	err := c.pages(ctx, repoPath(repo)+"/commits/"+segment(sha)+"/statuses", func(raw json.RawMessage) error {
		var status commitStatus
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("malformed commit status")
		}
		statuses = append(statuses, status)
		return nil
	})
	return statuses, err
}

func (c *Client) readCommitWorkflows(ctx context.Context, repo, sha string) (map[int64]workflowRun, error) {
	workflows := make(map[int64]workflowRun)
	// Search filters on the Actions REST endpoint cap results at 1,000 runs.
	for page := 1; page <= 10; page++ {
		var response struct {
			Total int           `json:"total_count"`
			Runs  []workflowRun `json:"workflow_runs"`
		}
		endpoint := fmt.Sprintf("%s/actions/runs?head_sha=%s&per_page=100&page=%d", repoPath(repo), url.QueryEscape(sha), page)
		if err := c.get(ctx, endpoint, &response); err != nil {
			return workflows, err
		}
		for _, run := range response.Runs {
			if run.SHA == sha && run.SuiteID > 0 && run.ID > workflows[run.SuiteID].ID {
				workflows[run.SuiteID] = run
			}
		}
		if page*100 >= response.Total && response.Total < 1000 {
			return workflows, nil
		}
	}
	return workflows, fmt.Errorf("GitHub workflow run search limit reached")
}

// ReadRCChecks reads the candidate commit's checks and joins Actions runs by check suite.
// Commit statuses without a verified workflow remain useful as commit-scoped evidence.
func (c *Client) ReadRCChecks(ctx context.Context, target release.Target) release.Validation {
	result := release.Validation{Evidence: release.Evidence{At: time.Now()}}
	if target.Tag == "" || !target.Valid() {
		result.Evidence.Error = "The candidate commit could not be identified."
		return result
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := c.get(ctx, repoPath(target.Repo)+"/commits/"+segment("refs/tags/"+target.Tag), &commit); err != nil {
		result.Evidence.Error = "Candidate tag: " + requestProblem(err)
		return result
	}
	sha := commit.SHA
	if !release.ValidRef(sha) {
		result.Evidence.Error = "GitHub did not return the candidate commit."
		return result
	}
	result.SHA = sha
	base := "https://github.com/" + target.Repo
	result.Evidence.URL = base + "/commit/" + segment(sha) + "/checks"
	runs, checkErr := c.readCommitCheckRuns(ctx, target.Repo, sha)
	statuses, statusErr := c.readCommitStatuses(ctx, target.Repo, sha)
	var workflows map[int64]workflowRun
	var workflowErr error
	if slices.ContainsFunc(runs, func(run commitCheckRun) bool { return run.App.Slug == "github-actions" && run.Suite.ID > 0 }) {
		workflows, workflowErr = c.readCommitWorkflows(ctx, target.Repo, sha)
	}
	for _, resultError := range []struct {
		source string
		err    error
	}{{"Check runs", checkErr}, {"Commit statuses", statusErr}, {"Workflow triggers", workflowErr}} {
		if resultError.err != nil {
			result.Evidence.Error = strings.TrimSpace(result.Evidence.Error + " " + resultError.source + ": " + requestProblem(resultError.err))
		}
	}
	result.Evidence.Known = result.Evidence.Error == ""
	result.Checks = validationChecks(runs, statuses, workflows, sha, base, result.Evidence.URL)
	return result
}

func validationChecks(runs []commitCheckRun, statuses []commitStatus, workflows map[int64]workflowRun, sha, base, checksURL string) []release.ValidationCheck {
	type checkKey struct {
		name, app, event, ref string
		scope                 int64
	}
	latest := make(map[checkKey]release.ValidationCheck, len(runs))
	names := make(map[string]bool, len(runs))
	for _, run := range runs {
		if run.Name == "" || (run.HeadSHA != "" && run.HeadSHA != sha) {
			continue
		}
		names[run.Name] = true
		check := release.ValidationCheck{Check: run.check(), App: run.App.Slug, ID: run.ID}
		key := checkKey{name: run.Name, app: check.App, scope: run.Suite.ID}
		if workflow := workflows[run.Suite.ID]; check.App == "github-actions" && workflow.ID > 0 {
			check.Event, check.Ref = workflow.Event, workflow.Ref
			check.WorkflowName = workflow.Name
			check.WorkflowURL = base + "/actions/runs/" + strconv.FormatInt(workflow.ID, 10)
			key.event, key.ref = workflow.Event, workflow.Ref
			key.scope = cmp.Or(workflow.WorkflowID, run.Suite.ID)
		}
		if check.URL == "" && check.WorkflowURL == "" && run.ID > 0 {
			check.URL = checksURL + "?check_run_id=" + strconv.FormatInt(run.ID, 10)
		}
		if old, ok := latest[key]; !ok || old.ID < check.ID {
			latest[key] = check
		}
	}
	checks := make([]release.ValidationCheck, 0, len(latest)+len(statuses))
	for _, check := range latest {
		checks = append(checks, check)
	}
	for _, status := range statuses {
		if status.Context == "" || names[status.Context] {
			continue
		}
		names[status.Context] = true
		checks = append(checks, release.ValidationCheck{Check: release.Check{
			Name: status.Context, Status: status.State, URL: status.URL,
		}})
	}
	slices.SortFunc(checks, func(a, b release.ValidationCheck) int {
		if a.Name != b.Name {
			return cmp.Compare(a.Name, b.Name)
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return checks
}
