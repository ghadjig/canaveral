// Command canaveral launches and controls parallel agent workspaces.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/bandito/canaveral/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(cli.Main(ctx, os.Args[1:]))
}
