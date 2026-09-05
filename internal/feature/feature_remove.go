package feature

// Remove and its phases: recording a namespace's newest session, stopping
// units, removing the worktree and branch, and closing windows.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
	"github.com/bandito/canaveral/internal/worktree"
)

// UnitsFor returns every systemd unit belonging to a feature: the ones its
// state records, plus any the manager still holds that state does not mention.
// Agents come first, then services in reverse start order, then strays.
func UnitsFor(ctx context.Context, f *state.Feature) []string {
	var names []string
	seen := map[string]bool{}
	add := func(n string) {
		if n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for _, a := range f.Agents {
		add(a.Unit)
	}
	// Reverse order: dependents go down before what they depend on.
	for i := len(f.Services) - 1; i >= 0; i-- {
		add(f.Services[i].Unit)
	}
	// Ask systemd as well. A reconcile interrupted between starting a unit
	// and saving the record leaves a unit that state has never heard of, and
	// state is about to be deleted — after which the only way back to it is
	// its name. This is what used to leave a live server holding a feature's
	// port long after the feature was gone, answering the *next* feature's
	// readiness probe from a deleted worktree.
	if live, err := unit.FeatureUnits(ctx, f.Project+"-"+f.Name); err == nil {
		for _, n := range live {
			add(n)
		}
	}
	return names
}

// stopFeatureUnits stops everything belonging to a feature and reports what
// happened, returning the units that would not stop.
// stopFeatureUnits stops everything belonging to a feature and reports what
// happened, returning the units that would not stop and, separately, one unit
// deliberately left running: the one this very process is executing under,
// if any.
//
// `canaveral rm`/`merge` invoked as a shell tool call by the feature's own
// agent is a descendant of that agent's unit, and every unit here is started
// with KillMode=mixed — SIGTERM to the main process, but SIGKILL to the whole
// cgroup once TimeoutStopSec elapses. Stopping that unit now would risk this
// function being killed mid-teardown, no matter which step it had reached.
// The caller stops the deferred unit at the very end instead, once the
// worktree, branch and state file are already gone — see Remove.
func stopFeatureUnits(ctx context.Context, f *state.Feature, r Reporter) (deferred string, failed []string) {
	names := UnitsFor(ctx, f)
	self := unit.Self()
	var now []string
	for _, n := range names {
		if n == self {
			deferred = n
			continue
		}
		now = append(now, n)
	}
	if len(now) == 0 {
		return deferred, nil
	}
	failed = unit.StopAll(ctx, now)
	// Count what actually stopped, not what was attempted: a teardown that
	// silently stopped nothing used to report success just the same.
	r.OK("stopped %d unit(s)", len(now)-len(failed))
	return deferred, failed
}

// mergeTarget reports whether a feature's branch has landed in the repo's
// default branch, and what that branch is called.
//
// ok is false when the question cannot be answered — no discoverable default
// branch, a branch git no longer knows about. Callers treat that as "do not
// block", because refusing to tear a feature down just because the repo has
// no `main` would be a worse failure than the one being guarded against.
func mergeTarget(ctx context.Context, f *state.Feature) (merged bool, target string, ok bool) {
	if f.Root == "" || f.Branch == "" {
		return false, "", false
	}
	def, err := worktree.DefaultBranch(ctx, f.Root)
	if err != nil {
		return false, "", false
	}
	if def == f.Branch {
		// The feature is sitting on the default branch itself; there is
		// nothing to merge it into.
		return true, def, true
	}
	m, err := worktree.IsMerged(ctx, f.Root, f.Branch, def)
	if err != nil {
		return false, def, false
	}
	return m, def, true
}

// ErrUnmerged reports a teardown refused because the work has not landed.
// Match it with errors.Is; the message itself comes from unmergedError.
var ErrUnmerged = errors.New("not merged into the default branch")

// unmergedError explains a refusal in the terms the user needs to act on:
// which branch, which target, and the two ways forward.
type unmergedError struct {
	feature, branch, target string
	// stashed changes the first way forward. `merge` only ever operates on
	// an active feature, so telling someone to run it against a parked one
	// would send them to a second, less helpful error.
	stashed bool
}

func (e *unmergedError) Is(target error) bool { return target == ErrUnmerged }

func (e *unmergedError) Error() string {
	land := fmt.Sprintf("land it with `canaveral merge %s`", e.feature)
	if e.stashed {
		land = fmt.Sprintf("restore and land it with `canaveral pop %s`, then `canaveral merge %s`",
			e.feature, e.feature)
	}
	return fmt.Sprintf(
		"%s is not merged into %s\n  %s\n  or discard the workspace with `canaveral rm %s --force` (the branch is kept)",
		e.branch, e.target, land, e.feature)
}

// Remove tears a feature down: units, windows, worktree and state.
//
// Refuses outright when the feature's branch has not been merged into the
// repo's default branch, unless force is set or the worktree is being kept.
// Committed work survives on the branch either way — Remove has never deleted
// an unmerged branch — but tearing down the workspace, ports and agent of
// something you have not landed yet is nearly always a mistake, and the branch
// left behind is easy to lose track of.
//
// Once the worktree is gone, the branch is deleted too if (and only if) it
// has been fully merged into the repo's default branch — merge history is
// what makes it safe, not the caller's say-so, so unmerged work is always
// kept regardless of keepBranch. keepBranch exists purely to opt out of
// deletion even when merged, e.g. to keep it around for a while longer.
func Remove(ctx context.Context, f *state.Feature, keepWorktree, force, keepBranch bool, r Reporter) error {
	// Checked before anything else: every step below this point is
	// destructive, and stopping a feature's services only to then refuse to
	// remove it would leave it half torn down.
	if !force && !keepWorktree && f.Worktree != "" {
		if merged, target, ok := mergeTarget(ctx, f); ok && !merged {
			return &unmergedError{feature: f.Name, branch: f.Branch, target: target}
		}
	}

	// Teardown publishes progress the same way creation does, and for the same
	// reason: stopping units and removing a worktree takes long enough that a
	// status bar should say so. Four steps — sessions, units, worktree, state —
	// counted rather than named individually, since unlike creation the work is
	// fixed and does not vary with the manifest.
	prog := newProgress(f, state.PhaseRemoving, 4)
	// No deferred finish: the state file is deleted below, so there is nothing
	// left to clear on the way out. A failure before that point does leave the
	// phase set, which the staleness bound in state.InPhase covers.
	prog.start("saving session")
	recordNamespaceSession(ctx, f)

	prog.done()
	prog.start("stopping services")
	deferredUnit, failed := stopFeatureUnits(ctx, f, r)
	if len(failed) > 0 {
		// Deliberately not fatal: refusing to remove a feature because one
		// unit is wedged would strand the worktree and branch too. Say so
		// loudly instead — `prune` finds these by name once state is gone.
		r.Warn("could not stop %s — run `canaveral prune` to reap it",
			strings.Join(failed, ", "))
	}

	// Windows are closed last, deliberately, and the unit this process is
	// itself running under (if any) is stopped alongside them rather than
	// above — see stopFeatureUnits. Both are for the same reason: `canaveral
	// merge` (and `rm`) is very often invoked from a terminal that is one of
	// this feature's own windows, or as a shell tool call from this feature's
	// own agent, and either one can terminate the very process running this
	// function. If that happens partway through, we want it to happen *after*
	// every step that leaves durable state behind (worktree, branch, state
	// file) has already succeeded — never before. Otherwise the feature space
	// survives as an orphaned worktree/branch/state record with no windows
	// left to manage it, and the terminal that invoked the command is left
	// stuck talking to a dead shell.

	prog.done()
	prog.start("removing worktree")
	if err := removeWorktreeAndBranch(ctx, f, keepWorktree, force, keepBranch, r); err != nil {
		return err
	}

	prog.done()
	prog.start("closing windows")
	if err := state.Remove(f.Project, f.Name); err != nil {
		return err
	}
	closeFeatureWindows(ctx, f, r)
	if deferredUnit != "" {
		if err := unit.Stop(ctx, deferredUnit); err != nil {
			r.Warn("could not stop %s — run `canaveral prune` to reap it", deferredUnit)
		} else {
			unit.Reset(ctx, deferredUnit)
		}
	}

	return nil
}

// Reap finishes any feature whose teardown was interrupted before it could
// complete: `rm` killed by the very terminal window it was closing, a crash,
// a hard reboot. Remove has no deferred cleanup — the state file is what
// carries progress between the process doing the work and whatever is
// watching it, so it is deliberately the last thing deleted — which means an
// interrupted run leaves it behind with Phase still set to "removing"
// forever, and nothing before this ever came back to finish the job.
//
// Only stale records are touched: state.InPhase's ten-minute bound is what
// tells a genuinely-in-progress removal (running right now, on another
// terminal) apart from an abandoned one, and reaping the former out from
// under it would race a live `rm`.
//
// force is passed through unconditionally. It only relaxes the dirty-worktree
// guard and the refusal to touch an unmerged branch, both of which the
// original invocation already satisfied (or was told to skip) to get this
// far — asking again here would just refuse a teardown that was already
// agreed to, for a worktree nobody is coming back to un-delete.
func Reap(ctx context.Context, r Reporter) ([]string, error) {
	all, err := state.LoadAll()
	if err != nil {
		return nil, err
	}
	var done []string
	for _, f := range all {
		if f.Phase != state.PhaseRemoving || f.InPhase() {
			continue
		}
		r.Step("finishing interrupted removal of %s", f.Key())
		if err := Remove(ctx, f, false, true, false, r); err != nil {
			r.Warn("%s: %v", f.Key(), err)
			continue
		}
		done = append(done, f.Key())
	}
	return done, nil
}

// recordNamespaceSession is a best-effort step: if this feature is
// namespaced, remember its newest opencode session before tearing down its
// agent, so a later sibling under the same namespace can still fork from it
// even though this feature's own state (and therefore any record of it) is
// about to be deleted. Called here rather than continuously because this is
// the last point at which the agent is guaranteed to still be reachable.
func recordNamespaceSession(ctx context.Context, f *state.Feature) {
	ns := Namespace(f.Name)
	if ns == "" {
		return
	}
	self := unit.Self()
	for _, a := range f.Agents {
		if a.Tool != "opencode" || a.URL == "" {
			continue
		}
		// Probing the agent running this very teardown is worse than
		// pointless: its HTTP server is what dispatched the shell tool call
		// that is now blocked waiting for Remove to return, so a request to
		// it here waits out the full client timeout for nothing — the agent
		// cannot answer until this function already has.
		if a.Unit != "" && a.Unit == self {
			continue
		}
		h := agent.Probe(ctx, a.URL, f.Worktree)
		if h.Reachable && h.SessionID != "" {
			_ = skills.RecordSession(f.Project, ns, a.Name, skills.SessionRecord{
				Feature: f.Name, SessionID: h.SessionID,
				Worktree: f.Worktree, UpdatedAt: h.Updated,
			})
		}
	}
}

// removeWorktreeAndBranch removes f's worktree (unless keepWorktree) and,
// once the worktree is gone, deletes the branch too if (and only if) it has
// been fully merged into the repo's default branch — merge history is what
// makes it safe, not the caller's say-so, so unmerged work is always kept
// regardless of keepBranch. keepBranch exists purely to opt out of deletion
// even when merged, e.g. to keep it around for a while longer.
func removeWorktreeAndBranch(ctx context.Context, f *state.Feature, keepWorktree, force, keepBranch bool, r Reporter) error {
	if keepWorktree || f.Worktree == "" {
		return nil
	}
	if err := worktree.Remove(ctx, f.Root, f.Worktree, force, f.Provisioned); err != nil {
		return err
	}
	_ = worktree.Prune(ctx, f.Root)
	r.OK("removed worktree")

	deleted := false
	if !keepBranch {
		if def, err := worktree.DefaultBranch(ctx, f.Root); err == nil && def != f.Branch {
			if merged, err := worktree.IsMerged(ctx, f.Root, f.Branch, def); err == nil && merged {
				if err := worktree.DeleteBranch(ctx, f.Root, f.Branch, false); err == nil {
					deleted = true
					r.OK("deleted merged branch %s", f.Branch)
				}
			}
		}
	}
	if !deleted {
		r.OK("kept branch %s", f.Branch)
	}
	return nil
}

// closeFeatureWindows closes every window canaveral opened for f, rehomes
// any stray windows the user opened themselves, and releases the feature's
// workspace. A no-op when Hyprland is not available or the feature has no
// windows.
func closeFeatureWindows(ctx context.Context, f *state.Feature, r Reporter) {
	if err := hypr.Available(ctx); err != nil || len(f.Windows) == 0 {
		return
	}
	if clients, err := hypr.Clients(ctx); err == nil {
		open := hypr.ByClass(clients)
		closed := 0
		var self *hypr.Client
		for _, w := range f.Windows {
			if c, ok := open[w.Class]; ok {
				if hypr.IsSelf(c) {
					// Close our own window last of all, and after
					// everything above has already returned
					// successfully, so the report below reaches the
					// user before their terminal potentially dies.
					cc := c
					self = &cc
					continue
				}
				if err := hypr.Close(ctx, c.Address); err == nil {
					closed++
				}
			}
		}
		if closed > 0 {
			r.OK("closed %d window(s)", closed)
		}
		if self != nil {
			r.OK("closing this window")
			_ = hypr.Close(ctx, self.Address)
		}
	}
	// Windows the user opened here themselves are not canaveral's to
	// close — they may hold real work — but leaving them behind
	// strands them on a workspace named for a feature that no longer
	// exists, and keeps that workspace alive. Move them somewhere
	// ordinary on the same monitor instead.
	rehomeStrays(ctx, f, r)

	// Hyprland won't destroy a workspace while it's a monitor's active
	// one, even with zero windows left (confirmed empirically) — so
	// closing the last window above can leave the feature's workspace
	// dangling forever on whatever monitor last displayed it, as a
	// phantom entry in the waybar pill list. Release it explicitly.
	if err := hypr.ReleaseWorkspace(ctx, f.HyprWorkspace()); err != nil {
		r.Warn("%v", err)
	}
}

// rehomeStrays moves any window still on a removed feature's workspace to an
// ordinary workspace on the same monitor.
//
// Only windows canaveral did not spawn can be left here, since its own were
// closed just before; those belong to the user and are never closed on their
// behalf.
func rehomeStrays(ctx context.Context, f *state.Feature, r Reporter) {
	name := f.HyprWorkspace()

	clients, err := hypr.Clients(ctx)
	if err != nil {
		return
	}
	var strays []hypr.Client
	for _, c := range clients {
		if c.Workspace.Name == name {
			strays = append(strays, c)
		}
	}
	if len(strays) == 0 {
		return
	}

	all, err := hypr.Workspaces(ctx)
	if err != nil {
		return
	}
	monitor := ""
	for _, w := range all {
		if w.Name == name {
			monitor = w.Monitor
			break
		}
	}

	target := hypr.RehomeTarget(all, monitor, name)
	if target == 0 {
		// Nothing ordinary on that monitor; a fresh workspace still beats
		// leaving them on one named after a feature that is gone.
		if target, err = hypr.NextFreeWorkspaceID(ctx); err != nil {
			return
		}
	}

	moved := 0
	for _, c := range strays {
		if err := hypr.MoveWindowToWorkspace(ctx, c.Address, target); err == nil {
			moved++
		}
	}
	if moved > 0 {
		r.OK("moved %d window(s) you added to workspace %d", moved, target)
	}
}
