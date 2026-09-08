package feature

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
)

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~/.config/google-chrome": filepath.Join(home, ".config/google-chrome"),
		"~":                       home,
		"/absolute/path":          "/absolute/path",
		"relative/path":           "relative/path",
	}
	for in, want := range cases {
		got, err := expandHome(in)
		if err != nil {
			t.Errorf("expandHome(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitRatioChainMatchesLiveVerifiedValues(t *testing.T) {
	// These exact numbers (0.8, 0.6667, 1.0) were verified against a real
	// dwindle workspace: applying them via hyprctl dispatch splitratio to 4
	// chained windows produced columns of 39.75%/19.8%/19.8%/19.7% of the
	// monitor width — a 40/20/20/20 split within rounding.
	order := []string{"chrome", "opencode", "terminal", "serverlogs"}
	fractions := map[string]float64{
		"chrome": 0.4, "opencode": 0.2, "terminal": 0.2, "serverlogs": 0.2,
	}
	got := splitRatioChain(order, fractions)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (one per split point, none for the last window)", len(got))
	}
	want := []float64{0.8, 0.6667, 1.0}
	for i, w := range want {
		if diff := got[i] - w; diff > 0.001 || diff < -0.001 {
			t.Errorf("ratio[%d] = %v, want %v", i, got[i], w)
		}
	}
}

func TestSplitRatioChainEvenSplitIsNeutral(t *testing.T) {
	// An even split at any point in the chain must be ratio 1.0 (confirmed
	// empirically to mean "50/50", despite "1.0" not obviously meaning that).
	got := splitRatioChain([]string{"a", "b"}, map[string]float64{"a": 0.5, "b": 0.5})
	if len(got) != 1 || got[0] != 1.0 {
		t.Errorf("got = %v, want [1.0]", got)
	}
}

func TestSplitRatioChainHandlesImperfectSum(t *testing.T) {
	// Validation allows [layout.default] to sum to 1.0 only within a
	// tolerance, so a slightly-off partition must still work. Each step only
	// looks at the remaining sum from that point on, so this must not error
	// or divide by zero, and should still produce a sensible (clamped,
	// finite) ratio.
	order := []string{"chrome", "opencode", "terminal", "serverlogs"}
	fractions := map[string]float64{
		"chrome": 0.34, "opencode": 0.2, "terminal": 0.2, "serverlogs": 0.2, // sums to 0.94
	}
	got := splitRatioChain(order, fractions)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, r := range got {
		if r <= 0 || r >= 2 {
			t.Errorf("ratio[%d] = %v, out of the valid (0,2) dwindle range", i, r)
		}
	}
}

func TestSplitRatioChainClampsExtremeInput(t *testing.T) {
	// A hand-edited or corrupted fraction must not be forwarded to hyprctl
	// as a nonsense ratio.
	got := splitRatioChain([]string{"a", "b"}, map[string]float64{"a": 100, "b": 0.001})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0] > 1.9 {
		t.Errorf("ratio = %v, want clamped to <= 1.9", got[0])
	}
}

func TestSplitRatioChainSingleWindow(t *testing.T) {
	// One window in [layout] means nothing to split at all.
	got := splitRatioChain([]string{"solo"}, map[string]float64{"solo": 1.0})
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}

func TestSplitRatioChainEmptyOrder(t *testing.T) {
	if got := splitRatioChain(nil, nil); got != nil {
		t.Errorf("got = %v, want nil", got)
	}
}

func TestIsLayoutFreshFalseWhenLayoutDisabled(t *testing.T) {
	m := &manifest.Manifest{}
	if isLayoutFresh(m, nil) {
		t.Error("a manifest with no [layout] must never be considered fresh")
	}
}

func TestIsLayoutFreshTrueWhenEveryWindowIsBeingSpawned(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	pending := map[string]pendingSpawn{"chrome": {}, "terminal": {}}
	if !isLayoutFresh(m, pending) {
		t.Error("layout should be fresh when all of its windows are being spawned")
	}
}

func TestIsLayoutFreshFalseWhenOneWindowIsAlreadyUp(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	// chrome is already open, so only terminal is pending.
	pending := map[string]pendingSpawn{"terminal": {}}
	if isLayoutFresh(m, pending) {
		t.Error("a partially-open layout must not be treated as fresh")
	}
}

