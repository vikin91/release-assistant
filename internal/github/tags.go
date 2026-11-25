package github

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"release-assistant/internal/apilog"
	"release-assistant/internal/app"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

const tagPageReuse = time.Minute

type (
	tagPage struct {
		items   []tagResponse
		release string
		at      time.Time
	}
	tagInventory struct {
		mu          sync.Mutex
		head        string
		refresh     string
		freshPages  bool
		page, index int
		items       []tagResponse
		tags        []resolvedTag
		issues      []error
		complete    bool
	}
)

// tagBatch resumes a bounded inventory without revalidating earlier tags in the series.
// A branch change starts a new series because ancestry is bound to its head.
func (c *Client) tagBatch(ctx context.Context, src release.Source, head, refresh string, publish func([]resolvedTag)) ([]resolvedTag, bool, error) {
	key := src.Repo + "/" + src.Branch + "/" + src.Stream
	value, _ := c.inventories.LoadOrStore(key, &tagInventory{})
	inventory := value.(*tagInventory)
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	restart := refresh != "" && refresh != inventory.refresh
	if inventory.head != head || inventory.complete || restart {
		inventory.freshPages = restart || (!inventory.complete && inventory.freshPages)
		if restart {
			inventory.refresh = refresh
		}
		inventory.head, inventory.page, inventory.index = head, 1, 0
		inventory.items, inventory.tags, inventory.issues = nil, nil, nil
		inventory.complete = false
	}
	cycle := ctx.Value(discoveryCycleKey{}).(*discoveryCycle)
	stopAt := cycle.remaining - app.TagBatchRequests
	for cycle.remaining-max(stopAt, 0) >= 8 {
		if inventory.items == nil {
			items, err := c.tagListPage(ctx, src.Repo, inventory.page, inventory.freshPages)
			if err != nil {
				return inventory.tags, true, errors.Join(append(inventory.issues, err)...)
			}
			inventory.items = items
			inventory.index = 0
		}
		for inventory.index < len(inventory.items) && cycle.remaining-max(stopAt, 0) >= 8 {
			name := inventory.items[inventory.index].Name
			major, minor, _, ok := release.Version(name)
			sm, sn, _, streamOK := release.Version(src.Stream + ".0")
			if ok && streamOK && major == sm && minor == sn {
				tag, err := c.resolveTag(ctx, src, name, head)
				_, deferred := errors.AsType[*githubread.DeferredError](err)
				if err != nil && (ctx.Err() != nil || deferred || cycle.budgetErr != nil || cycle.rateErr != nil) {
					return inventory.tags, true, errors.Join(append(inventory.issues, err)...)
				}
				if err != nil {
					inventory.issues = append(inventory.issues, err)
				}
				if tag.observation.Name != "" {
					inventory.tags = append(inventory.tags, tag)
					publish(inventory.tags)
				}
			}
			inventory.index++
		}
		if inventory.index < len(inventory.items) {
			break
		}
		if len(inventory.items) < 100 {
			inventory.complete = true
			break
		}
		if inventory.page == maxPages {
			inventory.issues = append(inventory.issues, fmt.Errorf("GitHub pagination limit reached (limit %d pages)", maxPages))
			inventory.complete = true
			break
		}
		inventory.page++
		inventory.items = nil
	}
	return inventory.tags, !inventory.complete, errors.Join(inventory.issues...)
}

// tagListPage can reuse the repo-wide inventory from another release's recent read.
// The branch-specific tag resolution still verifies the ref and ancestry.
func (c *Client) tagListPage(ctx context.Context, repo string, page int, fresh bool) ([]tagResponse, error) {
	endpoint := fmt.Sprintf("%s/tags?per_page=100&page=%d", repoPath(repo), page)
	version := apilog.Release(ctx)
	if version != "" && !fresh {
		c.tagPagesMu.Lock()
		cached := c.tagPages[endpoint]
		c.tagPagesMu.Unlock()
		if cached.release != version && time.Since(cached.at) < tagPageReuse {
			return cached.items, nil
		}
	}
	var items []tagResponse
	if err := c.get(ctx, endpoint, &items); err != nil {
		return nil, err
	}
	if version != "" {
		now := time.Now()
		c.tagPagesMu.Lock()
		if c.tagPages == nil {
			c.tagPages = make(map[string]tagPage)
		}
		maps.DeleteFunc(c.tagPages, func(_ string, p tagPage) bool { return now.Sub(p.at) >= tagPageReuse })
		c.tagPages[endpoint] = tagPage{items: items, at: now, release: version}
		c.tagPagesMu.Unlock()
	}
	return items, nil
}
