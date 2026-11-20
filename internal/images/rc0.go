package images

import (
	"cmp"
	"context"
	"time"

	"release-assistant/internal/release"
)

// ExpectedRCImages lists the registry checks for an ACS major version without performing any reads.
func ExpectedRCImages(major int, tag string) []release.Image {
	return append(rcCIImages(major, tag), rcKonfluxImages(major, tag)...)
}

func inspectRC(ctx context.Context, inspector *Inspector, major int, tag string, now time.Time) []release.Image {
	rows := ExpectedRCImages(major, tag)
	for i := range rows {
		image := &rows[i]
		image.Evidence = release.Evidence{At: now, URL: sourceURL(image.Reference)}
		if inspector == nil || inspector.registry == nil {
			image.Evidence.Error = "registry client is unavailable; image availability is unknown"
			continue
		}
		present, observedAt, err := inspector.registry.present(ctx, image.Reference)
		image.Evidence.At = observedAt
		image.Evidence.Known = err == nil
		if err != nil {
			image.Evidence.Error = "registry availability is unknown: " + err.Error()
		} else {
			image.Present = present
		}
	}
	return rows
}

func rcCIImages(major int, tag string) []release.Image {
	rows := make([]release.Image, 0, len(rcCICatalog)*len(rcCIRepositoryPrefixes))
	for _, item := range rcCICatalog {
		if item.acs4Only && major != 4 {
			continue
		}
		name := cmp.Or(item.repositoryName, item.name)
		for _, prefix := range rcCIRepositoryPrefixes {
			if item.rhacsOnly && prefix != ciRepositoryPrefix {
				continue
			}
			rows = append(rows, release.Image{
				Component: "acs", Family: ciFamily, Name: item.name,
				Reference: prefix + name + ":" + item.tagPrefix + tag,
			})
		}
	}
	return rows
}

func rcKonfluxImages(major int, tag string) []release.Image {
	rows := make([]release.Image, 0, len(rcKonfluxCatalog))
	for _, item := range rcKonfluxCatalog {
		if item.acs4Only && major != 4 {
			continue
		}
		repository := cmp.Or(item.repository, konfluxRepositoryPrefix+item.name)
		rows = append(rows, release.Image{
			Component: "acs", Family: konfluxFamily, Name: item.name,
			Reference: repository + ":" + item.tagPrefix + tag + item.tagSuffix,
			BuildJob:  cmp.Or(item.job, item.name+"-on-push"),
		})
	}
	return rows
}
