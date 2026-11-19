package release

// Candidate binds an observed release candidate and its branch comparison to a tag commit.
type Candidate struct {
	Target   Target
	SHA      string
	Number   uint64
	Evidence Evidence
	Ahead    int
	// CompareURL identifies the exact commit range used to count post-RC commits.
	CompareURL string
	PRs        []CandidatePR
	// PRsPending indicates that the verified commit range is still being checked for associated PRs.
	PRsPending bool
	// PRError indicates that the count is verified but the PR list may be incomplete.
	PRError string
}

// CandidatePR identifies a merged pull request associated with a post-RC commit.
type CandidatePR struct {
	Number int
	Title  string
	URL    string
}
