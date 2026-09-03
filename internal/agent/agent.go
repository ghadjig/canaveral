// Package agent manages opencode agent servers within a workspace.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// listenRe matches the line opencode prints once its HTTP server is bound.
var listenRe = regexp.MustCompile(`listening on (https?://[^\s]+)`)

// DiscoverURL polls the agent log until opencode reports its listen address.
//
// Parsing the log rather than pre-allocating a port avoids a TOCTOU race where
// another process claims the port between our probe and opencode's bind.
func DiscoverURL(ctx context.Context, logPath string, timeout time.Duration, alive func() error) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	for {
		if alive != nil {
			if err := alive(); err != nil {
				return "", err
			}
		}
		if b, err := os.ReadFile(logPath); err == nil {
			if m := listenRe.FindSubmatch(b); m != nil {
				return string(m[1]), nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timed out after %s waiting for opencode to report a listen address (see %s)", timeout, logPath)
		case <-ticker.C:
		}
	}
}

// Resolve returns the absolute path to the opencode binary.
//
// Resolving in the parent (rather than relying on the unit's PATH) turns a
// missing toolchain into an immediate, clear error instead of a unit that
// starts and dies.
//
// LookPath alone is not enough: canaveral is often started from a Hyprland
// keybind or a quickshell launcher, which inherit the session's PATH rather
// than a real terminal's. That PATH is missing whatever a shell startup file
// would have added — mise/asdf shims, ~/.local/bin, npm's global bin — which
// is exactly where opencode tends to live. The same trap is documented for
// canaveral's own binary in share/quickshell/LauncherWindow.qml. ShellPATH
// resolves the fuller PATH a real terminal would see, and is tried as a
// fallback before giving up.
func Resolve() (string, error) {
	if bin, err := exec.LookPath("opencode"); err == nil {
		return filepath.Abs(bin)
	}
	if bin, ok := lookPathIn("opencode", ShellPATH()); ok {
		return filepath.Abs(bin)
	}
	return "", errors.New("opencode not found in PATH (checked a login/interactive shell's PATH too)")
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
// Needed generally, not just for opencode: canaveral itself, the systemd
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
	dirs := filepath.SplitList(os.Getenv("PATH"))
	seen := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		seen[d] = true
	}
	for _, flag := range []string{"-ic", "-lc"} {
		for _, d := range filepath.SplitList(shellPATHVia(flag)) {
			if d == "" || seen[d] {
				continue
			}
			seen[d] = true
			dirs = append(dirs, d)
		}
	}
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

