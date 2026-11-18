package images

import (
	"context"
	"time"

	"release-assistant/internal/release"
)

// inspectRC0 checks the fixed ACS release image families independently of component images.
func inspectRC0(ctx context.Context, inspector *Inspector, config release.Config, revision string, now time.Time) []release.Image {
	major, _, _, _ := release.Version(config.Version)
	tag := config.RC0Target().Tag
	names := rc0Names(major)
	rows := make([]release.Image, 0, len(names)*2)
	for _, name := range names {
		for _, family := range []string{ciFamily, konfluxFamily} {
			repository := "quay.io/rhacs-eng/" + name
			file := "scripts/ci/lib.sh"
			if family == konfluxFamily {
				repository = "quay.io/rhacs-eng/release-" + name
				file = ".tekton/" + name + "-build.yaml"
			}
			reference := repository + ":" + tag
			if family == konfluxFamily {
				reference += "-fast"
			}
			definition := definitionURL(revision, file)
			image := release.Image{
				Component: "acs", Family: family, Name: name, Reference: reference,
				DefinitionURL: definition, Evidence: release.Evidence{At: now, URL: definition},
			}
			if inspector == nil || inspector.registry == nil {
				image.Evidence.Error = "registry client is unavailable; image availability is unknown"
			} else {
				present, err := inspector.registry.present(ctx, reference)
				image.Evidence = release.Evidence{At: time.Now(), URL: sourceURL(reference), Known: err == nil}
				if err != nil {
					image.Evidence.Error = "registry availability is unknown: " + err.Error()
				} else {
					image.Present = present
				}
			}
			rows = append(rows, image)
		}
	}
	return rows
}

func rc0Names(major int) []string {
	if major == 5 {
		return []string{"main", "roxctl", "scanner-v4"}
	}
	return []string{"main", "roxctl", "central-db", "scanner-v4", "scanner-v4-db"}
}
