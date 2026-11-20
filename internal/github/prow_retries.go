package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"release-assistant/internal/release"
)

type (
	prowJob struct {
		Name, Context, Trigger string
		Command                string `yaml:"rerun_command"`
		Branches               []string
		SkipBranches           []string `yaml:"skip_branches"`
	}
	prowFile struct {
		sha  string
		jobs []prowJob
	}
	prowTrigger struct {
		prowJob
		pattern *regexp.Regexp
	}
	failedPresubmit struct{ Name, URL string }
)

var singleTestCommand = regexp.MustCompile(`^/test [A-Za-z0-9][A-Za-z0-9_.-]*$`)

// prowRetries checks the complete comment against the applicable presubmit triggers.
func (c *Client) prowRetries(ctx context.Context, report *ManualRetryReport, branch string, statuses []commitStatus, scope retryScope) error {
	failed := failedPresubmits(report, statuses, scope)
	if len(failed) == 0 {
		return nil
	}
	triggers, err := c.prowTriggers(ctx, strings.ToLower(report.Target.Repo), branch)
	if err != nil {
		return fmt.Errorf("reading OpenShift CI presubmits: %w", err)
	}
	retries, problems := targetedProwRetries(failed, triggers)
	report.Prow = retries
	report.Warnings = append(report.Warnings, problems...)
	return nil
}

func targetedProwRetries(failed map[string]failedPresubmit, triggers []prowTrigger) ([]release.ProwRetry, []string) {
	var report release.ManualRetryReport
	var warnings []string
	selected := make(map[int]bool)
	for _, status := range slices.Sorted(maps.Keys(failed)) {
		build := failed[status]
		matched := -1
		for i, job := range triggers {
			if job.Context == status && job.Name == build.Name {
				if matched >= 0 {
					matched = -1
					break
				}
				matched = i
			}
		}
		if matched < 0 {
			warnings = append(warnings, status+": no unique presubmit matches this failed build")
			continue
		}
		cmd := triggers[matched].Command
		if !singleTestCommand.MatchString(cmd) || cmd == "/test all" || !triggers[matched].pattern.MatchString(cmd) {
			warnings = append(warnings, status+": no verified job-specific /test command")
			continue
		}
		if slices.ContainsFunc(triggers, func(other prowTrigger) bool {
			return other.Name != build.Name && other.pattern.MatchString(cmd)
		}) {
			warnings = append(warnings, status+": command also matches another presubmit")
			continue
		}
		selected[matched] = true
		report.Prow = append(report.Prow, release.ProwRetry{Context: status, Command: cmd, URL: build.URL})
	}
	if len(report.Prow) > 0 {
		body := report.ProwComment()
		for i, job := range triggers {
			if job.pattern.MatchString(body) != selected[i] {
				return nil, append(warnings, "The combined Prow comment does not match exactly the selected presubmits.")
			}
		}
	}
	return report.Prow, warnings
}

func (c *Client) prowTriggers(ctx context.Context, repo, branch string) ([]prowTrigger, error) {
	var files []struct{ Name, SHA, Type string }
	dir := "ci-operator/jobs/" + repo
	if err := c.get(ctx, "/repos/openshift/release/contents/"+dir, &files); err != nil {
		return nil, err
	}
	if files == nil || len(files) >= 1000 {
		return nil, fmt.Errorf("incomplete Prow configuration directory for %s", repo)
	}
	var triggers []prowTrigger
	for _, f := range files {
		if f.Type == "dir" {
			return nil, fmt.Errorf("nested Prow configuration directory %s cannot be verified", f.Name)
		}
		isYAML := strings.HasSuffix(f.Name, ".yaml") || strings.HasSuffix(f.Name, ".yml")
		if !isYAML || strings.HasSuffix(f.Name, "-periodics.yaml") || strings.HasSuffix(f.Name, "-postsubmits.yaml") {
			continue
		}
		if f.Type != "file" || f.SHA == "" {
			return nil, fmt.Errorf("incomplete Prow configuration entry %s", f.Name)
		}
		jobs, err := c.prowFileJobs(ctx, repo, dir+"/"+f.Name, f.SHA)
		if err != nil {
			return nil, err
		}
		for _, job := range jobs {
			allowed, err := prowBranchMatch(branch, job.Branches, job.SkipBranches)
			if err != nil {
				return nil, err
			}
			if !allowed {
				continue
			}
			if job.Context == "" {
				job.Context = job.Name
			}
			if job.Command == "" && job.Trigger == "" {
				job.Command = "/test " + job.Name
				// Prow's DefaultTriggerFor interpolates the job name as a regex.
				job.Trigger = fmt.Sprintf(`(?m)^/test( | .* )%s,?($|\s.*)`, job.Name)
			}
			pattern, err := regexp.Compile(job.Trigger)
			if err != nil {
				return nil, fmt.Errorf("invalid trigger for %s: %w", job.Name, err)
			}
			triggers = append(triggers, prowTrigger{job, pattern})
		}
	}
	return triggers, nil
}

func prowBranchMatch(branch string, branches, skip []string) (bool, error) {
	if len(skip) > 0 {
		excluded, err := regexp.MatchString(strings.Join(skip, "|"), branch)
		if excluded || err != nil {
			return false, err
		}
	}
	if len(branches) == 0 {
		return true, nil
	}
	return regexp.MatchString(strings.Join(branches, "|"), branch)
}

// prowFileJobs shares immutable blob reads across PRs and replaces a file when its SHA changes.
func (c *Client) prowFileJobs(ctx context.Context, repo, path, sha string) ([]prowJob, error) {
	if v, ok := c.prowFiles.Load(path); ok {
		if file := v.(prowFile); file.sha == sha {
			return file.jobs, nil
		}
	}
	var blob struct{ Encoding, Content string }
	if err := c.get(ctx, "/repos/openshift/release/git/blobs/"+segment(sha), &blob); err != nil {
		return nil, err
	}
	if blob.Encoding != "base64" {
		return nil, fmt.Errorf("unsupported encoding for %s", path)
	}
	data, err := base64.StdEncoding.DecodeString(blob.Content)
	if err != nil {
		return nil, err
	}
	var config struct{ Presubmits map[string][]prowJob }
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("invalid presubmits in %s: %w", path, err)
	}
	jobs := config.Presubmits[repo]
	c.prowFiles.Store(path, prowFile{sha, jobs})
	return jobs, nil
}
