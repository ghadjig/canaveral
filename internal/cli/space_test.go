package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/space"
	"github.com/bandito/canaveral/internal/state"
)

// isolateSpaces points both the config and the state directory at scratch
// ones. Both matter: this suite runs inside a live canaveral worktree, where
// the real ones are full of the machine's actual spaces and features.
func isolateSpaces(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestSpaceNamesAreSlugged(t *testing.T) {
	// `canaveral space new "3D Printing"` and `canaveral 3d-printing` have to
	// agree about what was created, or the space is unopenable by the very
	// command its own starter file tells you to run.
	cases := map[string]string{
		"3D Printing": "3d-printing",
		"notes":       "notes",
	}
	for in, want := range cases {
		if got := spaceName(in); got != want {
			t.Errorf("spaceName(%q) = %q, want %q", in, got, want)
		}
	}
}

// The rule: a name that means both things is refused rather than guessed at,
// the same way `canaveral restart` refuses one that is both a service and a
// feature. Whichever it chose, the other would be unreachable by the form
// actually typed.
func TestResolveBareNameRefusesOnlyRealAmbiguity(t *testing.T) {
	isolateSpaces(t)
	m := &manifest.Manifest{Name: "norules"}

	// Neither exists: free for whatever comes next to decide.
	if err := resolveBareName(m, "nothing"); err != nil {
		t.Errorf("refused a name that names nothing: %v", err)
	}

	// A space alone is not ambiguous.
	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	if err := resolveBareName(m, "3d-printing"); err != nil {
		t.Errorf("refused a space with no feature to clash with: %v", err)
	}

	// A feature alone is not ambiguous either.
	if err := state.Save(&state.Feature{Project: "norules", Name: "small-fixes"}); err != nil {
		t.Fatal(err)
	}
	if err := resolveBareName(m, "small-fixes"); err != nil {
		t.Errorf("refused a feature with no space to clash with: %v", err)
	}

	// Both: refused, and the message has to carry the way out of it.
	if err := state.Save(&state.Feature{Project: "norules", Name: "3d-printing"}); err != nil {
		t.Fatal(err)
	}
	err := resolveBareName(m, "3d-printing")
	if err == nil {
		t.Fatal("a name that is both a feature and a space was resolved silently")
	}
	for _, want := range []string{"canaveral open 3d-printing", "canaveral space open 3d-printing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q:\n%s", want, err)
		}
	}
}

