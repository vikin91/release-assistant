package release

import (
	"time"

	retry "github.com/vikin91/ci-retest"
)

// ManualRetryWarning explains the limits of job eligibility to the engineer.
const ManualRetryWarning = retry.ManualRetryWarning

type (
	// ManualRetryReport contains display-only retry instructions for a target revision.
	ManualRetryReport struct {
		Target   Target
		SHA, URL string
		At       time.Time
		Jobs     []ManualJob
		Prow     []ProwRetry
		Warnings []string
		// OtherChecks lists failed checks missing from both ci-retest ownership lists.
		OtherChecks []string
		// Pending marks a revision awaiting its first retry report.
		Pending bool
		// Incomplete marks a read that failed to verify all retry results.
		Incomplete bool
	}
	// ProwRetry pairs a failed status context with its configured presubmit command.
	ProwRetry = retry.ProwRetry
	// ManualJob identifies an Actions job with a verified rerun destination.
	ManualJob = retry.ActionsJob
)

// ProwComment returns the verified commands as a single copyable GitHub comment.
func (r ManualRetryReport) ProwComment() string {
	return (retry.Result{Prow: r.Prow}).ProwComment()
}
