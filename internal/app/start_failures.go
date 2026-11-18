package app

import (
	"fmt"
	"slices"
	"strconv"

	"release-assistant/internal/release"
)

func staleStartFailures(failures []release.StartFailure, reason string) []release.StartFailure {
	failures = slices.Clone(failures)
	for i := range failures {
		failures[i].Evidence.Known = false
		failures[i].Evidence.Error = reason
	}
	return failures
}

func markSupersededStartFailures(plan *release.Plan) {
	for i := range plan.StartFailures {
		failure := &plan.StartFailures[i]
		for _, run := range plan.Runs {
			checkID, err := strconv.ParseInt(run.ID, 10, 64)
			if err == nil && failure.CheckID > 0 && run.Job == failure.Job && checkID > failure.CheckID {
				failure.Superseded = true
				break
			}
		}
	}
}

// recordStartFailures groups newly observed check reports without inferring cluster health.
// The caller holds the session lock.
func (s *Session) recordStartFailures(previous, current release.Plan) {
	type group struct{ cluster, category string }
	type observation struct {
		checkID                        int64
		job, cluster, category, reason string
	}
	key := func(failure release.StartFailure) observation {
		return observation{failure.CheckID, failure.Job, failure.Cluster, failure.Category, failure.Reason}
	}
	seen := make(map[observation]bool, len(previous.StartFailures))
	if previous.SHA == current.SHA {
		for _, failure := range previous.StartFailures {
			seen[key(failure)] = true
		}
	}
	counts := map[group]int{}
	changed := map[group]bool{}
	representative := map[group]release.StartFailure{}
	observed := map[observation]bool{}
	for _, failure := range current.StartFailures {
		if failure.Superseded || !failure.Evidence.Fresh() {
			continue
		}
		identity := key(failure)
		if observed[identity] {
			continue
		}
		observed[identity] = true
		g := group{failure.Cluster, failure.Category}
		counts[g]++
		changed[g] = changed[g] || !seen[identity]
		if failure.Reason != "" && failure.CheckID > representative[g].CheckID {
			representative[g] = failure
		}
	}
	var groups []group
	for g := range counts {
		if changed[g] {
			groups = append(groups, g)
		}
	}
	slices.SortFunc(groups, func(a, b group) int {
		return slices.Compare([]string{a.cluster, a.category}, []string{b.cluster, b.category})
	})
	for _, g := range groups {
		cluster := g.cluster
		if cluster == "" {
			cluster = "unknown cluster"
		}
		category := "other creation failure"
		if g.category == "admission_policy" {
			category = "admission policy rejection"
		}
		message := fmt.Sprintf("Observed %s reported by checks on commit %s while inspecting %s (cluster %s; category %s).", counted(counts[g], "PipelineRun creation failure"), current.SHA, consoleTarget(current.Target), cluster, category)
		if reported := representative[g]; reported.Reason != "" {
			message += fmt.Sprintf(" GitHub check %d reports: %q.", reported.CheckID, reported.Reason)
		}
		message += " Current cluster availability is unknown."
		s.record(current.Target, current.SHA, "start-failures", "", message, "system", message)
	}
}
