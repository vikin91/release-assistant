package app

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/release"
)

type rcVulnerabilityReader interface {
	ReadRCVulnerabilities(context.Context, release.Candidate, string, release.RCVulnerabilities, release.Validation) release.RCVulnerabilities
}

type conformaReportReader interface {
	ReadConformaReport(context.Context, int64, string, string, string) release.ConformaReport
}

func (s *Session) refreshRCVulnerabilities(ctx context.Context, generation, revision uint64) {
	reader, ok := s.discovery.(rcVulnerabilityReader)
	if !ok {
		return
	}
	s.mu.Lock()
	repo := s.repos["acs"]
	candidate := repo.Candidate
	previous := release.PreviousCandidate(repo)
	current := generation == s.generation && revision == s.workRevision && s.rcReady()
	currentCache, previousCache := s.rcVulnerabilities[candidate.Target], s.rcVulnerabilities[previous.Target]
	validation := s.tagChecks["acs"].validation
	registry := slices.Clone(s.inventory.ReleaseImages)
	registryTag := s.inventory.Versions["acs"]
	s.mu.Unlock()
	if !current {
		return
	}

	observe := func(rc release.Candidate, evidence release.RCVulnerabilities) {
		s.mu.Lock()
		defer s.mu.Unlock()
		latest := s.repos["acs"]
		if generation != s.generation || revision != s.workRevision || canceled(ctx) ||
			latest.Candidate.Target != candidate.Target || latest.Candidate.SHA != candidate.SHA ||
			(rc.Target != candidate.Target && !slices.ContainsFunc(latest.Tags, func(tag release.Tag) bool {
				return tag.Name == rc.Target.Tag && tag.SHA == rc.SHA && tag.Evidence.Fresh()
			})) {
			return
		}
		if evidence.Target == rc.Target && evidence.SHA == rc.SHA {
			evidence.ObservedAt = time.Now()
			s.rcVulnerabilities[rc.Target] = evidence
			if s.activeProfile != "" && evidence.Problem == "" && slices.ContainsFunc(evidence.Images, func(image release.ImageVulnerabilities) bool {
				return image.Status == "Available" || image.ConformaReported
			}) {
				saved := s.dashboardForUpdate()
				reviews := make(map[string]release.RCVulnerabilities, 2)
				for _, choice := range []release.Candidate{candidate, previous} {
					if old := saved.Vulnerabilities[choice.Target.Tag]; old.Target == choice.Target && old.SHA == choice.SHA && choice.Target.Valid() {
						reviews[choice.Target.Tag] = old
					}
				}
				reviews[rc.Target.Tag] = evidence
				saved.Vulnerabilities = reviews
				s.storeDashboard(saved)
			}
		}
	}

	evidence := reader.ReadRCVulnerabilities(ctx, candidate, "", currentCache, validation)
	if conforma, ok := s.planner.(conformaReportReader); ok && registryTag == strings.TrimPrefix(candidate.Target.Tag, "v") && len(evidence.Images) > 0 {
		observe(candidate, evidence)
		evidence.Images = slices.Clone(evidence.Images)
		attachConformaReports(ctx, conforma, registry, evidence.Images, currentCache.Images)
	}
	observe(candidate, evidence)
	if previous.Target.Valid() && (previousCache.SHA != previous.SHA || !previousCache.ScansComplete() || previousCache.DescendantSHA != candidate.SHA) {
		observe(previous, reader.ReadRCVulnerabilities(ctx, previous, candidate.SHA, previousCache, release.Validation{}))
	}
}

