// Command ci-retest prints GitHub Actions rerun links and commands, and Prow retry comments.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"time"
	"unicode"

	"release-assistant/internal/github"
	"release-assistant/internal/release"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, newReader)
	stop()
	os.Exit(code)
}

func newReader(ctx context.Context) (*github.Client, error) {
	token := strings.TrimSpace(os.Getenv("GH_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if token == "" {
		data, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
		if err != nil || strings.TrimSpace(string(data)) == "" {
			return nil, errors.New("set GH_TOKEN or GITHUB_TOKEN, or sign in with gh auth login --hostname github.com")
		}
		token = strings.TrimSpace(string(data))
	}
	return github.New("", token, &http.Client{Timeout: 30 * time.Second}), nil
}

func targets(args []string, diagnostics io.Writer) ([]release.Target, error) {
	fs := flag.NewFlagSet("ci-retest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "stackrox", "repository")
	prs := fs.String("prs", "", "PR numbers")
	tags := fs.String("tags", "", "tag names")
	fs.Usage = func() {
		fmt.Fprintln(diagnostics, `Usage: ci-retest [--repo NAME|OWNER/REPO] --prs 24136 24135
       ci-retest [--repo NAME|OWNER/REPO] --tags 4.11.8-rc.0,5.0.0-rc.0

Choose one list and put it last. Spaces and commas are accepted. Short
repository names use the stackrox owner. Default: stackrox/stackrox.
Prints job rerun links, gh api commands, and a PR-wide /retest comment for failed
OpenShift CI presubmits. Only reads GitHub. Authentication: GH_TOKEN,
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

func run(ctx context.Context, args []string, out, diagnostics io.Writer, reader func(context.Context) (*github.Client, error)) int {
	targets, err := targets(args, diagnostics)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 2
	}
	c, err := reader(ctx)
	if err != nil {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	code := 0
	for i, t := range targets {
		if i > 0 {
			fmt.Fprintln(out)
		}
		report, err := c.ManualRetries(ctx, t)
		if err != nil {
			report.Warnings = append(report.Warnings, err.Error())
			code = 1
		}
		fmt.Fprintf(out, "---\n%s\n", report.URL)
		if t.Tag != "" {
			fmt.Fprintf(out, "Tag: %s\n", t.Tag)
		}
		switch {
		case len(report.Jobs)+len(report.Prow) > 0:
			fmt.Fprintln(out, "\nAvailable retries")
		case len(report.Warnings) > 0:
			fmt.Fprintln(out, "\nNext action: Investigate")
		default:
			fmt.Fprintln(out, "\nNext action: None")
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
			fmt.Fprintf(out, "Failed Prow checks: %s\nPost once on %s (PR-wide; other CI may also respond):\n%s\n", strings.Join(report.Prow, ", "), report.URL, release.ProwRetryComment)
		}
		fmt.Fprintln(out, "\nWarnings:")
		for _, w := range report.Warnings {
			fmt.Fprintln(out, w)
		}
		fmt.Fprintln(out, release.ManualRetryWarning)
		if ctx.Err() != nil {
			return 1
		}
	}
	return code
}
