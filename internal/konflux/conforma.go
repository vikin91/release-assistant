package konflux

import (
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"release-assistant/internal/release"
)

var conformaRunPath = regexp.MustCompile(`^/ns/([a-z0-9-]{1,63})/pipelinerun/(acs-conforma-dev-[a-z0-9-]{1,100})$`)

// ReadConformaReport reads the developer policy log for an approved tenant and verified registry digest.
func (a *Adapter) ReadConformaReport(ctx context.Context, appID int64, runURL, imageRepository, digest string) release.ConformaReport {
	if a.oc == nil || digest == "" {
		return release.ConformaReport{}
	}
	location, err := url.Parse(runURL)
	if err != nil || location.Scheme != "https" || location.User != nil || location.Port() != "" ||
		location.RawQuery != "" || location.Fragment != "" || !strings.HasPrefix(location.Hostname(), "konflux-ui.apps.") {
		return release.ConformaReport{}
	}
	path := conformaRunPath.FindStringSubmatch(location.Path)
	if path == nil {
		return release.ConformaReport{}
	}
	host := "api." + strings.TrimPrefix(location.Hostname(), "konflux-ui.apps.")
	namespace, run := path[1], path[2]
	login := a.oc.current.Load()
	if login == nil || login.host != host || !a.health.lookups.allows(ctx, appID, host, namespace) {
		return release.ConformaReport{}
	}
	if login.port != "" {
		host = net.JoinHostPort(host, login.port)
	}
	var task struct {
		Status struct{ PodName string }
	}
	address := "https://" + host
	if _, err := a.health.read(ctx, address+"/apis/tekton.dev/v1/namespaces/"+namespace+"/taskruns/"+run+"-verify", login.token, "application/json", &task); err != nil ||
		task.Status.PodName != run+"-verify-pod" {
		return release.ConformaReport{}
	}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/api/v1/namespaces/"+namespace+"/pods/"+task.Status.PodName+"/log?container=step-report-json", nil)
	if err != nil {
		return release.ConformaReport{}
	}
	req.Header.Set("Authorization", "Bearer "+login.token)
	resp, err := a.health.client.Do(req)
	if err != nil {
		return release.ConformaReport{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release.ConformaReport{}
	}
	report := parseConformaReport(resp.Body, imageRepository, digest)
	if report.Digest != "" {
		report.RunURL = runURL
	}
	return report
}

func parseConformaReport(body io.Reader, repository, digest string) release.ConformaReport {
	type finding struct {
		Msg      string
		Metadata struct{ Code, Term string }
	}
	var raw struct {
		Components []struct {
			Name, ContainerImage string
			Violations, Warnings []finding
		}
	}
	decoder := json.NewDecoder(io.LimitReader(body, 8<<20))
	if err := decoder.Decode(&raw); err != nil {
		return release.ConformaReport{}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return release.ConformaReport{}
	}
	var parent string
	for _, component := range raw.Components {
		if component.ContainerImage == repository+"@"+digest {
			parent = component.Name
			break
		}
	}
	if parent == "" {
		return release.ConformaReport{}
	}
	report := release.ConformaReport{Digest: digest}
	for _, component := range raw.Components {
		architecture := ""
		if component.Name != parent {
			platform, ok := strings.CutPrefix(component.Name, parent+"-sha256:")
			_, architecture, _ = strings.Cut(platform, "-")
			if !ok || architecture == "" || !strings.HasPrefix(component.ContainerImage, repository+"@sha256:") {
				continue
			}
		}
		for _, item := range slices.Concat(component.Violations, component.Warnings) {
			if strings.HasPrefix(item.Metadata.Code, "cve.") && strings.HasPrefix(item.Metadata.Term, "CVE-") {
				report.Findings = append(report.Findings, release.ConformaFinding{ID: item.Metadata.Term, Architecture: architecture, Rule: item.Metadata.Code, Message: item.Msg})
			}
		}
	}
	slices.SortFunc(report.Findings, func(a, b release.ConformaFinding) int {
		return cmp.Or(cmp.Compare(a.ID, b.ID), cmp.Compare(a.Architecture, b.Architecture), cmp.Compare(a.Rule, b.Rule))
	})
	report.Findings = slices.Compact(report.Findings)
	return report
}
