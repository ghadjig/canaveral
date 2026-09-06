package feature

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// recordingReporter captures Info lines so a test can read what a person
// staring at the terminal would have been told.
type recordingReporter struct {
	mu    sync.Mutex
	lines []string
}

func (*recordingReporter) Step(string, ...any) {}
func (*recordingReporter) OK(string, ...any)   {}
func (*recordingReporter) Warn(string, ...any) {}
func (r *recordingReporter) Info(format string, a ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, format)
}

func (r *recordingReporter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

func shortBeat(t *testing.T, d time.Duration) {
	t.Helper()
	prev := beatInterval
	beatInterval = d
	t.Cleanup(func() { beatInterval = prev })
}

func TestWhileWaitingReportsWhileTheStepBlocks(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortBeat(t, 5*time.Millisecond)

	r := &recordingReporter{}
	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 1)
	prog.start("service slow")

	err := whileWaiting(r, prog, "slow", time.Minute, "", func() error {
		time.Sleep(60 * time.Millisecond)
		return nil
	})
	if err != nil {
		t.Fatalf("whileWaiting: %v", err)
	}
	if r.count() == 0 {
		t.Fatal("no heartbeat reported: a step that blocks in silence is what this exists to prevent")
	}
}

func TestWhileWaitingStopsReportingOnceTheStepReturns(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortBeat(t, 5*time.Millisecond)

	r := &recordingReporter{}
	if err := whileWaiting(r, nil, "slow", time.Minute, "", func() error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatalf("whileWaiting: %v", err)
	}

	// whileWaiting waits for its reporting goroutine before returning, so the
	// count is final the moment it does. Anything arriving after this is a
	// goroutine still writing to a reporter its caller has moved on from.
	settled := r.count()
	time.Sleep(30 * time.Millisecond)
	if got := r.count(); got != settled {
		t.Errorf("reported %d more line(s) after returning; the ticker outlived the step", got-settled)
	}
}

func TestWhileWaitingKeepsTheProgressRecordFresh(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortBeat(t, 5*time.Millisecond)

	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 1)
	prog.start("service slow")

	// Backdate the step so that, without a heartbeat, InPhase would give up
	// on it — which is exactly what used to happen to a readiness probe
	// allowed longer than state.StalePhaseAfter.
	f.PhaseSince = time.Now().Add(-2 * state.StalePhaseAfter)
	f.PhaseBeat = f.PhaseSince
	if f.InPhase() {
		t.Fatal("precondition: a phase with no recent sign of life should not be believed")
	}

	if err := whileWaiting(quietReporter{}, prog, "slow", time.Minute, "", func() error {
		time.Sleep(30 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatalf("whileWaiting: %v", err)
	}

	if !f.InPhase() {
		t.Error("a step still running is not in phase: the progress bar vanishes mid-boot")
	}
	if since := f.PhaseSince; time.Since(since) < state.StalePhaseAfter {
		t.Error("PhaseSince moved: the heartbeat reset the clock a reader renders as step elapsed time")
	}
}

func TestWhileWaitingReturnsTheStepsError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortBeat(t, time.Hour) // no beats; only the return value is under test

	want := os.ErrDeadlineExceeded
	if got := whileWaiting(quietReporter{}, nil, "slow", time.Minute, "", func() error {
		return want
	}); got != want {
		t.Errorf("err = %v, want %v", got, want)
	}
}

func TestLogWatchDescribesGrowthThenQuiet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte("start\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := watchLog(path)
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := w.describe(); !strings.HasPrefix(got, "log +") {
		t.Errorf("describe() = %q, want it to report growth", got)
	}
	if got := w.describe(); !strings.HasPrefix(got, "log quiet for ") {
		t.Errorf("describe() = %q, want it to report the log has gone quiet", got)
	}
}

func TestLogWatchDescribesAMissingLog(t *testing.T) {
	w := watchLog(filepath.Join(t.TempDir(), "absent.log"))
	if got := w.describe(); got != "no log yet" {
		t.Errorf("describe() = %q, want %q", got, "no log yet")
	}
}

