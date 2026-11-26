package integration

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/githubread"
	"release-assistant/internal/images"
	"release-assistant/internal/release"
	"release-assistant/internal/web"
)

type browserFixture struct {
	mu                  sync.Mutex
	step, writes        int
	failGitHub          bool
	noTags              bool
	vulnerabilityReview bool
	pinningFailure      string
	branches            map[string]string
}

func observation() release.Evidence {
	return release.Evidence{At: time.Now(), Known: true, URL: "https://example.com/fixture-evidence"}
}
func (f *browserFixture) ReadComponentTagNames(_ context.Context, source release.Source) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGitHub {
		return nil, fmt.Errorf("fixture tag names unavailable")
	}
	if f.noTags && f.step == 0 {
		return nil, nil
	}
	if source.Name == "fact" {
		if f.step >= 1 {
			return []string{"0.4.8", "0.4.7"}, nil
		}
		return []string{"0.4.7"}, nil
	}
	return []string{"3.26.5"}, nil
}

func (f *browserFixture) Discover(ctx context.Context, r app.DiscoveryRequest) (release.Repository, error) {
	select {
	case <-ctx.Done():
		return release.Repository{}, ctx.Err()
	case <-time.After(3 * time.Second):
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failGitHub {
		return release.Repository{}, fmt.Errorf("GitHub GET /repos/%s/branches/%s rejected (HTTP 403): fixture permission denied", r.Source.Repo, r.Source.Branch)
	}
	if f.branches == nil {
		f.branches = map[string]string{}
	}
	f.branches[r.Source.Repo] = r.Source.Branch
	repository := release.Repository{Source: r.Source, Head: r.Source.Name + "-tag", Evidence: observation()}
	if r.Source.Name == "acs" {
		if f.vulnerabilityReview && f.step > 0 {
			repository.Head = "acs-rc1"
		}
		if r.Reads(app.ReadRC) {
			repository.Candidate = release.Candidate{
				Target: release.Target{Repo: r.Source.Repo, Tag: strings.TrimPrefix(r.Version, "v") + "-rc.0"},
				SHA:    "acs-tag", Evidence: observation(),
			}
			if f.vulnerabilityReview && f.step > 0 {
				repository.Tags = []release.Tag{{Name: "4.10.12-rc.0", SHA: "acs-tag", Evidence: observation()}}
				repository.Candidate.Target.Tag = "4.10.12-rc.1"
				repository.Candidate.SHA, repository.Candidate.Number = "acs-rc1", 1
			}
		}
		pr := release.PR{Target: release.Target{Repo: r.Source.Repo, PR: 201}, Title: "Fixture: ACS dependency backport", CommentLabel: true, Author: "mintmaker", URL: "https://example.com/pr201", SHA: "acs-pr-head", State: "open", Base: r.Source.Branch, Evidence: observation(), ChecksEvidence: observation(), Checks: []release.Check{{Name: "CI build", Status: "completed", Conclusion: "failure", Required: release.Yes}}}
		pr.MergeState = "blocked"
		if f.step >= 4 {
			pr.State, pr.Merged, pr.MergeSHA = "merged", true, "acs-next"
			repository.Head = "acs-next"
			if repository.Candidate.Target.Valid() {
				repository.Candidate.Ahead = 1
				repository.Candidate.NextMilestone = release.Evidence{Known: true, At: time.Now(), URL: "https://example.com/milestone/1"}
				repository.Candidate.CompareURL = "https://github.com/stackrox/stackrox/compare/acs-tag...acs-next"
				repository.Candidate.PRs = []release.CandidatePR{{Number: pr.Target.PR, Title: pr.Title, URL: pr.URL}}
			}
		}
		repository.PRs = []release.PR{pr}
		// The fixture pin PR can be reviewed before a number or component tag is selected.
		pin := release.PR{Target: release.Target{Repo: r.Source.Repo, PR: 202}, Title: "Fixture: component versions", State: "open", MergeState: "clean", Base: r.Source.Branch, SHA: "pin-head", Evidence: observation()}
		if f.step < 3 && r.PinningPR != pin.Target.PR {
			repository.PRs = append(repository.PRs, pin)
		}
		if r.DetectPinningPR && r.Reads(app.ReadPRs) {
			repository.PinDiscovery = release.PinDiscovery{Files: make(map[int]release.PinFileMatch), Evidence: observation()}
			if f.step < 3 {
				repository.PinDiscovery.Files[202] = release.PinFileMatch{Head: pin.SHA, Base: repository.Head, Reason: "Changes FACT_VERSION and COLLECTOR_VERSION."}
			}
		}
		repository.PreviousPins = release.Pins{Versions: map[string]string{"fact": "0.4.6", "collector": "3.26.4"}, Evidence: observation()}
		versions := map[string]string{"fact": "0.4.6", "collector": "3.26.4"}
		if f.step >= 3 {
			versions = map[string]string{"fact": "0.4.8", "collector": "3.26.5"}
		}
		if r.PinningPR > 0 {
			repository.PinningEvidence = observation()
			repository.PinningPR = &release.PR{
				Target: release.Target{Repo: r.Source.Repo, PR: r.PinningPR},
				Base:   r.Source.Branch, State: "closed", Merged: true, SHA: "pin-head", MergeSHA: "pin-merge",
				URL: "https://github.com/" + r.Source.Repo + "/pull/" + strconv.Itoa(r.PinningPR), Evidence: observation(),
			}
			if (r.PinningPR == 202 || os.Getenv("ACS_BROWSER_PINNING_OPEN") == "1") && f.step < 3 {
				repository.PinningPR.State, repository.PinningPR.Merged = "open", false
				repository.PinningPR.ChecksEvidence = observation()
				repository.PinningPR.Checks = []release.Check{{Name: "CI build", Status: "completed", Conclusion: "success", URL: "https://example.com/ci-build", Required: release.Yes}, {Name: "Red Hat Konflux / main-on-push", Status: "completed", Conclusion: "failure", URL: "https://example.com/main-build", Required: release.Yes}}
				if f.step >= 2 {
					repository.PinningPR.Checks[1].Conclusion = "success"
				}
				repository.PRs = append(repository.PRs, *repository.PinningPR)
			}
			repository.PinningPins = release.Pins{Versions: map[string]string{"fact": "0.4.7", "collector": "3.26.5"}, Paths: map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}, Evidence: observation()}
			switch f.pinningFailure {
			case "auth":
				repository.PinningPR = nil
				repository.PRs = []release.PR{pr}
				repository.PinningEvidence.Known = false
				repository.PinningEvidence.Error = "Authentication failed; GitHub rejected the token (HTTP 401)."
				repository.PinningPins = release.Pins{Evidence: repository.PinningEvidence}
			case "file":
				delete(repository.PinningPins.Versions, "fact")
				repository.PinningPins.Failures = map[string]string{"fact": "Not found or inaccessible with the current GitHub token (HTTP 404)."}
				repository.PinningPins.Evidence.Known = false
				repository.PinningPins.Evidence.Error = "FACT_VERSION: " + repository.PinningPins.Failures["fact"]
			}
		}
		repository.Pins = release.Pins{Versions: versions, Paths: map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}, Evidence: observation()}
		return repository, nil
	}
	version := "3.26.5"
	if r.Source.Name == "fact" {
		version = "0.4.7"
	}
	repository.Tags = []release.Tag{{Name: version, SHA: repository.Head, Belongs: release.Yes, Evidence: observation()}}
	if r.Source.Name == "fact" {
		pr := release.PR{Target: release.Target{Repo: r.Source.Repo, PR: 101}, Title: "Fixture: release fix <script>alert(1)</script>", Author: "fixture-author", URL: "https://example.com/pr101", SHA: "pr-head", State: "open", Base: r.Source.Branch, Evidence: observation(), ChecksEvidence: observation(), Checks: []release.Check{{Name: "CI build", Status: "completed", Conclusion: "failure", Required: release.Yes}}}
		pr.MergeState = "blocked"
		if f.step >= 1 {
			pr.State = "merged"
			pr.Merged = true
			pr.MergeSHA = "fact-next"
			repository.Head = "fact-next"
			repository.Tags = append(repository.Tags, release.Tag{Name: "0.4.8", SHA: "fact-next", Belongs: release.Yes, Evidence: observation()})
		}
		repository.PRs = []release.PR{pr}
		if f.step >= 4 {
			repository.PRs = append(repository.PRs, release.PR{Target: release.Target{Repo: r.Source.Repo, PR: 102}, Title: "Fixture: later change", URL: "https://example.com/pr102", SHA: "later-head", State: "merged", Merged: true, MergeSHA: "later", Base: r.Source.Branch, Evidence: observation()})
		}
	}
	switch {
	case f.noTags && f.step == 0:
		repository.Tags = nil
	case !r.Reads(app.ReadTags):
		repository.Tags = nil
	case r.TagCandidates != nil:
		repository.Tags = slices.DeleteFunc(repository.Tags, func(tag release.Tag) bool { return !slices.Contains(r.TagCandidates, tag.Name) })
	case r.ConfirmedTag != "":
		repository.Tags = slices.DeleteFunc(repository.Tags, func(tag release.Tag) bool { return tag.Name != r.ConfirmedTag })
	}
	return repository, nil
}
func (f *browserFixture) PlanRetries(ctx context.Context, target release.Target) (release.RetryPlan, error) {
	plan, err := f.Plan(ctx, target)
	return plan.RetryPlan(), err
}

