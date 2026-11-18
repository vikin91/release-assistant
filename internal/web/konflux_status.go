package web

import (
	"slices"
	"strings"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type (
	konfluxAdmission struct {
		Class, Label, Detail string
		Last, Next           time.Time
	}
	konfluxClusterStatus struct {
		Cluster  string
		Reads    []konflux.ReadIssue
		Rejected release.StartFailure
		Tab      string
	}
)

func (p pageModel) konfluxStatus(now time.Time) []konfluxClusterStatus {
	const recentStart = 30 * time.Minute
	clusters := make(map[string]*konfluxClusterStatus)
	forCluster := func(cluster string) *konfluxClusterStatus {
		key := strings.ToLower(strings.TrimSpace(cluster))
		if clusters[key] == nil {
			clusters[key] = &konfluxClusterStatus{Cluster: key}
		}
		return clusters[key]
	}
	for _, issue := range p.Snapshot.KonfluxReadIssues {
		cluster := forCluster(issue.Cluster)
		cluster.Reads = append(cluster.Reads, issue)
	}
	acsRepo := p.Snapshot.Config.ACS().Repo
	for _, item := range p.Plans {
		for _, failure := range item.Plan.StartFailures {
			age := now.Sub(failure.CheckStartedAt)
			if failure.Superseded || !failure.Evidence.Fresh() || age < 0 || age > recentStart {
				continue
			}
			cluster := forCluster(failure.Cluster)
			if failure.CheckStartedAt.After(cluster.Rejected.CheckStartedAt) {
				cluster.Rejected = failure
				cluster.Tab = planTab(item.Plan.Target, acsRepo)
			}
		}
	}
	status := make([]konfluxClusterStatus, 0, len(clusters))
	for _, cluster := range clusters {
		status = append(status, *cluster)
	}
	slices.SortFunc(status, func(a, b konfluxClusterStatus) int { return strings.Compare(a.Cluster, b.Cluster) })
	return status
}

func planTab(target release.Target, acsRepo string) string {
	switch {
	case target.Repo == acsRepo && target.PR > 0:
		return "acs-prs"
	case target.Repo == acsRepo:
		return "acs-rcs"
	case target.PR > 0:
		return "component-prs"
	default:
		return "component-releases"
	}
}

// KonfluxAdmission describes observed pipeline starts independently of build outcomes.
func (p pageModel) KonfluxAdmission() konfluxAdmission {
	var failure konfluxAdmission
	var started time.Time
	result := konfluxAdmission{Class: "unverified", Label: "Unknown"}
	var active konfluxAdmission
	for _, item := range p.Plans {
		plan := item.Plan
		next := p.Snapshot.NextSourceCheck[plan.Target.Repo]
		for _, f := range plan.StartFailures {
			if f.Superseded || !f.Evidence.Fresh() || f.Reason == "" || (!started.IsZero() && !f.CheckStartedAt.After(started)) {
				continue
			}
			started = f.CheckStartedAt
			failure = konfluxAdmission{"unverified", "Unknown", f.Reason, f.Evidence.At, next}
			if age := time.Since(started); age >= 0 && age <= 30*time.Minute {
				failure.Class, failure.Label = "rejected", "Unhealthy"
			}
		}
		if plan.Evidence.At.After(result.Last) {
			result.Last, result.Next = plan.Evidence.At, next
			result.Detail = plan.Evidence.Error
			if result.Detail == "" {
				for _, warning := range plan.Warnings {
					if strings.HasPrefix(warning, "identity unknown:") {
						result.Detail = warning
						break
					}
				}
			}
		}
		if !plan.Evidence.Fresh() || time.Since(plan.Evidence.At) > 30*time.Minute {
			continue
		}
		for _, run := range plan.Runs {
			if run.ID != "" && run.Status == "in_progress" && run.Outcome == "" && plan.Evidence.At.After(active.Last) {
				active = konfluxAdmission{"accepted", "Healthy", "", plan.Evidence.At, next}
			}
		}
	}
	if failure.Class == "rejected" {
		return failure
	}
	var readIssue *konflux.ReadIssue
	for _, cluster := range p.KonfluxStatus {
		for _, issue := range cluster.Reads {
			if readIssue == nil || issue.LastFailure.After(readIssue.LastFailure) {
				readIssue = &issue
			}
		}
	}
	if readIssue != nil {
		return konfluxAdmission{"unverified", "Unknown", readIssue.Endpoint + " read failed: " + readIssue.Reason, readIssue.LastFailure, readIssue.RetryAt}
	}
	if active.Class != "" {
		return active
	}
	if failure.Detail != "" {
		return failure
	}
	return result
}
