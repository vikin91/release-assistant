package main

import (
	"regexp"
	"runtime/debug"
	"strings"
)

var pseudoVersionSuffix = regexp.MustCompile(`[.-][0-9]{14}-([0-9a-f]{12})$`)

func buildVersion() string {
	info, _ := debug.ReadBuildInfo()
	return versionFromBuildInfo(info)
}

func versionFromBuildInfo(info *debug.BuildInfo) string {
	const devVersion = "dev version"
	if info == nil {
		return devVersion
	}
	version := strings.TrimSuffix(info.Main.Version, "+dirty")
	coreVersion, _, _ := strings.Cut(version, "+")
	pseudo := pseudoVersionSuffix.FindStringSubmatch(coreVersion)
	if strings.HasPrefix(version, "v") && pseudo == nil {
		return version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" && setting.Value != "" {
			return "v" + setting.Value[:min(7, len(setting.Value))]
		}
	}
	if pseudo != nil {
		return "v" + pseudo[1][:7]
	}
	return devVersion
}
