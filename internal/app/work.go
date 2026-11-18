package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"release-assistant/internal/githubread"
	"release-assistant/internal/release"
)

type sourceWork struct {
	revision                     uint64
	scope                        DiscoveryScope
	plans, images, releaseImages bool
	tagInventory                 bool
}

func (s *Session) rcReady() bool {
	return rcImageWaitReason(s.config, s.choices, s.repos["acs"]) == ""
}

// RCImageWaitReason describes the first prerequisite preventing RC.0 image checks.
func (s Snapshot) RCImageWaitReason() string {
	for _, repo := range s.Repositories {
		if repo.Source.Name == "acs" {
			return rcImageWaitReason(s.Config, s.Choices, repo)
		}
	}
	return rcImageWaitReason(s.Config, s.Choices, release.Repository{})
}

func rcImageWaitReason(config release.Config, choices release.Choices, acs release.Repository) string {
	if config.PinningPR == 0 {
		return "Add the pinning PR number in Release configuration to start verification."
	}
	p := acs.PinningPR
	switch {
	case p == nil || p.Target != config.PinningTarget() || !p.Evidence.Fresh() || !acs.PinningEvidence.Fresh():
		return "Waiting to verify the pinning PR."
	case p.Base != config.Branch:
		return "The pinning PR targets a different branch."
	case !p.Merged && p.State == "closed":
		return "The pinning PR closed without merging."
	case !p.Merged:
		return "Waiting for the pinning PR to merge."
	case p.MergeSHA == "":
		return "Waiting to verify the pinning PR merge commit."
	case !acs.Pins.Evidence.Fresh():
		return "Waiting to verify the component pins on the ACS release branch."
	}
	for _, source := range config.Components {
		if strings.TrimPrefix(acs.Pins.Versions[source.Name], "v") != strings.TrimPrefix(choices.Tags[source.Name], "v") {
			return "The ACS release branch pins must match the selected component tags."
		}
	}
	return ""
}

func (s *Session) work(source release.Source, discover bool) sourceWork {
	scope := ReadTargets | ReadChecks
	if discover {
		scope |= ReadPRs
	}
	if source.Name == "acs" {
		scope |= ReadPins
		if discover {
			scope |= ReadPreviousPins
		}
	} else if s.choices.Tags[source.Name] != "" {
		scope |= ReadTags | ReadCoverage
	}
	return sourceWork{
		scope: scope, plans: true, images: source.Name == "acs",
		releaseImages: source.Name == "acs" && s.rcReady(),
	}
}

func (s *Session) refreshPlanned(ctx context.Context, generation uint64, source release.Source, discover bool) error {
	s.mu.Lock()
	work := s.work(source, discover)
	work.revision = s.workRevision
	s.mu.Unlock()
	priority := githubread.Builds
	if discover {
		priority = githubread.PRs
	}
	ctx = githubread.WithPriority(ctx, priority, source.Repo)
	ctx, _ = githubread.Track(ctx, 80)
	err := s.refresh(ctx, generation, source, true, work)
	if !discover || source.Name == "acs" || ctx.Err() != nil {
		return err
	}
	// Tag inventories have their own result so incomplete ancestry reads cannot
	// prevent PR observations or selected build checks from being published.
	s.mu.Lock()
	revision := s.workRevision
	s.mu.Unlock()
	tags := sourceWork{scope: ReadTags, revision: revision, tagInventory: true}
	tagErr := s.refresh(githubread.WithPriority(ctx, githubread.Background, source.Repo), generation, source, true, tags)
	return errors.Join(err, tagErr)
}

