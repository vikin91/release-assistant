package web

import (
	"fmt"
	"iter"
	"net/url"
	"slices"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

type (
	retestBucket struct {
		Source release.Source
		Groups []retestGroup
	}
	retestGroup struct {
		Target                    release.Target
		Label, URL, Title, Branch string
		Items                     []app.RetestItem
	}
)

// RetestLogNewestFirst presents the latest recorded decisions at the top of the queue log.
func (p pageModel) RetestLogNewestFirst() iter.Seq2[int, app.RetestDecision] {
	return slices.Backward(p.Snapshot.RetestLog)
}

// RetestBuckets groups retries by repository while preserving the queue order within each table.
func (p pageModel) RetestBuckets() []retestBucket {
	buckets := []retestBucket{{Source: p.Snapshot.Config.ACS()}}
	for _, tab := range p.ComponentPRTabs() {
		buckets = append(buckets, retestBucket{Source: p.ConfiguredSources[prComponent(tab)]})
	}
	other := retestBucket{Source: release.Source{Name: "other repositories"}}
	for _, item := range p.Snapshot.RetestQueue {
		bucket := &other
		for i := range buckets {
			source := buckets[i].Source
			if source.Repo != item.Target.Repo {
				continue
			}
			if bucket == &other {
				bucket = &buckets[i]
			}
			if i == 0 || source.Branch == item.Branch {
				bucket = &buckets[i]
				break
			}
		}
		n := len(bucket.Groups)
		if n == 0 || bucket.Groups[n-1].Target != item.Target {
			repoURL := "https://github.com/" + item.Target.Repo
			label, link := "Tag "+item.Target.Tag, repoURL+"/tree/refs/tags/"+url.PathEscape(item.Target.Tag)
			if item.Target.PR > 0 {
				label, link = fmt.Sprintf("PR #%d", item.Target.PR), fmt.Sprintf("%s/pull/%d", repoURL, item.Target.PR)
			}
			bucket.Groups = append(bucket.Groups, retestGroup{
				Target: item.Target, Label: label, URL: link, Title: item.Title, Branch: item.Branch,
			})
			n++
		}
		group := &bucket.Groups[n-1]
		group.Items = append(group.Items, item)
	}
	if len(other.Groups) > 0 {
		buckets = append(buckets, other)
	}
	return buckets
}
