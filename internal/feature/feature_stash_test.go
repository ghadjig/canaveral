package feature

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
)

// requireSystemd skips a test that reaches Stash, which refuses to run
// without a reachable user manager — it is about to stop units, and doing
// half of that is worse than doing none.
func requireSystemd(t *testing.T) {
	t.Helper()
	if err := unit.Available(context.Background()); err != nil {
		t.Skipf("no systemd user manager: %v", err)
	}
}

func TestStashMovesTheRecordAndKeepsTheWorktree(t *testing.T) {
	requireSystemd(t)
	f := gitFeature(t, false)
	// Unmerged, and with an uncommitted file in the worktree: exactly the
	// state `rm` refuses and stash must not.
	if err := os.MkdirAll(f.Worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	scratch := filepath.Join(f.Worktree, "wip.txt")
	if err := os.WriteFile(scratch, []byte("half an idea"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.Slot = 3
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}

	s, err := Stash(context.Background(), f, quietReporter{})
	if err != nil {
		t.Fatalf("Stash: %v", err)
	}

	if _, err := state.Load(f.Project, f.Name); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("state.Load after Stash = %v, want ErrNotFound: the record must leave the active tree", err)
	}
	if _, err := state.LoadStash(f.Project, f.Name); err != nil {
		t.Errorf("LoadStash after Stash = %v, want the parked record", err)
	}
	if s.Feature.Slot != 3 {
		t.Errorf("stashed slot = %d, want 3 recorded so Pop can prefer it", s.Feature.Slot)
	}
	if s.StashedAt.IsZero() {
		t.Error("StashedAt is zero; `pop` with no argument has nothing to order by")
	}
	// The point of the whole feature: nothing on disk was touched.
	if b, err := os.ReadFile(scratch); err != nil || string(b) != "half an idea" {
		t.Errorf("uncommitted file = %q/%v, want it untouched", b, err)
	}
}

func TestStashLeavesNoLingeringPhase(t *testing.T) {
	// Stash writes the record and then deletes the active one, so a phase
	// left reading "removing" would sit in the stash forever and be picked
	// up by Reap as an interrupted teardown.
	requireSystemd(t)
	f := gitFeature(t, false)
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}
	s, err := Stash(context.Background(), f, quietReporter{})
	if err != nil {
		t.Fatalf("Stash: %v", err)
	}
	if s.Feature.Phase != "" {
		t.Errorf("stashed phase = %q, want it cleared", s.Feature.Phase)
	}
	stored, err := state.LoadStash(f.Project, f.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Feature.Phase != "" {
		t.Errorf("stored phase = %q, want it cleared", stored.Feature.Phase)
	}
}

func TestStashFreesTheSlotForTheNextFeature(t *testing.T) {
	requireSystemd(t)
	f := gitFeature(t, false)
	f.Slot = 0
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}
	if _, err := Stash(context.Background(), f, quietReporter{}); err != nil {
		t.Fatalf("Stash: %v", err)
	}
	slot, err := state.AllocateSlot(f.Project, "something-else")
	if err != nil {
		t.Fatal(err)
	}
	if slot != 0 {
		t.Errorf("next slot = %d, want 0: stashing has to actually free the ports", slot)
	}
}

func TestPopRefusesWhenTheNameIsAlreadyActive(t *testing.T) {
	// Only an interrupted Stash produces this, and guessing which record
	// wins would silently discard one of them.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &state.Feature{Project: "norules", Name: "both", Branch: "both", Worktree: t.TempDir()}
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveStash(&state.Stash{Feature: f, StashedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{Name: "norules"}
	_, err := Pop(context.Background(), m, "both", Options{}, quietReporter{})
	if err == nil || !strings.Contains(err.Error(), "already active") {
		t.Errorf("Pop = %v, want a refusal naming the conflict", err)
	}
}

func TestPopReportsAnUnknownNameAsNotFound(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Name: "norules"}
	_, err := Pop(context.Background(), m, "never-stashed", Options{}, quietReporter{})
	if !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Pop = %v, want ErrNotFound so the CLI can list what is stashed", err)
	}
}

func TestDiscardStashRefusesUnmergedWork(t *testing.T) {
	f := gitFeature(t, false)
	s := &state.Stash{Feature: f, StashedAt: time.Now()}
	if err := state.SaveStash(s); err != nil {
		t.Fatal(err)
	}
	err := DiscardStash(context.Background(), s, false, false, false, quietReporter{})
	if !errors.Is(err, ErrUnmerged) {
		t.Errorf("DiscardStash = %v, want ErrUnmerged: a parked branch is no less unmerged", err)
	}
	if _, err := state.LoadStash(f.Project, f.Name); err != nil {
		t.Errorf("stash was dropped despite the refusal: %v", err)
	}
}

