package state

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func newFeature(project, name string, slot int) *Feature {
	return &Feature{
		Project:  project,
		Name:     name,
		Root:     "/w/" + project,
		Slot:     slot,
		Branch:   name,
		Worktree: "/wt/" + project + "/" + name,
		Ports:    map[string]int{"web": 3000 + slot},
		// One window, so this is an ordinary feature rather than a headless
		// one. Slot allocation turns on exactly this: a feature with no
		// windows is a background worker and never takes a number, so a
		// fixture without one would silently opt every test out of the
		// behaviour it means to exercise.
		Windows:   []Window{{Name: "term", Class: "canaveral-" + project + "-" + name}},
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

func TestEnsureWSlotsIsStableAcrossProjects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Deliberately not in alphabetical order: the widget slot must follow
	// creation, not the name, otherwise adding a feature that sorts early
	// renumbers everything after it.
	base := time.Now().Add(-time.Hour)
	mk := func(project, name string, age time.Duration) {
		f := newFeature(project, name, 0)
		f.CreatedAt = base.Add(age)
		if err := Save(f); err != nil {
			t.Fatalf("Save %s/%s: %v", project, name, err)
		}
	}
	mk("norules", "zeta", 0)
	mk("alpha", "yankee", time.Minute)

	got, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	if len(got) != 2 || got[0].Name != "zeta" || got[0].WSlot != 1 || got[1].WSlot != 2 {
		t.Fatalf("slots not assigned by creation time: %+v", got)
	}

	// Both projects allocate from one global sequence, unlike Slot.
	if got[0].Project == got[1].Project {
		t.Fatal("test setup: expected two projects")
	}

	// A new feature that sorts first alphabetically must take the next free
	// number, not slot 1.
	mk("alpha", "aaa", 2*time.Minute)
	got, err = EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	for _, f := range got {
		want := map[string]int{"zeta": 1, "yankee": 2, "aaa": 3}[f.Name]
		if f.WSlot != want {
			t.Errorf("%s has slot %d, want %d", f.Name, f.WSlot, want)
		}
	}

	// Removing a feature frees its number for the next one, keeping the list
	// dense enough for a fixed row of widgets.
	if err := Remove("norules", "zeta"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	mk("norules", "later", 3*time.Minute)
	got, err = EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	for _, f := range got {
		want := map[string]int{"yankee": 2, "aaa": 3, "later": 1}[f.Name]
		if f.WSlot != want {
			t.Errorf("after reuse %s has slot %d, want %d", f.Name, f.WSlot, want)
		}
	}
}

func TestEnsureWSlotsSkipsHeadlessFeatures(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	base := time.Now().Add(-time.Hour)
	mk := func(name string, age time.Duration, headless bool) {
		f := newFeature("norules", name, 0)
		f.CreatedAt = base.Add(age)
		if headless {
			f.Windows = nil
		}
		if err := Save(f); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}
	// The worker is created FIRST, so if headless features took part in
	// allocation at all it would hold slot 1 and push both real features up.
	mk("worker", 0, true)
	mk("real-one", time.Minute, false)
	mk("real-two", 2*time.Minute, false)

	got, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	want := map[string]int{"worker": 0, "real-one": 1, "real-two": 2}
	for _, f := range got {
		if f.WSlot != want[f.Name] {
			t.Errorf("%s has slot %d, want %d", f.Name, f.WSlot, want[f.Name])
		}
	}
	// Headless sorts last, so a widget rendering this list in order does not
	// lead with an unnumbered row.
	if got[len(got)-1].Name != "worker" {
		t.Errorf("headless feature should sort last, got order %v", names(got))
	}
}

func TestEnsureWSlotsReleasesSlotWhenFeatureBecomesHeadless(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := newFeature("norules", "was-real", 0)
	if err := Save(f); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWSlots(); err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	got, err := Load("norules", "was-real")
	if err != nil {
		t.Fatal(err)
	}
	if got.WSlot != 1 {
		t.Fatalf("setup: want slot 1, got %d", got.WSlot)
	}

	// Rebuilt without windows. The number it was holding has to come back,
	// and come back ON DISK — an in-memory zero would be re-read as a live
	// claim by the next process to load the file.
	got.Windows = nil
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWSlots(); err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	reloaded, err := Load("norules", "was-real")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WSlot != 0 {
		t.Errorf("slot not released on disk: got %d, want 0", reloaded.WSlot)
	}

	// And the freed number is available to the next real feature.
	next := newFeature("norules", "fresh", 0)
	if err := Save(next); err != nil {
		t.Fatal(err)
	}
	all, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	for _, f := range all {
		if f.Name == "fresh" && f.WSlot != 1 {
			t.Errorf("freed slot not reused: fresh got %d, want 1", f.WSlot)
		}
	}
}

func names(fs []*Feature) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

// A space is reached by letter, from a sequence of its own, so it must not
// consume a feature's number. Two spaces sharing this pool pushed the next
// feature to slot 5 while 3 and 4 answered to nothing a feature could press.
func TestEnsureWSlotsSkipsSpaces(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	base := time.Now().Add(-time.Hour)
	mk := func(project, name string, age time.Duration, space bool) {
		f := newFeature(project, name, 0)
		f.CreatedAt = base.Add(age)
		f.Space = space
		if err := Save(f); err != nil {
			t.Fatalf("Save %s: %v", name, err)
		}
	}
	// Both spaces are created FIRST and are not headless — they have windows,
	// which is the whole point: nothing but Space itself can keep them out of
	// the sequence. If they took part, the features below would be 3 and 4.
	mk("3d", "3d", 0, true)
	mk("audio", "audio", time.Minute, true)
	mk("norules", "real-one", 2*time.Minute, false)
	mk("norules", "real-two", 3*time.Minute, false)

	got, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	want := map[string]int{"3d": 0, "audio": 0, "real-one": 1, "real-two": 2}
	for _, f := range got {
		if f.WSlot != want[f.Name] {
			t.Errorf("%s has slot %d, want %d", f.Name, f.WSlot, want[f.Name])
		}
	}
}

// A space carrying a number allocated before spaces left the sequence has to
// give it back, and give it back ON DISK — an in-memory zero would be re-read
// as a live claim by the next process to load the file, and the slot would
// stay unreachable.
func TestEnsureWSlotsReleasesASpacesExistingSlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	held := newFeature("3d", "3d", 0)
	held.Space = true
	held.WSlot = 1
	if err := Save(held); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureWSlots(); err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	reloaded, err := Load("3d", "3d")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.WSlot != 0 {
		t.Errorf("space slot not released on disk: got %d, want 0", reloaded.WSlot)
	}

	// And the number it was sitting on goes to a real feature.
	next := newFeature("norules", "fresh", 0)
	if err := Save(next); err != nil {
		t.Fatal(err)
	}
	all, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	for _, f := range all {
		if f.Name == "fresh" && f.WSlot != 1 {
			t.Errorf("released slot not reused: fresh got %d, want 1", f.WSlot)
		}
	}
}

func TestEnsureWSlotsReassignsDuplicates(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, n := range []string{"one", "two"} {
		f := newFeature("norules", n, 0)
		f.WSlot = 1 // hand-edited state, or a bug: both claim slot 1
		if err := Save(f); err != nil {
			t.Fatal(err)
		}
	}
	got, err := EnsureWSlots()
	if err != nil {
		t.Fatalf("EnsureWSlots: %v", err)
	}
	if got[0].WSlot == got[1].WSlot {
		t.Fatalf("duplicate slots survived: %d and %d", got[0].WSlot, got[1].WSlot)
	}
}

func TestSetDiscoveredMergesIntoPorts(t *testing.T) {
	f := &Feature{Ports: map[string]int{"admin": 4001}}
	f.SetDiscovered(map[string]int{"web": 3050})

	// One map for consumers, regardless of where each port came from.
	if f.Ports["web"] != 3050 {
		t.Errorf("Ports[web] = %d, want 3050", f.Ports["web"])
	}
	if f.Ports["admin"] != 4001 {
		t.Errorf("Ports[admin] = %d, want 4001 — allocation must survive", f.Ports["admin"])
	}
	if f.Discovered["web"] != 3050 {
		t.Errorf("Discovered[web] = %d, want 3050", f.Discovered["web"])
	}
	if _, ok := f.Discovered["admin"]; ok {
		t.Error("an allocated port leaked into Discovered")
	}
}

func TestSetDiscoveredOnNilMaps(t *testing.T) {
	f := &Feature{}
	f.SetDiscovered(map[string]int{"web": 3050})
	if f.Ports["web"] != 3050 || f.Discovered["web"] != 3050 {
		t.Errorf("Ports=%v Discovered=%v, want web=3050 in both", f.Ports, f.Discovered)
	}
}

func TestSetDiscoveredEmptyIsNoop(t *testing.T) {
	f := &Feature{}
	f.SetDiscovered(nil)
	if f.Discovered != nil {
		t.Errorf("Discovered = %v, want nil", f.Discovered)
	}
}

// The whole point of keeping Discovered separate: Ports is recomputed from
// the manifest on every load, and a discovered value has no formula to be
// recomputed from. Without the overlay, adopting an already-running service
// would hand out the slot-derived port it is not listening on.
func TestMergeDiscoveredSurvivesPortRecompute(t *testing.T) {
	f := &Feature{
		Ports:      map[string]int{"web": 3050},
		Discovered: map[string]int{"web": 3050},
	}

	f.Ports = map[string]int{"web": 3001} // as portsFor would recompute it
	f.MergeDiscovered()

	if f.Ports["web"] != 3050 {
		t.Errorf("Ports[web] = %d, want 3050 — the discovered port must win", f.Ports["web"])
	}
}

func TestMergeDiscoveredWithNothingDiscovered(t *testing.T) {
	f := &Feature{Ports: map[string]int{"web": 3001}}
	f.MergeDiscovered()
	if f.Ports["web"] != 3001 {
		t.Errorf("Ports[web] = %d, want 3001", f.Ports["web"])
	}
}

func TestForgetDiscovered(t *testing.T) {
	f := &Feature{
		Ports:      map[string]int{"web": 3050, "admin": 4001},
		Discovered: map[string]int{"web": 3050},
	}
	f.ForgetDiscovered([]string{"web"})

	// A restart may bind a different port; the old value must not survive to
	// be handed out if discovery then fails.
	if _, ok := f.Ports["web"]; ok {
		t.Errorf("Ports still holds web = %d", f.Ports["web"])
	}
	if _, ok := f.Discovered["web"]; ok {
		t.Error("Discovered still holds web")
	}
	if f.Ports["admin"] != 4001 {
		t.Errorf("Ports[admin] = %d, want 4001 — untouched", f.Ports["admin"])
	}
}

func TestForgetDiscoveredOnEmptyFeature(t *testing.T) {
	f := &Feature{}
	f.ForgetDiscovered([]string{"web"}) // must not panic on nil maps
}

func TestDiscoveredSurvivesSaveLoad(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &Feature{Project: "p", Name: "f", Slot: 1}
	f.SetDiscovered(map[string]int{"web": 3050})
	if err := Save(f); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load("p", "f")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Discovered["web"] != 3050 {
		t.Errorf("Discovered[web] = %d, want 3050", got.Discovered["web"])
	}
}

// A space has no project, so neither its key nor its Hyprland workspace is
// prefixed by one — "3d-printing:3d-printing" would be a colon separating a
// thing from itself.
func TestSpaceIdentityDropsTheProjectPrefix(t *testing.T) {
	f := &Feature{Project: "norules", Name: "small-fixes"}
	if got, want := f.Key(), "norules/small-fixes"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if got, want := f.HyprWorkspace(), "norules:small-fixes"; got != want {
		t.Errorf("HyprWorkspace() = %q, want %q", got, want)
	}

	s := &Feature{Project: "3d-printing", Name: "3d-printing", Space: true}
	if got, want := s.Key(), "3d-printing"; got != want {
		t.Errorf("space Key() = %q, want %q", got, want)
	}
	if got, want := s.HyprWorkspace(), "3d-printing"; got != want {
		t.Errorf("space HyprWorkspace() = %q, want %q", got, want)
	}
}

// deadPID returns a pid that is certainly not running: a child that has been
// started and reaped. Recycling could in principle hand it to something else,
// but not within the microseconds this test needs it for.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

// The bug this exists for: a boot that got all the way to its last window,
// then had its process killed between the final save and the clear. Every
// window was up and the agent was answering questions, and the row above them
// still read "booting, window terminal, 3 of 4" — for the ten minutes it took
// the staleness bound to expire. Nobody was advancing that phase and the
// record said who would have been, so there was no need to guess.
func TestInPhaseDisbelievesAPhaseWhoseProcessIsGone(t *testing.T) {
	f := &Feature{
		Phase:      PhaseBooting,
		PhaseLabel: "window terminal",
		PhaseSince: time.Now(),
		PhasePID:   deadPID(t),
	}
	if f.InPhase() {
		t.Error("a phase whose owner has exited must not be believed")
	}
}

func TestInPhaseBelievesAPhaseThisProcessOwns(t *testing.T) {
	f := &Feature{Phase: PhaseBooting, PhaseSince: time.Now(), PhasePID: os.Getpid()}
	if !f.InPhase() {
		t.Error("a fresh phase owned by a live process must be believed")
	}
}

// Records written before PhasePID existed have nobody to ask about, so the
// staleness bound has to keep working on its own for them.
func TestInPhaseFallsBackToTheStalenessBoundWithoutAPID(t *testing.T) {
	fresh := &Feature{Phase: PhaseBooting, PhaseSince: time.Now()}
	if !fresh.InPhase() {
		t.Error("a fresh phase with no pid must still be believed")
	}
	old := &Feature{Phase: PhaseBooting, PhaseSince: time.Now().Add(-2 * StalePhaseAfter)}
	if old.InPhase() {
		t.Error("a phase past the staleness bound must not be believed, pid or not")
	}
}

// The bound is a backstop for a recycled pid, so it has to outrank a live
// one rather than the other way round.
func TestInPhaseDisbelievesAStalePhaseEvenWithALivePID(t *testing.T) {
	f := &Feature{
		Phase:      PhaseBooting,
		PhaseSince: time.Now().Add(-2 * StalePhaseAfter),
		PhasePID:   os.Getpid(),
	}
	if f.InPhase() {
		t.Error("a stale phase must not be rescued by a pid that happens to be alive")
	}
}

// The staleness bound used to be measured from the start of the step, which
// made it a cap on how long any single step could take: yogurt's devspace
// service allows its readiness probe thirty minutes and regularly needs ten,
// so a boot that was going perfectly well lost its progress bar a third of
// the way through. A step that keeps saying it is there must keep being
// believed.
func TestInPhaseBelievesASlowStepThatKeepsBeating(t *testing.T) {
	f := &Feature{
		Phase:      PhaseBooting,
		PhaseLabel: "service devspace",
		PhaseSince: time.Now().Add(-2 * StalePhaseAfter),
		PhaseBeat:  time.Now(),
		PhasePID:   os.Getpid(),
	}
	if !f.InPhase() {
		t.Error("a long step with a recent heartbeat must be believed")
	}
}

// The heartbeat is a claim about the owner, not about the step, so it cannot
// rescue a phase whose owner stopped making it.
func TestInPhaseDisbelievesAPhaseWhoseHeartbeatStopped(t *testing.T) {
	f := &Feature{
		Phase:      PhaseBooting,
		PhaseSince: time.Now().Add(-3 * StalePhaseAfter),
		PhaseBeat:  time.Now().Add(-2 * StalePhaseAfter),
		PhasePID:   os.Getpid(),
	}
	if f.InPhase() {
		t.Error("a heartbeat that has itself gone stale must not keep the phase alive")
	}
}

func TestBeatPhaseRefreshesLivenessWithoutMovingTheStepsClock(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &Feature{Project: "p", Name: "f"}
	if err := f.SetPhase(PhaseBooting, "service devspace", 2, 8); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	since := f.PhaseSince

	time.Sleep(2 * time.Millisecond)
	if err := f.BeatPhase(PhaseNote{Detail: "log +4.0K"}); err != nil {
		t.Fatalf("BeatPhase: %v", err)
	}

	if !f.PhaseSince.Equal(since) {
		t.Error("PhaseSince moved; a reader renders it as how long this step has been going")
	}
	if !f.PhaseBeat.After(since) {
		t.Error("PhaseBeat did not advance, so the phase still ages out mid-step")
	}
	// Durable, not just in memory: the process reading it is a different one.
	got, err := Load("p", "f")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PhaseBeat.Before(got.PhaseSince) {
		t.Errorf("persisted PhaseBeat = %v, older than PhaseSince %v", got.PhaseBeat, got.PhaseSince)
	}
}

// Nothing to keep alive outside a phase, and writing the record anyway would
// resurrect one that ClearPhase had just settled.
func TestBeatPhaseIsANoOpOutsideAPhase(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &Feature{Project: "p", Name: "f"}
	if err := f.BeatPhase(PhaseNote{Detail: "log +4.0K"}); err != nil {
		t.Fatalf("BeatPhase: %v", err)
	}
	if !f.PhaseBeat.IsZero() {
		t.Error("BeatPhase recorded a heartbeat for a feature that is not in a phase")
	}
}

func TestClearPhaseClearsTheHeartbeatToo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &Feature{Project: "p", Name: "f"}
	if err := f.SetPhase(PhaseBooting, "service devspace", 2, 8); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if err := f.BeatPhase(PhaseNote{Detail: "log +4.0K"}); err != nil {
		t.Fatalf("BeatPhase: %v", err)
	}
	if err := f.ClearPhase(); err != nil {
		t.Fatalf("ClearPhase: %v", err)
	}
	if !f.PhaseBeat.IsZero() {
		t.Error("PhaseBeat survived ClearPhase")
	}
	if f.PhaseDetail != "" {
		t.Errorf("PhaseDetail = %q, want it cleared with the phase", f.PhaseDetail)
	}
}

