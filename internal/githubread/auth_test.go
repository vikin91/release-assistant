package githubread

import "testing"

func TestAuthTokenPrecedence(t *testing.T) {
	for name, tc := range map[string]struct {
		gh, github, want string
		wantSaved        bool
	}{
		"GH_TOKEN first":    {" explicit ", "second", "explicit", false},
		"GITHUB_TOKEN next": {" ", " second ", "second", false},
		"saved gh login":    {"", "", "login", true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("GH_TOKEN", tc.gh)
			t.Setenv("GITHUB_TOKEN", tc.github)
			called := false
			token := authToken(func() string {
				called = true
				return "login"
			})
			if token != tc.want || called != tc.wantSaved {
				t.Fatal("incorrect GitHub credential precedence")
			}
		})
	}
}
