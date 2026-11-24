package app

import (
	"crypto/rand"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/release"
)

const releaseStateVersion = 1

// ReleaseProfile is a saved tracking slot. An empty configuration is a draft.
type ReleaseProfile struct {
	ID         string         `json:"id"`
	Config     release.Config `json:"config"`
	LastActive time.Time      `json:"last_active,omitzero"`
}

// Label identifies the configured release or a placeholder.
func (p ReleaseProfile) Label() string {
	if p.Config.Version == "" {
		return "Unconfigured release"
	}
	return "ACS " + p.Config.Version
}

func stream(version string) string {
	major, minor, _, ok := release.Version(version)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d", major, minor)
}

func (s *ReleaseState) validateProfiles() {
	ids, streams := map[string]bool{}, map[string]bool{}
	s.file.Profiles = slices.DeleteFunc(s.file.Profiles, func(p ReleaseProfile) bool {
		key := stream(p.Config.Version)
		if p.ID == "" || ids[p.ID] || (p.Config.Version != "" && p.Config.Validate() != nil) || (key != "" && streams[key]) {
			s.warnings = append(s.warnings, "Ignored an invalid or duplicate saved profile for "+p.Label()+".")
			return true
		}
		ids[p.ID] = true
		streams[key] = key != ""
		return false
	})
	if s.file.ActiveProfile != "" {
		if p, ok := profileByID(s.file.Profiles, s.file.ActiveProfile); !ok || p.Config.Version == "" {
			s.file.ActiveProfile = ""
			for _, candidate := range s.file.Profiles {
				if candidate.Config.Version != "" {
					s.file.ActiveProfile = candidate.ID
					break
				}
			}
		}
	}
}

// SetInitialRelease selects the saved profile for an explicitly configured CLI release.
func (s *ReleaseState) SetInitialRelease(config release.Config, pinningOverride bool) error {
	if config.Version == "" {
		return nil
	}
	if err := config.Validate(); err != nil {
		return err
	}
	config.Version = strings.TrimPrefix(config.Version, "v")
	next := s.file
	idx := slices.IndexFunc(next.Profiles, func(p ReleaseProfile) bool { return stream(p.Config.Version) == stream(config.Version) })
	profile := ReleaseProfile{ID: rand.Text()}
	if idx >= 0 {
		old := next.Profiles[idx]
		if old.Config.Version != config.Version {
			return fmt.Errorf("%s already tracks the %s stream; change its patch version in Settings", old.Label(), stream(config.Version))
		}
		profile = old
	}
	if pinningOverride || config.PinningPR != 0 {
		storePinning(&next, config)
	}
	profile.Config = profileConfig(config)
	next.Profiles = slices.Clone(next.Profiles)
	if idx < 0 {
		next.Profiles = append(next.Profiles, profile)
	} else {
		next.Profiles[idx] = profile
	}
	setActiveProfile(&next, profile.ID)
	return s.commit(next)
}

func profileByID(profiles []ReleaseProfile, id string) (ReleaseProfile, bool) {
	idx := slices.IndexFunc(profiles, func(p ReleaseProfile) bool { return p.ID == id })
	if idx < 0 {
		return ReleaseProfile{}, false
	}
	return profiles[idx], true
}

func storePinning(state *releaseStateFile, config release.Config) {
	if config.PinningPR != 0 || config.DetectPinningPR || state.PinningPRs[config.Version].Branch != "" {
		state.PinningPRs = maps.Clone(state.PinningPRs)
		if state.PinningPRs == nil {
			state.PinningPRs = make(map[string]savedPinningChoice)
		}
		state.PinningPRs[config.Version] = pinningChoice(config)
	}
}

func profileConfig(config release.Config) release.Config {
	config.Components = slices.Clone(config.Components)
	config.PinningPR, config.ManualPinningPR, config.DetectPinningPR = 0, 0, false
	return config
}

