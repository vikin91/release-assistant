package images

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"release-assistant/internal/release"
)

func tagResponse(tag, digest string, start int64) *http.Response {
	return response(http.StatusOK, fmt.Sprintf(`{"tags":[{"name":%q,"manifest_digest":%q,"start_ts":%d,"end_ts":null}]}`, tag, digest, start), nil)
}

func availableComponentResponse(request *http.Request) *http.Response {
	if strings.HasPrefix(request.URL.Path, "/api/v1/") {
		return tagResponse(request.URL.Query().Get("specificTag"), "sha256:"+strings.Repeat("a", 64), 1795615650)
	}
	return response(http.StatusOK, validImageManifest, nil)
}

func TestActiveTagMappingUsesOnlyExactActiveRecord(t *testing.T) {
	const tag = "2.39.28"
	digest := "sha256:" + strings.Repeat("a", 64)
	for name, fixture := range map[string]struct {
		records string
		status  release.Truth
		err     bool
	}{
		"exact active tag": {records: fmt.Sprintf(`
			{"name":"2.39.28","manifest_digest":"superseded","start_ts":100,"end_ts":200},
			{"name":"2.39.28-other","manifest_digest":"different","start_ts":201,"end_ts":null},
			{"name":"2.39.28","manifest_digest":%q,"start_ts":200,"end_ts":null}`, digest), status: release.Yes},
		"no active tag":         {status: release.No},
		"invalid active tag":    {records: `{"name":"2.39.28","manifest_digest":"invalid","start_ts":200,"end_ts":null}`, err: true},
		"duplicate active tags": {records: strings.TrimSuffix(strings.Repeat(fmt.Sprintf(`{"name":"2.39.28","manifest_digest":%q,"start_ts":200},`, digest), 2), ","), err: true},
	} {
		t.Run(name, func(t *testing.T) {
			i := New(testHTTPClient(func(req *http.Request) *http.Response {
				if req.Method != http.MethodGet || req.URL.Path != "/api/v1/repository/stackrox-io/scanner/tag/" ||
					req.URL.Query().Get("specificTag") != tag || req.URL.Query().Get("onlyActiveTags") != "true" ||
					req.Header.Get("Authorization") != "" {
					t.Errorf("unexpected public tag request %s %s", req.Method, req.URL)
				}
				return response(http.StatusOK, `{"tags":[`+fixture.records+`]}`, nil)
			}), "private-user", "private-password").WithQuayAPIToken("private-api-token")
			image, err := i.observeImage(t.Context(), release.Image{Component: "scanner", Family: release.ImagePublic, Reference: "quay.io/stackrox-io/scanner:" + tag}, false)
			if (err != nil) != fixture.err || image.Present != fixture.status {
				t.Fatalf("got image %+v, error %v", image, err)
			}
			if fixture.status == release.Yes && (image.TagDigest != digest || !image.TagUpdatedAt.Equal(time.Unix(200, 0))) {
				t.Fatalf("incorrect tag mapping: %+v", image)
			}
		})
	}
}

func TestPrivateTagMappingRefreshesOnEveryPoll(t *testing.T) {
	const tag = "2.39.28"
	current := validImageManifest
	var updated int64 = 1794589039
	var mismatch bool
	i := New(testHTTPClient(func(req *http.Request) *http.Response {
		if strings.HasPrefix(req.URL.Path, "/api/v1/") {
			if req.Header.Get("Authorization") != "Bearer private-api-token" || req.URL.Query().Get("specificTag") != tag {
				t.Errorf("private tag API did not receive the scoped token and tag")
			}
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(current)))
			if mismatch {
				digest = "sha256:" + strings.Repeat("f", 64)
			}
			return tagResponse(tag, digest, updated)
		}
		if req.Header.Get("Authorization") != "" {
			t.Errorf("Quay API token was sent to the registry")
		}
		return response(http.StatusOK, current, nil)
	}), "", "").WithQuayAPIToken("private-api-token")
	ref := release.Image{Component: "scanner", Family: "ci", Reference: "quay.io/rhacs-eng/scanner:" + tag}
	for step := range 3 {
		image, err := i.observeImage(t.Context(), ref, false)
		if err != nil || image.Present != release.Yes {
			t.Fatalf("manifest should verify availability: %+v, %v", image, err)
		}
		if step < 2 && (!image.TagUpdatedAt.Equal(time.Unix(updated, 0)) || image.TagDigest != fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(current)))) {
			t.Fatalf("got stale or incorrect current tag mapping: %+v", image)
		}
		if step == 2 && (!image.TagUpdatedAt.IsZero() || image.TagError == "") {
			t.Fatalf("different manifest and tag digests should leave age unverified: %+v", image)
		}
		current = strings.ReplaceAll(current, strings.Repeat("a", 64), strings.Repeat("b", 64))
		updated = 1795615650
		mismatch = step == 1
	}
}
