package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"release-assistant/internal/release"
)

type (
	// commitRangePRCache retains successful commit associations across bounded polls.
	commitRangePRCache struct {
		mu       sync.Mutex
		baseSHA  string
		byCommit map[string][]release.CandidatePR
	}
	commitRangeComparison struct {
		Status  string
		Ahead   int `json:"ahead_by"`
		Commits []struct {
			SHA    string
			Commit struct{ Message string }
		}
	}
	commitRangeAssociations struct {
		prs              []release.CandidatePR
		commitsWithoutPR []release.CandidateCommit
	}
)

// ReadComponentTagPRs finds PRs whose merge commits landed between two component tags.
func (c *Client) ReadComponentTagPRs(ctx context.Context, src release.Source, previousSHA, selectedSHA string) ([]release.CandidatePR, error) {
	if previousSHA == selectedSHA {
		return nil, nil
	}
	cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: maxCandidateRequests, remaining: maxCandidateRequests}
	ctx = context.WithValue(ctx, discoveryCycleKey{}, cycle)
	endpoint := repoPath(src.Repo) + "/compare/" + segment(previousSHA) + "..." + segment(selectedSHA)
	var comparison commitRangeComparison
	if err := c.get(ctx, endpoint+"?per_page=100&page=1", &comparison); err != nil {
		return nil, err
	}
	if comparison.Status != "ahead" || comparison.Ahead <= 0 {
		return nil, fmt.Errorf("the previous tag is not an ancestor of the selected tag")
	}
	associations, err := c.readRangePRs(ctx, src, endpoint, previousSHA, comparison, comparison.Ahead)
	if err != nil {
		return nil, err
	}
	return associations.prs, nil
}

func (c *Client) readRangePRs(ctx context.Context, src release.Source, endpoint, baseSHA string, comparison commitRangeComparison, ahead int) (commitRangeAssociations, error) {
	if ahead == 0 {
		return commitRangeAssociations{}, nil
	}
	mergeCommitOnly := src.Name != "acs"
	key := src.Repo + ":" + src.Branch
	if mergeCommitOnly {
		key += ":component-tag"
	}
	value, _ := c.rangePRCache.LoadOrStore(key, &commitRangePRCache{})
	cache := value.(*commitRangePRCache)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.baseSHA != baseSHA {
		cache.baseSHA = baseSHA
		cache.byCommit = make(map[string][]release.CandidatePR)
	}

	var result commitRangeAssociations
	seen := make(map[int]bool)
	read := 0
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := c.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &comparison); err != nil {
				return result, err
			}
		}
		for _, commit := range comparison.Commits {
			if commit.SHA == "" {
				return result, fmt.Errorf("GitHub returned a comparison commit without a SHA")
			}
			prs, ok := cache.byCommit[commit.SHA]
			if !ok {
				var err error
				prs, err = c.commitPRs(ctx, src, commit.SHA, mergeCommitOnly)
				if err != nil {
					return result, err
				}
				cache.byCommit[commit.SHA] = prs
			}
			if !mergeCommitOnly && len(prs) == 0 {
				subject, _, _ := strings.Cut(commit.Commit.Message, "\n")
				result.commitsWithoutPR = append(result.commitsWithoutPR, release.CandidateCommit{
					SHA: commit.SHA, Subject: strings.TrimSpace(subject), URL: "https://github.com/" + src.Repo + "/commit/" + segment(commit.SHA),
				})
			}
			for _, pr := range prs {
				if !seen[pr.Number] {
					seen[pr.Number] = true
					result.prs = append(result.prs, pr)
				}
			}
			read++
		}
		if read == ahead {
			return result, nil
		}
		if len(comparison.Commits) < 100 {
			break
		}
	}
	return result, fmt.Errorf("GitHub did not return every comparison commit")
}

func (c *Client) commitPRs(ctx context.Context, src release.Source, sha string, mergeCommitOnly bool) ([]release.CandidatePR, error) {
	var prs []release.CandidatePR
	err := c.pages(ctx, repoPath(src.Repo)+"/commits/"+segment(sha)+"/pulls", func(raw json.RawMessage) error {
		var pr prResponse
		if err := json.Unmarshal(raw, &pr); err != nil {
			return err
		}
		if pr.Number <= 0 || pr.MergedAt == nil || pr.Base.Ref != src.Branch {
			return nil
		}
		if mergeCommitOnly && pr.MergeCommitSHA == "" {
			return fmt.Errorf("GitHub did not report the merge commit for PR #%d", pr.Number)
		}
		// A commit can also be associated with PRs that landed outside the range.
		if !mergeCommitOnly || pr.MergeCommitSHA == sha {
			prs = append(prs, release.CandidatePR{
				Number: pr.Number, Title: pr.Title, URL: "https://github.com/" + src.Repo + "/pull/" + strconv.Itoa(pr.Number),
			})
		}
		return nil
	})
	return prs, err
}
