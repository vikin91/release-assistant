package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	maxCachedWindows        = 256
	maxCacheableWindowBytes = 2 << 20
)

type (
	// CommitWindowCache retains validated commit graphs for the lifetime of the process.
	CommitWindowCache struct {
		mu         sync.Mutex
		enabled    bool
		generation uint64
		windows    map[commitWindowKey][]graphCommit
		reportHits func(repo string, hits int)
	}
	commitWindowKey struct {
		origin, repo, root string
		credential         [sha256.Size]byte
	}
)

// NewCommitWindowCache starts with an empty, enabled cache.
// reportHits, when supplied, can be called concurrently for independent discoveries.
func NewCommitWindowCache(reportHits func(repo string, hits int)) *CommitWindowCache {
	return &CommitWindowCache{enabled: true, generation: 1, reportHits: reportHits}
}

// Enabled reports whether discovery can reuse commit windows.
func (c *CommitWindowCache) Enabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enabled
}

// SetEnabled discards entries when switching modes.
func (c *CommitWindowCache) SetEnabled(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enabled != enabled {
		c.enabled = enabled
		c.windows = nil
		c.generation++
	}
}

// Clear discards entries and prevents outstanding fetches from repopulating them.
func (c *CommitWindowCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.windows = nil
	c.generation++
}

func (c *CommitWindowCache) lookup(key commitWindowKey) ([]graphCommit, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.enabled {
		return nil, 0
	}
	return c.windows[key], c.generation
}

func (c *CommitWindowCache) save(key commitWindowKey, generation uint64, window []graphCommit) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	if c.windows == nil {
		c.windows = make(map[commitWindowKey][]graphCommit)
	}
	if _, exists := c.windows[key]; !exists && len(c.windows) == maxCachedWindows {
		// shortcut: arbitrary eviction bounds memory without tracking recency.
		for old := range c.windows {
			delete(c.windows, old)
			break
		}
	}
	c.windows[key] = window
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

func windowAllowsStorage(resp *http.Response) bool {
	if resp.StatusCode != http.StatusOK {
		return false
	}
	for _, value := range resp.Header.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return false
			}
		}
	}
	return true
}

func (g *commitGraph) readWindow(ctx context.Context, c *Client, repo, root string) error {
	endpoint := repoPath(repo) + "/commits?sha=" + url.QueryEscape(root) + "&per_page=100"
	var window []graphCommit
	var key commitWindowKey
	var generation uint64
	if g.cache != nil && fullCommitSHA(root) && ctx.Err() == nil {
		key = commitWindowKey{origin: c.baseURL, repo: repo, root: root, credential: sha256.Sum256([]byte(c.token))}
		cached, epoch := g.cache.lookup(key)
		generation = epoch
		if len(cached) > 0 && g.addWindow(root, cached) == nil {
			g.cacheHits++
			return nil
		}
	}
	var storable bool
	var storage *bool
	if generation != 0 {
		storage = &storable
	}
	if err := c.getObservation(ctx, endpoint, &window, storage); err != nil {
		return err
	}
	if err := g.addWindow(root, window); err != nil {
		return err
	}
	if storable && len(window) <= 100 && ctx.Err() == nil {
		g.cache.save(key, generation, window)
	}
	return nil
}
