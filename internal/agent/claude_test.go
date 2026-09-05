package agent

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claudeStore points the harness at a temporary ~/.claude for the duration
// of a test, so nothing reads (or writes) the real one.
func claudeStore(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
}

// writeTranscript lays down one session file for workdir, exactly where
// Claude Code would put it.
func writeTranscript(t *testing.T, workdir, id string, lines ...string) string {
	t.Helper()
	dir, err := projectDir(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// scan runs the transcript walk directly, so a test can say whether the
// program is live without a claude process actually existing.
func scan(t *testing.T, live bool, lines ...string) Health {
	t.Helper()
	var entries []claudeEntry
	for _, l := range lines {
		var e claudeEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("fixture line is not valid JSON: %v\n%s", err, l)
		}
		entries = append(entries, e)
	}
	h := Health{Reachable: true, Live: live, State: StateIdle}
	scanClaude(entries, time.Now(), live, &h)
	return h
}

const (
	claudePrompt   = `{"type":"user","isSidechain":false,"uuid":"u1","timestamp":"2026-09-05T10:00:00.000Z","message":{"role":"user","content":"add a login page"}}`
	claudeReply    = `{"type":"assistant","isSidechain":false,"uuid":"a1","parentUuid":"u1","timestamp":"2026-09-05T10:00:04.000Z","message":{"role":"assistant","model":"claude-opus-4-5","content":[{"type":"text","text":"Done — the page is at /login."}],"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5,"cache_creation_input_tokens":7}}}`
	claudeTurnDone = `{"type":"system","subtype":"turn_duration","isSidechain":false,"uuid":"s1","timestamp":"2026-09-05T10:00:04.100Z","durationMs":4000}`
)

func TestProjectSlugMatchesClaudesOwnEncoding(t *testing.T) {
	cases := map[string]string{
		"/home/bandito/projects/norules":          "-home-bandito-projects-norules",
		"/home/bandito/projects/norules/apps/app": "-home-bandito-projects-norules-apps-app",
		"/home/me/my_app.v2":                      "-home-me-my-app-v2",
	}
	for in, want := range cases {
		if got := projectSlug(in); got != want {
			t.Errorf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeProbeWithNoTranscriptsIsIdleNotAnError(t *testing.T) {
	// A feature nobody has opened an agent in yet is the normal state, not
	// a fault: it must not paint the status row red.
	claudeStore(t)
	h := (claude{}).Probe(context.Background(), Conn{Dir: "/wt/fresh"})
	if !h.Reachable || h.Err != nil {
		t.Fatalf("Reachable = %v, Err = %v, want reachable and no error", h.Reachable, h.Err)
	}
	if h.Sessions != 0 || h.State != StateIdle {
		t.Errorf("Sessions = %d, State = %q, want 0 and idle", h.Sessions, h.State)
	}
}

func TestClaudeProbeReadsTheNewestTranscript(t *testing.T) {
	claudeStore(t)
	wt := "/wt/feature"

	old := writeTranscript(t, wt, "11111111-1111-4111-8111-111111111111", claudePrompt)
	newer := writeTranscript(t, wt, "22222222-2222-4222-8222-222222222222", claudePrompt, claudeReply, claudeTurnDone)
	// Modification time is what orders sessions, and both files were just
	// written, so it is set explicitly rather than left to the clock.
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, older, older); err != nil {
		t.Fatal(err)
	}
	_ = newer

	h := (claude{}).Probe(context.Background(), Conn{Dir: wt})
	if h.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2", h.Sessions)
	}
	if h.SessionID != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("SessionID = %q, want the newest session", h.SessionID)
	}
	if h.Model != "claude-opus-4-5" {
		t.Errorf("Model = %q", h.Model)
	}
	if h.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic", h.Provider)
	}
	if got, want := h.Tokens.Total(), int64(42); got != want {
		t.Errorf("Tokens.Total() = %d, want %d", got, want)
	}
}

func TestClaudeIdleOnceTheTurnHasEnded(t *testing.T) {
	h := scan(t, true, claudePrompt, claudeReply, claudeTurnDone)
	if h.Busy || h.State != StateIdle {
		t.Errorf("Busy = %v, State = %q, want idle after a turn_duration marker", h.Busy, h.State)
	}
	if h.Worked != 4*time.Second {
		t.Errorf("Worked = %s, want 4s from the turn's own duration", h.Worked)
	}
}

