package images

import (
	"net/http"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestInspectRC0UsesMajorFamilyPatternsAndNormalizedTags(t *testing.T) {
	for _, tt := range []struct {
		version string
		count   int
		names   []string
	}{
		{version: "4.10.1", count: 10, names: []string{"main", "roxctl", "central-db", "scanner-v4", "scanner-v4-db"}},
		{version: "4.11.0", count: 10, names: []string{"main", "roxctl", "central-db", "scanner-v4", "scanner-v4-db"}},
		{version: "4.8.7", count: 10, names: []string{"main", "roxctl", "central-db", "scanner-v4", "scanner-v4-db"}},
		{version: "5.0.1", count: 6, names: []string{"main", "roxctl", "scanner-v4"}},
		{version: "5.1.2", count: 6, names: []string{"main", "roxctl", "scanner-v4"}},
	} {
		t.Run(tt.version, func(t *testing.T) {
			inspector := New(testHTTPClient(func(*http.Request) *http.Response {
				return response(http.StatusOK, validImageManifest, nil)
			}), "", "")
			config := configFor(tt.version)
			if strings.HasPrefix(tt.version, "4.") {
				config.Components[2].Stream = strings.Join(strings.Split(tt.version, ".")[:2], ".")
			}
			tags := map[string]string{"fact": "0.3.4", "collector": "0.4.5"}
			if len(config.Components) == 3 {
				tags["scanner"] = config.Components[2].Stream + ".1"
			}
			inventory, err := inspector.Inspect(t.Context(), config, tags)
			if err != nil {
				t.Fatalf("Inspect() error = %v", err)
			}
			if len(inventory.ReleaseImages) != tt.count || inventory.Versions["acs"] != tt.version+"-rc.0" {
				t.Fatalf("RC.0 rows=%d ACS tag=%q, want rows=%d tag=%s-rc.0", len(inventory.ReleaseImages), inventory.Versions["acs"], tt.count, tt.version)
			}
			want := make(map[string]bool)
			for _, name := range tt.names {
				want["ci/"+name] = false
				want["konflux/"+name] = false
			}
			for _, image := range inventory.ReleaseImages {
				key := image.Family + "/" + image.Name
				if _, ok := want[key]; !ok {
					t.Errorf("unexpected RC.0 image row %q", key)
				}
				want[key] = true
				expected := "quay.io/rhacs-eng/" + image.Name + ":" + tt.version + "-rc.0"
				if image.Family == konfluxFamily {
					expected = "quay.io/rhacs-eng/release-" + image.Name + ":" + tt.version + "-rc.0-fast"
				}
				if image.Reference != expected {
					t.Errorf("RC.0 reference = %q, want %q", image.Reference, expected)
				}
				if image.Present != release.Yes || !image.Evidence.Fresh() || image.DefinitionURL == "" {
					t.Errorf("RC.0 registry result incomplete: %#v", image)
				}
			}
			for key, found := range want {
				if !found {
					t.Errorf("missing RC.0 image row %q", key)
				}
			}
		})
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
