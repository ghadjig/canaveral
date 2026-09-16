package wm

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestClassIsSanitised(t *testing.T) {
	cases := []struct{ scope, window, want string }{
		// A feature's scope is "project/feature", and the slash sanitises to
		// the same hyphen the two halves used to be joined by — so classes
		// are spelled exactly as they were before scope became one string,
		// and a window open across the change is still recognised.
		{"norules/small-fixes", "chrome", "canaveral-norules-small-fixes-chrome"},
		{"nor ules/add/tasks", "server logs", "canaveral-nor-ules-add-tasks-server-logs"},
		// A space has no project, so its scope is the bare name.
		{"3d-printing", "onshape", "canaveral-3d-printing-onshape"},
	}
	for _, c := range cases {
		if got := Class(c.scope, c.window); got != c.want {
			t.Errorf("Class(%q,%q) = %q, want %q", c.scope, c.window, got, c.want)
		}
	}
}

// A class is embedded verbatim in a sway criteria string, so anything the
// criteria parser treats as punctuation would end the criteria early. clean
// already keeps a class to letters, digits, underscore and hyphen; this says
// so where the sway backend can rely on it.
func TestClassIsSafeInASwayCriteria(t *testing.T) {
	class := Class("nor;ules/small,fixes", `we"ird'`)
	if err := swaySafeCriteria(class); err != nil {
		t.Errorf("Class(...) = %q, which sway cannot be given: %v", class, err)
	}
	if got := regexp.QuoteMeta(class); got != class {
		t.Errorf("class %q needs regex escaping (%q); sway matches it as a pattern", class, got)
	}
}

func TestClassPrefixGroupsOnlyOwnFeature(t *testing.T) {
	// The group rule is scoped to one feature so two features open at once are
	// never tabbed into each other.
	p := ClassPrefix("norules/small-fixes")
	if !strings.HasPrefix(Class("norules/small-fixes", "terminal"), p) {
		t.Error("class must carry the feature prefix")
	}
	if strings.HasPrefix(Class("norules/other-feature", "terminal"), p) {
		t.Error("a different feature must not share the prefix")
	}
	// A space must not be swept up by a project whose name it matches.
	if strings.HasPrefix(Class("norules", "terminal"), p) {
		t.Error("a space must not share a feature's prefix")
	}
}