// The turn marker is the authoritative signal: a prompt newer than the last
// one means the agent is still going, however much it has already said.
func TestClaudeBusyWhileATurnIsOpen(t *testing.T) {
	second := `{"type":"user","isSidechain":false,"uuid":"u9","timestamp":"2026-09-05T10:05:00.000Z","message":{"role":"user","content":"now add logout"}}`
	partial := `{"type":"assistant","isSidechain":false,"uuid":"a9","timestamp":"2026-09-05T10:05:02.000Z","message":{"role":"assistant","content":[{"type":"text","text":"Working on it."}]}}`
	h := scan(t, true, claudePrompt, claudeReply, claudeTurnDone, second, partial)
	if !h.Busy || h.State != StateBusy {
		t.Errorf("Busy = %v, State = %q, want busy: the newest turn has no end marker", h.Busy, h.State)
	}
	if h.Worked != 4*time.Second {
		t.Errorf("Worked = %s, want only the turn that finished", h.Worked)
	}
}

// Transcripts written before Claude Code emitted turn markers fall back to
// the shape of the conversation: an unanswered prompt means it is working.
func TestClaudeBusyWithoutTurnMarkers(t *testing.T) {
	if h := scan(t, true, claudePrompt); !h.Busy {
		t.Error("Busy = false for an unanswered prompt")
	}
	if h := scan(t, true, claudePrompt, claudeReply); h.Busy {
		t.Error("Busy = true after a reply with nothing outstanding")
	}
}

// A killed agent leaves its last turn open forever. Nothing is left running
// to finish it, so reporting it as busy would strand the row on "working"
// until someone opened the feature again.
func TestClaudeNotBusyWhenNothingIsRunning(t *testing.T) {
	h := scan(t, false, claudePrompt)
	if h.Busy || h.State != StateIdle {
		t.Errorf("Busy = %v, State = %q, want idle when no claude process is up", h.Busy, h.State)
	}
}

func TestClaudeReportsTheToolInFlight(t *testing.T) {
	call := `{"type":"assistant","isSidechain":false,"uuid":"a2","timestamp":"2026-09-05T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`
	h := scan(t, true, claudePrompt, call)
	if h.Activity == nil {
		t.Fatal("Activity = nil, want the outstanding tool call")
	}
	if h.Activity.Tool != "Bash" || h.Activity.Title != "go test ./..." {
		t.Errorf("Activity = %+v", h.Activity)
	}
}

