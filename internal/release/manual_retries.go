package release

import (
	"fmt"
	"strings"
)

// ManualRetryWarning explains the limits of job eligibility to the engineer.
const ManualRetryWarning = "This tool cannot tell flakes from true failures. Inspect each failure before retrying."

type (
	// ManualRetryReport contains display-only retry instructions for a target revision.
	ManualRetryReport struct {
		Target   Target
		SHA, URL string
		Jobs     []ManualJob
		Prow     []ProwRetry
		Warnings []string
		// OtherChecks explains ownership of failed checks handled outside ci-retest.
		OtherChecks []string
		// Pending marks a revision awaiting its first retry report.
		Pending bool
		// Incomplete marks a read that failed to verify all retry results.
		Incomplete bool
	}
	// ProwRetry pairs a failed status context with its configured presubmit command.
	ProwRetry struct{ Context, Command string }
	// ManualJob identifies an Actions job with a verified rerun destination.
	ManualJob struct {
		ID                      int64
		Name, URL, WorkflowName string
	}
)

// RerunCommand returns the GitHub CLI command for this job in its target repository.
func (j ManualJob) RerunCommand(repo string) string {
	return fmt.Sprintf("gh api --method POST repos/%s/actions/jobs/%d/rerun", repo, j.ID)
}

// ProwComment returns the verified commands as a single copyable GitHub comment.
func (r ManualRetryReport) ProwComment() string {
	lines := make([]string, len(r.Prow))
	for i, job := range r.Prow {
		lines[i] = job.Command
	}
	return strings.Join(lines, "\n")
}
