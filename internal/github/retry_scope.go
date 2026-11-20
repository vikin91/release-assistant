package github

import (
	"cmp"
	"context"
	"crypto/sha256"
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
		required               []requiredCheck
		workflowInputs         map[string]retryWorkflowInput
		workflowInputsVerified bool
		optional               bool
	}
	retryWorkflowJob struct {
		Name, Uses, If  string
		Needs           yaml.Node
		Strategy        struct{ Matrix yaml.Node }
		ContinueOnError string `yaml:"continue-on-error"`
	}
	retryWorkflow struct {
		Jobs        map[string]retryWorkflowJob
		auditedJobs map[string][]string
	}
)

func (s retryScope) matches(name string, appID int64) bool {
	return slices.ContainsFunc(s.required, func(r requiredCheck) bool {
		return r.context == name && (r.appID == 0 || r.appID == appID)
	})
}

func (s retryScope) includesOptional(source retrySource, job string) bool {
	return s.optional && retryOptionalList.matches(source, job)
}

// scopedRunChecks selects failures that can run with the current prerequisite results.
func (c *Client) scopedRunChecks(ctx context.Context, target release.Target, pr retryPR, run retryRun, scope retryScope, failures, observed []commitCheckRun) ([]commitCheckRun, []string, error) {
	source := retrySource{strings.ToLower(target.Repo), "github-actions", run.Path}
	var publishers []retryWorkflowProducer
	for _, input := range scope.workflowInputs {
		if input.build.ID == run.ID {
			publishers = append(publishers, input.pending...)
		}
	}
	if !slices.ContainsFunc(failures, func(check commitCheckRun) bool {
		return !retryDenyList.matches(source, check.Name) && (len(publishers) != 0 || scope.includesOptional(source, check.Name) ||
			slices.ContainsFunc(scope.required, func(rule requiredCheck) bool {
				return (rule.appID == 0 || rule.appID == check.App.ID) && !strings.HasPrefix(rule.context, "Red Hat Konflux /")
			}))
	}) {
		return nil, nil, nil
	}

	revision := run.workflowRevision(target.Repo, pr)
	if target.Tag != "" {
		revision = run.SHA
	}
	if revision == "" {
		return nil, nil, fmt.Errorf("cannot verify the workflow revision for %s (run %d)", run.Path, run.ID)
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
		} else {
			w.applyAuditedTree(retryWorkflowTrees[source.repo][file], data)
		}
		files[file] = w
		return w
	}
	load(run.Path)
	var results []retryCheckResult
	var passed []commitCheckRun
	for _, check := range observed {
		if check.Suite.ID == run.SuiteID && check.App.Slug == "github-actions" && check.HeadSHA == pr.Head.SHA {
			results = append(results, retryCheckResult{check.Name, check.Status == "completed" && check.check().Result() == release.Yes})
			if check.Status == "completed" && check.Conclusion == "success" {
				passed = append(passed, check)
			}
		}
	}
	var selected []commitCheckRun
	var blocked []string
	for _, check := range failures {
		if retryDenyList.matches(source, check.Name) {
			continue
		}
		needed := scope.matches(check.Name, check.App.ID) || slices.ContainsFunc(publishers, func(producer retryWorkflowProducer) bool {
			return producer.neededFor(check.Name, run.Path, load)
		})
		mergePrerequisite := false
		for _, root := range scope.required {
			if (root.appID != 0 && root.appID != check.App.ID) || !workflowPrerequisite(run.Path, root.context, check.Name, load) {
				continue
			}
			mergePrerequisite = true
			needed = needed || !slices.ContainsFunc(passed, func(current commitCheckRun) bool {
				return current.Name == root.context && (root.appID == 0 || current.App.ID == root.appID)
			})
		}
		if !needed && (mergePrerequisite || !scope.includesOptional(source, check.Name) || workflowCheckHasDependents(run.Path, check.Name, load)) {
			continue
		}
		if blocker := workflowRetryBlocker(run.Path, check.Name, results, load); blocker != "" {
			blocked = append(blocked, check.Name+": waiting for prerequisite "+blocker)
			continue
		}
		if blocker := retryWorkflowDependencyProblem(target, scope, run, check.Name, load); blocker != "" {
			blocked = append(blocked, check.Name+": "+blocker)
			continue
		}
		selected = append(selected, check)
	}
	if len(problems) > 0 {
		return nil, blocked, errors.Join(problems...)
	}
	return selected, blocked, nil
}

