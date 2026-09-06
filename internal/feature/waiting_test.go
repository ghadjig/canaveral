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
