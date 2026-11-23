package app

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"

	"release-assistant/internal/release"
)

func TestCheckPRRetriesUsesBothReadersForIncludedHead(t *testing.T) {
	for name, scenario := range map[string]struct {
		choice release.Choice
		ciSHA  string
	}{
		"should refresh an included PR":              {release.Included, "head"},
		"should reject an undecided PR":              {"", "head"},
		"should reject an excluded PR":               {release.Excluded, "head"},
		"should hide commands when the head changes": {release.Included, "new-head"},
	} {
		t.Run(name, func(t *testing.T) {
			session, _ := newFixture()
			source := configuration().ACS()
			target := release.Target{Repo: source.Repo, PR: 42}
			pr := release.PR{Target: target, SHA: "head", Base: source.Branch, State: "open", Evidence: observed()}
			session.repos[source.Name] = release.Repository{Source: source, PRs: []release.PR{pr}}
			session.choices.PRs[target] = scenario.choice

			var konfluxCalls, ciCalls int
			session.planner = buildPollPlanner(func(_ context.Context, got release.Target) (release.Plan, error) {
				konfluxCalls++
				return release.Plan{Target: got, SHA: pr.SHA, Evidence: observed(),
					Proposals: []release.Proposal{{Target: got, SHA: pr.SHA, Payload: "/konflux-retest checks"}}}, nil
			})
			session.options.ManualRetries = manualReaderFunc(func(_ context.Context, got release.Target) (release.ManualRetryReport, error) {
				ciCalls++
				return release.ManualRetryReport{Target: got, SHA: scenario.ciSHA, Jobs: []release.ManualJob{{ID: 123}}}, nil
			})
			session.dispatcher.Interactive(false)

			action := Action{Kind: "check-pr-retries", Generation: session.generation, Target: target, SHA: pr.SHA}
			err := session.Apply(t.Context(), action)
			result := session.Snapshot().PRRetryChecks[target]
			if scenario.choice != release.Included {
				if err == nil || konfluxCalls != 0 || ciCalls != 0 || result.SHA != "" {
					t.Fatalf("unauthorized retry check read or published evidence: %v, %+v", err, result)
				}
				return
			}
			if err != nil || konfluxCalls != 1 || ciCalls != 1 {
				t.Fatalf("expected both retry results, got %+v, %v", result, err)
			}
			if scenario.ciSHA == pr.SHA {
				if len(result.Konflux.Proposals) != 1 || len(result.CI.Jobs) != 1 {
					t.Fatalf("expected both retry commands, got %+v", result)
				}
			} else if len(result.Konflux.Proposals) != 0 || len(result.CI.Jobs) != 0 ||
				result.Konflux.Evidence.Error == "" || !result.CI.Incomplete {
				t.Fatalf("expected the changed head to suppress both sets of commands, got %+v", result)
			}
			if len(session.dispatcher.Views()) != 0 {
				t.Fatal("read-only check updated dispatch state")
			}
		})
	}
}

func TestNewlyIncludedPRRefreshesBeforeTheNextInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, fixture := newFixture()
		config := configuration()
		source := config.ACS()
		repo := fixture.repos[source.Name]
		for _, number := range []int{41, 42} {
			repo.PRs = append(repo.PRs, release.PR{
				Target: release.Target{Repo: source.Repo, PR: number}, SHA: "head",
				State: "open", Base: source.Branch, Evidence: observed(),
			})
		}
		fixture.repos[source.Name] = repo
		var konflux, ci []release.Target
		session.planner = buildPollPlanner(func(_ context.Context, target release.Target) (release.Plan, error) {
			if target.Repo == source.Repo && target.PR > 0 {
				konflux = append(konflux, target)
			}
			return release.Plan{Target: target, SHA: "head", Evidence: observed()}, nil
		})
		session.options.ManualRetries = manualReaderFunc(func(_ context.Context, target release.Target) (release.ManualRetryReport, error) {
			if target.Repo == source.Repo && target.PR > 0 {
				ci = append(ci, target)
			}
			return release.ManualRetryReport{Target: target, SHA: "head"}, nil
		})
		apply(t, session, Action{Kind: "configure", Config: config})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go session.Run(ctx)
		synctest.Wait()

		old, added := repo.PRs[0].Target, repo.PRs[1].Target
		apply(t, session, Action{Kind: "choice", Target: old, SHA: "head", Choice: release.Included})
		synctest.Wait()
		konflux, ci = nil, nil
		apply(t, session, Action{Kind: "choice", Target: added, SHA: "head", Choice: release.Included})
		synctest.Wait()
		want := []release.Target{added, old}
		if !slices.Equal(konflux, want) || !slices.Equal(ci, want) {
			t.Fatalf("both immediate polls should read the added PR first: Konflux %v, CI %v", konflux, ci)
		}
	})
}