func setActiveProfile(state *releaseStateFile, id string) {
	now := time.Now()
	for i := range state.Profiles {
		p := &state.Profiles[i]
		if p.ID == state.ActiveProfile || p.ID == id {
			p.LastActive = now
		}
	}
	state.ActiveProfile = id
}

// ReleaseProfiles returns the sidebar-independent navigation state.
func (s *Session) ReleaseProfiles() (profiles []ReleaseProfile, active string, warnings []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.options.Profiles {
		profiles = slices.Clone(s.options.ReleaseState.file.Profiles)
		for i := range profiles {
			profiles[i].Config.Components = slices.Clone(profiles[i].Config.Components)
		}
		return profiles, s.activeProfile, slices.Clone(s.options.ReleaseState.warnings)
	}
	return nil, "", nil
}

// ProfilesEnabled reports whether the session serves release navigation.
func (s *Session) ProfilesEnabled() bool { return s.options.Profiles }

// targetsActiveProfile checks the optional scope on a release action under s.mu.
func (s *Session) targetsActiveProfile(id string) bool {
	return !s.options.Profiles || id == "" || id == s.activeProfile
}

// ProfileSnapshot returns the requested view without starting or switching polling.
func (s *Session) ProfileSnapshot(id string) (Snapshot, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, exists := profileByID(s.options.ReleaseState.file.Profiles, id)
	if exists && s.activeProfile == id {
		return s.snapshot(s.releaseWork, true), true
	}
	work := s.profiles[id]
	if work == nil {
		work = newReleaseWork()
		work.config = profileConfig(p.Config)
		work.restorePRChoices(s.options.ReleaseState)
		work.restoreComponentTags(s.options.ReleaseState)
		work.pendingPinningPR = s.options.ReleaseState.restorePinningChoice(&work.config)
	}
	return s.snapshot(work, false), exists
}