func TestBuildWindowSpecForAnAlreadyOpenWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "chrome", Exec: "chromium --class={{.Class}}"}
	class := hypr.Class("p/f", "chrome")
	open := map[string]hypr.Client{class: {}}

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, nil, open, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending != nil {
		t.Error("an already-open window must not produce a pending spawn")
	}
	if rec.Name != "chrome" || rec.Class != class || rec.Dir != f.Worktree {
		t.Errorf("rec = %+v", rec)
	}
}

func TestBuildWindowSpecForAMissingWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p", Terminal: "alacritty"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "chrome", Exec: "chromium --class={{.Class}}"}

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, nil, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending == nil {
		t.Fatal("a window that is not open must produce a pending spawn")
	}
	class := hypr.Class("p/f", "chrome")
	if pending.spec.Class != class || pending.spec.Cmd != "chromium --class="+class {
		t.Errorf("spec = %+v", pending.spec)
	}
	if pending.spec.IsTerminal {
		t.Error("an exec window must not be wrapped in a terminal")
	}
	if rec.Class != class {
		t.Errorf("rec.Class = %q, want %q", rec.Class, class)
	}
}

// An application that cannot be told to take our class is adopted by
// match_class instead. Without this every open would spawn another copy,
// which is what a slicer or a browser being reopened on every reset looks
// like to the user.
func TestBuildWindowSpecAdoptsAMatchClassWindowOnTheFeatureWorkspace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Space: true, Name: "3d", Worktree: "/home/u"}
	w := manifest.Window{Name: "bambustudio", Exec: "BambuStudio.AppImage", MatchClass: "^BambuStudio$"}

	c := hypr.Client{Address: "0xbambu", Class: "BambuStudio"}
	c.Workspace.Name = f.HyprWorkspace()

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w,
		tmpl.Vars{}, nil, []hypr.Client{c}, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending != nil {
		t.Error("a matching window on the feature's workspace is already open; it must not be spawned again")
	}
	if rec.MatchClass != w.MatchClass {
		t.Errorf("rec.MatchClass = %q, want %q — close has no other way to find the window", rec.MatchClass, w.MatchClass)
	}
}

// The same class on someone else's workspace is one of the user's own
// windows. Adopting it would mean never launching the application and then
// dragging their window onto the feature's workspace instead.
func TestBuildWindowSpecDoesNotAdoptAMatchClassWindowElsewhere(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Space: true, Name: "3d", Worktree: "/home/u"}
	w := manifest.Window{Name: "bambustudio", Exec: "BambuStudio.AppImage", MatchClass: "^BambuStudio$"}

	c := hypr.Client{Address: "0xtheirs", Class: "BambuStudio"}
	c.Workspace.Name = "2"

	_, pending, err := buildWindowSpec(context.Background(), m, f, w,
		tmpl.Vars{}, nil, []hypr.Client{c}, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending == nil {
		t.Fatal("a window of that class on another workspace is the user's own; ours must still be spawned")
	}
	if pending.match == nil || !pending.match.MatchString("BambuStudio") {
		t.Error("the pending spawn must carry the compiled pattern, or the window it creates cannot be placed")
	}
	if pending.spec.IsTerminal {
		t.Error("an exec window must not be wrapped in a terminal — that is the whole bug")
	}
}

func TestBuildWindowSpecUsesADeclaredSubdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "api", Exec: "app --class={{.Class}}", Dir: "api"}

	rec, _, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, nil, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if rec.Dir != filepath.Join(f.Worktree, "api") {
		t.Errorf("rec.Dir = %q, want %q", rec.Dir, filepath.Join(f.Worktree, "api"))
	}
}

