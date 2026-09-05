package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/launcherhistory"
	"github.com/bandito/canaveral/internal/registry"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
)

// completeProject writes a checkout with the given features already recorded,
// and returns its root. Tests address it by path rather than by registry name,
// which exercises resolveProject's path fallback at the same time.
func completeProject(t *testing.T, name string, features ...string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name = \"" + name + "\"\n\n[[service]]\nname = \"web\"\ncmd = \"bin/dev\"\n\n[[agent]]\nname = \"main\"\ntool = \"opencode\"\n"
	if err := os.WriteFile(filepath.Join(root, "canaveral.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, f := range features {
		if err := state.Save(&state.Feature{Project: name, Name: f, Root: root, Branch: f}); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func values(c completion) []string {
	out := make([]string, 0, len(c.Candidates))
	for _, cand := range c.Candidates {
		out = append(out, cand.Value)
	}
	return out
}

func kinds(c completion) map[string]string {
	out := map[string]string{}
	for _, cand := range c.Candidates {
		out[cand.Value] = cand.Kind
	}
	return out
}

func TestCompleteFirstWordOffersCommandsAndExistingFeaturesOnly(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "small-fixes")

	c := complete([]string{root, "r"}, true)
	got := kinds(c)
	for _, want := range []string{"rm", "reset", "restart"} {
		if got[want] != candCommand {
			t.Errorf("%q missing from candidates %v", want, values(c))
		}
	}
	// Bare dispatch only opens what already exists. Offering to create here
	// would put a worktree, a branch, a server and an agent one keystroke away
	// from a mistyped command — the exact thing `canaveral new` exists to stop.
	for _, cand := range c.Candidates {
		if cand.Kind == candNew {
			t.Errorf("first word offered to create %q; creating is `new`'s job", cand.Value)
		}
	}

	// Existing features are still offered, since opening one is what a bare
	// name means.
	c = complete([]string{root, "small"}, true)
	if kinds(c)["small-fixes"] != candFeature {
		t.Errorf("existing feature missing from %v", values(c))
	}
}

func TestCompleteNewOffersCreationAndNotExistingFeatures(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "small-fixes", "workflows/one")

	c := complete([]string{root, "new", "small"}, true)
	got := kinds(c)
	if got["small"] != candNew {
		t.Errorf("no create candidate in %v", values(c))
	}
	// `new` refuses a name that already exists, so listing existing features
	// would be offering an error.
	if got["small-fixes"] != "" {
		t.Errorf("existing feature offered to `new`: %v", values(c))
	}

	// Namespaces stay: creating inside an existing one is ordinary.
	c = complete([]string{root, "new", ""}, true)
	if kinds(c)["workflows/"] != candNamespace {
		t.Errorf("namespace missing from `new` candidates %v", values(c))
	}
	for _, cand := range c.Candidates {
		if cand.Kind == candFeature {
			t.Errorf("existing feature %q offered to `new`", cand.Value)
		}
	}
}

func TestCompleteNewOffersNamespacesWithNoFeaturesLeft(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")

	// A namespace outlives the features that were in it: its SKILL.md and
	// recorded sessions are exactly what the next feature under it inherits.
	// Deriving the list from live features alone hid the namespace with the
	// most accumulated knowledge behind having to retype it from memory.
	for _, ns := range []string{"onboarding", "leaves"} {
		if _, err := skills.Dir("norules", ns); err != nil {
			t.Fatal(err)
		}
	}

	c := complete([]string{root, "new", ""}, true)
	got := kinds(c)
	for _, want := range []string{"onboarding/", "leaves/"} {
		if got[want] != candNamespace {
			t.Errorf("%q missing from `new` candidates %v", want, values(c))
		}
	}

	// Only `new` creates, so only `new` gains them: an empty namespace has
	// nothing to open or remove, and offering it elsewhere is offering an
	// error.
	if v := values(complete([]string{root, "rm", ""}, true)); len(v) != 0 {
		t.Errorf("rm candidates = %v, want none; an empty namespace holds nothing to remove", v)
	}
}

func TestCompleteNewNarrowsIntoAFeaturelessNamespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")

	if _, err := skills.Dir("norules", "onboarding/deep"); err != nil {
		t.Fatal(err)
	}

	// One segment at a time, the same as feature paths: "onboarding" is an
	// intermediate directory with no skill of its own, but it is still the
	// next segment to type.
	c := complete([]string{root, "new", ""}, true)
	if kinds(c)["onboarding/"] != candNamespace {
		t.Fatalf("candidates = %v, want the first segment", values(c))
	}
	c = complete([]string{root, "new", "onboarding/"}, true)
	if kinds(c)["onboarding/deep/"] != candNamespace {
		t.Fatalf("candidates = %v, want the nested namespace", values(c))
	}
	// Slug drops empty segments, so a bare separator slugs back to the
	// namespace itself. Offering that would answer "create inside onboarding"
	// with "create a feature called onboarding".
	for _, cand := range c.Candidates {
		if cand.Kind == candNew {
			t.Errorf("offered to create %q from a bare separator", cand.Value)
		}
	}
	if v := values(complete([]string{root, "new", "onboarding/!!!"}, true)); len(v) != 0 {
		t.Errorf("candidates = %v, want none; that slugs back to the namespace", v)
	}
	// Still creatable, and still slugged.
	c = complete([]string{root, "new", "onboarding/Step One"}, true)
	if kinds(c)["onboarding/step-one"] != candNew {
		t.Errorf("candidates = %v, want the slug it will create inside the namespace", values(c))
	}
}

