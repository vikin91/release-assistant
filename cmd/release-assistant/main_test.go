package main

import (
	"context"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
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
