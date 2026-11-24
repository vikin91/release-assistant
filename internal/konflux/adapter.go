// Package konflux isolates the public konflux-retest API from application contracts.
package konflux

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/release"

	retry "github.com/vikin91/konflux-retest"
)

type (
	// Config supplies startup credentials without storing them in application snapshots.
	Config struct {
		GitHubToken, QuayUsername, QuayPassword string
		HTTPClient                              *http.Client
		KonfluxToken                            func(context.Context, string) (string, error)
		// AllowKonfluxLookup authorizes the GitHub App, API hostname, and namespace before token lookup.
		AllowKonfluxLookup func(context.Context, int64, string, string) bool
		// OnReadPauseChange receives endpoint transitions. A zero RetryAt means reads resumed.
		OnReadPauseChange func(ReadIssue)
		// CommentFooter attributes posted retries to the running build.
		CommentFooter string

		apiHost, apiPort string
		lookups          Lookups
	}
	observationError struct {
		message string
		cause   error
	}
	// Adapter reads Konflux health and delegates retry planning to konflux-retest.
	Adapter struct {
		config        retry.Config
		reads         *readBackoff
		health        healthProbe
		commentFooter string
		pr            func(context.Context, retry.Config, string, int) (retry.Result, error)
		tag           func(context.Context, retry.Config, string, string) (retry.Result, error)
	}
)

func (e *observationError) Error() string { return e.message }

func (e *observationError) Unwrap() error { return e.cause }

// IsUnsupportedRetagContract reports whether every planner error is an unsupported retag contract.
func IsUnsupportedRetagContract(err error) bool {
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		children := e.Unwrap()
		for _, child := range children {
			if !IsUnsupportedRetagContract(child) {
				return false
			}
		}
		return len(children) > 0
	case interface{ Unwrap() error }:
		return IsUnsupportedRetagContract(e.Unwrap())
	default:
		return errors.Is(err, retry.ErrUnsupportedRetagContract)
	}
}

// New constructs the retry boundary.
func New(c Config) *Adapter {
	client := http.Client{Timeout: 30 * time.Second}
	if c.HTTPClient != nil {
		client = *c.HTTPClient
	}
	health := newHealthProbe(c, client)
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	reads := &readBackoff{
		base: base, now: time.Now, endpoints: make(map[string]*endpointBackoff), onPauseChange: c.OnReadPauseChange,
		apiHost: c.apiHost, apiPort: c.apiPort,
	}
	client.Transport = reads
	return &Adapter{
		config: retry.Config{
			VerifyRunIdentity: true, VerifyMissingPRTriggers: true,
			GitHubToken: c.GitHubToken, QuayUsername: c.QuayUsername, QuayPassword: c.QuayPassword,
			HTTPClient: &client, KonfluxToken: c.KonfluxToken, AllowKonfluxLookup: c.AllowKonfluxLookup,
		},
		reads:         reads,
		health:        health,
		commentFooter: c.CommentFooter,
		pr:            retry.CheckPR, tag: retry.CheckTag,
	}
}

// ReadIssues copies current endpoint diagnostics for the dashboard.
func (a *Adapter) ReadIssues() []ReadIssue {
	return a.reads.issues()
}

// PlanRetries exposes retry offers and build observations from a single read.
func (a *Adapter) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	plan, err := a.Plan(ctx, target)
	return plan.RetryPlan(), err
}

