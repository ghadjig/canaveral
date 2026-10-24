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
			"stream and by Claude Code's transcripts, with a slow safety refresh;\n"+
			"feature rescans reuse cached agent health.\n\nFlags:")
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

	// A widget runs this for as long as the session lasts, which is days, so
	// an upgrade of canaveral would otherwise leave it streaming whatever the
	// old binary knew. That is how a bar went on reading every opencode 2
	// agent with a build that only spoke opencode 1. Stop once our own
	// executable has been replaced; whatever runs this starts the new one.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if exe, err := os.Executable(); err == nil {
		go watchExecutable(ctx, exe, 10*time.Second, func() {
			fmt.Fprintln(os.Stderr, "canaveral watch: this binary has been replaced; exiting so the new one can take over")
			cancel()
		})
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

// watchExecutable calls replaced once the file at path is no longer the one
// this process started from: renamed over (a new inode, which is how
// scripts/install.sh installs), rewritten in place (a new mtime) or removed.
func watchExecutable(ctx context.Context, path string, every time.Duration, replaced func()) {
	before, err := os.Stat(path)
	if err != nil {
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now, err := os.Stat(path)
		if err == nil && os.SameFile(before, now) && now.ModTime().Equal(before.ModTime()) {
			continue
		}
		replaced()
		return
	}
}
