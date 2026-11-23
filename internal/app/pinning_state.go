package app

import (
	"maps"
	"strings"

	"release-assistant/internal/release"
)

type savedPinningChoice struct {
	Branch string `json:"branch"`
	Number int    `json:"number,omitempty"`
	Detect bool   `json:"detect,omitempty"`
}

func pinningChoice(config release.Config) savedPinningChoice {
	number := config.PinningPR
	if config.DetectPinningPR {
		number = config.ManualPinningPR
	}
	return savedPinningChoice{Branch: config.Branch, Number: number, Detect: config.DetectPinningPR}
}

func (s *ReleaseState) savePinningChoice(version string, choice savedPinningChoice) error {
	if s == nil {
		return nil
	}
	next := s.file
	next.PinningPRs = make(map[string]savedPinningChoice, len(s.file.PinningPRs)+1)
	maps.Copy(next.PinningPRs, s.file.PinningPRs)
	next.PinningPRs[strings.TrimPrefix(version, "v")] = choice
	return s.commit(next)
}

// restorePinningChoice returns the saved manual target for verification by ACS discovery.
func (s *ReleaseState) restorePinningChoice(config *release.Config) int {
	if s == nil {
		return 0
	}
	saved, ok := s.file.PinningPRs[strings.TrimPrefix(config.Version, "v")]
	if !ok || saved.Branch != config.Branch || saved.Number < 0 {
		return 0
	}
	if saved.Detect {
		config.DetectPinningPR = true
		config.ManualPinningPR = saved.Number
		return 0
	}
	return saved.Number
}

// configurePinningChoice preserves an unchanged saved number until GitHub verifies it.
func (s *Session) configurePinningChoice(config *release.Config) (int, error) {
	previous := pinningChoice(s.config)
	if s.pendingPinningPR > 0 {
		previous.Number = s.pendingPinningPR
	}
	next := pinningChoice(*config)
	sameRelease := s.config.Validate() == nil && previous.Branch == next.Branch &&
		strings.TrimPrefix(s.config.Version, "v") == strings.TrimPrefix(config.Version, "v")
	if sameRelease && next == previous {
		if s.pendingPinningPR > 0 {
			config.PinningPR = 0
		}
		return s.pendingPinningPR, nil
	}
	if !sameRelease && next.Number == 0 && !next.Detect {
		return s.options.ReleaseState.restorePinningChoice(config), nil
	}
	return 0, s.options.ReleaseState.savePinningChoice(config.Version, next)
}

// verifySavedPinningPR applies the pinning role only after a current identity read.
func (s *Session) verifySavedPinningPR(number int, pr *release.PR) bool {
	if number == 0 || s.pendingPinningPR != number || pr == nil || !pr.Evidence.Fresh() ||
		pr.Target != (release.Target{Repo: s.config.ACS().Repo, PR: number}) || pr.SHA == "" ||
		pr.Base != s.config.Branch || (pr.State != "open" && pr.State != "closed" && pr.State != "merged") {
		return false
	}
	s.pendingPinningPR = 0
	s.config.PinningPR = number
	s.includePinningPR()
	s.workRevision++
	return true
}
