package feature

// Keeping a slow step visible while it blocks.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// beatInterval is how often a blocking step says it is still there.
//
// Two jobs, one ticker. It refreshes the phase record, which has to happen
// comfortably inside state.StalePhaseAfter or a watcher gives up on a boot
// that is genuinely still going; and it prints a line, which is the only
// thing distinguishing a slow start from a hung one for whoever is watching
// the terminal. Thirty seconds is short enough to answer "is this alive?"
// before impatience sets in, and long enough that even a thirty-minute
// readiness probe adds sixty lines rather than a scroll of noise.
//
// A var, not a const, so a test can shrink it rather than sitting out the
// real thirty seconds to see a single beat.
var beatInterval = 30 * time.Second

// whileWaiting runs fn, reporting every beatInterval until it returns.
//
// This exists because of a real abandonment: a devspace cold start was killed
// at 7m36s, two minutes before it would have finished, because canaveral had
// printed "waiting for http readiness probe, up to 30m0s" and then said
// nothing at all while the service's own log was filling up the whole time.
// The information needed to wait it out was on disk and simply never shown.
//
// what names the thing being waited on, limit is the bound it has been given,
// and logPath is the service's log — whether it is still growing is the one
// fact that separates "slow" from "wedged", and it costs a stat to ask.
func whileWaiting(r Reporter, prog *progress, what string, limit time.Duration, logPath string, fn func() error) error {
	// Sampled before fn starts, so the first tick already has a baseline to
	// compare against and can say something more useful than "no idea yet".
	w := watchLog(logPath)
	start := time.Now()

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(beatInterval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// Sampled once and used twice: describe advances the watch's
				// own baseline, so asking it a second time for the status bar
				// would report "quiet" against the reading the terminal line
				// had just consumed.
				detail := w.describe()
				prog.beat(detail)
				r.Info("still waiting for %s: %s of %s · %s",
					what, time.Since(start).Round(time.Second), limit, detail)
			}
		}
	}()

	err := fn()
	close(done)
	// Waited on rather than left to finish on its own: it touches the feature
	// record and the reporter, both of which the caller goes on using the
	// moment this returns.
	wg.Wait()
	return err
}

// logWatch tracks how a service's log is growing between heartbeats.
type logWatch struct {
	path string
	size int64
	// grewAt is when the size was last seen to change, which is the age
	// reported when it stops changing. Zero while the file has never existed.
	grewAt time.Time
	// seen is false until the file has been stat'ed successfully once, so a
	// service that has not yet written a byte is not reported as having gone
	// quiet the instant it started.
	seen bool
}

func watchLog(path string) *logWatch {
	w := &logWatch{path: path}
	w.sample()
	return w
}

// sample reads the current size, reporting whether it grew since the last call.
func (w *logWatch) sample() (grew int64) {
	if w.path == "" {
		return 0
	}
	fi, err := os.Stat(w.path)
	if err != nil {
		return 0
	}
	n := fi.Size()
	// Only growth counts. A log that is truncated and rewritten — which
	// unit.Start does on every launch — would otherwise read as a shrinking
	// file and, taken as "no change", as a service that had gone quiet.
	if n > w.size {
		grew = n - w.size
		w.grewAt = time.Now()
	}
	w.size, w.seen = n, true
	return grew
}

// describe says, in a few words, whether the thing being waited on is still
// producing output.
func (w *logWatch) describe() string {
	grew := w.sample()
	switch {
	case !w.seen:
		return "no log yet"
	case grew > 0:
		return "log +" + humanBytes(grew)
	case w.grewAt.IsZero():
		return "log still empty"
	default:
		return "log quiet for " + time.Since(w.grewAt).Round(time.Second).String()
	}
}

// humanBytes renders a byte count the way the status table does. Local to this
// package because internal/cli's copy is about presenting a table and this is
// about presenting a sentence; sharing them would couple two things that only
// happen to format alike today.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(n)/float64(div), "KMGT"[exp])
}
