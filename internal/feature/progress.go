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
// Every write is best-effort: failing a feature's creation because its progress
// could not be recorded would trade a cosmetic problem for a real one. It is
// not silent, though. A record that cannot be written is a record that will sit
// on whatever it last said, so the one Reporter line this type does emit is
// about that.
type progress struct {
	f     *state.Feature
	r     Reporter
	phase string
	label string
	step  int
	total int
	// warned suppresses repeat warnings. A state directory that cannot be
	// written fails identically at every step, and one line per step would
	// bury the reporter's real output under a stack of the same sentence.
	warned bool
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
	if m.Space {
		// No worktree step to count: a space has none, and counting one it
		// never performs would leave the bar a step short of full forever.
		n = 0
	}
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

func newProgress(f *state.Feature, r Reporter, phase string, total int) *progress {
	return &progress{f: f, r: r, phase: phase, total: total}
}

// start announces a step that is about to begin. The count is of steps
// completed, so the first `start` reports 0 of N and the bar begins empty.
func (p *progress) start(label string) {
	if p == nil {
		return
	}
	p.label = label
	p.publish()
}

// done marks the step just announced as finished, and publishes the new count.
//
// Published, not merely counted. The last thing written is what a reader is
// left looking at if this process dies before finish, and the label belongs to
// the step that has just *ended*: leaving the count a step short would make a
// completed run read as one stuck on its final step, which is the more
// alarming of the two lies and the one that actually got reported as a bug.
func (p *progress) done() {
	if p == nil {
		return
	}
	if p.step < p.total {
		p.step++
	}
	p.publish()
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
	if err := p.f.ClearPhase(); err != nil {
		p.warn("could not settle progress: %v", err)
	}
}

func (p *progress) publish() {
	if err := p.f.SetPhase(p.phase, p.label, p.step, p.total); err != nil {
		p.warn("could not record progress: %v", err)
	}
}

func (p *progress) warn(format string, a ...any) {
	if p.r == nil || p.warned {
		return
	}
	p.warned = true
	p.r.Warn(format, a...)
}
