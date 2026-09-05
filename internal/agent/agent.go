// Package agent manages the coding agents canaveral runs inside a workspace.
//
// canaveral does not care which agent you use, but the agents differ in more
// than their command line. opencode runs as a headless HTTP server that
// canaveral supervises in its own systemd unit and interrogates over REST;
// Claude Code has no server at all and is only ever the terminal program
// itself, so everything canaveral knows about it comes from the transcripts
// it writes under ~/.claude. A Harness (see harness.go) is the seam between
// those two shapes, and the vocabulary in this file — Health, State, Todos,
// Activity — is what every harness has to answer in, so that `canaveral
// status`, `canaveral watch` and the widgets above them never have to know
// which one is underneath.
package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// resolveBin returns the absolute path to a harness's executable.
//
// Resolving in the parent (rather than relying on the unit's PATH) turns a
// missing toolchain into an immediate, clear error instead of a unit that
// starts and dies.
//
// LookPath alone is not enough: canaveral is often started from a Hyprland
// keybind or a quickshell launcher, which inherit the session's PATH rather
// than a real terminal's. That PATH is missing whatever a shell startup file
// would have added — mise/asdf shims, ~/.local/bin, npm's global bin — which
// is exactly where these tools tend to live. The same trap is documented for
// canaveral's own binary in share/quickshell/LauncherWindow.qml. ShellPATH
// resolves the fuller PATH a real terminal would see, and is tried as a
// fallback before giving up.
func resolveBin(name string) (string, error) {
	if bin, err := exec.LookPath(name); err == nil {
		return filepath.Abs(bin)
	}
	if bin, ok := lookPathIn(name, ShellPATH()); ok {
		return filepath.Abs(bin)
	}
	return "", &NotInstalledError{Bin: name}
}

// NotInstalledError reports that a harness's executable is nowhere on PATH.
type NotInstalledError struct{ Bin string }

func (e *NotInstalledError) Error() string {
	return e.Bin + " not found in PATH (checked a login/interactive shell's PATH too)"
}

// shellPATH caches ShellPATH's result: every caller within a process wants
// the same answer, and computing it spawns a couple of shells, which is not
// free enough to redo for every window, service and agent a single `open`
// reconciles.
var shellPATH struct {
	once  sync.Once
	value string
}

// ShellPATH returns PATH the way a real terminal would see it: the current
// process's PATH, extended with whatever an interactive or a login $SHELL
// adds via its startup files.
//
// Needed generally, not just for the agents: canaveral itself, the systemd
// --user units it starts (see internal/unit's inheritEnv) and the window
// terminals it spawns via hyprctl all begin with whatever PATH launched
// canaveral — a Hyprland keybind or the quickshell launcher hand over the
// compositor session's PATH, not a login shell's, and that is missing
// whatever an rc file would have added. Different setups put that PATH in
// different files — bash only reads ~/.bashrc for an interactive shell,
// unless a profile file explicitly sources it, so both kinds are tried and
// merged rather than picking one.
func ShellPATH() string {
	shellPATH.once.Do(func() {
		shellPATH.value = computeShellPATH()
	})
	return shellPATH.value
}

func computeShellPATH() string {
	p := os.Getenv("PATH")
	for _, flag := range []string{"-ic", "-lc"} {
		p = MergePATH(p, shellPATHVia(flag))
	}
	return p
}

