package web

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"release-assistant/internal/release"
)

type rcValidationRow struct {
	Key, Name, Status, Class, Source, Trigger, URL, WorkflowURL string
	Retry                                                       *rcRetryModel
}

func (rc rcModel) validationStatus() tabStatus {
	status := tabStatus{State: tabComplete, Reason: "No failed, canceled, or running validation checks in the current results."}
	validation := rc.Plan.Plan.Validation
	switch {
	case validation.Evidence.Error != "":
		status = tabStatus{State: tabNeedsAttention, Reason: "Some RC validation evidence is unavailable."}
	case !validation.Evidence.Fresh() || !rc.ValidationObserved:
		status = tabStatus{State: tabInProgress, Reason: "Waiting for RC validation checks."}
	}
	for _, row := range rc.ValidationRows {
		switch {
		case row.Retry != nil:
			status.add(tabActionNeeded, "A failed or canceled RC validation check can be retested.")
		case row.Class == "no":
			status.add(tabNeedsAttention, "An RC validation check failed or was canceled.")
		case row.Class == "unknown":
			status.add(tabInProgress, "RC validation checks are still running.")
		}
	}
	return status
}

func (p *pageModel) prepareRCValidation(plan release.Plan, runs map[string]release.Run, retries map[string]*buildRetryCandidate, imageJobs map[string]bool) {
	// A verified Konflux tag run takes precedence over other runs on the same SHA.
	for _, run := range plan.Runs {
		if imageJobs[run.Job] || run.ID == "" {
			continue
		}
		if _, policy := conformaScenario(run.Job); policy {
			continue
		}
		check := release.Check{Name: run.Job, Status: run.Status, Conclusion: run.Outcome, URL: run.URL}
		p.RC.ValidationObserved = true
		row := p.rcValidationRow(check, "konflux:"+run.ID, "Konflux", "Tag push · "+plan.Target.Tag)
		if row.Status == "" {
			continue
		}
		if candidate := retries[run.Job]; rcRetryMatches(candidate, run, plan.SHA) {
			row.Retry = new(p.rcRetry(candidate))
			row.Retry.Label = "Retest Konflux check"
		}
		p.RC.ValidationRows = append(p.RC.ValidationRows, row)
	}

	seenKonflux := make(map[string]bool)
	for _, observed := range plan.Validation.Checks {
		check := observed.Check
		job, prefixed := strings.CutPrefix(check.Name, "Red Hat Konflux / ")
		source := observed.App
		trigger := "Commit check · trigger unverified"
		if prefixed || strings.Contains(strings.ToLower(observed.App), "konflux") {
			scenario, policy := conformaScenario(job)
			if policy && (scenario == "acs-conforma-dev" || strings.HasPrefix(scenario, "acs-conforma-dev-")) {
				continue
			}
			if seenKonflux[job] || imageJobs[job] || (!policy && runs[job].ID != "") {
				continue
			}
			seenKonflux[job] = true
			check.Name = job
			source = "Konflux"
			if policy {
				source = "Conforma"
			}
		} else if observed.App == "github-actions" {
			source = "GitHub Actions"
			if observed.Event != "" {
				trigger = observed.Event
				if observed.Ref != "" {
					trigger += " · " + observed.Ref
				}
			}
		}
		if source == "" {
			source = "GitHub check"
		}
		key := "github:" + strconv.FormatInt(observed.ID, 10) + ":" + check.Name
		row := p.rcValidationRow(check, key, source, trigger)
		p.RC.ValidationObserved = true
		if row.Status != "" {
			row.WorkflowURL = observed.WorkflowURL
			p.RC.ValidationRows = append(p.RC.ValidationRows, row)
		}
	}
	slices.SortStableFunc(p.RC.ValidationRows, func(a, b rcValidationRow) int {
		if a.Class != b.Class {
			return cmp.Compare(a.Class, b.Class)
		}
		return cmp.Compare(a.Name, b.Name)
	})
}

func conformaScenario(name string) (string, bool) {
	scenario, _, _ := strings.Cut(name, " / ")
	return scenario, strings.HasPrefix(scenario, "acs-conforma-")
}

func (p pageModel) rcValidationRow(check release.Check, key, source, trigger string) rcValidationRow {
	row := rcValidationRow{
		Key: key, Name: check.Name, Source: source, Trigger: trigger,
		URL: cmp.Or(safeURL(check.URL), p.RC.ChecksURL),
	}
	switch {
	case check.Conclusion == "cancelled":
		row.Status, row.Class = "Canceled", "no"
	case check.Result() == release.No:
		row.Status, row.Class = "Failed", "no"
	case check.Running():
		row.Status, row.Class = "Running", "unknown"
	}
	return row
}
