package github

import (
	"context"
	"fmt"
	"strings"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

type pinPRDiff string

// ReadPinningDiffs refreshes the release head and open PR list before dispatch.
func (c *Client) ReadPinningDiffs(ctx context.Context, req release.PinningRequest, previous map[int]release.PinFileMatch) (release.PinDiscovery, error) {
	source := release.Source{Repo: req.Repo, Branch: req.Branch}
	prs, err := c.listOpenPRs(ctx, source)
	discovery := c.discoverPinFiles(ctx, app.DiscoveryRequest{
		Source: source, Version: req.Version, ComparePinDiffs: true, PreviousPinFiles: previous,
	}, prs, err, req.Head)
	var branch branchResponse
	endpoint := repoPath(source.Repo) + "/branches/" + segment(source.Branch)
	if err := c.get(ctx, endpoint, &branch); err != nil {
		return release.PinDiscovery{}, fmt.Errorf("could not recheck the StackRox release branch: %w", err)
	}
	if branch.Commit.SHA != req.Head {
		return release.PinDiscovery{}, fmt.Errorf("the StackRox release branch changed; review the updated pins")
	}
	return discovery, nil
}

func exactPinDiff(raw string, paths map[string]bool) string {
	if !strings.HasPrefix(raw, "diff --git ") {
		return "-"
	}
	var changes []release.PinChange
	for _, entry := range strings.Split(strings.TrimPrefix(raw, "diff --git "), "\ndiff --git ") {
		lines := strings.Split(strings.TrimSuffix(entry, "\n"), "\n")
		path, _, found := strings.Cut(strings.TrimPrefix(lines[0], "a/"), " b/")
		if !found || !paths[path] || len(lines) < 7 || lines[0] != "a/"+path+" b/"+path ||
			!strings.HasPrefix(lines[1], "index ") || !strings.HasSuffix(lines[1], " 100644") ||
			lines[2] != "--- a/"+path || lines[3] != "+++ b/"+path || lines[4] != "@@ -1 +1 @@" {
			return "-"
		}
		before, rest, ok := diffPinLine(lines[5:], "-")
		if !ok {
			return "-"
		}
		after, rest, ok := diffPinLine(rest, "+")
		if !ok || len(rest) != 0 {
			return "-"
		}
		changes = append(changes, release.PinChange{Path: path, Before: before, After: after})
		delete(paths, path)
	}
	if len(paths) != 0 {
		return "-"
	}
	return release.PinSignature(changes)
}

func diffPinLine(lines []string, prefix string) (string, []string, bool) {
	if len(lines) == 0 || !strings.HasPrefix(lines[0], prefix) {
		return "", nil, false
	}
	text := strings.TrimPrefix(lines[0], prefix)
	lines = lines[1:]
	if len(lines) > 0 && lines[0] == "\\ No newline at end of file" {
		return text, lines[1:], true
	}
	return text + "\n", lines, true
}