func (s *Session) applyProfile(a Action) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.options.ReleaseState
	next := state.file
	next.Profiles = slices.Clone(next.Profiles)
	idx := slices.IndexFunc(next.Profiles, func(p ReleaseProfile) bool { return p.ID == a.ProfileID })
	if a.Kind == "add-release" {
		if idx >= 0 {
			return fmt.Errorf("release profile already exists")
		}
		id := a.ProfileID
		if id == "" {
			id = rand.Text()
		}
		next.Profiles = append(next.Profiles, ReleaseProfile{ID: id})
		return state.commit(next)
	}
	creating := a.Kind == "configure" && idx < 0
	if creating {
		idx = len(next.Profiles)
		next.Profiles = append(next.Profiles, ReleaseProfile{ID: rand.Text()})
	}
	if idx < 0 {
		return fmt.Errorf("release profile no longer exists")
	}
	profile := next.Profiles[idx]
	previousPinning := next.PinningPRs[profile.Config.Version]
	switch a.Kind {
	case "open-release":
		return nil
	case "activate-release":
		if profile.Config.Version == "" {
			return fmt.Errorf("save the release configuration before activating it")
		}
		if profile.ID == s.activeProfile {
			return nil
		}
		setActiveProfile(&next, profile.ID)
	case "remove-release":
		if len(next.Profiles) < 2 {
			return fmt.Errorf("keep at least one release profile")
		}
		if profile.ID == next.ActiveProfile {
			setActiveProfile(&next, "")
		}
		next.Profiles = slices.Delete(next.Profiles, idx, idx+1)
		if key := stream(profile.Config.Version); key != "" {
			next.Releases = maps.Clone(next.Releases)
			next.ComponentTags = maps.Clone(next.ComponentTags)
			next.PinningPRs = maps.Clone(next.PinningPRs)
			maps.DeleteFunc(next.Releases, func(version string, _ map[string]release.Choice) bool { return stream(version) == key })
			maps.DeleteFunc(next.ComponentTags, func(version string, _ map[string]savedComponentTag) bool { return stream(version) == key })
			maps.DeleteFunc(next.PinningPRs, func(version string, _ savedPinningChoice) bool { return stream(version) == key })
		}
	case "configure":
		if profile.ID == s.activeProfile && a.Generation != s.generation {
			return fmt.Errorf("page belongs to an earlier release selection; reopen the release")
		}
		if err := a.Config.Validate(); err != nil {
			return err
		}
		a.Config.Version = strings.TrimPrefix(a.Config.Version, "v")
		key := stream(a.Config.Version)
		for _, other := range next.Profiles {
			if other.ID != profile.ID && stream(other.Config.Version) == key {
				return fmt.Errorf("%s already tracks the %s stream", other.Label(), key)
			}
		}
		// Retain existing saved pinning choices when returning to a patch.
		if profile.Config.Version == a.Config.Version || a.Config.PinningPR != 0 || a.Config.DetectPinningPR {
			storePinning(&next, a.Config)
		}
		next.Profiles[idx].Config = profileConfig(a.Config)
	}
	if err := state.commit(next); err != nil {
		return err
	}
	if a.Kind == "configure" && profile.ID == s.activeProfile {
		if slices.Equal(s.config.Components, a.Config.Components) && s.config.Version == a.Config.Version && s.config.Branch == a.Config.Branch {
			s.updateProfilePinning(a.Config)
			return nil
		}
		s.activeProfile = ""
		delete(s.profiles, profile.ID)
		s.dispatcher.Reset()
	}
	if a.Kind == "configure" && profile.ID != next.ActiveProfile {
		if work := s.profiles[profile.ID]; work != nil {
			saved := next.Profiles[idx].Config
			if work.config.Version != saved.Version || work.config.Branch != saved.Branch || !slices.Equal(work.config.Components, saved.Components) ||
				state.file.PinningPRs[saved.Version] != previousPinning {
				delete(s.profiles, profile.ID)
				s.dispatcher.ForgetRelease(profile.ID)
			}
		}
	}
	if a.Kind == "remove-release" {
		delete(s.profiles, profile.ID)
		if profile.ID == s.activeProfile {
			s.dispatcher.Reset()
		}
	}
	if s.activeProfile != next.ActiveProfile {
		if a.Kind == "activate-release" && s.activeProfile != "" {
			s.snapshot(s.releaseWork, true)
		}
		s.activateProfile(next.ActiveProfile)
	}
	if a.Kind == "remove-release" {
		s.dispatcher.ForgetRelease(profile.ID)
	}
	return nil
}

func (s *Session) updateProfilePinning(config release.Config) {
	before := pinningChoice(s.config)
	if s.pendingPinningPR > 0 {
		before.Number = s.pendingPinningPR
	}
	if before == pinningChoice(config) {
		return
	}
	s.clearPinningSelection()
	s.config = config
	s.pendingPinningPR = 0
	s.includePinningPR()
	s.generation++
	s.workRevision++
	delete(s.polls, pollID{s.config.ACS().Repo, pollPRs})
	select {
	case s.changed <- struct{}{}:
	default:
	}
}

func (s *Session) activateProfile(id string) {
	if s.monitorCancel != nil {
		s.monitorCancel()
	}
	s.drain()
	profile, _ := profileByID(s.options.ReleaseState.file.Profiles, id)
	s.dispatcher.SelectRelease(id, profile.Config.Version)
	s.activeProfile = id
	s.releaseWork = s.profiles[id]
	if s.releaseWork == nil {
		s.releaseWork = newReleaseWork()
		s.config = profileConfig(profile.Config)
		if id != "" {
			s.profiles[id] = s.releaseWork
			s.restorePRChoices()
			s.restoreComponentTags(s.options.ReleaseState)
			s.pendingPinningPR = s.options.ReleaseState.restorePinningChoice(&s.config)
		}
	} else {
		s.resumedAt = time.Now()
	}
	s.generation++
	s.workRevision++
	if id != "" {
		s.record(release.Target{}, "", "release-activated", "", "Tracking "+profile.Label()+".", "user")
	}
	select {
	case s.changed <- struct{}{}:
	default:
	}
}
