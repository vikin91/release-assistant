package konflux

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
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

	ocLogin struct {
		host, port, token string
	}
	ocCredentials struct {
		current atomic.Pointer[ocLogin]
		read    func(context.Context) *ocLogin
	}
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
	c.oc = &ocCredentials{read: func(ctx context.Context) *ocLogin {
		// Read both values from the same kubeconfig snapshot so a concurrent login cannot mix them.
		output := command(ctx, "oc", "config", "view", "--minify", "--raw",
			`-o=jsonpath={.clusters[0].cluster.server}{"\n"}{.users[0].user.token}`)
		server, token, ok := strings.Cut(output, "\n")
		u, err := url.Parse(server)
		if !ok || token == "" || strings.ContainsAny(token, " \t\r\n") ||
			err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil
		}
		return &ocLogin{host: u.Hostname(), port: u.Port(), token: token}
	}}
	c.oc.current.Store(c.oc.read(ctx))
	return c
}

func (l *ocLogin) tokenForHost(_ context.Context, host string) (string, error) {
	if l == nil || l.host != host {
		return "", errors.New("no OpenShift sign-in for this cluster")
	}
	return l.token, nil
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
