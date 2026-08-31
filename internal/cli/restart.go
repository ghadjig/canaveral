package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/state"
)

func runRestart(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restart", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral restart <feature> <service> [service...]\n\n"+
			"Stop and restart a feature's services, waiting on each one's `ready`\n"+
			"probe before moving on. The log is truncated, so what you see after is\n"+
			"only this run.\n\n"+
			"Services must be named. There is no \"restart everything\": bouncing a\n"+
			"long-running job by accident is worse than typing its name.\n\n"+
			"  canaveral restart small-fixes web\n"+
			"  canaveral restart small-fixes web jobs")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		fs.Usage()
		return fmt.Errorf("specify a feature and at least one service")
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	f, err := state.Load(m.Name, feature.Slug(pos[0]))
	if err != nil {
		return err
	}

	r := reporter{}
	r.Step("restart %s", color(cBold, f.Key()))
	return feature.RestartServices(ctx, m, f, pos[1:], r)
}
