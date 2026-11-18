package web

import (
	"slices"
	"strings"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type (
	konfluxAdmission     struct{ Class, Label string }
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
	for _, cluster := range p.KonfluxStatus {
		if cluster.Tab != "" {
			return konfluxAdmission{"rejected", "Start rejected"}
		}
	}
	if len(p.KonfluxStatus) == 0 {
		for _, plan := range p.Plans {
			if !plan.Plan.Evidence.Fresh() || time.Since(plan.Plan.Evidence.At) > 30*time.Minute {
				continue
			}
			for _, run := range plan.Plan.Runs {
				if run.ID != "" && run.Status == "in_progress" && run.Outcome == "" {
					return konfluxAdmission{"accepted", "Pipeline accepted"}
				}
			}
		}
	}
	return konfluxAdmission{"unverified", "Admission unverified"}
}
