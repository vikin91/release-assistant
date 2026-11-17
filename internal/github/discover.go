package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/app"
	"release-assistant/internal/dispatch"
	"release-assistant/internal/release"
)

type (
	branchResponse struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	tagResponse struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	gitTagResponse struct {
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	compareResponse struct {
		Status string `json:"status"`
	}
	commitResponse struct {
		Commit struct {
			Author struct {
				Date time.Time `json:"date"`
			} `json:"author"`
			Committer struct {
				Date time.Time `json:"date"`
			} `json:"committer"`
		} `json:"commit"`
	}
	prResponse struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Merged         bool       `json:"merged"`
		MergedAt       *time.Time `json:"merged_at"`
		MergeCommitSHA string     `json:"merge_commit_sha"`
		Labels         []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	branchProtectionResponse struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
			AppID   int64  `json:"app_id"`
		} `json:"checks"`
	}
)

// Discover observes one repository within a fixed HTTP request budget.
// It returns partial observations alongside errors when reads are incomplete.
func (c *Client) Discover(ctx context.Context, req app.DiscoveryRequest) (release.Repository, error) {
	return c.discover(ctx, req, maxDiscoveryRequests)
}

func (c *Client) discover(ctx context.Context, req app.DiscoveryRequest, limit int) (release.Repository, error) {
	cycle := &discoveryCycle{reads: map[string]cachedRead{}, limit: limit, remaining: limit}
	ctx = context.WithValue(ctx, discoveryCycleKey{}, cycle)
	result := release.Repository{Source: req.Source}
	var issues []error
	base := repoPath(req.Source.Repo)
	var branch branchResponse
	if err := c.get(ctx, base+"/branches/"+segment(req.Source.Branch), &branch); err != nil {
		issues = append(issues, err)
		result.Evidence = release.Evidence{At: time.Now(), Error: requestProblem(err)}
	} else {
		result.Head = branch.Commit.SHA
		result.Evidence = release.Evidence{At: time.Now(), URL: "https://github.com/" + req.Source.Repo + "/tree/" + req.Source.Branch, Known: true}
	}
	if result.Head == "" && result.Evidence.Error == "" {
		result.Evidence = release.Evidence{At: time.Now(), Error: "GitHub returned no commit SHA for the configured branch."}
		issues = append(issues, fmt.Errorf("GitHub returned no commit SHA for the configured branch"))
	}
	if req.Source.Name == "acs" && req.Reads(app.ReadPins) {
		cycle.reserved = min(pinReadReserve, cycle.remaining)
	}

	source := req.Source
	if source.Name == "acs" && source.Stream == "" {
		major, minor, _, ok := release.Version(req.Version)
		if ok {
			source.Stream = fmt.Sprintf("%d.%d", major, minor)
		}
	}
	result.Source = source
	if req.Reads(app.ReadTags) {
		publish := func(tags []resolvedTag) {
			if req.Progress == nil {
				return
			}
			partial := result
			partial.Tags = nil
			for _, tag := range tags {
				partial.Tags = append(partial.Tags, tag.observation)
			}
			partial.Evidence.Known = false
			partial.TagsPending = true
			req.Progress(partial)
		}
		var tags []resolvedTag
		var tagErr error
		if req.Scope != app.ReadAll && req.ConfirmedTag != "" {
			var tag resolvedTag
			tag, tagErr = c.resolveTag(ctx, source, req.ConfirmedTag, result.Head)
			if tag.observation.Name != "" {
				tags = append(tags, tag)
			}
		} else if req.Scope == app.ReadTags && result.Head != "" {
			tags, result.TagsPending, tagErr = c.tagBatch(ctx, source, result.Head, publish)
		} else {
			tags, tagErr = c.listTags(ctx, source, result.Head, publish)
		}
		if tagErr != nil {
			issues = append(issues, tagErr)
		}
		for _, tag := range tags {
			result.Tags = append(result.Tags, tag.observation)
		}
	}
	best := -1
	for i := range result.Tags {
		if result.Tags[i].Belongs == release.Yes && (best < 0 || compareTagNames(result.Tags[i].Name, result.Tags[best].Name) > 0) {
			best = i
		}
	}
	ignored := map[int]bool{}
	for _, target := range req.Ignored {
		if target.Repo == req.Source.Repo && target.PR > 0 {
			ignored[target.PR] = true
		}
	}

	var prs []prResponse
	if req.Reads(app.ReadPRs) {
		var prErr error
		if req.Scope == app.ReadAll {
			prs, prErr = c.listRelevantPRs(ctx, source, result.Tags, best, req.ConfirmedTag, ignored)
		} else {
			prErr = c.pages(ctx, base+"/pulls?state=open&base="+url.QueryEscape(source.Branch), func(raw json.RawMessage) error {
				var p prResponse
				if err := json.Unmarshal(raw, &p); err != nil {
					return fmt.Errorf("malformed pull request record")
				}
				if p.Base.Ref == source.Branch {
					prs = append(prs, p)
				}
				return nil
			})
		}
		if prErr != nil {
			issues = append(issues, prErr)
		}
	}
	tracked := map[int]bool{}
	for _, target := range req.Tracked {
		if req.Reads(app.ReadTargets) && target.Repo == req.Source.Repo && target.PR > 0 {
			tracked[target.PR] = true
		}
	}
	for _, summary := range prs {
		p := convertPR(req.Source.Repo, summary)
		if ignored[p.Target.PR] {
			result.PRs = append(result.PRs, p)
			continue
		}
		if p.State == "open" && req.Reads(app.ReadChecks) && (req.Scope == app.ReadAll || tracked[p.Target.PR] || p.Target.PR == req.PinningPR) {
			c.readChecks(ctx, source, &p)
		}
		result.PRs = append(result.PRs, p)
	}
	for number := range tracked {
		if ignored[number] || hasPR(result.PRs, number) {
			continue
		}
		var raw prResponse
		if err := c.get(ctx, base+"/pulls/"+strconv.Itoa(number), &raw); err != nil {
			issues = append(issues, err)
			continue
		}
		p := convertPR(req.Source.Repo, raw)
		if p.State == "open" && req.Reads(app.ReadChecks) {
			c.readChecks(ctx, source, &p)
		}
		result.PRs = append(result.PRs, p)
	}
	var merged []release.PR
	for _, p := range result.PRs {
		if p.Merged && p.MergeSHA != "" && !ignored[p.Target.PR] {
			merged = append(merged, p)
		}
	}
	if req.Reads(app.ReadCoverage) && len(merged) > 0 && len(result.Tags) > 0 {
		roots := slices.Clone(result.Tags)
		slices.SortStableFunc(roots, func(a, b release.Tag) int { return compareTagNames(b.Name, a.Name) })
		var shas []string
		for _, tag := range roots {
			shas = append(shas, tag.SHA)
		}
		graph := newCommitGraph()
		if err := graph.load(ctx, c, source.Repo, shas); err != nil {
			issues = append(issues, err)
		}
		for ti := range result.Tags {
			tag := &result.Tags[ti]
			reachable, complete := graph.ancestors(tag.SHA)
			for _, p := range merged {
				tag.Contains[p.MergeSHA] = graphContains(reachable, complete, p.MergeSHA)
				if tag.Contains[p.MergeSHA] == "" {
					tag.Evidence.Known = false
					tag.Evidence.Error = "merge coverage unavailable"
				}
			}
		}
	}

	if source.Name == "acs" && req.Reads(app.ReadPins) {
		cycle.reserved = 0
		if result.Head == "" {
			result.Pins.Evidence = result.Evidence
		} else {
			pins, err := c.readPins(ctx, req.Source.Repo, result.Head, req.Version)
			result.Pins = pins
			if err != nil {
				issues = append(issues, err)
			}
		}
		if req.PinningPR > 0 && !ignored[req.PinningPR] {
			result.PinningEvidence = release.Evidence{At: time.Now(), URL: "https://github.com/" + req.Source.Repo + "/pull/" + strconv.Itoa(req.PinningPR)}
			var raw prResponse
			if err := c.get(ctx, base+"/pulls/"+strconv.Itoa(req.PinningPR), &raw); err != nil {
				issues = append(issues, err)
				result.PinningEvidence.Error = requestProblem(err)
				result.PinningPins.Evidence = result.PinningEvidence
			} else {
				result.PinningEvidence.Known = true
				p := convertPR(req.Source.Repo, raw)
				eligible := p.Base == req.Source.Branch && p.State == "open" && !p.Merged && p.SHA != ""
				if eligible && req.Reads(app.ReadChecks) {
					if existing := findPR(result.PRs, p.Target.PR); existing != nil && existing.SHA == p.SHA && !existing.ChecksEvidence.At.IsZero() {
						p.Checks = existing.Checks
						p.ChecksEvidence = existing.ChecksEvidence
					} else {
						c.readChecks(ctx, source, &p)
					}
				}
				if findPR(result.PRs, p.Target.PR) != nil || eligible {
					upsertPR(&result.PRs, p)
				}
				result.PinningPR = &p
				if p.Base == req.Source.Branch {
					ref := p.SHA
					if p.Merged {
						ref = p.MergeSHA
						if ref == "" {
							result.PinningPins = release.Pins{Evidence: release.Evidence{
								At: time.Now(), URL: p.URL, Error: "GitHub returned no merge commit SHA for the merged pinning PR.",
							}}
							issues = append(issues, fmt.Errorf("merged pinning PR has no merge commit SHA"))
						}
					}
					if !p.Merged && ref == "" {
						result.PinningPins.Evidence = release.Evidence{At: time.Now(), URL: p.URL, Error: "GitHub returned no PR head commit SHA."}
						issues = append(issues, fmt.Errorf("GitHub returned no PR head commit SHA"))
					}
					if ref != "" {
						pins, err := c.readPins(ctx, req.Source.Repo, ref, req.Version)
						result.PinningPins = pins
						if err != nil {
							issues = append(issues, err)
						}
					}
				} else {
					result.PinningPins.Evidence = release.Evidence{At: time.Now(), URL: p.URL, Error: fmt.Sprintf("PR targets branch %s; expected %s.", p.Base, req.Source.Branch)}
					issues = append(issues, fmt.Errorf("pinning PR targets a different base branch"))
				}
			}
		}
	}
	if source.Name == "acs" && req.Reads(app.ReadPreviousPins) {
		if cycle.remaining >= 12 && cycle.budgetErr == nil && cycle.rateErr == nil {
			baseline := &discoveryCycle{reads: map[string]cachedRead{}, limit: 12, remaining: 12}
			baselineCtx := context.WithValue(ctx, discoveryCycleKey{}, baseline)
			result.PreviousPins, result.PreviousPublished = c.previousPins(baselineCtx, source.Repo, req.Version)
		} else {
			result.PreviousPins.Evidence = release.Evidence{At: time.Now(), Error: "Previous ACS release versions are deferred until GitHub read allowance is available."}
		}
	}
	if result.TagsPending {
		result.Evidence.Known = false
	}
	return finishDiscovery(result, issues, cycle)
}

