package github

import (
	"regexp"
	"testing"

	"release-assistant/internal/release"
)

func TestTargetedProwComment(t *testing.T) {
	job := func(name, command, trigger string) prowTrigger {
		return prowTrigger{prowJob: prowJob{Name: name, Context: "ci/prow/" + name, Command: command}, pattern: regexp.MustCompile(trigger)}
	}
	a := job("a", "/test alias-a", `(?m)^/test alias-a$`)
	b := job("b", "/test alias-b", `(?m)^/test alias-b$`)
	for name, tc := range map[string]struct {
		jobs   []prowTrigger
		failed map[string]failedPresubmit
		want   string
		warn   bool
	}{
		"only failed jobs in stable order":        {[]prowTrigger{b, a, job("green", "/test green", `(?m)^/test green$`)}, map[string]failedPresubmit{b.Context: {Name: b.Name}, a.Context: {Name: a.Name}}, "/test alias-a\n/test alias-b", false},
		"command also targets a green job":        {[]prowTrigger{a, b, job("green", "/test green", `(?m)^/test (green|alias-a)$`)}, map[string]failedPresubmit{a.Context: {Name: a.Name}, b.Context: {Name: b.Name}}, "/test alias-b", true},
		"multiline body also targets a green job": {[]prowTrigger{a, b, job("green", "/test green", `alias-a\n/test alias-b`)}, map[string]failedPresubmit{a.Context: {Name: a.Name}, b.Context: {Name: b.Name}}, "", true},
		"build belongs to a different job":        {[]prowTrigger{a}, map[string]failedPresubmit{a.Context: {Name: "another-job"}}, "", true},
		"duplicate context and build":             {[]prowTrigger{a, a}, map[string]failedPresubmit{a.Context: {Name: a.Name}}, "", true},
		"blanket test command":                    {[]prowTrigger{job("a", "/test all", `/test all`)}, map[string]failedPresubmit{a.Context: {Name: a.Name}}, "", true},
		"blanket retest command":                  {[]prowTrigger{job("a", "/retest", `/retest`)}, map[string]failedPresubmit{a.Context: {Name: a.Name}}, "", true},
		"extra command injected into config":      {[]prowTrigger{job("a", "/test alias-a\n/retest", `alias-a`)}, map[string]failedPresubmit{a.Context: {Name: a.Name}}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			jobs, warnings := targetedProwRetries(tc.failed, tc.jobs)
			report := release.ManualRetryReport{Prow: jobs}
			if got := report.ProwComment(); got != tc.want || (len(warnings) > 0) != tc.warn {
				t.Fatalf("comment %q, warnings %v", got, warnings)
			}
		})
	}
}