func TestBeatPhaseCarriesTheDetailToReadersThatCannotSeeIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &Feature{Project: "p", Name: "f"}
	if err := f.SetPhase(PhaseBooting, "service devspace", 2, 8); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if err := f.BeatPhase(PhaseNote{Detail: "log +18.4K"}); err != nil {
		t.Fatalf("BeatPhase: %v", err)
	}

	// The status bar is a different process; in memory is not good enough.
	got, err := Load("p", "f")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.PhaseDetail != "log +18.4K" {
		t.Errorf("persisted PhaseDetail = %q, want %q", got.PhaseDetail, "log +18.4K")
	}
}

// A detail describes the step that produced it. Carried into the next step it
// would be a confident statement about the wrong thing.
func TestSetPhaseDropsThePreviousStepsDetail(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &Feature{Project: "p", Name: "f"}
	if err := f.SetPhase(PhaseBooting, "service devspace", 2, 8); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if err := f.BeatPhase(PhaseNote{Detail: "log quiet for 4m0s"}); err != nil {
		t.Fatalf("BeatPhase: %v", err)
	}
	if err := f.SetPhase(PhaseBooting, "agent main", 3, 8); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if f.PhaseDetail != "" {
		t.Errorf("PhaseDetail = %q, want the new step to start with nothing said about it", f.PhaseDetail)
	}
}

func TestSetPhaseRecordsTheOwnerAndClearPhaseForgetsIt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &Feature{Project: "p", Name: "f"}

	if err := f.SetPhase(PhaseBooting, "worktree", 0, 3); err != nil {
		t.Fatalf("SetPhase: %v", err)
	}
	if f.PhasePID != os.Getpid() {
		t.Errorf("PhasePID = %d, want this process (%d)", f.PhasePID, os.Getpid())
	}

	if err := f.ClearPhase(); err != nil {
		t.Fatalf("ClearPhase: %v", err)
	}
	if f.PhasePID != 0 {
		t.Errorf("PhasePID = %d, want it cleared with the rest of the phase", f.PhasePID)
	}
}