func TestDiscardStashRemovesTheRecord(t *testing.T) {
	f := gitFeature(t, true)
	s := &state.Stash{Feature: f, StashedAt: time.Now()}
	if err := state.SaveStash(s); err != nil {
		t.Fatal(err)
	}
	if err := DiscardStash(context.Background(), s, true, false, true, quietReporter{}); err != nil {
		t.Fatalf("DiscardStash: %v", err)
	}
	if _, err := state.LoadStash(f.Project, f.Name); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("LoadStash after DiscardStash = %v, want ErrNotFound", err)
	}
}

func TestVarsForResumesTheStashedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{
		Project: "norules", Name: "small-fixes", Worktree: "/wt/sf",
		Agents: []state.Agent{{Name: "main", Tool: "opencode", URL: "http://127.0.0.1:4096"}},
	}
	v := varsFor(context.Background(), m, f, false, map[string]string{"main": "ses_parked"})
	if got, want := v.Agent["main"].Session, "--session ses_parked"; got != want {
		t.Errorf("Agent.main.Session = %q, want %q", got, want)
	}
	// Fork is the former spelling and has to keep rendering the same thing,
	// or every manifest written before the rename stops resuming anything.
	if v.Agent["main"].Fork != v.Agent["main"].Session {
		t.Errorf("Fork = %q, want it to alias Session (%q)", v.Agent["main"].Fork, v.Agent["main"].Session)
	}
}

func TestVarsForPrefersItsOwnSessionOverASiblingFork(t *testing.T) {
	// A popped feature already has its own history; handing it a
	// namespace neighbour's instead would be a strictly worse answer, and
	// forking would cost an HTTP round trip to find that out.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{
		Project: "norules", Name: "onboarding/step2", Worktree: "/wt/s2",
		// An unreachable URL: if forking were attempted it would fail and
		// leave Session empty, which is exactly what this asserts against.
		Agents: []state.Agent{{Name: "main", Tool: "opencode", URL: "http://127.0.0.1:1"}},
	}
	v := varsFor(context.Background(), m, f, true, map[string]string{"main": "ses_own"})
	if got, want := v.Agent["main"].Session, "--session ses_own"; got != want {
		t.Errorf("Agent.main.Session = %q, want %q", got, want)
	}
}

func TestVarsForLeavesSessionEmptyWithNothingToResume(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{
		Project: "norules", Name: "small-fixes", Worktree: "/wt/sf",
		Agents: []state.Agent{{Name: "main", Tool: "opencode", URL: "http://127.0.0.1:4096"}},
	}
	v := varsFor(context.Background(), m, f, false, nil)
	if v.Agent["main"].Session != "" {
		t.Errorf("Agent.main.Session = %q, want empty", v.Agent["main"].Session)
	}
}

func TestStashUsesItsOwnPhaseSoReapCannotDeleteTheWorktree(t *testing.T) {
	// The hazard this guards: Reap finishes a stale PhaseRemoving by calling
	// Remove with force set, which deletes the worktree. An interrupted
	// stash sharing that phase would turn `canaveral prune` into exactly the
	// loss of uncommitted work stashing exists to prevent.
	f := gitFeature(t, false)
	f.Phase = state.PhaseStashing
	f.PhaseSince = time.Now().Add(-2 * state.StalePhaseAfter)
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}

	done, err := Reap(context.Background(), quietReporter{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("Reap tore down an interrupted stash: %v", done)
	}
	if _, err := state.Load(f.Project, f.Name); err != nil {
		t.Errorf("state.Load = %v, want the feature left active for `canaveral stash` to retry", err)
	}
}

func TestDiscardStashPointsAtPopBeforeMerge(t *testing.T) {
	// `merge` only ever operates on an active feature, so the usual "land it
	// with `canaveral merge x`" would send the user to a second, less
	// helpful error.
	f := gitFeature(t, false)
	s := &state.Stash{Feature: f, StashedAt: time.Now()}
	if err := state.SaveStash(s); err != nil {
		t.Fatal(err)
	}
	err := DiscardStash(context.Background(), s, false, false, false, quietReporter{})
	if err == nil || !strings.Contains(err.Error(), "canaveral pop feat") {
		t.Errorf("DiscardStash = %v, want it to say to pop first", err)
	}
}
