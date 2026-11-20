package main

import (
	"flag"
	"io"
	"maps"
	"os"
	"strings"
	"testing"

	"release-assistant/internal/konflux"
	"release-assistant/internal/release"
)

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

func TestDefaultKonfluxLookup(t *testing.T) {
	const defaultLookup = "296509,api.stone-prd-rh01.pg1f.p1.openshiftapps.com,rh-acs-tenant"
	const customLookup = "296509,api.stone-prd-rh01.pg1f.p1.openshiftapps.com,other-tenant"
	const otherCluster = "296509,api.stone-stg.example.openshiftapps.com,rh-acs-tenant"
	for name, tc := range map[string]struct {
		explicit, want []string
	}{
		"no flags uses the ACS namespace": {want: []string{defaultLookup}},
		"one explicit flag replaces the default": {
			explicit: []string{customLookup},
			want:     []string{customLookup},
		},
		"repeated flags replace the default": {
			explicit: []string{customLookup, otherCluster},
			want:     []string{customLookup, otherCluster},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, want := make(konflux.Lookups), make(konflux.Lookups)
			for _, value := range tc.explicit {
				if err := got.Add(value); err != nil {
					t.Fatal(err)
				}
			}
			for _, value := range tc.want {
				if err := want.Add(value); err != nil {
					t.Fatal(err)
				}
			}
			if err := applyDefaultKonfluxLookup(got); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(got, want) {
				t.Fatalf("lookup policy = %v, want %v", got, want)
			}
		})
	}
}
