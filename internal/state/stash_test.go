package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newStash(project, name string, slot int, stashedAt time.Time) *Stash {
	return &Stash{
		Feature:   newFeature(project, name, slot),
		StashedAt: stashedAt.Truncate(time.Second),
		Sessions:  map[string]string{"main": "ses_" + name},
	}
}

func TestStashSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	want := newStash("norules", "small-fixes", 2, time.Now())
	if err := SaveStash(want); err != nil {
		t.Fatalf("SaveStash: %v", err)
	}
	got, err := LoadStash("norules", "small-fixes")
	if err != nil {
		t.Fatalf("LoadStash: %v", err)
	}
	if got.Feature.Branch != want.Feature.Branch || got.Feature.Worktree != want.Feature.Worktree {
		t.Errorf("branch/worktree = %q/%q, want %q/%q",
			got.Feature.Branch, got.Feature.Worktree, want.Feature.Branch, want.Feature.Worktree)
	}
	if got.Feature.Slot != 2 {
		t.Errorf("Slot = %d, want the slot it was stashed on recorded for Pop to prefer", got.Feature.Slot)
	}
	if got.Sessions["main"] != "ses_small-fixes" {
		t.Errorf("Sessions[main] = %q, want the recorded session", got.Sessions["main"])
	}
	if !got.StashedAt.Equal(want.StashedAt) {
		t.Errorf("StashedAt = %v, want %v", got.StashedAt, want.StashedAt)
	}
}

func TestLoadStashMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := LoadStash("norules", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("LoadStash = %v, want ErrNotFound", err)
	}
}

// A stash must be invisible to every active-feature enumeration, because that
// is the whole mechanism by which it stops holding a port slot, a widget
// number and a row in `ls`.
func TestStashedFeaturesAreInvisibleToActiveListings(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Save(newFeature("norules", "live", 0)); err != nil {
		t.Fatal(err)
	}
	if err := SaveStash(newStash("norules", "parked", 1, time.Now())); err != nil {
		t.Fatal(err)
	}

	names, err := List("norules")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "live" {
		t.Errorf("List = %v, want only [live]", names)
	}
	all, err := LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Name != "live" {
		t.Errorf("LoadAll returned %d features, want only the live one", len(all))
	}
}

func TestStashReleasesItsSlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	// Slot 0 exists only as a stash, so the next feature created should take
	// it rather than being pushed to 1 — a stash that kept costing a slot
	// would leave a project's ports growing forever.
	if err := SaveStash(newStash("norules", "parked", 0, time.Now())); err != nil {
		t.Fatal(err)
	}
	slot, err := AllocateSlot("norules", "brand-new")
	if err != nil {
		t.Fatal(err)
	}
	if slot != 0 {
		t.Errorf("AllocateSlot = %d, want 0: a stashed feature holds no slot", slot)
	}
}

func TestAllocateSlotPreferringReclaimsAFreeSlot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Save(newFeature("norules", "other", 0)); err != nil {
		t.Fatal(err)
	}
	slot, err := AllocateSlotPreferring("norules", "popped", 3)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 3 {
		t.Errorf("AllocateSlotPreferring = %d, want 3: nothing has taken it, so the ports come back unchanged", slot)
	}
}

func TestAllocateSlotPreferringFallsBackWhenTaken(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i, n := range []string{"a", "b"} {
		if err := Save(newFeature("norules", n, i)); err != nil {
			t.Fatal(err)
		}
	}
	// Slot 1 went to something else while this was parked.
	slot, err := AllocateSlotPreferring("norules", "popped", 1)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 2 {
		t.Errorf("AllocateSlotPreferring = %d, want the lowest free slot 2", slot)
	}
}

func TestAllocateSlotPreferringHonoursAnExistingRecord(t *testing.T) {
	// A feature that is already active keeps its slot, preference or not:
	// ports are stable for a feature's whole life.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := Save(newFeature("norules", "live", 4)); err != nil {
		t.Fatal(err)
	}
	slot, err := AllocateSlotPreferring("norules", "live", 0)
	if err != nil {
		t.Fatal(err)
	}
	if slot != 4 {
		t.Errorf("AllocateSlotPreferring = %d, want the existing slot 4", slot)
	}
}

func TestLoadStashesOrdersNewestFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	if err := SaveStash(newStash("norules", "old", 0, now.Add(-2*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := SaveStash(newStash("norules", "recent", 1, now)); err != nil {
		t.Fatal(err)
	}
	if err := SaveStash(newStash("norules", "middling", 2, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}

	got, err := LoadStashes("norules")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"recent", "middling", "old"}
	if len(got) != len(want) {
		t.Fatalf("LoadStashes returned %d, want %d", len(got), len(want))
	}
	for i, n := range want {
		if got[i].Feature.Name != n {
			t.Errorf("LoadStashes[%d] = %q, want %q — `canaveral pop` with no argument takes the first",
				i, got[i].Feature.Name, n)
		}
	}
}

func TestRemoveStashIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := RemoveStash("norules", "never-existed"); err != nil {
		t.Errorf("RemoveStash on a missing stash = %v, want nil", err)
	}
}

func TestNamespacedStashNestsAndPrunes(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := SaveStash(newStash("norules", "onboarding/step1", 0, time.Now())); err != nil {
		t.Fatal(err)
	}
	names, err := ListStashes("norules")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "onboarding/step1" {
		t.Fatalf("ListStashes = %v, want [onboarding/step1]", names)
	}

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	nsDir := filepath.Join(dir, "stashed", "norules", "onboarding")
	if _, err := os.Stat(nsDir); err != nil {
		t.Fatalf("namespace dir missing: %v", err)
	}
	if err := RemoveStash("norules", "onboarding/step1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nsDir); !os.IsNotExist(err) {
		t.Errorf("namespace dir survived its last stash: %v", err)
	}
}

// The file's location is the truth about which feature it is. A record whose
// embedded name disagrees — hand-edited, or copied from another project —
// must not restore itself somewhere else.
func TestLoadStashTrustsThePathOverTheRecord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := newStash("norules", "correct", 0, time.Now())
	if err := SaveStash(s); err != nil {
		t.Fatal(err)
	}
	s.Feature.Name = "wrong"
	s.Feature.Project = "elsewhere"

	got, err := LoadStash("norules", "correct")
	if err != nil {
		t.Fatal(err)
	}
	if got.Feature.Name != "correct" || got.Feature.Project != "norules" {
		t.Errorf("LoadStash = %s/%s, want norules/correct", got.Feature.Project, got.Feature.Name)
	}
}

func TestLoadAllStashesSpansProjects(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	if err := SaveStash(newStash("alpha", "one", 0, now.Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := SaveStash(newStash("beta", "two", 0, now)); err != nil {
		t.Fatal(err)
	}
	got, err := LoadAllStashes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("LoadAllStashes returned %d, want 2", len(got))
	}
	if got[0].Feature.Project != "beta" {
		t.Errorf("first = %s, want the most recently stashed across every project", got[0].Feature.Project)
	}
}
