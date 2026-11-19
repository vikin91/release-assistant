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
	paths := pinPaths(req.Version)
	for _, pr := range prs {
		if pr.State == "closed" {
			continue
		}
		if pr.State != "open" || pr.Number <= 0 || pr.Base.Ref != req.Source.Branch || pr.Base.SHA == "" || pr.Head.SHA == "" {
			d.Evidence.Error = "Incomplete PR identity."
			continue
		}
		if cached, ok := req.PreviousPinFiles[pr.Number]; ok && cached.Head == pr.Head.SHA && cached.Base == pr.Base.SHA {
			d.Files[pr.Number] = cached
			continue
		}
		reason, err := c.changedPinReason(ctx, req.Source.Repo, pr, paths)
		if err != nil {
			d.Evidence.Error = fmt.Sprintf("PR #%d: %s", pr.Number, err)
			continue
		}
		d.Files[pr.Number] = release.PinFileMatch{Head: pr.Head.SHA, Base: pr.Base.SHA, Reason: reason}
	}
	return d
}

// changedPinReason verifies that the mutable files endpoint still describes the listed revision.
func (c *Client) changedPinReason(ctx context.Context, repo string, pr prResponse, paths map[string]string) (string, error) {
	endpoint := fmt.Sprintf("%s/pulls/%d", repoPath(repo), pr.Number)
	changed := map[string]bool{}
	count := 0
	err := c.pages(ctx, endpoint+"/files", func(raw json.RawMessage) error {
		count++
		var file struct {
			Filename string
			Previous string `json:"previous_filename"`
		}
		if err := json.Unmarshal(raw, &file); err != nil || file.Filename == "" {
			return fmt.Errorf("incomplete changed-file record")
		}
		for _, path := range paths {
			if path == file.Filename || path == file.Previous {
				changed[path] = true
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	var current prResponse
	if err := c.get(ctx, endpoint, &current); err != nil {
		return "", err
	}
	if current.Number != pr.Number || current.Base != pr.Base || current.Head != pr.Head || current.State != "open" {
		return "", fmt.Errorf("PR changed during file discovery")
	}
	if current.ChangedFiles != nil && *current.ChangedFiles != count {
		return "", fmt.Errorf("incomplete changed-file list")
	}
	var names []string
	for _, component := range []string{"fact", "collector", "scanner"} {
		if changed[paths[component]] {
			names = append(names, paths[component])
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	last := names[len(names)-1]
	if len(names) > 1 {
		last = strings.Join(names[:len(names)-1], ", ") + " and " + last
	}
	return "Changes " + last + ".", nil
}
