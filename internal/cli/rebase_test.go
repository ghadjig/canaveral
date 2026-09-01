package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bandito/canaveral/internal/state"
)

// gitFeatureRepo creates a repo with an initial commit on "main" and a
// checked-out "feat" branch with its own commit on top, returning the repo
// path (both the sole checkout and, for these tests, the feature's own
// "worktree").
func gitFeatureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	run("checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "work")
	return dir
}

func TestEnsureRebasableAllowsACleanWorktree(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "feat", Worktree: gitFeatureRepo(t), Branch: "feat"}
	if err := ensureRebasable(context.Background(), f); err != nil {
		t.Errorf("ensureRebasable: %v", err)
	}
}

func TestEnsureRebasableRefusesADirtyWorktree(t *testing.T) {
	dir := gitFeatureRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &state.Feature{Project: "p", Name: "feat", Worktree: dir, Branch: "feat"}
	if err := ensureRebasable(context.Background(), f); err == nil {
		t.Error("ensureRebasable should refuse a dirty worktree")
	}
}

func TestResolveRebaseTargetPrefersOntoOverEverythingElse(t *testing.T) {
	got, err := resolveRebaseTarget(context.Background(), "unused", "origin", "custom-branch", true)
	if err != nil {
		t.Fatal(err)
	}
	if got != "custom-branch" {
		t.Errorf("target = %q, want custom-branch", got)
	}
}

func TestResolveRebaseTargetFallsBackToTheLocalDefaultWithoutARemote(t *testing.T) {
	got, err := resolveRebaseTarget(context.Background(), gitFeatureRepo(t), "origin", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "main" {
		t.Errorf("target = %q, want main", got)
	}
}
