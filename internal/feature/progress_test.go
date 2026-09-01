package feature

import (
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

func TestReconcileSteps(t *testing.T) {
	cases := []struct {
		name string
		m    manifest.Manifest
		opt  Options
		want int
	}{
		{"nothing declared", manifest.Manifest{}, Options{}, 1},
		{"precheck adds one", manifest.Manifest{Precheck: "true"}, Options{}, 2},
		{
			"one of each",
			manifest.Manifest{
				Services: []manifest.Service{{Name: "web"}},
				Agents:   []manifest.Agent{{Name: "main"}},
				Windows:  []manifest.Window{{Name: "term"}},
			},
			Options{}, 4,
		},
		{
			"skip flags drop their own count, not the others",
			manifest.Manifest{
				Services: []manifest.Service{{Name: "web"}, {Name: "jobs"}},
				Agents:   []manifest.Agent{{Name: "main"}},
				Windows:  []manifest.Window{{Name: "term"}},
			},
			Options{NoServices: true}, 3,
		},
		{
			"skipping everything still counts the worktree",
			manifest.Manifest{
				Precheck: "true",
				Services: []manifest.Service{{Name: "web"}},
				Agents:   []manifest.Agent{{Name: "main"}},
				Windows:  []manifest.Window{{Name: "term"}},
			},
			Options{NoServices: true, NoAgents: true, NoWindows: true}, 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := reconcileSteps(&c.m, c.opt); got != c.want {
				t.Errorf("reconcileSteps = %d, want %d", got, c.want)
			}
		})
	}
}

func TestProgressMethodsAreNilSafe(t *testing.T) {
	// Reconcile passes prog through many layers of helpers; a nil *progress
	// (never constructed, e.g. in a test calling one of those helpers
	// directly) must not panic on any of these.
	var p *progress
	p.start("whatever")
	p.done()
	p.finish()
}

func TestProgressStartPublishesTheStepIntoState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, state.PhaseBooting, 3)

	prog.start("worktree")

	if f.Phase != state.PhaseBooting || f.PhaseLabel != "worktree" {
		t.Errorf("phase = %q/%q, want %q/worktree", f.Phase, f.PhaseLabel, state.PhaseBooting)
	}
	if f.PhaseStep != 0 || f.PhaseTotal != 3 {
		t.Errorf("step/total = %d/%d, want 0/3 (nothing done yet)", f.PhaseStep, f.PhaseTotal)
	}
}

func TestProgressDoneAdvancesTheStepForTheNextStart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, state.PhaseBooting, 2)

	prog.start("worktree")
	prog.done()
	prog.start("agents")

	if f.PhaseStep != 1 {
		t.Errorf("PhaseStep = %d, want 1 after one done()", f.PhaseStep)
	}
}

func TestProgressDoneNeverExceedsTotal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	prog := newProgress(&state.Feature{Project: "p", Name: "f"}, state.PhaseBooting, 1)

	prog.done()
	prog.done()
	prog.done()

	prog.start("last")
	// This test only cares that done() saturates rather than overshooting;
	// asserting on prog's private step field from within the package.
	if prog.step != prog.total {
		t.Errorf("step = %d, want it capped at total (%d)", prog.step, prog.total)
	}
}

func TestProgressFinishClearsThePhase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &state.Feature{Project: "p", Name: "f"}
	prog := newProgress(f, state.PhaseBooting, 1)
	prog.start("worktree")

	prog.finish()

	if f.Phase != "" || f.PhaseLabel != "" {
		t.Errorf("phase = %q/%q, want cleared", f.Phase, f.PhaseLabel)
	}
}
