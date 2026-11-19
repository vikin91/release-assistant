package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"release-assistant/internal/release"
)

type (
	retryScope struct {
		required []requiredCheck
		optional bool
	}
	retryWorkflowJob struct {
		Name, Uses      string
		Needs           yaml.Node
		Strategy        struct{ Matrix yaml.Node }
		ContinueOnError string `yaml:"continue-on-error"`
	}
	retryWorkflow struct{ Jobs map[string]retryWorkflowJob }
)

func (s retryScope) matches(name string, appID int64) bool {
	return slices.ContainsFunc(s.required, func(r requiredCheck) bool {
		return r.context == name && (r.appID == 0 || r.appID == appID)
	})
}

func (s retryScope) includesOptional(source retrySource, job string) bool {
	return s.optional && retryOptionalList.matches(source, job)
}

// scopedRunChecks keeps direct requirements even when dependency evidence is unavailable.
func (c *Client) scopedRunChecks(ctx context.Context, target release.Target, pr retryPR, run retryRun, scope retryScope, failures, observed []commitCheckRun) ([]commitCheckRun, error) {
	source := retrySource{strings.ToLower(target.Repo), "github-actions", run.Path}
	var selected, candidates []commitCheckRun
	for _, check := range failures {
		switch {
		case retryDenyList.matches(source, check.Name):
		case scope.matches(check.Name, check.App.ID), scope.includesOptional(source, check.Name):
			selected = append(selected, check)
		default:
			candidates = append(candidates, check)
		}
	}
	if len(candidates) == 0 || len(scope.required) == 0 {
		return selected, nil
	}

	// Successful required checks in this suite need no upstream retries.
	var roots []requiredCheck
	for _, rule := range scope.required {
		if rule.appID != 0 && !slices.ContainsFunc(candidates, func(c commitCheckRun) bool { return c.App.ID == rule.appID }) {
			continue
		}
		if slices.ContainsFunc(observed, func(c commitCheckRun) bool {
			return c.Suite.ID == run.SuiteID && c.Name == rule.context && (rule.appID == 0 || c.App.ID == rule.appID) && c.Conclusion == "success"
		}) {
			continue
		}
		roots = append(roots, rule)
	}
	if len(roots) == 0 {
		return selected, nil
	}

	revision := run.workflowRevision(target.Repo, pr)
	if revision == "" {
		return selected, fmt.Errorf("cannot verify the workflow revision for %s (run %d)", run.Path, run.ID)
	}
	files := make(map[string]retryWorkflow)
	var problems []error
	load := func(file string) retryWorkflow {
		if w, ok := files[file]; ok {
			return w
		}
		var w retryWorkflow
		data, err := c.File(ctx, target.Repo, revision, file)
		if err == nil {
			err = yaml.Unmarshal(data, &w)
		}
		if err != nil {
			problems = append(problems, fmt.Errorf("reading prerequisites from %s: %w", file, err))
			w = retryWorkflow{}
		}
		files[file] = w
		return w
	}
	for _, check := range candidates {
		for _, root := range roots {
			if root.appID != 0 && root.appID != check.App.ID {
				continue
			}
			if workflowPrerequisite(run.Path, root.context, check.Name, load) {
				selected = append(selected, check)
				break
			}
		}
	}
	return selected, errors.Join(problems...)
}

// workflowRevision identifies the PR merge revision used by local workflow calls.
// A refreshed PR merge commit is usable only while the run's base and head still match.
func (run retryRun) workflowRevision(repo string, pr retryPR) string {
	ref := fmt.Sprintf("refs/pull/%d/merge", pr.Number)
	for _, workflow := range run.References {
		if workflow.Ref == ref && workflow.SHA != "" && strings.HasPrefix(workflow.Path, repo+"/.github/workflows/") && strings.HasSuffix(workflow.Path, "@"+workflow.SHA) {
			return workflow.SHA
		}
	}
	for _, p := range run.PRs {
		if p.Number == pr.Number && p.Head.SHA == pr.Head.SHA && pr.Base.SHA != "" && p.Base.SHA == pr.Base.SHA {
			return pr.MergeSHA
		}
	}
	return ""
}

