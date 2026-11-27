package release

type (
	// RetryPlan carries the eligible actions and evidence from one retry planner read.
	RetryPlan struct {
		Target   Target
		SHA      string
		Evidence Evidence
		Warnings []string
		Manual   ManualRetries
		Comments []Proposal

		konflux konfluxRetryDetails
	}

	// ManualRetries carries Actions and Prow offers for an engineer to execute.
	ManualRetries struct {
		Jobs        []ManualJob
		Prow        []ProwRetry
		OtherChecks []string

		pending, incomplete bool
	}

	konfluxRetryDetails struct {
		action, message string
		runs            []Run
		advisories      []Advisory
		startFailures   []StartFailure
		diagnostics     []KonfluxDiagnostic
		validation      Validation
		apiUnavailable  bool
	}
)

// RetryPlan exposes CI Retest offers through the shared planning contract.
func (r ManualRetryReport) RetryPlan() RetryPlan {
	return RetryPlan{
		Target: r.Target, SHA: r.SHA, Warnings: r.Warnings,
		Evidence: Evidence{At: r.At, URL: r.URL, Known: r.SHA != "" && !r.Incomplete && !r.Pending},
		Manual: ManualRetries{
			Jobs: r.Jobs, Prow: r.Prow, OtherChecks: r.OtherChecks,
			pending: r.Pending, incomplete: r.Incomplete,
		},
	}
}

// ManualReport gives the CI dashboard the observed offers and diagnostics.
func (p RetryPlan) ManualReport() ManualRetryReport {
	return ManualRetryReport{
		Target: p.Target, SHA: p.SHA, At: p.Evidence.At, URL: p.Evidence.URL,
		Warnings: p.Warnings, Jobs: p.Manual.Jobs, Prow: p.Manual.Prow,
		OtherChecks: p.Manual.OtherChecks, Pending: p.Manual.pending, Incomplete: p.Manual.incomplete,
	}
}

// RetryPlan exposes Konflux offers while retaining the build observations from the same read.
func (p Plan) RetryPlan() RetryPlan {
	return RetryPlan{
		Target: p.Target, SHA: p.SHA, Evidence: p.Evidence, Warnings: p.Warnings,
		Comments: p.Proposals,
		konflux: konfluxRetryDetails{
			action: p.Action, message: p.Message, runs: p.Runs,
			advisories: p.Advisories, startFailures: p.StartFailures,
			diagnostics: p.Diagnostics,
			validation:  p.Validation, apiUnavailable: p.KonfluxAPIUnavailable,
		},
	}
}

// KonfluxPlan gives the build dashboard and dispatch the observations from the planner read.
func (p RetryPlan) KonfluxPlan() Plan {
	return Plan{
		Target: p.Target, SHA: p.SHA, Evidence: p.Evidence, Warnings: p.Warnings,
		Proposals: p.Comments, Action: p.konflux.action, Message: p.konflux.message,
		Runs: p.konflux.runs, Advisories: p.konflux.advisories, StartFailures: p.konflux.startFailures,
		Diagnostics: p.konflux.diagnostics,
		Validation:  p.konflux.validation, KonfluxAPIUnavailable: p.konflux.apiUnavailable,
	}
}
