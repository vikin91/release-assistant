package images

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func TestScannerImageCreationFollowsCurrentManifest(t *testing.T) {
	const (
		reference = "quay.io/rhacs-eng/scanner:2.39.28"
		root      = "/v2/rhacs-eng/scanner"
	)
	var currentConfig, currentDigest, previousDigest string
	inspector := New(testHTTPClient(func(req *http.Request) *http.Response {
		if req.URL.Path == "/v2/auth" {
			return response(http.StatusOK, `{"token":"pull-token"}`, nil)
		}
		configDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(currentConfig)))
		child := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json",
			"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`,
			configDigest, len(currentConfig))
		childDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(child)))
		if req.URL.Host == "cdn01.quay.io" {
			if req.Header.Get("Authorization") != "" {
				t.Error("the registry token reached the CDN")
			}
			return response(http.StatusOK, currentConfig, nil)
		}
		if !strings.HasPrefix(req.URL.Path, root+"/") {
			return response(http.StatusOK, validImageManifest, nil)
		}
		if req.Header.Get("Authorization") != "Bearer pull-token" {
			return response(http.StatusUnauthorized, "", http.Header{
				"Www-Authenticate": {`Bearer realm="https://quay.io/v2/auth",service="quay.io",scope="repository:rhacs-eng/scanner:pull"`},
			})
		}
		switch req.URL.Path {
		case root + "/manifests/2.39.28":
			index := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json",
				"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":%q,"size":%d,
				"platform":{"os":"linux","architecture":"amd64"}}]}`, childDigest, len(child))
			currentDigest = fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(index)))
			return response(http.StatusOK, index, nil)
		case root + "/manifests/" + childDigest:
			return response(http.StatusOK, child, nil)
		case root + "/blobs/" + configDigest:
			return response(http.StatusFound, "", http.Header{"Location": {"https://cdn01.quay.io/signed"}})
		default:
			t.Errorf("unexpected registry request %s", req.URL.Path)
			return response(http.StatusNotFound, "", nil)
		}
	}), "reader", "password")

	config := configFor("4.10.12")
	config.Components[2].Stream = "2.39"
	tags := map[string]string{"scanner": "2.39.28"}
	for n, step := range []struct {
		created string
		force   bool
	}{
		{created: "2026-11-24T13:20:00Z"},
		{created: "2026-11-25T11:50:00Z"},
		{created: "2026-11-25T11:50:00Z", force: true},
		{force: true},
	} {
		currentConfig = fmt.Sprintf(`{"created":%q}`, step.created)
		inventory, err := inspector.InspectImages(t.Context(), config, tags, step.force, nil)
		if err != nil {
			t.Fatal(err)
		}
		index := slices.IndexFunc(inventory.Images, func(image release.Image) bool { return image.Reference == reference })
		if index < 0 {
			t.Fatalf("scanner image %s missing", reference)
		}
		image := inventory.Images[index]
		expected, _ := time.Parse(time.RFC3339, step.created)
		if image.ManifestDigest != currentDigest || (n == 1 && image.ManifestDigest == previousDigest) {
			t.Fatalf("manifest digest should match the current image: %+v", image)
		}
		previousDigest = image.ManifestDigest
		if image.Present != release.Yes || !image.Evidence.Fresh() || !image.CreatedAt.Equal(expected) ||
			(!expected.IsZero() && image.CreatedPlatform != "linux/amd64") {
			t.Errorf("force=%t: got %s %q, want %s and verified presence: %+v",
				step.force, image.CreatedAt, image.CreatedPlatform, expected, image)
		}
	}
}
