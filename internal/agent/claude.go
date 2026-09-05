package agent

// The Claude Code harness.
//
// Claude Code has no server. There is nothing to start in a systemd unit,
// nothing to attach to over HTTP, and no API to ask what it is doing — the
// program in your terminal *is* the agent. Everything canaveral knows about
// one therefore comes from the transcript Claude Code writes as it works:
// one JSON-lines file per session under
//
//	~/.claude/projects/<slugged working directory>/<session uuid>.jsonl
//
// That file is append-only and written as things happen, so reading it is a
// genuine live view rather than a post-hoc log — the same information the
// TUI is drawing from, a few milliseconds behind.
//
// Two things do not survive the lack of a server, and they are called out
// here rather than faked. A permission prompt is drawn and answered entirely
// in the TUI and never reaches the transcript until it has been resolved, so
// this harness cannot report StateWaiting: an agent stopped on a permission
// prompt looks busy, because from the transcript's point of view it is still
// mid-turn. And there is no push notification of any kind, so `canaveral
// watch` falls back to its polling interval instead of reacting instantly.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// claude implements Harness for Claude Code.
type claude struct{}

func (claude) Name() string { return "claude" }

func (claude) Resolve() (string, error) { return resolveBin("claude") }

// Serves is false: Claude Code is the terminal program, not a server behind
// it, so canaveral starts no unit for it and records neither URL nor port.
func (claude) Serves() bool { return false }

func (claude) ServeCmd(string) string { return "" }

func (claude) DiscoverURL(context.Context, string, time.Duration, func() error) (string, error) {
	return "", fmt.Errorf("claude has no server to discover")
}

// EnvFor maps the manifest's model onto ANTHROPIC_MODEL, which is what
// Claude Code reads when no --model flag is given.
//
// A manifest's `agent` (opencode's named persona) has no equivalent and is
// deliberately dropped rather than approximated: Claude Code's subagents are
// defined by files in .claude/agents and are chosen per task by the model,
// not fixed for a session.
func (claude) EnvFor(sel Selection) map[string]string {
	env := map[string]string{}
	if sel.Model != "" {
		env["ANTHROPIC_MODEL"] = sel.Model
	}
	return env
}

// AttachArgv starts Claude Code itself. There is no --dir equivalent: the
// working directory the process is started in is what scopes its session
// history, which is why the caller runs this in c.Dir.
func (claude) AttachArgv(_ Conn, cont bool) []string {
	argv := []string{"claude"}
	if cont {
		argv = append(argv, "--continue")
	}
	return argv
}

func (claude) SessionFlag(id string) string {
	if id == "" {
		return ""
	}
	return "--resume " + id
}

// Watch has nothing to subscribe to; callers poll instead.
func (claude) Watch(context.Context, Conn, func()) error { return ErrNoEvents }

// claudeHome returns Claude Code's state directory, honouring the
// CLAUDE_CONFIG_DIR override it reads itself.
func claudeHome() (string, error) {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// projectSlug is how Claude Code names the directory holding a working
// directory's transcripts: every character outside [A-Za-z0-9] becomes a
// hyphen, leading slash included. "/home/me/src/app" becomes
// "-home-me-src-app".
//
// The mapping is lossy and therefore not reversible, which does not matter
// here — canaveral only ever goes from a known worktree to its transcripts,
// never the other way.
func projectSlug(dir string) string {
	dir = filepath.Clean(dir)
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '-'
	}, dir)
}

// projectDir returns the directory holding dir's transcripts.
func projectDir(dir string) (string, error) {
	home, err := claudeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "projects", projectSlug(dir)), nil
}

// transcript is one session file, newest first by modification time.
type transcript struct {
	path    string
	id      string
	updated time.Time
}

// transcripts lists the sessions Claude Code has for dir, newest first.
//
// A missing project directory is not an error: it only means Claude Code has
// never been run there, which is the normal state of a feature nobody has
// opened an agent in yet.
func transcripts(dir string) ([]transcript, error) {
	pd, err := projectDir(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(pd)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []transcript
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, transcript{
			path:    filepath.Join(pd, e.Name()),
			id:      strings.TrimSuffix(e.Name(), ".jsonl"),
			updated: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].updated.After(out[j].updated) })
	return out, nil
}

