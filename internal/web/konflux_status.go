package web

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

type (
	konfluxAdmission struct {
		Class, Label, Detail, SignInCluster string
		Last, Next, Created                 time.Time
		APIReadable                         bool
	}
	konfluxClusterStatus struct {
		Cluster  string
		Reads    []konflux.ReadIssue
		Rejected release.StartFailure
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
	for _, item := range p.Plans {
		for _, failure := range item.Plan.StartFailures {
			age := now.Sub(failure.CheckStartedAt)
			if failure.Superseded || !failure.Evidence.Fresh() || age < 0 || age > recentStart {
				continue
			}
			cluster := forCluster(failure.Cluster)
			if failure.CheckStartedAt.After(cluster.Rejected.CheckStartedAt) {
				cluster.Rejected = failure
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

// KonfluxAdmission renders the independent health probe for the header.
func (p pageModel) KonfluxAdmission() konfluxAdmission {
	health := p.Snapshot.KonfluxHealth
	if health == nil || health.CheckedAt.IsZero() {
		return konfluxAdmission{Class: "pending", Label: "Unknown"}
	}
	status := konfluxAdmission{
		Class: "unverified", Label: "Unverified", Detail: health.Problem,
		Last: health.CheckedAt, Next: health.Next, Created: health.AdmittedAt, APIReadable: health.APIReadable,
	}
	switch {
	case health.Healthy():
		status.Class, status.Label = "accepted", "Healthy"
	case health.APIReadable:
		if status.Detail == "" {
			status.Detail = "No PipelineRun admission observed in the last 30 minutes."
		}
	case health.APIStatus == http.StatusUnauthorized:
		status.Class, status.Label, status.SignInCluster = "unavailable", "Sign-in required", health.Cluster
	case health.APIStatus == http.StatusForbidden:
		status.Class, status.Label = "unavailable", "API access denied"
	case health.APIStatus == 0 || health.APIStatus >= 500:
		status.Class, status.Label = "unavailable", "API unavailable"
	}
	return status
}
