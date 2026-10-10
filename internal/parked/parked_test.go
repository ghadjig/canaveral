package parked

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// project builds a git checkout with a manifest, isolated from the
// developer's own state and from the feature worktree these tests usually
// run inside, whose CANAVERAL_* variables would otherwise leak in.
func project(t *testing.T, name string) *manifest.Manifest {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("CANAVERAL_ROOT", "")
	t.Setenv("CANAVERAL_FEATURE", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)

	root := filepath.Join(tmp, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, manifest.FileName),
		[]byte("name = \""+name+"\"\nbranch = \"feat/{{.Feature}}\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-q", "-b", "main")
	git(t, root, "add", ".")
	git(t, root, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init")
	m, err := manifest.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func names(ws []Workspace) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name+":"+string(w.Kind))
	}
	return out
}

func TestDormantsListsOnlyUnrecordedWakeableWorktrees(t *testing.T) {
	m := project(t, "demo")
	ctx := context.Background()
	wt := func(rel string) string { return filepath.Join(m.Root, "worktrees", filepath.FromSlash(rel)) }

	// Dormant, on a branch the template never produced: still wakeable.
	git(t, m.Root, "worktree", "add", "-q", "-b", "whatever", wt("ns/sleepy"))
	// Has an active record.
	git(t, m.Root, "worktree", "add", "-q", "-b", "feat/live", wt("live"))
	if err := state.Save(&state.Feature{Project: "demo", Name: "live", Worktree: wt("live")}); err != nil {
		t.Fatal(err)
	}
	// Stashed.
	git(t, m.Root, "worktree", "add", "-q", "-b", "feat/parked", wt("parked"))
	if err := state.SaveStash(&state.Stash{Feature: &state.Feature{Project: "demo", Name: "parked"}}); err != nil {
		t.Fatal(err)
	}
	// Detached: no branch to adopt.
	git(t, m.Root, "worktree", "add", "-q", "--detach", wt("detached"))
	// Outside the worktree root: no feature name reaches it.
	git(t, m.Root, "worktree", "add", "-q", "-b", "elsewhere", filepath.Join(filepath.Dir(m.Root), "elsewhere"))
	// A name that does not survive slugging.
	git(t, m.Root, "worktree", "add", "-q", "-b", "caps", wt("Caps"))

	got, err := Dormants(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "ns/sleepy" || got[0].Branch != "whatever" || got[0].Kind != Dormant {
		t.Fatalf("Dormants = %+v, want only ns/sleepy on whatever", got)
	}
	if got[0].Key != "demo/ns/sleepy" || got[0].Since.IsZero() {
		t.Errorf("dormant entry = %+v, want key demo/ns/sleepy and a last-touched time", got[0])
	}

	all, err := Project(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("Project = %v, want the stash and the dormant worktree", names(all))
	}
}

// Run from inside a feature worktree, the manifest found is that worktree's
// own copy, so its Root is the worktree. Resolving the worktree directory
// from there would look for worktrees nested inside it and find nothing.
func TestDormantsFromInsideAWorktree(t *testing.T) {
	m := project(t, "demo")
	ctx := context.Background()
	here := filepath.Join(m.Root, "worktrees", "here")
	git(t, m.Root, "worktree", "add", "-q", "-b", "feat/here", here)
	git(t, m.Root, "worktree", "add", "-q", "-b", "feat/other", filepath.Join(m.Root, "worktrees", "other"))
	if err := state.Save(&state.Feature{Project: "demo", Name: "here", Worktree: here}); err != nil {
		t.Fatal(err)
	}

	inside, err := manifest.Load(here)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Dormants(ctx, inside)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("Dormants from inside a worktree = %v, want [other]", names(got))
	}
}

func TestFindDormant(t *testing.T) {
	m := project(t, "demo")
	ctx := context.Background()
	git(t, m.Root, "worktree", "add", "-q", "-b", "whatever", filepath.Join(m.Root, "worktrees", "ns", "sleepy"))

	d, ok := FindDormant(ctx, m, "ns/sleepy")
	if !ok || d.Branch != "whatever" {
		t.Fatalf("FindDormant(ns/sleepy) = %+v, %v", d, ok)
	}
	for _, name := range []string{"ns", "missing", ""} {
		if _, ok := FindDormant(ctx, m, name); ok {
			t.Errorf("FindDormant(%q) found something", name)
		}
	}

	// Recorded, even unreadably, is not dormant: a corrupt record must not
	// make its worktree look abandoned.
	dir, _ := state.Dir()
	p := filepath.Join(dir, "features", "demo", "ns", "sleepy.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := FindDormant(ctx, m, "ns/sleepy"); ok {
		t.Error("a worktree with a corrupt record was reported dormant")
	}
}

func TestProjectOrdersNewestFirst(t *testing.T) {
	m := project(t, "demo")
	old := time.Now().Add(-48 * time.Hour)
	for name, at := range map[string]time.Time{"older": old, "newer": old.Add(time.Hour)} {
		if err := state.SaveStash(&state.Stash{Feature: &state.Feature{Project: "demo", Name: name}, StashedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	// Touched just now, so it sorts ahead of both stashes.
	git(t, m.Root, "worktree", "add", "-q", "-b", "fresh", filepath.Join(m.Root, "worktrees", "fresh"))

	got, err := Project(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"fresh:dormant", "newer:stashed", "older:stashed"}
	if g := names(got); len(g) != 3 || g[0] != want[0] || g[1] != want[1] || g[2] != want[2] {
		t.Fatalf("Project = %v, want %v", g, want)
	}
}
