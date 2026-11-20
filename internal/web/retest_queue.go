package web

import (
	"fmt"
	"net/url"

	"release-assistant/internal/app"
	"release-assistant/internal/release"
)

type (
	retestBucket struct {
		Label  string
		Groups []retestGroup
	}
	retestGroup struct {
		Target                    release.Target
		Label, URL, Title, Branch string
		Items                     []app.RetestItem
	}
)

// RetestBuckets groups consecutive queue items while preserving their session order.
func (p pageModel) RetestBuckets() []retestBucket {
	acs := p.Snapshot.Config.ACS().Repo
	buckets := []retestBucket{
		{Label: "ACS tags"}, {Label: "ACS PRs against the release branch"},
		{Label: "Component tags"}, {Label: "Component PRs against their release branches"},
	}
	for _, item := range p.Snapshot.RetestQueue {
		index := 0
		if item.Target.Repo != acs {
			index += 2
		}
		if item.Target.PR > 0 {
			index++
		}
		bucket := &buckets[index]
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
	return buckets
}