func TestLogWatchTreatsATruncatedLogAsQuietNotShrunken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatal(err)
	}
	w := watchLog(path)

	// unit.Start truncates a service's log on every launch. Reading the
	// smaller size as growth would report a negative byte count.
	if err := os.WriteFile(path, []byte("restarted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := w.describe(); strings.Contains(got, "-") {
		t.Errorf("describe() = %q, want no negative growth after a truncation", got)
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{512, "512B"},
		{1024, "1.0K"},
		{1536, "1.5K"},
		{5 * 1024 * 1024, "5.0M"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLastLineReturnsTheMostRecentCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte("first\nsecond\nthird\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := watchLog(path).lastLine(); got != "third" {
		t.Errorf("lastLine() = %q, want %q", got, "third")
	}
}

// devspace colours nearly every line it prints. These bytes reach a status bar
// as JSON, where they render as literal garbage rather than as colour.
func TestLastLineStripsANSIEscapes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	body := "\x1b[0;32m[info]\x1b[0m Running bundle install...\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := watchLog(path).lastLine()
	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("lastLine() = %q, still carries escape bytes", got)
	}
	if got != "[info] Running bundle install..." {
		t.Errorf("lastLine() = %q", got)
	}
}

// A service caught mid-write has bytes after the final newline. Showing half a
// line, then the other half ten seconds later, reads as corruption.
func TestLastLineIgnoresAPartiallyWrittenLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte("complete line\npartial with no newl"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := watchLog(path).lastLine(); got != "complete line" {
		t.Errorf("lastLine() = %q, want the last COMPLETE line", got)
	}
}

func TestLastLineSkipsBlankTrailingLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte("something happened\n\n   \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := watchLog(path).lastLine(); got != "something happened" {
		t.Errorf("lastLine() = %q, want the last line with content in it", got)
	}
}

// Only the end of the file is read: this is sampled every beat, and a devspace
// log with the pod's output streamed into it runs to megabytes.
func TestLastLineReadsOnlyTheTailOfALargeLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	big := strings.Repeat("noise line that is here only to make the file large\n", 40000)
	if err := os.WriteFile(path, []byte(big+"the last word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() < tailBytes*10 {
		t.Fatalf("test log is not big enough to exercise the tail read")
	}
	if got := watchLog(path).lastLine(); got != "the last word" {
		t.Errorf("lastLine() = %q", got)
	}
}

func TestLastLineTruncatesARunawayLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 4000)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := watchLog(path).lastLine()
	if n := len([]rune(got)); n > maxLineRunes+1 {
		t.Errorf("lastLine() is %d runes, want it capped near %d", n, maxLineRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("lastLine() = %q, want a truncation marker", got)
	}
}

func TestLastLineIsEmptyWithoutALog(t *testing.T) {
	if got := watchLog(filepath.Join(t.TempDir(), "absent.log")).lastLine(); got != "" {
		t.Errorf("lastLine() = %q, want empty", got)
	}
	if got := watchLog("").lastLine(); got != "" {
		t.Errorf("lastLine() with no path = %q, want empty", got)
	}
}

// The status bar and the terminal have different readers and different
// tolerances, so the record is refreshed more often than a line is printed.
func TestWhileWaitingBeatsMoreOftenThanItPrints(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	shortBeat(t, 5*time.Millisecond)

	path := filepath.Join(t.TempDir(), "svc.log")
	if err := os.WriteFile(path, []byte("Running bundle install...\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recordingReporter{}
	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 1)
	prog.start("service slow")

	if err := whileWaiting(r, prog, "slow", time.Minute, path, func() error {
		time.Sleep(120 * time.Millisecond)
		return nil
	}); err != nil {
		t.Fatalf("whileWaiting: %v", err)
	}

	if f.PhaseLine != "Running bundle install..." {
		t.Errorf("PhaseLine = %q, want the log's last line on the record", f.PhaseLine)
	}
	// ~24 beats in 120ms at a 5ms interval, so a 1:1 cadence would print far
	// more than a third of them.
	beats := 120 / 5
	if got := r.count(); got > beats/2 {
		t.Errorf("printed %d lines for roughly %d beats; the terminal is being written as often as the record", got, beats)
	}
	if r.count() == 0 {
		t.Error("printed nothing at all: the terminal still needs its line")
	}
}