func TestCompleteNewSlugsTheNameItWillActuallyCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")

	// Showing "My_Feature" and producing "my-feature" would be lying about what
	// Enter does.
	c := complete([]string{root, "new", "My_Feature"}, true)
	if v := values(c); len(v) != 1 || v[0] != "my-feature" {
		t.Fatalf("candidates = %v, want the slug that will be created", v)
	}
}

func TestCompleteFeaturesOneNamespaceSegmentAtATime(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "flat", "workflows/one", "workflows/two")

	// A namespace collapses to a single entry rather than listing everything
	// underneath it, the same way completing a directory path does.
	c := complete([]string{root, "rm", ""}, true)
	got := kinds(c)
	if got["flat"] != candFeature {
		t.Errorf("flat feature missing from %v", values(c))
	}
	if got["workflows/"] != candNamespace {
		t.Errorf("namespace missing from %v", values(c))
	}
	if got["workflows/one"] != "" {
		t.Errorf("namespace contents leaked into the top level: %v", values(c))
	}
	for _, cand := range c.Candidates {
		if cand.Value == "workflows/" && !cand.Continues {
			t.Error("namespace is not marked Continues; accepting it would end the word")
		}
	}

	// Accepting it narrows to its contents.
	c = complete([]string{root, "rm", "workflows/"}, true)
	if len(c.Candidates) != 2 || kinds(c)["workflows/one"] != candFeature {
		t.Fatalf("candidates = %v, want both namespaced features", values(c))
	}
	if c.Common != "workflows/" {
		t.Errorf("common = %q, want the shared prefix of both", c.Common)
	}
}

func TestCompleteMarksDestructiveCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "flat")

	if c := complete([]string{root, "rm", ""}, true); !c.Destructive {
		t.Error("rm not marked destructive; the launcher would run it without confirming")
	}
	if c := complete([]string{root, "ls", ""}, true); c.Destructive {
		t.Error("ls marked destructive")
	}
}

func TestCompleteDoesNotCountFlagsAsPositionalArguments(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "flat")

	// `rm --force <tab>` is still completing rm's first argument. Counting the
	// flag would advance to the second and offer nothing at all.
	c := complete([]string{root, "rm", "--force", ""}, true)
	if kinds(c)["flat"] != candFeature {
		t.Fatalf("candidates = %v, want the feature list", values(c))
	}
}

