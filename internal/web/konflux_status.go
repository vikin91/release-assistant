package web

import (
	"slices"
	"strings"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type konfluxClusterStatus struct {
	Cluster  string
	Reads    []konflux.ReadIssue
	Rejected release.StartFailure
	Tab      string
}

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
			if failure.Category != "admission_policy" || failure.Superseded || !failure.Evidence.Fresh() || age < 0 || age > recentStart {
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

// BuildSummary describes the verified build evidence for one target.
func (p planModel) BuildSummary() string {
	result, running := p.Plan.BuildStatus()
	switch {
	case result == release.No:
		return "Build failed"
	case running:
		return "Build running"
	case result == release.Yes:
		return "Build passed"
	default:
		return "Unverified"
	}
}

// KonfluxLabel surfaces the most urgent observation across monitored targets.
func (p pageModel) KonfluxLabel() string {
	if len(p.KonfluxStatus) > 0 {
		return "Needs attention"
	}
	if len(p.Plans) == 0 {
		return "Awaiting evidence"
	}
	label := "Builds passed"
	for _, item := range p.Plans {
		result, running := item.Plan.BuildStatus()
		switch {
		case result == release.No:
			return "Build failed"
		case running:
			label = "Build running"
		case result != release.Yes && label != "Build running":
			label = "Unverified"
		}
	}
	return label
}
