package github

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type (
	tagReadMode uint8
	tagReuseKey struct{}
	tagRead     struct {
		etag, lastModified string
		data               []byte
		immutable          bool
	}
)

const (
	tagConditional tagReadMode = iota + 1
	tagImmutable
)

func (c *Client) getTagRef(ctx context.Context, endpoint string, dst any) error {
	return c.readObservation(ctx, http.MethodGet, endpoint, nil, dst, tagConditional)
}

func (c *Client) getTagSHA(ctx context.Context, endpoint string, dst any, shas ...string) error {
	if ctx.Value(tagReuseKey{}) == nil {
		return c.get(ctx, endpoint, dst)
	}
	for _, sha := range shas {
		if !fullCommitSHA(sha) {
			return c.get(ctx, endpoint, dst)
		}
	}
	return c.readObservation(ctx, http.MethodGet, endpoint, nil, dst, tagImmutable)
}

func (c *Client) rememberTagRead(endpoint string, dst any, header http.Header, mode tagReadMode) {
	switch value := dst.(type) {
	case *compareResponse:
		if !slices.Contains([]string{"ahead", "identical", "behind", "diverged"}, value.Status) {
			return
		}
	case *commitResponse:
		if value.Commit.Committer.Date.IsZero() {
			return
		}
	case *gitTagResponse:
		if !fullCommitSHA(value.Object.SHA) || (value.Object.Type != "commit" && value.Object.Type != "tag") {
			return
		}
	case *branchResponse:
		if !fullCommitSHA(value.Commit.SHA) {
			return
		}
	}
	entry := tagRead{etag: header.Get("ETag"), lastModified: header.Get("Last-Modified"), immutable: mode == tagImmutable}
	for _, value := range header.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			name, _, _ := strings.Cut(directive, "=")
			if strings.EqualFold(strings.TrimSpace(name), "no-cache") {
				entry.immutable = false
			}
		}
	}
	if !entry.immutable && (c.token == "" || (entry.etag == "" && entry.lastModified == "")) {
		return
	}
	data, err := json.Marshal(dst)
	if err != nil || len(data) > 128<<10 {
		return
	}
	entry.data = data
	c.saveTagRead(endpoint, entry)
}

func (c *Client) saveTagRead(endpoint string, entry tagRead) {
	const maxTagReads = 512
	c.tagReadsMu.Lock()
	defer c.tagReadsMu.Unlock()
	if len(entry.data) == 0 {
		delete(c.tagReads, endpoint)
		return
	}
	if c.tagReads == nil {
		c.tagReads = make(map[string]tagRead)
	}
	if _, exists := c.tagReads[endpoint]; !exists && len(c.tagReads) == maxTagReads {
		// shortcut: arbitrary eviction bounds memory without tracking recency.
		for old := range c.tagReads {
			delete(c.tagReads, old)
			break
		}
	}
	c.tagReads[endpoint] = entry
}

// ReadComponentTagNames lists final versions in the stream with two prefix reads.
func (c *Client) ReadComponentTagNames(ctx context.Context, source release.Source) ([]string, error) {
	var names []string
	major, minor, _, ok := release.Version(source.Stream + ".0")
	if !ok {
		return nil, fmt.Errorf("invalid component release stream")
	}
	for _, prefix := range []string{source.Stream + ".", "v" + source.Stream + "."} {
		var refs []struct{ Ref string }
		endpoint := repoPath(source.Repo) + "/git/matching-refs/tags/" + segment(prefix)
		if err := c.getTagRef(ctx, endpoint, &refs); err != nil {
			return nil, err
		}
		if refs == nil {
			return nil, fmt.Errorf("GitHub returned an invalid tag list")
		}
		for _, ref := range refs {
			name, match := strings.CutPrefix(ref.Ref, "refs/tags/")
			m, n, _, final := release.Version(name)
			if match && final && m == major && n == minor {
				names = append(names, name)
			}
		}
	}
	slices.SortFunc(names, func(a, b string) int {
		if versionOrder := compareTagNames(b, a); versionOrder != 0 {
			return versionOrder
		}
		return strings.Compare(b, a)
	})
	return slices.Compact(names), nil
}

// fullCommitSHA excludes branch names, tags and abbreviated object IDs.
func fullCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	var digest [20]byte
	_, err := hex.Decode(digest[:], []byte(value))
	return err == nil
}

func cacheControlAllowsStorage(header http.Header) bool {
	for _, value := range header.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return false
			}
		}
	}
	return true
}