// claudeEntry is one line of a transcript. Only the fields canaveral reads
// are declared; a transcript line carries a good deal more.
type claudeEntry struct {
	Type    string `json:"type"` // user | assistant | system | summary | ...
	Subtype string `json:"subtype"`
	UUID    string `json:"uuid"`
	// ParentUUID chains entries into a conversation. A sidechain entry whose
	// parent is not itself a sidechain entry is the root of one subagent.
	ParentUUID *string `json:"parentUuid"`
	// IsSidechain marks the entries belonging to a Task subagent. They share
	// the parent's transcript file rather than getting one of their own,
	// which is the main structural difference from opencode.
	IsSidechain bool      `json:"isSidechain"`
	IsMeta      bool      `json:"isMeta"`
	Timestamp   time.Time `json:"timestamp"`
	// DurationMs is set on the system/turn_duration entry Claude Code writes
	// when a turn ends: the one unambiguous "the agent has stopped" marker
	// in the file, and the source of Worked.
	DurationMs int64 `json:"durationMs"`
	// CostUSD is present on older transcripts only; newer ones record no
	// cost at all, which is why Health.Cost is often zero here.
	CostUSD           float64        `json:"costUSD"`
	IsAPIErrorMessage bool           `json:"isApiErrorMessage"`
	Message           *claudeMessage `json:"message"`
}

type claudeMessage struct {
	Role  string `json:"role"`
	Model string `json:"model"`
	// Content is a bare string for a typed prompt and an array of blocks
	// otherwise, so it has to be decoded in two attempts.
	Content json.RawMessage `json:"content"`
	Usage   claudeUsage     `json:"usage"`
}

type claudeUsage struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_input_tokens"`
	CacheWrite int64 `json:"cache_creation_input_tokens"`
}

type claudeBlock struct {
	Type      string         `json:"type"` // text | thinking | tool_use | tool_result
	Text      string         `json:"text"`
	Name      string         `json:"name"`
	ID        string         `json:"id"`
	ToolUseID string         `json:"tool_use_id"`
	Input     map[string]any `json:"input"`
}

// blocks decodes a message's content, normalising the plain-string form a
// typed prompt uses into a single text block.
func (m *claudeMessage) blocks() []claudeBlock {
	if m == nil || len(m.Content) == 0 {
		return nil
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return []claudeBlock{{Type: "text", Text: s}}
	}
	var bs []claudeBlock
	if err := json.Unmarshal(m.Content, &bs); err != nil {
		return nil
	}
	return bs
}

