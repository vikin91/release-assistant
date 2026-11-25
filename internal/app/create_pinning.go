package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"release-assistant/internal/apilog"
	"release-assistant/internal/release"
)

type (
	// PinningPreview holds the reviewed request and all currently known blockers.
	PinningPreview struct {
		Request     release.PinningRequest
		ID          string
		Rows        []PinningRow
		Reasons     []string
		Duplicate   int
		inputsReady bool
	}
	// PinningRow shows how one selected component would affect the release branch.
	PinningRow struct {
		Name, Current, Tag, Images string
	}
)

func pinningPreview(x Snapshot, available, creating bool) PinningPreview {
	p := PinningPreview{Request: release.PinningRequest{
		Repo: x.Config.ACS().Repo, Branch: x.Config.Branch, Version: x.Config.Version,
	}}
	if x.Config.Validate() != nil {
		return p
	}
	switch {
	case !x.Monitoring:
		p.Reasons = append(p.Reasons, "Activate this release to create a pinning PR.")
	case !x.WritesEnabled:
		p.Reasons = append(p.Reasons, "Start Release Assistant with --enable-writes to create a pinning PR.")
	}
	if !available {
		p.Reasons = append(p.Reasons, "The local GitHub CLI (gh) is unavailable.")
	}
	if creating {
		p.Reasons = append(p.Reasons, "Pinning PR creation is in progress.")
	}
	var acs release.Repository
	for _, repo := range x.Repositories {
		if repo.Source.Name == "acs" {
			acs = repo
			break
		}
	}
	p.Request.Head = acs.Head
	pinsReady := acs.Pins.Evidence.Fresh() && acs.Evidence.Fresh() && acs.Head != ""
	if !pinsReady {
		p.Reasons = append(p.Reasons, "The current StackRox pins could not be verified.")
	}

	paths := release.PinPaths(x.Config.Version)
	var selections []string
	completeDiff, imagesReady := pinsReady, x.Inventory.Evidence.Fresh()
	for _, row := range []PinningRow{{Name: "Collector"}, {Name: "Fact"}, {Name: "Scanner"}} {
		name := strings.ToLower(row.Name)
		path, required := paths[name]
		if !required {
			continue
		}
		row.Current, row.Tag, row.Images = acs.Pins.Versions[name], x.Choices.Tags[name], "Waiting for tag"
		if row.Tag == "" {
			row.Tag = x.PendingTags[name]
		}
		selections = append(selections, row.Tag)
		version := strings.TrimPrefix(row.Tag, "v")
		if _, _, _, valid := release.Version(version); !valid || x.PendingTags[name] != "" {
			completeDiff = false
			p.Reasons = append(p.Reasons, fmt.Sprintf("Select a verified %s tag in Component releases.", row.Name))
		} else {
			row.Images = "Pending"
			var ci, konflux, pending bool
			for _, image := range x.Assessment.Images {
				if image.Image.Component != name || !image.Required || image.SelectedTag != row.Tag {
					continue
				}
				ci = ci || image.Image.Family == "ci"
				konflux = konflux || image.Image.Family == "konflux"
				pending = pending || image.Status != release.Yes
			}
			if x.Inventory.Evidence.Fresh() && ci && konflux && !pending {
				row.Images = "Ready"
			} else {
				imagesReady = false
				p.Reasons = append(p.Reasons, fmt.Sprintf("Waiting for %s %s images to be verified.", row.Name, row.Tag))
			}
			if pinsReady {
				before := acs.Pins.Contents[name]
				after, singleLine := release.ReplacementPin(before, version)
				if !singleLine || row.Current == "" || strings.TrimSpace(before) != row.Current {
					completeDiff = false
					p.Reasons = append(p.Reasons, fmt.Sprintf("The contents of %s could not be verified as a single version line.", path))
				} else if before != after {
					p.Request.Changes = append(p.Request.Changes, release.PinChange{Path: path, Before: before, After: after})
				}
			}
		}
		p.Rows = append(p.Rows, row)
	}

	if !completeDiff {
		p.Request.Changes = nil
	} else if len(p.Request.Changes) == 0 {
		p.Reasons = append(p.Reasons, "The selected tags already match the release branch pins.")
	}
	if completeDiff && len(p.Request.Changes) != 0 {
		if !acs.PinDiscovery.Evidence.Historical {
			p.Duplicate = acs.PinDiscovery.Duplicate(release.PinSignature(p.Request.Changes))
		}
		if !acs.PinDiscovery.Evidence.Fresh() {
			p.Reasons = append(p.Reasons, "Release Assistant could not verify whether an identical PR is open.")
		}
	}
	p.inputsReady = completeDiff && imagesReady && len(p.Request.Changes) != 0
	p.ID = pinningPreviewID(p.Request, selections)
	return p
}

