package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// clearFeatureEnv unsets everything the "which feature am I in" lookup reads.
//
// These tests run inside a canaveral feature worktree, where CANAVERAL_PROJECT,
// CANAVERAL_FEATURE and CANAVERAL_ROOT are all set by the agent's own unit. A
// test that inherits them is testing the surrounding worktree, not its own
// fixture, and will pass or fail for reasons that have nothing to do with the
// code — see AGENTS.md.
func clearFeatureEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CANAVERAL_PROJECT", "")
	t.Setenv("CANAVERAL_FEATURE", "")
	t.Setenv("CANAVERAL_ROOT", "")
	// Hyprland is the last-resort signal; a test must never reach the real
	// compositor and pick up whatever workspace the developer is looking at.
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
}

// saveFeature records a feature with a real worktree directory on disk.
func saveFeature(t *testing.T, project, name string) *state.Feature {
	t.Helper()
	wt := filepath.Join(t.TempDir(), project, name)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &state.Feature{Project: project, Name: name, Worktree: wt}
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestFocusedFeatureFromWorkingDirectory(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	f := saveFeature(t, "norules", "small-fixes")

	// A subdirectory counts: you are still in the feature.
	sub := filepath.Join(f.Worktree, "app", "models")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	got, err := focusedFeature(t.Context())
	if err != nil {
		t.Fatalf("focusedFeature: %v", err)
	}
	if got.Name != "small-fixes" {
		t.Errorf("feature = %q, want small-fixes", got.Name)
	}
}

func TestFocusedFeatureFromEnvironmentOutsideTheWorktree(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	saveFeature(t, "norules", "small-fixes")

	// The case this exists for: a terminal canaveral opened on the feature's
	// workspace, which has since been cd'd somewhere else entirely. The shell
	// still belongs to that feature.
	t.Chdir(t.TempDir())
	t.Setenv("CANAVERAL_PROJECT", "norules")
	t.Setenv("CANAVERAL_FEATURE", "small-fixes")

	got, err := focusedFeature(t.Context())
	if err != nil {
		t.Fatalf("focusedFeature: %v", err)
	}
	if got.Name != "small-fixes" {
		t.Errorf("feature = %q, want small-fixes", got.Name)
	}
}

func TestFocusedFeaturePrefersTheDirectoryOverTheEnvironment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	saveFeature(t, "norules", "from-env")
	inDir := saveFeature(t, "norules", "from-dir")

	// Standing in one feature's worktree from a shell that belongs to another
	// is entirely possible. Where you actually are wins over where you came
	// from: it is the more specific answer, and the one you can see.
	t.Chdir(inDir.Worktree)
	t.Setenv("CANAVERAL_PROJECT", "norules")
	t.Setenv("CANAVERAL_FEATURE", "from-env")

	got, err := focusedFeature(t.Context())
	if err != nil {
		t.Fatalf("focusedFeature: %v", err)
	}
	if got.Name != "from-dir" {
		t.Errorf("feature = %q, want from-dir", got.Name)
	}
}

func TestFocusedFeatureFindsFeaturesOfAnyProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	saveFeature(t, "canaveral", "other")
	f := saveFeature(t, "norules", "small-fixes")

	// No manifest is in scope here at all, which is the point: `canaveral path`
	// with no argument has to answer from a directory belonging to no project.
	t.Chdir(f.Worktree)

	got, err := focusedFeature(t.Context())
	if err != nil {
		t.Fatalf("focusedFeature: %v", err)
	}
	if got.Project != "norules" || got.Name != "small-fixes" {
		t.Errorf("feature = %s/%s, want norules/small-fixes", got.Project, got.Name)
	}
}

func TestFocusedFeatureFailsWhenThereIsNoSignal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	saveFeature(t, "norules", "small-fixes")
	t.Chdir(t.TempDir())

	if _, err := focusedFeature(t.Context()); err == nil {
		t.Fatal("focusedFeature succeeded outside any feature")
	}
}

func TestCurrentFeatureIgnoresAnotherProjectsEnvironment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clearFeatureEnv(t)
	saveFeature(t, "canaveral", "elsewhere")
	t.Chdir(t.TempDir())
	t.Setenv("CANAVERAL_PROJECT", "canaveral")
	t.Setenv("CANAVERAL_FEATURE", "elsewhere")

	// currentFeature is scoped to the project a command is operating on, so a
	// shell belonging to a different project's feature must not answer for it —
	// `canaveral merge` here would otherwise merge a branch in another repo.
	m := manifestNamed("norules")
	if _, err := currentFeature(m); err == nil {
		t.Fatal("currentFeature resolved a feature of a different project")
	}
}

// manifestNamed is the smallest manifest currentFeature needs: just the project.
func manifestNamed(name string) *manifest.Manifest {
	return &manifest.Manifest{Name: name}
}
