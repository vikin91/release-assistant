package images

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"release-assistant/internal/release"
)

// InspectReference checks an observed build output without deriving tags or reading source files.
func (i *Inspector) InspectReference(ctx context.Context, reference string) (release.Image, error) {
	image := release.Image{Component: "acs", Family: konfluxFamily, Reference: reference, Evidence: release.Evidence{At: time.Now()}}
	repository, version, valid := splitReference(reference)
	var err error
	switch {
	case !valid:
		err = fmt.Errorf("build reported an invalid Quay image reference")
	case i == nil || i.registry == nil:
		err = fmt.Errorf("registry client is unavailable")
	default:
		image.Name = strings.TrimPrefix(path.Base(repository), "release-")
		image.Evidence.URL = "https://quay.io/v2/" + repository + "/manifests/" + url.PathEscape(version)
		image.Present, err = i.registry.present(ctx, reference)
	}
	image.Evidence.Known = err == nil
	if err != nil {
		image.Evidence.Error = err.Error()
	}
	return image, err
}
