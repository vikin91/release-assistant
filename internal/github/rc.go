package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

// maxCandidateRequests leaves room in the ACS poll for pinning and build reads.
const maxCandidateRequests = 20

type (
	candidateTag struct {
		name, sha, kind string
		number          uint64
		published       bool
	}
)

// readCandidate resolves the release tags and verifies the highest reachable RC.
func (c *Client) readCandidate(ctx context.Context, req app.DiscoveryRequest, head string) (release.Candidate, []release.Tag, error) {
	if _, _, _, ok := release.Version(req.Version); !ok {
		return release.Candidate{}, nil, nil
	}
	cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: maxDiscoveryRequests, remaining: maxDiscoveryRequests}
	ctx = context.WithValue(ctx, discoveryCycleKey{}, cycle)
	base := repoPath(req.Source.Repo)
	tags, err := c.candidateTags(ctx, base, strings.TrimPrefix(req.Version, "v"))
	if err != nil {
		return release.Candidate{Evidence: release.Evidence{At: time.Now(), Error: requestProblem(err)}}, nil, err
	}
	rc := release.Candidate{Evidence: release.Evidence{Known: true, At: time.Now()}}
	if head == "" {
		rc.Evidence.Error = "The release branch head is unavailable."
	}
	var inventory []release.Tag
	var inventoryErr error
	for _, ref := range tags {
		tag := release.Tag{Name: ref.name, Evidence: release.Evidence{At: time.Now(), URL: "https://github.com/" + req.Source.Repo + "/tree/" + ref.name}}
		tag.SHA, err = c.peelCommit(ctx, base, ref.name, ref.sha, ref.kind)
		tag.Evidence.Known = err == nil
		if err != nil {
			tag.Evidence.Error = requestProblem(err)
			inventoryErr = errors.Join(inventoryErr, err)
		}
		// Refs are ordered by descending RC number. An unreadable higher RC
		// prevents selecting a lower one until its ancestry can be verified.
		if !ref.published && rc.Target.Tag == "" && rc.Evidence.Fresh() {
			if err != nil {
				rc.Evidence = tag.Evidence
			} else {
				var candidate release.Candidate
				candidate, tag.Belongs, err = c.compareCandidate(ctx, req, ref, tag.SHA, head)
				if err != nil {
					rc.Evidence = release.Evidence{At: time.Now(), Error: requestProblem(err)}
					inventoryErr = errors.Join(inventoryErr, err)
				} else if tag.Belongs == release.Yes {
					rc = candidate
				}
			}
		}
		inventory = append(inventory, tag)
	}
	if rc.Evidence.Fresh() && (rc.Target.Tag == "" || rc.Ahead > 0) &&
		!slices.ContainsFunc(tags, func(tag candidateTag) bool {
			return tag.published || tag.number > rc.Number
		}) {
		title := fmt.Sprintf("%s-rc.%d", strings.TrimPrefix(req.Version, "v"), rc.Number+1)
		rc.NextMilestone = c.readRCMilestone(ctx, base, title)
	}
	return rc, inventory, inventoryErr
}

func (c *Client) readRCMilestone(ctx context.Context, base, title string) release.Evidence {
	evidence := release.Evidence{At: time.Now()}
	for page := 1; page <= maxPages; page++ {
		var milestones []struct {
			Title string
			State string
			URL   string `json:"html_url"`
		}
		endpoint := fmt.Sprintf("%s/milestones?state=open&per_page=100&page=%d", base, page)
		if err := c.get(ctx, endpoint, &milestones); err != nil {
			evidence.Error = requestProblem(err)
			return evidence
		}
		if milestones == nil {
			break
		}
		for _, milestone := range milestones {
			if milestone.Title == title && milestone.State == "open" && milestone.URL != "" {
				evidence.Known, evidence.URL = true, milestone.URL
				return evidence
			}
		}
		if len(milestones) < 100 {
			evidence.Known = true
			return evidence
		}
	}
	evidence.Error = "GitHub did not return a complete milestone list."
	return evidence
}

