package cli

import (
	"context"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// TestRunPruneDryRunDoesNotStopAnything is deliberately narrow: --dry-run
// only ever lists (unit.List is a read-only systemctl query; unit.StopAll,
// the only part of runPrune with real side effects, is unreachable under
// --dry-run), so this is safe to run for real regardless of whatever live
// canaveral units this sandbox's own session happens to have. It exists to
// exercise runPrune's own wiring (flag parsing, loading state, the
// no-orphans/dry-run branches); Orphans' actual claim/reap logic is
// covered directly in internal/unit.
func TestRunPruneDryRunDoesNotStopAnything(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "prune-dry-run"))

	if err := runPrune(context.Background(), []string{"--dry-run"}); err != nil {
		t.Fatalf("runPrune --dry-run: %v", err)
	}
}

// TestRunPruneDryRunListsAStuckRemovalWithoutFinishingIt exercises the other
// half of runPrune: a feature whose teardown was interrupted long enough ago
// that its phase is no longer believed. --dry-run must say so without
// actually finishing it — feature.Reap's own behaviour is covered directly in
// internal/feature.
func TestRunPruneDryRunListsAStuckRemovalWithoutFinishingIt(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "prune-stuck"))

	f := &state.Feature{Project: "prune-stuck", Name: "abandoned", Root: t.TempDir()}
	f.Phase = state.PhaseRemoving
	f.PhaseSince = time.Now().Add(-2 * state.StalePhaseAfter)
	if err := state.Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := runPrune(context.Background(), []string{"--dry-run"}); err != nil {
		t.Fatalf("runPrune --dry-run: %v", err)
	}
	if _, err := state.Load(f.Project, f.Name); err != nil {
		t.Errorf("state.Load after --dry-run = %v, want the record untouched", err)
	}
}
