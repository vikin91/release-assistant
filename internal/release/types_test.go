package release

import "testing"

func TestChecksStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		checks []Check
		stale  bool
		want   Truth
	}{
		"should accept successful required checks":          {checks: []Check{{Required: Yes, Status: "completed", Conclusion: "success"}}, want: Yes},
		"should accept legacy success":                      {checks: []Check{{Required: Yes, Status: "success"}}, want: Yes},
		"should ignore optional failures":                   {checks: []Check{{Required: No, Status: "failure"}}, want: Yes},
		"should reject required failure":                    {checks: []Check{{Required: Yes, Conclusion: "failure"}}, want: No},
		"should reject timeout":                             {checks: []Check{{Required: Yes, Conclusion: "timed_out"}}, want: No},
		"should wait for running checks":                    {checks: []Check{{Required: Yes, Status: "in_progress"}}},
		"should wait for required check not yet reported":   {checks: []Check{{Required: Yes, Status: "pending"}}},
		"should preserve unknown requirements":              {checks: []Check{{Status: "success"}}},
		"should preserve read failures":                     {checks: []Check{{Required: Yes, Status: "success"}}, stale: true},
		"should accept verified absence of required checks": {want: Yes},
	} {
		t.Run(name, func(t *testing.T) {
			pr := PR{Evidence: freshEvidence(), ChecksEvidence: freshEvidence(), Checks: tc.checks}
			if tc.stale {
				pr.ChecksEvidence.Error = "timeout"
			}
			if got := pr.ChecksStatus(); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}