func TestClaudeToolResultClosesTheCall(t *testing.T) {
	call := `{"type":"assistant","isSidechain":false,"uuid":"a2","timestamp":"2026-09-05T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"go test ./..."}}]}}`
	result := `{"type":"user","isSidechain":false,"uuid":"u2","timestamp":"2026-09-05T10:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	h := scan(t, true, claudePrompt, call, result)
	if h.Activity != nil {
		t.Errorf("Activity = %+v, want nil once the result has come back", h.Activity)
	}
}

// A tool result is the harness feeding the model, not you talking to it.
// Counting one as a prompt would reset SincePrompt on every tool round trip
// and make the "how long has it been working on what I asked" timer useless.
func TestClaudeToolResultIsNotAPrompt(t *testing.T) {
	result := `{"type":"user","isSidechain":false,"uuid":"u2","timestamp":"2026-09-05T10:00:06.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	h := scan(t, true, claudePrompt, result)
	if h.LastUser != "add a login page" {
		t.Errorf("LastUser = %q, want the typed prompt", h.LastUser)
	}
}

func TestClaudeHidesTheReplyToAnEarlierPrompt(t *testing.T) {
	second := `{"type":"user","isSidechain":false,"uuid":"u9","timestamp":"2026-09-05T10:05:00.000Z","message":{"role":"user","content":"now add logout"}}`
	h := scan(t, true, claudePrompt, claudeReply, claudeTurnDone, second)
	if h.LastUser != "now add logout" {
		t.Errorf("LastUser = %q", h.LastUser)
	}
	if h.LastAssistant != "" {
		t.Errorf("LastAssistant = %q, want empty while the newest prompt is unanswered", h.LastAssistant)
	}
}

func TestClaudeShowsTheReplyOnceItAnswers(t *testing.T) {
	h := scan(t, true, claudePrompt, claudeReply, claudeTurnDone)
	if h.LastAssistant != "Done — the page is at /login." {
		t.Errorf("LastAssistant = %q", h.LastAssistant)
	}
}

func TestClaudeReadsTodosFromTheLastTodoWrite(t *testing.T) {
	todo := `{"type":"assistant","isSidechain":false,"uuid":"a3","timestamp":"2026-09-05T10:00:07.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"TodoWrite","input":{"todos":[
		{"content":"Write the form","status":"completed"},
		{"content":"Wire the route","status":"in_progress"},
		{"content":"Add a test","status":"pending"}]}}]}}`
	h := scan(t, true, claudePrompt, todo)
	if h.Todos.Total != 3 || h.Todos.Completed != 1 || h.Todos.InProgress != 1 || h.Todos.Pending != 1 {
		t.Errorf("Todos = %+v", h.Todos)
	}
	if h.Todos.Current != "Wire the route" {
		t.Errorf("Todos.Current = %q, want the in-progress task", h.Todos.Current)
	}
}

// A finished plan is never cleared, so it must not go on being reported
// beside whatever the conversation has moved on to.
func TestClaudeDropsAResolvedTodoListAfterANewerPrompt(t *testing.T) {
	done := `{"type":"assistant","isSidechain":false,"uuid":"a3","timestamp":"2026-09-05T10:00:07.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"TodoWrite","input":{"todos":[{"content":"Write the form","status":"completed"}]}}]}}`
	next := `{"type":"user","isSidechain":false,"uuid":"u9","timestamp":"2026-09-05T10:05:00.000Z","message":{"role":"user","content":"something else entirely"}}`

	if h := scan(t, true, claudePrompt, done); h.Todos.Total != 1 {
		t.Fatalf("Todos = %+v, want the finished list while it is still current", h.Todos)
	}
	if h := scan(t, true, claudePrompt, done, next); h.Todos.Total != 0 {
		t.Errorf("Todos = %+v, want it dropped once a newer prompt arrived", h.Todos)
	}
}

// Subagents share the parent's transcript rather than getting one of their
// own, so their spend has to be picked out of the same file — and it is real
// spend on this feature's behalf.
func TestClaudeCountsSubagentTokensAndSessions(t *testing.T) {
	spawn := `{"type":"assistant","isSidechain":false,"uuid":"a2","timestamp":"2026-09-05T10:00:05.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Task","input":{"description":"explore"}}]}}`
	subRoot := `{"type":"user","isSidechain":true,"uuid":"s1","parentUuid":"a2","timestamp":"2026-09-05T10:00:06.000Z","message":{"role":"user","content":"explore"}}`
	subReply := `{"type":"assistant","isSidechain":true,"uuid":"s2","parentUuid":"s1","timestamp":"2026-09-05T10:00:07.000Z","message":{"role":"assistant","content":[{"type":"text","text":"found it"}],"usage":{"input_tokens":100,"output_tokens":200}}}`

	h := scan(t, true, claudePrompt, claudeReply, spawn, subRoot, subReply)
	if h.SubSessions != 1 {
		t.Errorf("SubSessions = %d, want 1", h.SubSessions)
	}
	if got, want := h.Tokens.Total(), int64(342); got != want {
		t.Errorf("Tokens.Total() = %d, want %d (parent plus subagent)", got, want)
	}
	// The subagent's own text is not what you are talking to.
	if h.LastAssistant != "" && h.LastAssistant == "found it" {
		t.Error("LastAssistant took its text from a subagent")
	}
}

func TestClaudeSurfacesAnAPIError(t *testing.T) {
	fail := `{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"uuid":"a4","timestamp":"2026-09-05T10:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 529 overloaded"}]}}`
	h := scan(t, true, claudePrompt, fail)
	if h.LastError != "API Error: 529 overloaded" {
		t.Errorf("LastError = %q", h.LastError)
	}
	if h.LastAssistant != "" {
		t.Errorf("LastAssistant = %q, an error is not something the agent said", h.LastAssistant)
	}
}

func TestClaudeForkCopiesAndRehomesTheConversation(t *testing.T) {
	claudeStore(t)
	src, dst := "/wt/step1", "/wt/step2"
	prompt := `{"type":"user","isSidechain":false,"uuid":"u1","sessionId":"aaaa","cwd":"/wt/step1","message":{"role":"user","content":"hello"}}`
	writeTranscript(t, src, "aaaa", prompt)

	newID, err := (claude{}).Fork(context.Background(), Conn{Dir: src}, "aaaa", dst)
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if newID == "aaaa" || newID == "" {
		t.Fatalf("Fork returned %q, want a fresh session id", newID)
	}

	dir, err := projectDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, newID+".jsonl"))
	if err != nil {
		t.Fatalf("the fork is not filed under the destination worktree: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &obj); err != nil {
		t.Fatal(err)
	}
	if obj["sessionId"] != newID {
		t.Errorf("sessionId = %v, want the copy's own id %q", obj["sessionId"], newID)
	}
	if obj["cwd"] != dst {
		t.Errorf("cwd = %v, want %q — a copy still pointing at the source would work in the wrong worktree", obj["cwd"], dst)
	}
	// The messages are the point of a fork; they must survive verbatim.
	msg, _ := obj["message"].(map[string]any)
	if msg == nil || msg["content"] != "hello" {
		t.Errorf("message = %v, want the original content", obj["message"])
	}
}