func finishDiscovery(result release.Repository, issues []error, cycle *discoveryCycle) (release.Repository, error) {
	if cycle.budgetErr != nil {
		issues = append(issues, cycle.budgetErr)
	}
	if cycle.rateErr != nil {
		issues = append(issues, cycle.rateErr)
	}
	if len(issues) > 0 {
		result.Evidence.At = time.Now()
		if result.Evidence.Error == "" {
			result.Evidence.Error = "one or more GitHub observations are incomplete"
		}
		return result, fmt.Errorf("GitHub discovery incomplete (%d observation(s)): %w", len(issues), errors.Join(issues...))
	}
	return result, nil
}

type resolvedTag struct {
	observation release.Tag
	commitDate  time.Time
}

func (c *Client) listTags(ctx context.Context, src release.Source, headSHA string, progress ...func([]resolvedTag)) ([]resolvedTag, error) {
	var out []resolvedTag
	var firstErr error
	err := c.pages(ctx, repoPath(src.Repo)+"/tags", func(raw json.RawMessage) error {
		var t tagResponse
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("malformed tag record")
		}
		major, minor, _, ok := release.Version(t.Name)
		sm, sn, _, streamOK := release.Version(src.Stream + ".0")
		if !ok || !streamOK || major != sm || minor != sn {
			return nil
		}
		tag, err := c.resolveTag(ctx, src, t.Name, headSHA)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if tag.observation.Name != "" {
			out = append(out, tag)
			for _, publish := range progress {
				publish(out)
			}
		}
		return nil
	})
	return out, errors.Join(err, firstErr)
}

