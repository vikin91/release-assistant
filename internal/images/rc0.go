package images

import (
	"context"
	"time"

	"release-assistant/internal/release"
)

// inspectRC0 checks the fixed ACS release image families independently of component images.
func inspectRC0(ctx context.Context, inspector *Inspector, config release.Config, now time.Time) []release.Image {
	major, _, _, _ := release.Version(config.Version)
	tag := config.RC0Target().Tag
	names := rc0ImageNames[major]
	rows := make([]release.Image, 0, len(names)*len(imageFamilies))
	for _, name := range names {
		for _, family := range imageFamilies {
			reference := family.repositoryPrefix + name + ":" + tag + family.tagSuffix
			image := release.Image{
				Component: "acs", Family: family.family, Name: name, Reference: reference,
				Evidence: release.Evidence{At: now, URL: sourceURL(reference)},
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
