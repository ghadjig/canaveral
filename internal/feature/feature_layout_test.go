package feature

import (
	"context"
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
	// [layout.current] reflects a live, hand-resized window and rarely sums
	// to exactly 1.0 (only one column was resized; the others kept their old
	// size). Each step only looks at the remaining sum from that point on,
	// so this must not error or divide by zero, and should still produce a
	// sensible (clamped, finite) ratio.
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
	// A hand-edited or corrupted current value must not be forwarded to
	// hyprctl as a nonsense ratio.
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
	class := hypr.Class("p", "f", "chrome")
	open := map[string]hypr.Client{class: {}}

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, open, quietReporter{})
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

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending == nil {
		t.Fatal("a window that is not open must produce a pending spawn")
	}
	class := hypr.Class("p", "f", "chrome")
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

func TestBuildWindowSpecUsesADeclaredSubdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "api", Exec: "app --class={{.Class}}", Dir: "api"}

	rec, _, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, map[string]hypr.Client{}, quietReporter{})
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