// ServeCmd builds the shell command that starts a headless opencode server.
func ServeCmd(bin string) string {
	// --port 0 lets the kernel pick a free port, which we then read from the log.
	return fmt.Sprintf("exec %s serve --hostname 127.0.0.1 --port 0", shellQuote(bin))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var client = &http.Client{Timeout: 4 * time.Second}

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

// Activity is the tool call an agent is executing right now — the most
// literal answer to "what is it doing": a bash command it is waiting on, a
// file it is editing, a search it is running.
type Activity struct {
	// Tool is the tool's name, e.g. "bash", "edit", "grep".
	Tool string `json:"tool"`
	// Title is opencode's own human-readable description of the call: the
	// command line for bash, the path for a file tool. It can be long and
	// multi-line, so a consumer should expect to truncate it.
	Title string `json:"title,omitempty"`
	// Since is when the call started.
	Since time.Time `json:"since"`
}

// Health summarises what an agent server is doing right now.
type Health struct {
	Reachable bool
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

type sessionInfo struct {
	ID string `json:"id"`
	// ParentID is set on subagent sessions, which the Task tool creates in
	// the same directory as the conversation that spawned them.
	ParentID string `json:"parentID"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Tokens Tokens  `json:"tokens"`
	Cost   float64 `json:"cost"`
	Time   struct {
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type sessionListResp struct {
	Data []sessionInfo `json:"data"`
}

type sessionStatusInfo struct {
	Type string `json:"type"`
}

// messageInfo is the "info" half of a session message.
//
// Note the field names: the role is "role" (not "type") and the model is a
// flat "modelID" (not a nested object). Getting these wrong is silent — the
// JSON simply decodes to zero values — which is exactly how token, cost and
// busy reporting stayed broken while looking fine.
type messageInfo struct {
	ID   string `json:"id"`
	Role string `json:"role"`
	Time struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed"`
	} `json:"time"`
	Finish     string  `json:"finish"`
	Tokens     Tokens  `json:"tokens"`
	Cost       float64 `json:"cost"`
	ModelID    string  `json:"modelID"`
	Variant    string  `json:"variant"`
	ProviderID string  `json:"providerID"`
	Error      *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// sessionMessage is one entry of GET /session/{id}/message, which returns a
// bare array of {info, parts} — not a {"data": [...]} envelope, and not the
// flat message objects the /api/session/{id}/message surface implies.
type sessionMessage struct {
	Info  messageInfo   `json:"info"`
	Parts []messagePart `json:"parts"`
}

// messagePart is one part of a message. Only tool parts are used, to report
// what the agent is executing right now; the rest (text, reasoning,
// step markers) carry nothing a status view needs.
type messagePart struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Tool  string `json:"tool"`
	State struct {
		Status string `json:"status"` // pending | running | completed | error
		// Title is only filled in once a call finishes, so a call that is
		// still running — precisely the one worth reporting — has none, and
		// its arguments have to stand in. Confirmed against a live server.
		Title string         `json:"title"`
		Input map[string]any `json:"input"`
		Time  struct {
			Start int64 `json:"start"`
		} `json:"time"`
	} `json:"state"`
}

// requestTool identifies the message and tool call a question or
// permission request was raised from, which is what lets classify tell a
// genuinely open request from one whose turn has since ended.
type requestTool struct {
	MessageID string `json:"messageID"`
}

// permissionRequest is one entry of GET /permission, the server-wide list of
// pending permission requests.
type permissionRequest struct {
	SessionID  string      `json:"sessionID"`
	Permission string      `json:"permission"`
	Patterns   []string    `json:"patterns"`
	Tool       requestTool `json:"tool"`
}

// questionRequest is one entry of GET /question, the server-wide list of
// pending questions. A request can carry several questions; only the first
// is surfaced, since a compact widget has room for one headline and the
// rest are visible in the TUI anyway.
type questionRequest struct {
	SessionID string      `json:"sessionID"`
	Tool      requestTool `json:"tool"`
	Questions []struct {
		Question string `json:"question"`
		Header   string `json:"header"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

// todoItem is one entry of GET /session/{id}/todo.
type todoItem struct {
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

// Probe reports what the agent rooted at dir is currently doing.
//
// Sessions are filtered by location.directory because an opencode server
// exposes the user's entire global session history, not just this workspace's.
func Probe(ctx context.Context, baseURL, dir string) Health {
	if baseURL == "" {
		return Health{Err: fmt.Errorf("no url")}
	}

	var list sessionListResp
	if err := getJSON(ctx, baseURL+"/api/session?limit=100&order=desc", &list); err != nil {
		return Health{Err: err}
	}
	h := Health{Reachable: true}

	mine := sessionsInDir(list.Data, dir)

	// Subagent sessions (the Task tool spawns one per subagent) live in the
	// same directory as the conversation that created them, so a single
	// conversation can look like several sessions. Only top-level ones are
	// candidates for "the" current session: a subagent updates at almost
	// the same instant as its parent, so picking the newest of everything
	// would flip between them and make the reported model, state and
	// numbers jump around at random.
	roots := topLevelSessions(mine)
	h.Sessions = len(roots)
	if len(roots) == 0 {
		h.State = StateIdle
		return h
	}

	newest := roots[0]
	h.Updated = time.UnixMilli(newest.Time.Updated)
	h.SessionID = newest.ID

	// Cost and tokens are summed over the whole family — the conversation
	// plus every subagent beneath it. Subagent work is real spend on this
	// feature's behalf, and it is easily the larger share; reporting only
	// the parent understated one live feature by roughly three times.
	//
	// The session list already carries per-session totals, so this needs no
	// extra requests.
	family, familyIDs := sessionFamily(mine, newest)
	h.SubSessions = len(family) - 1
	for _, x := range family {
		h.Tokens.Input += x.Tokens.Input
		h.Tokens.Output += x.Tokens.Output
		h.Tokens.Reasoning += x.Tokens.Reasoning
		h.Tokens.Cache.Read += x.Tokens.Cache.Read
		h.Tokens.Cache.Write += x.Tokens.Cache.Write
		h.Cost += x.Cost
	}

	var msgs []sessionMessage
	if err := getJSON(ctx, baseURL+"/session/"+url.PathEscape(newest.ID)+"/message", &msgs); err != nil {
		h.State = StateIdle
		return h
	}

	// Messages drive only the current turn and elapsed work; totals come
	// from the session list above.
	//
	// Worked counts this conversation's turns only. A parent's turn stays
	// open while a subagent runs, so its duration already covers that work;
	// adding the subagents' own turns would double-count it.
	now := time.Now()
	turns := scanTurns(msgs, now)
	h.Worked = turns.worked
	h.LastUser = turns.lastUser
	h.SincePrompt = turns.sincePrompt
	// Only report what the agent said if it said it *after* the latest
	// prompt. Once a new prompt arrives, the previous turn's closing
	// remarks describe finished work, and showing them next to a
	// now-unrelated task reads as if they were the current state.
	if turns.lastAssistantIdx > turns.lastUserIdx {
		h.LastAssistant = turns.lastAssistant
	}

	h.Activity = currentActivity(msgs)

	if cur := turns.cur; cur != nil {
		h.Model = cur.ModelID
		h.Variant = cur.Variant
		h.Provider = cur.ProviderID
		h.Busy = cur.Time.Completed == nil
		if h.Busy {
			// Still generating; this turn has not contributed to Worked.
			h.Working = now.Sub(time.UnixMilli(cur.Time.Created))
		}
		if cur.Error != nil {
			h.LastError = cur.Error.Data.Message
		}
	}

	h.Todos = fetchTodos(ctx, baseURL, newest.ID)
	// A list with nothing left in_progress or pending is a finished plan.
	// opencode never clears it, so without this check it would go on
	// reporting the same "N/N done" bar forever, including once the
	// conversation has moved on to unrelated work. If a newer prompt has
	// arrived since the list was last touched, treat it the same way as a
	// stale LastAssistant above and drop it, rather than showing finished
	// work next to whatever the agent is doing now.
	if h.Todos.Total > 0 && h.Todos.InProgress == 0 && h.Todos.Pending == 0 && turns.lastUserIdx > turns.lastTodoWriteIdx {
		h.Todos = Todos{}
	}
	h.State, h.Pending = classify(ctx, baseURL, newest.ID, familyIDs, h.Busy, turns.completedByID)
	return h
}

// sessionsInDir filters a server's full session list down to the ones
// rooted at dir, sorted newest-updated-first.
func sessionsInDir(all []sessionInfo, dir string) []sessionInfo {
	want := filepath.Clean(dir)
	mine := make([]sessionInfo, 0, len(all))
	for _, s := range all {
		if want == "" || filepath.Clean(s.Location.Directory) == want {
			mine = append(mine, s)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Time.Updated > mine[j].Time.Updated })
	return mine
}

// topLevelSessions returns the sessions in mine that are not subagent
// sessions (see the ParentID field doc).
func topLevelSessions(mine []sessionInfo) []sessionInfo {
	roots := make([]sessionInfo, 0, len(mine))
	for _, x := range mine {
		if x.ParentID == "" {
			roots = append(roots, x)
		}
	}
	return roots
}

// sessionFamily returns root plus every subagent session transitively
// spawned from it, and the set of their IDs.
func sessionFamily(mine []sessionInfo, root sessionInfo) (family []sessionInfo, familyIDs map[string]bool) {
	byParent := map[string][]sessionInfo{}
	for _, x := range mine {
		if x.ParentID != "" {
			byParent[x.ParentID] = append(byParent[x.ParentID], x)
		}
	}
	family = []sessionInfo{root}
	for queue := []string{root.ID}; len(queue) > 0; {
		id := queue[0]
		queue = queue[1:]
		for _, child := range byParent[id] {
			family = append(family, child)
			queue = append(queue, child.ID)
		}
	}
	familyIDs = make(map[string]bool, len(family))
	for _, x := range family {
		familyIDs[x.ID] = true
	}
	return family, familyIDs
}

// turnScan is the result of scanning a session's messages (oldest first)
// for turn timing and the newest text on each side of the conversation.
type turnScan struct {
	// worked is the sum of (completed − created) across every assistant
	// turn that has finished — actual generation time, not wall-clock span.
	worked time.Duration
	// completedByID records whether each message has finished, keyed by
	// message ID. classify uses it to tell a genuinely open question or
	// permission request from one whose turn has already ended — see
	// classify's comment for why /question and /permission cannot always
	// be trusted alone.
	completedByID map[string]bool
	// cur is the last assistant message seen — the current turn, since
	// messages arrive oldest-first.
	cur                           *messageInfo
	lastUser, lastAssistant       string
	lastUserIdx, lastAssistantIdx int
	lastTodoWriteIdx              int
	sincePrompt                   time.Duration
}

// scanTurns walks a session's messages for the data Probe needs from the
// current turn: how long assistant turns have taken so far, the current
// (last) assistant message, the newest user/assistant text, and when the
// todo list was last touched. now anchors SincePrompt.
func scanTurns(msgs []sessionMessage, now time.Time) turnScan {
	t := scanCompletedTurns(msgs)
	scanConversationText(msgs, &t)
	if t.lastUserIdx >= 0 {
		if c := msgs[t.lastUserIdx].Info.Time.Created; c > 0 {
			t.sincePrompt = now.Sub(time.UnixMilli(c))
		}
	}
	return t
}

// scanCompletedTurns computes worked and completedByID, and finds the
// current (last) assistant message. Messages arrive oldest-first, so the
// last one seen with role "assistant" is the current turn.
func scanCompletedTurns(msgs []sessionMessage) turnScan {
	t := turnScan{lastUserIdx: -1, lastAssistantIdx: -1, lastTodoWriteIdx: -1}
	t.completedByID = make(map[string]bool, len(msgs))
	for i := range msgs {
		m := &msgs[i].Info
		t.completedByID[m.ID] = m.Time.Completed != nil
		if m.Role != "assistant" {
			continue
		}
		if m.Time.Completed != nil {
			t.worked += time.UnixMilli(*m.Time.Completed).Sub(time.UnixMilli(m.Time.Created))
		}
		t.cur = m
	}
	return t
}

// scanConversationText fills in the newest text on each side of the
// conversation, and when the todo list was last touched. A message can hold
// several text parts, so simply keeping the last non-empty one seen lands
// on the most recent.
func scanConversationText(msgs []sessionMessage, t *turnScan) {
	for i := range msgs {
		role := msgs[i].Info.Role
		if role != "user" && role != "assistant" {
			continue
		}
		for _, part := range msgs[i].Parts {
			if role == "assistant" && part.Type == "tool" && strings.EqualFold(part.Tool, "todowrite") {
				t.lastTodoWriteIdx = i
			}
			if part.Type != "text" || strings.TrimSpace(part.Text) == "" {
				continue
			}
			if role == "user" {
				t.lastUser = previewText(part.Text)
				t.lastUserIdx = i
			} else {
				t.lastAssistant = previewText(part.Text)
				t.lastAssistantIdx = i
			}
		}
	}
}

// currentActivity reports the tool call in flight in the newest turn, nil
// if nothing is running or pending. Only the newest turn is inspected:
// earlier turns' calls have all finished by definition.
func currentActivity(msgs []sessionMessage) *Activity {
	if len(msgs) == 0 {
		return nil
	}
	var act *Activity
	for _, part := range msgs[len(msgs)-1].Parts {
		if part.Type != "tool" {
			continue
		}
		if st := part.State.Status; st == "running" || st == "pending" {
			title := part.State.Title
			if title == "" {
				title = describeInput(part.State.Input)
			}
			act = &Activity{
				Tool:  part.Tool,
				Title: firstLine(title),
				Since: time.UnixMilli(part.State.Time.Start),
			}
		}
	}
	return act
}

// classify determines the fuller State beyond the plain busy/idle that
// Health.Busy already carries, and what the agent is blocked on if anything.
//
// Order matters. A pending question or permission wins over everything else,
// including busy: opencode keeps the session "busy" while a tool call sits
// waiting for your answer, but an agent that cannot move without you is the
// thing worth surfacing, not the fact that its turn is technically still
// open.
// describeInput picks the argument that best describes a tool call, for the
// running calls that have no title yet. The keys are the ones opencode's
// built-in tools use; an unrecognised tool simply gets no description
// rather than a dump of its whole input.
func describeInput(in map[string]any) string {
	for _, k := range []string{"command", "filePath", "path", "pattern", "query", "url", "description"} {
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

// fetchTodos reads the session's task list. A missing or empty list is not
// an error: most sessions never use one.
func fetchTodos(ctx context.Context, baseURL, sessionID string) Todos {
	var items []todoItem
	if err := getJSON(ctx, baseURL+"/session/"+url.PathEscape(sessionID)+"/todo", &items); err != nil {
		return Todos{}
	}
	var t Todos
	for _, it := range items {
		t.Total++
		switch it.Status {
		case "completed":
			t.Completed++
		case "in_progress":
			t.InProgress++
			if t.Current == "" {
				t.Current = it.Content
			}
		case "cancelled":
			t.Cancelled++
		default:
			t.Pending++
		}
	}
	return t
}

// classify determines the fuller State beyond the plain busy/idle that
// Health.Busy already carries, and what the agent is blocked on if
// anything.
//
// Pending items are matched against the whole session family, not just the
// conversation itself: a subagent that stops for permission blocks
// everything above it, because the parent's turn is sitting waiting for
// that subagent's result. Matching only the parent reported such an agent
// as merely "busy" while the TUI was showing a permission prompt.
//
// Order matters. A pending question or permission wins over everything
// else, including busy: opencode keeps the session "busy" while a tool call
// sits waiting for your answer, but an agent that cannot move without you
// is the thing worth surfacing.
//
// Neither list can be trusted blindly, though: if opencode crashes (or is
// killed) while a question or permission tool call is outstanding, restarting
// it can leave that request listed forever — there is no one left to answer
// it, and nothing left to time it out. The message the request names is
// checked before honouring it: once that message has a completed time, its
// turn has ended one way or another (answered through a channel this poll
// missed, aborted, or superseded by a restart) and the request is stale,
// no matter what /question or /permission still say.
func classify(ctx context.Context, baseURL, sessionID string, family map[string]bool, busy bool, completedByID map[string]bool) (State, *Pending) {
	if p := pendingQuestion(ctx, baseURL, sessionID, family, completedByID); p != nil {
		return StateWaiting, p
	}
	if p := pendingPermission(ctx, baseURL, sessionID, family, completedByID); p != nil {
		return StateWaiting, p
	}
	if isRetrying(ctx, baseURL, sessionID) {
		return StateRetrying, nil
	}
	if busy {
		return StateBusy, nil
	}
	return StateIdle, nil
}

// pendingQuestion reports the first genuinely open question raised against
// a session in family, nil if there is none.
//
// The server-wide list is used rather than a per-session one: it is a
// single request no matter how many sessions exist, and it lives on the
// same API surface as /session/{id}/message, which is the one verified to
// actually return data.
func pendingQuestion(ctx context.Context, baseURL, sessionID string, family map[string]bool, completedByID map[string]bool) *Pending {
	var qs []questionRequest
	if err := getJSON(ctx, baseURL+"/question", &qs); err != nil {
		return nil
	}
	for _, req := range qs {
		if !family[req.SessionID] {
			continue
		}
		if !toolCallStillOpen(ctx, baseURL, req.SessionID, req.Tool.MessageID, sessionID, completedByID) {
			continue
		}
		// A request can carry several questions; only the first is
		// surfaced, since a compact widget has room for one headline and
		// the rest are visible in the TUI anyway.
		if len(req.Questions) == 0 {
			continue
		}
		q := req.Questions[0]
		p := &Pending{Kind: BlockQuestion, Header: q.Header, Detail: q.Question}
		for _, o := range q.Options {
			p.Options = append(p.Options, o.Label)
		}
		return p
	}
	return nil
}

// pendingPermission reports the first genuinely open permission request
// raised against a session in family, nil if there is none.
func pendingPermission(ctx context.Context, baseURL, sessionID string, family map[string]bool, completedByID map[string]bool) *Pending {
	var perms []permissionRequest
	if err := getJSON(ctx, baseURL+"/permission", &perms); err != nil {
		return nil
	}
	for _, req := range perms {
		if !family[req.SessionID] {
			continue
		}
		if !toolCallStillOpen(ctx, baseURL, req.SessionID, req.Tool.MessageID, sessionID, completedByID) {
			continue
		}
		return &Pending{
			Kind:      BlockPermission,
			Header:    req.Permission,
			Detail:    req.Permission,
			Resources: req.Patterns,
		}
	}
	return nil
}

// isRetrying reports whether sessionID is currently auto-retrying after a
// provider error.
func isRetrying(ctx context.Context, baseURL, sessionID string) bool {
	var statuses map[string]sessionStatusInfo
	if err := getJSON(ctx, baseURL+"/session/status", &statuses); err != nil {
		return false
	}
	st, ok := statuses[sessionID]
	return ok && st.Type == "retry"
}

// toolCallStillOpen reports whether the message a question or permission
// request names (via its "tool" field) is still an open turn, i.e. the
// request is genuinely blocking rather than a leftover from a turn that has
// already ended.
//
// messageID is empty on an opencode version that predates the tool field;
// such a request is trusted as-is, since there is nothing to check it
// against and treating "unknown" as "stale" would hide real blocks.
//
// newestSessionID/completedByID cover the one session Probe already fetched
// the transcript for, which is the common case (the request belongs to the
// conversation itself, or the conversation is blocked on its own turn). A
// request from a different family member — a subagent asking its own
// question or permission — needs its own message fetched, which happens
// rarely enough that the extra round trip is not worth avoiding.
func toolCallStillOpen(ctx context.Context, baseURL, reqSessionID, messageID, newestSessionID string, completedByID map[string]bool) bool {
	if messageID == "" {
		return true
	}
	if reqSessionID == newestSessionID {
		done, ok := completedByID[messageID]
		if !ok {
			return true
		}
		return !done
	}
	var msgs []sessionMessage
	if err := getJSON(ctx, baseURL+"/session/"+url.PathEscape(reqSessionID)+"/message", &msgs); err != nil {
		return true
	}
	for _, m := range msgs {
		if m.Info.ID == messageID {
			return m.Info.Time.Completed == nil
		}
	}
	return true
}

func getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ForkInto copies a session and re-homes the copy to dir, returning the new
// session's ID.
//
// opencode fixes a session's directory when it is created, and a fork
// inherits the original's — so a fork alone would leave the copy pointing at
// whichever worktree the source came from. That directory may since have
// been deleted (leaving an agent hanging in a path that no longer exists),
// and even when it still exists it is the wrong feature's, which hides the
// session from anything scoping sessions by directory.
//
// Moving the copy afterwards fixes both. The move endpoint is experimental
// and cannot cross projects, which is not a limit here: a project's features
// all live under the same project root.
func ForkInto(ctx context.Context, baseURL, sessionID, dir string) (string, error) {
	var forked struct {
		ID string `json:"id"`
	}
	if err := postJSON(ctx, baseURL+"/session/"+url.PathEscape(sessionID)+"/fork", struct{}{}, &forked); err != nil {
		return "", fmt.Errorf("fork %s: %w", sessionID, err)
	}
	if forked.ID == "" {
		return "", fmt.Errorf("fork %s: no session id returned", sessionID)
	}

	body := map[string]any{
		"sessionID":   forked.ID,
		"destination": map[string]string{"directory": dir},
	}
	if err := postJSON(ctx, baseURL+"/experimental/control-plane/move-session", body, nil); err != nil {
		return "", fmt.Errorf("move forked session into %s: %w", dir, err)
	}
	return forked.ID, nil
}

func postJSON(ctx context.Context, u string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("POST %s: status %d", u, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