func TestCompleteOffersFlagsForAWordStartingWithDash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")

	c := complete([]string{root, "rm", "--k"}, true)
	if len(c.Candidates) != 2 {
		t.Fatalf("candidates = %v, want --keep-branch and --keep-worktree", values(c))
	}
	if c.Candidates[0].Kind != candFlag {
		t.Errorf("kind = %q, want %q", c.Candidates[0].Kind, candFlag)
	}
}

func TestCompleteSecondArgumentDependsOnTheCommand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "flat")

	// logs reads either kind of log, so it takes both.
	c := complete([]string{root, "logs", "flat", ""}, true)
	got := kinds(c)
	if got["web"] != candService || got["main"] != candAgent {
		t.Errorf("logs candidates = %v, want the service and the agent", values(c))
	}

	// attach only ever talks to an agent.
	c = complete([]string{root, "attach", "flat", ""}, true)
	if v := values(c); len(v) != 1 || v[0] != "main" {
		t.Errorf("attach candidates = %v, want just the agent", v)
	}
}

func TestCompleteFallsBackToSubstringMatching(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "insurance-claims")

	c := complete([]string{root, "rm", "claims"}, true)
	if v := values(c); len(v) != 1 || v[0] != "insurance-claims" {
		t.Fatalf("candidates = %v, want the substring match", v)
	}
	if !c.Fuzzy {
		t.Error("Fuzzy not set; the caller cannot tell these are guesses rather than completions")
	}
}

func TestCompleteReportsAnUnknownProjectInBand(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")

	// A completer that fails mid-keystroke is useless to a UI, so the problem
	// has to come back as data.
	c := complete([]string{"nope", "rm", ""}, true)
	if c.Error == "" {
		t.Error("no error reported for an unknown project")
	}
	if c.Candidates == nil {
		t.Error("candidates is nil, not an empty list; the JSON would decode as null")
	}
}

func TestCompleteOffersHistoryAfterProjectsAtTheFirstWord(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")
	if _, err := registry.Add(root); err != nil {
		t.Fatal(err)
	}
	if err := launcherhistory.Record("norules rm my-feature"); err != nil {
		t.Fatal(err)
	}

	c := complete([]string{""}, true)
	v := values(c)
	if len(v) != 2 || v[0] != "norules" || v[1] != "norules rm my-feature" {
		t.Fatalf("candidates = %v, want the project first and the history line after it", v)
	}
	if kinds(c)["norules rm my-feature"] != candHistory {
		t.Errorf("history line has kind %q, want %q", kinds(c)["norules rm my-feature"], candHistory)
	}
}

func TestCompleteHistoryVanishesOnceNothingMatchesWhatWasTyped(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")
	if _, err := registry.Add(root); err != nil {
		t.Fatal(err)
	}
	if err := launcherhistory.Record("norules rm my-feature"); err != nil {
		t.Fatal(err)
	}

	// Still typing the project name: the history line matches the same
	// prefix and stays.
	c := complete([]string{"nor"}, true)
	if kinds(c)["norules rm my-feature"] != candHistory {
		t.Errorf("history line dropped too early: %v", values(c))
	}

	// Nothing on offer starts with this, so it should not linger just because
	// it was there a keystroke ago.
	c = complete([]string{"zzz"}, true)
	if len(c.Candidates) != 0 {
		t.Errorf("candidates = %v, want none once nothing matches", values(c))
	}
}

func TestCompletionCoversEveryCommand(t *testing.T) {
	// A command with no entry completes nothing at all, silently. Registering
	// it in commands() is the step everyone remembers; this is the one they
	// forget.
	for _, c := range commands() {
		if _, ok := commandArgs[c.name]; !ok {
			t.Errorf("commandArgs has no entry for %q", c.name)
		}
		if _, ok := commandFlags[c.name]; !ok {
			t.Errorf("commandFlags has no entry for %q (use an empty map if it takes none)", c.name)
		}
	}
}

