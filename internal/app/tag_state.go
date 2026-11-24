package app

import (
	"fmt"
	"maps"
	"strings"

	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

// saveComponentTag preserves an explicit clear so pinning defaults cannot override it.
func (s *ReleaseState) saveComponentTag(version string, source release.Source, tag, sha string) error {
	if s == nil {
		return nil
	}
	version = strings.TrimPrefix(version, "v")
	tags := make(map[string]savedComponentTag, len(s.file.ComponentTags[version])+1)
	maps.Copy(tags, s.file.ComponentTags[version])
	tags[source.Name] = savedComponentTag{Repo: source.Repo, Branch: source.Branch, Tag: tag, SHA: sha}
	next := s.file
	next.ComponentTags = make(map[string]map[string]savedComponentTag, len(s.file.ComponentTags)+1)
	maps.Copy(next.ComponentTags, s.file.ComponentTags)
	next.ComponentTags[version] = tags
	return s.commit(next)
}

func (s *releaseWork) restoreComponentTags(state *ReleaseState) {
	if state == nil {
		return
	}
	tags := state.file.ComponentTags[strings.TrimPrefix(s.config.Version, "v")]
	for _, source := range s.config.Components {
		saved, ok := tags[source.Name]
		if !ok || saved.Repo != source.Repo || saved.Branch != source.Branch {
			continue
		}
		if saved.Tag != "" {
			major, minor, _, final := release.Version(saved.Tag)
			if !final || fmt.Sprintf("%d.%d", major, minor) != source.Stream || saved.SHA == "" {
				continue
			}
			s.pendingTags[source.Name] = saved
		}
		s.tagInitialized[source.Name] = true
	}
}

// verifySavedTag requires fresh ancestry and the original commit before enabling tag monitoring.
func (s *Session) verifySavedTag(source release.Source, tags []release.Tag) bool {
	saved := s.pendingTags[source.Name]
	if saved.Tag == "" {
		return false
	}
	for _, tag := range tags {
		if tag.Name != saved.Tag || tag.SHA == "" || !tag.Evidence.Fresh() {
			continue
		}
		if tag.SHA == saved.SHA && tag.Belongs == "" {
			continue
		}
		delete(s.pendingTags, source.Name)
		target := release.Target{Repo: source.Repo, Tag: tag.Name}
		switch {
		case tag.SHA != saved.SHA:
			s.record(target, tag.SHA, "tag-invalidated", "", "resolved commit changed", "system")
		case tag.Belongs == release.Yes:
			s.selectComponentTag(source, tag.Name, tag.SHA)
			s.dispatcher.ObserveIdentity(dispatch.Identity{Target: target, SHA: tag.SHA, Open: true, Evidence: tag.Evidence})
		default:
			message := fmt.Sprintf("Saved tag %s is outside %s. Select a tag again.", target, source.Branch)
			s.record(target, tag.SHA, "tag-invalidated", "", message, "system", message)
		}
		s.workRevision++
		return true
	}
	return false
}