func mergeDiscovery(old, latest release.Repository, request DiscoveryRequest) release.Repository {
	if !request.Reads(ReadCoverage) {
		for i := range latest.Tags {
			for _, tag := range old.Tags {
				if tag.Name == latest.Tags[i].Name && tag.SHA == latest.Tags[i].SHA {
					latest.Tags[i].Contains = maps.Clone(tag.Contains)
					break
				}
			}
		}
	}
	readsTagInventory := request.Reads(ReadTags) && (request.Scope == ReadAll || request.ConfirmedTag == "")
	if latest.TagsPending || !readsTagInventory {
		tags := slices.Clone(old.Tags)
		if latest.Head != old.Head {
			for i := range tags {
				tags[i].Evidence.Known = false
			}
		}
		for _, tag := range latest.Tags {
			index := slices.IndexFunc(tags, func(t release.Tag) bool { return t.Name == tag.Name })
			if index < 0 {
				tags = append(tags, tag)
			} else {
				tags[index] = tag
			}
		}
		latest.Tags = tags
	}
	if !readsTagInventory && latest.Source.Name != "acs" {
		latest.TagsPending = latest.TagsPending || old.TagsPending || old.Head != latest.Head
	}
	if !request.Reads(ReadChecks) {
		for i := range latest.PRs {
			latest.PRs[i].ChecksEvidence.Known = false
		}
	}
	if !request.Reads(ReadPRs) {
		prs := slices.Clone(old.PRs)
		for _, p := range latest.PRs {
			index := slices.IndexFunc(prs, func(pr release.PR) bool { return pr.Target == p.Target })
			if index < 0 {
				prs = append(prs, p)
			} else {
				prs[index] = p
			}
		}
		for i := range prs {
			if request.Reads(ReadTargets) && slices.Contains(request.Tracked, prs[i].Target) && !slices.ContainsFunc(latest.PRs, func(p release.PR) bool { return p.Target == prs[i].Target }) {
				prs[i].Evidence.Known = false
				prs[i].ChecksEvidence.Known = false
			}
		}
		latest.PRs = prs
	}
	if request.Scope == ReadTags && latest.Head == old.Head && old.Evidence.Fresh() {
		latest.Evidence = old.Evidence
	}
	if !request.Reads(ReadPreviousPins) {
		latest.PreviousPins, latest.PreviousPublished = old.PreviousPins, old.PreviousPublished
	}
	if !request.Reads(ReadPins) {
		latest.Pins, latest.PinningPR, latest.PinningPins, latest.PinningEvidence = old.Pins, old.PinningPR, old.PinningPins, old.PinningEvidence
		if latest.Head != old.Head {
			latest.Pins.Evidence.Known = false
		}
	}
	return latest
}

// recordDiscoveryChanges reports newly available work and PR transitions once.
// The caller holds the session lock.
func (s *Session) recordDiscoveryChanges(old, latest release.Repository, request DiscoveryRequest) {
	if request.Reads(ReadTags) && request.ConfirmedTag == "" {
		newTags := 0
		for _, tag := range latest.Tags {
			if tag.Evidence.Fresh() && tag.Belongs == release.Yes && !slices.ContainsFunc(old.Tags, func(previous release.Tag) bool { return previous.Name == tag.Name }) {
				newTags++
			}
		}
		if newTags > 0 {
			message := fmt.Sprintf("Found %s on %s. Review component release choices.", counted(newTags, "additional release tag"), consoleBranch(latest.Source))
			if len(old.Tags) == 0 {
				message = discoverySummary(request, latest, latest.TagsPending)
			}
			s.record(release.Target{Repo: latest.Source.Repo}, "", "tags-discovered", "", message, "system", message)
		}
	}
	for _, pr := range latest.PRs {
		if !pr.Evidence.Fresh() {
			continue
		}
		index := slices.IndexFunc(old.PRs, func(previous release.PR) bool { return previous.Target == pr.Target })
		message := ""
		switch {
		case index < 0 && pr.State == "open":
			message = fmt.Sprintf("Found pull request %s targeting %s: %s.", consoleTarget(pr.Target), pr.Base, pr.Title)
		case index >= 0 && !old.PRs[index].Merged && pr.Merged:
			message = fmt.Sprintf("Pull request %s merged into %s at merge commit %s.", consoleTarget(pr.Target), pr.Base, pr.MergeSHA)
		case index >= 0 && old.PRs[index].State == "open" && pr.State == "closed":
			message = fmt.Sprintf("Pull request %s closed without merging.", consoleTarget(pr.Target))
		}
		if message != "" {
			s.record(pr.Target, pr.SHA, "pr-state", "", message, "system", message)
		}
	}
}
