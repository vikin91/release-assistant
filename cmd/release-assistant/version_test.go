package main

import (
	"runtime/debug"
	"testing"
)

func TestVersionFromBuildInfo(t *testing.T) {
	for name, tc := range map[string]struct {
		version, revision, want string
	}{
		"tag takes precedence": {version: "v1.2.3", revision: "123456789abc", want: "v1.2.3"},
		"tagged working copy":  {version: "v1.2.3-rc.1+dirty", revision: "123456789abc", want: "v1.2.3-rc.1"},
		"untagged commit":      {version: "v0.0.0-20261123112958-123456789abc+dirty", revision: "123456789abc", want: "v1234567"},
		"revision only":        {version: "(devel)", revision: "123456789abc", want: "v1234567"},
		"pseudo-version only":  {version: "v1.2.4-0.20261123112958-123456789abc", want: "v1234567"},
		"missing metadata":     {version: "(devel)", want: "dev version"},
	} {
		t.Run(name, func(t *testing.T) {
			info := debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: tc.revision}}}
			info.Main.Version = tc.version
			if got := versionFromBuildInfo(&info); got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
		})
	}
}