func TestClaudeForkRefusesAnUnknownSession(t *testing.T) {
	claudeStore(t)
	if _, err := (claude{}).Fork(context.Background(), Conn{Dir: "/wt/a"}, "nope", "/wt/b"); err == nil {
		t.Error("Fork of a session that does not exist should fail")
	}
}

func TestClaudeAttachAndSessionFlags(t *testing.T) {
	h := claude{}
	if got := h.SessionFlag(""); got != "" {
		t.Errorf("SessionFlag(\"\") = %q, want empty", got)
	}
	if got, want := h.SessionFlag("abc"), "--resume abc"; got != want {
		t.Errorf("SessionFlag = %q, want %q", got, want)
	}
	if got := strings.Join(h.AttachArgv(Conn{Dir: "/wt"}, true), " "); got != "claude --continue" {
		t.Errorf("AttachArgv --continue = %q", got)
	}
	if got := strings.Join(h.AttachArgv(Conn{Dir: "/wt"}, false), " "); got != "claude" {
		t.Errorf("AttachArgv fresh = %q", got)
	}
}

// Claude Code is not supervised by canaveral, and the rest of the codebase
// keys a great deal of behaviour off that one bit.
func TestClaudeDoesNotServe(t *testing.T) {
	if (claude{}).Serves() {
		t.Error("Serves() = true; canaveral would try to start a unit for it")
	}
	if err := (claude{}).Watch(context.Background(), Conn{}, func() {}); err != ErrNoEvents {
		t.Errorf("Watch err = %v, want ErrNoEvents so callers fall back to polling", err)
	}
	if !Probeable("claude", "") {
		t.Error("Probeable = false without a URL; a harness with no server would never be probed")
	}
}

func TestClaudeEnvSelectsTheModel(t *testing.T) {
	env := (claude{}).EnvFor(Selection{Model: "claude-opus-4-5", Agent: "build"})
	if env["ANTHROPIC_MODEL"] != "claude-opus-4-5" {
		t.Errorf("env = %v, want ANTHROPIC_MODEL set", env)
	}
	if len(env) != 1 {
		t.Errorf("env = %v, want only the model — claude has no session-wide persona", env)
	}
}

func TestForRejectsAnUnknownTool(t *testing.T) {
	if _, err := For("nano-banana"); err == nil {
		t.Error("For(unknown) = nil error")
	}
	h := Probe(context.Background(), "nano-banana", Conn{Dir: "/wt"})
	if h.Reachable || h.Err == nil {
		t.Errorf("Probe with an unknown tool = %+v, want an unreachable row carrying the error", h)
	}
}

// claudeRunning is the only way to tell whether a harness canaveral does not
// supervise is up, so it is worth exercising against a real process rather
// than trusting the /proc walk by inspection.
func TestClaudeRunningFindsAProcessInTheDirectory(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	bin, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip("cannot read sleep")
	}
	// Named "claude" because that is what the scan matches on.
	fake := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(fake, bin, 0o755); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	if claudeRunning(work) {
		t.Fatal("claudeRunning = true before anything was started")
	}

	cmd := exec.Command(fake, "30")
	cmd.Dir = work
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start the fake agent: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	if !claudeRunning(work) {
		t.Error("claudeRunning = false with a claude process in the directory")
	}
	// A sibling worktree must not be mistaken for this one.
	if claudeRunning(t.TempDir()) {
		t.Error("claudeRunning = true for an unrelated directory")
	}
}

// An error belongs to the turn that hit it. Reporting one for the rest of
// the conversation would leave the status row red long after the agent
// recovered.
func TestClaudeClearsAnErrorOnceTheModelGetsThrough(t *testing.T) {
	fail := `{"type":"assistant","isSidechain":false,"isApiErrorMessage":true,"uuid":"a4","timestamp":"2026-09-05T10:00:08.000Z","message":{"role":"assistant","content":[{"type":"text","text":"API Error: 529 overloaded"}]}}`
	h := scan(t, true, claudePrompt, fail, claudeReply, claudeTurnDone)
	if h.LastError != "" {
		t.Errorf("LastError = %q, want it cleared by the successful turn that followed", h.LastError)
	}
	if h.LastAssistant != "Done — the page is at /login." {
		t.Errorf("LastAssistant = %q", h.LastAssistant)
	}
}
