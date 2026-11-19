package github

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"release-assistant/internal/app"
	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type tagInventory struct {
	mu          sync.Mutex
	head        string
	page, index int
	items       []tagResponse
	tags        []resolvedTag
	issues      []error
	complete    bool
}

// tagBatch resumes a bounded inventory without revalidating earlier tags in the series.
// A branch change starts a new series because ancestry is bound to its head.
func (c *Client) tagBatch(ctx context.Context, src release.Source, head string, publish func([]resolvedTag)) ([]resolvedTag, bool, error) {
	key := src.Repo + "/" + src.Branch + "/" + src.Stream
	value, _ := c.inventories.LoadOrStore(key, &tagInventory{})
	inventory := value.(*tagInventory)
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	if inventory.head != head || inventory.complete {
		inventory.head, inventory.page, inventory.index = head, 1, 0
		inventory.items, inventory.tags, inventory.issues = nil, nil, nil
		inventory.complete = false
	}
	cycle := ctx.Value(discoveryCycleKey{}).(*discoveryCycle)
	stopAt := cycle.remaining - app.TagBatchRequests
	for cycle.remaining-max(stopAt, 0) >= 8 {
		if inventory.items == nil {
			var items []tagResponse
			endpoint := fmt.Sprintf("%s/tags?per_page=100&page=%d", repoPath(src.Repo), inventory.page)
			if err := c.get(ctx, endpoint, &items); err != nil {
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
