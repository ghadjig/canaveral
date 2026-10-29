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
// Some things do not survive the lack of a server, and they are called out
// here rather than faked. A permission prompt is drawn and answered entirely
// in the TUI and never reaches the transcript until it has been resolved, so
// an agent stopped on one looks busy: from the transcript's point of view it
// is still mid-turn. A question (AskUserQuestion) or a plan awaiting approval
// (ExitPlanMode) is different — the tool call is written as soon as it is
// made, and stays open until you answer — so those do report StateWaiting.
//
// Spend is only partly visible. Claude Code writes no cost per message; it
// writes a cost-state entry holding the whole run's total when the process
// exits. So Health.Cost covers every run of the session that has finished,
// and the run in progress joins it when it ends.
//
// Background work is visible too. A shell command or subagent started in
// the background returns at once with a task id, the turn ends, and a
// task-notification naming that id arrives when it finishes, waking the
// agent. Between the two the agent is reported as StateBackground rather
// than idle — but only for tasks started since the last prompt you typed.
// A dev server the agent started an hour ago and left running would
// otherwise hold the feature in StateBackground for as long as it ran,
// although nothing is waiting on it; once you have moved the conversation
// on, the agent has too. Such tasks are still listed in Health.Background.
//
// There is no push notification of any kind, so Watch polls the transcript
// directory instead.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

func (claude) Discover(context.Context, string, time.Duration, func() error) (Endpoint, error) {
	return Endpoint{}, fmt.Errorf("claude has no server to discover")
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
//
// The --settings add the hooks that tell canaveral when a question or a
// permission dialog is up; see claudehook.go. They are merged with your own
// settings, not a replacement for them.
//
// The brief goes in --append-system-prompt rather than as a first prompt:
// it then costs no turn, sits outside the conversation, and is given again
// each time the window starts, so it stays current as siblings come and go.
func (claude) AttachArgv(c Conn, cont bool) []string {
	argv := []string{"claude"}
	if settings := claudeSettings(); settings != "" {
		argv = append(argv, "--settings", settings)
	}
	if c.Brief != "" {
		argv = append(argv, "--append-system-prompt", c.Brief)
	}
	if cont {
		argv = append(argv, "--continue")
	}
	return argv
}

// ClientEnv is empty: there is no server to authenticate to.
func (claude) ClientEnv(Conn) map[string]string { return nil }

func (claude) SessionFlag(id string) string {
	if id == "" {
		return ""
	}
	return "--resume " + id
}

// claudeWatchInterval is how often Watch looks at the transcripts. A stat
// per session file is all it costs, and a second is well inside the time a
// person takes to glance at a status bar after typing a prompt.
const claudeWatchInterval = time.Second

// Watch calls fn whenever a transcript in c.Dir changes.
//
// There is no server to push events, but the transcript is appended to as
// things happen, so a change to it is the event. Without this the watch
// would only re-probe on its safety interval, and an agent that had just
// been given a prompt would sit at idle for up to that long.
func (claude) Watch(ctx context.Context, c Conn, fn func()) error {
	if c.Dir == "" {
		return ErrNoEvents
	}
	t := time.NewTicker(claudeWatchInterval)
	defer t.Stop()
	last := transcriptsFingerprint(c.Dir)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if fp := transcriptsFingerprint(c.Dir); fp != last {
				last = fp
				fn()
			}
		}
	}
}

// transcriptsFingerprint summarises the state of dir's transcripts, changing
// whenever one is created or written to. Size is included alongside the
// modification time because two appends within the filesystem's timestamp
// granularity would otherwise look like one.
func transcriptsFingerprint(dir string) string {
	sessions, err := transcripts(dir)
	if err != nil || len(sessions) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range sessions {
		info, err := os.Stat(s.path)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", s.id, info.Size(), info.ModTime().UnixNano())
		// A subagent writes to its own file, and says it has finished in
		// another, neither of which touches the conversation's.
		subs, _ := os.ReadDir(subagentsDir(s.path))
		for _, e := range subs {
			if info, err := e.Info(); err == nil {
				fmt.Fprintf(&b, "%s:%d:%d;", e.Name(), info.Size(), info.ModTime().UnixNano())
			}
		}
	}
	return b.String()
}

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

