package images

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"
)

func (c *registryClient) imageCreation(ctx context.Context, repo, authorization string, data []byte, manifest imageManifest) (time.Time, string) {
	var metadata struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &metadata); err == nil {
		if created, err := time.Parse(time.RFC3339, metadata.Annotations["org.opencontainers.image.created"]); err == nil {
			return created, ""
		}
	}
	base := c.baseURL + "/v2/" + repo
	platform := ""
	if len(manifest.Manifests) > 0 {
		index := slices.IndexFunc(manifest.Manifests, func(entry manifestDescriptor) bool {
			var platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			}
			return json.Unmarshal(entry.Platform, &platform) == nil && platform.OS == "linux" && platform.Architecture == "amd64"
		})
		if index < 0 {
			return time.Time{}, ""
		}
		data := c.readImageMetadata(ctx, base+"/manifests/", authorization, manifestAccept, manifest.Manifests[index], false)
		manifest = imageManifest{}
		if json.Unmarshal(data, &manifest) != nil {
			return time.Time{}, ""
		}
		platform = "linux/amd64"
	}

	if !validImageConfigType(manifest.Config.MediaType) {
		return time.Time{}, ""
	}
	data = c.readImageMetadata(ctx, base+"/blobs/", authorization, "application/octet-stream", manifest.Config, true)
	var config struct {
		Created time.Time `json:"created"`
	}
	if json.Unmarshal(data, &config) != nil || config.Created.IsZero() {
		return time.Time{}, ""
	}
	return config.Created, platform
}

func (c *registryClient) readImageMetadata(ctx context.Context, base, authorization, accept string, descriptor manifestDescriptor, blob bool) []byte {
	if !validDescriptor(descriptor) || *descriptor.Size < 2 || *descriptor.Size > maxRegistryBody {
		return nil
	}
	response, err := c.request(ctx, base+descriptor.Digest, authorization, accept)
	if err != nil {
		return nil
	}
	body := response.Body
	defer func() { _ = body.Close() }()
	if blob && (response.StatusCode == http.StatusFound || response.StatusCode == http.StatusTemporaryRedirect) {
		dst, err := response.Location()
		if err != nil || dst.Scheme != "https" || dst.User != nil || dst.Fragment != "" || !quayCDN(dst.Host) {
			return nil
		}
		// The signed CDN URL is sufficient for the blob read.
		response, err = c.request(ctx, dst.String(), "", accept)
		if err != nil {
			return nil
		}
		defer func() { _ = response.Body.Close() }()
	}
	if response.StatusCode != http.StatusOK {
		return nil
	}
	data, err := readBody(response.Body)
	if err != nil || fmt.Sprintf("sha256:%x", sha256.Sum256(data)) != descriptor.Digest {
		return nil
	}
	return data
}

func quayCDN(host string) bool {
	switch host {
	case "cdn.quay.io", "cdn01.quay.io", "cdn02.quay.io", "cdn03.quay.io",
		"cdn04.quay.io", "cdn05.quay.io", "cdn06.quay.io":
		return true
	default:
		return false
	}
}
