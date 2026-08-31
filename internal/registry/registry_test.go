package registry

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// project writes a minimal valid checkout and returns its root.
func project(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name = \"" + name + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "canaveral.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRecordThenFind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := project(t, filepath.Join(t.TempDir(), "norules"), "norules")

	if err := Record("norules", root); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, found, err := Find("norules")
	if err != nil || !found {
		t.Fatalf("Find = %v, %v, %v", p, found, err)
	}
	if p.Root != root {
		t.Errorf("root = %q, want %q", p.Root, root)
	}
	if p.LastUsed.IsZero() {
		t.Error("LastUsed not set; the launcher's ordering depends on it")
	}
}

func TestRecordRefusesToRepointALiveRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	first := project(t, filepath.Join(base, "a", "web"), "web")
	second := project(t, filepath.Join(base, "b", "web"), "web")

	if err := Record("web", first); err != nil {
		t.Fatal(err)
	}
	// Both checkouts exist and both call themselves "web", so they already
	// share <state>/features/web and therefore each other's features. Silently
	// repointing would hide that; the caller has to be told.
	err := Record("web", second)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Record = %v, want ErrConflict", err)
	}
	p, _, _ := Find("web")
	if p.Root != first {
		t.Errorf("root = %q, want the original %q left in place", p.Root, first)
	}
}

func TestRecordRepointsAVanishedRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	old := project(t, filepath.Join(base, "old"), "web")
	if err := Record("web", old); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}

	// A repository that moved is the common case, and there is nothing to
	// conflict with once the original is gone.
	moved := project(t, filepath.Join(base, "new"), "web")
	if err := Record("web", moved); err != nil {
		t.Fatalf("Record: %v", err)
	}
	p, _, _ := Find("web")
	if p.Root != moved {
		t.Errorf("root = %q, want %q", p.Root, moved)
	}
}

func TestLoadDerivesProjectsFromFeatureState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := project(t, filepath.Join(t.TempDir(), "legacy"), "legacy")
	// A project that predates the registry has no entry of its own, but every
	// feature record carries the root it belongs to.
	if err := state.Save(&state.Feature{Project: "legacy", Name: "x", Root: root}); err != nil {
		t.Fatal(err)
	}

	p, found, err := Find("legacy")
	if err != nil || !found {
		t.Fatalf("Find = %v, %v, %v", p, found, err)
	}
	if p.Root != root {
		t.Errorf("root = %q, want %q", p.Root, root)
	}
}

func TestForgetADerivedEntryReportsItIsNotInTheFile(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := project(t, filepath.Join(t.TempDir(), "legacy"), "legacy")
	if err := state.Save(&state.Feature{Project: "legacy", Name: "x", Root: root}); err != nil {
		t.Fatal(err)
	}

	found, err := Forget("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("Forget reported a removal, but the entry was only ever derived")
	}
	if _, still, _ := Find("legacy"); !still {
		t.Error("derived entry disappeared; it should keep coming back while its features exist")
	}
}

func TestPruneDropsOnlyDeadEntries(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	live := project(t, filepath.Join(base, "live"), "live")
	dead := project(t, filepath.Join(base, "dead"), "dead")
	if err := Record("live", live); err != nil {
		t.Fatal(err)
	}
	if err := Record("dead", dead); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dead); err != nil {
		t.Fatal(err)
	}

	dropped, err := Prune()
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0].Name != "dead" {
		t.Fatalf("dropped = %v, want [dead]", dropped)
	}
	if _, found, _ := Find("live"); !found {
		t.Error("live project was pruned too")
	}
}

func TestMRUOrdersByLastUsedThenName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	for _, n := range []string{"alpha", "beta"} {
		if err := Record(n, project(t, filepath.Join(base, n), n)); err != nil {
			t.Fatal(err)
		}
		// Record stamps time.Now(); without a gap the two are indistinguishable
		// at the clock's resolution and the test would assert nothing.
		time.Sleep(2 * time.Millisecond)
	}

	got, err := MRU()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "beta" {
		t.Fatalf("MRU = %v, want the most recent (beta) first", got)
	}
}

func TestScanSkipsWorktreesAndNestedProjects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	root := project(t, filepath.Join(base, "norules"), "norules")

	// canaveral copies the manifest into every worktree it provisions, so the
	// conventional layout puts a second, identical canaveral.toml here. A scan
	// that descended into it would register the same project twice — and then
	// refuse the second with a conflict, which is worse than useless noise.
	project(t, filepath.Join(root, "worktrees", "small-fixes"), "norules")

	// A worktree configured to live outside the project is not caught by "stop
	// at the first manifest", so it has to be recognised on its own: git writes
	// .git as a file, not a directory, in a linked worktree.
	away := project(t, filepath.Join(base, "elsewhere", "small-fixes"), "norules")
	if err := os.WriteFile(filepath.Join(away, ".git"), []byte("gitdir: /somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	found, conflicts, err := Scan(base)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("conflicts = %v, want none", conflicts)
	}
	if len(found) != 1 || found[0].Name != "norules" || found[0].Root != root {
		t.Fatalf("found = %v, want just the main checkout at %s", found, root)
	}
}

func TestRecordIgnoresAFeatureWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	base := t.TempDir()
	real := project(t, filepath.Join(base, "norules"), "norules")
	if err := Record("norules", real); err != nil {
		t.Fatal(err)
	}

	// canaveral copies the manifest into every worktree it provisions, so from
	// inside one, manifest.Find stops there and reports the worktree as the
	// project root. Recording that would claim the project lives inside its own
	// feature, and then conflict with the real checkout on every command run
	// from anywhere else — which is exactly how this was found.
	wt := project(t, filepath.Join(real, "worktrees", "small-fixes"), "norules")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /somewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Record("norules", wt); err != nil {
		t.Fatalf("Record from a worktree = %v, want it quietly ignored", err)
	}
	p, _, _ := Find("norules")
	if p.Root != real {
		t.Errorf("root = %q, want the real checkout %q", p.Root, real)
	}
	if _, err := Add(wt); err == nil {
		t.Error("Add accepted a feature worktree")
	}
}
