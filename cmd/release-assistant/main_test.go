package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"

	"release-assistant/internal/release"
)

func TestStartupCredentials(t *testing.T) {
	for name, tc := range map[string]struct {
		gh, github, want string
		wantGH           bool
	}{"GH_TOKEN first": {"explicit", "second", "explicit", false}, "GITHUB_TOKEN second": {"", "second", "second", false}, "existing gh login": {"", "", "login", true}} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			called := false
			command := func(_ context.Context, name string, args ...string) string {
				if name == "gh" {
					called = true
					return "login"
				}
				if strings.Contains(strings.Join(args, " "), "server") {
					return "https://api.example.openshiftapps.com:6443"
				}
				return "cluster-token"
			}
			c := loadCredentialsFrom(t.Context(), command)
			if c.github != tc.want || called != tc.wantGH {
				t.Fatal("credential precedence incorrect")
			}
			if token, err := c.clusterToken(t.Context(), "api.example.openshiftapps.com"); err != nil || token != "cluster-token" {
				t.Fatal("startup cluster session not reused")
			}
			if _, err := c.clusterToken(t.Context(), "api.other.openshiftapps.com"); err == nil {
				t.Fatal("cluster token sent to another host")
			}
		})
	}
}

func TestVersionDefaultsFillOnlyUnsetComponentFields(t *testing.T) {
	components := applyVersionDefaults("4.10.12", []release.Source{
		{Name: "fact", Repo: "stackrox/fact"},
		{Name: "collector", Repo: "stackrox/collector", Stream: "9.9", Branch: "custom"},
		{Name: "scanner", Repo: "acme/scanner"},
	})
	if components[0].Stream != "0.2" || components[0].Branch != "release-0.2" {
		t.Fatalf("fact = %+v, want prefilled stream and derived branch", components[0])
	}
	if components[1].Stream != "9.9" || components[1].Branch != "custom" {
		t.Fatalf("collector = %+v, want explicit flags preserved", components[1])
	}
	if components[2].Stream != "2.39" || components[2].Branch != "release-2.39" {
		t.Fatalf("scanner = %+v, want prefilled stream and derived branch", components[2])
	}
	if components[2].Repo != "acme/scanner" {
		t.Fatalf("scanner repository = %q, want the explicit value", components[2].Repo)
	}

	unsupported := applyVersionDefaults("4.9.9", []release.Source{{Name: "fact", Repo: "stackrox/fact"}})
	if unsupported[0].Stream != "" || unsupported[0].Branch != "" {
		t.Fatalf("unsupported version prefilled %+v", unsupported[0])
	}
}

func TestWritesRequireExplicitStartupFlag(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"read-only by default":  {want: "false"},
		"explicit write opt-in": {args: []string{"-enable-writes"}, want: "true"},
		"explicit read-only":    {args: []string{"-enable-writes=false"}, want: "false"},
	} {
		t.Run(name, func(t *testing.T) {
			originalFlags, originalArgs := flag.CommandLine, os.Args
			t.Cleanup(func() { flag.CommandLine, os.Args = originalFlags, originalArgs })
			flag.CommandLine = flag.NewFlagSet("release-assistant", flag.ContinueOnError)
			flag.CommandLine.SetOutput(io.Discard)
			os.Args = append([]string{"release-assistant", "-port=0"}, tc.args...)
			if err := run(); err == nil || !strings.Contains(err.Error(), "port") {
				t.Fatalf("invalid port must stop startup before services: %v", err)
			}
			option := flag.Lookup("enable-writes")
			if option == nil || option.Value.String() != tc.want || option.DefValue != "false" {
				t.Fatalf("startup flag = %+v, want %s", option, tc.want)
			}
			if flag.Lookup("disable-writes") != nil {
				t.Fatal("obsolete flag still registered")
			}
		})
	}
}