func TestByClassKeepsFirstMatch(t *testing.T) {
	cs := []Client{
		{Address: "0x1", InitialClass: "canaveral-a", Title: "first"},
		{Address: "0x2", InitialClass: "canaveral-a", Title: "duplicate"},
		{Address: "0x3", InitialClass: "canaveral-b"},
	}
	got := ByClass(cs)
	if got["canaveral-a"].Address != "0x1" {
		t.Errorf("duplicate should not shadow the original: %+v", got["canaveral-a"])
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

// The whole point of match_class is that the class alone is ambiguous: the
// user's own copy of the same application carries it too. The workspace is
// what disambiguates, since no two features share one.
func TestMatchInWorkspaceIgnoresTheSameClassElsewhere(t *testing.T) {
	re := regexp.MustCompile("^BambuStudio$")
	cs := []Client{
		{Address: "0xmine", Class: "BambuStudio"},
		{Address: "0xtheirs", Class: "BambuStudio"},
	}
	cs[0].Workspace.Name = "other"
	cs[1].Workspace.Name = "3d"

	got, ok := MatchInWorkspace(cs, re, "3d")
	if !ok {
		t.Fatal("a matching window on the feature's workspace should be found")
	}
	if got.Address != "0xtheirs" {
		t.Errorf("adopted %s, want the window on the feature's own workspace", got.Address)
	}
	if _, ok := MatchInWorkspace(cs, re, "empty"); ok {
		t.Error("no window is on that workspace; nothing should be adopted")
	}
}

func TestClassMatchesEitherClass(t *testing.T) {
	re := regexp.MustCompile("^Google-chrome$")
	if !ClassMatches(re, Client{Class: "Google-chrome", InitialClass: ""}) {
		t.Error("the current class should match")
	}
	if !ClassMatches(re, Client{Class: "", InitialClass: "Google-chrome"}) {
		t.Error("the initial class should match")
	}
	if ClassMatches(re, Client{Class: "chromium", InitialClass: "chromium"}) {
		t.Error("an unrelated class must not match")
	}
}

func TestRehomeTargetPrefersLowestOrdinaryOnSameMonitor(t *testing.T) {
	ws := []Workspace{
		{ID: 5, Name: "5", Monitor: "eDP-1"},
		{ID: 2, Name: "2", Monitor: "eDP-1"},
		{ID: 1, Name: "1", Monitor: "DP-3"},              // other monitor
		{ID: -1337, Name: "norules:x", Monitor: "eDP-1"}, // another feature
		{ID: 9, Name: "norules:dying", Monitor: "eDP-1"}, // the one being torn down
	}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 2 {
		t.Errorf("RehomeTarget = %d, want 2 (lowest ordinary on that monitor)", got)
	}
}

// Hyprland gives a named workspace a large negative id and sway gives it -1,
// so both are excluded by the same ID <= 0 test; a name that merely starts
// with a digit — which sway does report a positive number for — is excluded by
// the name comparison instead.
func TestRehomeTargetSkipsNamedWorkspaces(t *testing.T) {
	ws := []Workspace{
		{ID: -100, Name: "norules:other", Monitor: "eDP-1"},
		{ID: -1, Name: "norules:swayish", Monitor: "eDP-1"},
		{ID: 3, Name: "3d-printing", Monitor: "eDP-1"},
		{ID: 7, Name: "7", Monitor: "eDP-1"},
	}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 7 {
		t.Errorf("RehomeTarget = %d, want 7", got)
	}
}

func TestRehomeTargetNoneAvailable(t *testing.T) {
	ws := []Workspace{{ID: 3, Name: "3", Monitor: "DP-3"}}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 0 {
		t.Errorf("RehomeTarget = %d, want 0 so the caller picks a fresh one", got)
	}
}

func TestIsSelfTrueForTheCallingProcess(t *testing.T) {
	if !IsSelf(Client{PID: os.Getpid()}) {
		t.Error("IsSelf should be true for the calling process's own PID")
	}
}

func TestIsSelfFalseForAnUnrelatedPID(t *testing.T) {
	// A PID that (almost certainly) does not exist, so parentPID fails
	// immediately and IsSelf must not treat that as a match.
	if IsSelf(Client{PID: 999999999}) {
		t.Error("IsSelf should be false for a PID that cannot be our ancestor")
	}
}

func TestIsSelfFalseForANonPositivePID(t *testing.T) {
	if IsSelf(Client{PID: 0}) {
		t.Error("IsSelf should be false for PID 0")
	}
}

func TestCmdlineReadsTheRunningProcess(t *testing.T) {
	got, ok := Cmdline(os.Getpid())
	if !ok {
		t.Fatal("Cmdline(self) = _, false; want the test binary's own command line")
	}
	if !strings.Contains(got, os.Args[0]) {
		t.Errorf("Cmdline(self) = %q, want it to contain %q", got, os.Args[0])
	}
}

// A pid that does not exist, and the pid 0 that Client carries when the
// compositor did not report one, must both be "cannot tell" rather than an
// empty command line a caller could mistake for an answer.
func TestCmdlineReportsWhenItCannotTell(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if got, ok := Cmdline(pid); ok {
			t.Errorf("Cmdline(%d) = %q, true; want _, false", pid, got)
		}
	}
}

func TestValidFractionsRejectsNonsense(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		ok   bool
	}{
		{"even split", []float64{0.5, 0.5}, true},
		{"within tolerance", []float64{0.6, 0.37}, true},
		{"does not sum to one", []float64{0.5, 0.2}, false},
		{"a whole column of nothing", []float64{0, 1}, false},
		{"a column wider than the screen", []float64{1.5, -0.5}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := validFractions(c.in); (err == nil) != c.ok {
				t.Errorf("validFractions(%v) = %v, want ok=%v", c.in, err, c.ok)
			}
		})
	}
}

func TestNormaliseMakesFractionsSumToOne(t *testing.T) {
	got := normalise([]float64{0.6, 0.3})
	total := got[0] + got[1]
	if total < 0.9999 || total > 1.0001 {
		t.Errorf("normalise sums to %v, want 1", total)
	}
	// Proportions are preserved: the first column is still twice the second.
	if ratio := got[0] / got[1]; ratio < 1.999 || ratio > 2.001 {
		t.Errorf("normalise changed the proportions: %v", got)
	}
}
