package unit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFakeSystemd writes fake `systemd-run` and `systemctl` scripts into
// a temp dir and prepends it to PATH, so exec.LookPath resolves to them
// instead of any real systemd user manager. Behavior is controlled by the
// FAKE_SYSTEMCTL_*/FAKE_SYSTEMD_RUN_* environment variables the scripts
// read — set whichever ones a given test needs via t.Setenv. If FAKE_LOG is
// set, every invocation appends its args (one line, space-joined) to that
// file, so a test can assert on calls it does not otherwise observe (e.g.
// that Start's timeout path really did call Stop and Reset).
func installFakeSystemd(t *testing.T) {
	t.Helper()
	dir := t.TempDir()

	systemdRun := `#!/bin/sh
if [ -n "$FAKE_LOG" ]; then echo "systemd-run $*" >> "$FAKE_LOG"; fi
if [ -n "$FAKE_SYSTEMD_RUN_SLEEP" ]; then sleep "$FAKE_SYSTEMD_RUN_SLEEP"; fi
printf '%s' "$FAKE_SYSTEMD_RUN_STDERR" >&2
exit "${FAKE_SYSTEMD_RUN_EXIT:-0}"
`
	systemctl := `#!/bin/sh
if [ -n "$FAKE_LOG" ]; then echo "systemctl $*" >> "$FAKE_LOG"; fi
shift # drop --user
case "$1" in
  stop)
    if [ "$2" = "$FAKE_SYSTEMCTL_STOP_FAIL_UNIT" ] && [ -n "$FAKE_SYSTEMCTL_STOP_FAIL_UNIT" ]; then
      printf '%s' "$FAKE_SYSTEMCTL_STOP_FAIL_STDERR" >&2
      exit 1
    fi
    printf '%s' "$FAKE_SYSTEMCTL_STOP_STDERR" >&2
    exit "${FAKE_SYSTEMCTL_STOP_EXIT:-0}"
    ;;
  reset-failed)
    exit 0
    ;;
  show)
    if [ "$2" = "-p" ]; then
      printf '%s' "$FAKE_SYSTEMCTL_SHOW_VERSION"
      exit 0
    fi
    printf '%s' "$FAKE_SYSTEMCTL_SHOW_OUTPUT"
    exit "${FAKE_SYSTEMCTL_SHOW_EXIT:-0}"
    ;;
  list-units)
    printf '%s' "$FAKE_SYSTEMCTL_LIST_UNITS_OUTPUT"
    exit 0
    ;;
  is-system-running)
    exit "${FAKE_SYSTEMCTL_IS_SYSTEM_RUNNING_EXIT:-0}"
    ;;
  *)
    exit 1
    ;;
esac
`
	for name, body := range map[string]string{"systemd-run": systemdRun, "systemctl": systemctl} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// fakeLog points FAKE_LOG at a fresh file in a temp dir and returns a
// reader for its eventual contents.
func fakeLog(t *testing.T) func() string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "log")
	t.Setenv("FAKE_LOG", path)
	return func() string {
		b, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func TestStartSucceedsAndTruncatesTheLog(t *testing.T) {
	installFakeSystemd(t)
	logPath := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(logPath, []byte("stale output from a previous run"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Start(context.Background(), Spec{Name: "canaveral-test-start-ok", Cmd: "true", LogPath: logPath})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("log = %q, want truncated before the unit starts", b)
	}
}

func TestStartFailsWithSystemdRunsStderr(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMD_RUN_EXIT", "1")
	t.Setenv("FAKE_SYSTEMD_RUN_STDERR", "Failed to start transient unit: some dbus error")

	err := Start(context.Background(), Spec{Name: "canaveral-test-start-fail", Cmd: "true"})
	if err == nil || !strings.Contains(err.Error(), "some dbus error") {
		t.Errorf("err = %v, want it to include systemd-run's stderr", err)
	}
}

func TestStartTimesOutAndCleansUpTheHalfLaunchedUnit(t *testing.T) {
	installFakeSystemd(t)
	getLog := fakeLog(t)
	t.Setenv("FAKE_SYSTEMD_RUN_SLEEP", "1")

	orig := startTimeout
	startTimeout = 20 * time.Millisecond
	t.Cleanup(func() { startTimeout = orig })

	err := Start(context.Background(), Spec{Name: "canaveral-test-start-timeout", Cmd: "true"})
	if err == nil || !strings.Contains(err.Error(), "no answer from the systemd user manager") {
		t.Fatalf("err = %v, want a no-answer timeout error", err)
	}
	// A launch that timed out still asked systemd to start the unit, which
	// keeps going unless explicitly undone — Start must call Stop and Reset
	// for the name it gave up on.
	log := getLog()
	if !strings.Contains(log, "systemctl --user stop canaveral-test-start-timeout.service") {
		t.Errorf("log = %q, want a stop of the abandoned unit", log)
	}
	if !strings.Contains(log, "systemctl --user reset-failed canaveral-test-start-timeout.service") {
		t.Errorf("log = %q, want a reset-failed of the abandoned unit", log)
	}
}

func TestStopIgnoresAnAlreadyGoneUnit(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_STOP_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_STOP_STDERR", "Unit canaveral-x.service not loaded.")
	if err := Stop(context.Background(), "canaveral-x"); err != nil {
		t.Errorf("Stop = %v, want nil for an already-gone unit", err)
	}
}

func TestStopFailsOnARealError(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_STOP_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_STOP_STDERR", "Failed to connect to bus")
	err := Stop(context.Background(), "canaveral-x")
	if err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Errorf("err = %v, want it to include the real failure", err)
	}
}

func TestResetNeverFailsRegardlessOfExitCode(t *testing.T) {
	installFakeSystemd(t)
	getLog := fakeLog(t)
	// Reset returns nothing, so there is nothing to assert on beyond "did
	// not panic" and "actually invoked reset-failed" (via the log).
	Reset(context.Background(), "canaveral-x")
	if !strings.Contains(getLog(), "reset-failed canaveral-x.service") {
		t.Error("Reset did not call systemctl reset-failed")
	}
}

func TestStopAllReturnsOnlyTheUnitsThatFailed(t *testing.T) {
	installFakeSystemd(t)
	getLog := fakeLog(t)
	t.Setenv("FAKE_SYSTEMCTL_STOP_FAIL_UNIT", "canaveral-bad.service")
	t.Setenv("FAKE_SYSTEMCTL_STOP_FAIL_STDERR", "Failed to connect to bus")

	failed := StopAll(context.Background(), []string{"canaveral-good", "canaveral-bad"})
	if len(failed) != 1 || failed[0] != "canaveral-bad" {
		t.Errorf("failed = %v, want only canaveral-bad", failed)
	}
	// The good one must have been reset (freeing its name for reuse); the
	// bad one, having failed to stop, must not have been.
	log := getLog()
	if !strings.Contains(log, "reset-failed canaveral-good.service") {
		t.Errorf("log = %q, want canaveral-good reset", log)
	}
	if strings.Contains(log, "reset-failed canaveral-bad.service") {
		t.Errorf("log = %q, want canaveral-bad left alone since it never stopped", log)
	}
}

func TestQueryParsesAnActiveUnit(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_SHOW_OUTPUT",
		"LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=4242\n"+
			"ControlGroup=/user.slice/nonexistent.slice\nMemoryCurrent=1048576\nCPUUsageNSec=2500000000\n")

	st, err := Query(context.Background(), "canaveral-x")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !st.Loaded || st.ActiveState != "active" || st.SubState != "running" {
		t.Errorf("st = %+v", st)
	}
	if st.MainPID != 4242 {
		t.Errorf("MainPID = %d, want 4242", st.MainPID)
	}
	if st.Memory != 1048576 {
		t.Errorf("Memory = %d, want 1048576", st.Memory)
	}
	if st.CPU != 2500*time.Millisecond {
		t.Errorf("CPU = %s, want 2.5s", st.CPU)
	}
}

