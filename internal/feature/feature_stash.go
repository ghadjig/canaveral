package feature

// Stash and Pop: parking a whole feature workspace and bringing it back.
//
// A stash is deliberately the non-destructive half of Remove. Everything
// Remove tears down that costs something to keep — systemd units, windows,
// the workspace, the port slot, the widget number — is released here too.
// Everything Remove tears down that *is* the work — the worktree, its
// uncommitted edits and untracked files, the branch, the agent's
// conversation — is left exactly as it stands. That is what makes stashing
// safe on unmerged work where `rm` refuses outright: there is nothing to
// refuse, because nothing is lost.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
)

// Stash parks a feature: records which conversation each agent was in, stops
// its units, closes its windows, and moves its record out of the active tree
// so it stops holding a port slot and a widget number.
//
// The worktree and branch are not touched at all. No merge check and no
// dirty-worktree check, therefore: both exist in Remove to stop work being
// destroyed, and this destroys none.
func Stash(ctx context.Context, f *state.Feature, r Reporter) (*state.Stash, error) {
	if err := unit.Available(ctx); err != nil {
		return nil, err
	}

	// Three steps — sessions, units, windows — published the same way
	// Remove's four are, and for the same reason: stopping units takes long
	// enough that a status bar should say so. A phase of its own, not
	// PhaseRemoving: see state.PhaseStashing for what reusing that would
	// cost.
	prog := newProgress(f, state.PhaseStashing, 3)

	prog.start("saving sessions")
	sessions := recordSessions(ctx, f)
	// A namespace sibling should be able to fork from this feature while it
	// is parked, exactly as it can once one is removed. Stashing is the more
	// common way a feature goes quiet, so skipping this would make the
	// namespace's shared history go stale in the ordinary case.
	recordNamespaceSession(ctx, f)

	prog.done()
	prog.start("stopping services")
	deferredUnit, failed := stopFeatureUnits(ctx, f, r)
	if len(failed) > 0 {
		// Not fatal, for the same reason it is not fatal in Remove: a wedged
		// unit must not strand the workspace. `prune` finds it by name once
		// the record leaves the active tree.
		r.Warn("could not stop %s — run `canaveral prune` to reap it", strings.Join(failed, ", "))
	}

	prog.done()
	prog.start("closing windows")
	s := &state.Stash{Feature: f, StashedAt: time.Now(), Sessions: sessions}
	// Written before the active record is deleted, and the deletion is what
	// makes it a stash: if this process dies between the two, the worst case
	// is a feature that exists in both trees, which Pop resolves in favour
	// of the live one. Deleting first would risk losing the record entirely.
	//
	// The phase is cleared on the copy being written, not left at
	// "removing": nothing is going to come back and advance it, and a stash
	// that reads as mid-teardown forever would be picked up by Reap.
	f.Phase, f.PhaseLabel, f.PhaseStep, f.PhaseTotal = "", "", 0, 0
	f.PhaseSince = time.Time{}
	if err := state.SaveStash(s); err != nil {
		return nil, fmt.Errorf("save stash: %w", err)
	}
	if err := state.Remove(f.Project, f.Name); err != nil {
		return nil, err
	}
	closeFeatureWindows(ctx, f, r)
	// Stopped last, after the stash record is durable — see Remove for the
	// full reasoning. `canaveral stash` run from the feature's own terminal
	// or as a tool call from its own agent kills this very process, and if
	// that happens it must happen after the record is safely written.
	if deferredUnit != "" {
		if err := unit.Stop(ctx, deferredUnit); err != nil {
			r.Warn("could not stop %s — run `canaveral prune` to reap it", deferredUnit)
		} else {
			unit.Reset(ctx, deferredUnit)
		}
	}
	return s, nil
}

