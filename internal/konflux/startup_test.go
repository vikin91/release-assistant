package konflux

import (
	"context"
	"slices"
	"testing"
)

func TestStartupClusterCredentials(t *testing.T) {
	c := startupConfig(t.Context(), nil, func(_ context.Context, name string, args ...string) string {
		want := []string{"config", "view", "--minify", "--raw", `-o=jsonpath={.clusters[0].cluster.server}{"\n"}{.users[0].user.token}`}
		if name != "oc" || !slices.Equal(args, want) {
			t.Fatalf("unexpected credential command %s %v", name, args)
		}
		return "https://api.example.openshiftapps.com:6443\ncluster-token"
	})
	login := c.oc.current.Load()
	if token, err := login.tokenForHost(t.Context(), "api.example.openshiftapps.com"); err != nil || token != "cluster-token" || login.port != "6443" {
		t.Fatal("startup cluster session not reused")
	}
	if _, err := login.tokenForHost(t.Context(), "api.other.openshiftapps.com"); err == nil {
		t.Fatal("cluster token sent to another host")
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
