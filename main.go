// Command canaveral launches and controls parallel agent workspaces.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/bandito/canaveral/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), cli.StopSignals()...)
	code := cli.Main(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
