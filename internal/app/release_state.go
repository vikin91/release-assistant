package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/release"
)

type (
	// ReleaseState holds one session's local choices for all ACS patch releases.
	ReleaseState struct {
		path     string
		file     releaseStateFile
		warnings []string
	}

	releaseStateFile struct {
		Version           int                                     `json:"version"`
		Profiles          []ReleaseProfile                        `json:"profiles"`
		ActiveProfile     string                                  `json:"active_profile,omitempty"`
		Releases          map[string]map[string]release.Choice    `json:"releases"`
		ComponentTags     map[string]map[string]savedComponentTag `json:"component_tags,omitempty"`
		PinningPRs        map[string]savedPinningChoice           `json:"pinning_prs,omitempty"`
		ImageObservations map[string]savedImageObservation        `json:"image_observations,omitempty"`
	}

	savedComponentTag struct {
		Repo   string `json:"repo"`
		Branch string `json:"branch"`
		Tag    string `json:"tag,omitempty"`
		SHA    string `json:"sha,omitempty"`
	}
)

// OpenReleaseState loads local decisions and initializes the file on first use.
func OpenReleaseState(path string, log *Log) (*ReleaseState, error) {
	if path == "" {
		return nil, errors.New("release state file path is empty")
	}
	state := &ReleaseState{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if log != nil {
			log.Append(Record{
				At: time.Now(), Decision: "pr-state-created", Origin: "system",
				Result: fmt.Sprintf("No state file found at %s. Writing a new one.", path),
			})
		}
		state.file.Version = releaseStateVersion
		state.file.Profiles = []ReleaseProfile{}
		state.file.Releases = make(map[string]map[string]release.Choice)
		if err := state.write(state.file); err != nil {
			return nil, fmt.Errorf("create release state %q: %w", path, err)
		}
		return state, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read release state %q: %w", path, err)
	}
	if err := json.Unmarshal(data, &state.file); err != nil {
		return nil, fmt.Errorf("decode release state %q: %w", path, err)
	}
	if state.file.Version != 0 && state.file.Version != releaseStateVersion {
		return nil, fmt.Errorf("state file %q uses unsupported version %d; move it aside to start with a new file", path, state.file.Version)
	}
	if state.file.Releases == nil {
		if state.file.Version == 0 {
			return nil, fmt.Errorf("PR state %q must contain a releases object", path)
		}
		state.file.Releases = make(map[string]map[string]release.Choice)
	}
	for version, choices := range state.file.Releases {
		if _, _, _, ok := release.Version(version); !ok || strings.HasPrefix(version, "v") {
			return nil, fmt.Errorf("PR state %q contains invalid release %q", path, version)
		}
		for key, choice := range choices {
			target := statePRTarget(key)
			if target.PR < 1 || !release.ValidRepo(target.Repo) || target.String() != key {
				return nil, fmt.Errorf("PR state %q contains invalid PR %q", path, key)
			}
			switch choice {
			case release.Included, release.Excluded, release.Ignored:
			default:
				return nil, fmt.Errorf("PR state %q contains invalid choice %q for %s", path, choice, key)
			}
		}
	}
	state.validateProfiles()
	return state, nil
}

func statePRTarget(key string) release.Target {
	repo, number, _ := strings.Cut(key, "#")
	pr, _ := strconv.Atoi(number)
	return release.Target{Repo: repo, PR: pr}
}

func (s *Session) restorePRChoices() {
	s.releaseWork.restorePRChoices(s.options.ReleaseState)
	for target, choice := range s.choices.PRs {
		if choice == release.Excluded || choice == release.Ignored {
			s.dispatcher.Ignore(target, true)
		}
	}
}

func (s *releaseWork) restorePRChoices(state *ReleaseState) {
	if state == nil {
		return
	}
	choices := state.file.Releases[strings.TrimPrefix(s.config.Version, "v")]
	for key, choice := range choices {
		target := statePRTarget(key)
		if target.Repo != s.config.ACS().Repo && !slices.ContainsFunc(s.config.Components, func(source release.Source) bool {
			return source.Repo == target.Repo
		}) {
			continue
		}
		s.choices.PRs[target] = choice
	}
}

// savePRChoices commits the whole action before its choices take effect in the session.
// The caller holds the session lock.
func (s *ReleaseState) savePRChoices(version string, targets map[release.Target]string, choice release.Choice) error {
	if s == nil {
		return nil
	}
	version = strings.TrimPrefix(version, "v")
	choices := make(map[string]release.Choice, len(s.file.Releases[version])+len(targets))
	maps.Copy(choices, s.file.Releases[version])
	for target := range targets {
		if choice == "" {
			delete(choices, target.String())
		} else {
			choices[target.String()] = choice
		}
	}
	next := s.file
	next.Releases = maps.Clone(next.Releases)
	if len(choices) == 0 {
		delete(next.Releases, version)
	} else {
		next.Releases[version] = choices
	}
	return s.commit(next)
}

func (s *ReleaseState) commit(next releaseStateFile) error {
	next.Version = releaseStateVersion
	if err := s.write(next); err != nil {
		return fmt.Errorf("save release state %q: %w", s.path, err)
	}
	s.file = next
	return nil
}

// write stages the replacement beside the destination so the rename is atomic.
func (s *ReleaseState) write(state releaseStateFile) error {
	return writeStateJSON(s.path, state, "  ")
}

func writeStateJSON(path string, state any, indent string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".release-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", indent)
	err = encoder.Encode(state)
	if err == nil {
		err = file.Sync()
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