// MergePATH returns base with every directory of extra that base does not
// already contain appended to it, dropping duplicates from either side.
//
// Append-only, never reordering: base is the authoritative list — a
// toolchain's resolved PATH, say — and whatever it puts first must keep
// winning, or a project pinning ruby 3.4.7 would start resolving some other
// ruby the moment a wider PATH were folded in. extra only ever contributes
// directories that were missing entirely.
func MergePATH(base, extra string) string {
	var dirs []string
	seen := map[string]bool{}
	add := func(list string, skipEmpty bool) {
		for _, d := range filepath.SplitList(list) {
			if seen[d] || (skipEmpty && d == "") {
				continue
			}
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
	// An empty entry in base means the current directory and is the
	// caller's business to keep; one arriving from extra is almost always a
	// stray separator, and appending "." to every unit's PATH is not.
	add(base, false)
	add(extra, true)
	return strings.Join(dirs, string(filepath.ListSeparator))
}

// shellPATHVia runs $SHELL with flag (e.g. "-ic" for interactive, "-lc" for
// login) and prints its PATH, or "" if the shell can't be run or times out.
func shellPATHVia(flag string) string {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// -i shells source files that assume a terminal; none of that reads
	// stdin, so leaving it at the default (/dev/null) is enough to avoid a
	// hang, and stderr noise (job control warnings) is simply discarded.
	out, err := exec.CommandContext(ctx, shell, flag, `printf '%s' "$PATH"`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lookPathIn searches path (a PATH-style list) for an executable file named
// name, the way exec.LookPath does but against an explicit list rather than
// the process's own os.Getenv("PATH").
func lookPathIn(name, path string) (string, bool) {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		info, err := os.Stat(p)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return p, true
	}
	return "", false
}

// resetShellPATHCacheForTest clears ShellPATH's cache. Only meant to be
// called between tests that set up different PATH/HOME/SHELL fixtures —
// production code always wants the first, cached answer.
func resetShellPATHCacheForTest() {
	shellPATH = struct {
		once  sync.Once
		value string
	}{}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Tokens is a token usage breakdown for a session.
type Tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// Total returns all tokens attributable to the session.
func (t Tokens) Total() int64 {
	return t.Input + t.Output + t.Reasoning + t.Cache.Read + t.Cache.Write
}

// State classifies what an agent is doing right now: waiting (blocked on a
// question or permission request that only you can answer), retrying (hit a
// provider error and is auto-retrying), busy (actively generating), idle
// (nothing pending).
//
// Waiting deliberately outranks busy. opencode keeps a session's own status
// "busy" while a question or permission request is outstanding — the turn
// hasn't ended, it's blocked inside a tool call — but from your point of
// view the agent has stopped and cannot proceed without you, which is the
// more useful thing to report.
//
// Not every harness can report every state: see Harness.Serves and the
// per-harness notes for which ones a given tool can actually distinguish.
type State string

const (
	StateIdle     State = "idle"
	StateBusy     State = "busy"
	StateWaiting  State = "waiting"
	StateRetrying State = "retrying"
)

// BlockKind distinguishes the two things an agent can be blocked on.
type BlockKind string

const (
	// BlockQuestion is the assistant asking you something via the question
	// tool, with a header, full text and a set of options.
	BlockQuestion BlockKind = "question"
	// BlockPermission is the assistant asking to be allowed to act, e.g.
	// run a command or edit a file outside its worktree.
	BlockPermission BlockKind = "permission"
)

// Pending describes what an agent is waiting on, for display.
type Pending struct {
	Kind BlockKind `json:"kind"`
	// Header is a short label (opencode caps it at 30 characters), which is
	// what a compact widget should show; Detail is the full text.
	Header    string   `json:"header,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Options   []string `json:"options,omitempty"`
	Resources []string `json:"resources,omitempty"`
}

// Todos is the agent's task list for the current session, which both
// opencode and Claude Code maintain as they work through a multi-step job.
//
// This is the only real progress signal either tool exposes: there is no
// percentage or ETA anywhere in the API, but "6 of 9 done, currently on X"
// is genuine, self-reported progress.
type Todos struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	InProgress int `json:"in_progress"`
	Pending    int `json:"pending"`
	Cancelled  int `json:"cancelled"`
	// Current is the content of the task being worked on right now, empty
	// when none is in progress. It is the single most useful line for a
	// compact widget: what is it actually doing.
	Current string `json:"current,omitempty"`
}

// Done reports the fraction of tasks completed, in [0,1]. Cancelled tasks
// count as resolved rather than outstanding, so abandoning work does not
// leave a bar permanently short of full.
func (t Todos) Done() float64 {
	if t.Total == 0 {
		return 0
	}
	return float64(t.Completed+t.Cancelled) / float64(t.Total)
}

// resolved reports whether a task list has nothing left to do. Such a list
// is a finished plan, and neither tool ever clears one, so callers drop it
// once a newer prompt has arrived rather than showing finished work beside
// whatever the agent has moved on to.
func (t Todos) resolved() bool {
	return t.Total > 0 && t.InProgress == 0 && t.Pending == 0
}

// Activity is the tool call an agent is executing right now — the most
// literal answer to "what is it doing": a bash command it is waiting on, a
// file it is editing, a search it is running.
type Activity struct {
	// Tool is the tool's name, e.g. "bash", "edit", "grep".
	Tool string `json:"tool"`
	// Title is the agent's own human-readable description of the call: the
	// command line for bash, the path for a file tool. It can be long and
	// multi-line, so a consumer should expect to truncate it.
	Title string `json:"title,omitempty"`
	// Since is when the call started.
	Since time.Time `json:"since"`
}

// Health summarises what an agent is doing right now.
type Health struct {
	Reachable bool
	// Live reports whether the agent program is actually running.
	//
	// Distinct from Reachable, which says only that canaveral managed to
	// find anything out at all. For a serving harness the two coincide: an
	// opencode server that answers is by definition running. Claude Code
	// leaves its transcripts behind when it exits, so everything below stays
	// readable — and worth showing — after the program itself has gone, and
	// this is the bit that tells the two situations apart.
	Live bool
	// Busy reports that the newest session has an assistant message that has
	// not finished generating. Kept alongside State for callers that only
	// care about the busy/idle distinction.
	Busy bool
	// State is the fuller busy/waiting/idle/retrying classification.
	State State
	// Pending is what the agent is blocked on, set only when State is
	// StateWaiting.
	Pending *Pending
	// Sessions is the number of top-level conversations in the agent's
	// directory. Subagent sessions are excluded and counted separately, so
	// this stays the number of things you actually started.
	Sessions int
	// SubSessions is how many subagent sessions the current conversation
	// has spawned. They are separate sessions sharing the directory, which
	// is why a single conversation can look like four.
	SubSessions int
	Tokens      Tokens
	Cost        float64
	// Model is the model used by the most recent assistant message.
	Model string
	// Variant is the provider-specific reasoning effort ("high", "low", ...)
	// of the most recent assistant message, empty when the provider has no
	// such notion.
	Variant string
	// Provider is the provider serving the model, e.g. "github-copilot".
	Provider string
	// LastError is the error from the most recent assistant turn, if it failed.
	LastError string
	// Updated is when the newest session last changed.
	Updated time.Time
	// SessionID is the ID of the newest session, empty if there are none.
	// Used to fork a namespace sibling's context into a new feature (see
	// internal/feature.forkArgsFor) — the same field Probe already sorts by.
	SessionID string
	// Worked is the sum of (completed − created) across every assistant turn
	// that has finished in the newest session — actual generation time, not
	// wall-clock span, so it excludes the gaps where nothing was happening.
	Worked time.Duration
	// Working is how long the current assistant message has been
	// generating. Note this is not "how long since I asked": one prompt
	// produces many assistant messages, one per tool round trip, so this
	// resets every few seconds. SincePrompt is the human-meaningful timer.
	Working time.Duration
	// SincePrompt is how long it has been since your most recent message —
	// the answer to "how long has it been working on what I asked". Zero
	// when the session has no user message.
	SincePrompt time.Duration
	// Todos is the current session's task list, zero when the agent has not
	// used one.
	Todos Todos
	// Activity is the tool call in flight, nil when none is running.
	Activity *Activity
	// LastUser is the most recent thing you said to this agent, and
	// LastAssistant the most recent thing it has said in reply to that
	// prompt — each collapsed to a single line for display.
	//
	// LastAssistant is deliberately empty while an agent is working on a
	// prompt it has not answered yet: the previous turn's closing remarks
	// describe finished work, and showing them beside a new task reads as
	// if they were the current state.
	LastUser      string
	LastAssistant string
	Err           error
}

// describeInput picks the argument that best describes a tool call, for the
// running calls that have no title yet. The keys are the ones the agents'
// built-in tools use — both spellings of a file path, since opencode says
// filePath and Claude Code says file_path — and an unrecognised tool simply
// gets no description rather than a dump of its whole input.
func describeInput(in map[string]any) string {
	for _, k := range []string{"command", "filePath", "file_path", "path", "pattern", "query", "url", "description", "prompt"} {
		if v, ok := in[k]; ok {
			if str, ok := v.(string); ok && str != "" {
				return str
			}
		}
	}
	return ""
}

// markdownNoise matches the emphasis markers that carry no meaning once a
// message is flattened to one line.
var markdownNoise = regexp.MustCompile("(\\*\\*|`)")

// previewText flattens a message to a single display line: markdown
// emphasis stripped, whitespace collapsed, and capped well above what any
// caller shows so the wire payload stays small while leaving room for the
// caller to truncate as it likes.
func previewText(s string) string {
	s = markdownNoise.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	const max = 300
	if len(s) > max {
		s = strings.TrimSpace(s[:max]) + "…"
	}
	return s
}

// firstLine trims a possibly long, multi-line tool title down to something
// a single status line can hold.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