func (c *Client) resolveTag(ctx context.Context, src release.Source, name, headSHA string) (resolvedTag, error) {
	var ref gitTagResponse
	if err := c.get(ctx, repoPath(src.Repo)+"/git/ref/tags/"+segment(name), &ref); err != nil {
		return resolvedTag{}, err
	}
	sha, kind := ref.Object.SHA, ref.Object.Type
	for depth := 0; kind == "tag" && depth < 5; depth++ {
		var obj gitTagResponse
		if err := c.get(ctx, repoPath(src.Repo)+"/git/tags/"+segment(sha), &obj); err != nil {
			return resolvedTag{}, err
		}
		sha, kind = obj.Object.SHA, obj.Object.Type
	}
	if sha == "" || kind == "tag" {
		return resolvedTag{}, fmt.Errorf("tag %s could not be resolved", name)
	}
	if kind != "commit" {
		return resolvedTag{}, fmt.Errorf("tag %s does not resolve to a commit", name)
	}
	tag := resolvedTag{observation: release.Tag{Name: name, SHA: sha, Contains: map[string]release.Truth{}, Evidence: release.Evidence{At: time.Now(), URL: "https://github.com/" + src.Repo + "/tree/" + name, Known: true}}}
	var metadata commitResponse
	if err := c.get(ctx, repoPath(src.Repo)+"/commits/"+segment(sha), &metadata); err != nil {
		tag.observation.Evidence.Known = false
		tag.observation.Evidence.Error = "tag commit metadata unavailable"
		return tag, err
	}
	tag.commitDate = metadata.Commit.Committer.Date
	if headSHA == "" {
		tag.observation.Evidence.Known = false
		tag.observation.Evidence.Error = "branch head unavailable for ancestry"
		return tag, nil
	}
	var cmp compareResponse
	if err := c.get(ctx, repoPath(src.Repo)+"/compare/"+segment(sha)+"..."+segment(headSHA), &cmp); err != nil {
		tag.observation.Evidence.Known = false
		tag.observation.Evidence.Error = "tag ancestry unavailable"
		return tag, err
	}
	switch cmp.Status {
	case "ahead", "identical":
		tag.observation.Belongs = release.Yes
	case "behind", "diverged":
		tag.observation.Belongs = release.No
	default:
		tag.observation.Evidence.Known = false
		tag.observation.Evidence.Error = "tag ancestry status unknown"
	}
	return tag, nil
}