// recordSessions probes each opencode agent for the session it is working in,
// so a pop can reopen that conversation rather than starting a new one.
//
// Best-effort throughout: an unreachable agent, or one that has not been
// spoken to yet, simply contributes nothing. The agent running this very
// command is skipped for the reason spelled out in recordNamespaceSession —
// it cannot answer until this function has already returned.
func recordSessions(ctx context.Context, f *state.Feature) map[string]string {
	self := unit.Self()
	out := map[string]string{}
	for _, a := range f.Agents {
		if a.Tool != "opencode" || a.URL == "" {
			continue
		}
		if a.Unit != "" && a.Unit == self {
			continue
		}
		if h := agent.Probe(ctx, a.URL, f.Worktree); h.Reachable && h.SessionID != "" {
			out[a.Name] = h.SessionID
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Pop restores a stashed feature and reconciles it back to life.
//
// The stashed record is put back as-is except for its slot, which was
// released on stash and is re-allocated here (preferring the old number, so
// ports usually come back unchanged), and its ports and database suffix,
// which follow the slot and the manifest exactly as they do on any other
// load. Reconcile then finds an existing feature whose worktree is already
// there and already provisioned, so it only has to start what is missing:
// services, agents, windows.
func Pop(ctx context.Context, m *manifest.Manifest, name string, opt Options, r Reporter) (*Result, error) {
	name = Slug(name)
	s, err := state.LoadStash(m.Name, name)
	if err != nil {
		return nil, err
	}
	// A name that is live and stashed at once should not silently overwrite
	// the live one. It takes an interrupted Stash to get here, so say what
	// to do rather than guessing.
	if _, err := state.Load(m.Name, name); err == nil {
		return nil, fmt.Errorf("%s/%s is already active; the stash is stale, and `canaveral rm %s` will clear it once the feature is gone", m.Name, name, name)
	}

	f := s.Feature
	// A stash's whole value is that the worktree was left alone, so say so
	// when it is not there any more: something outside canaveral deleted it,
	// and Reconcile is about to rebuild it from the branch. The committed
	// work survives that; anything uncommitted did not, and finding out
	// silently is the wrong way to learn it.
	if f.Worktree != "" {
		if _, err := os.Stat(f.Worktree); err != nil {
			r.Warn("worktree %s is gone; rebuilding it from %s (uncommitted work there is lost)",
				f.Worktree, f.Branch)
		}
	}

	slot, err := state.AllocateSlotPreferring(m.Name, name, f.Slot)
	if err != nil {
		return nil, err
	}
	if slot != f.Slot {
		r.Info("slot %d was taken; this feature comes back on slot %d", f.Slot, slot)
	}
	f.Slot = slot
	if err := state.Save(f); err != nil {
		return nil, fmt.Errorf("restore state: %w", err)
	}
	// Removed only once the active record is durable, so an interruption
	// leaves the feature recoverable from one tree or the other rather than
	// from neither.
	if err := state.RemoveStash(m.Name, name); err != nil {
		return nil, err
	}

	opt.Resume = s.Sessions
	res, err := Reconcile(ctx, m, name, opt, r)
	if res != nil {
		res.Restored = true
	}
	return res, err
}

// DiscardStash tears down a stashed feature for good: its worktree and, if it
// has landed, its branch. Units and windows are already gone by definition,
// which is why this is not simply Remove — Remove would ask systemd and
// Hyprland about a feature that has not existed to either of them since it
// was stashed.
func DiscardStash(ctx context.Context, s *state.Stash, keepWorktree, force, keepBranch bool, r Reporter) error {
	f := s.Feature
	if !force && !keepWorktree && f.Worktree != "" {
		if merged, target, ok := mergeTarget(ctx, f); ok && !merged {
			return &unmergedError{feature: f.Name, branch: f.Branch, target: target, stashed: true}
		}
	}
	if err := removeWorktreeAndBranch(ctx, f, keepWorktree, force, keepBranch, r); err != nil {
		return err
	}
	return state.RemoveStash(f.Project, f.Name)
}
