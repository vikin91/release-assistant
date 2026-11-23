package githubread

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// AuthToken uses GH_TOKEN, GITHUB_TOKEN, or the saved gh login, in that order.
func AuthToken(ctx context.Context) string {
	return authToken(func() string {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", "github.com").Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	})
}

func authToken(savedToken func() string) string {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token
		}
	}
	return savedToken()
}