func TestCommonPrefixNeverShortensWhatWasTyped(t *testing.T) {
	// Callers insert Common unconditionally, so returning less than the user
	// already typed would delete their input.
	got := commonPrefix([]candidate{{Value: "alpha"}, {Value: "beta"}}, "x")
	if got != "x" {
		t.Errorf("commonPrefix = %q, want the typed prefix back", got)
	}
	got = commonPrefix([]candidate{{Value: "reset"}, {Value: "restart"}}, "r")
	if got != "res" {
		t.Errorf("commonPrefix = %q, want %q", got, "res")
	}
	got = commonPrefix(nil, "r")
	if got != "r" {
		t.Errorf("commonPrefix = %q, want %q", got, "r")
	}
}

// stashFeature parks a feature of an already-written completeProject, so
// completion tests can exercise the stash tree without going near systemd.
func stashFeature(t *testing.T, project, root, name string, stashedAt time.Time) {
	t.Helper()
	if err := state.SaveStash(&state.Stash{
		Feature:   &state.Feature{Project: project, Name: name, Root: root, Branch: name},
		StashedAt: stashedAt,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCompletePopOffersStashesNewestFirst(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "running")
	now := time.Now()
	stashFeature(t, "norules", root, "parked-long-ago", now.Add(-2*time.Hour))
	stashFeature(t, "norules", root, "parked-just-now", now)

	c := complete([]string{root, "pop", ""}, true)
	got := values(c)
	if len(got) != 2 {
		t.Fatalf("pop offered %v, want exactly the two stashes", got)
	}
	// Newest first, matching what a bare `canaveral pop` would restore.
	if got[0] != "parked-just-now" {
		t.Errorf("first candidate = %q, want the most recently stashed", got[0])
	}
	if kinds(c)["parked-just-now"] != candStash {
		t.Errorf("stash candidate kind = %q, want %q", kinds(c)["parked-just-now"], candStash)
	}
	// A running feature is not something `pop` can do anything with.
	for _, v := range got {
		if v == "running" {
			t.Error("pop offered an active feature")
		}
	}
}

func TestCompleteFirstWordOffersStashesBecauseBareDispatchRestoresThem(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "running")
	stashFeature(t, "norules", root, "parked", time.Now())

	c := complete([]string{root, "par"}, true)
	if kinds(c)["parked"] != candStash {
		t.Errorf("stashed feature missing from bare dispatch candidates %v", values(c))
	}
}

func TestCompleteNewOffersAStashRatherThanCreatingOverIt(t *testing.T) {
	// `canaveral new <stashed>` restores rather than refusing, so completion
	// has to say "this is parked" instead of "create this feature" — the
	// latter would be describing something that is not going to happen.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")
	stashFeature(t, "norules", root, "parked", time.Now())

	c := complete([]string{root, "new", "parked"}, true)
	got := kinds(c)
	if got["parked"] != candStash {
		t.Errorf("stash missing from %v", values(c))
	}
	for _, cand := range c.Candidates {
		if cand.Kind == candNew {
			t.Errorf("offered to create %q over an existing stash", cand.Value)
		}
	}
}

func TestCompleteRmOffersStashesToo(t *testing.T) {
	// Stashing something must not be a way to make it undeletable.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "running")
	stashFeature(t, "norules", root, "parked", time.Now())

	c := complete([]string{root, "rm", ""}, true)
	got := kinds(c)
	if got["parked"] != candStash {
		t.Errorf("stash missing from rm candidates %v", values(c))
	}
	if got["running"] != candFeature {
		t.Errorf("active feature missing from rm candidates %v", values(c))
	}
}

func TestCompleteStashOffersOnlyActiveFeatures(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules", "running")
	stashFeature(t, "norules", root, "parked", time.Now())

	got := values(complete([]string{root, "stash", ""}, true))
	if len(got) != 1 || got[0] != "running" {
		t.Errorf("stash offered %v, want only the active feature", got)
	}
}

func TestCompleteOffersANamespaceHoldingOnlyStashes(t *testing.T) {
	// A namespace whose features are all parked is still one worth
	// descending into — its shared skill and its stashes are both in there.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	root := completeProject(t, "norules")
	stashFeature(t, "norules", root, "onboarding/step1", time.Now())

	c := complete([]string{root, ""}, true)
	if kinds(c)["onboarding/"] != candNamespace {
		t.Errorf("namespace missing from %v", values(c))
	}
}
