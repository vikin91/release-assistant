package app

import (
	"fmt"
	"strings"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

// initializePinnedTags fills untouched choices using verified tags from the merged pinning PR.
// The caller holds the session lock; initialized choices stay protected after invalidation.
func (s *Session) initializePinnedTags() bool {
	acs := s.repos["acs"]
	pr := acs.PinningPR
	if s.config.PinningPR <= 0 || pr == nil || pr.Target.PR != s.config.PinningPR || pr.Target.Repo != s.config.ACS().Repo ||
		pr.Base != s.config.Branch || !pr.Merged || pr.MergeSHA == "" || !pr.Evidence.Fresh() || !acs.PinningPins.Evidence.Fresh() {
		return false
	}
	changed := false
	for _, source := range s.config.Components {
		if s.tagInitialized[source.Name] || s.choices.Tags[source.Name] != "" {
			continue
		}
		version := strings.TrimSpace(acs.PinningPins.Versions[source.Name])
		major, minor, _, valid := release.Version(version)
		if !valid || fmt.Sprintf("%d.%d", major, minor) != source.Stream {
			continue
		}
		repo, found := s.repos[source.Name]
		if !found || repo.Source != source {
			continue
		}
		normalized := strings.TrimPrefix(version, "v")
		var selected *release.Tag
		for _, name := range []string{version, normalized, "v" + normalized} {
			for _, tag := range repo.Tags {
				if tag.Name == name && tag.SHA != "" && tag.Belongs == release.Yes && tag.Evidence.Fresh() {
					selected = new(tag)
					break
				}
			}
			if selected != nil {
				break
			}
		}
		if selected == nil {
			continue
		}
		target := release.Target{Repo: source.Repo, Tag: selected.Name}
		s.choices.Tags[source.Name] = selected.Name
		s.confirmedSHAs[source.Name] = selected.SHA
		s.tagInitialized[source.Name] = true
		s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target, SHA: selected.SHA, Open: true, Evidence: selected.Evidence})
		s.dispatcher.Ignore(target, false)
		s.record(target, selected.SHA, "tag-from-pinning-pr", "", fmt.Sprintf("Selected from merged ACS pinning PR #%d.", pr.Target.PR), "system", fmt.Sprintf("Selected release tag %s from merged ACS pinning pull request #%d.", consoleRevision(target, selected.SHA), pr.Target.PR))
		changed = true
	}
	if changed {
		s.selectionComplete = false
		s.workRevision++
	}
	return changed
}
