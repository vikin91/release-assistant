package konflux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	recentAdmission = 30 * time.Minute
	metadataList    = "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1,application/json;q=0.9"
)

type (
	// Health records a successful API read and evidence of recent PipelineRun admission.
	Health struct {
		Cluster, Problem            string
		CheckedAt, Next, AdmittedAt time.Time
		APIReadable                 bool
		APIStatus                   int
	}
	healthProbe struct {
		client     http.Client
		host, port string
		namespaces []string
	}
	pipelineRunMetadata struct {
		Metadata struct {
			Namespace string    `json:"namespace"`
			CreatedAt time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
	}
)

// Healthy reports when the probe verified both access and recent admission.
func (h Health) Healthy() bool {
	return h.APIReadable && !h.AdmittedAt.IsZero()
}

func newHealthProbe(c Config, client http.Client) healthProbe {
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	probe := healthProbe{client: client, host: c.apiHost, port: c.apiPort}
	var firstHost string
	for lookup := range c.lookups {
		if firstHost == "" || lookup.host < firstHost {
			firstHost = lookup.host
		}
		if lookup.host == c.apiHost {
			probe.namespaces = append(probe.namespaces, lookup.namespace)
		}
	}
	if len(probe.namespaces) == 0 {
		probe.host = firstHost
	}
	slices.Sort(probe.namespaces)
	probe.namespaces = slices.Compact(probe.namespaces)
	return probe
}

// ProbeHealth reads the approved cluster independently of retry planning and its endpoint backoff.
func (a *Adapter) ProbeHealth(ctx context.Context) Health {
	p := &a.health
	cluster := strings.TrimPrefix(p.host, "api.")
	if len(p.namespaces) == 0 || a.config.KonfluxToken == nil {
		return Health{Cluster: cluster, APIStatus: http.StatusUnauthorized, Problem: "No OpenShift sign-in for an approved Konflux cluster."}
	}
	token, err := a.config.KonfluxToken(ctx, p.host)
	if err != nil || token == "" {
		return Health{Cluster: cluster, APIStatus: http.StatusUnauthorized, Problem: "No OpenShift sign-in for the Konflux cluster."}
	}

	result := Health{Cluster: cluster}
	for _, namespace := range p.namespaces {
		observed := p.readNamespace(ctx, cluster, namespace, token)
		if observed.Healthy() {
			return observed
		}
		if observed.APIReadable || !result.APIReadable {
			result = observed
		}
	}
	return result
}

func (p *healthProbe) readNamespace(ctx context.Context, cluster, namespace, token string) Health {
	result := Health{Cluster: cluster}
	host := p.host
	if p.port != "" {
		host = net.JoinHostPort(host, p.port)
	}
	var live struct {
		Kind  string                `json:"kind"`
		Items []pipelineRunMetadata `json:"items"`
	}
	address := "https://" + host + "/apis/tekton.dev/v1/namespaces/" + namespace + "/pipelineruns?limit=1"
	status, err := p.read(ctx, address, token, metadataList, &live)
	result.APIStatus = status
	if err != nil {
		result.Problem = "PipelineRun API: " + err.Error()
		return result
	}
	if live.Kind != "PartialObjectMetadataList" && live.Kind != "PipelineRunList" {
		result.Problem = "PipelineRun API returned an unexpected response."
		return result
	}
	result.APIReadable = true
	now := time.Now()
	for _, item := range live.Items {
		if created := item.Metadata.CreatedAt; item.Metadata.Namespace == namespace && recent(created, now) {
			result.AdmittedAt = created
			return result
		}
	}
	result.AdmittedAt, err = p.readRecentStart(ctx, host, namespace, token, now)
	if err != nil {
		result.Problem = "PipelineRun events: " + err.Error()
	}
	return result
}

func (p *healthProbe) readRecentStart(ctx context.Context, host, namespace, token string, now time.Time) (time.Time, error) {
	query := url.Values{
		"limit":         {"100"},
		"fieldSelector": {"involvedObject.kind=PipelineRun,reason=Started,type=Normal"},
	}
	for range 4 {
		var page struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				FirstTimestamp time.Time `json:"firstTimestamp"`
				EventTime      time.Time `json:"eventTime"`
				InvolvedObject struct {
					Kind       string `json:"kind"`
					APIVersion string `json:"apiVersion"`
					Namespace  string `json:"namespace"`
				} `json:"involvedObject"`
			} `json:"items"`
		}
		address := "https://" + host + "/api/v1/namespaces/" + namespace + "/events?" + query.Encode()
		if _, err := p.read(ctx, address, token, "application/json", &page); err != nil {
			return time.Time{}, err
		}
		if page.Kind != "EventList" {
			return time.Time{}, errors.New("unexpected response")
		}
		var latest time.Time
		for _, event := range page.Items {
			obj := event.InvolvedObject
			if obj.Kind != "PipelineRun" || obj.Namespace != namespace || !strings.HasPrefix(obj.APIVersion, "tekton.dev/") {
				continue
			}
			at := event.EventTime
			if at.IsZero() {
				at = event.FirstTimestamp
			}
			if at.After(latest) && recent(at, now) {
				latest = at
			}
		}
		if !latest.IsZero() || page.Metadata.Continue == "" {
			return latest, nil
		}
		query.Set("continue", page.Metadata.Continue)
	}
	return time.Time{}, errors.New("recent starts were not verified within the event page limit")
}

func recent(at, now time.Time) bool {
	return !at.After(now) && at.After(now.Add(-recentAdmission))
}

func (p *healthProbe) read(ctx context.Context, address, token, accept string, dest any) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return 0, errors.New("invalid read URL")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	resp, err := p.client.Do(req)
	if err != nil {
		if failure, ok := errors.AsType[net.Error](err); ok && failure.Timeout() {
			return 0, errors.New("request timed out")
		}
		return 0, errors.New("network request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(dest); err != nil {
		return resp.StatusCode, errors.New("could not read the response")
	}
	return resp.StatusCode, nil
}
