package images

import (
	"cmp"
	"context"
	"time"

	"release-assistant/internal/release"
)

// inspectRC0 checks the fixed ACS release image families independently of component images.
func inspectRC0(ctx context.Context, inspector *Inspector, config release.Config, now time.Time) []release.Image {
	major, _, _, _ := release.Version(config.Version)
	tag := config.RC0Target().Tag
	rows := append(rcCIImages(major, tag), rcKonfluxImages(major, tag)...)
	for i := range rows {
		image := &rows[i]
		image.Evidence = release.Evidence{At: now, URL: sourceURL(image.Reference)}
		if inspector == nil || inspector.registry == nil {
			image.Evidence.Error = "registry client is unavailable; image availability is unknown"
			continue
		}
		present, err := inspector.registry.present(ctx, image.Reference)
		image.Evidence.At = time.Now()
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
	rows := make([]release.Image, 0, len(rc0CICatalog)*len(rc0CIRepositoryPrefixes))
	for _, item := range rc0CICatalog {
		if item.acs4Only && major != 4 {
			continue
		}
		name := cmp.Or(item.repositoryName, item.name)
		for _, prefix := range rc0CIRepositoryPrefixes {
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
	rows := make([]release.Image, 0, len(rc0KonfluxCatalog))
	for _, item := range rc0KonfluxCatalog {
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
