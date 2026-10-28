package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Claude Code writes a tool call to its transcript only once the call has a
// result. That is fine for a call that runs, but a question or a permission
// dialog is a call whose result is your answer, so for as long as it is on
// screen the transcript holds nothing but the prompt that led to it, and reads
// as a turn in progress. Hooks fire at the moment the dialog goes up, so the
// agent's terminal is started with hooks that report each call to `canaveral
// claude-hook`, which keeps the session's unfinished calls in a small file for
// Probe to read next to the transcript.

// ClaudeHookCommand is the subcommand Claude Code's hooks run.
const ClaudeHookCommand = "claude-hook"

// hookBin is the binary the hooks call, a variable so tests can pin it.
var hookBin = os.Executable

// claudeHookEvents are the events the hooks are registered for. A permission
// request carries no tool_use_id, so PreToolUse is needed to supply one; the
// last three are what end calls and turns.
var claudeHookEvents = []string{
	"PreToolUse", "PermissionRequest", "PostToolUse", "PostToolUseFailure",
	"UserPromptSubmit", "Stop",
}

// claudeSettings renders the --settings value that installs the hooks, ""
// when canaveral cannot name its own binary.
func claudeSettings() string {
	bin, err := hookBin()
	if err != nil {
		return ""
	}
	hook := []map[string]any{{
		"type":    "command",
		"command": shellWord(bin) + " " + ClaudeHookCommand,
		// It does a read and a write on tmpfs. Anything slower is stuck,
		// and every tool call waits on it.
		"timeout": 5,
	}}
	hooks := map[string]any{}
	for _, ev := range claudeHookEvents {
		// No matcher: an empty one matches every tool.
		hooks[ev] = []map[string]any{{"hooks": hook}}
	}
	b, err := json.Marshal(map[string]any{"hooks": hooks})
	if err != nil {
		return ""
	}
	return string(b)
}

// claudeHookInput is the part of a hook's stdin canaveral reads.
type claudeHookInput struct {
	Event     string `json:"hook_event_name"`
	SessionID string `json:"session_id"`
	// AgentID is set for a subagent's calls. Those are never shown to you
	// and their turns end on their own, so they are left out.
	AgentID   string         `json:"agent_id"`
	ToolName  string         `json:"tool_name"`
	ToolInput map[string]any `json:"tool_input"`
	ToolUseID string         `json:"tool_use_id"`
}

// claudeCall is a tool call the hooks have seen start and not finish.
type claudeCall struct {
	ID   string `json:"id,omitempty"`
	Tool string `json:"tool"`
	// Pending is set once the call is waiting on you.
	Pending *Pending  `json:"pending,omitempty"`
	At      time.Time `json:"at"`
}

// claudeHookState is what is kept per session.
type claudeHookState struct {
	Calls []claudeCall `json:"calls"`
}

// safeSessionID keeps a session id from naming a path outside the hook
// directory; Claude Code's are UUIDs.
var safeSessionID = regexp.MustCompile(`^[A-Za-z0-9-]+$`)

// claudeHookDir holds one state file per session. $XDG_RUNTIME_DIR is tmpfs
// and private, and what it holds is only meaningful while the process that
// wrote it runs.
func claudeHookDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "canaveral", "claude-hooks")
	}
	return filepath.Join(os.TempDir(), "canaveral", "claude-hooks")
}

func claudeHookPath(sessionID string) string {
	return filepath.Join(claudeHookDir(), sessionID+".json")
}

// RecordClaudeHook applies the hook event on r to its session's state file.
func RecordClaudeHook(r io.Reader) error {
	var in claudeHookInput
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return fmt.Errorf("read hook input: %w", err)
	}
	if in.AgentID != "" || !safeSessionID.MatchString(in.SessionID) {
		return nil
	}
	if err := os.MkdirAll(claudeHookDir(), 0o700); err != nil {
		return err
	}
	path := claudeHookPath(in.SessionID)

	// Parallel tool calls fire their hooks concurrently, and each of these
	// is a read-modify-write of the same file.
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock %s: %w", path, err)
	}

	st := readClaudeHookState(path)
	st.apply(in, time.Now())
	if len(st.Calls) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readClaudeHookState loads a session's state, empty when there is none.
func readClaudeHookState(path string) claudeHookState {
	var st claudeHookState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (st *claudeHookState) apply(in claudeHookInput, now time.Time) {
	switch in.Event {
	case "PreToolUse":
		st.Calls = append(st.Calls, claudeCall{
			ID:   in.ToolUseID,
			Tool: in.ToolName,
			// A question or plan is waiting on you from the moment it is
			// called, with no permission step in between.
			Pending: pendingFor(claudeBlock{Name: in.ToolName, Input: in.ToolInput}),
			At:      now,
		})
	case "PermissionRequest":
		p := &Pending{
			Kind:   BlockPermission,
			Header: in.ToolName,
			Detail: firstLine(describeInput(in.ToolInput)),
		}
		// The request names no call, so it goes to the newest call of that
		// tool not already waiting.
		for i := len(st.Calls) - 1; i >= 0; i-- {
			c := &st.Calls[i]
			if c.Tool != in.ToolName {
				continue
			}
			if c.Pending == nil {
				c.Pending = p
			}
			return
		}
		st.Calls = append(st.Calls, claudeCall{Tool: in.ToolName, Pending: p, At: now})
	case "PostToolUse", "PostToolUseFailure":
		for i, c := range st.Calls {
			if c.ID != "" && c.ID == in.ToolUseID {
				st.Calls = append(st.Calls[:i], st.Calls[i+1:]...)
				break
			}
		}
	case "UserPromptSubmit", "Stop":
		// Nothing outlives a turn, a denied permission included, which
		// fires no hook of its own.
		st.Calls = nil
	}
}

// claudeHookPending returns what session's newest unfinished call is waiting
// on you for, nil when nothing.
//
// answered reports whether the transcript has a result for a call. A denied
// permission fires no hook, but its result is written, and that is what
// stops it from reading as waiting for the rest of the turn.
func claudeHookPending(sessionID string, answered func(id string) bool) *Pending {
	if !safeSessionID.MatchString(sessionID) {
		return nil
	}
	st := readClaudeHookState(claudeHookPath(sessionID))
	for i := len(st.Calls) - 1; i >= 0; i-- {
		c := st.Calls[i]
		if c.Pending != nil && (c.ID == "" || !answered(c.ID)) {
			return c.Pending
		}
	}
	return nil
}
