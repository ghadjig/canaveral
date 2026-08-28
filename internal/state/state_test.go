package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newFeature(project, name string, slot int) *Feature {
	return &Feature{
		Project:   project,
		Name:      name,
		Root:      "/w/" + project,
		Slot:      slot,
		Branch:    name,
		Worktree:  "/wt/" + project + "/" + name,
		Ports:     map[string]int{"web": 3000 + slot},
		CreatedAt: time.Now().Truncate(time.Second),
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := newFeature("norules", "small-fixes", 0)
	f.DBSuffix = "_small_fixes"
	f.Services = []Service{{Name: "web", Unit: "u-web"}, {Name: "jobs", Unit: "u-jobs", Optional: true}}
	f.Agents = []Agent{{Name: "main", Unit: "u-agent", URL: "http://127.0.0.1:4096", Port: 4096}}
	f.Windows = []Window{{Name: "chrome", Class: "canaveral-norules-small-fixes-chrome"}}

	if err := Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load("norules", "small-fixes")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Slot != 0 || got.Ports["web"] != 3000 || got.DBSuffix != "_small_fixes" {
		t.Errorf("round trip mismatch: %+v", got)
	}
	if len(got.Services) != 2 || !got.Services[1].Optional {
		t.Errorf("services not preserved: %+v", got.Services)
	}
	if len(got.Windows) != 1 || got.Windows[0].Class == "" {
		t.Errorf("windows not preserved: %+v", got.Windows)
	}
	if !got.CreatedAt.Equal(f.CreatedAt) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, f.CreatedAt)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := Load("norules", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFeaturesAreIsolatedPerProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Save(newFeature("alpha", "shared-name", 0)); err != nil {
		t.Fatal(err)
	}
	if err := Save(newFeature("beta", "shared-name", 0)); err != nil {
		t.Fatal(err)
	}
	// Same feature name in two projects must not collide.
	a, err := Load("alpha", "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	if a.Root != "/w/alpha" {
		t.Errorf("Root = %q, want /w/alpha", a.Root)
	}
	names, _ := List("beta")
	if len(names) != 1 {
		t.Errorf("beta features = %v", names)
	}
}

func TestAllocateSlotIsDenseAndStable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for i, n := range []string{"one", "two", "three"} {
		slot, err := AllocateSlot("norules", n)
		if err != nil {
			t.Fatal(err)
		}
		if slot != i {
			t.Fatalf("AllocateSlot(%s) = %d, want %d", n, slot, i)
		}
		if err := Save(newFeature("norules", n, slot)); err != nil {
			t.Fatal(err)
		}
	}

	// An existing feature keeps its slot, so its ports never move.
	slot, err := AllocateSlot("norules", "two")
	if err != nil {
		t.Fatal(err)
	}
	if slot != 1 {
		t.Errorf("existing feature slot = %d, want 1", slot)
	}

	// A removed slot is reused by the next new feature.
	if err := Remove("norules", "two"); err != nil {
		t.Fatal(err)
	}
	slot, err = AllocateSlot("norules", "four")
	if err != nil {
		t.Fatal(err)
	}
	if slot != 1 {
		t.Errorf("reused slot = %d, want 1", slot)
	}
}

func TestLoadProjectOrdersBySlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// Saved out of order; listing must come back by slot.
	for _, f := range []*Feature{
		newFeature("p", "zulu", 2),
		newFeature("p", "alpha", 0),
		newFeature("p", "mike", 1),
	} {
		if err := Save(f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := LoadProject("p")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mike", "zulu"}
	for i, f := range got {
		if f.Name != want[i] {
			t.Errorf("position %d = %q, want %q", i, f.Name, want[i])
		}
	}
}

func TestProjectsAndLoadAll(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Save(newFeature("beta", "x", 0)); err != nil {
		t.Fatal(err)
	}
	if err := Save(newFeature("alpha", "y", 0)); err != nil {
		t.Fatal(err)
	}
	ps, err := Projects()
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0] != "alpha" {
		t.Errorf("Projects = %v, want sorted [alpha beta]", ps)
	}
	all, err := LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Errorf("LoadAll = %d, want 2", len(all))
	}
}

func TestProjectsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ps, err := Projects()
	if err != nil {
		t.Fatalf("Projects on empty state: %v", err)
	}
	if len(ps) != 0 {
		t.Errorf("Projects = %v, want none", ps)
	}
}

func TestHyprWorkspaceAndKey(t *testing.T) {
	f := newFeature("norules", "small-fixes", 0)
	if got := f.HyprWorkspace(); got != "norules:small-fixes" {
		t.Errorf("HyprWorkspace = %q", got)
	}
	if got := f.Key(); got != "norules/small-fixes" {
		t.Errorf("Key = %q", got)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Remove("norules", "never-existed"); err != nil {
		t.Errorf("Remove of absent feature: %v", err)
	}
}

func TestLogDirAndWorktreePath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	got, err := LogDir("norules", "small-fixes")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "canaveral", "logs", "norules", "small-fixes"); got != want {
		t.Errorf("LogDir = %q, want %q", got, want)
	}

	wt, err := WorktreePath("norules", "small-fixes")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "canaveral", "worktrees", "norules", "small-fixes"); wt != want {
		t.Errorf("WorktreePath = %q, want %q", wt, want)
	}
}

func TestNamespacedFeatureSaveLoadList(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := newFeature("norules", "onboarding/step1", 0)
	if err := Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	g := newFeature("norules", "onboarding/step2", 1)
	if err := Save(g); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// A sibling with a deeper namespace under the same parent.
	h := newFeature("norules", "onboarding/sub/step1", 2)
	if err := Save(h); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load("norules", "onboarding/step1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != "onboarding/step1" {
		t.Errorf("loaded.Name = %q, want onboarding/step1", loaded.Name)
	}

	names, err := List("norules")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"onboarding/step1", "onboarding/step2", "onboarding/sub/step1"}
	if len(names) != len(want) {
		t.Fatalf("List = %v, want (any order) %v", names, want)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("List missing %q, got %v", w, names)
		}
	}
}

func TestNamespacedFeatureDoesNotCollideWithFlatSibling(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// A flat feature literally named "onboarding" and a namespaced one
	// "onboarding/step1" must coexist: the flat one is stored as
	// "onboarding.json", the namespaced one under the "onboarding/"
	// directory, so they never collide on disk (unlike the equivalent git
	// branch names, which do).
	flat := newFeature("norules", "onboarding", 0)
	if err := Save(flat); err != nil {
		t.Fatalf("Save flat: %v", err)
	}
	nested := newFeature("norules", "onboarding/step1", 1)
	if err := Save(nested); err != nil {
		t.Fatalf("Save nested: %v", err)
	}

	if _, err := Load("norules", "onboarding"); err != nil {
		t.Errorf("Load flat: %v", err)
	}
	if _, err := Load("norules", "onboarding/step1"); err != nil {
		t.Errorf("Load nested: %v", err)
	}
}

func TestRemovePrunesEmptyNamespaceDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	f := newFeature("norules", "onboarding/step1", 0)
	if err := Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	nsDir := filepath.Join(base, "canaveral", "features", "norules", "onboarding")
	if _, err := os.Stat(nsDir); err != nil {
		t.Fatalf("namespace dir missing before remove: %v", err)
	}

	if err := Remove("norules", "onboarding/step1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("namespace dir should be pruned once empty, stat err = %v", err)
	}
}

func TestRemoveKeepsNonEmptyNamespaceDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Save(newFeature("norules", "onboarding/step1", 0)); err != nil {
		t.Fatal(err)
	}
	if err := Save(newFeature("norules", "onboarding/step2", 1)); err != nil {
		t.Fatal(err)
	}
	if err := Remove("norules", "onboarding/step1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := Load("norules", "onboarding/step2"); err != nil {
		t.Errorf("sibling should survive: %v", err)
	}
}

func TestWorktreePathInUsesConfiguredRoot(t *testing.T) {
	got, err := WorktreePathIn("/p/norules/worktrees", "norules", "onboarding/step1")
	if err != nil {
		t.Fatal(err)
	}
	// No project directory under a configured root: it is already
	// project-specific.
	if want := "/p/norules/worktrees/onboarding/step1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestWorktreePathInFallsBackToStateDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	got, err := WorktreePathIn("", "norules", "small-fixes")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "canaveral", "worktrees", "norules", "small-fixes"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