// A stashed feature still occupies the name: popping it is what bare dispatch
// would do, so a space must not shadow one.
func TestResolveBareNameCountsAStashedFeature(t *testing.T) {
	isolateSpaces(t)
	m := &manifest.Manifest{Name: "norules"}
	if _, err := space.Create("parked"); err != nil {
		t.Fatal(err)
	}
	if err := state.SaveStash(&state.Stash{
		Feature: &state.Feature{Project: "norules", Name: "parked"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := resolveBareName(m, "parked"); err == nil {
		t.Error("a stashed feature was shadowed by a space of the same name")
	}
}

func TestSpaceRecordOnlyMatchesSpaces(t *testing.T) {
	isolateSpaces(t)
	// A project feature whose name happens to equal its project's must not be
	// mistaken for a space; only the marker says which it is.
	if err := state.Save(&state.Feature{Project: "notes", Name: "notes"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := spaceRecord("notes"); ok {
		t.Error("spaceRecord matched a plain feature")
	}
	if err := state.Save(&state.Feature{Project: "board", Name: "board", Space: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := spaceRecord("board"); !ok {
		t.Error("spaceRecord missed a space")
	}
}

// A space's workspace is its bare name, so the focused-workspace lookup has to
// accept one without a colon — that is how `canaveral path` and the widget
// jump keybinds find the space you are looking at.
func TestSplitWorkspaceName(t *testing.T) {
	cases := []struct {
		ws               string
		project, feature string
		ok               bool
	}{
		{"norules:small-fixes", "norules", "small-fixes", true},
		{"3d-printing", "3d-printing", "3d-printing", true},
		{"", "", "", false},
		{":feature", "", "", false},
		{"project:", "", "", false},
	}
	for _, c := range cases {
		p, f, ok := splitWorkspaceName(c.ws)
		if p != c.project || f != c.feature || ok != c.ok {
			t.Errorf("splitWorkspaceName(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.ws, p, f, ok, c.project, c.feature, c.ok)
		}
	}
}

func TestUnknownSpaceSuggestsANearOne(t *testing.T) {
	isolateSpaces(t)
	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	err := unknownSpace("3d-printng")
	if err == nil || !strings.Contains(err.Error(), "canaveral 3d-printing") {
		t.Errorf("error = %v, want a suggestion of the near miss", err)
	}
}

// The launcher's grammar is `<project> [command] [args...]`, and a space fits
// none of it: it is one word that is itself the whole command. These tests
// pin the wire contract LauncherWindow.qml reads — completion.space is what
// tells it to drop the -C and to treat a one-word line as runnable.
func TestLauncherOffersSpacesAsAFirstWord(t *testing.T) {
	isolateSpaces(t)
	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{"3d"}, true)
	var found bool
	for _, cand := range c.Candidates {
		if cand.Value == "3d-printing" && cand.Kind == candSpace {
			found = true
		}
	}
	if !found {
		t.Fatalf("the launcher cannot see spaces: %+v", c.Candidates)
	}
	// Not yet a complete name, so not yet runnable.
	if c.Space {
		t.Error("Space set on a partial name; Enter would run a half-typed line")
	}
}

func TestLauncherMarksAFullySpelledSpaceAsRunnable(t *testing.T) {
	isolateSpaces(t)
	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	// One word, fully typed, no trailing space: the launcher has to know this
	// line already means something, or Enter does nothing at all.
	c := complete([]string{"3d-printing"}, true)
	if !c.Space || c.Project != "3d-printing" {
		t.Errorf("Space=%v Project=%q, want the line marked runnable", c.Space, c.Project)
	}
	// "open" is what makes the launcher append --focus: going there is the
	// entire intent of opening one from a hotkey.
	if c.Command != "open" {
		t.Errorf("Command = %q, want open", c.Command)
	}
}

// Past the first word only flags can follow, because the name was the verb.
func TestLauncherCompletesFlagsAfterASpace(t *testing.T) {
	isolateSpaces(t)
	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{"3d-printing", ""}, true)
	if !c.Space {
		t.Fatal("Space not set once the space is a completed word")
	}
	var got []string
	for _, cand := range c.Candidates {
		got = append(got, cand.Value)
	}
	if len(got) == 0 || !strings.Contains(strings.Join(got, " "), "--focus") {
		t.Errorf("candidates = %v, want open's flags", got)
	}
	// --base names a git ref to branch from, which a space cannot have.
	for _, g := range got {
		if g == "--base" {
			t.Error("--base offered for a space, which has no branch")
		}
	}
}

// A space must stay reachable when the project registry is unreadable or
// empty — it does not use the registry at all, and the launcher is the main
// way anyone opens one.
func TestLauncherShowsSpacesWithNoProjectsRegistered(t *testing.T) {
	isolateSpaces(t)
	if _, err := space.Create("notes"); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{""}, true)
	if len(c.Candidates) == 0 {
		t.Fatal("no candidates at all with no projects registered")
	}
	if c.Error != "" {
		t.Errorf("Error = %q, want spaces offered regardless", c.Error)
	}
}

// Outside a project, tab completion has only two things it can honestly
// offer: canaveral's own commands, and the spaces reachable from anywhere.
func TestTerminalCompletionOutsideAProjectOffersCommandsAndSpaces(t *testing.T) {
	isolateSpaces(t)
	t.Setenv("CANAVERAL_ROOT", "") // or manifest.Find answers from the env
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })

	if _, err := space.Create("3d-printing"); err != nil {
		t.Fatal(err)
	}
	c := complete([]string{""}, false)
	kinds := map[string]bool{}
	for _, cand := range c.Candidates {
		kinds[cand.Kind] = true
	}
	if !kinds[candSpace] {
		t.Error("no space offered outside a project")
	}
	if !kinds[candCommand] {
		t.Error("no command offered outside a project")
	}
}
