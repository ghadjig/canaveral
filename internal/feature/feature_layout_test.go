package feature

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/wm"
)

// useHyprland pins which compositor internal/wm drives, and makes it look
// like one that is running.
//
// Both halves are needed. Without the override these tests read the
// environment of the session they are running in, which is a real compositor,
// so the fake hyprctl on PATH would be ignored on a sway machine. Without the
// instance signature wm.Available refuses, the window layer is skipped with a
// warning, and the assertions below quietly have nothing to assert — which is
// how these tests came to depend on the developer's own desktop being
// Hyprland. See AGENTS.md.
func useHyprland(t *testing.T) {
	t.Helper()
	t.Setenv(wm.OverrideEnv, "hyprland")
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "test")
}

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
	class := wm.Class("p/f", "chrome")
	open := map[string]wm.Client{class: {}}

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

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, nil, map[string]wm.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending == nil {
		t.Fatal("a window that is not open must produce a pending spawn")
	}
	class := wm.Class("p/f", "chrome")
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

	c := wm.Client{Address: "0xbambu", Class: "BambuStudio"}
	c.Workspace.Name = f.WorkspaceName()

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w,
		tmpl.Vars{}, nil, []wm.Client{c}, map[string]wm.Client{}, quietReporter{})
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

	c := wm.Client{Address: "0xtheirs", Class: "BambuStudio"}
	c.Workspace.Name = "2"

	_, pending, err := buildWindowSpec(context.Background(), m, f, w,
		tmpl.Vars{}, nil, []wm.Client{c}, map[string]wm.Client{}, quietReporter{})
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

	rec, _, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, nil, map[string]wm.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if rec.Dir != filepath.Join(f.Worktree, "api") {
		t.Errorf("rec.Dir = %q, want %q", rec.Dir, filepath.Join(f.Worktree, "api"))
	}
}