// A window's own PID is the ground truth for what it is running; a test can
// stand in for one by pointing at a process it knows the command line of.
func clientRunning(t *testing.T, cmdline string) hypr.Client {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 600")
	// sh ignores arguments after the script, so this one exists purely to
	// put the simulated command into the process's own /proc entry.
	c.Args = append(c.Args, cmdline)
	if err := c.Start(); err != nil {
		t.Fatalf("start stand-in process: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_, _ = c.Process.Wait()
	})
	// Start returns as soon as execve is under way, and /proc reports an
	// empty command line until the kernel has finished mapping the new
	// argument vector in.
	for range 200 {
		if _, ok := hypr.Cmdline(c.Process.Pid); ok {
			return hypr.Client{PID: c.Process.Pid}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stand-in process %d never reported a command line", c.Process.Pid)
	return hypr.Client{}
}

func agents(url string) []state.Agent {
	return []state.Agent{{Name: "main", Tool: "opencode", URL: url}}
}

// An agent restarts onto a new port (agent.ServeCmd uses --port 0), so the
// window still attached to the old one is talking to a closed socket. Before
// this was detected the window was adopted on its class alone and left in
// place, failing on the first keystroke with "Creating a session failed".
func TestIsStaleWhenTheAgentURLMoved(t *testing.T) {
	c := clientRunning(t, "opencode attach http://127.0.0.1:38259 --dir /wt")
	cmd := "opencode attach http://127.0.0.1:38231 --dir /wt"
	if !isStale(c, cmd, agents("http://127.0.0.1:38231")) {
		t.Error("a window attached to the previous agent URL must be stale")
	}
}

func TestIsStaleFalseWhenTheAgentDidNotMove(t *testing.T) {
	cmd := "opencode attach http://127.0.0.1:38231 --dir /wt"
	c := clientRunning(t, cmd)
	if isStale(c, cmd, agents("http://127.0.0.1:38231")) {
		t.Error("an unchanged agent URL must leave its window alone")
	}
}

// Fork arguments are only ever added when a feature is created, so a window
// old enough to be adopted cannot have them in the command rendered for it
// now. Comparing whole command lines would read that as drift and close a
// perfectly good window; comparing only the URL does not.
func TestIsStaleIgnoresAForkArgumentTheWindowWasSpawnedWith(t *testing.T) {
	url := "http://127.0.0.1:38231"
	c := clientRunning(t, "opencode attach "+url+" --dir /wt --session ses_abc")
	if isStale(c, "opencode attach "+url+" --dir /wt", agents(url)) {
		t.Error("a window differing only by its fork argument must be left alone")
	}
}

// Only the agent moved, so replacing chrome and the plain terminal alongside
// it would close windows the user is using for no reason at all.
func TestIsStaleFalseForAWindowThatIgnoresAgents(t *testing.T) {
	c := clientRunning(t, "chromium --class=canaveral-p-f-chrome")
	cmd := "chromium --class=canaveral-p-f-chrome"
	if isStale(c, cmd, agents("http://127.0.0.1:38231")) {
		t.Error("a window that never mentions an agent must survive an agent restart")
	}
}

// A window whose process cannot be read tells us nothing, and guessing wrong
// here costs the user a window they were using.
func TestIsStaleFalseWhenThePIDIsUnknown(t *testing.T) {
	cmd := "opencode attach http://127.0.0.1:38231 --dir /wt"
	if isStale(hypr.Client{}, cmd, agents("http://127.0.0.1:38231")) {
		t.Error("an unreadable window must not be closed on a guess")
	}
}

func TestIsStaleFalseWhenTheAgentHasNoURL(t *testing.T) {
	c := clientRunning(t, "opencode attach http://127.0.0.1:38259 --dir /wt")
	if isStale(c, "opencode attach  --dir /wt", []state.Agent{{Name: "main"}}) {
		t.Error("an agent with no URL gives nothing to compare and must not close a window")
	}
}

// installSpawnRecordingHyprctl puts a fake `hyprctl` on PATH that appends the
// feature's state record to a log every time a window is dispatched, so a
// test can ask what the progress bar was saying at the moment each window was
// actually created — which is the thing the counting got wrong.
func installSpawnRecordingHyprctl(t *testing.T, log string) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  clients) printf '%s' "${CANAVERAL_TEST_CLIENTS:-[]}" ;;
  dispatch) cat "$CANAVERAL_TEST_RECORD" >> "$CANAVERAL_TEST_LOG"; printf 'ok' ;;
  version|keyword) : ;;
  *) exit 1 ;;
