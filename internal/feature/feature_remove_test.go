package feature

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
)

func TestReapFinishesAStaleInterruptedRemoval(t *testing.T) {
	f := gitFeature(t, true)
	f.Phase = state.PhaseRemoving
	f.PhaseSince = time.Now().Add(-2 * state.StalePhaseAfter)
	if err := state.Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	done, err := Reap(context.Background(), quietReporter{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(done) != 1 || done[0] != f.Key() {
		t.Fatalf("Reap finished %v, want [%s]", done, f.Key())
	}
	if _, err := state.Load(f.Project, f.Name); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("state.Load after Reap = %v, want ErrNotFound", err)
	}
}

func TestReapLeavesALiveRemovalAlone(t *testing.T) {
	// PhaseSince is fresh, so InPhase is still true: an `rm` running right now
	// on another terminal must not be raced by a concurrent Reap.
	f := gitFeature(t, true)
	f.Phase = state.PhaseRemoving
	f.PhaseSince = time.Now()
	if err := state.Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	done, err := Reap(context.Background(), quietReporter{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("Reap touched a live removal: %v", done)
	}
	if _, err := state.Load(f.Project, f.Name); err != nil {
		t.Errorf("state.Load after Reap = %v, want the record still there", err)
	}
}

func TestReapIgnoresFeaturesNotBeingRemoved(t *testing.T) {
	f := gitFeature(t, true)
	if err := state.Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}

	done, err := Reap(context.Background(), quietReporter{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(done) != 0 {
		t.Errorf("Reap touched a feature with no phase set: %v", done)
	}
}

func TestUnitsForOrdersAgentsFirstThenServicesInReverse(t *testing.T) {
	// Reverse service order matters: a service started later may depend on an
	// earlier one, so it has to go down first.
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: "a"}, {Unit: "b"}, {Unit: "c"}},
		Agents:   []state.Agent{{Unit: "agent"}},
	}
	got := UnitsFor(context.Background(), f)
	want := []string{"agent", "c", "b", "a"}
	if len(got) < len(want) {
		t.Fatalf("UnitsFor = %v, want at least %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("UnitsFor[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func TestUnitsForDeduplicates(t *testing.T) {
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: "dup"}, {Unit: "dup"}},
		Agents:   []state.Agent{{Unit: "dup"}},
	}
	if got := UnitsFor(context.Background(), f); len(got) != 1 || got[0] != "dup" {
		t.Errorf("UnitsFor = %v, want [dup]", got)
	}
}

func TestUnitsForSkipsEmptyNames(t *testing.T) {
	// An interrupted reconcile can leave a half-written record; stopping ""
	// would ask systemctl to stop ".service".
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: ""}, {Unit: "real"}},
	}
	if got := UnitsFor(context.Background(), f); len(got) != 1 || got[0] != "real" {
		t.Errorf("UnitsFor = %v, want [real]", got)
	}
}

// gitFeature builds a real repo with a feature branch and returns a state
// record pointing at it. mergeTarget shells out to git, so there is no
// meaningful way to test it against a fake.
func gitFeature(t *testing.T, merge bool) *state.Feature {
	t.Helper()
	// These tests run Remove far enough to touch the state directory, and
	// this suite executes inside a real canaveral worktree where XDG_STATE_HOME
	// points at the live one. Isolate it.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	run("checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "work")
	run("checkout", "-q", "main")
	if merge {
		run("merge", "-q", "--no-ff", "-m", "merge feat", "feat")
	}
	return &state.Feature{
		Project: "p", Name: "feat", Root: repo, Branch: "feat",
		Worktree: filepath.Join(repo, "wt"),
	}
}

func TestMergeTargetDetectsUnmergedBranch(t *testing.T) {
	merged, target, ok := mergeTarget(context.Background(), gitFeature(t, false))
	if !ok {
		t.Fatal("mergeTarget could not answer for a plain repo")
	}
	if merged {
		t.Error("a branch with its own commit is not merged into main")
	}
	if target != "main" {
		t.Errorf("target = %q, want main", target)
	}
}

func TestMergeTargetDetectsMergedBranch(t *testing.T) {
	merged, target, ok := mergeTarget(context.Background(), gitFeature(t, true))
	if !ok || !merged {
		t.Errorf("merged = %v, ok = %v, want both true", merged, ok)
	}
	if target != "main" {
		t.Errorf("target = %q, want main", target)
	}
}

func TestMergeTargetTreatsTheDefaultBranchAsMerged(t *testing.T) {
	// A feature sitting on main itself has nothing to be merged into, and
	// must not be blocked from removal by the guard.
	f := gitFeature(t, false)
	f.Branch = "main"
	merged, _, ok := mergeTarget(context.Background(), f)
	if !ok || !merged {
		t.Errorf("merged = %v, ok = %v, want both true", merged, ok)
	}
}

