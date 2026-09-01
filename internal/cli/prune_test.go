package cli

import (
	"context"
	"testing"
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
