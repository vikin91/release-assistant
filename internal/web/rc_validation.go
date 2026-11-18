package web

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"release-assistant/internal/release"
)

type (
	rcValidationRow struct {
		Key, Name, Status, Class, URL, LinkLabel string
		WorkflowName, WorkflowURL                string
		Retry                                    *rcRetryModel
	}
	rcValidationGroup struct {
		Key, WorkflowName string
		Rows              []rcValidationRow
	}
)

func (rc rcModel) validationStatus() tabStatus {
	status := tabStatus{State: tabComplete, Reason: "No failed, canceled, or running validation checks in the current results."}
	validation := rc.Plan.Plan.Validation
	switch {
	case validation.Evidence.Error != "":
		status = tabStatus{State: tabNeedsAttention, Reason: "Some RC validation evidence is unavailable."}
	case !validation.Evidence.Fresh() || !rc.ValidationObserved:
		status = tabStatus{State: tabInProgress, Reason: "Waiting for RC validation checks."}
	}
	for _, group := range rc.ValidationGroups {
		for _, row := range group.Rows {
			switch {
			case row.Retry != nil:
				status.add(tabActionNeeded, "A failed or canceled RC validation check can be retested.")
			case row.Class == "no":
				status.add(tabNeedsAttention, "An RC validation check failed or was canceled.")
			case row.Class == "unknown":
				status.add(tabInProgress, "RC validation checks are still running.")
			}
		}
	}
	return status
}

func (p *pageModel) prepareRCValidation(plan release.Plan, runs map[string]release.Run, retries map[string]*buildRetryCandidate, imageJobs map[string]bool) {
	var rows []rcValidationRow
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
		row := p.rcValidationRow(check, "konflux:"+run.ID, "")
		if row.Status == "" {
			continue
		}
		if candidate := retries[run.Job]; rcRetryMatches(candidate, run, plan.SHA) {
			row.Retry = new(p.rcRetry(candidate))
			row.Retry.Label = "Retest Konflux check"
		}
		rows = append(rows, row)
	}

	seenKonflux := make(map[string]bool)
	for _, observed := range plan.Validation.Checks {
		check := observed.Check
		job, prefixed := strings.CutPrefix(check.Name, "Red Hat Konflux / ")
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
		}
		key := "github:" + strconv.FormatInt(observed.ID, 10) + ":" + check.Name
		row := p.rcValidationRow(check, key, observed.WorkflowURL)
		p.RC.ValidationObserved = true
		if row.Status != "" {
			row.WorkflowName = observed.WorkflowName
			rows = append(rows, row)
		}
	}
	slices.SortStableFunc(rows, func(a, b rcValidationRow) int {
		if a.Class != b.Class {
			return cmp.Compare(a.Class, b.Class)
		}
		return cmp.Compare(a.Name, b.Name)
	})
	groupByRun := make(map[string]int)
	for _, row := range rows {
		group := rcValidationGroup{Key: row.Key, WorkflowName: row.WorkflowName, Rows: []rcValidationRow{row}}
		if row.WorkflowName != "" && row.WorkflowURL != "" {
			if index, ok := groupByRun[row.WorkflowURL]; ok {
				existing := &p.RC.ValidationGroups[index]
				existing.Rows = append(existing.Rows, row)
				continue
			}
			group.Key = row.WorkflowURL
			groupByRun[row.WorkflowURL] = len(p.RC.ValidationGroups)
		}
		p.RC.ValidationGroups = append(p.RC.ValidationGroups, group)
	}
}

func conformaScenario(name string) (string, bool) {
	scenario, _, _ := strings.Cut(name, " / ")
	return scenario, strings.HasPrefix(scenario, "acs-conforma-")
}

func (p pageModel) rcValidationRow(check release.Check, key, workflowURL string) rcValidationRow {
	row := rcValidationRow{
		Key: key, Name: check.Name, LinkLabel: "View check", WorkflowURL: safeURL(workflowURL),
		URL: safeURL(check.URL),
	}
	if row.URL == "" {
		if row.WorkflowURL != "" {
			row.URL, row.LinkLabel = row.WorkflowURL, "View workflow run"
		} else {
			row.URL = p.RC.ChecksURL
		}
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
