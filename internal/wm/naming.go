package wm

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ByClass indexes clients by the class they mapped with.
func ByClass(cs []Client) map[string]Client {
	m := make(map[string]Client, len(cs))
	for _, c := range cs {
		// Keep the first match so a duplicate never hides the original window.
		if _, ok := m[c.InitialClass]; !ok {
			m[c.InitialClass] = c
		}
	}
	return m
}

// ClassMatches reports whether a window's class satisfies a match_class
// pattern. Both the current and the initial class are tried: an application
// that renames its class after mapping should still be recognisable, and
// which of the two a given toolkit populates is not something the user
// writing the pattern can be expected to know.
func ClassMatches(re *regexp.Regexp, c Client) bool {
	return re.MatchString(c.Class) || re.MatchString(c.InitialClass)
}

// MatchInWorkspace finds a window matching re on the named workspace.
//
// This is how an application that will not carry canaveral's class is
// identified. Its class alone is ambiguous — the user's own browser has the
// same one, and adopting that would move it out from under them — but a
// window of that class on a workspace a single feature owns exclusively can
// only be the one canaveral opened there.
func MatchInWorkspace(cs []Client, re *regexp.Regexp, workspace string) (Client, bool) {
	for _, c := range cs {
		if c.Workspace.Name == workspace && ClassMatches(re, c) {
			return c, true
		}
	}
	return Client{}, false
}

var classSafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// ClassPrefix is the shared prefix of every window class for a workspace.
//
// scope is state.Feature.Key(): "project/feature" for a feature and the bare
// name for a space, which has no project to be prefixed by. Since clean turns
// "/" into "-" either way, a feature's classes are spelled exactly as they
// were when this took the two halves separately — an already-open window is
// still recognised as its own across the change.
func ClassPrefix(scope string) string {
	return fmt.Sprintf("canaveral-%s-", clean(scope))
}

func clean(s string) string {
	return strings.Trim(classSafe.ReplaceAllString(s, "-"), "-")
}

// Class builds the window class for one of a workspace's windows.
func Class(scope, window string) string {
	return ClassPrefix(scope) + clean(window)
}

// RehomeTarget picks where windows stranded on a dying workspace should go:
// the lowest-numbered ordinary workspace already on the same monitor, so they
// stay on the screen the user had them on. Named workspaces are skipped —
// they belong to other features — as is the workspace being torn down.
//
// Returns 0 when the monitor has no ordinary workspace to fall back to, and
// the caller should pick a fresh one instead.
func RehomeTarget(ws []Workspace, monitor, leaving string) int {
	best := 0
	for _, w := range ws {
		if w.Monitor != monitor || w.Name == leaving || w.ID <= 0 {
			continue
		}
		// Ordinary workspaces are the numeric ones; a named workspace is
		// some other feature's.
		if strconv.Itoa(w.ID) != w.Name {
			continue
		}
		if best == 0 || w.ID < best {
			best = w.ID
		}
	}
	return best
}

// IsSelf reports whether c is the window hosting the calling process — i.e.
// whether c.PID is an ancestor of (or is) os.Getpid(). This lets callers that
// are about to close a batch of windows they own recognize and defer closing
// the one they're themselves running in, since closing it can terminate the
// calling process immediately.
func IsSelf(c Client) bool {
	if c.PID <= 0 {
		return false
	}
	pid := os.Getpid()
	for depth := 0; depth < 64 && pid > 1; depth++ {
		if pid == c.PID {
			return true
		}
		ppid, err := parentPID(pid)
		if err != nil {
			return false
		}
		pid = ppid
	}
	return false
}

// parentPID reads the parent PID of pid from /proc, for walking the process
// ancestry chain in IsSelf.
func parentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// Format: pid (comm) state ppid ... — comm may itself contain spaces or
	// parens, so find the last ')' and split from there.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

// Cmdline returns the full command line of pid, arguments joined by spaces.
//
// This is the only reliable answer to "what is that window actually
// running?". A window's recorded command is what canaveral believes it
// spawned, which is not the same thing once anything has drifted, and the
// point of asking is precisely to catch drift.
func Cmdline(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil || len(data) == 0 {
		// Empty rather than missing means a kernel thread, a process that
		// has exited, or one caught mid-exec: it exists but says nothing.
		// Reporting that as an answer would let a caller conclude the
		// process is running something it is not.
		return "", false
	}
	// Arguments are NUL-separated, with a trailing NUL.
	args := strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
	return strings.Join(args, " "), true
}

// validFractions rejects a [layout] that could not describe a row of columns,
// before either backend starts issuing commands for it.
//
// The sum is allowed more drift than manifest.validateDefaultFractions
// allows, because this is not a second opinion on a manifest that has already
// been validated: it is a backstop against a hand-edited state file, and it
// only has to catch what would make a backend do something absurd. Each
// backend renormalises rather than trusting the sum.
func validFractions(fractions []float64) error {
	total := 0.0
	for _, f := range fractions {
		if math.IsNaN(f) || f <= 0 || f >= 1 {
			return fmt.Errorf("invalid column fraction %v", f)
		}
		total += f
	}
	if math.Abs(total-1) > 0.05 {
		return fmt.Errorf("column fractions sum to %.3f, not 1", total)
	}
	return nil
}

// normalise scales fractions to sum to exactly 1, so arithmetic downstream
// does not have to carry the tolerance validFractions allows.
func normalise(fractions []float64) []float64 {
	total := 0.0
	for _, f := range fractions {
		total += f
	}
	out := make([]float64, len(fractions))
	for i, f := range fractions {
		out[i] = f / total
	}
	return out
}
