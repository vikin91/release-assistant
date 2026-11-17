// Package release holds service-independent release observations and assessment.
package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type (
	// Truth distinguishes absent evidence from a positive or negative observation.
	Truth string
	// Choice records scope independently of write authority.
	Choice string
	// Evidence retains observation provenance and current access failures.
	Evidence struct {
		At         time.Time
		URL, Error string
		Known      bool
	}
	// Target identifies a PR or a tag; SHA is deliberately separate.
	Target struct {
		Repo string
		PR   int
		Tag  string
	}
	// Source describes an explicitly configured repository branch and version stream.
	Source struct{ Name, Repo, Branch, Stream string }
	// Config selects one release context. Runtime timing and credentials live elsewhere.
	Config struct {
		Version, Branch string
		Components      []Source
		PinningPR       int
	}
	// PR includes historical merge identity for tag coverage.
	PR struct {
		Target                                         Target
		SHA, Title, Author, URL, State, Base, MergeSHA string
		Merged, OptOut                                 bool
		Evidence                                       Evidence
		Checks                                         []Check
		ChecksEvidence                                 Evidence
	}
	// Check is display evidence, never application-level retry eligibility.
	Check struct {
		Name, Status, Conclusion, URL string
		Required                      Truth
	}
	// Tag retains branch membership and merge containment from commit comparisons.
	Tag struct {
		Name, SHA string
		Belongs   Truth
		Contains  map[string]Truth
		Evidence  Evidence
	}
	// Pins separates actual branch pins from a proposed PR's values.
	Pins struct {
		Versions, Paths map[string]string
		// Failures records the safe read error for each component version file.
		Failures map[string]string
		Evidence Evidence
	}
	// Repository is a partial, revision-bound discovery result.
	Repository struct {
		Source Source
		Head   string
		PRs    []PR
		Tags   []Tag
		Pins   Pins
		// PinningPins uses the merge commit for a merged PR and its head otherwise.
		PinningPins Pins
		PinningPR   *PR
		// PinningEvidence records the PR metadata request, including failure when no PR is returned.
		PinningEvidence   Evidence
		PreviousPublished Truth
		Evidence          Evidence
	}
	// Image describes an expected reference and registry evidence. Empty references are unresolved.
	Image struct {
		Component, Family, Reference string
		Name, DefinitionURL          string
		Present                      Truth
		Evidence                     Evidence
	}
	// ImageStatus explains one expected image check without hiding partial results.
	ImageStatus struct {
		Image       Image
		SelectedTag string
		Status      Truth
		Reason      string
		Required    bool
	}
	// PinSummary compares selected versions with actual branch pins and a proposed PR.
	PinSummary struct {
		BranchStatus                Truth
		BranchSummary, BranchReason string
		// BranchProblems lists specific missing inputs or failed requests.
		BranchProblems             []string
		PRNumber                   int
		PRURL, PRSummary, PRReason string
		PRStatus                   Truth
		// PRProblems lists specific missing inputs or failed PR requests.
		PRProblems []string
	}
	// Inventory binds image observations to the ACS release and confirmed component versions.
	Inventory struct {
		// ReleaseVersion is the normalized ACS version used to compute image names.
		ReleaseVersion string
		Versions       map[string]string
		Images         []Image
		// ReleaseImages records the later ACS RC.0 build, independently of component prerequisites.
		ReleaseImages []Image
		Evidence      Evidence
	}
	// Run exposes stable library-owned run evidence without library types.
	Run struct {
		Job, ID, Status, Outcome, URL string
		Eligible                      bool
		// Image records an observed build output, separately from retry eligibility.
		Image Image
	}
	// Advisory is a separately acknowledged finding, never an implicit gate.
	Advisory struct{ ID, Message, URL string }
	// Proposal binds one exact comment to one observed run.
	Proposal struct {
		Target                   Target
		SHA, Job, RunID, Payload string
		At                       time.Time
	}
	// Plan preserves useful partial retry decisions and uncertainty.
	Plan struct {
		Target               Target
		SHA, Action, Message string
		Runs                 []Run
		Proposals            []Proposal
		Warnings             []string
		Advisories           []Advisory
		Evidence             Evidence
	}
	// Choices contains only session release decisions.
	Choices struct {
		PRs          map[Target]Choice
		Tags         map[string]string
		Acknowledged map[string]bool
	}
	// Input is the complete immutable input to pure release assessment.
	Input struct {
		Config       Config
		Choices      Choices
		Repositories []Repository
		Inventory    Inventory
	}
	// Condition explains one readiness predicate and its evidence.
	Condition struct {
		Name     string
		Status   Truth
		Reason   string
		Evidence []Evidence
	}
	// Assessment contains derived readiness and copyable recommendations.
	Assessment struct {
		Ready           bool
		Conditions      []Condition
		Recommendations []string
		Commands        string
		ProposedTags    map[string]string
		ReleaseGuides   map[string]string
		Images          []ImageStatus
		ImageProblem    string
		Pins            PinSummary
	}
)