// Plan preserves partial results. Posting requires stable evidence for each proposed job.
func (a *Adapter) Plan(ctx context.Context, t release.Target) (release.Plan, error) {
	if ctx.Value(readCycleKey{}) == nil {
		ctx = WithReadCycle(ctx)
	}
	var reads planReads
	ctx = context.WithValue(ctx, planReadsKey{}, &reads)
	var r retry.Result
	var err error
	if t.PR > 0 {
		r, err = a.pr(ctx, a.config, t.Repo, t.PR)
	} else {
		r, err = a.tag(ctx, a.config, t.Repo, t.Tag)
	}
	p := release.Plan{Target: t, SHA: r.SHA, Action: string(r.Action), Message: r.Message, Warnings: append([]string(nil), r.Warnings...), Evidence: release.Evidence{At: time.Now(), URL: r.URL, Known: err == nil}}
	for i := range p.Warnings {
		p.Warnings[i] = observationProblem(p.Warnings[i], a.config)
	}
	if err != nil {
		p.Evidence.Error = observationProblem(err.Error(), a.config)
		p.KonfluxAPIUnavailable = reads.unavailable.Load() || reads.deferred.Load()
		if reads.deferred.Load() {
			p.Evidence.Error += "\nSome Konflux reads were deferred by endpoint backoff."
		}
		err = &observationError{message: p.Evidence.Error, cause: err}
	}
	for _, run := range r.Runs {
		id := ""
		if run.IdentityKnown && run.ID > 0 {
			id = strconv.FormatInt(run.ID, 10)
		}
		p.Runs = append(p.Runs, release.Run{Job: run.Job, ID: id, CheckID: run.ID, Status: run.Status, Outcome: run.Conclusion, URL: run.URL, Eligible: run.Eligible, Image: imageOutput(run, p.Evidence.At)})
	}
	for _, failure := range r.StartFailures {
		evidence := release.Evidence{At: p.Evidence.At}
		if failure.CheckID > 0 && r.SHA != "" && release.ValidRepo(t.Repo) {
			evidence.Known = true
			evidence.URL = "https://github.com/" + t.Repo + "/runs/" + strconv.FormatInt(failure.CheckID, 10)
		}
		superseded := false
		for _, run := range r.Runs {
			if failure.CheckID > 0 && run.IdentityKnown && run.Job == failure.Job && run.ID > failure.CheckID {
				superseded = true
				break
			}
		}
		p.StartFailures = append(p.StartFailures, release.StartFailure{
			CheckID: failure.CheckID, Job: failure.Job, Cluster: failure.Cluster,
			Category: string(failure.Category), Reason: observationProblem(failure.Reason, a.config),
			ExternalID: observationProblem(failure.ExternalID, a.config), CheckStartedAt: failure.CheckStartedAt,
			Superseded: superseded, Evidence: evidence,
		})
	}
	for _, advisory := range r.Advisories {
		p.Advisories = append(p.Advisories, release.Advisory{
			ID: t.String() + ":" + strconv.FormatInt(advisory.ID, 10), Name: advisory.Name,
			Message: advisory.Name + ": " + advisory.Conclusion, URL: advisory.URL,
		})
	}
	addProposal := func(job, evidence string) {
		payload := "/konflux-retest " + job
		if t.Tag != "" {
			payload = "/retest " + job + " tag:" + t.Tag
		}
		p.Proposals = append(p.Proposals, release.Proposal{
			RunID: evidence, Target: t, SHA: r.SHA, Job: job, Payload: payload,
			CommentFooter: a.commentFooter, At: p.Evidence.At,
		})
	}
	for _, job := range r.Jobs {
		addProposal(job, proposalEvidence(t, job, r))
	}
	if t.PR > 0 && t.Valid() && r.SHA != "" {
		for _, job := range r.MissingPRJobs {
			if !slices.Contains(r.Jobs, job) {
				addProposal(job, release.PRMissingBuildEvidence)
			}
		}
	}
	return p, err
}

func proposalEvidence(target release.Target, job string, result retry.Result) string {
	var checkID int64
	for _, run := range result.Runs {
		if run.Job != job || !run.Eligible || run.ID <= 0 {
			continue
		}
		if run.IdentityKnown {
			return strconv.FormatInt(run.ID, 10)
		}
		if target.PR > 0 && target.Valid() && slices.ContainsFunc(result.StartFailures, func(failure retry.StartFailure) bool {
			return failure.Job == job && failure.CheckID == run.ID && failure.PRNumber == target.PR
		}) {
			checkID = run.ID
		}
	}
	if checkID > 0 {
		return release.PRCheckEvidencePrefix + strconv.FormatInt(checkID, 10)
	}
	return ""
}

var (
	outputImagePattern  = regexp.MustCompile(`^(quay\.io/[a-z0-9_-]+/[a-z0-9._/-]+)(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@(sha256:[a-f0-9]{64}))?$`)
	outputDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// imageOutput preserves scoped build evidence without treating it as registry availability.
func imageOutput(run retry.Run, at time.Time) release.Image {
	if !run.IdentityKnown || run.ID <= 0 || (run.ImageURL == "" && run.ImageDigest == "") {
		return release.Image{}
	}
	image := release.Image{Component: "acs", Family: "konflux", Reference: run.ImageURL, Evidence: release.Evidence{At: at, URL: run.URL}}
	parts := outputImagePattern.FindStringSubmatch(run.ImageURL)
	switch {
	case run.ImageURL == "":
		image.Evidence.Error = "The build has not reported a valid image reference."
	case len(parts) != 3 || strings.Contains(parts[1], ".."):
		image.Evidence.Error = "The build reported an unsupported image reference."
	case parts[2] != "" && run.ImageDigest != "" && parts[2] != run.ImageDigest:
		image.Evidence.Error = "The build reported conflicting image digests."
	default:
		digest := run.ImageDigest
		if digest == "" {
			digest = parts[2]
		}
		if outputDigestPattern.MatchString(digest) {
			image.Reference = parts[1] + "@" + digest
			image.Evidence.Known = true
		} else {
			image.Evidence.Error = "The build has not reported a valid image digest."
		}
	}
	return image
}

// observationProblem exposes dependency diagnostics without startup credentials.
func observationProblem(message string, config retry.Config) string {
	for _, credential := range []string{config.GitHubToken, config.QuayPassword} {
		if credential != "" {
			message = strings.ReplaceAll(message, credential, "[redacted]")
		}
	}
	return message
}
