package feature

// Keeping a slow step visible while it blocks.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// beatInterval is how often a blocking step samples its log and refreshes the
// phase record.
//
// Ten seconds, not the thirty this started at. The record is what a status bar
// reads, and it now carries the service's most recent log line — a field whose
// whole value is being current. At thirty seconds "where is it now" was
// answered by where it was half a minute ago, which during `bundle install`
// is a different phase of the boot. The write is a few hundred bytes to a file
// `canaveral watch` is already re-reading five times a second, so the cost of
// sampling more often is not worth measuring.
//
// A var, not a const, so a test can shrink it rather than sitting out the real
// interval to see a single beat.
var beatInterval = 10 * time.Second

// reportEvery is how often the *terminal* line is printed, as a multiple of
// beatInterval.
//
// Deliberately not the same cadence. The two outputs have different readers
// and different tolerances: a status bar is glanced at and wants the freshest
// answer, while a terminal line is scrolled through afterwards and wants to
// stay readable — at ten seconds a thirty-minute readiness probe would leave
// 180 near-identical lines behind it. Thirty seconds was right for the
// terminal and stays.
const reportEvery = 3

// whileWaiting runs fn, reporting every beatInterval until it returns.
//
// This exists because of a real abandonment: a devspace cold start was killed
// at 7m36s, two minutes before it would have finished, because canaveral had
// printed "waiting for http readiness probe, up to 30m0s" and then said
// nothing at all while the service's own log was filling up the whole time.
// The information needed to wait it out was on disk and simply never shown.
//
// what names the thing being waited on, limit is the bound it has been given,
// and logPath is the service's log — whether it is still growing, and what it
// last said, are the two facts that separate "slow" from "wedged".
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
		for n := 1; ; n++ {
			select {
			case <-done:
				return
			case <-t.C:
				// Sampled once and used twice: describe advances the watch's
				// own baseline, so asking it a second time for the status bar
				// would report "quiet" against the reading the terminal line
				// had just consumed.
				detail := w.describe()
				prog.beat(state.PhaseNote{Detail: detail, Line: w.lastLine()})
				if n%reportEvery == 0 {
					r.Info("still waiting for %s: %s of %s · %s",
						what, time.Since(start).Round(time.Second), limit, detail)
				}
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

// tailBytes bounds how much of the end of a log is read to find its last line.
//
// The whole file is the obvious implementation and the wrong one here: a
// devspace log with the pod's own output streamed into it passes a megabyte
// during a normal boot, and this is sampled every beatInterval for the length
// of the wait. 8 KiB is far more than one line and cheap to re-read.
const tailBytes = 8 << 10

// ansiRe matches the colour escapes a terminal-oriented service writes.
//
// Stripped because this string is bound for a status bar via JSON, which will
// render the escape bytes as literal garbage rather than as colour. devspace
// writes them on nearly every line it prints itself.
var ansiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// lastLine returns the service's most recent complete log line, cleaned up for
// display, or "" when there is nothing usable yet.
func (w *logWatch) lastLine() string {
	if w.path == "" {
		return ""
	}
	f, err := os.Open(w.path)
	if err != nil {
		return ""
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	off, n := int64(0), fi.Size()
	if n > tailBytes {
		off, n = n-tailBytes, tailBytes
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return ""
	}

	// Anything after the final newline is a line the service is still in the
	// middle of writing. Showing half a line, and then the other half ten
	// seconds later, reads as corruption rather than as progress.
	s := string(buf)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	} else if off > 0 {
		// No newline in the whole window and more file before it: one
		// enormous line, and no way to know where it started.
		return ""
	}

	for _, line := range slices.Backward(strings.Split(s, "\n")) {
		line = strings.TrimSpace(ansiRe.ReplaceAllString(line, ""))
		if line == "" {
			continue
		}
		return truncate(line, maxLineRunes)
	}
	return ""
}

// maxLineRunes caps the line put on the phase record.
//
// Not for the display's sake — a status bar elides to its own width, and it is
// the only consumer. For the record's: it is rewritten every beat and read by
// `canaveral watch` five times a second, and a devspace log has single lines
// carrying a whole cache key that run past a kilobyte.
const maxLineRunes = 160

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
