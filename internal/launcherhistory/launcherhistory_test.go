package launcherhistory

import "testing"

func TestRecordBumpsARepeatedLineToTheFront(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Record("norules rm my-feature"); err != nil {
		t.Fatal(err)
	}
	if err := Record("norules ls"); err != nil {
		t.Fatal(err)
	}
	// Typed again: this should move back to the front rather than appear
	// twice.
	if err := Record("norules rm my-feature"); err != nil {
		t.Fatal(err)
	}

	entries, err := Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want exactly one entry per distinct line", entries)
	}
	if entries[0].Line != "norules rm my-feature" {
		t.Errorf("entries[0] = %q, want the re-typed line first", entries[0].Line)
	}
}

func TestRecordIgnoresBlankLines(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := Record("   "); err != nil {
		t.Fatal(err)
	}
	entries, err := Recent(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %v, want none recorded for a blank line", entries)
	}
}

func TestRecentCapsAtTheRequestedLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, line := range []string{"a", "b", "c", "d"} {
		if err := Record(line); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Recent(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %v, want exactly 2", entries)
	}
	if entries[0].Line != "d" || entries[1].Line != "c" {
		t.Errorf("entries = %v, want the two most recent, most recent first", entries)
	}
}
