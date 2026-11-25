package web

import (
	"cmp"
	"net/url"
	"strconv"
	"strings"

	"release-assistant/internal/release"
)

type (
	tagRebuildModel struct {
		CommitURL, KonfluxComment, ActionsURL, ExampleCommand string
		Workflows                                             []tagRebuildWorkflow
	}
	tagRebuildWorkflow struct {
		Name, URL, Command string
	}
)

func tagRebuild(tag tagModel, plans []release.Plan) *tagRebuildModel {
	target := release.Target{Repo: tag.Repo, Tag: tag.Tag.Name}
	if !target.Valid() || !tag.Tag.Evidence.Fresh() || !release.ValidRef(tag.Tag.SHA) {
		return nil
	}
	base := "https://github.com/" + target.Repo
	query := url.Values{"query": {"event:push branch:" + target.Tag}}
	runURL := base + "/actions/runs/"
	endpoint := "gh api --method POST repos/" + target.Repo + "/actions/runs/"
	rebuild := &tagRebuildModel{
		CommitURL:      base + "/commit/" + url.PathEscape(tag.Tag.SHA),
		KonfluxComment: "/test tag:" + target.Tag,
		ActionsURL:     base + "/actions?" + query.Encode(),
		ExampleCommand: endpoint + "RUN_ID/rerun",
	}
	seen := make(map[string]bool)
	for _, plan := range plans {
		if plan.Target != target || plan.SHA != tag.Tag.SHA || plan.Validation.SHA != tag.Tag.SHA || plan.Validation.Evidence.Historical {
			continue
		}
		for _, check := range plan.Validation.Checks {
			if !matchesRebuildWorkflow(check, target.Tag) {
				continue
			}
			id, ok := strings.CutPrefix(check.WorkflowURL, runURL)
			number, err := strconv.ParseUint(id, 10, 64)
			if !ok || err != nil || number == 0 || seen[id] {
				continue
			}
			seen[id] = true
			rebuild.Workflows = append(rebuild.Workflows, tagRebuildWorkflow{
				Name: cmp.Or(check.WorkflowName, "Workflow run "+id), URL: check.WorkflowURL,
				Command: endpoint + id + "/rerun",
			})
		}
	}
	return rebuild
}

func matchesRebuildWorkflow(check release.ValidationCheck, tag string) bool {
	if check.App != "github-actions" || check.Event != "push" ||
		(check.Ref != tag && check.Ref != "refs/tags/"+tag) ||
		strings.EqualFold(check.WorkflowName, "Test Konflux builds") {
		return false
	}
	path, ref, _ := strings.Cut(check.WorkflowPath, "@")
	return path != ".github/workflows/konflux.yml" && path != ".github/workflows/konflux-tests.yml" &&
		(!strings.HasPrefix(ref, "refs/") || ref == "refs/tags/"+tag)
}
