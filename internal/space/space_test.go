package space

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolate points the package at a scratch config directory.
func isolate(t *testing.T) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	// Dir does not create it; the tests that write definitions by hand do.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateThenLoadRoundTrips(t *testing.T) {
	isolate(t)
	path, err := Create("3d-printing")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasSuffix(path, "3d-printing.toml") {
		t.Errorf("path = %q, want it named after the space", path)
	}

	// The starter has to load, or `canaveral space new` hands you a file the
	// very next command refuses — the same trap `canaveral init` fell into.
	m, err := Load("3d-printing")
	if err != nil {
		t.Fatalf("the starter definition does not load: %v", err)
	}
	if !m.Space {
		t.Error("Space = false; nearly every difference in handling hangs off this bit")
	}
	if m.Name != "3d-printing" {
		t.Errorf("Name = %q, want the file's own name", m.Name)
	}
	if m.Branch != "" {
		t.Errorf("Branch = %q, want empty: a space has no branch it could ever have", m.Branch)
	}
	if len(m.Windows) == 0 {
		t.Error("the starter should declare a window; a space with none opens nothing")
	}
}

// The definition is the only copy of something you wrote, and there is no
// branch or worktree left holding a second one.
func TestCreateRefusesToOverwrite(t *testing.T) {
	isolate(t)
	if _, err := Create("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := Create("x"); err == nil {
		t.Fatal("Create overwrote an existing definition")
	}
}

func TestListIsSortedAndSkipsStrays(t *testing.T) {
	dir := isolate(t)
	for _, n := range []string{"zebra", "apple"} {
		if _, err := Create(n); err != nil {
			t.Fatal(err)
		}
	}
	// Neither of these is a space, and neither may appear in the listing.
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "subdir.toml"), 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "apple" || got[1] != "zebra" {
		t.Errorf("List() = %v, want [apple zebra]", got)
	}
}

// Listing must neither fail nor leave anything behind when no space has ever
// been defined. Shell completion calls this on every other keystroke, and a
// config directory canaveral quietly created is a config directory it was
// never asked to create.
func TestListWithNoDirectoryIsEmptyAndCreatesNothing(t *testing.T) {
	base := t.TempDir()
	cfg := filepath.Join(base, "nothing-here")
	t.Setenv("XDG_CONFIG_HOME", cfg)

	got, err := List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List() = %v, want empty", got)
	}
	if _, err := os.Stat(cfg); err == nil {
		t.Error("List created a config directory nobody asked for")
	}
}

// A "/" would nest the definition file and collide with the namespace syntax,
// which a space cannot take part in anyway.
func TestNamesAreFlat(t *testing.T) {
	isolate(t)
	for _, bad := range []string{"", "a/b", "../escape", ".hidden", "-leading"} {
		if _, err := Path(bad); err == nil {
			t.Errorf("Path(%q) accepted a name that is not a single flat word", bad)
		}
	}
	for _, good := range []string{"3d-printing", "notes", "a.b_c-1"} {
		if _, err := Path(good); err != nil {
			t.Errorf("Path(%q) rejected a valid name: %v", good, err)
		}
	}
}

func TestLoadUnknownIsErrNotFound(t *testing.T) {
	isolate(t)
	_, err := Load("nope")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Load of an undefined space = %v, want a not-found error", err)
	}
	if Exists("nope") {
		t.Error("Exists = true for an undefined space")
	}
}

func TestRemove(t *testing.T) {
	isolate(t)
	if _, err := Create("x"); err != nil {
		t.Fatal(err)
	}
	if err := Remove("x"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if Exists("x") {
		t.Error("still there after Remove")
	}
	if err := Remove("x"); err == nil {
		t.Error("Remove of a missing space should say so")
	}
}

// A space's directory defaults to home. It is emphatically not created:
// canaveral makes nothing on disk for a space, which is the entire reason
// removing one can never delete anything.
func TestDirDefaultsToHomeAndIsNotCreated(t *testing.T) {
	dir := isolate(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	if err := os.WriteFile(filepath.Join(dir, "bare.toml"), []byte("[[window]]\nname=\"t\"\nrun=\"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load("bare")
	if err != nil {
		t.Fatal(err)
	}
	if m.Root != home {
		t.Errorf("Root = %q, want %q", m.Root, home)
	}

	body := "dir = \"~/models\"\n[[window]]\nname=\"t\"\nrun=\"\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tilde.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err = Load("tilde")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "models")
	if m.Root != want {
		t.Errorf("Root = %q, want %q", m.Root, want)
	}
	if _, err := os.Stat(want); err == nil {
		t.Error("canaveral created the directory; a space names one you already keep")
	}
}