esac
`
	path := filepath.Join(dir, "hyprctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("CANAVERAL_TEST_LOG", log)
	// Spawn writes the window's environment to a file under $XDG_RUNTIME_DIR
	// and relies on the shell it starts to delete it. No shell ever starts
	// here, so without this the suite drops a file holding a whole window's
	// environment into the developer's own runtime directory on every run.
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// The step announced for a window has to be the spawn, not the template
// render that precedes it.
//
// Counting the spec-building loop reported every window finished before the
// first one existed, so a two-window feature published "window terminal, 1 of
// 2" and then went quiet for the whole of the part that actually takes time
// and can fail. When a boot died anywhere after that — which is exactly what
// happened — the row it left behind named the last window and a count one
// short of full, describing a feature stuck on its terminal when in fact
// every window was up.
func TestReconcileWindowsCountsTheSpawnNotTheSpecBuild(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	record := filepath.Join(stateHome, "canaveral", "features", "p", "f.json")
	t.Setenv("CANAVERAL_TEST_RECORD", record)
	log := filepath.Join(t.TempDir(), "spawns.log")
	installSpawnRecordingHyprctl(t, log)

	// No [layout]: the free path spawns without waiting on the compositor to
	// map anything, which is all this test needs to observe.
	run := "true"
	m := &manifest.Manifest{Root: "/p", Terminal: "alacritty", Windows: []manifest.Window{
		{Name: "opencode", Run: &run},
		{Name: "terminal", Run: &run},
	}}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 2)

	err := reconcileWindows(context.Background(), m, f, tmpl.Vars{}, nil,
		&Result{}, quietReporter{}, "", prog)
	if err != nil {
		t.Fatalf("reconcileWindows: %v", err)
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read spawn log: %v", err)
	}
	var snapshots []state.Feature
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var snap state.Feature
		if err := dec.Decode(&snap); err != nil {
			break
		}
		snapshots = append(snapshots, snap)
	}
	if len(snapshots) != 2 {
		t.Fatalf("recorded %d spawns, want 2", len(snapshots))
	}
	for i, want := range []struct {
		label string
		step  int
	}{{"window opencode", 0}, {"window terminal", 1}} {
		if snapshots[i].PhaseLabel != want.label || snapshots[i].PhaseStep != want.step {
			t.Errorf("at spawn %d the record said %q %d/%d, want %q %d/2",
				i, snapshots[i].PhaseLabel, snapshots[i].PhaseStep,
				snapshots[i].PhaseTotal, want.label, want.step)
		}
	}
	if prog.step != 2 {
		t.Errorf("step = %d after both windows, want 2 (a boot that finished must read as finished)", prog.step)
	}
}

// A window that is already open is never spawned, so its step has to be
// counted where it is decided instead — otherwise the bar stops one short for
// every window a reconcile finds already up, which is the common case for
// `reset` on a live feature.
func TestReconcileWindowsCountsAWindowThatNeedsNoSpawn(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("CANAVERAL_TEST_RECORD", filepath.Join(stateHome, "canaveral", "features", "p", "f.json"))
	log := filepath.Join(t.TempDir(), "spawns.log")
	installSpawnRecordingHyprctl(t, log)

	run := "true"
	m := &manifest.Manifest{Root: "/p", Terminal: "alacritty", Windows: []manifest.Window{
		{Name: "terminal", Run: &run},
	}}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 1)

	// hyprctl reports the window as already open, so buildWindowSpec adopts it.
	class := hypr.Class(f.Key(), "terminal")
	clients, err := json.Marshal([]hypr.Client{{Address: "0x1", Class: class, InitialClass: class}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANAVERAL_TEST_CLIENTS", string(clients))

	if err := reconcileWindows(context.Background(), m, f, tmpl.Vars{}, nil,
		&Result{}, quietReporter{}, "", prog); err != nil {
		t.Fatalf("reconcileWindows: %v", err)
	}

	if _, err := os.Stat(log); err == nil {
		t.Error("an already-open window must not be respawned")
	}
	if prog.step != 1 {
		t.Errorf("step = %d, want 1: an adopted window is still a step done", prog.step)
	}
}