// readTranscript decodes a session file. Unparseable lines are skipped
// rather than failing the read: the file is being appended to while we read
// it, so the last line is quite legitimately half-written.
func readTranscript(path string) ([]claudeEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []claudeEntry
	sc := bufio.NewScanner(f)
	// A single entry holds a whole assistant turn, tool inputs included, and
	// a large file edit runs well past bufio's 64 KiB default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e claudeEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Probe reports what Claude Code is doing in c.Dir, from its newest
// transcript there.
func (claude) Probe(_ context.Context, c Conn) Health {
	if c.Dir == "" {
		return Health{Err: fmt.Errorf("no directory")}
	}
	sessions, err := transcripts(c.Dir)
	if err != nil {
		return Health{Err: err}
	}

	out := Health{
		Reachable: true,
		Live:      claudeRunning(c.Dir),
		Sessions:  len(sessions),
		Provider:  "anthropic",
		State:     StateIdle,
	}
	if len(sessions) == 0 {
		return out
	}

	newest := sessions[0]
	out.SessionID = newest.id
	out.Updated = newest.updated

	entries, err := readTranscript(newest.path)
	if err != nil {
		out.Err = err
		return out
	}
	scanClaude(entries, time.Now(), out.Live, &out)
	return out
}

// claudeScan is the running state of a transcript walk.
type claudeScan struct {
	// openTools holds tool calls that have been issued but whose result has
	// not arrived, keyed by the tool_use id. Whatever is left at the end is
	// what the agent is doing right now.
	openTools map[string]*Activity
	// openOrder preserves issue order so the newest open call wins, since a
	// map has none.
	openOrder []string
	// sidechainUUIDs is every subagent entry's uuid, used to tell a subagent
	// root (parent outside the set) from a subagent continuation.
	sidechainUUIDs map[string]bool

	sawTurnEnd                      bool
	lastPromptIdx, lastTurnEndIdx   int
	lastAssistantIdx, lastTodoIdx   int
	lastPromptAt, lastActivityAt    time.Time
	lastUserText, lastAssistantText string
	todos                           Todos
}

// scanClaude walks a transcript oldest-first and fills in h.
//
// live says whether a Claude Code process is actually running in the
// directory. It gates the busy determination: a transcript whose last turn
// never ended — because the program was killed, or the machine rebooted —
// would otherwise report an agent as busy forever, with nothing left running
// to ever finish the turn.
func scanClaude(entries []claudeEntry, now time.Time, live bool, h *Health) {
	s := claudeScan{
		openTools:        map[string]*Activity{},
		sidechainUUIDs:   map[string]bool{},
		lastPromptIdx:    -1,
		lastTurnEndIdx:   -1,
		lastAssistantIdx: -1,
		lastTodoIdx:      -1,
	}

	for i := range entries {
		e := &entries[i]
		if e.IsSidechain {
			s.scanSidechain(e, h)
			continue
		}
		if !e.Timestamp.IsZero() {
			s.lastActivityAt = e.Timestamp
		}
		switch e.Type {
		case "system":
			if e.Subtype == "turn_duration" {
				s.sawTurnEnd = true
				s.lastTurnEndIdx = i
				h.Worked += time.Duration(e.DurationMs) * time.Millisecond
			}
		case "user":
			s.scanUser(i, e)
		case "assistant":
			s.scanAssistant(i, e, h)
		}
	}

	h.LastUser = s.lastUserText
	// Only report what the agent said if it said it after the latest prompt;
	// see Health.LastAssistant for why.
	if s.lastAssistantIdx > s.lastPromptIdx {
		h.LastAssistant = s.lastAssistantText
	}
	if !s.lastPromptAt.IsZero() {
		h.SincePrompt = now.Sub(s.lastPromptAt)
	}

	h.Busy = live && s.busy()
	if h.Busy {
		h.State = StateBusy
		if !s.lastActivityAt.IsZero() {
			h.Working = now.Sub(s.lastActivityAt)
		}
		h.Activity = s.currentActivity()
	}

	h.Todos = s.todos
	// A finished plan is never cleared, so drop it once a newer prompt has
	// arrived — the same reasoning as the opencode harness's version.
	if h.Todos.resolved() && s.lastPromptIdx > s.lastTodoIdx {
		h.Todos = Todos{}
	}
}

// busy reports whether the newest turn is still open.
//
// The turn_duration marker is authoritative when the transcript has any: a
// prompt more recent than the last one means the agent has not finished.
// Transcripts written by versions that predate the marker fall back to the
// shape of the conversation — an unanswered prompt, or a tool call with no
// result yet.
func (s *claudeScan) busy() bool {
	if s.sawTurnEnd {
		return s.lastPromptIdx > s.lastTurnEndIdx
	}
	return s.lastPromptIdx > s.lastAssistantIdx || len(s.openTools) > 0
}

// scanSidechain accounts for a Task subagent's entry. Its tokens are real
// spend on the feature's behalf and are counted; its messages are not part
// of the conversation you are having and are otherwise ignored.
func (s *claudeScan) scanSidechain(e *claudeEntry, h *Health) {
	s.sidechainUUIDs[e.UUID] = true
	if e.ParentUUID != nil && !s.sidechainUUIDs[*e.ParentUUID] {
		h.SubSessions++
	}
	if e.Type == "assistant" {
		addUsage(h, e)
	}
}

// scanUser handles a user entry, which is either something you typed or the
// result of a tool call being fed back to the model. Only the former counts
// as a prompt.
func (s *claudeScan) scanUser(i int, e *claudeEntry) {
	if e.IsMeta || e.Message == nil {
		return
	}
	var text string
	prompt := true
	for _, b := range e.Message.blocks() {
		switch b.Type {
		case "tool_result":
			// A tool result is the harness talking to the model, not you
			// talking to it; it closes the call it names.
			prompt = false
			if b.ToolUseID != "" {
				s.closeTool(b.ToolUseID)
			}
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				text = b.Text
			}
		}
	}
	if !prompt || text == "" {
		return
	}
	s.lastUserText = previewText(text)
	s.lastPromptIdx = i
	if !e.Timestamp.IsZero() {
		s.lastPromptAt = e.Timestamp
	}
}