const (
	// Yes is positively established evidence.
	Yes Truth = "yes"
	// No is a positively established negative result.
	No Truth = "no"
	// Included records a selected PR.
	Included Choice = "included"
	// Excluded keeps a PR monitored outside the selected scope.
	Excluded Choice = "excluded"
	// Ignored stops detailed reads and all writes.
	Ignored Choice = "ignored"
)

var (
	versionRE = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	repoRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	refRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// Version parses final semantic versions only; prereleases are ineligible.
func Version(s string) (major, minor, patch int, ok bool) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return
	}
	major, e1 := strconv.Atoi(m[1])
	minor, e2 := strconv.Atoi(m[2])
	patch, e3 := strconv.Atoi(m[3])
	return major, minor, patch, e1 == nil && e2 == nil && e3 == nil
}

// ValidRef excludes shell metacharacters and Git ref ambiguity at input boundaries.
func ValidRef(s string) bool {
	return refRE.MatchString(s) && !strings.Contains(s, "..") && !strings.Contains(s, "//") && !strings.HasSuffix(s, "/") && !strings.HasSuffix(s, ".lock")
}

// ValidRepo validates GitHub repository identities.
func ValidRepo(s string) bool { return repoRE.MatchString(s) && !strings.Contains(s, "..") }

// Validate applies identically to CLI and browser configuration.
func (c Config) Validate() error {
	major, minor, _, ok := Version(c.Version)
	if !ok || (major != 4 && major != 5) {
		return fmt.Errorf("ACS version must be a final 4.x or 5.x patch version")
	}
	if major == 4 && minor < 10 {
		return fmt.Errorf("ACS 4.9 and older are not supported")
	}
	if !ValidRef(c.Branch) || c.PinningPR < 0 {
		return fmt.Errorf("an explicit ACS branch and nonnegative pinning PR are required")
	}
	names := map[string]bool{}
	for _, s := range c.Components {
		if names[s.Name] || (s.Name != "fact" && s.Name != "collector" && s.Name != "scanner") {
			return fmt.Errorf("invalid or repeated component %q", s.Name)
		}
		names[s.Name] = true
		if !ValidRepo(s.Repo) || !ValidRef(s.Branch) {
			return fmt.Errorf("invalid repository or branch for %s", s.Name)
		}
		a, b, _, ok := Version(s.Stream + ".0")
		if !ok || fmt.Sprintf("%d.%d", a, b) != s.Stream {
			return fmt.Errorf("%s stream must be major.minor", s.Name)
		}
	}
	if !names["fact"] || !names["collector"] || (major == 4 && !names["scanner"]) || (major == 5 && names["scanner"]) {
		return fmt.Errorf("fact and collector are required; scanner is required only for ACS 4.x")
	}
	return nil
}

// ACS returns the configured StackRox release source.
func (c Config) ACS() Source { return Source{Name: "acs", Repo: "stackrox/stackrox", Branch: c.Branch} }

// PinningTarget identifies the explicitly supplied PR; zero means none was supplied.
func (c Config) PinningTarget() Target {
	if c.PinningPR <= 0 {
		return Target{}
	}
	return Target{Repo: c.ACS().Repo, PR: c.PinningPR}
}

// String gives a stable display and map key representation.
func (t Target) String() string {
	if t.PR > 0 {
		return fmt.Sprintf("%s#%d", t.Repo, t.PR)
	}
	return t.Repo + "@" + t.Tag
}

// Valid verifies that exactly one target kind is specified.
func (t Target) Valid() bool {
	return ValidRepo(t.Repo) && ((t.PR > 0 && t.Tag == "") || (t.PR == 0 && ValidRef(t.Tag)))
}

// Fresh requires a successful current observation.
func (e Evidence) Fresh() bool { return e.Known && e.Error == "" && !e.At.IsZero() }

// Result classifies a check without granting retry permission.
func (c Check) Result() Truth {
	result := c.Conclusion
	if result == "" {
		result = c.Status
	}
	switch result {
	case "success", "neutral", "skipped":
		return Yes
	case "failure", "error", "cancelled", "timed_out", "action_required", "startup_failure", "stale":
		return No
	default:
		return ""
	}
}

// ChecksStatus requires current evidence and successful required checks.
func (p PR) ChecksStatus() Truth {
	if !p.Evidence.Fresh() || !p.ChecksEvidence.Fresh() {
		return ""
	}
	status := Yes
	for _, check := range p.Checks {
		if check.Required == No {
			continue
		}
		if check.Required == "" {
			status = ""
			continue
		}
		switch check.Result() {
		case No:
			return No
		case "":
			status = ""
		}
	}
	return status
}