// A window's own PID is the ground truth for what it is running; a test can
// stand in for one by pointing at a process it knows the command line of.
func clientRunning(t *testing.T, cmdline string) wm.Client {
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
		if _, ok := wm.Cmdline(c.Process.Pid); ok {
			return wm.Client{PID: c.Process.Pid}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("stand-in process %d never reported a command line", c.Process.Pid)
	return wm.Client{}
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
	if isStale(wm.Client{}, cmd, agents("http://127.0.0.1:38231")) {
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
	useHyprland(t)
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
		&Result{}, quietReporter{}, prog)
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
	class := wm.Class(f.Key(), "terminal")
	clients, err := json.Marshal([]wm.Client{{Address: "0x1", Class: class, InitialClass: class}})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANAVERAL_TEST_CLIENTS", string(clients))

	if err := reconcileWindows(context.Background(), m, f, tmpl.Vars{}, nil,
		&Result{}, quietReporter{}, prog); err != nil {
		t.Fatalf("reconcileWindows: %v", err)
	}

	if _, err := os.Stat(log); err == nil {
		t.Error("an already-open window must not be respawned")
	}
	if prog.step != 1 {
		t.Errorf("step = %d, want 1: an adopted window is still a step done", prog.step)
	}
}

// A compositor error after mapping windows used to discard their declarations:
// the windows stayed on screen but watch called the feature a headless worker.
// Exercise the real reconciliation path and its persisted state, rather than
// only checking the temporary record in memory.
func TestReconcileWindowsRetainsDeclarationsAfterLayoutFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "test")
	t.Setenv("LAYOUT_SPAWNED", filepath.Join(dir, "spawned"))
	script := `#!/bin/sh
case "$1" in
  version|keyword) printf ok ;;
  clients)
    if [ -e "$LAYOUT_SPAWNED" ]; then
      printf '%s' '[{"address":"0xa","initialClass":"canaveral-p-f-a","workspace":{"name":"p:f"}},{"address":"0xb","initialClass":"canaveral-p-f-b","workspace":{"name":"p:f"}}]'
    else
      printf '[]'
    fi ;;
  dispatch) touch "$LAYOUT_SPAWNED"; printf ok ;;
  getoption) printf '%s' '{"int":0}' ;;
  monitors) printf '%s' '[{"name":"DP-1","focused":true,"activeWorkspace":{"name":"current"}}]' ;;
  workspaces) printf '%s' '[{"name":"p:f","monitor":"DP-1"}]' ;;
  activewindow) printf '%s' '{"address":"0xff"}' ;;
  --batch) printf 'ok\nInvalid dispatcher\nok\n' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	useHyprland(t)
	run := "true"
	m := &manifest.Manifest{Root: "/p", Terminal: "alacritty", Windows: []manifest.Window{
		{Name: "a", Run: &run}, {Name: "b", Run: &run},
	}}
	m.Layout.Order = []string{"a", "b"}
	m.Layout.Default = map[string]float64{"a": 0.6, "b": 0.4}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	prog := newProgress(f, quietReporter{}, state.PhaseBooting, 2)
	res := &Result{Feature: f}
	err := reconcileWindows(context.Background(), m, f, tmpl.Vars{}, nil, res, quietReporter{}, prog)
	if err == nil || !strings.Contains(err.Error(), "Invalid dispatcher") {
		t.Fatalf("layout failure = %v", err)
	}
	if len(res.SpawnedWindow) != 2 {
		t.Fatalf("spawned windows = %v, want both before layout fails", res.SpawnedWindow)
	}
	// Reconcile's deferred finish must not erase declarations either.
	prog.finish()
	saved, err := state.Load("p", "f")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Headless() || len(saved.Windows) != 2 || saved.Phase != "" {
		t.Fatalf("failed layout lost window identity or did not settle: %+v", saved)
	}
	for i, name := range []string{"a", "b"} {
		if w := saved.Windows[i]; w.Name != name || w.Class != wm.Class(f.Key(), name) || w.Workspace != "p:f" {
			t.Errorf("window %d declaration = %+v", i, w)
		}
	}
	all, err := state.EnsureWSlots()
	if err != nil || len(all) != 1 || all[0].WSlot != 1 {
		t.Fatalf("feature with failed layout needs a workspace slot: %v, %v", all, err)
	}
}

func TestLayoutWaitsForWindowsBeforeTouchingFocus(t *testing.T) {
	for _, failSpawn := range []bool{false, true} {
		t.Run(fmt.Sprint(failSpawn), func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$LAYOUT_LOG"
case "$1" in
  dispatch) if [ "$LAYOUT_FAIL" = true ]; then exit 1; fi; printf ok ;;
  clients) printf '%s' '[{"address":"0xa","initialClass":"a","workspace":{"name":"p:f"}},{"address":"0xb","initialClass":"b","workspace":{"name":"p:f"}}]' ;;
  getoption) printf '%s' '{"int":0}' ;;
  monitors) printf '%s' '[{"name":"DP-1","focused":true,"activeWorkspace":{"name":"latest"}}]' ;;
  workspaces) printf '%s' '[{"name":"p:f","monitor":"DP-1"}]' ;;
  activewindow) printf '%s' '{"address":"0xff"}' ;;
  --batch) printf ok ;;
  keyword) printf ok ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			useHyprland(t)
			t.Setenv("LAYOUT_LOG", log)
			t.Setenv("LAYOUT_FAIL", fmt.Sprint(failSpawn))
			t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
			m := &manifest.Manifest{}
			m.Layout.Order = []string{"a", "b"}
			m.Layout.Default = map[string]float64{"a": 0.6, "b": 0.4}
			pending := map[string]pendingSpawn{
				"a": {name: "a", spec: wm.SpawnSpec{Class: "a", Workspace: "p:f", Cmd: "true"}},
				"b": {name: "b", spec: wm.SpawnSpec{Class: "b", Workspace: "p:f", Cmd: "true"}},
			}
			err := reconcileLayoutWindows(context.Background(), m, pending, true, &Result{}, quietReporter{}, nil)
			if (err != nil) != failSpawn {
				t.Fatalf("layout error = %v", err)
			}
			b, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			calls := string(b)
			if strings.Contains(calls, "\ndispatch focus") || strings.Contains(calls, "moveworkspacetomonitor") {
				t.Fatalf("layout changed focus outside the batch: %s", calls)
			}
			if failSpawn {
				if strings.Contains(calls, "--batch") {
					t.Fatalf("failed spawn must not touch focus: %s", calls)
				}
				return
			}
			batch := strings.Index(calls, "--batch")
			if batch < 0 || strings.LastIndex(calls, "dispatch exec") > batch || strings.Index(calls, "activewindow") < strings.LastIndex(calls, "dispatch exec") {
				t.Fatalf("focus must be captured and batched after all spawns: %s", calls)
			}
			if !strings.Contains(calls, "dispatch workspace name:latest") {
				t.Fatalf("must restore the latest workspace: %s", calls)
			}
		})
	}
}

func TestNewClassedWindowIsPlacedBeforeLayout(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$PLACEMENT_LOG"
case "$1" in
  clients)
    if [ -f "$PLACEMENT_MOVED" ]; then ws='p:f'; else ws='active'; fi
    printf '[{"address":"0xab","initialClass":"canaveral-browser","workspace":{"name":"%s"}}]' "$ws"
    ;;
  dispatch) touch "$PLACEMENT_MOVED"; printf ok ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "hyprctl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	useHyprland(t)
	t.Setenv("PLACEMENT_LOG", log)
	t.Setenv("PLACEMENT_MOVED", filepath.Join(dir, "moved"))
	addr, err := waitForClassInWorkspace(context.Background(), "canaveral-browser", "p:f", time.Second)
	if err != nil || addr != "0xab" {
		t.Fatalf("placement = %q, %v", addr, err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dispatch movetoworkspacesilent name:p:f,address:0xab") || strings.Count(string(b), "clients") < 2 {
		t.Fatalf("must silently move and recheck before layout: %s", b)
	}
}
