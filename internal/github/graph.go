package github

import (
	"context"
	"errors"
	"fmt"

	"release-assistant/internal/release"
)

type (
	commitGraph struct {
		parents      map[string][]string
		failed       map[string]error
		requests     int
		cache        *CommitWindowCache
		cacheHits    int
		limitReached bool
	}
	graphCommit struct {
		SHA     string `json:"sha"`
		Parents *[]struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
)

func newCommitGraph() *commitGraph {
	return &commitGraph{parents: map[string][]string{}, failed: map[string]error{}}
}

// load follows every parent across bounded history windows shared by all roots.
// Window boundaries never imply that a commit has no more ancestors.
func (g *commitGraph) load(ctx context.Context, c *Client, repo string, roots []string) error {
	pending := append([]string(nil), roots...)
	seen := map[string]bool{}
	var issues []error
	for len(pending) > 0 {
		sha := pending[0]
		pending = pending[1:]
		if seen[sha] {
			continue
		}
		seen[sha] = true
		if parents, ok := g.parents[sha]; ok {
			pending = append(pending, parents...)
			continue
		}
		if err := g.failed[sha]; err != nil {
			issues = append(issues, err)
			continue
		}
		if g.requests == maxPages {
			g.limitReached = true
			break
		}
		g.requests++
		if err := g.readWindow(ctx, c, repo, sha); err != nil {
			g.failed[sha] = err
			issues = append(issues, err)
			continue
		}
		pending = append(pending, g.parents[sha]...)
	}
	return errors.Join(issues...)
}

func (g *commitGraph) addWindow(root string, window []graphCommit) error {
	found := false
	for _, commit := range window {
		if commit.SHA == "" || commit.Parents == nil {
			return fmt.Errorf("GitHub commit graph returned malformed commit metadata")
		}
		for _, parent := range *commit.Parents {
			if parent.SHA == "" || parent.SHA == commit.SHA {
				return fmt.Errorf("GitHub commit graph returned malformed parent metadata")
			}
		}
		found = found || commit.SHA == root
	}
	if !found {
		return fmt.Errorf("GitHub commit graph omitted the requested commit")
	}
	for _, commit := range window {
		parents := make([]string, 0, len(*commit.Parents))
		for _, parent := range *commit.Parents {
			parents = append(parents, parent.SHA)
		}
		g.parents[commit.SHA] = parents
	}
	return nil
}

// ancestors permits negative answers only when every reachable parent is known.
func (g *commitGraph) ancestors(root string) (map[string]bool, bool) {
	reachable := map[string]bool{}
	pending := []string{root}
	complete := true
	for len(pending) > 0 {
		sha := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if reachable[sha] {
			continue
		}
		reachable[sha] = true
		parents, ok := g.parents[sha]
		if !ok {
			complete = false
			continue
		}
		pending = append(pending, parents...)
	}
	return reachable, complete
}

func graphContains(reachable map[string]bool, complete bool, sha string) release.Truth {
	if reachable[sha] {
		return release.Yes
	}
	if complete {
		return release.No
	}
	return ""
}