func (c *Client) compareCandidate(ctx context.Context, req app.DiscoveryRequest, ref candidateTag, sha, head string) (release.Candidate, release.Truth, error) {
	src := req.Source
	endpoint := repoPath(src.Repo) + "/compare/" + segment(sha) + "..." + segment(head)
	var comparison commitRangeComparison
	if err := c.get(ctx, endpoint+"?per_page=100&page=1", &comparison); err != nil {
		return release.Candidate{}, "", err
	}
	switch comparison.Status {
	case "behind", "diverged":
		return release.Candidate{}, release.No, nil
	case "ahead":
		if comparison.Ahead <= 0 {
			return release.Candidate{}, "", fmt.Errorf("GitHub returned an invalid post-RC commit count")
		}
	case "identical":
		comparison.Ahead = 0
	default:
		return release.Candidate{}, "", fmt.Errorf("GitHub returned an unknown tag ancestry status")
	}
	rc := release.Candidate{
		Target: release.Target{Repo: src.Repo, Tag: ref.name}, SHA: sha, Number: ref.number,
		Evidence: release.Evidence{At: time.Now(), Known: true}, Ahead: comparison.Ahead,
		CompareURL: "https://github.com/" + src.Repo + "/compare/" + sha + "..." + head,
		PRsPending: comparison.Ahead > 0,
	}
	if req.CandidateProgress != nil {
		req.CandidateProgress(head, rc)
	}
	if req.Reads(app.ReadPRs) {
		cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: maxCandidateRequests, remaining: maxCandidateRequests}
		ctx = context.WithValue(ctx, discoveryCycleKey{}, cycle)
		rc.PRsPending = false
		associations, err := c.readRangePRs(ctx, src, endpoint, rc.SHA, comparison, rc.Ahead)
		rc.PRs, rc.CommitsWithoutPR = associations.prs, associations.commitsWithoutPR
		if err != nil {
			rc.PRError = requestProblem(err)
		}
	}
	return rc, release.Yes, nil
}

// completeCandidatePRs looks up merged PRs for the range verified by tag discovery.
func (c *Client) completeCandidatePRs(ctx context.Context, src release.Source, rc release.Candidate, head string) release.Candidate {
	if !rc.Evidence.Fresh() || rc.SHA == "" || head == "" ||
		rc.CompareURL != "https://github.com/"+src.Repo+"/compare/"+rc.SHA+"..."+head {
		return release.Candidate{}
	}
	if !rc.PRsPending && rc.PRError == "" {
		return rc
	}
	cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: maxCandidateRequests, remaining: maxCandidateRequests}
	ctx = context.WithValue(ctx, discoveryCycleKey{}, cycle)
	endpoint := repoPath(src.Repo) + "/compare/" + segment(rc.SHA) + "..." + segment(head)
	var comparison commitRangeComparison
	rc.PRs, rc.CommitsWithoutPR, rc.PRError, rc.PRsPending = nil, nil, "", false
	err := c.get(ctx, endpoint+"?per_page=100&page=1", &comparison)
	if err == nil {
		var associations commitRangeAssociations
		associations, err = c.readRangePRs(ctx, src, endpoint, rc.SHA, comparison, rc.Ahead)
		rc.PRs, rc.CommitsWithoutPR = associations.prs, associations.commitsWithoutPR
	}
	if err != nil {
		rc.PRError = requestProblem(err)
	}
	return rc
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
			if !match {
				continue
			}
			if suffix == "" {
				tags = append(tags, candidateTag{name: tagPrefix, sha: ref.Object.SHA, kind: ref.Object.Type, published: true})
				continue
			}
			rawNumber, isRC := strings.CutPrefix(suffix, "-rc.")
			number, err := strconv.ParseUint(rawNumber, 10, 64)
			if !isRC || err != nil || strconv.FormatUint(number, 10) != rawNumber {
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
