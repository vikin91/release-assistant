package images

import (
	"cmp"

	"release-assistant/internal/release"
)

// ExpectedRCImages lists the registry checks for an ACS major version without performing any reads.
func ExpectedRCImages(major int, tag string) []release.Image {
	return append(rcCIImages(major, tag), rcKonfluxImages(major, tag)...)
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
