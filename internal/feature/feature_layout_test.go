package feature

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	f := &state.Feature{Project: "p", Name: "f"}
	if isLayoutFresh(m, f, nil) {
		t.Error("a manifest with no [layout] must never be considered fresh")
	}
}

func TestIsLayoutFreshTrueWhenNothingIsOpenYet(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	f := &state.Feature{Project: "p", Name: "f"}
	if !isLayoutFresh(m, f, map[string]hypr.Client{}) {
		t.Error("layout should be fresh when none of its windows are open")
	}
}

func TestIsLayoutFreshFalseWhenOneWindowAlreadyOpen(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	f := &state.Feature{Project: "p", Name: "f"}
	open := map[string]hypr.Client{
		hypr.Class("p", "f", "chrome"): {},
	}
	if isLayoutFresh(m, f, open) {
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
