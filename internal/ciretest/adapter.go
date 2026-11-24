// Package ciretest adapts CI Retest observations to Release Assistant's planner.
package ciretest

import (
	"context"
	"errors"

	retry "github.com/vikin91/ci-retest"

	"release-assistant/internal/release"
)

// Adapter preserves CI Retest's results for Release Assistant's retry queue.
type Adapter struct {
	client *retry.Client
}

// New keeps one client alive so Prow configuration is reused across polls.
func New(config retry.Config) *Adapter {
	return &Adapter{client: retry.New(config)}
}

// PlanRetries offers manual retries for the target's merge requirements and prerequisites.
func (a *Adapter) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	var result retry.Result
	var err error
	if target.PR > 0 && target.Tag == "" {
		result, err = a.client.CheckPR(ctx, target.Repo, target.PR, retry.Options{})
	} else if target.PR == 0 && target.Tag != "" {
		result, err = a.client.CheckTag(ctx, target.Repo, target.Tag, retry.Options{})
	} else {
		return release.RetryPlan{Target: target}, errors.New("invalid retry target")
	}
	plan := release.ManualRetryReport{
		Target: release.Target(result.Target), SHA: result.SHA, URL: result.URL,
		Jobs: result.Jobs, Prow: result.Prow, Warnings: result.Warnings, OtherChecks: result.OtherChecks,
	}.RetryPlan()
	if err != nil {
		plan.Evidence.Known, plan.Evidence.Error = false, err.Error()
	}
	return plan, err
}