func TestQueryReturnsErrNotFoundWhenNotLoaded(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_SHOW_OUTPUT", "LoadState=not-found\nActiveState=inactive\nSubState=dead\n")

	_, err := Query(context.Background(), "canaveral-gone")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestQueryErrorsWhenSystemctlItselfFails(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_SHOW_EXIT", "1")

	if _, err := Query(context.Background(), "canaveral-x"); err == nil {
		t.Error("Query should fail when systemctl show exits non-zero")
	}
}

func TestQueryFallsBackToCgroupMemoryWhenSystemdHasNone(t *testing.T) {
	installFakeSystemd(t)
	fakeCgroup := t.TempDir()
	orig := cgroupRoot
	cgroupRoot = fakeCgroup
	t.Cleanup(func() { cgroupRoot = orig })

	cg := "/user.slice/test.slice"
	if err := os.MkdirAll(filepath.Join(fakeCgroup, cg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeCgroup, cg, "memory.current"), []byte("2097152\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeCgroup, cg, "cgroup.procs"), []byte("100\n200\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FAKE_SYSTEMCTL_SHOW_OUTPUT",
		fmt.Sprintf("LoadState=loaded\nActiveState=active\nSubState=running\nControlGroup=%s\nMemoryCurrent=[not set]\n", cg))

	st, err := Query(context.Background(), "canaveral-x")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if st.Memory != 2097152 {
		t.Errorf("Memory = %d, want 2097152 from the cgroup fallback", st.Memory)
	}
	if st.Procs != 2 {
		t.Errorf("Procs = %d, want 2", st.Procs)
	}
}

func TestListParsesUnitNames(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST_UNITS_OUTPUT",
		"canaveral-p-f-svc-web.service loaded active running Web\n"+
			"canaveral-p-f-agent-main.service loaded active running Agent\n")

	got, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"canaveral-p-f-agent-main", "canaveral-p-f-svc-web"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got = %v, want %v (sorted)", got, want)
	}
}

