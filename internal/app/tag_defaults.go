package app

import (
	"fmt"
	"strings"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

func (s *Session) componentPinnedVersion(source release.Source) string {
	acs := s.repos["acs"]
	pr := acs.PinningPR
	if s.tagInitialized[source.Name] || s.choices.Tags[source.Name] != "" ||
		s.config.PinningPR <= 0 || pr == nil || pr.Target != s.config.PinningTarget() ||
		pr.Base != s.config.Branch || !pr.Merged || pr.MergeSHA == "" || !pr.Evidence.Fresh() || !acs.PinningPins.Evidence.Fresh() {
		return ""
	}
	version := strings.TrimSpace(acs.PinningPins.Versions[source.Name])
	major, minor, _, valid := release.Version(version)
	if !valid || fmt.Sprintf("%d.%d", major, minor) != source.Stream {
		return ""
	}
	return version
}

// initializePinnedTags fills untouched choices from verified component tag evidence.
func (s *Session) initializePinnedTags() bool {
	changed := false
	for _, source := range s.config.Components {
		version := s.componentPinnedVersion(source)
		if version == "" {
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
		s.selectComponentTag(source, selected.Name, selected.SHA)
		s.polls[pollID{source.Name, pollTags}] = pollRecord{identity: selected.Name, finished: selected.Evidence.At}
		s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target, SHA: selected.SHA, Open: true, Evidence: selected.Evidence})
		s.dispatcher.Ignore(target, false)
		s.record(target, selected.SHA, "tag-from-pinning-pr", "", fmt.Sprintf("Selected from merged ACS pinning PR #%d.", s.config.PinningPR), "system", fmt.Sprintf("Selected release tag %s from merged ACS pinning pull request #%d.", consoleRevision(target, selected.SHA), s.config.PinningPR))
		changed = true
	}
	if changed {
		s.workRevision++
	}
	return changed
}
