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

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/registry"
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
		{"new", "create a feature: worktree, branch, services, agent and windows", runNew},
		{"open", "open an existing feature explicitly (for names clashing with commands)", runOpen},
		{"reset", "bring up whatever is missing for a feature", runReset},
		{"restart", "stop and restart named services of a feature", runRestart},
		{"ls", "list features", runLs},
		{"status", "show services, agents, windows and telemetry", runStatus},
		{"rm", "tear a feature down; deletes the branch too once it's merged", runRm},
		{"prune", "stop leftover units whose feature no longer exists", runPrune},
		{"rebase", "fetch, then rebase the current (or named) feature onto the default branch", runRebase},
		{"merge", "rebase and merge the current (or named) feature, then tear it down", runMerge},
		{"attach", "attach a terminal to a feature's agent", runAttach},
		{"logs", "print or follow a service or agent log", runLogs},
		{"path", "print a feature's worktree path", runPath},
		{"exec", "run a command inside a feature's worktree", runExec},
		{"projects", "list the projects canaveral knows about, and where they live", runProjects},
		{"complete", "list completion candidates for a partial command line", runComplete},
		{"ws-slot", "map a stable slot number to a feature's workspace (for status bars)", runWSSlot},
		{"watch", "stream feature/agent state as JSON for a status widget", runWatch},
	}
}

// commandNames returns every dispatchable command name, sorted.
func commandNames() []string {
	cs := commands()
	out := make([]string, 0, len(cs)+2)
	for _, c := range cs {
		out = append(out, c.name)
	}
	out = append(out, "help", "version")
	sort.Strings(out)
	return out
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
// A bare word that is not a known command is treated as the name of an
// *existing* feature, so `canaveral small-fixes` opens that feature. It never
// creates one — see openFeature for why `canaveral new` earns its keyword.
func Main(ctx context.Context, args []string) int {
	args, err := chdirToProject(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "canaveral: %v\n", err)
		return 1
	}
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

// chdirToProject consumes leading -C/--project flags and moves into the
// project each one names, returning the remaining arguments.
//
// It changes the working directory rather than threading a root through every
// command, because project resolution already funnels through one place
// (manifest.Find walking up from "."), and every command's subprocesses inherit
// the directory for free. The alternative — a root parameter on each of a dozen
// command functions plus everything they spawn — buys nothing a chdir at the
// entrypoint does not.
//
// A name is looked up in the project registry first and treated as a path
// second, so `-C norules` means the registered project even when a directory of
// that name happens to sit in the current one. That precedence is the whole
// point of the flag: it exists to be usable from anywhere, and "anywhere"
// includes directories with confusing names in them.
func chdirToProject(args []string) ([]string, error) {
	for len(args) > 0 {
		var target string
		switch a := args[0]; {
		case a == "-C" || a == "--project":
			if len(args) < 2 {
				return nil, fmt.Errorf("%s needs a project name or path", a)
			}
			target, args = args[1], args[2:]
		case strings.HasPrefix(a, "-C="):
			target, args = strings.TrimPrefix(a, "-C="), args[1:]
		case strings.HasPrefix(a, "--project="):
			target, args = strings.TrimPrefix(a, "--project="), args[1:]
		default:
			return args, nil
		}
		root, err := resolveProject(target)
		if err != nil {
			return nil, err
		}
		if err := os.Chdir(root); err != nil {
			return nil, fmt.Errorf("enter project %s: %w", target, err)
		}
		// Redundant while the chdir holds, but a worktree carries its own copy
		// of the manifest, so leaving a stale root from the surrounding shell
		// in place would let the env fallback in manifest.Find answer for a
		// different project than the one just selected.
		if err := os.Setenv("CANAVERAL_ROOT", root); err != nil {
			return nil, err
		}
	}
	return args, nil
}

// resolveProject turns a -C argument into a project root.
func resolveProject(target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("empty project name")
	}
	p, found, err := registry.Find(target)
	if err != nil {
		return "", err
	}
	if found {
		if !p.Alive() {
			return "", fmt.Errorf("project %s is registered at %s, which no longer exists (run `canaveral projects --prune`)", target, p.Root)
		}
		return p.Root, nil
	}
	if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
		root, findErr := manifest.Find(target)
		if findErr != nil {
			return "", findErr
		}
		return root, nil
	}
	known, _ := registry.Load()
	if len(known) == 0 {
		return "", fmt.Errorf("unknown project %q, and no projects are registered (run `canaveral projects --scan ~/code`)", target)
	}
	names := make([]string, 0, len(known))
	for _, k := range known {
		names = append(names, k.Name)
	}
	return "", fmt.Errorf("unknown project %q (known: %s)", target, strings.Join(names, ", "))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "canaveral - one workspace per feature")
	fmt.Fprintln(w, "\nUsage:")
	fmt.Fprintln(w, "  canaveral new <feature>    create a feature workspace")
	fmt.Fprintln(w, "  canaveral <feature>        reconcile an existing feature, then focus it")
	fmt.Fprintln(w, "  canaveral <command> ...")
	fmt.Fprintln(w, "  canaveral -C <project> ... run a command against a project from anywhere")
	fmt.Fprintln(w, "\nCommands:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	for _, c := range cs {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nExamples:")
	fmt.Fprintln(w, "  canaveral new small-fixes  create the small-fixes feature")
	fmt.Fprintln(w, "  canaveral small-fixes      open the small-fixes feature")
	fmt.Fprintln(w, "  canaveral reset            respawn anything missing")
	fmt.Fprintln(w, "  canaveral ls               list features and their ports")
	fmt.Fprintln(w, "\nRun 'canaveral <command> -h' for command flags.")
}

// nearest returns the candidate closest to s, or "" when none is close enough.
//
// Deliberately timid. A suggestion is printed right next to potentially
// destructive commands, and "rs" sits one edit from both `ls` and `rm` — so
// anything shorter than four characters gets no suggestion at all, and a tie
// between two candidates gets none either.
func nearest(s string, candidates []string) string {
	if len(s) < 4 {
		return ""
	}
	max := 1
	if len(s) > 6 {
		max = 2
	}
	best, bestD, tied := "", max+1, false
	for _, c := range candidates {
		switch d := editDistance(s, c); {
		case d < bestD:
			best, bestD, tied = c, d, false
		case d == bestD:
			tied = true
		}
	}
	if tied {
		return ""
	}
	return best
}

// editDistance is Levenshtein distance over bytes, which is enough for the
// ASCII slugs canaveral deals in.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
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
