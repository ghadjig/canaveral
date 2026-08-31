package cli

import (
	"flag"
	"testing"
	"time"
)

func TestParseArgsInterspersed(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	url := fs.Bool("url", false, "")
	n := fs.Int("n", 0, "")

	pos, err := parseArgs(fs, []string{"small-fixes", "--url", "main", "-n", "5"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !*url {
		t.Error("--url not parsed after a positional")
	}
	if *n != 5 {
		t.Errorf("-n = %d, want 5", *n)
	}
	if len(pos) != 2 || pos[0] != "small-fixes" || pos[1] != "main" {
		t.Errorf("positionals = %v", pos)
	}
}

func TestReservedCoversEveryCommand(t *testing.T) {
	r := reserved()
	// A feature named after a command would be unreachable by bare dispatch.
	for _, c := range commands() {
		if !r[c.name] {
			t.Errorf("command %q is not reserved", c.name)
		}
	}
	if !r["help"] || !r["version"] {
		t.Error("help/version must be reserved")
	}
	if r["small-fixes"] {
		t.Error("ordinary feature names must not be reserved")
	}
}

func TestVersionLine(t *testing.T) {
	orig := [3]string{Version, Commit, BuildDate}
	defer func() { Version, Commit, BuildDate = orig[0], orig[1], orig[2] }()

	cases := []struct {
		name                      string
		version, commit, buildate string
		want                      string
	}{
		{"unstamped", "dev", "unknown", "unknown", "canaveral dev"},
		{"stamped", "v1.2.0", "abc1234", "2026-01-02T03:04:05Z",
			"canaveral v1.2.0 (abc1234) built 2026-01-02T03:04:05Z"},
		// git describe already ends in the commit, so don't repeat it.
		{"describe", "abc1234-dirty", "abc1234", "2026-01-02T03:04:05Z",
			"canaveral abc1234-dirty built 2026-01-02T03:04:05Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			Version, Commit, BuildDate = c.version, c.commit, c.buildate
			if got := versionLine(); got != c.want {
				t.Errorf("versionLine() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSortedPortNames(t *testing.T) {
	got := sortedPortNames(map[string]int{"web": 1, "api": 2, "vite": 3})
	want := []string{"api", "vite", "web"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("sortedPortNames[%d] = %q, want %q", i, got[i], w)
		}
	}
}

func TestPortSummary(t *testing.T) {
	if got := portSummary(map[string]int{"api": 4000, "web": 3000}); got != "4000,3000" {
		t.Errorf("portSummary = %q", got)
	}
	if got := portSummary(nil); got != "-" {
		t.Errorf("portSummary(nil) = %q, want -", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{0: "-", 512: "512B", 1024: "1.0K", 1048576: "1.0M"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanCount(t *testing.T) {
	cases := map[int64]string{0: "-", 999: "999", 8899: "8.9k", 2500000: "2.50M"}
	for in, want := range cases {
		if got := humanCount(in); got != want {
			t.Errorf("humanCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                           "-",
		250 * time.Millisecond:      "250ms",
		90 * time.Second:            "1m30s",
		2*time.Hour + 5*time.Minute: "2h5m",
	}
	for in, want := range cases {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanCost(t *testing.T) {
	if got := humanCost(0); got != "-" {
		t.Errorf("humanCost(0) = %q", got)
	}
	if got := humanCost(0.001); got != "<$0.01" {
		t.Errorf("humanCost(0.001) = %q", got)
	}
	if got := humanCost(1.5); got != "$1.50" {
		t.Errorf("humanCost(1.5) = %q", got)
	}
}

func TestOneLineCollapsesWhitespace(t *testing.T) {
	if got := oneLine("line one\n\tline  two\n"); got != "line one line two" {
		t.Errorf("oneLine = %q", got)
	}
}

func TestShorten(t *testing.T) {
	if got := shorten("abcdef", 4); got != "abc\u2026" {
		t.Errorf("shorten = %q", got)
	}
	if got := shorten("abc", 10); got != "abc" {
		t.Errorf("shorten should not pad: %q", got)
	}
}

func TestNearestCatchesCommandTypos(t *testing.T) {
	// The motivating case: `canaveral stratus` used to spawn a feature.
	if got := nearest("stratus", commandNames()); got != "status" {
		t.Errorf("nearest(stratus) = %q, want status", got)
	}
	if got := nearest("merg", commandNames()); got != "merge" {
		t.Errorf("nearest(merg) = %q, want merge", got)
	}
}

func TestNearestStaysQuietOnRealFeatureNames(t *testing.T) {
	// A deliberate feature name must not be second-guessed.
	for _, n := range []string{"small-fixes", "onboarding", "fix-dangling"} {
		if got := nearest(n, commandNames()); got != "" {
			t.Errorf("nearest(%q) = %q, want no suggestion", n, got)
		}
	}
}

func TestNearestWillNotConfuseShortDestructiveCommands(t *testing.T) {
	// "ls" and "rm" are one edit apart; suggesting the wrong one is worse
	// than suggesting nothing.
	if got := nearest("rs", commandNames()); got != "" {
		t.Errorf("nearest(rs) = %q, want no suggestion", got)
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"stratus", "status", 1},
		{"kitten", "sitting", 3},
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestNewAndPruneAreRegistered(t *testing.T) {
	// Bare dispatch only opens existing features now, so `new` has to exist
	// for anything to be creatable at all — and being registered is what
	// reserves it against a feature shadowing it.
	have := map[string]bool{}
	for _, c := range commands() {
		have[c.name] = true
	}
	for _, n := range []string{"new", "prune"} {
		if !have[n] {
			t.Errorf("command %q is not registered", n)
		}
	}
}
