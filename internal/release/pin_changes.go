package release

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

type (
	// PinChange preserves exact file contents for the proposed PR diff.
	PinChange struct {
		Path, Before, After string
	}
	// PinningRequest binds a creation to the release branch revision shown to the user.
	PinningRequest struct {
		Repo, Branch, Head, Version string
		Changes                     []PinChange
	}
)

// HeadBranch keeps the automated and manual branch names identical for a reviewed diff.
func (r PinningRequest) HeadBranch() string {
	signature := PinSignature(r.Changes)
	return fmt.Sprintf("release-assistant/pin-%s-%s-%s", strings.TrimPrefix(r.Version, "v"), r.Head[:min(12, len(r.Head))], signature[:12])
}

// Title identifies the release receiving the pin update.
func (r PinningRequest) Title() string {
	return "chore(release): update component pins for " + r.Version
}

// PinPaths defines the version files present in each ACS release series.
func PinPaths(version string) map[string]string {
	paths := map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}
	if major, _, _, ok := Version(version); ok && major == 4 {
		paths["scanner"] = "SCANNER_VERSION"
	}
	return paths
}

// PinSignature ignores file ordering while retaining exact paths and contents.
func PinSignature(changes []PinChange) string {
	ordered := slices.Clone(changes)
	slices.SortFunc(ordered, func(a, b PinChange) int { return strings.Compare(a.Path, b.Path) })
	data, _ := json.Marshal(ordered)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

// ReplacementPin preserves the existing newline convention of a single version line.
func ReplacementPin(before, version string) (string, bool) {
	line := strings.TrimSuffix(strings.TrimSuffix(before, "\n"), "\r")
	current := strings.TrimSpace(line)
	if _, _, _, ok := Version(current); !ok || strings.ContainsAny(line, "\r\n") {
		return "", false
	}
	if strings.HasPrefix(current, "v") {
		version = "v" + version
	}
	return strings.Replace(before, current, version, 1), true
}

// Duplicate returns the lowest open PR number with a verified matching diff.
func (d PinDiscovery) Duplicate(signature string) int {
	number := 0
	for pr, match := range d.Files {
		if match.Diff == signature && (number == 0 || pr < number) {
			number = pr
		}
	}
	return number
}
