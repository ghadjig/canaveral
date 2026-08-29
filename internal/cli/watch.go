package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/bandito/canaveral/internal/watch"
)

func runWatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral watch [flags]\n\n"+
			"Stream a JSON snapshot of every feature's agent state, one line per\n"+
			"change, for a status widget to consume. Driven by opencode's event\n"+
			"stream rather than polling, so it is idle until something happens.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		all      = fs.Bool("all", false, "watch every project, not just this one")
		debounce = fs.Duration("debounce", 150*time.Millisecond, "coalesce bursts of events into one refresh")
		rescan   = fs.Duration("rescan", 3*time.Second, "how often to look for new or removed features")
		safety   = fs.Duration("safety", 30*time.Second, "full refresh interval, in case an event is missed")
		git      = fs.Duration("git", 30*time.Second, "how often to remeasure branch stats (costs git subprocesses)")
	)
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	project := ""
	if !*all {
		m, err := loadManifest()
		if err != nil {
			return err
		}
		project = m.Name
	}

	r := watch.NewRunner(watch.Options{
		Project:  project,
		Debounce: *debounce,
		Rescan:   *rescan,
		Safety:   *safety,
		Git:      *git,
	})

	// Line-buffered: a consumer reads this a line at a time, so each
	// snapshot has to reach it as it is produced rather than sitting in a
	// buffer until the process exits.
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	return r.Run(ctx, w)
}