func (s *claudeScan) scanAssistant(i int, e *claudeEntry, h *Health) {
	addUsage(h, e)
	if e.Message == nil {
		return
	}
	if e.Message.Model != "" {
		h.Model = e.Message.Model
	}
	blocks := e.Message.blocks()

	// An error belongs to the turn that hit it, and an error message is not
	// something the agent said. A later assistant message means the model
	// got through, so an earlier error stops being the current state —
	// without this it would be reported for the rest of the conversation.
	if e.IsAPIErrorMessage {
		h.LastError = previewText(firstText(blocks))
		return
	}
	h.LastError = ""

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			s.lastAssistantText = previewText(b.Text)
			s.lastAssistantIdx = i
		case "tool_use":
			s.openTool(b, e.Timestamp)
			if strings.EqualFold(b.Name, "todowrite") {
				s.lastTodoIdx = i
				s.todos = todosFrom(b.Input)
			}
		}
	}
}

// firstText returns the first non-blank text block, "" when there is none.
func firstText(blocks []claudeBlock) string {
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return b.Text
		}
	}
	return ""
}

func (s *claudeScan) openTool(b claudeBlock, at time.Time) {
	if b.ID == "" {
		return
	}
	s.openTools[b.ID] = &Activity{
		Tool:  b.Name,
		Title: firstLine(describeInput(b.Input)),
		Since: at,
	}
	s.openOrder = append(s.openOrder, b.ID)
}

func (s *claudeScan) closeTool(id string) {
	delete(s.openTools, id)
}

// currentActivity returns the most recently issued tool call that has not
// come back, nil when none is outstanding.
func (s *claudeScan) currentActivity() *Activity {
	for i := len(s.openOrder) - 1; i >= 0; i-- {
		if act, ok := s.openTools[s.openOrder[i]]; ok {
			return act
		}
	}
	return nil
}

func addUsage(h *Health, e *claudeEntry) {
	if e.Message != nil {
		u := e.Message.Usage
		h.Tokens.Input += u.Input
		h.Tokens.Output += u.Output
		h.Tokens.Cache.Read += u.CacheRead
		h.Tokens.Cache.Write += u.CacheWrite
	}
	h.Cost += e.CostUSD
}

// todosFrom reads a TodoWrite call's argument. Claude Code's task list has
// the same three statuses opencode's does, minus "cancelled".
func todosFrom(in map[string]any) Todos {
	raw, ok := in["todos"].([]any)
	if !ok {
		return Todos{}
	}
	var t Todos
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status, _ := m["status"].(string)
		content, _ := m["content"].(string)
		t.count(status, content)
	}
	return t
}

// claudeRunning reports whether a Claude Code process is working in dir.
//
// There is no unit to ask and no socket to connect to, so the process table
// is the only ground truth available. /proc is scanned for a process whose
// working directory is dir (or somewhere beneath it, since you may well have
// started it from a subdirectory) and whose command line names the claude
// binary.
//
// Best-effort by design: every failure — an unreadable /proc entry, a
// process that exited mid-scan, a platform without /proc — is treated as
// "not this one" rather than as an error, and a false negative only costs
// the busy/idle distinction, never correctness of anything canaveral does.
func claudeRunning(dir string) bool {
	want := filepath.Clean(dir)
	if resolved, err := filepath.EvalSymlinks(want); err == nil {
		want = resolved
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd"))
		if err != nil {
			continue
		}
		if cwd != want && !strings.HasPrefix(cwd, want+string(filepath.Separator)) {
			continue
		}
		if isClaudeCmdline(filepath.Join("/proc", e.Name(), "cmdline")) {
			return true
		}
	}
	return false
}