// HistoryHint says where an agent of tool working in dir keeps its
// conversations, for another agent to read; "" when there are none to read,
// or they are not in files (opencode keeps them behind its server).
func HistoryHint(tool, dir string) string {
	if tool != "claude" {
		return ""
	}
	pd, err := projectDir(dir)
	if err != nil {
		return ""
	}
	if info, err := os.Stat(pd); err != nil || !info.IsDir() {
		return ""
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(pd, home+"/") {
		pd = "~" + strings.TrimPrefix(pd, home)
	}
	return "Claude Code transcripts in " + pd + "/ (JSONL, newest file is the latest conversation)"
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
	IsSidechain bool `json:"isSidechain"`
	IsMeta      bool `json:"isMeta"`
	// IsCompactSummary marks the user entry Claude Code writes after
	// compacting the conversation: the summary it continues from, dressed as
	// a prompt. Compaction also happens while the agent sits idle, and no
	// turn follows to close it, so taken for a prompt it reads as busy for
	// good.
	IsCompactSummary bool      `json:"isCompactSummary"`
	Timestamp        time.Time `json:"timestamp"`
	// DurationMs is set on the system/turn_duration entry Claude Code writes
	// when a turn ends: the one unambiguous "the agent has stopped" marker
	// in the file, and the source of Worked.
	DurationMs int64 `json:"durationMs"`
	// CostUSD is present on older transcripts only; newer ones record no
	// cost per message.
	CostUSD float64 `json:"costUSD"`
	// TotalCostUSD and StartTime are set on the cost-state entry Claude Code
	// writes as a process exits: that run's total spend, and when the run
	// began. The total is per run, not per session — resuming a session
	// starts it from zero — so runs are told apart by StartTime and summed.
	TotalCostUSD      float64        `json:"totalCostUSD"`
	StartTime         int64          `json:"startTime"`
	IsAPIErrorMessage bool           `json:"isApiErrorMessage"`
	Message           *claudeMessage `json:"message"`
	// ToolUseResult is the structured result of the tool call a user entry
	// answers. An object for most tools and a bare string for some, so it is
	// decoded on demand by launchedTask and stoppedTask.
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	// Attachment carries, among much else, a prompt that arrived while the
	// agent was mid-turn — which is how a task-notification is delivered
	// when the agent is busy at the moment its task finishes.
	Attachment *claudeAttachment `json:"attachment"`
}

type claudeAttachment struct {
	Type   string `json:"type"`
	Prompt string `json:"prompt"`
}

// claudeToolResult is the part of a toolUseResult that concerns background
// work.
type claudeToolResult struct {
	// BackgroundTaskID is set when a shell command was started in the
	// background, or moved there after outliving its timeout.
	BackgroundTaskID string `json:"backgroundTaskId"`
	// AgentID and Status ("async_launched") are set when a subagent was
	// started in the background.
	AgentID string `json:"agentId"`
	Status  string `json:"status"`
	// TaskID is set on TaskStop's result: the task it stopped.
	TaskID string `json:"task_id"`
}

func (e *claudeEntry) toolResult() claudeToolResult {
	var r claudeToolResult
	if len(e.ToolUseResult) > 0 && e.ToolUseResult[0] == '{' {
		_ = json.Unmarshal(e.ToolUseResult, &r)
	}
	return r
}

// launchedTask returns the id of the background task this entry's tool
// result started, "" when it started none.
func (e *claudeEntry) launchedTask() string {
	r := e.toolResult()
	if r.BackgroundTaskID != "" {
		return r.BackgroundTaskID
	}
	if r.Status == "async_launched" {
		return r.AgentID
	}
	return ""
}

// taskNotification matches the task id in the message Claude Code feeds the
// model when a background task ends, however it ended: completed, failed
// and stopped are all written the same way.
var taskNotification = regexp.MustCompile(`(?s)<task-notification>.*?<task-id>([^<]+)</task-id>`)

// finishedTasks returns the ids of the background tasks text reports as
// ended.
func finishedTasks(text string) []string {
	var ids []string
	for _, m := range taskNotification.FindAllStringSubmatch(text, -1) {
		ids = append(ids, strings.TrimSpace(m[1]))
	}
	return ids
}

type claudeMessage struct {
	// ID is the API response's id. One response is written as several
	// entries, one per content block, each repeating the response's usage,
	// so this is what keeps it from being counted once per block.
	ID    string `json:"id"`
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
	s := scanClaude(entries, time.Now(), out.Live, &out)
	scanSubagents(newest.path, out.Live, &out)
	applyClaudeHooks(newest.id, s, &out)
	return out
}

// applyClaudeHooks reports the question or permission dialog the hooks say
// is up, which the transcript cannot show; see claudehook.go. Only an open
// turn can be waiting on one, which also rules out a state file left behind
// by a process that was killed mid-dialog.
func applyClaudeHooks(sessionID string, s *claudeScan, h *Health) {
	if !h.Busy || h.Pending != nil {
		return
	}
	if p := claudeHookPending(sessionID, func(id string) bool { return s.answered[id] }); p != nil {
		h.State = StateWaiting
		h.Pending = p
	}
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
	// openPending holds the open calls that are waiting on you rather than
	// running, keyed like openTools.
	openPending map[string]*Pending
	// answered is every tool call with a result, open in the walk or not:
	// Claude Code writes a call and its result together, so most never are.
	answered map[string]bool
	// sidechainUUIDs is every subagent entry's uuid, used to tell a subagent
	// root (parent outside the set) from a subagent continuation.
	sidechainUUIDs map[string]bool
	// counted is every API response whose usage has been added already.
	counted map[string]bool
	// runCost is each finished run's total, keyed by its start time. A run
	// can write its cost-state more than once; the last one is its total.
	runCost map[int64]float64
	// background holds the background tasks that have been started and not
	// reported finished, keyed by task id, with bgOrder keeping launch order
	// for the same reason openOrder exists.
	background map[string]Activity
	bgOrder    []string
	// launchIdx is the entry index each background task was started at, and
	// lastTypedIdx that of the last prompt you typed — a task-notification
	// excluded, since the turn it starts carries on the same piece of work.
	// A task started after lastTypedIdx is one the agent is waiting on.
	launchIdx    map[string]int
	lastTypedIdx int

	// lastCommandIdx is the last slash command you ran, which only opens a
	// turn when the model answers it (lastReplyIdx is any reply at all), and
	// lastErrorIdx the last API error, which ends a turn without the
	// turn_duration marker.
	lastCommandIdx, lastReplyIdx    int
	lastErrorIdx                    int
	sawTurnEnd                      bool
	lastPromptIdx, lastTurnEndIdx   int
	lastAssistantIdx, lastTodoIdx   int
	lastPromptAt, lastActivityAt    time.Time
	lastUserText, lastAssistantText string
	todos                           Todos
}

// scanClaude walks a transcript oldest-first and fills in h, returning the
// walk's state for what Probe reads alongside the transcript.
//
// live says whether a Claude Code process is actually running in the
// directory. It gates the busy determination: a transcript whose last turn
// never ended — because the program was killed, or the machine rebooted —
// would otherwise report an agent as busy forever, with nothing left running
// to ever finish the turn.
func scanClaude(entries []claudeEntry, now time.Time, live bool, h *Health) *claudeScan {
	s := &claudeScan{
		openTools:        map[string]*Activity{},
		openPending:      map[string]*Pending{},
		answered:         map[string]bool{},
		sidechainUUIDs:   map[string]bool{},
		counted:          map[string]bool{},
		runCost:          map[int64]float64{},
		background:       map[string]Activity{},
		launchIdx:        map[string]int{},
		lastTypedIdx:     -1,
		lastCommandIdx:   -1,
		lastReplyIdx:     -1,
		lastErrorIdx:     -1,
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
		case "cost-state":
			s.runCost[e.StartTime] = e.TotalCostUSD
			// Written as the process exits, and background tasks are the
			// process's children: none of them outlived it.
			s.background = map[string]Activity{}
		case "attachment":
			if e.Attachment != nil {
				s.finish(finishedTasks(e.Attachment.Prompt))
			}
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
	// An error belongs to the turn that hit it; anything you have done since
	// has moved on from it.
	if s.lastErrorIdx < max(s.lastPromptIdx, s.lastCommandIdx) {
		h.LastError = ""
	}
	// Only report what the agent said if it said it after the latest prompt;
	// see Health.LastAssistant for why.
	if s.lastAssistantIdx > s.lastPromptIdx {
		h.LastAssistant = s.lastAssistantText
	}
	if !s.lastPromptAt.IsZero() {
		h.SincePrompt = now.Sub(s.lastPromptAt)
	}

	for _, c := range s.runCost {
		h.Cost += c
	}

	h.Busy = live && s.busy()
	if h.Busy {
		h.State = StateBusy
		if !s.lastActivityAt.IsZero() {
			h.Working = now.Sub(s.lastActivityAt)
		}
		h.Activity = s.currentActivity()
		if p := s.pending(); p != nil {
			h.State = StateWaiting
			h.Pending = p
		}
	}

	if live {
		awaited := false
		for _, id := range s.bgOrder {
			if act, ok := s.background[id]; ok {
				h.Background = append(h.Background, act)
				awaited = awaited || s.launchIdx[id] > s.lastTypedIdx
			}
		}
		if !h.Busy && awaited {
			h.State = StateBackground
		}
	}

	h.Todos = s.todos
	// A finished plan is never cleared, so drop it once a newer prompt has
	// arrived — the same reasoning as the opencode harness's version.
	if h.Todos.resolved() && s.lastPromptIdx > s.lastTodoIdx {
		h.Todos = Todos{}
	}
	return s
}

// busy reports whether the newest turn is still open.
//
// The turn_duration marker is authoritative when the transcript has any: a
// prompt more recent than the last one means the agent has not finished.
// Transcripts written by versions that predate the marker fall back to the
// shape of the conversation — an unanswered prompt, or a tool call with no
// result yet.
//
// A slash command counts as the prompt only once the model has replied to
// it: most (/login, /model, /exit) never reach the model at all. An API error
// closes a turn as surely as the marker does, and Claude Code writes none
// after one.
func (s *claudeScan) busy() bool {
	open := s.lastPromptIdx
	if s.lastCommandIdx > open && s.lastReplyIdx > s.lastCommandIdx {
		open = s.lastCommandIdx
	}
	if s.sawTurnEnd {
		return open > max(s.lastTurnEndIdx, s.lastErrorIdx)
	}
	return open > max(s.lastAssistantIdx, s.lastErrorIdx) || len(s.openTools) > 0
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
		s.addUsage(h, e)
	}
}

// scanUser handles a user entry, which is either something you typed or the
// result of a tool call being fed back to the model. Only the former counts
// as a prompt.
func (s *claudeScan) scanUser(i int, e *claudeEntry) {
	if e.Message == nil {
		return
	}
	blocks := e.Message.blocks()
	// Background bookkeeping comes before the meta check: a notification can
	// arrive in an entry that is not something you typed.
	launched := e.launchedTask()
	if stopped := e.toolResult().TaskID; stopped != "" {
		s.finish([]string{stopped})
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			if launched != "" {
				if act, ok := s.openTools[b.ToolUseID]; ok {
					s.launch(i, launched, *act, e.Timestamp)
				}
			}
		case "text":
			s.finish(finishedTasks(b.Text))
		}
	}
	if e.IsMeta || e.IsCompactSummary {
		return
	}

	var text string
	prompt := true
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			// A tool result is the harness talking to the model, not you
			// talking to it; it closes the call it names.
			prompt = false
			if b.ToolUseID != "" {
				s.closeTool(b.ToolUseID)
				s.answered[b.ToolUseID] = true
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
	if local, ok := localCommand(text); ok {
		if local != "" {
			s.lastUserText = local
			s.lastCommandIdx = i
		}
		return
	}
	s.lastUserText = previewText(text)
	s.lastPromptIdx = i
	if !strings.Contains(text, "<task-notification>") {
		s.lastTypedIdx = i
	}
	if !e.Timestamp.IsZero() {
		s.lastPromptAt = e.Timestamp
	}
}

func (s *claudeScan) scanAssistant(i int, e *claudeEntry, h *Health) {
	s.addUsage(h, e)
	if e.Message == nil {
		return
	}
	// Claude Code stamps the messages it makes up itself, API errors among
	// them, with a model of "<synthetic>"; it is not the one in use.
	if e.Message.Model != "" && e.Message.Model != "<synthetic>" {
		h.Model = e.Message.Model
	}
	blocks := e.Message.blocks()

	// An error belongs to the turn that hit it, and an error message is not
	// something the agent said. A later assistant message means the model
	// got through, so an earlier error stops being the current state —
	// without this it would be reported for the rest of the conversation.
	s.lastReplyIdx = i
	if e.IsAPIErrorMessage {
		s.lastErrorIdx = i
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

var commandName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)

// localCommand recognises the entries Claude Code writes for what you run in
// its prompt rather than say to the model: a slash command, its output, and a
// ! shell command with its output. For a command it returns what to show as
// the last thing you did; for output it returns "" and true.
func localCommand(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if m := commandName.FindStringSubmatch(t); m != nil && strings.HasPrefix(t, "<command-") {
		return m[1], true
	}
	if strings.HasPrefix(t, "<bash-input>") {
		return previewText("! " + strings.TrimSuffix(strings.TrimPrefix(t, "<bash-input>"), "</bash-input>")), true
	}
	for _, p := range []string{"<local-command-stdout>", "<local-command-stderr>", "<bash-stdout>", "<bash-stderr>"} {
		if strings.HasPrefix(t, p) {
			return "", true
		}
	}
	return "", false
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
	if p := pendingFor(b); p != nil {
		s.openPending[b.ID] = p
	}
	s.openOrder = append(s.openOrder, b.ID)
}

func (s *claudeScan) closeTool(id string) {
	delete(s.openTools, id)
	delete(s.openPending, id)
}

// launch records a background task started by the call act describes. Its
// clock starts when the task was handed off, which is when the call that
// started it returned.
func (s *claudeScan) launch(i int, id string, act Activity, at time.Time) {
	s.launchIdx[id] = i
	if !at.IsZero() {
		act.Since = at
	}
	if _, ok := s.background[id]; !ok {
		s.bgOrder = append(s.bgOrder, id)
	}
	s.background[id] = act
}

func (s *claudeScan) finish(ids []string) {
	for _, id := range ids {
		delete(s.background, id)
	}
}

// pending returns what the agent is waiting on you for, nil when nothing.
// The newest open call wins, the same as for currentActivity.
func (s *claudeScan) pending() *Pending {
	for i := len(s.openOrder) - 1; i >= 0; i-- {
		if p, ok := s.openPending[s.openOrder[i]]; ok {
			return p
		}
	}
	return nil
}

// pendingFor describes a tool call that blocks on you, nil for any other.
//
// These two are the only calls whose being open means "waiting for an
// answer" rather than "running": the tool's whole job is to put something
// in front of you, and its result is your reply.
func pendingFor(b claudeBlock) *Pending {
	switch b.Name {
	case "AskUserQuestion":
		p := &Pending{Kind: BlockQuestion, Header: "question"}
		qs, _ := b.Input["questions"].([]any)
		for _, q := range qs {
			m, ok := q.(map[string]any)
			if !ok {
				continue
			}
			// Several questions can be asked at once; the first is enough to
			// say what it is about, and its options are what a widget can show.
			if h, _ := m["header"].(string); h != "" {
				p.Header = h
			}
			p.Detail, _ = m["question"].(string)
			opts, _ := m["options"].([]any)
			for _, o := range opts {
				if om, ok := o.(map[string]any); ok {
					if l, _ := om["label"].(string); l != "" {
						p.Options = append(p.Options, l)
					}
				}
			}
			break
		}
		return p
	case "ExitPlanMode":
		plan, _ := b.Input["plan"].(string)
		return &Pending{Kind: BlockPermission, Header: "approve plan", Detail: previewText(plan)}
	}
	return nil
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

// addUsage adds an assistant entry's tokens and cost, once per API response.
func (s *claudeScan) addUsage(h *Health, e *claudeEntry) {
	if e.Message != nil {
		if id := e.Message.ID; id != "" {
			if s.counted[id] {
				return
			}
			s.counted[id] = true
		}
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
