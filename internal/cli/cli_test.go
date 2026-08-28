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
