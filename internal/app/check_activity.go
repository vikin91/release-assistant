package app

import "release-assistant/internal/release"

type (
	checkActivity struct {
		state checkState
		note  string
	}
	checkState uint8
)

const (
	checksUnobserved checkState = iota
	checksAbsent
	checksPassed
	checksUnverified
	checksPending
	checksFailed
)

func checkStateOf(check release.Check) checkState {
	switch {
	case check.Result() == release.No:
		return checksFailed
	case check.Running():
		return checksPending
	case check.Result() == release.Yes:
		return checksPassed
	default:
		return checksUnverified
	}
}

func prCheckActivity(pr release.PR) checkActivity {
	if pr.ChecksEvidence.At.IsZero() && pr.CheckResultsEvidence.At.IsZero() {
		return checkActivity{}
	}
	if !pr.Evidence.Fresh() {
		return checkActivity{state: checksUnverified}
	}
	if pr.ChecksEvidence.Fresh() {
		state := checksAbsent
		for _, check := range pr.Checks {
			if check.Required == release.Yes {
				state = max(state, checkStateOf(check))
			}
		}
		return checkActivity{state: state}
	}
	activity := checkActivity{state: checksUnverified, note: " Required checks could not be verified."}
	if pr.CheckResultsEvidence.Known {
		state := checksAbsent
		for _, check := range pr.Checks {
			state = max(state, checkStateOf(check))
		}
		if state >= checksPending || (state == checksPassed && pr.CheckResultsEvidence.Fresh()) {
			activity.state = state
		}
	}
	return activity
}

func rcCheckActivity(validation release.Validation) checkActivity {
	if validation.Evidence.At.IsZero() {
		return checkActivity{}
	}
	state := checksAbsent
	for _, check := range validation.Checks {
		state = max(state, checkStateOf(check.Check))
	}
	if !validation.Evidence.Fresh() {
		if state >= checksPending {
			return checkActivity{state: state, note: " Some check results could not be verified."}
		}
		state = checksUnverified
	}
	return checkActivity{state: state}
}

func (s *Session) recordCheckChange(target release.Target, sha string, before, after checkActivity) {
	if after == before || after.state == checksUnobserved {
		return
	}
	subject := "GitHub validation checks"
	if target.PR > 0 {
		subject = "Required GitHub checks"
		if after.note != "" || after.state == checksUnverified {
			subject = "GitHub checks"
		}
	}
	decision := "github-checks-changed"
	status := "unverified"
	switch after.state {
	case checksAbsent:
		status = "absent"
	case checksPassed:
		status = "passing"
	case checksPending:
		status = "pending"
	case checksFailed:
		status = "failing"
	}
	if after.state == checksFailed || after.state == checksUnverified || after.note != "" {
		decision = "github-checks-attention"
	}
	message := subject + " are " + status + " for " + consoleRevision(target, sha) + "." + after.note
	s.record(target, sha, decision, "", status, "system", message)
}
