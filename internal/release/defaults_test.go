package release

import "testing"

func TestDefaultsForKnownACSMinors(t *testing.T) {
	for name, tc := range map[string]struct {
		version string
		streams map[string]string
	}{
		"4.10": {"4.10.12", map[string]string{"fact": "0.2", "collector": "3.24", "scanner": "2.39"}},
		"4.11": {"4.11.3", map[string]string{"fact": "0.3", "collector": "3.25", "scanner": "2.40"}},
		"5.0":  {"5.0.1", map[string]string{"fact": "0.4", "collector": "3.26"}},
		"5.1":  {"5.1.0", map[string]string{"fact": "0.5", "collector": "3.27"}},
		"5.2":  {"5.2.0", map[string]string{"fact": "0.6", "collector": "3.28"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := make(map[string]string)
			for _, source := range DefaultsFor(tc.version) {
				got[source.Name] = source.Stream
				if source.Branch != "" {
					t.Errorf("%s default must leave the branch to the caller, got %q", source.Name, source.Branch)
				}
				if source.Repo != "stackrox/"+source.Name {
					t.Errorf("%s repository = %q", source.Name, source.Repo)
				}
			}
			if len(got) != len(tc.streams) {
				t.Fatalf("components = %v, want %v", got, tc.streams)
			}
			for component, stream := range tc.streams {
				if got[component] != stream {
					t.Errorf("%s stream = %q, want %q", component, got[component], stream)
				}
			}
		})
	}
}

func TestDefaultsForUnknownVersionsPrefillNothing(t *testing.T) {
	for name, version := range map[string]string{
		"unsupported 4.9":   "4.9.9",
		"unmapped 4.12":     "4.12.0",
		"prerelease":        "5.0.1-rc.1",
		"not a version":     "latest",
		"unsupported major": "6.0.1",
	} {
		t.Run(name, func(t *testing.T) {
			if got := DefaultsFor(version); got != nil {
				t.Fatalf("DefaultsFor(%q) = %+v, want none", version, got)
			}
		})
	}
}

func TestConfigRejectsUnsupportedACS4Minor(t *testing.T) {
	config := Config{Version: "4.9.9", Branch: "release-4.9", Components: []Source{
		{Name: "fact", Repo: "stackrox/fact", Branch: "release-0.1", Stream: "0.1"},
		{Name: "collector", Repo: "stackrox/collector", Branch: "release-3.23", Stream: "3.23"},
		{Name: "scanner", Repo: "stackrox/scanner", Branch: "release-2.38", Stream: "2.38"},
	}}
	if err := config.Validate(); err == nil {
		t.Fatal("ACS 4.9 must be rejected")
	}
	config.Version = "4.10.1"
	if err := config.Validate(); err != nil {
		t.Fatalf("ACS 4.10 must remain valid: %v", err)
	}
}
