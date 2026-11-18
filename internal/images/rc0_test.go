package images

import (
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestInspectRC0UsesMajorFamilyPatternsAndNormalizedTags(t *testing.T) {
	for _, tt := range []struct {
		version   string
		scannerV2 bool
	}{
		{version: "4.10.1", scannerV2: true},
		{version: "4.11.7", scannerV2: true},
		{version: "5.0.1"},
		{version: "5.1.2"},
	} {
		t.Run(tt.version, func(t *testing.T) {
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			config := configFor(tt.version)
			tags := map[string]string{"fact": "0.3.4", "collector": "0.4.5"}
			if tt.scannerV2 {
				config.Components[2].Stream = strings.Join(strings.Split(tt.version, ".")[:2], ".")
				tags["scanner"] = config.Components[2].Stream + ".1"
			}
			inventory, err := inspector.Inspect(t.Context(), config, tags)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			tag := tt.version + "-rc.0"
			if inventory.Versions["acs"] != tag {
				t.Fatalf("inventory candidate tag = %q, want %q", inventory.Versions["acs"], tag)
			}
			want := expectedKonfluxImages(tag, tt.scannerV2)
			for reference := range expectedCIImages(tag, tt.scannerV2) {
				want[reference] = ""
			}
			assertReleaseImages(t, inventory.ReleaseImages, want)
			for _, image := range inventory.ReleaseImages {
				if image.Present != release.Yes || !image.Evidence.Fresh() || image.Evidence.URL != sourceURL(image.Reference) {
					t.Errorf("RC.0 registry result incomplete: %#v", image)
				}
			}
		})
	}
}

func TestReleaseImageCatalogsAcceptTheStableTag(t *testing.T) {
	assertReleaseImages(t, rcCIImages(4, "4.11.7"), expectedCIImages("4.11.7", true))
	assertReleaseImages(t, rcKonfluxImages(4, "4.11.7"), expectedKonfluxImages("4.11.7", true))
}

func expectedCIImages(tag string, scannerV2 bool) map[string]string {
	want := map[string]string{
		"quay.io/rhacs-eng/stackrox-operator-bundle:v" + tag: "",
		"quay.io/rhacs-eng/stackrox-operator-index:v" + tag:  "",
	}
	for _, namespace := range []string{"rhacs-eng", "stackrox-io"} {
		for _, name := range []string{"main", "central-db", "roxctl", "scanner-v4", "scanner-v4-db", "stackrox-operator", "collector", "fact"} {
			want["quay.io/"+namespace+"/"+name+":"+tag] = ""
		}
		if scannerV2 {
			for _, name := range []string{"scanner", "scanner-slim", "scanner-db", "scanner-db-slim"} {
				want["quay.io/"+namespace+"/"+name+":"+tag] = ""
			}
		}
	}
	return want
}

func expectedKonfluxImages(tag string, scannerV2 bool) map[string]string {
	want := map[string]string{
		"quay.io/rhacs-eng/release-main:" + tag:                       "main-on-push",
		"quay.io/rhacs-eng/release-central-db:" + tag:                 "central-db-on-push",
		"quay.io/rhacs-eng/release-operator:" + tag:                   "operator-on-push",
		"quay.io/rhacs-eng/release-roxctl:" + tag:                     "roxctl-on-push",
		"quay.io/rhacs-eng/release-scanner-v4:" + tag:                 "scanner-v4-on-push",
		"quay.io/rhacs-eng/release-scanner-v4-db:" + tag:              "scanner-v4-db-on-push",
		"quay.io/rhacs-eng/release-collector:" + tag:                  "retag-collector",
		"quay.io/rhacs-eng/release-fact:" + tag:                       "retag-fact",
		"quay.io/rhacs-eng/release-operator-bundle:v" + tag:           "operator-bundle-on-push",
		"quay.io/rhacs-eng/stackrox-operator-index:v" + tag + "-fast": "operator-index-on-push",
	}
	if scannerV2 {
		for _, name := range []string{"scanner", "scanner-slim", "scanner-db", "scanner-db-slim"} {
			want["quay.io/rhacs-eng/release-"+name+":"+tag] = "retag-" + name
		}
	}
	return want
}

func assertReleaseImages(t *testing.T, images []release.Image, want map[string]string) {
	t.Helper()
	for _, image := range images {
		job, ok := want[image.Reference]
		if !ok {
			t.Errorf("unexpected image %q", image.Reference)
			continue
		}
		delete(want, image.Reference)
		family := ciFamily
		if job != "" {
			family = konfluxFamily
		}
		if image.Family != family || image.BuildJob != job {
			t.Errorf("image %q has family %q and job %q, want %q and %q", image.Reference, image.Family, image.BuildJob, family, job)
		}
	}
	for reference := range want {
		t.Errorf("missing image %q", reference)
	}
}

func TestInspectRC0FailuresRemainInformationalPerRow(t *testing.T) {
	inspector := New(testHTTPClient(func(request *http.Request) *http.Response {
		if strings.Contains(request.URL.Path, "release-main") {
			return response(http.StatusForbidden, "denied", nil)
		}
		return response(http.StatusOK, validImageManifest, nil)
	}), "", "")
	inventory, err := inspector.Inspect(t.Context(), configFor("5.0.0"), map[string]string{"fact": "0.3.1", "collector": "0.4.1"})
	if err != nil {
		t.Fatalf("informational RC.0 failure affected required component inventory: %v", err)
	}
	var unknownReleaseMain, knownReleaseRoxctl bool
	for _, image := range inventory.ReleaseImages {
		if image.Name == "main" && image.Family == konfluxFamily && image.Evidence.Error != "" && image.Present == "" {
			unknownReleaseMain = true
		}
		if image.Name == "roxctl" && image.Present == release.Yes {
			knownReleaseRoxctl = true
		}
	}
	if !unknownReleaseMain || !knownReleaseRoxctl {
		t.Fatalf("informational rows lost independent results: unknownMain=%t knownRoxctl=%t", unknownReleaseMain, knownReleaseRoxctl)
	}
}
