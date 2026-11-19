package web

import (
	"cmp"
	"crypto/sha256"
	"fmt"
	"path"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type (
	startFailureReport struct {
		Key              string
		Target           release.Target
		Current, Earlier []startFailureCluster
	}
	startFailureCluster struct {
		Name     string
		Failures []startFailureModel
	}
	startFailureModel struct {
		Key, Repo string
		Failure   release.StartFailure
		Commits   []string
		Targets   []release.Target
	}
	startFailureKey struct {
		cluster, repo string
		checkID       int64
		fallback      string
	}
)

// startFailureReports keeps observations of a shared commit scoped to each monitored ACS PR.
func startFailureReports(plans []planModel, tab string) []startFailureReport {
	if tab != "acs-prs" {
		current, earlier := groupStartFailures(plans, tab, false)
		if len(current)+len(earlier) == 0 {
			return nil
		}
		return []startFailureReport{{Key: tab, Current: current, Earlier: earlier}}
	}
	byPR := make(map[release.Target][]planModel)
	for _, plan := range plans {
		if len(plan.Plan.StartFailures) > 0 {
			target := plan.Plan.Target
			byPR[target] = append(byPR[target], plan)
		}
	}
	var reports []startFailureReport
	for target, observations := range byPR {
		key := fmt.Sprintf("%s-%d", tab, target.PR)
		current, earlier := groupStartFailures(observations, key, true)
		reports = append(reports, startFailureReport{Key: key, Target: target, Current: current, Earlier: earlier})
	}
	slices.SortFunc(reports, func(a, b startFailureReport) int { return cmp.Compare(a.Target.PR, b.Target.PR) })
	return reports
}

// groupStartFailures deduplicates check evidence across the activity's monitored targets.
func groupStartFailures(plans []planModel, scope string, groupedByPR bool) (reported, earlier []startFailureCluster) {
	groups := make(map[string][]startFailureModel)
	positions := make(map[startFailureKey]int)
	for _, plan := range plans {
		for _, failure := range plan.Plan.StartFailures {
			cluster := strings.TrimSpace(failure.Cluster)
			key := startFailureKey{cluster: cluster, repo: plan.Plan.Target.Repo, checkID: failure.CheckID}
			if failure.CheckID <= 0 {
				key.fallback = plan.Plan.SHA + "\x00" + failure.Job + "\x00" + failure.Category + "\x00" + failure.Reason
			}
			rows := groups[cluster]
			index, exists := positions[key]
			if !exists {
				index = len(rows)
				positions[key] = index
				rows = append(rows, startFailureModel{
					Key: fmt.Sprintf("%s:%s:%d:%x", scope, key.repo, key.checkID, sha256.Sum256([]byte(cluster+"\x00"+key.fallback))), Repo: key.repo,
					Failure: failure,
				})
			}
			row := &rows[index]
			superseded := row.Failure.Superseded && failure.Superseded
			if failure.Evidence.Fresh() && !row.Failure.Evidence.Fresh() ||
				failure.Evidence.Fresh() == row.Failure.Evidence.Fresh() && failure.Evidence.At.After(row.Failure.Evidence.At) {
				row.Failure = failure
			}
			row.Failure.Superseded = superseded
			if plan.Plan.SHA != "" && !slices.Contains(row.Commits, plan.Plan.SHA) {
				row.Commits = append(row.Commits, plan.Plan.SHA)
			}
			if !groupedByPR && !slices.Contains(row.Targets, plan.Plan.Target) {
				row.Targets = append(row.Targets, plan.Plan.Target)
			}
			groups[cluster] = rows
		}
	}
	for cluster, failures := range groups {
		slices.SortFunc(failures, func(a, b startFailureModel) int { return strings.Compare(a.Key, b.Key) })
		currentGroup := startFailureCluster{Name: cluster}
		earlierGroup := startFailureCluster{Name: cluster}
		for i := range failures {
			slices.Sort(failures[i].Commits)
			slices.SortFunc(failures[i].Targets, func(a, b release.Target) int { return strings.Compare(a.String(), b.String()) })
			if failures[i].Failure.Superseded {
				earlierGroup.Failures = append(earlierGroup.Failures, failures[i])
			} else {
				currentGroup.Failures = append(currentGroup.Failures, failures[i])
			}
		}
		if len(currentGroup.Failures) > 0 {
			reported = append(reported, currentGroup)
		}
		if len(earlierGroup.Failures) > 0 {
			earlier = append(earlier, earlierGroup)
		}
	}
	byCluster := func(a, b startFailureCluster) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(reported, byCluster)
	slices.SortFunc(earlier, byCluster)
	return reported, earlier
}

// RepoName returns the short repository name for monitored PR and tag labels.
func (f startFailureModel) RepoName() string {
	return path.Base(f.Repo)
}