// isClaudeCmdline reports whether a /proc cmdline names the claude binary,
// either directly or as the script argument of an interpreter — an npm
// install runs as `node .../claude`, a native one simply as `claude`.
//
// Only the first two arguments are considered. A later one saying "claude"
// is a word in a prompt or a path being edited, not the program being run,
// and matching it would report every shell that ever mentioned the tool.
func isClaudeCmdline(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	if len(args) > 2 {
		args = args[:2]
	}
	for _, arg := range args {
		if arg != "" && filepath.Base(arg) == "claude" {
			return true
		}
	}
	return false
}

// Fork copies a conversation into dir so a new feature starts from a
// namespace sibling's context instead of from nothing.
//
// Claude Code has no fork of its own and no way to move a session between
// directories: a session belongs to the directory it was started in, because
// that directory is literally how its transcript is filed. Copying the
// transcript into the destination's project directory under a fresh id is
// the whole of what a fork means here — the copy is a complete, independent
// conversation that `claude --resume` opens like any other.
//
// The per-entry cwd and session id are rewritten as the copy is made, so the
// new session does not carry the source worktree's path around in its own
// history. Nothing else is touched: the messages are the point.
func (claude) Fork(_ context.Context, c Conn, sessionID, dir string) (string, error) {
	src, err := transcriptPath(c.Dir, sessionID)
	if err != nil {
		return "", err
	}
	dstDir, err := projectDir(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", dstDir, err)
	}

	newID, err := uuid4()
	if err != nil {
		return "", err
	}
	entries, err := readRawTranscript(src)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", src, err)
	}

	dst := filepath.Join(dstDir, newID+".jsonl")
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", dst, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, obj := range entries {
		rehome(obj, newID, dir)
		b, err := json.Marshal(obj)
		if err != nil {
			continue
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			return "", fmt.Errorf("write %s: %w", dst, err)
		}
	}
	if err := w.Flush(); err != nil {
		return "", fmt.Errorf("write %s: %w", dst, err)
	}
	return newID, nil
}

// rehome points one copied transcript entry at its new session and worktree.
// Both spellings of the session key are rewritten: Claude Code writes
// sessionId on every entry and session_id on assistant ones.
func rehome(obj map[string]any, sessionID, dir string) {
	for _, k := range []string{"sessionId", "session_id"} {
		if _, ok := obj[k]; ok {
			obj[k] = sessionID
		}
	}
	if _, ok := obj["cwd"]; ok {
		obj["cwd"] = dir
	}
}

// readRawTranscript decodes a transcript into generic maps, so a copy keeps
// every field rather than only the ones claudeEntry declares.
func readRawTranscript(path string) ([]map[string]any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(sc.Bytes(), &obj); err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out, sc.Err()
}

// transcriptPath locates one session's file under dir's project directory.
func transcriptPath(dir, sessionID string) (string, error) {
	if dir == "" || sessionID == "" {
		return "", fmt.Errorf("claude: need both a directory and a session id to fork")
	}
	pd, err := projectDir(dir)
	if err != nil {
		return "", err
	}
	// Session ids come from directory listings and are never user input, but
	// a path separator arriving here would escape the project directory, so
	// it is refused rather than cleaned.
	if strings.ContainsAny(sessionID, `/\`) {
		return "", fmt.Errorf("claude: invalid session id %q", sessionID)
	}
	p := filepath.Join(pd, sessionID+".jsonl")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("claude: no transcript for session %s in %s", sessionID, dir)
	}
	return p, nil
}

// uuid4 generates the random UUID Claude Code names a session file with.
func uuid4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
