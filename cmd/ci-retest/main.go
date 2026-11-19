// Command ci-retest prints Konflux retry comments followed by supported Actions and Prow retries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"release-assistant/internal/github"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, newReader)
	stop()
	os.Exit(code)
}

type readers struct {
	konfluxPlan   func(context.Context, release.Target) (release.Plan, error)
	manualRetries func(context.Context, release.Target) (release.ManualRetryReport, error)
}

func newReader(ctx context.Context, lookups konflux.Lookups) (readers, error) {
	config := konflux.StartupConfig(ctx, lookups)
	if config.GitHubToken == "" {
		return readers{}, errors.New("set GH_TOKEN or GITHUB_TOKEN, or sign in with gh auth login --hostname github.com")
	}
	config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	return readers{
		konfluxPlan:   konflux.New(config).Plan,
		manualRetries: github.New("", config.GitHubToken, config.HTTPClient).ManualRetries,
	}, nil
}

func targets(args []string, diagnostics io.Writer, lookups konflux.Lookups) ([]release.Target, error) {
	fs := flag.NewFlagSet("ci-retest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("allow-konflux-lookup", "allow a Konflux lookup for APP_ID,API_HOST,NAMESPACE (repeatable)", lookups.Add)
	repo := fs.String("repo", "stackrox", "repository")
	prs := fs.String("prs", "", "PR numbers")
	tags := fs.String("tags", "", "tag names")
	fs.Usage = func() {
		fmt.Fprintln(diagnostics, `Usage: ci-retest [--repo NAME|OWNER/REPO] --prs 24136 24135
       ci-retest [--repo NAME|OWNER/REPO] --tags 4.11.8-rc.0,5.0.0-rc.0

Choose one list and put it last. Spaces and commas are accepted. Short
repository names use the stackrox owner. Default: stackrox/stackrox.
Prints eligible Konflux retry comments first, then supported Actions reruns and
Prow comments. For verified Konflux reads, use the active oc login and pass
--allow-konflux-lookup APP_ID,API_HOST,NAMESPACE before the target list.
GitHub authentication: GH_TOKEN,
GITHUB_TOKEN, or a saved gh login. Read access to Actions, checks,
commit statuses, contents and pull requests is needed.`)
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	var hasPRs, hasTags bool
	fs.Visit(func(f *flag.Flag) {
		hasPRs = hasPRs || f.Name == "prs"
		hasTags = hasTags || f.Name == "tags"
	})
	if hasPRs == hasTags {
		return nil, errors.New("choose exactly one of --prs or --tags")
	}
	if !strings.Contains(*repo, "/") {
		*repo = "stackrox/" + *repo
	}
	if !release.ValidRepo(*repo) {
		return nil, errors.New("invalid repository")
	}
	list := *tags
	if hasPRs {
		list = *prs
	}
	values := strings.FieldsFunc(strings.Join(append([]string{list}, fs.Args()...), " "), func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	var result []release.Target
	seen := make(map[release.Target]bool)
	for _, value := range values {
		t := release.Target{Repo: *repo}
		if hasPRs {
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("invalid PR number %q", value)
			}
			t.PR = n
		} else {
			t.Tag = value
		}
		if hasTags && !github.ValidRetryTag(t.Tag) {
			return nil, fmt.Errorf("invalid target %q", value)
		}
		if !seen[t] {
			result = append(result, t)
			seen[t] = true
		}
	}
	if len(result) == 0 {
		return nil, errors.New("empty target list")
	}
	return result, nil
}

func run(ctx context.Context, args []string, out, diagnostics io.Writer, reader func(context.Context, konflux.Lookups) (readers, error)) int {
	lookups := make(konflux.Lookups)
	targets, err := targets(args, diagnostics, lookups)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 2
	}
	c, err := reader(ctx, lookups)
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	code := 0
	for i, t := range targets {
		if i > 0 {
			fmt.Fprintln(out)
		}
		plan, planErr := c.konfluxPlan(ctx, t)
		report, err := c.manualRetries(ctx, t)
		if planErr != nil {
			report.Warnings = append(report.Warnings, "Konflux: "+planErr.Error())
			code = 1
		}
		if err != nil {
			report.Warnings = append(report.Warnings, err.Error())
			code = 1
		}
		if len(plan.Proposals) > 0 && (plan.SHA == "" || plan.Target != t || report.SHA != plan.SHA) {
			report.Warnings = append(report.Warnings, "The Konflux and CI checks could not confirm the same target revision. Run ci-retest again.")
			plan.Proposals = nil
			code = 1
		}
		fmt.Fprintf(out, "---\n%s\n", report.URL)
		if t.Tag != "" {
			fmt.Fprintf(out, "Tag: %s\n", t.Tag)
		}
		switch {
		case len(plan.Proposals)+len(report.Jobs)+len(report.Prow) > 0:
			fmt.Fprintln(out, "\nAvailable retries")
		case len(report.OtherChecks)+len(report.Warnings) > 0:
			fmt.Fprintln(out, "\nNext action: Investigate")
		default:
			fmt.Fprintln(out, "\nNext action: None")
		}
		for _, proposal := range plan.Proposals {
			fmt.Fprintf(out, "Post this Konflux comment on %s (commit %s)\n%s\n\n", plan.Evidence.URL, proposal.SHA, proposal.Payload)
		}
		for j, job := range report.Jobs {
			if j > 0 {
				fmt.Fprintln(out)
			}
			if job.WorkflowName != "" {
				fmt.Fprintf(out, "%s / ", job.WorkflowName)
			}
			fmt.Fprintf(out, "%s\n%s\n", job.Name, job.URL)
			fmt.Fprintln(out, job.RerunCommand(t.Repo))
		}
		if len(report.Prow) > 0 {
			if len(report.Jobs) > 0 {
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "Failed Prow checks: post this comment on %s\n%s\n", report.URL, report.ProwComment())
		}
		if len(report.OtherChecks) > 0 {
			fmt.Fprintln(out, "\nUnsupported checks")
			for _, check := range report.OtherChecks {
				fmt.Fprintln(out, check)
			}
		}
		if len(report.Jobs)+len(report.Prow) > 0 {
			report.Warnings = append(report.Warnings, release.ManualRetryWarning)
		}
		if len(report.Warnings) > 0 {
			fmt.Fprintln(out, "\nWarnings:")
			for _, w := range report.Warnings {
				fmt.Fprintln(out, w)
			}
		}
		if ctx.Err() != nil {
			return 1
		}
	}
	return code
}
