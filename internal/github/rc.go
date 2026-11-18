package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/release"
)

// maxCandidateRequests leaves room in the ACS poll for pinning and build reads.
const maxCandidateRequests = 20

type (
	candidateTag struct {
		name, sha, kind string
		number          uint64
	}
	// candidatePRCache retains successful commit associations across bounded polls.
	candidatePRCache struct {
		mu       sync.Mutex
		tagSHA   string
		byCommit map[string][]release.CandidatePR
	}
	candidateComparison struct {
		Status  string
		Ahead   int `json:"ahead_by"`
		Commits []struct{ SHA string }
	}
)

func (c *Client) readCandidate(ctx context.Context, src release.Source, version, head string) release.Candidate {
	if _, _, _, ok := release.Version(version); !ok {
		return release.Candidate{}
	}
	if head == "" {
		return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: "The release branch head is unavailable."}}
	}
	base := repoPath(src.Repo)
	tags, err := c.candidateTags(ctx, base, strings.TrimPrefix(version, "v")+"-rc.")
	if err != nil {
		return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: requestProblem(err)}}
	}
	for _, tag := range tags {
		for depth := 0; tag.kind == "tag" && depth < 5; depth++ {
			var object gitTagResponse
			if err := c.get(ctx, base+"/git/tags/"+segment(tag.sha), &object); err != nil {
				return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: requestProblem(err)}}
			}
			tag.sha, tag.kind = object.Object.SHA, object.Object.Type
		}
		if tag.kind != "commit" || tag.sha == "" {
			return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: "The RC tag did not resolve to a commit."}}
		}
		endpoint := base + "/compare/" + segment(tag.sha) + "..." + segment(head)
		var comparison candidateComparison
		err := c.get(ctx, endpoint+"?per_page=100&page=1", &comparison)
		if err != nil {
			return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: requestProblem(err)}}
		}
		switch comparison.Status {
		case "behind", "diverged":
			continue
		case "ahead":
			if comparison.Ahead <= 0 {
				return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: "GitHub returned an invalid post-RC commit count."}}
			}
		case "identical":
			comparison.Ahead = 0
		default:
			return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: "GitHub returned an unknown tag ancestry status."}}
		}
		candidate := release.Candidate{
			Target: release.Target{Repo: src.Repo, Tag: tag.name}, SHA: tag.sha, Number: tag.number,
			Evidence: release.Evidence{At: time.Now(), Known: true}, Ahead: comparison.Ahead,
			CompareURL: "https://github.com/" + src.Repo + "/compare/" + tag.sha + "..." + head,
		}
		if err := c.readCandidatePRs(ctx, src, endpoint, comparison, &candidate); err != nil {
			candidate.PRError = requestProblem(err)
		}
		return candidate
	}
	return release.Candidate{Evidence: release.Evidence{At: time.Now(), Known: true}}
}

func (c *Client) candidateTags(ctx context.Context, base, prefix string) ([]candidateTag, error) {
	var tags []candidateTag
	for _, tagPrefix := range []string{prefix, "v" + prefix} {
		var refs []struct {
			Ref    string
			Object struct {
				SHA  string
				Type string
			}
		}
		if err := c.get(ctx, base+"/git/matching-refs/tags/"+segment(tagPrefix), &refs); err != nil {
			return nil, err
		}
		for _, ref := range refs {
			suffix, match := strings.CutPrefix(ref.Ref, "refs/tags/"+tagPrefix)
			number, err := strconv.ParseUint(suffix, 10, 64)
			if !match || err != nil || strconv.FormatUint(number, 10) != suffix {
				continue
			}
			tags = append(tags, candidateTag{name: tagPrefix + suffix, sha: ref.Object.SHA, kind: ref.Object.Type, number: number})
		}
	}
	slices.SortFunc(tags, func(a, b candidateTag) int {
		return cmp.Or(cmp.Compare(b.number, a.number), cmp.Compare(a.name, b.name))
	})
	return tags, nil
}

func (c *Client) readCandidatePRs(ctx context.Context, src release.Source, endpoint string, comparison candidateComparison, candidate *release.Candidate) error {
	if candidate.Ahead == 0 {
		return nil
	}
	key := src.Repo + ":" + src.Branch
	value, _ := c.candidatePRs.LoadOrStore(key, &candidatePRCache{})
	cache := value.(*candidatePRCache)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.tagSHA != candidate.SHA {
		cache.tagSHA = candidate.SHA
		cache.byCommit = make(map[string][]release.CandidatePR)
	}

	seen := make(map[int]bool)
	read := 0
	for page := 1; page <= maxPages; page++ {
		if page > 1 {
			if err := c.get(ctx, fmt.Sprintf("%s?per_page=100&page=%d", endpoint, page), &comparison); err != nil {
				return err
			}
		}
		for _, commit := range comparison.Commits {
			if commit.SHA == "" {
				return fmt.Errorf("GitHub returned a post-RC commit without an SHA")
			}
			prs, ok := cache.byCommit[commit.SHA]
			if !ok {
				var err error
				prs, err = c.candidateCommitPRs(ctx, src, commit.SHA)
				if err != nil {
					return err
				}
				cache.byCommit[commit.SHA] = prs
			}
			for _, pr := range prs {
				if !seen[pr.Number] {
					seen[pr.Number] = true
					candidate.PRs = append(candidate.PRs, pr)
				}
			}
			read++
		}
		if read == candidate.Ahead {
			return nil
		}
		if len(comparison.Commits) < 100 {
			break
		}
	}
	return fmt.Errorf("GitHub did not return every post-RC commit")
}

func (c *Client) candidateCommitPRs(ctx context.Context, src release.Source, sha string) ([]release.CandidatePR, error) {
	var prs []release.CandidatePR
	err := c.pages(ctx, repoPath(src.Repo)+"/commits/"+segment(sha)+"/pulls", func(raw json.RawMessage) error {
		var pr prResponse
		if err := json.Unmarshal(raw, &pr); err != nil {
			return err
		}
		if pr.Number > 0 && pr.MergedAt != nil && pr.Base.Ref == src.Branch {
			prs = append(prs, release.CandidatePR{
				Number: pr.Number, Title: pr.Title, URL: "https://github.com/" + src.Repo + "/pull/" + strconv.Itoa(pr.Number),
			})
		}
		return nil
	})
	return prs, err
}