func TestListErrorsWhenSystemctlItselfFails(t *testing.T) {
	installFakeSystemd(t)
	// list-units always exits 0 in the fake; simulate a hard failure the
	// only other way systemctl can fail here: remove it from PATH entirely
	// after installing, so LookPath itself fails.
	t.Setenv("PATH", t.TempDir())
	if _, err := List(context.Background()); err == nil {
		t.Error("List should fail when systemctl cannot be found at all")
	}
}

func TestFeatureUnitsFiltersToOneWorkspace(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_LIST_UNITS_OUTPUT",
		"canaveral-p-f-svc-web.service loaded active running Web\n"+
			"canaveral-p-other-svc-web.service loaded active running Web\n")

	got, err := FeatureUnits(context.Background(), "p-f")
	if err != nil {
		t.Fatalf("FeatureUnits: %v", err)
	}
	if len(got) != 1 || got[0] != "canaveral-p-f-svc-web" {
		t.Errorf("got = %v, want only p-f's own unit", got)
	}
}

func TestAvailableFailsWithoutSystemdRunInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Available(context.Background()); err == nil {
		t.Error("Available should fail when systemd-run is not on PATH")
	}
}

func TestAvailableSucceedsWhenSystemIsRunning(t *testing.T) {
	installFakeSystemd(t)
	if err := Available(context.Background()); err != nil {
		t.Errorf("Available: %v", err)
	}
}

func TestAvailableAcceptsADegradedButReachableManager(t *testing.T) {
	installFakeSystemd(t)
	// is-system-running exits non-zero for "degraded", which is still usable
	// as long as some other query gets a real answer back.
	t.Setenv("FAKE_SYSTEMCTL_IS_SYSTEM_RUNNING_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_SHOW_VERSION", "systemd 255 (255.4-1)")
	if err := Available(context.Background()); err != nil {
		t.Errorf("Available: %v", err)
	}
}

func TestAvailableFailsWhenNothingIsReachable(t *testing.T) {
	installFakeSystemd(t)
	t.Setenv("FAKE_SYSTEMCTL_IS_SYSTEM_RUNNING_EXIT", "1")
	t.Setenv("FAKE_SYSTEMCTL_SHOW_VERSION", "")
	if err := Available(context.Background()); err == nil {
		t.Error("Available should fail when no systemd user manager answers at all")
	}
}