type retryCheckResult struct {
	name   string
	passed bool
}

// workflowCheckHasDependents identifies jobs that belong to a dependency chain.
func workflowCheckHasDependents(file, name string, load func(string) retryWorkflow) bool {
	visited := make(map[string]bool)
	for path.Dir(file) == ".github/workflows" && !visited[file] {
		visited[file] = true
		w := load(file)
		id, _, child := w.resolve(name)
		if id == "" {
			return false
		}
		for downstream, job := range w.Jobs {
			if !job.runsOnFailure() && slices.Contains(w.directNeeds(downstream), id) {
				return true
			}
		}
		local, ok := strings.CutPrefix(w.Jobs[id].Uses, "./")
		if !ok || child == "" {
			return false
		}
		file, name = local, child
	}
	return false
}

// runsOnFailure recognizes failure handlers that can run without a successful upstream job.
func (job retryWorkflowJob) runsOnFailure() bool {
	condition := strings.TrimSpace(job.If)
	condition = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(condition, "${{"), "}}"))
	return condition == "failure()" || strings.HasPrefix(condition, "failure() &&") ||
		strings.HasPrefix(condition, "always() && contains(join(needs.*.result, ','), 'failure')")
}

// workflowRetryBlocker checks the ancestors of a job in its caller and local reusable workflows.
func workflowRetryBlocker(file, name string, results []retryCheckResult, load func(string) retryWorkflow) string {
	visited := make(map[string]bool)
	for path.Dir(file) == ".github/workflows" && !visited[file] {
		visited[file] = true
		w := load(file)
		id, call, child := w.resolve(name)
		if id == "" {
			return ""
		}
		if blocker := w.prerequisiteBlocker(id, results, load); blocker != "" {
			return blocker
		}
		local, ok := strings.CutPrefix(w.Jobs[id].Uses, "./")
		if !ok || child == "" {
			return ""
		}
		var nested []retryCheckResult
		for _, result := range results {
			if suffix, ok := strings.CutPrefix(result.name, call+" / "); ok {
				nested = append(nested, retryCheckResult{suffix, result.passed})
			}
		}
		file, name, results = local, child, nested
	}
	return ""
}

func (w retryWorkflow) prerequisiteBlocker(id string, results []retryCheckResult, load func(string) retryWorkflow) string {
	seen := map[string]bool{id: true}
	work := w.directNeeds(id)
	for len(work) > 0 {
		dependency := work[0]
		work = work[1:]
		if seen[dependency] {
			continue
		}
		seen[dependency] = true
		matched := false
		for _, result := range results {
			resolved, _, child := w.resolve(result.name)
			if resolved != dependency || !verifiedChild(w.Jobs[dependency], child, load) {
				continue
			}
			matched = true
			if !result.passed {
				return result.name
			}
		}
		if !matched {
			return cmp.Or(w.Jobs[dependency].Name, dependency)
		}
		work = append(work, w.directNeeds(dependency)...)
	}
	return ""
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

// applyAuditedTree only overlays edges audited against this workflow revision.
func (w *retryWorkflow) applyAuditedTree(trees []retryWorkflowTree, data []byte) {
	if len(trees) == 0 {
		return
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	for _, tree := range trees {
		if !slices.Contains(tree.Fingerprints, fingerprint) {
			continue
		}
		w.auditedJobs = tree.Jobs
		for id, uses := range tree.Calls {
			job := w.Jobs[id]
			job.Uses = uses
			w.Jobs[id] = job
		}
		return
	}
}

func (w retryWorkflow) directNeeds(id string) []string {
	if needs, recorded := w.auditedJobs[id]; recorded {
		return slices.Clone(needs)
	}
	node := w.Jobs[id].Needs
	var needs []string
	switch node.Kind {
	case yaml.ScalarNode:
		needs = []string{node.Value}
	case yaml.SequenceNode:
		if node.Decode(&needs) != nil {
			return nil
		}
	}
	return slices.DeleteFunc(needs, func(dependency string) bool {
		job, ok := w.Jobs[dependency]
		return !ok || (job.ContinueOnError != "" && job.ContinueOnError != "false")
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
		for _, id := range w.directNeeds(job) {
			if id == upstream {
				return true
			}
			work = append(work, id)
		}
	}
	return false
}
