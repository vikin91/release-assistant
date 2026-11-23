package konflux

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"release-assistant/internal/githubread"
)

type (
	konfluxLookup struct {
		appID           int64
		host, namespace string
	}
	// Lookups identifies the GitHub Apps, cluster hosts, and namespaces approved at startup.
	Lookups map[konfluxLookup]struct{}
)

var konfluxLookupPattern = regexp.MustCompile(`^([1-9][0-9]*),(api\.(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+openshiftapps\.com),([a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)$`)

// Add parses one APP_ID,API_HOST,NAMESPACE lookup flag.
func (lookups Lookups) Add(value string) error {
	fields := konfluxLookupPattern.FindStringSubmatch(value)
	if fields == nil {
		return errors.New("expected a positive GitHub App ID, a Konflux API hostname, and a namespace, separated by commas")
	}
	appID, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return errors.New("GitHub App ID is out of range")
	}
	lookups[konfluxLookup{appID: appID, host: fields[2], namespace: fields[3]}] = struct{}{}
	return nil
}

func (lookups Lookups) allows(_ context.Context, appID int64, host, namespace string) bool {
	_, allowed := lookups[konfluxLookup{appID: appID, host: host, namespace: namespace}]
	return allowed
}

// StartupConfig reads GitHub, Quay and cluster credentials for the approved lookups.
func StartupConfig(ctx context.Context, lookups Lookups) Config {
	token := githubread.AuthToken(ctx)
	config := startupConfig(ctx, lookups, commandOutput)
	config.GitHubToken = token
	return config
}

func startupConfig(ctx context.Context, lookups Lookups, command func(context.Context, string, ...string) string) Config {
	c := Config{
		AllowKonfluxLookup: lookups.allows,
		lookups:            lookups,
		QuayUsername:       os.Getenv("QUAY_USERNAME"),
		QuayPassword:       os.Getenv("QUAY_PASSWORD"),
	}
	if c.QuayUsername == "" && c.QuayPassword == "" {
		c.QuayUsername = os.Getenv("QUAY_RHACS_ENG_RO_USERNAME")
		c.QuayPassword = os.Getenv("QUAY_RHACS_ENG_RO_PASSWORD")
	}
	var clusterHost, clusterBearer string
	server := command(ctx, "oc", "whoami", "--show-server")
	if u, err := url.Parse(server); err == nil && u.Scheme == "https" {
		clusterHost = u.Hostname()
		c.apiHost, c.apiPort = clusterHost, u.Port()
		clusterBearer = command(ctx, "oc", "whoami", "--show-token")
	}
	c.KonfluxToken = func(_ context.Context, host string) (string, error) {
		if host != clusterHost || clusterBearer == "" {
			return "", fmt.Errorf("no startup oc authentication for this cluster")
		}
		return clusterBearer, nil
	}
	return c
}
func commandOutput(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
