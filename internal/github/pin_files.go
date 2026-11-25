package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

// discoverPinFiles caches only complete comparisons from the same base and head.
func (c *Client) discoverPinFiles(ctx context.Context, req app.DiscoveryRequest, prs []prResponse, listErr error, branchHead string) release.PinDiscovery {
	d := release.PinDiscovery{Files: make(map[int]release.PinFileMatch), Evidence: release.Evidence{At: time.Now(), Known: true}}
	if listErr != nil {
		d.Evidence.Error = listErr.Error()
	}
	if branchHead == "" {
		d.Evidence.Error = "Release branch head unavailable."
	}
	paths := release.PinPaths(req.Version)
	for _, pr := range prs {
		if pr.State == "closed" {
			continue
		}
		if pr.State != "open" || pr.Number <= 0 || pr.Base.Ref != req.Source.Branch || pr.Base.SHA == "" || pr.Head.SHA == "" {
			d.Evidence.Error = "Incomplete PR identity."
			continue
		}
		if cached, ok := req.PreviousPinFiles[pr.Number]; ok && cached.Head == pr.Head.SHA && cached.Base == pr.Base.SHA && (!req.ComparePinDiffs || cached.Diff != "") {
			d.Files[pr.Number] = cached
			continue
		}
		match, err := c.changedPinFiles(ctx, req.Source.Repo, pr, paths, req.ComparePinDiffs)
		if err != nil {
			d.Evidence.Error = fmt.Sprintf("PR #%d: %s", pr.Number, err)
			continue
		}
		d.Files[pr.Number] = match
	}
	return d
}

// changedPinFiles verifies that the mutable files endpoint still describes the listed revision.
func (c *Client) changedPinFiles(ctx context.Context, repo string, pr prResponse, paths map[string]string, compare bool) (release.PinFileMatch, error) {
	match := release.PinFileMatch{Head: pr.Head.SHA, Base: pr.Base.SHA}
	candidate := make(map[string]bool)
	endpoint := fmt.Sprintf("%s/pulls/%d", repoPath(repo), pr.Number)
	changed := map[string]bool{}
	count := 0
	err := c.pages(ctx, endpoint+"/files", func(raw json.RawMessage) error {
		count++
		var file struct {
			Filename string
			Previous string `json:"previous_filename"`
			Status   string `json:"status"`
			Added    *int   `json:"additions"`
			Deleted  *int   `json:"deletions"`
		}
		if err := json.Unmarshal(raw, &file); err != nil || file.Filename == "" {
			return fmt.Errorf("incomplete changed-file record")
		}
		for _, path := range paths {
			if path == file.Filename || path == file.Previous {
				changed[path] = true
				if file.Filename == path && file.Previous == "" && file.Status == "modified" &&
					(file.Added == nil || *file.Added == 1) && (file.Deleted == nil || *file.Deleted == 1) {
					candidate[path] = true
				}
			}
		}
		return nil
	})
	if err != nil {
		return match, err
	}
	if compare {
		match.Diff = "-"
		if count != 0 && count == len(candidate) {
			var raw pinPRDiff
			if err := c.get(ctx, endpoint, &raw); err != nil {
				return match, err
			}
			if !strings.HasPrefix(string(raw), "diff --git ") {
				return match, fmt.Errorf("GitHub did not return the pull request diff")
			}
			match.Diff = exactPinDiff(string(raw), candidate)
		}
	}
	var current prResponse
	if err := c.get(ctx, endpoint, &current); err != nil {
		return match, err
	}
	if current.Number != pr.Number || current.Base != pr.Base || current.Head != pr.Head || current.State != "open" {
		return match, fmt.Errorf("PR changed during file discovery")
	}
	if current.ChangedFiles != nil && *current.ChangedFiles != count {
		return match, fmt.Errorf("incomplete changed-file list")
	}
	var names []string
	for _, component := range []string{"fact", "collector", "scanner"} {
		if changed[paths[component]] {
			names = append(names, paths[component])
		}
	}
	if len(names) == 0 {
		return match, nil
	}
	last := names[len(names)-1]
	if len(names) > 1 {
		last = strings.Join(names[:len(names)-1], ", ") + " and " + last
	}
	match.Reason = "Changes " + last + "."
	return match, nil
}
