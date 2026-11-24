// Command release-assistant serves one local ACS preparation session.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"release-assistant/internal/apilog"
	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/github"
	"release-assistant/internal/githubread"
	"release-assistant/internal/images"
	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
	"release-assistant/internal/web"
)

const defaultKonfluxLookup = "296509,api.stone-prd-rh01.pg1f.p1.openshiftapps.com,rh-acs-tenant"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	var config release.Config
	var options app.Options
	var limits dispatch.Options
	var port int
	statePath := flag.String("state-file", "./state.json", "local JSON file for PR, tag, and pinning choices by ACS release")
	debugLog := flag.Bool("v", false, "show Debug messages in the terminal and dashboard Log tab")
	allowedLookups := make(konflux.Lookups)
	flag.StringVar(&config.Version, "version", "", "ACS patch version, e.g. 5.0.1")
	flag.StringVar(&config.Branch, "branch", "", "explicit ACS target branch")
	flag.IntVar(&config.PinningPR, "pinning-pr", 0, "optional ACS pinning PR number")
	var components [3]release.Source
	for n, name := range []string{"fact", "collector", "scanner"} {
		components[n].Name = name
		flag.StringVar(&components[n].Repo, name+"-repo", "stackrox/"+name, "component GitHub repository")
		flag.StringVar(&components[n].Branch, name+"-branch", "", "explicit component release branch")
		flag.StringVar(&components[n].Stream, name+"-stream", "", "component major.minor stream")
	}
	flag.IntVar(&port, "port", 8080, "loopback HTTP port")
	flag.DurationVar(&options.Poll, "poll", 0, "override initial refresh intervals for checks, builds, and images")
	flag.DurationVar(&options.Discovery, "discover", 0, "override initial refresh intervals for PR and tag discovery")
	flag.DurationVar(&options.BackoffCap, "backoff-cap", 5*time.Minute, "maximum per-source access-error backoff")
	flag.DurationVar(&options.Timeout, "timeout", 2*time.Minute, "maximum work duration per source cycle")
	flag.DurationVar(&limits.Gap, "comment-gap", time.Minute, "minimum gap for the same comment on the same target and commit")
	flag.DurationVar(&limits.SlowGap, "slow-gap", time.Hour, "minimum gap for repeating the same comment at the job failure threshold")
	flag.IntVar(&limits.Threshold, "failure-threshold", 10, "distinct consecutive failed runs before slowdown")
	flag.BoolVar(&limits.EnableWrites, "enable-writes", false, "allow retry comments after per-target RW permission and required confirmation")
	flag.Func("allow-konflux-lookup",
		"Konflux lookup for APP_ID,API_HOST,NAMESPACE (repeatable, replaces default "+defaultKonfluxLookup+")",
		allowedLookups.Add)
	flag.Parse()
	if flag.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	var invalidInterval bool
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "pinning-pr" {
			options.PinningPROverride = true
		}
		if (f.Name == "poll" && options.Poll <= 0) || (f.Name == "discover" && options.Discovery <= 0) {
			invalidInterval = true
		}
	})
	if port < 1 || port > 65535 || invalidInterval || options.BackoffCap <= 0 || options.Timeout <= 0 || limits.Gap <= 0 || limits.SlowGap <= 0 || limits.Threshold < 1 {
		return fmt.Errorf("port and timing/threshold settings must be positive and valid")
	}
	options.RefreshIntervals = app.DefaultRefreshIntervals(options.Poll, options.Discovery)
	prefilled := applyVersionDefaults(config.Version, components[:])
	for _, source := range prefilled {
		if source.Name != "scanner" || source.Branch != "" || source.Stream != "" {
			config.Components = append(config.Components, source)
		}
	}
	options.Initial = config
	if err := applyDefaultKonfluxLookup(allowedLookups); err != nil {
		return err
	}
	log := app.NewLog(os.Stdout, *debugLog)
	state, err := app.OpenReleaseState(*statePath, log)
	if err != nil {
		return err
	}
	if err := state.SetInitialRelease(config, options.PinningPROverride); err != nil {
		return err
	}
	options.Profiles = true
	options.ReleaseState = state
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	plannerConfig := konflux.StartupConfig(ctx, allowedLookups)
	options.GitHubReads = githubread.New(apilog.Transport{Base: http.DefaultTransport, CallingWithContext: log.CallingAPIWithContext})
	client := &http.Client{Transport: options.GitHubReads, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	windowCache := github.NewCommitWindowCache(nil).WithReleaseHitLog(log.CommitWindowCacheHitsWithContext)
	gh := github.New("", plannerConfig.GitHubToken, client).WithCommitWindowCache(windowCache)
	options.Cache = windowCache
	options.ManualRetries = gh
	plannerConfig.HTTPClient = client
	plannerConfig.OnReadPauseChange = log.KonfluxReadPauseChanged
	plannerConfig.CommentFooter = "*posted from release-assistant " + buildVersion() + "*"
	planner := konflux.New(plannerConfig)
	options.KonfluxProbe = planner.ProbeHealth
	inspector := images.New(client, plannerConfig.QuayUsername, plannerConfig.QuayPassword)
	dispatcher := dispatch.New(gh, planner, github.NewWriter(gh, log), limits)
	session := app.New(gh, planner, inspector, dispatcher, log, options)

	handler, err := web.New(session)
	if err != nil {
		return err
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	server := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: time.Minute}
	monitoringDone := make(chan struct{})
	go func() { defer close(monitoringDone); session.Run(ctx) }()
	servingDone := make(chan error, 1)
	go func() { servingDone <- server.Serve(listener) }()
	fmt.Printf("ACS release assistant: http://127.0.0.1:%d\n", port)
	select {
	case err = <-servingDone:
		stop()
	case <-ctx.Done():
		stop()
	}
	dispatcher.Stop()
	closing, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(closing)
	<-monitoringDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// applyDefaultKonfluxLookup supplies the ACS namespace when the flags supply no lookup policy.
func applyDefaultKonfluxLookup(lookups konflux.Lookups) error {
	if len(lookups) > 0 {
		return nil
	}
	if err := lookups.Add(defaultKonfluxLookup); err != nil {
		return fmt.Errorf("invalid default Konflux lookup: %w", err)
	}
	return nil
}

// applyVersionDefaults fills component streams and branches from the CLI version.
// Explicit component flags win, and the values only seed the initial setup form.
func applyVersionDefaults(version string, components []release.Source) []release.Source {
	known := make(map[string]release.Source)
	for _, source := range release.DefaultsFor(version) {
		known[source.Name] = source
	}
	for i := range components {
		if fallback, ok := known[components[i].Name]; ok {
			if components[i].Repo == "" {
				components[i].Repo = fallback.Repo
			}
			if components[i].Stream == "" {
				components[i].Stream = fallback.Stream
			}
		}
		if components[i].Branch == "" && components[i].Stream != "" {
			components[i].Branch = "release-" + components[i].Stream
		}
	}
	return components
}