func pinningPreviewID(req release.PinningRequest, selections []string) string {
	// Bind the click to the release head and all selected tags, including unchanged pins.
	data, _ := json.Marshal([]any{req.Repo, req.Branch, req.Version, req.Head, selections, release.PinSignature(req.Changes)})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Session) createPinningPR(ctx context.Context, a Action) error {
	s.mu.Lock()
	if a.Generation != s.generation || !s.targetsActiveProfile(a.ProfileID) {
		s.mu.Unlock()
		return fmt.Errorf("the active release changed; reload the page")
	}
	work, profileID := s.releaseWork, s.activeProfile
	cfg := s.config
	preview := s.snapshot(work, true).PinningPreview
	if preview.ID == "" || preview.ID != a.ID {
		s.mu.Unlock()
		return fmt.Errorf("the proposed pins changed; review the updated page")
	}
	if len(preview.Reasons) != 0 || preview.Duplicate != 0 {
		s.mu.Unlock()
		return fmt.Errorf("pinning PR creation is blocked; see the reasons below")
	}
	work.creatingPinningPR = true
	previous := s.previousPinFiles(s.repos["acs"])
	ctx = s.dispatcher.WithScope(apilog.WithRelease(ctx, cfg.Version))
	s.mu.Unlock()

	receipt, discovery, err := s.dispatcher.CreatePinningPR(ctx, preview.Request, previous, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.releaseWork != work || s.generation != a.Generation {
			return false
		}
		current := s.snapshot(work, true).PinningPreview
		// Dispatch has just rechecked open PRs, so only local inputs need to agree here.
		return current.ID == preview.ID && current.inputsReady
	})

	s.mu.Lock()
	work.creatingPinningPR = false
	if current := work.repos["acs"]; current.Head == preview.Request.Head {
		if discovery.Evidence.At.After(current.PinDiscovery.Evidence.At) {
			current.PinDiscovery = discovery
		}
		if receipt.Number != 0 {
			if current.PinDiscovery.Files == nil {
				current.PinDiscovery.Files = make(map[int]release.PinFileMatch)
			} else {
				current.PinDiscovery.Files = maps.Clone(current.PinDiscovery.Files)
			}
			current.PinDiscovery.Files[receipt.Number] = release.PinFileMatch{Diff: release.PinSignature(preview.Request.Changes)}
		}
		work.repos["acs"] = current
		if profileID == s.activeProfile && s.releaseWork == work && a.Generation == s.generation {
			s.observeDashboardRepo(current, false, "")
			if receipt.Number != 0 {
				delete(work.polls, pollID{"acs", pollPRs})
				s.wakePolls()
			}
		}
	}
	if receipt.Number > 0 && sameDashboardConfig(work.config, cfg) {
		work.createdPinningPR = receipt
		if profileID != "" {
			profile, found := profileByID(s.options.ReleaseState.file.Profiles, profileID)
			if found && sameDashboardConfig(profile.Config, cfg) {
				saved := s.dashboards.Profiles[profileID]
				saved.Config = profileConfig(cfg)
				saved.CreatedPinningPR = receipt
				s.dashboards.Profiles[profileID] = saved
				s.dashboardDirty = true
			}
		}
	}
	s.drain()
	s.mu.Unlock()
	return err
}

func (s *Session) previousPinFiles(repo release.Repository) map[int]release.PinFileMatch {
	saved := s.dashboards.Profiles[s.activeProfile]
	if !sameDashboardConfig(saved.Config, s.config) {
		return maps.Clone(repo.PinDiscovery.Files)
	}
	files := maps.Clone(saved.Repositories["acs"].PinDiscovery.Files)
	if files == nil {
		files = make(map[int]release.PinFileMatch, len(repo.PinDiscovery.Files))
	}
	maps.Copy(files, repo.PinDiscovery.Files)
	return files
}