func (c *Client) listRelevantPRs(ctx context.Context, src release.Source, tags []release.Tag, best int, confirmed string, ignored map[int]bool) ([]prResponse, error) {
	seen := map[int]bool{}
	var out []prResponse
	add := func(p prResponse) error {
		if p.Base.Ref == src.Branch && !seen[p.Number] {
			seen[p.Number] = true
			out = append(out, p)
		}
		return nil
	}
	err := c.pages(ctx, repoPath(src.Repo)+"/pulls?state=open&base="+url.QueryEscape(src.Branch), func(raw json.RawMessage) error {
		var p prResponse
		if e := json.Unmarshal(raw, &p); e != nil {
			return fmt.Errorf("malformed pull request record")
		}
		return add(p)
	})
	if err != nil {
		return out, err
	}
	baseline := best
	confirmedFound := confirmed == ""
	for i := range tags {
		if confirmed != "" && tags[i].Name == confirmed {
			confirmedFound = true
			if tags[i].Belongs != release.Yes {
				return out, fmt.Errorf("confirmed tag ancestry is not established")
			}
			baseline = i
			break
		}
	}
	if !confirmedFound {
		return out, fmt.Errorf("confirmed tag is missing from the current tag inventory")
	}
	if baseline < 0 {
		return out, fmt.Errorf("no eligible baseline tag; merged PR inventory is unknown")
	}
	var base commitResponse
	if err := c.get(ctx, repoPath(src.Repo)+"/commits/"+segment(tags[baseline].SHA), &base); err != nil {
		return out, err
	}
	query := "repo:" + src.Repo + " is:pr is:merged base:" + src.Branch + " merged:>=" + base.Commit.Committer.Date.UTC().Format("2006-01-02")
	endpoint := "/search/issues?q=" + url.QueryEscape(query) + "&sort=created&order=asc"
	err = c.searchMerged(ctx, endpoint, src, ignored, add)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (c *Client) searchMerged(ctx context.Context, endpoint string, src release.Source, ignored map[int]bool, add func(prResponse) error) error {
	for page := 1; page <= 10; page++ { // GitHub search exposes at most 1,000 results.
		var result struct {
			Total      int  `json:"total_count"`
			Incomplete bool `json:"incomplete_results"`
			Items      []struct {
				Number int `json:"number"`
			} `json:"items"`
		}
		if err := c.get(ctx, fmt.Sprintf("%s&per_page=100&page=%d", endpoint, page), &result); err != nil {
			return err
		}
		if result.Incomplete {
			return fmt.Errorf("merged pull request search results are incomplete")
		}
		for _, issue := range result.Items {
			if ignored[issue.Number] {
				continue
			}
			var p prResponse
			if err := c.get(ctx, repoPath(src.Repo)+"/pulls/"+strconv.Itoa(issue.Number), &p); err != nil {
				return err
			}
			if err := add(p); err != nil {
				return err
			}
		}
		if page*100 >= result.Total {
			return nil
		}
	}
	return fmt.Errorf("merged pull request search exceeded GitHub's 1,000 result limit")
}

func (c *Client) readChecks(ctx context.Context, src release.Source, p *release.PR) {
	p.ChecksEvidence = release.Evidence{At: time.Now(), URL: p.URL, Known: true}
	base := repoPath(src.Repo)
	var problems []string
	for page := 1; page <= maxPages; page++ {
		var response struct {
			Total int `json:"total_count"`
			Runs  []struct {
				Name       string `json:"name"`
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
				URL        string `json:"html_url"`
			} `json:"check_runs"`
		}
		endpoint := fmt.Sprintf("%s/commits/%s/check-runs?per_page=100&page=%d", base, segment(p.SHA), page)
		if err := c.get(ctx, endpoint, &response); err != nil {
			problems = append(problems, "Check runs: "+requestProblem(err))
			break
		}
		for _, run := range response.Runs {
			p.Checks = append(p.Checks, release.Check{Name: run.Name, Status: run.Status, Conclusion: run.Conclusion, URL: run.URL})
		}
		if page*100 >= response.Total {
			break
		}
		if page == maxPages {
			problems = append(problems, "Check runs: GitHub pagination limit reached.")
		}
	}
	err := c.pages(ctx, base+"/commits/"+segment(p.SHA)+"/statuses", func(raw json.RawMessage) error {
		var status struct {
			Context string `json:"context"`
			State   string `json:"state"`
			URL     string `json:"target_url"`
		}
		if err := json.Unmarshal(raw, &status); err != nil {
			return fmt.Errorf("malformed commit status")
		}
		if !slices.ContainsFunc(p.Checks, func(check release.Check) bool { return check.Name == status.Context }) {
			p.Checks = append(p.Checks, release.Check{Name: status.Context, Status: status.State, URL: status.URL})
		}
		return nil
	})
	if err != nil {
		problems = append(problems, "Commit statuses: "+requestProblem(err))
	}
	var required branchProtectionResponse
	if err := c.get(ctx, base+"/branches/"+segment(src.Branch)+"/protection/required_status_checks", &required); err != nil {
		problems = append(problems, "Required-check settings: "+requestProblem(err))
	} else {
		requiredNames := required.Contexts
		for _, check := range required.Checks {
			requiredNames = append(requiredNames, check.Context)
		}
		for i := range p.Checks {
			if slices.Contains(requiredNames, p.Checks[i].Name) {
				p.Checks[i].Required = release.Yes
			} else {
				p.Checks[i].Required = release.No
			}
		}
		for _, name := range requiredNames {
			if !slices.ContainsFunc(p.Checks, func(check release.Check) bool { return check.Name == name }) {
				p.Checks = append(p.Checks, release.Check{Name: name, Status: "pending", Required: release.Yes})
			}
		}
	}
	if len(problems) > 0 {
		p.ChecksEvidence.Error = strings.Join(problems, " ")
	}
}

func convertPR(repo string, p prResponse) release.PR {
	state := p.State
	if p.Merged || p.MergedAt != nil {
		state = "merged"
	}
	r := release.PR{
		Target: release.Target{Repo: repo, PR: p.Number}, SHA: p.Head.SHA,
		Title: p.Title, Author: p.User.Login, URL: p.HTMLURL, State: state, Base: p.Base.Ref,
		MergeSHA: p.MergeCommitSHA, Merged: p.Merged || p.MergedAt != nil,
		Evidence: release.Evidence{At: time.Now(), URL: p.HTMLURL, Known: p.Head.SHA != ""},
	}
	for _, l := range p.Labels {
		if strings.EqualFold(l.Name, "no-release-assistant") {
			r.OptOut = true
		}
	}
	return r
}

func hasPR(prs []release.PR, n int) bool {
	for _, p := range prs {
		if p.Target.PR == n {
			return true
		}
	}
	return false
}

func findPR(prs []release.PR, n int) *release.PR {
	for i := range prs {
		if prs[i].Target.PR == n {
			return &prs[i]
		}
	}
	return nil
}

func upsertPR(prs *[]release.PR, p release.PR) {
	if existing := findPR(*prs, p.Target.PR); existing != nil {
		*existing = p
		return
	}
	*prs = append(*prs, p)
}

func compareTagNames(a, b string) int {
	am, an, ap, aok := release.Version(a)
	bm, bn, bp, bok := release.Version(b)
	if !aok || !bok {
		return strings.Compare(a, b)
	}
	if am != bm {
		return cmpInt(am, bm)
	}
	if an != bn {
		return cmpInt(an, bn)
	}
	return cmpInt(ap, bp)
}
func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (c *Client) readPins(ctx context.Context, repo, sha, version string) (release.Pins, error) {
	p := release.Pins{Versions: map[string]string{}, Paths: map[string]string{}, Evidence: release.Evidence{At: time.Now(), Known: true, URL: "https://github.com/" + repo + "/tree/" + sha}}
	paths := map[string]string{"fact": "FACT_VERSION", "collector": "COLLECTOR_VERSION"}
	if major, _, _, ok := release.Version(version); ok && major == 4 {
		paths["scanner"] = "SCANNER_VERSION"
	}
	var issues []error
	for _, name := range []string{"fact", "collector", "scanner"} {
		path, required := paths[name]
		if !required {
			continue
		}
		p.Paths[name] = path
		data, err := c.File(ctx, repo, sha, path)
		if err != nil {
			if p.Failures == nil {
				p.Failures = map[string]string{}
			}
			p.Failures[name] = requestProblem(err)
			issues = append(issues, fmt.Errorf("read %s version pin: %w", name, err))
			continue
		}
		p.Versions[name] = strings.TrimSpace(string(data))
		if p.Versions[name] == "" {
			if p.Failures == nil {
				p.Failures = map[string]string{}
			}
			p.Failures[name] = "Version file is empty."
			issues = append(issues, fmt.Errorf("%s is empty", path))
		}
	}
	if len(issues) > 0 {
		p.Evidence.Known = false
		var details []string
		for _, name := range []string{"fact", "collector", "scanner"} {
			if problem := p.Failures[name]; problem != "" {
				details = append(details, paths[name]+": "+problem)
			}
		}
		p.Evidence.Error = strings.Join(details, "\n")
		return p, fmt.Errorf("required version pin unavailable: %w", errors.Join(issues...))
	}
	return p, nil
}

// File reads a repository file at an immutable commit SHA.
func (c *Client) File(ctx context.Context, repo, sha, filePath string) ([]byte, error) {
	if !release.ValidRepo(repo) || sha == "" || filePath == "" || strings.HasPrefix(filePath, "/") || pathpkg.Clean(filePath) != filePath || filePath == ".." || strings.HasPrefix(filePath, "../") {
		return nil, fmt.Errorf("invalid GitHub file identity")
	}
	var result struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	parts := strings.Split(filePath, "/")
	for i := range parts {
		parts[i] = segment(parts[i])
	}
	endpoint := repoPath(repo) + "/contents/" + strings.Join(parts, "/") + "?ref=" + url.QueryEscape(sha)
	if err := c.get(ctx, endpoint, &result); err != nil {
		return nil, err
	}
	if result.Encoding != "base64" {
		return nil, fmt.Errorf("GitHub file encoding unsupported")
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(result.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("GitHub file content malformed")
	}
	return decoded, nil
}

func (c *Client) previousPins(ctx context.Context, repo, version string) (release.Pins, release.Truth) {
	major, minor, patch, ok := release.Version(version)
	if !ok || patch == 0 {
		return release.Pins{Evidence: release.Evidence{At: time.Now(), Error: "No previous ACS patch tag is defined for this release."}}, ""
	}
	previous := fmt.Sprintf("%d.%d.%d", major, minor, patch-1)
	var ref gitTagResponse
	name := previous
	endpoint := repoPath(repo) + "/git/ref/tags/"
	err := c.get(ctx, endpoint+segment(name), &ref)
	if failure, ok := errors.AsType[*requestError](err); ok && failure.statusCode == 404 {
		name = "v" + previous
		err = c.get(ctx, endpoint+segment(name), &ref)
	}
	evidence := release.Evidence{At: time.Now(), URL: "https://github.com/" + repo + "/tree/" + name}
	if err != nil {
		evidence.Error = "Unable to read previous ACS release tag " + previous + ": " + requestProblem(err)
		return release.Pins{Evidence: evidence}, ""
	}
	sha, kind := ref.Object.SHA, ref.Object.Type
	for depth := 0; kind == "tag" && depth < 5; depth++ {
		var tag gitTagResponse
		if err := c.get(ctx, repoPath(repo)+"/git/tags/"+segment(sha), &tag); err != nil {
			evidence.Error = "Unable to resolve previous ACS release tag " + name + ": " + requestProblem(err)
			return release.Pins{Evidence: evidence}, ""
		}
		sha, kind = tag.Object.SHA, tag.Object.Type
	}
	if sha == "" || kind != "commit" {
		evidence.Error = "Previous ACS release tag " + name + " did not resolve to a commit."
		return release.Pins{Evidence: evidence}, ""
	}
	pins, _ := c.readPins(ctx, repo, sha, previous)
	return pins, release.Yes
}

// ReadTarget returns fresh PR or tag identity and opt-out state.
func (c *Client) ReadTarget(ctx context.Context, target release.Target) (dispatch.Identity, error) {
	identity := dispatch.Identity{Target: target, Evidence: release.Evidence{At: time.Now()}}
	if !target.Valid() {
		return identity, fmt.Errorf("invalid GitHub target")
	}
	if target.PR > 0 {
		var p prResponse
		if err := c.get(ctx, repoPath(target.Repo)+"/pulls/"+strconv.Itoa(target.PR), &p); err != nil {
			return identity, err
		}
		identity.SHA = p.Head.SHA
		identity.Base = p.Base.Ref
		identity.Open = p.State == "open" && !p.Merged
		for _, l := range p.Labels {
			if strings.EqualFold(l.Name, "no-release-assistant") {
				identity.OptOut = true
			}
		}
		identity.Evidence.URL = p.HTMLURL
	} else {
		var ref struct {
			Object struct {
				SHA  string `json:"sha"`
				Type string `json:"type"`
			} `json:"object"`
		}
		if err := c.get(ctx, repoPath(target.Repo)+"/git/ref/tags/"+segment(target.Tag), &ref); err != nil {
			return identity, err
		}
		sha, kind := ref.Object.SHA, ref.Object.Type
		for depth := 0; kind == "tag" && depth < 5; depth++ {
			var obj gitTagResponse
			if err := c.get(ctx, repoPath(target.Repo)+"/git/tags/"+segment(sha), &obj); err != nil {
				return identity, err
			}
			sha, kind = obj.Object.SHA, obj.Object.Type
		}
		if kind != "commit" || sha == "" {
			return identity, fmt.Errorf("GitHub tag could not be resolved")
		}
		identity.SHA = sha
		identity.Open = true
		identity.Evidence.URL = "https://github.com/" + target.Repo + "/tree/" + target.Tag
	}
	identity.Evidence.Known = identity.SHA != ""
	if !identity.Evidence.Known {
		return identity, fmt.Errorf("GitHub target SHA unavailable")
	}
	return identity, nil
}
