// Package cli implements the canaveral command line interface.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

// Version, Commit and BuildDate are overridden at build time via -ldflags by
// scripts/build.sh. They exist so an installed binary can say exactly which
// source it came from, which matters when several worktrees each build one.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// versionLine renders the one-line build identity printed by --version.
func versionLine() string {
	s := "canaveral " + Version
	if Commit != "unknown" && !strings.Contains(Version, Commit) {
		s += " (" + Commit + ")"
	}
	if BuildDate != "unknown" {
		s += " built " + BuildDate
	}
	return s
}

type command struct {
	name    string
	summary string
	run     func(context.Context, []string) error
}

func commands() []command {
	return []command{
		{"init", "write a starter canaveral.toml for a project", runInit},
		{"open", "open a feature explicitly (for names clashing with commands)", runOpen},
		{"reset", "bring up whatever is missing for a feature", runReset},
		{"ls", "list features", runLs},
		{"status", "show services, agents, windows and telemetry", runStatus},
		{"rm", "tear a feature down; deletes the branch too once it's merged", runRm},
		{"merge", "rebase and merge the current (or named) feature, then tear it down", runMerge},
		{"attach", "attach a terminal to a feature's agent", runAttach},
		{"logs", "print or follow a service or agent log", runLogs},
		{"path", "print a feature's worktree path", runPath},
		{"exec", "run a command inside a feature's worktree", runExec},
		{"hyprwatch", "react to Hyprland events instead of polling (waybar refresh)", runHyprwatch},
		{"watch", "stream feature/agent state as JSON for a status widget", runWatch},
	}
}

// reserved lists names that cannot be used as bare feature arguments.
func reserved() map[string]bool {
	r := map[string]bool{"help": true, "version": true}
	for _, c := range commands() {
		r[c.name] = true
	}
	return r
}

// Main dispatches a command and returns a process exit code.
//
// A bare word that is not a known command is treated as a feature name, so
// `canaveral small-fixes` opens that feature.
func Main(ctx context.Context, args []string) int {
	if len(args) == 0 {
		usage(os.Stdout)
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return 0
	case "-v", "--version", "version":
		fmt.Println(versionLine())
		return 0
	}

	run := func(fn func(context.Context, []string) error, rest []string) int {
		if err := fn(ctx, rest); err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(os.Stderr, "canaveral: interrupted")
				return 130
			}
			fmt.Fprintf(os.Stderr, "canaveral: %v\n", err)
			return 1
		}
		return 0
	}

	for _, c := range commands() {
		if c.name == args[0] {
			return run(c.run, args[1:])
		}
	}
	if strings.HasPrefix(args[0], "-") {
		fmt.Fprintf(os.Stderr, "canaveral: unknown flag %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
	// Bare feature name.
	return run(runOpen, args)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "canaveral - one workspace per feature")
	fmt.Fprintln(w, "\nUsage:")
	fmt.Fprintln(w, "  canaveral <feature>        create or reconcile a feature, then focus it")
	fmt.Fprintln(w, "  canaveral <command> ...")
	fmt.Fprintln(w, "\nCommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	for _, c := range cs {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nExamples:")
	fmt.Fprintln(w, "  canaveral small-fixes      open the small-fixes feature")
	fmt.Fprintln(w, "  canaveral reset            respawn anything missing")
	fmt.Fprintln(w, "  canaveral ls               list features and their ports")
	fmt.Fprintln(w, "\nRun 'canaveral <command> -h' for command flags.")
}

// parseArgs parses flags that may appear before, after or between positional
// arguments. The standard flag package stops at the first non-flag token, which
// would silently ignore `canaveral attach small-fixes main --url`.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// --- shared output helpers ---

const (
	cReset  = "\033[0m"
	cDim    = "\033[2m"
	cBold   = "\033[1m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
	cYellow = "\033[33m"
	cCyan   = "\033[36m"
)

var useColor = os.Getenv("NO_COLOR") == "" && isTTY(os.Stdout)

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func color(c, s string) string {
	if !useColor {
		return s
	}
	return c + s + cReset
}

// reporter renders feature progress to the terminal.
type reporter struct{}

func (reporter) Step(format string, a ...any) {
	fmt.Printf("%s %s\n", color(cCyan, "::"), fmt.Sprintf(format, a...))
}

func (reporter) OK(format string, a ...any) {
	fmt.Printf("%s %s\n", color(cGreen, " ok"), fmt.Sprintf(format, a...))
}

func (reporter) Warn(format string, a ...any) {
	fmt.Printf("%s %s\n", color(cYellow, " ! "), fmt.Sprintf(format, a...))
}

func (reporter) Info(format string, a ...any) {
	fmt.Printf("    %s\n", color(cDim, fmt.Sprintf(format, a...)))
}

func humanBytes(b uint64) string {
	if b == 0 {
		return "-"
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit && exp < 3; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%c", float64(b)/float64(div), "KMGT"[exp])
}

func humanDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func humanAgo(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	if d < time.Second {
		return "just now"
	}
	return humanDuration(d.Truncate(time.Second)) + " ago"
}

func humanCount(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
	}
}

func humanCost(c float64) string {
	if c <= 0 {
		return "-"
	}
	if c < 0.01 {
		return "<$0.01"
	}
	return fmt.Sprintf("$%.2f", c)
}

// oneLine collapses a multi-line error into a single display line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func homeTilde(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !strings.HasPrefix(p, home) {
		return p
	}
	return "~" + p[len(home):]
}

func dim(s string) string { return color(cDim, s) }
