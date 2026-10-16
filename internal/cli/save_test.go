package cli

import (
	"reflect"
	"testing"

	"github.com/bandito/canaveral/internal/registry"
	"github.com/bandito/canaveral/internal/space"
)

// `new <name>` is how a space is started from the launcher. The line maps
// onto no project, so the completer has to hand the launcher the argv to run
// — completion.run — and LauncherWindow.qml runs exactly that.
func TestLauncherNewSpaceRunsAnEmptyFocusedSpace(t *testing.T) {
	isolateDirs(t)
	c := complete([]string{"new", "3D_Printing"}, true)
	if c.Keyword != "new" {
		t.Fatalf("Keyword = %q, want new", c.Keyword)
	}
	want := []string{"space", "new", "--empty", "--no-edit", "--open", "--focus", "3d-printing"}
	if !reflect.DeepEqual(c.Run, want) {
		t.Errorf("Run = %v, want %v", c.Run, want)
	}
	// The slug is offered as the create candidate, so Enter takes it before
	// running: the launcher must not show one name and create another.
	if len(c.Candidates) != 1 || c.Candidates[0].Kind != candNew || c.Candidates[0].Value != "3d-printing" {
		t.Errorf("candidates = %+v", c.Candidates)
	}

	// Once accepted the name is settled and the line still runs.
	c = complete([]string{"new", "3d-printing", ""}, true)
	if !reflect.DeepEqual(c.Run, want) {
		t.Errorf("after accepting, Run = %v, want %v", c.Run, want)
	}
}

func TestLauncherNewWithoutANameRunsNothing(t *testing.T) {
	isolateDirs(t)
	for _, words := range [][]string{{"new"}, {"new", ""}} {
		c := complete(words, true)
		if c.Keyword != "new" || c.Run != nil {
			t.Errorf("%q: Keyword=%q Run=%v, want a keyword line that runs nothing yet", words, c.Keyword, c.Run)
		}
	}
}

func TestLauncherNewRefusesAnExistingSpace(t *testing.T) {
	isolateDirs(t)
	if _, err := space.CreateEmpty("notes"); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{"new", "notes"}, true)
	if c.Run != nil || c.Error == "" {
		t.Errorf("Run=%v Error=%q, want it refused in-band", c.Run, c.Error)
	}
}

func TestLauncherNewRefusesACommandName(t *testing.T) {
	isolateDirs(t)
	c := complete([]string{"new", "status"}, true)
	if c.Run != nil || c.Error == "" {
		t.Errorf("Run=%v Error=%q, want a command name refused", c.Run, c.Error)
	}
}

func TestLauncherSave(t *testing.T) {
	isolateDirs(t)
	if _, err := space.CreateEmpty("notes"); err != nil {
		t.Fatal(err)
	}
	// On its own it saves whichever space is focused.
	c := complete([]string{"save"}, true)
	if c.Keyword != "save" || !reflect.DeepEqual(c.Run, []string{"save"}) {
		t.Errorf("Keyword=%q Run=%v", c.Keyword, c.Run)
	}
	c = complete([]string{"save", "no"}, true)
	if len(c.Candidates) != 1 || c.Candidates[0].Value != "notes" {
		t.Errorf("candidates = %+v, want the space offered", c.Candidates)
	}
	if !reflect.DeepEqual(c.Run, []string{"save", "no"}) {
		t.Errorf("Run = %v", c.Run)
	}
}

func TestLauncherOffersKeywordsAsFirstWords(t *testing.T) {
	isolateDirs(t)
	for _, prefix := range []string{"n", "s"} {
		seen := map[string]bool{}
		for _, cand := range complete([]string{prefix}, true).Candidates {
			seen[cand.Value] = true
		}
		if !seen["new"] && !seen["save"] {
			t.Errorf("%q: no keyword offered: %v", prefix, seen)
		}
	}
	// The empty line is left to what you were working on.
	for _, cand := range complete([]string{""}, true).Candidates {
		if cand.Value == "new" || cand.Value == "save" {
			t.Errorf("keyword %q offered on an empty line", cand.Value)
		}
	}
}

func TestCurrentSpaceFromEnv(t *testing.T) {
	isolateDirs(t)
	// Not a space: a project feature's terminal must not be mistaken for one.
	t.Setenv("CANAVERAL_PROJECT", "norules")
	t.Setenv("CANAVERAL_FEATURE", "small-fixes")
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "") // keep the workspace fallback out of it
	if name, err := currentSpace(t.Context()); err == nil {
		t.Errorf("resolved %q from a project feature's environment", name)
	}
}

// A project that happens to be called "new" predates the keyword and must
// keep meaning what it did.
func TestLauncherProjectOutranksKeyword(t *testing.T) {
	isolateDirs(t)
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "new")
	if _, err := registry.Add(root); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{"new", "rm", ""}, true)
	if c.Keyword != "" || c.Project != "new" {
		t.Errorf("Keyword=%q Project=%q, want the project", c.Keyword, c.Project)
	}
}