func (s *releaseWork) vulnerabilityReviews() []release.RCVulnerabilities {
	repo := s.repos["acs"]
	if !repo.Candidate.Target.Valid() || repo.Candidate.Target.Tag == "" {
		return nil
	}
	var reviews []release.RCVulnerabilities
	for _, rc := range []release.Candidate{repo.Candidate, release.PreviousCandidate(repo)} {
		if !rc.Target.Valid() {
			continue
		}
		review := s.rcVulnerabilities[rc.Target]
		if review.SHA != rc.SHA || review.Target != rc.Target || rc.Target != repo.Candidate.Target && review.DescendantSHA != "" && review.DescendantSHA != repo.Candidate.SHA {
			if rc.Target == repo.Candidate.Target {
				reviews = append(reviews, release.RCVulnerabilities{Target: rc.Target, SHA: rc.SHA})
			}
			continue
		}
		reviews = append(reviews, cloneVulnerabilityReview(review))
	}
	return reviews
}

func cloneVulnerabilityReview(review release.RCVulnerabilities) release.RCVulnerabilities {
	review.Images = slices.Clone(review.Images)
	for i := range review.Images {
		review.Images[i].Findings = slices.Clone(review.Images[i].Findings)
		review.Images[i].ConformaReport.Findings = slices.Clone(review.Images[i].ConformaReport.Findings)
	}
	return review
}

func applyDashboardVulnerabilities(snapshot *Snapshot, saved map[string]release.RCVulnerabilities) {
	rc := snapshot.Candidate()
	if len(saved) == 0 || !rc.Target.Valid() || rc.SHA == "" {
		return
	}
	for _, repo := range snapshot.Repositories {
		if repo.Source.Name != "acs" {
			continue
		}
		for _, choice := range []release.Candidate{rc, release.DisplayPreviousCandidate(repo)} {
			old := saved[choice.Target.Tag]
			if !choice.Target.Valid() || old.Target != choice.Target || old.SHA != choice.SHA ||
				(choice.Target != rc.Target && old.DescendantSHA != rc.SHA) {
				continue
			}
			i := slices.IndexFunc(snapshot.RCVulnerabilities, func(review release.RCVulnerabilities) bool {
				return review.Target == choice.Target && review.SHA == choice.SHA
			})
			if i >= 0 {
				live := snapshot.RCVulnerabilities[i]
				if len(live.Images) > 0 || live.Status != "" {
					continue
				}
				old.Problem = live.Problem
			}
			old.Historical = true
			old = cloneVulnerabilityReview(old)
			if i >= 0 {
				snapshot.RCVulnerabilities[i] = old
			} else {
				if len(snapshot.RCVulnerabilities) == 0 && choice.Target != rc.Target {
					snapshot.RCVulnerabilities = append(snapshot.RCVulnerabilities, release.RCVulnerabilities{Target: rc.Target, SHA: rc.SHA})
				}
				snapshot.RCVulnerabilities = append(snapshot.RCVulnerabilities, old)
			}
		}
		return
	}
}

func attachConformaReports(ctx context.Context, reader conformaReportReader, registry []release.Image, images, previous []release.ImageVulnerabilities) {
	var reads sync.WaitGroup
	for i := range images {
		row := &images[i]
		if !row.ConformaReported || row.ConformaDetails == "" {
			continue
		}
		index := slices.IndexFunc(registry, func(image release.Image) bool {
			return image.Family == "konflux" && image.Evidence.Fresh() && image.Present == release.Yes &&
				strings.HasPrefix(image.Reference, "quay.io/rhacs-eng/"+row.Image+":") && image.ManifestDigest != ""
		})
		if index < 0 {
			continue
		}
		image := registry[index]
		repository, _, _ := strings.Cut(image.Reference, ":")
		if cached := slices.IndexFunc(previous, func(old release.ImageVulnerabilities) bool {
			return old.Image == row.Image && old.ConformaReport.RunURL == row.ConformaDetails && old.ConformaReport.Digest == image.ManifestDigest
		}); cached >= 0 {
			row.ConformaReport = previous[cached].ConformaReport
			continue
		}
		reads.Go(func() {
			row.ConformaReport = reader.ReadConformaReport(ctx, row.ConformaAppID, row.ConformaDetails, repository, image.ManifestDigest)
		})
	}
	reads.Wait()
}
