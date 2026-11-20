package app

import (
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

const maxRecentRecords = 1_000

type (
	// Record contains application-owned audit fields only, never raw HTTP errors.
	Record struct {
		At       time.Time      `json:"at"`
		Release  string         `json:"release"`
		Target   release.Target `json:"target"`
		SHA      string         `json:"sha,omitempty"`
		Decision string         `json:"decision"`
		Payload  string         `json:"payload,omitempty"`
		Result   string         `json:"result"`
		Origin   string         `json:"origin"`
		// message supplies human-readable prose independently of audit fields.
		message string
	}
	// Log writes readable decisions and exposes write failures to the UI.
	Log struct {
		mu      sync.Mutex
		output  io.Writer
		debug   bool
		failure string
		recent  []Record
	}
)

// NewLog writes audit records to w and retains recent activity for the UI.
// Both destinations include Debug records only when debug is true.
func NewLog(w io.Writer, debug bool) *Log { return &Log{output: w, debug: debug} }

// CallingAPI records an outbound HTTP attempt in the terminal and browser activity logs.
func (l *Log) CallingAPI(system, method, resource string) {
	l.Append(Record{
		At: time.Now(), Decision: "api-call", Origin: "system",
		message: fmt.Sprintf("Calling API: %s %s %s", system, method, resource),
	})
}

// CommitWindowCacheHits records reused history once per discovery pass.
func (l *Log) CommitWindowCacheHits(repo string, hits int) {
	l.Append(Record{
		At: time.Now(), Target: release.Target{Repo: repo}, Decision: "commit-window-cache-hit", Origin: "system",
		Result: fmt.Sprintf("GitHub discovery for %s reused %s from the in-memory cache.", repo, counted(hits, "commit history window")),
	})
}

// KonfluxReadPauseChanged records endpoint transitions shared by all release targets.
func (l *Log) KonfluxReadPauseChanged(issue konflux.ReadIssue) {
	endpoint := issue.Endpoint + " on " + issue.Cluster
	r := Record{At: time.Now(), Decision: "konflux-reads-resumed", Origin: "system"}
	r.Result = fmt.Sprintf("Konflux %s reads resumed.", endpoint)
	if !issue.RetryAt.IsZero() {
		r.Decision = "konflux-reads-paused"
		r.Result = fmt.Sprintf("Konflux %s reads paused (%s). Next recovery probe eligible at %s.",
			endpoint, issue.Reason, issue.RetryAt.Format(time.RFC3339))
	}
	l.Append(r)
}

// GitHubCommentAttempt records the exact comment and destination before the HTTP request can be sent.
func (l *Log) GitHubCommentAttempt(p release.Proposal) error {
	destination := "PR " + p.Target.String()
	if p.Target.Tag != "" {
		destination = fmt.Sprintf("tag %s (commit %s)", p.Target, p.SHA)
	}
	return l.append(Record{
		At: time.Now(), Target: p.Target, SHA: p.SHA, Decision: "github-comment-attempt", Origin: "system",
		Result: fmt.Sprintf("Posting GitHub comment %q to %s.", p.Payload, destination),
	})
}

// Append records a decision; failures remain visible until a successful write.
func (l *Log) Append(r Record) { _ = l.append(r) }

func (l *Log) append(r Record) error {
	if r.Decision == "work-complete" || (r.Decision == "observation" && r.message == "") {
		return nil
	}
	level := recordLevel(r)
	if level == "Debug" && !l.debug {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.recent) == maxRecentRecords {
		l.recent = slices.Delete(l.recent, 0, 1)
	}
	l.recent = append(l.recent, r)
	var text strings.Builder
	stamp := r.At.Format(time.RFC3339)
	fmt.Fprintf(&text, "%s %s: %s\n", stamp, level, strings.ReplaceAll(recordMessage(r), "\n", "\n    "))
	if l.debug && r.Payload != "" && r.Decision != "image" {
		fmt.Fprintf(&text, "%s Debug: Retry comment for %s:\n    %s\n", stamp, consoleRevision(r.Target, r.SHA), strings.ReplaceAll(r.Payload, "\n", "\n    "))
	}
	if l.output != nil {
		n, err := io.WriteString(l.output, text.String())
		if err == nil && n == text.Len() {
			l.failure = ""
			return nil
		}
	}
	l.failure = "Action log write failed"
	return errors.New(l.failure)
}

// Message describes a decision in the same prose used by the console log.
func (r Record) Message() string { return recordMessage(r) }

// Level reports the severity shared by console and browser activity.
func (r Record) Level() string { return recordLevel(r) }

// counted keeps counts grammatical in CLI sentences.
func counted(count int, noun string) string {
	if count != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", count, noun)
}

// retryDelay expresses polling backoff in seconds or minutes for the CLI.
func retryDelay(delay time.Duration) string {
	count, unit := delay.Seconds(), "second"
	if delay >= time.Minute {
		count, unit = delay.Minutes(), "minute"
	}
	if count != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%g %s", count, unit)
}

// consoleTarget keeps repository and ref names readable without omitting the ref.
func consoleTarget(target release.Target) string {
	if target.Repo == "" {
		return "the release"
	}
	repo := path.Base(target.Repo)
	if target.PR > 0 {
		return fmt.Sprintf("%s#%d", repo, target.PR)
	}
	if target.Tag != "" {
		return repo + "/" + target.Tag
	}
	return repo
}

// consoleBranch identifies the branch used by a repository read.
func consoleBranch(source release.Source) string {
	return consoleTarget(release.Target{Repo: source.Repo}) + "/" + source.Branch
}

// consoleRevision distinguishes PR head commits from commits resolved through tags.
func consoleRevision(target release.Target, sha string) string {
	name := consoleTarget(target)
	if sha == "" {
		return name
	}
	if target.PR > 0 {
		return fmt.Sprintf("%s (pull request head commit %s)", name, sha)
	}
	if target.Tag != "" {
		return fmt.Sprintf("%s (tag commit %s)", name, sha)
	}
	return name
}

// discoverySubject names only work included in the requested scope.
func discoverySubject(request DiscoveryRequest) string {
	var subjects []string
	if request.Reads(ReadPRs) {
		subjects = append(subjects, "pull requests")
	} else if request.Reads(ReadTargets) && len(request.Tracked) > 0 {
		subjects = append(subjects, "selected pull requests")
	}
	if request.Reads(ReadTags) {
		if request.Scope != ReadAll && request.ConfirmedTag != "" {
			subjects = append(subjects, "release tag "+request.ConfirmedTag)
		} else {
			subjects = append(subjects, "release tags")
		}
	}
	if request.Source.Name == "acs" && request.Reads(ReadPins) {
		subjects = append(subjects, "component version pins")
	}
	if request.Reads(ReadChecks) {
		subjects = append(subjects, "pull request checks")
	}
	if request.Reads(ReadCoverage) {
		subjects = append(subjects, "tag coverage of merged pull requests")
	}
	if len(subjects) == 0 {
		return "the branch head"
	}
	return strings.Join(subjects, ", ")
}

// discoverySummary reports scoped counts and identifies the observed branch head.
func discoverySummary(request DiscoveryRequest, repo release.Repository, incomplete bool) string {
	var counts []string
	if request.Reads(ReadPRs) || (request.Reads(ReadTargets) && len(request.Tracked) > 0) {
		counts = append(counts, counted(len(repo.PRs), "pull request"))
	}
	if request.Reads(ReadTags) {
		counts = append(counts, counted(len(repo.Tags), "release tag"))
	}
	branch := consoleBranch(request.Source)
	message := fmt.Sprintf("Checked %s on %s.", discoverySubject(request), branch)
	if len(counts) > 0 {
		message = fmt.Sprintf("Found %s while checking %s.", strings.Join(counts, " and "), branch)
	}
	if incomplete {
		if len(counts) == 0 {
			message = fmt.Sprintf("Could not finish checking %s on %s.", discoverySubject(request), branch)
		} else {
			message += " These results are incomplete."
		}
	}
	if repo.Head != "" {
		message += fmt.Sprintf(" Branch head of %s is %s.", branch, repo.Head)
	}
	return message
}

func settingChangeMessage[T int | bool](name string, before, after T) string {
	return fmt.Sprintf("Setting changed: %q from '%v' to '%v'.", name, before, after)
}

func recordSettingChange[T int | bool](s *Session, name string, before T, after *T) {
	if after != nil && before != *after {
		s.record(release.Target{}, "", "setting-changed", "", settingChangeMessage(name, before, *after), "user")
	}
}

// recordMessage translates audit decisions into sentences for the CLI.
func recordMessage(r Record) string {
	if r.message != "" {
		return r.message
	}
	target := consoleRevision(r.Target, r.SHA)
	for operation, subject := range map[string]string{
		"tags": "release tags", "github": "pull requests and release tags",
		"checks": "build results", "images": "image availability", "pinning-images": "build images",
	} {
		if r.Decision == operation+"-error" {
			return fmt.Sprintf("Could not read %s in %s. %s", subject, target, r.Result)
		}
		if r.Decision == operation+"-recovered" {
			return fmt.Sprintf("Reading %s in %s is working again.", subject, target)
		}
	}
	switch r.Decision {
	case "configure":
		return fmt.Sprintf("Preparing release %s.", r.Release)
	case "inclusion":
		if r.Result == "" {
			return fmt.Sprintf("Cleared the selection for %s.", target)
		}
		return fmt.Sprintf("Marked %s as %s.", target, r.Result)
	case "tag-confirmed":
		return fmt.Sprintf("Selected release tag %s.", target)
	case "tag-invalidated":
		if r.SHA != "" {
			return fmt.Sprintf("Release tag %s now points to commit %s. Select it again before continuing.", consoleTarget(r.Target), r.SHA)
		}
		return fmt.Sprintf("The commit for %s changed. Select the tag again before continuing.", target)
	case "tag-cleared":
		return fmt.Sprintf("Preparing a new tag in %s. Pull request monitoring continues.", consoleTarget(r.Target))
	case "conforma-acknowledged":
		if r.Result == "true" {
			return fmt.Sprintf("Marked the Conforma advisory in %s as reviewed.", target)
		}
		return fmt.Sprintf("Removed the Conforma review acknowledgement in %s.", target)
	case "RW":
		if r.Result == "true" {
			return fmt.Sprintf("Enabled permission to post retry comments in %s.", target)
		}
		return fmt.Sprintf("Disabled permission to post retry comments in %s.", target)
	case "ignore":
		if r.Result == "true" {
			return fmt.Sprintf("Ignoring retry suggestions in %s.", target)
		}
		return fmt.Sprintf("Considering retry suggestions in %s.", target)
	case "interactive":
		on := r.Result == "true"
		return settingChangeMessage("Confirm each retry comment", !on, on)
	case "revoked":
		return fmt.Sprintf("Revoked permission to post retry comments in %s because the target changed or no longer allows writes.", target)
	case "reject":
		if r.Result == "true" {
			return fmt.Sprintf("Rejected the proposed retry in %s.", target)
		}
		return fmt.Sprintf("Cleared the rejection of the proposed retry in %s.", target)
	case "approve":
		return fmt.Sprintf("Approved one retry comment in %s.", target)
	case "withheld":
		return fmt.Sprintf("Did not post a retry comment in %s because its approval is out of date.", target)
	case "dispatch":
		return fmt.Sprintf("Authorized a retry comment in %s.", target)
	case "delivery":
		switch r.Result {
		case "sent":
			return fmt.Sprintf("Posted a retry comment in %s.", target)
		case "failed":
			return fmt.Sprintf("Could not post a retry comment in %s.", target)
		default:
			return fmt.Sprintf("Could not confirm whether a retry comment was posted in %s. Check GitHub before retrying.", target)
		}
	default:
		return r.Result
	}
}

// recordLevel assigns console severity without changing the UI's audit records.
func recordLevel(r Record) string {
	switch {
	case r.Decision == "image":
		switch release.Truth(r.Result) {
		case release.Yes:
			return "Info"
		case release.No:
			return "Warn"
		default:
			return "Debug"
		}
	case strings.HasSuffix(r.Decision, "-error"), r.Decision == "delivery" && r.Result == "failed":
		return "Error"
	case r.Decision == "github-checks-attention", r.Decision == "konflux-reads-paused",
		r.Decision == "start-failures", r.Decision == "retry-wait", r.Decision == "tag-invalidated", r.Decision == "revoked", r.Decision == "withheld", r.Decision == "delivery" && r.Result == "uncertain":
		return "Warn"
	case r.Decision == "api-call", r.Decision == "observation", r.Decision == "checks", r.Decision == "commit-window-cache-hit":
		return "Debug"
	default:
		return "Info"
	}
}

// Recent returns copied session activity, including records whose output write failed.
func (l *Log) Recent() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.recent)
}

// Error reports audit availability without exposing filesystem or credential details.
func (l *Log) Error() string { l.mu.Lock(); defer l.mu.Unlock(); return l.failure }
