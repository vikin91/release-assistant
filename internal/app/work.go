package app

import (
	"context"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type sourceWork struct {
	revision            uint64
	scope               DiscoveryScope
	plans, images, skip bool
}

func (s *Session) tagsConfirmed() bool {
	for _, source := range s.config.Components {
		if s.choices.Tags[source.Name] == "" {
			return false
		}
	}
	return true
}

func (s *Session) selectionReady() bool {
	if !s.tagsConfirmed() {
		return false
	}
	for _, source := range append([]release.Source{s.config.ACS()}, s.config.Components...) {
		if !s.prLoaded[source.Name] {
			return false
		}
	}
	return true
}

func (s *Session) phase() string {
	if !s.tagsConfirmed() {
		return "tags"
	}
	if !s.selectionComplete {
		return "prs"
	}
	if s.rcReady() {
		return "rc"
	}
	return "builds"
}

func (s *Session) rcReady() bool {
	acs := s.repos["acs"]
	p := acs.PinningPR
	if p == nil || p.Target != s.config.PinningTarget() || !p.Merged || p.Base != s.config.Branch || p.MergeSHA == "" || !p.Evidence.Fresh() || !acs.PinningEvidence.Fresh() || !acs.Pins.Evidence.Fresh() {
		return false
	}
	for _, source := range s.config.Components {
		if strings.TrimPrefix(acs.Pins.Versions[source.Name], "v") != strings.TrimPrefix(s.choices.Tags[source.Name], "v") {
			return false
		}
	}
	return true
}

func (s *Session) work(source release.Source, discover bool) sourceWork {
	switch s.phase() {
	case "tags":
		if source.Name == "acs" {
			return sourceWork{scope: ReadPins, skip: !discover}
		}
		return sourceWork{scope: ReadTags, skip: !discover}
	case "prs":
		scope := ReadPRs | ReadTargets
		if source.Name == "acs" {
			scope |= ReadPins
		}
		return sourceWork{scope: scope, skip: !discover}
	default:
		scope := ReadTags | ReadTargets | ReadChecks | ReadCoverage
		if source.Name == "acs" {
			scope = ReadPins | ReadTargets | ReadChecks
		}
		return sourceWork{scope: scope, plans: true, images: source.Name == "acs"}
	}
}

func (s *Session) refreshPlanned(ctx context.Context, generation uint64, source release.Source, discover bool) error {
	s.mu.Lock()
	work := s.work(source, discover)
	work.revision = s.workRevision
	s.mu.Unlock()
	if work.skip {
		return nil
	}
	return s.refresh(ctx, generation, source, discover, work)
}

// suspendPlans removes retry authority while keeping earlier build observations visible.
// The caller holds the session lock.
func (s *Session) suspendPlans() {
	for target, plan := range s.plans {
		plan.Proposals = nil
		plan.Evidence.Known = false
		plan.Evidence.Error = ""
		s.plans[target] = plan
		s.dispatcher.ObservePlan(plan)
	}
}

func mergeDiscovery(old, latest release.Repository, request DiscoveryRequest) release.Repository {
	if !request.Reads(ReadTags) || (request.Scope != ReadAll && request.ConfirmedTag != "") {
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
	if !request.Reads(ReadChecks) {
		for i := range latest.PRs {
			latest.PRs[i].ChecksEvidence.Known = false
		}
	}
	if !request.Reads(ReadPins) {
		latest.Pins, latest.PinningPR, latest.PinningPins, latest.PinningEvidence, latest.PreviousPublished = old.Pins, old.PinningPR, old.PinningPins, old.PinningEvidence, old.PreviousPublished
		if latest.Head != old.Head {
			latest.Pins.Evidence.Known = false
		}
	}
	return latest
}