func (f *browserFixture) Plan(_ context.Context, t release.Target) (release.Plan, error) {
	f.mu.Lock()
	pinningFailure := f.pinningFailure
	step := f.step
	f.mu.Unlock()
	if pinningFailure == "auth" && t.Repo == "stackrox/stackrox" && t.PR > 0 && t.PR != 201 {
		return release.Plan{Target: t}, fmt.Errorf("GitHub authentication failed (HTTP 401)")
	}
	var sha string
	if t.Tag != "" {
		if t.Repo == "stackrox/stackrox" {
			sha = "acs-tag"
		} else if t.Tag == "0.4.8" {
			sha = "fact-next"
		} else if t.Repo == "stackrox/fact" {
			sha = "fact-tag"
		} else {
			sha = "collector-tag"
		}
	} else {
		sha = "pr-head"
		if t.Repo == "stackrox/stackrox" {
			sha = "pin-head"
			if t.PR == 201 {
				sha = "acs-pr-head"
			}
		}
	}
	p := release.Plan{Target: t, SHA: sha, Action: "Wait", Message: "Fixture observations; no external service calls", Evidence: observation()}
	if t.PR > 0 {
		job := "fact-on-push"
		if t.Repo == "stackrox/stackrox" {
			job = "main-on-push"
		}
		proposal := release.Proposal{Target: t, SHA: sha, Job: job, RunID: "fixture-run-1", Payload: "/konflux-retest " + job, At: time.Now()}
		p.Action = "Retest"
		p.Proposals = []release.Proposal{proposal}
		p.Runs = []release.Run{{Job: proposal.Job, ID: proposal.RunID, Status: "completed", Outcome: "failure", Eligible: true, URL: "https://example.com/fixture-run"}}
		if t.Repo == "stackrox/stackrox" {
			p.Runs[0].Image = release.Image{Reference: "quay.io/rhacs-eng/release-main@sha256:" + strings.Repeat("a", 64), Evidence: observation()}
			p.Runs = append(p.Runs, release.Run{Job: "roxctl-on-push", ID: "fixture-run-2", Status: "completed", Outcome: "failure", URL: "https://example.com/roxctl-build", Image: release.Image{Reference: "quay.io/rhacs-eng/release-roxctl@sha256:" + strings.Repeat("b", 64), Evidence: observation()}})
			if t.PR != 201 && step >= 2 {
				p.Action = "None"
				p.Message = "Pinning checks passed; checking reported output images."
				p.Proposals = nil
				for i := range p.Runs {
					p.Runs[i].Outcome, p.Runs[i].Eligible = "success", false
				}
			}
		}
		p.Advisories = []release.Advisory{{ID: "fixture-policy-1", Message: "Conforma fixture finding; human review required", URL: "https://example.com/finding"}}
	}
	if os.Getenv("ACS_BROWSER_START_FAILURES") == "1" && t.Repo == "stackrox/stackrox" && t.PR > 0 && t.PR != 201 && step < 2 {
		p.Action, p.Message, p.Proposals = "Wait", "Checks on this commit reported PipelineRun creation failures.", nil
		p.Runs = []release.Run{{Job: "main-on-push", Status: "completed", Outcome: "failure"}, {Job: "roxctl-on-push", Status: "completed", Outcome: "failure"}}
		for index, run := range p.Runs {
			checkID := int64(501 + index)
			evidence := observation()
			evidence.URL = fmt.Sprintf("https://example.com/check/%d", checkID)
			p.StartFailures = append(p.StartFailures, release.StartFailure{
				CheckID: checkID, Job: run.Job, Cluster: "example.openshiftapps.com", Category: "admission_policy",
				Reason: "Admission policy deny-pipelinerun-create rejected this attempt: PipelineRun creation is temporarily disabled.", Evidence: evidence,
			})
		}
	}
	return p, nil
}
func (f *browserFixture) ReadTarget(ctx context.Context, t release.Target) (dispatch.Identity, error) {
	p, err := f.Plan(ctx, t)
	if err != nil {
		return dispatch.Identity{Target: t}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	base := ""
	if t.PR > 0 {
		base = f.branches[t.Repo]
	}
	open := t.Tag != "" || f.step == 0
	if t.PR > 0 && t.Repo == "stackrox/stackrox" {
		if t.PR == 201 {
			open = f.step < 4
		} else {
			open = f.step < 3 && os.Getenv("ACS_BROWSER_PINNING_OPEN") == "1"
		}
	}
	return dispatch.Identity{Target: t, SHA: p.SHA, Base: base, Open: open, CommentLabel: t.Repo == "stackrox/stackrox" && t.PR == 201, Evidence: observation()}, nil
}
func (f *browserFixture) Write(context.Context, release.Proposal) (dispatch.Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	return dispatch.Delivery{Status: "sent", URL: "https://example.com/fake-comment/" + strconv.Itoa(f.writes)}, nil
}
func (f *browserFixture) Inspect(ctx context.Context, config release.Config, tags map[string]string) (release.Inventory, error) {
	f.mu.Lock()
	present := f.step >= 2
	f.mu.Unlock()
	client := &http.Client{Transport: browserRegistryTransport{componentsPresent: present}}
	return images.New(client, "", "").Inspect(ctx, config, tags)
}

func (f *browserFixture) InspectComponents(ctx context.Context, config release.Config, tags map[string]string) (release.Inventory, error) {
	f.mu.Lock()
	present := f.step >= 2
	f.mu.Unlock()
	client := &http.Client{Transport: browserRegistryTransport{componentsPresent: present}}
	return images.New(client, "", "").InspectComponents(ctx, config, tags)
}

func (f *browserFixture) InspectReference(ctx context.Context, reference string) (release.Image, error) {
	client := &http.Client{Transport: browserRegistryTransport{componentsPresent: !strings.Contains(reference, "release-roxctl")}}
	return images.New(client, "", "").InspectReference(ctx, reference)
}

type browserRegistryTransport struct{ componentsPresent bool }

func (t browserRegistryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Host != "quay.io" {
		return nil, fmt.Errorf("fixture received a non-registry request")
	}
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	status := http.StatusNotFound
	body := `{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`
	if t.componentsPresent && !strings.Contains(request.URL.Path, "-rc.0") {
		status = http.StatusOK
		body = `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":1},"layers":[]}`
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
}

func (f *browserFixture) ReadRCVulnerabilities(_ context.Context, rc release.Candidate, descendant string, _ release.RCVulnerabilities, _ release.Validation) release.RCVulnerabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := release.RCVulnerabilities{Target: rc.Target, SHA: rc.SHA, DescendantSHA: descendant}
	if !f.vulnerabilityReview {
		return result
	}
	main := "Available"
	if rc.Number == 1 && f.step == 1 {
		main = "Pending"
	}
	result.Images = []release.ImageVulnerabilities{
		{Image: "release-scanner-v4", Status: "Available", ConformaReported: true, Findings: []release.Vulnerability{
			{ID: "CVE-2026-119373", Package: "sqlite-libs", Severity: "Important", CVSS: new(7.8), Fixed: "0:3.26.0-28.el8_10"},
			{ID: "CVE-2026-10079", Package: "Scanner V4", Severity: "Important", CVSS: new(8.5)},
		}},
		{Image: "release-main", Status: main},
	}
	return result
}

// TestBrowserFixture is an opt-in fixture server for real-browser acceptance checks.
func TestBrowserFixture(t *testing.T) {
	if os.Getenv("ACS_BROWSER_FIXTURE") == "" {
		t.Skip("set ACS_BROWSER_FIXTURE to exercise the browser with fixture services")
	}
	f := &browserFixture{noTags: os.Getenv("ACS_BROWSER_NO_TAGS") == "1", vulnerabilityReview: os.Getenv("ACS_BROWSER_VULNERABILITIES") == "1"}
	dispatcher := dispatch.New(f, f, f, dispatch.Options{EnableWrites: os.Getenv("ACS_BROWSER_ENABLE_WRITES") == "1", Gap: time.Minute})
	options := app.Options{GitHubReads: githubread.New(nil), Poll: 100 * time.Millisecond, Discovery: 100 * time.Millisecond}
	if f.vulnerabilityReview {
		options.Initial = release.Config{Version: "4.10.12", Branch: "release-4.10", Components: release.DefaultsFor("4.10.12")}
		for i := range options.Initial.Components {
			component := &options.Initial.Components[i]
			component.Branch = "release-" + component.Stream
		}
	}
	if os.Getenv("ACS_BROWSER_PROFILES") == "1" {
		state, err := app.OpenReleaseState(filepath.Join(t.TempDir(), "state.json"), nil)
		if err != nil {
			t.Fatal(err)
		}
		options.Profiles, options.ReleaseState = true, state
	}
	session := app.New(f, f, f, dispatcher, app.NewLog(io.Discard, false), options)
	handler, err := web.New(session)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:18088")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/fixture", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			switch r.FormValue("action") {
			case "advance":
				f.mu.Lock()
				f.step++
				f.mu.Unlock()
			case "finish":
				once.Do(func() { close(done) })
			case "pin-auth", "pin-file", "pin-recover":
				f.mu.Lock()
				f.pinningFailure = strings.TrimPrefix(r.FormValue("action"), "pin-")
				f.mu.Unlock()
			case "github-fail", "github-recover":
				f.mu.Lock()
				f.failGitHub = r.FormValue("action") == "github-fail"
				f.mu.Unlock()
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, err := fmt.Fprintf(w, `<!doctype html><title>Fixture controls</title><h1>Fixture observations, step %d</h1><p>Fake comments sent: %d. No external services are called.</p><p>Steps: 0 open PR; 1 merged with newer tag; 2 images present; 3 pinning merged and actual pins updated; 4 ACS PR merged and later component PR.</p><a href="/">Open assistant</a><form method="post"><button name="action" value="advance">Advance fixture observations</button><button name="action" value="github-fail">Simulate GitHub failure</button><button name="action" value="github-recover">Restore GitHub</button><button name="action" value="pin-auth">Simulate pin PR authentication failure</button><button name="action" value="pin-file">Simulate missing pin version file</button><button name="action" value="pin-recover">Restore pin PR</button><button name="action" value="finish">Finish browser validation</button></form>`, f.step, f.writes); err != nil {
			t.Errorf("write browser fixture controls: %v", err)
		}
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			t.Logf("browser form metadata: host=%q origin=%q fetchSite=%q", r.Host, r.Header.Get("Origin"), r.Header.Get("Sec-Fetch-Site"))
		}
		handler.ServeHTTP(w, r)
	}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan error, 1)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); session.Run(ctx) }()
	go func() { serverDone <- server.Serve(listener) }()
	t.Log("Fixture browser URL: http://127.0.0.1:18088")
	select {
	case <-done:
	case err := <-serverDone:
		t.Error(err)
	case <-time.After(15 * time.Minute):
		t.Error("browser validation timed out")
	}
	cancel()
	_ = server.Close()
	<-monitorDone
}