func TestMergeTargetCannotAnswerWithoutARepo(t *testing.T) {
	// ok=false means "do not block": refusing teardown because the repo has
	// no discoverable default branch would be worse than the risk guarded.
	f := &state.Feature{Project: "p", Name: "f", Root: t.TempDir(), Branch: "feat"}
	if _, _, ok := mergeTarget(context.Background(), f); ok {
		t.Error("mergeTarget claimed an answer for a non-repo")
	}
}

func TestMergeTargetCannotAnswerWithoutABranch(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "f", Root: "", Branch: ""}
	if _, _, ok := mergeTarget(context.Background(), f); ok {
		t.Error("mergeTarget claimed an answer with no root or branch")
	}
}

func TestRemoveRefusesUnmergedWork(t *testing.T) {
	f := gitFeature(t, false)
	err := Remove(context.Background(), f, false, false, false, quietReporter{})
	if !errors.Is(err, ErrUnmerged) {
		t.Fatalf("Remove err = %v, want ErrUnmerged", err)
	}
	// The refusal must come before anything destructive: state is still there.
	if !strings.Contains(err.Error(), "canaveral merge feat") {
		t.Errorf("error should point at merge: %v", err)
	}
}

func TestRemoveAllowsUnmergedWorkWithForce(t *testing.T) {
	f := gitFeature(t, false)
	// Not a full teardown (no state file, no worktree), but it must get past
	// the guard rather than refusing.
	err := Remove(context.Background(), f, false, true, false, quietReporter{})
	if errors.Is(err, ErrUnmerged) {
		t.Error("--force must override the merge guard")
	}
}

func TestRemoveSkipsTheGuardWhenKeepingTheWorktree(t *testing.T) {
	f := gitFeature(t, false)
	err := Remove(context.Background(), f, true, false, false, quietReporter{})
	if errors.Is(err, ErrUnmerged) {
		t.Error("--keep-worktree leaves the checkout and branch intact; nothing to guard")
	}
}

// gitBranchExists reports whether branch still exists in repo.
func gitBranchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "branch", "--list", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) != ""
}

func TestRemoveDeletesMergedBranchAfterTeardown(t *testing.T) {
	f := gitFeature(t, true)
	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if gitBranchExists(t, f.Root, f.Branch) {
		t.Error("merged branch should have been deleted")
	}
}

func TestRemoveKeepsBranchWhenRequested(t *testing.T) {
	f := gitFeature(t, true)
	if err := Remove(context.Background(), f, false, false, true, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !gitBranchExists(t, f.Root, f.Branch) {
		t.Error("--keep-branch must keep a merged branch around")
	}
}

func TestRemoveKeepsUnmergedBranchRegardlessOfKeepBranch(t *testing.T) {
	// keepBranch only ever opts *out* of deletion; it must never be read as
	// permission to delete an unmerged branch when it is false.
	f := gitFeature(t, false)
	if err := Remove(context.Background(), f, false, true, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !gitBranchExists(t, f.Root, f.Branch) {
		t.Error("unmerged branch must survive Remove even with keepBranch=false")
	}
}

func TestRemoveRecordsNamespaceSessionBeforeTeardown(t *testing.T) {
	f := gitFeature(t, true)
	f.Project = "norules"
	f.Name = "onboarding/step1"

	sessions := `{"data":[{"id":"ses_live","location":{"directory":"` + f.Worktree + `"},"time":{"updated":` +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + `}}]}`
	srv := agentFakeServer(t, sessions, `[]`)
	f.Agents = []state.Agent{{Name: "main", Tool: "opencode", URL: srv.URL}}

	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	rec, ok, err := skills.LatestSession(f.Project, "onboarding", "main")
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if !ok {
		t.Fatal("Remove did not record the namespace session before tearing down")
	}
	if rec.SessionID != "ses_live" {
		t.Errorf("recorded session = %q, want ses_live", rec.SessionID)
	}
	if rec.Feature != f.Name {
		t.Errorf("recorded feature = %q, want %q", rec.Feature, f.Name)
	}
}

func TestRemoveDoesNotRecordSessionForUnnamespacedFeature(t *testing.T) {
	f := gitFeature(t, true)
	f.Project = "norules"
	// f.Name is "feat", not namespaced.

	sessions := `{"data":[{"id":"ses_live","location":{"directory":"` + f.Worktree + `"},"time":{"updated":` +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + `}}]}`
	srv := agentFakeServer(t, sessions, `[]`)
	f.Agents = []state.Agent{{Name: "main", Tool: "opencode", URL: srv.URL}}

	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok, err := skills.LatestSession(f.Project, "", "main"); err == nil && ok {
		t.Error("Remove recorded a session for a feature with no namespace")
	}
}
