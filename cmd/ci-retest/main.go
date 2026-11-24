// Command ci-retest prints retry instructions for GitHub Actions and Prow.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/vikin91/ci-retest/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}
