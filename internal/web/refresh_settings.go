package web

import (
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/app"
)

type (
	refreshSection struct {
		Title, Help string
		Groups      []refreshGroup
	}
	refreshGroup struct {
		Title  string
		Fields []refreshField
	}
	refreshField struct {
		Key         app.RefreshKey
		Label, Help string
	}
)

func settingsSection(value string) string {
	switch value {
	case "retry-settings", "github-read-settings", "refresh-settings", "tag-choice-settings", "startup-source-read-settings":
		return value
	default:
		return ""
	}
}

func refreshSections() []refreshSection {
	const (
		componentPRHelp    = "Find PRs targeting the component release branch. Update each PR’s state to open, merged, or closed."
		componentCheckHelp = "Refresh GitHub CI results for tracked component PRs."
	)
	return []refreshSection{
		{Title: "Components", Help: "Applies to Collector, Fact, and Scanner (ACS 4.x). Each component follows its own tag selection.", Groups: []refreshGroup{
			{Title: "Before tag selection", Fields: []refreshField{
				{app.BeforePRs, "Component PRs", componentPRHelp},
				{app.BeforePRChecks, "Component PR checks", componentCheckHelp},
				{app.BeforeTags, "Saved component tag", "Verify a tag restored from saved settings or named in merged ACS pins."},
			}},
			{Title: "After tag selection", Fields: []refreshField{
				{app.AfterPRs, "Component PRs", componentPRHelp},
				{app.AfterPRChecks, "Component PR checks", componentCheckHelp},
				{app.AfterTags, "Selected component tag", "Verify the selected tag and retry an incomplete list of PRs between tags."},
				{app.ComponentTagChecks, "Component tag checks", "Refresh GitHub CI results for the selected component tag."},
			}},
		}},
		{Title: "ACS", Help: "Track the ACS release branch, release candidates, builds, and images.", Groups: []refreshGroup{
			{Fields: []refreshField{
				{app.ACSPRs, "ACS release PRs", "Find PRs targeting the ACS release branch. Update each PR’s state to open, merged, or closed."},
				{app.ACSPRChecks, "ACS PR checks", "Refresh GitHub CI results for tracked ACS PRs."},
				{app.ACSTags, "ACS tags", "Find ACS release tags, including rc.0, rc.1, and later candidates. Resolve their commit SHAs."},
				{app.ACSTagChecks, "ACS tag checks", "Refresh GitHub CI results for the current release candidate. Check each new candidate immediately."},
				{app.KonfluxBuilds, "Konflux builds", "Refresh Konflux build status for tracked component and ACS PRs and tags."},
				{app.ReleaseImages, "Release images", "Check Quay for images for selected component tags and the current ACS release candidate."},
			}},
		}},
	}
}

// wholeMinutes leaves the field empty for fractional CLI durations so
// an unrelated settings save cannot round them to a different interval.
func wholeMinutes(d time.Duration) string {
	if d%time.Minute != 0 {
		return ""
	}
	return strconv.FormatInt(int64(d/time.Minute), 10)
}

func submittedIntervals(form url.Values) (app.RefreshIntervals, error) {
	intervals := app.RefreshIntervals{}
	for field, values := range form {
		name, ok := strings.CutPrefix(field, "refresh_")
		if !ok {
			continue
		}
		if len(values) != 1 {
			return nil, fmt.Errorf("submit one value for refresh interval %q", name)
		}
		if values[0] == "" {
			continue
		}
		minutes, err := strconv.ParseUint(values[0], 10, 64)
		if err != nil || minutes == 0 {
			return nil, fmt.Errorf("refresh interval %q must be a positive whole number of minutes", name)
		}
		if minutes > math.MaxInt64/uint64(time.Minute) {
			return nil, fmt.Errorf("refresh interval %q is too large", name)
		}
		intervals[app.RefreshKey(name)] = time.Duration(minutes) * time.Minute
	}
	return intervals, intervals.Validate()
}
