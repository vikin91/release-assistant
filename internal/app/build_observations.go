package app

import "release-assistant/internal/release"

// retainVerifiedBuilds keeps display evidence tied to the same check and commit.
func retainVerifiedBuilds(previous release.Plan, current *release.Plan) {
	if current.SHA == "" || previous.SHA != current.SHA || current.Evidence.Error == "" {
		return
	}
	for i := range current.Runs {
		run := &current.Runs[i]
		if run.ID != "" || run.Status != "unknown" || run.CheckID <= 0 {
			continue
		}
		for _, old := range previous.Runs {
			if old.CheckID != run.CheckID || old.Job != run.Job {
				continue
			}
			if old.ID != "" {
				run.LastVerified = release.BuildObservation{Status: old.Status, Outcome: old.Outcome, At: previous.Evidence.At}
			} else {
				run.LastVerified = old.LastVerified
			}
			break
		}
	}
}
