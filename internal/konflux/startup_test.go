package konflux

import (
	"context"
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
			c := startupConfig(t.Context(), nil, command)
			if c.GitHubToken != tc.want || called != tc.wantGH {
				t.Fatal("credential precedence incorrect")
			}
			if token, err := c.KonfluxToken(t.Context(), "api.example.openshiftapps.com"); err != nil || token != "cluster-token" {
				t.Fatal("startup cluster session not reused")
			}
			if _, err := c.KonfluxToken(t.Context(), "api.other.openshiftapps.com"); err == nil {
				t.Fatal("cluster token sent to another host")
			}
		})
	}
}

func TestKonfluxLookupRequiresExactApprovedTuple(t *testing.T) {
	const host = "api.example.openshiftapps.com"
	lookups := make(Lookups)
	if lookups.allows(t.Context(), 42, host, "builds") {
		t.Fatal("empty policy allowed a lookup")
	}
	for _, value := range []string{"42," + host + ",builds", "43," + host + ",release"} {
		if err := lookups.Add(value); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		konfluxLookup
		allowed bool
	}{
		"approved build App":   {konfluxLookup{42, host, "builds"}, true},
		"approved release App": {konfluxLookup{43, host, "release"}, true},
		"wrong namespace":      {konfluxLookup{42, host, "release"}, false},
		"wrong App":            {konfluxLookup{44, host, "builds"}, false},
		"missing App":          {konfluxLookup{0, host, "builds"}, false},
		"wrong hostname":       {konfluxLookup{42, "api.other.openshiftapps.com", "builds"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := lookups.allows(t.Context(), tc.appID, tc.host, tc.namespace); got != tc.allowed {
				t.Fatalf("policy returned %t, want %t", got, tc.allowed)
			}
		})
	}
	for _, value := range []string{"0," + host + ",builds", "42,*,builds", "42," + host + ":6443,builds", "42," + host + ",*", "9223372036854775808," + host + ",builds"} {
		if err := lookups.Add(value); err == nil {
			t.Errorf("accepted invalid lookup %q", value)
		}
	}
}
