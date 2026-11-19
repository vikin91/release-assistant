package release

import "fmt"

const (
	// ManualRetryWarning explains the limits of job eligibility to the engineer.
	ManualRetryWarning = "This tool cannot tell flakes from true failures. Inspect each failure before retrying."
	// ProwRetryComment requests a PR-wide retry from Prow and other subscribed CI.
	ProwRetryComment = "/retest"
)

type (
	// ManualRetryReport contains display-only retry instructions for a target revision.
	ManualRetryReport struct {
		Target         Target
		SHA, URL       string
		Jobs           []ManualJob
		Prow, Warnings []string
	}
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
