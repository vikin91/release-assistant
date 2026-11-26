package github

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"release-assistant/internal/release"
)

const prMergeStatesQuery = `query PRMergeStates($owner: String!, $repo: String!, $base: String!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequests(states: OPEN, baseRefName: $base, first: 100, after: $cursor) {
      nodes { number headRefOid isDraft mergeStateStatus }
      pageInfo { hasNextPage endCursor }
    }
  }
}`

// readListedPRMergeStates batches verdicts for the REST inventory within the remaining read allowance.
func (c *Client) readListedPRMergeStates(ctx context.Context, cycle *discoveryCycle, repo release.Repository, ignored map[int]bool) {
	if c.token == "" {
		return
	}
	pending := make(map[int]*release.PR)
	for i := range repo.PRs {
		pr := &repo.PRs[i]
		if !ignored[pr.Target.PR] && pr.State == "open" && !pr.Draft && pr.MergeState == "" &&
			pr.Base == repo.Source.Branch && pr.Evidence.Fresh() {
			pending[pr.Target.PR] = pr
		}
	}
	owner, name, _ := strings.Cut(repo.Source.Repo, "/")
	request := struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}{Query: prMergeStatesQuery, Variables: map[string]string{"owner": owner, "repo": name, "base": repo.Source.Branch}}
	for range maxPages {
		if len(pending) == 0 || cycle.remaining <= cycle.reserved || cycle.rateErr != nil || ctx.Err() != nil {
			return
		}
		payload, err := json.Marshal(request)
		if err != nil {
			return
		}
		var response struct {
			Data struct {
				Repository struct {
					PullRequests struct {
						Nodes []struct {
							Number                       int
							HeadRefOid, MergeStateStatus string
							IsDraft                      bool
						}
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
					}
				}
			}
		}
		if err := c.readObservation(ctx, http.MethodPost, "/graphql", bytes.NewReader(payload), &response, nil, 0); err != nil {
			return
		}
		page := response.Data.Repository.PullRequests
		for _, node := range page.Nodes {
			pr := pending[node.Number]
			if pr != nil && node.HeadRefOid == pr.SHA && node.IsDraft == pr.Draft && node.MergeStateStatus != "" {
				pr.MergeState = strings.ToLower(node.MergeStateStatus)
				delete(pending, node.Number)
			}
		}
		if !page.PageInfo.HasNextPage || page.PageInfo.EndCursor == "" || page.PageInfo.EndCursor == request.Variables["cursor"] {
			return
		}
		request.Variables["cursor"] = page.PageInfo.EndCursor
	}
}
