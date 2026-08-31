package feature

import (
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// progress publishes a feature's lifecycle progress into its state record.
//
// Deliberately separate from Reporter. Reporter writes for a person reading a
// terminal, and its presentation is the CLI's business; this writes for another
// *process* — `canaveral watch`, and through it a status bar — which needs a
// step count and a stable label rather than prose. The two have different
// audiences and different lifetimes, so they stay different things.
//
// Every write is best-effort. Failing a feature's creation because its progress
// could not be recorded would trade a cosmetic problem for a real one.
type progress struct {
	f     *state.Feature
	phase string
	step  int
	total int
}

// reconcileSteps counts the work a reconcile pass will do, so the total is
// known before the first step rather than growing as it goes.
//
// One for the worktree, one for the precheck when the manifest declares one,
// and one for each service, agent and window that is not being skipped. The
// steps are wildly unequal in duration — a readiness probe may take two
// minutes where a window spawn takes milliseconds — so this measures work
// remaining, never time remaining.
func reconcileSteps(m *manifest.Manifest, opt Options) int {
	n := 1
	if m.Precheck != "" {
		n++
	}
	if !opt.NoServices {
		n += len(m.Services)
	}
	if !opt.NoAgents {
		n += len(m.Agents)
	}
	if !opt.NoWindows {
		n += len(m.Windows)
	}
	return n
}

func newProgress(f *state.Feature, phase string, total int) *progress {
	p := &progress{f: f, phase: phase, total: total}
	return p
}

// start announces a step that is about to begin. The count is of steps
// completed, so the first `start` reports 0 of N and the bar begins empty.
func (p *progress) start(label string) {
	if p == nil {
		return
	}
	_ = p.f.SetPhase(p.phase, label, p.step, p.total)
}

// done marks the step just announced as finished.
func (p *progress) done() {
	if p == nil {
		return
	}
	if p.step < p.total {
		p.step++
	}
}

// finish clears the phase, settling the feature.
//
// Called on the way out whether or not the pass succeeded: a failed run has
// stopped making progress either way, and leaving the phase set would show a
// frozen progress bar until the staleness bound expired.
func (p *progress) finish() {
	if p == nil {
		return
	}
	_ = p.f.ClearPhase()
}
