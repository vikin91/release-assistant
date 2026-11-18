package web

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

type (
	startFailureCluster struct {
		Name     string
		Failures []startFailureModel
	}
	startFailureModel struct {
		Key, Repo, Category, Namespace string
		Failure                        release.StartFailure
		Commits                        []string
		Targets                        []release.Target
	}
	startFailureKey struct {
		cluster, repo string
		checkID       int64
		fallback      string
	}
)

var pipelineRunNamespaceRE = regexp.MustCompile(
	`\bcreating pipelinerun [a-z0-9][a-z0-9.-]* in namespace ([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?) has failed\b`,
)

// groupStartFailures deduplicates check evidence across the activity's monitored targets.
func groupStartFailures(plans []planModel, tab string) (reported, earlier []startFailureCluster) {
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
					Key: fmt.Sprintf("%s:%s:%d:%x", tab, key.repo, key.checkID, sha256.Sum256([]byte(cluster+"\x00"+key.fallback))), Repo: key.repo,
					Failure: failure, Category: startFailureCategory(failure.Category),
				})
			}
			row := &rows[index]
			superseded := row.Failure.Superseded && failure.Superseded
			if failure.Evidence.Fresh() && !row.Failure.Evidence.Fresh() ||
				failure.Evidence.Fresh() == row.Failure.Evidence.Fresh() && failure.Evidence.At.After(row.Failure.Evidence.At) {
				row.Failure = failure
				row.Category = startFailureCategory(failure.Category)
			}
			row.Failure.Superseded = superseded
			if plan.Plan.SHA != "" && !slices.Contains(row.Commits, plan.Plan.SHA) {
				row.Commits = append(row.Commits, plan.Plan.SHA)
			}
			if !slices.Contains(row.Targets, plan.Plan.Target) {
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
			if match := pipelineRunNamespaceRE.FindStringSubmatch(failures[i].Failure.Reason); match != nil {
				failures[i].Namespace = match[1]
			}
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

func startFailureCategory(category string) string {
	switch category {
	case "admission_policy":
		return "Admission policy rejection"
	case "other":
		return "Other creation failure"
	default:
		return "Creation failure (category not reported)"
	}
}