// workflowPrerequisite follows needs within each invocation. Matrix siblings
// of a required job are independent; needs on a matrix job waits for all its legs.
func workflowPrerequisite(file, required, failed string, load func(string) retryWorkflow) bool {
	for path.Dir(file) == ".github/workflows" {
		w := load(file)
		root, rootCall, rootChild := w.resolve(required)
		upstream, failedCall, failedChild := w.resolve(failed)
		if root == "" || upstream == "" {
			return false
		}
		if root != upstream {
			return w.needs(root, upstream) &&
				verifiedChild(w.Jobs[root], rootChild, load) &&
				verifiedChild(w.Jobs[upstream], failedChild, load)
		}
		local, ok := strings.CutPrefix(w.Jobs[root].Uses, "./")
		if !ok || rootCall != failedCall || rootChild == "" || failedChild == "" {
			return false
		}
		file, required, failed = local, rootChild, failedChild
	}
	return false
}

// verifiedChild verifies the nested check within the local caller's revision.
func verifiedChild(job retryWorkflowJob, child string, load func(string) retryWorkflow) bool {
	for child != "" {
		local, ok := strings.CutPrefix(job.Uses, "./")
		if !ok || path.Dir(local) != ".github/workflows" {
			return false
		}
		w := load(local)
		id, _, rest := w.resolve(child)
		if id == "" {
			return false
		}
		job, child = w.Jobs[id], rest
		if job.ContinueOnError != "" && job.ContinueOnError != "false" {
			return false
		}
	}
	return true
}

// resolve maps an emitted check name to a unique job in this workflow.
func (w retryWorkflow) resolve(name string) (id, call, child string) {
	for key, job := range w.Jobs {
		display := cmp.Or(job.Name, key)
		instance, nested := name, ""
		if job.Uses != "" {
			instance, nested, _ = strings.Cut(name, " / ")
		}
		if job.matches(display, instance) {
			if id != "" {
				return "", "", ""
			}
			id, call, child = key, instance, nested
		}
	}
	return id, call, child
}

func (job retryWorkflowJob) matches(display, instance string) bool {
	if instance == display {
		return true
	}
	if job.Strategy.Matrix.Kind == 0 {
		return false
	}
	if row, ok := strings.CutPrefix(instance, display+" ("); ok && strings.HasSuffix(row, ")") {
		// For a single literal axis we can also verify a leg that GitHub has
		// not created yet, such as a downstream job blocked by a failed build.
		matrix := job.Strategy.Matrix
		if matrix.Kind == yaml.MappingNode && len(matrix.Content) == 2 {
			key, axis := matrix.Content[0].Value, matrix.Content[1]
			if key != "include" && key != "exclude" && axis.Kind == yaml.SequenceNode {
				value := strings.TrimSuffix(row, ")")
				return slices.ContainsFunc(axis.Content, func(v *yaml.Node) bool {
					return v.Kind != yaml.ScalarNode || strings.Contains(v.Value, "${{") || v.Value == value
				})
			}
		}
		return true
	}
	prefix, expression, ok := strings.Cut(display, "${{ matrix.")
	if !ok {
		return false
	}
	axis, suffix, ok := strings.Cut(expression, " }}")
	var axes map[string]yaml.Node
	if !ok || job.Strategy.Matrix.Decode(&axes) != nil {
		return false
	}
	return slices.ContainsFunc(axes[axis].Content, func(v *yaml.Node) bool {
		return v.Kind == yaml.ScalarNode && instance == prefix+v.Value+suffix
	})
}

func (w retryWorkflow) needs(root, upstream string) bool {
	seen := make(map[string]bool)
	work := []string{root}
	for len(work) > 0 {
		job := work[len(work)-1]
		work = work[:len(work)-1]
		if seen[job] {
			continue
		}
		seen[job] = true
		node := w.Jobs[job].Needs
		var needs []string
		switch node.Kind {
		case yaml.ScalarNode:
			needs = []string{node.Value}
		case yaml.SequenceNode:
			if node.Decode(&needs) != nil {
				continue
			}
		}
		for _, id := range needs {
			job, ok := w.Jobs[id]
			if !ok || (job.ContinueOnError != "" && job.ContinueOnError != "false") {
				continue
			}
			if id == upstream {
				return true
			}
			work = append(work, id)
		}
	}
	return false
}
