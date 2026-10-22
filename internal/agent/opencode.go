package agent

// The opencode harness. opencode runs headless as an HTTP server that
// canaveral starts in a systemd unit and then interrogates over REST, which
// is why it can report far more than a harness with no server: pending
// questions and permission requests, retry state, and per-session token and
// cost totals all come straight from the running process.
//
// This speaks opencode 2's API, which lives entirely under /api and refuses
// any request without HTTP Basic auth. Everything outside /api belongs to the
// bundled web UI, which answers any path at all with 200 and an HTML page —
// so a path that has moved does not fail with a 404 here, it fails to
// decode. That is how the first opencode 2 release left canaveral blind
// without a single error that said why.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/ocevents"
)

// opencode implements Harness for opencode.
type opencode struct{}

func (opencode) Name() string { return "opencode" }

func (opencode) Resolve() (string, error) { return resolveBin("opencode") }

func (opencode) Serves() bool { return true }

// ServeCmd builds the shell command that starts a headless opencode server.
func (opencode) ServeCmd(bin string) string {
	// --port 0 lets the kernel pick a free port, which we then read from the log.
	return fmt.Sprintf("exec %s serve --hostname 127.0.0.1 --port 0", shellQuote(bin))
}

// serverUser is the Basic auth username opencode's server accepts. Only the
// password varies.
const serverUser = "opencode"

// passwordEnv is the variable an opencode client reads the server password
// from. The server reads it too, along with the older spelling below.
const passwordEnv = "OPENCODE_PASSWORD"

func (opencode) EnvFor(sel Selection) map[string]string {
	env := map[string]string{
		// Blank rather than absent, so a password set in a manifest's [env]
		// cannot reach the server. Given none, opencode generates a fresh
		// one on every start and prints it next to its address, which is
		// where Discover reads it from — exactly as it does the port, and
		// for the same reason: a unit adopted after canaveral lost its
		// state can still be reached.
		passwordEnv:                "",
		"OPENCODE_SERVER_PASSWORD": "",
	}
	if sel.Model != "" {
		env["OPENCODE_MODEL"] = sel.Model
	}
	if sel.Agent != "" {
		env["OPENCODE_AGENT"] = sel.Agent
	}
	return env
}

// AttachArgv opens an opencode TUI against the running server.
//
// The directory is passed even though the caller already runs this in
// c.Dir: an opencode client takes its session scope from the argument, not
// from its own working directory.
func (opencode) AttachArgv(c Conn, cont bool) []string {
	argv := []string{"opencode", "--server", c.URL}
	if c.Dir != "" {
		argv = append(argv, c.Dir)
	}
	if cont {
		argv = append(argv, "--continue")
	}
	return argv
}

// ClientEnv hands the TUI the server's password. A client connecting with
// --server reads it only from the environment; there is no flag for it.
func (opencode) ClientEnv(c Conn) map[string]string {
	if c.Password == "" {
		return nil
	}
	return map[string]string{passwordEnv: c.Password}
}

func (opencode) SessionFlag(id string) string {
	if id == "" {
		return ""
	}
	return "--session " + id
}

func (opencode) Watch(ctx context.Context, c Conn, fn func()) error {
	if c.URL == "" {
		return ErrNoEvents
	}
	return ocevents.Watch(ctx, c.URL, c.Password, func(ev ocevents.Event) {
		if relevantEvent(ev.Type) {
			invalidateRecent(c.URL)
			fn()
		}
	})
}

// relevantEvent filters opencode's event firehose down to the ones that can
// change a feature's headline state.
//
// opencode emits a great deal per turn (text and reasoning deltas, tool
// input as it streams); reacting to all of it would mean re-probing every
// agent dozens of times a second for no visible difference. The list is
// deliberately broad because an event only triggers a re-read — being
// slightly over-inclusive costs one HTTP round trip, while being
// under-inclusive would mean missing a state change entirely.
//
// Todo progress has no event of its own: it is a todowrite tool call, and
// arrives as session.tool.success.
func relevantEvent(t string) bool {
	switch t {
	case "server.connected",
		"session.created", "session.deleted", "session.moved",
		"session.execution.started", "session.execution.succeeded",
		"session.execution.failed", "session.execution.interrupted",
		"session.step.started", "session.step.ended", "session.step.failed",
		"session.text.ended",
		"session.tool.called", "session.tool.success", "session.tool.failed",
		"session.retry.scheduled",
		"session.compaction.started", "session.compaction.ended",
		"permission.asked", "permission.replied", "permission.rejected",
		"form.created", "form.replied", "form.cancelled":
		return true
	}
	return false
}

// listenRe matches the line opencode prints once its HTTP server is bound.
var listenRe = regexp.MustCompile(`listening on (https?://[^\s]+)`)

// passwordRe matches the line opencode prints with the password it
// generated, right after listenRe's.
var passwordRe = regexp.MustCompile(`(?m)^server password (\S+)`)

// Discover polls the agent log until opencode reports its listen address
// and password.
//
// Parsing the log rather than pre-allocating a port avoids a TOCTOU race where
// another process claims the port between our probe and opencode's bind.
func (opencode) Discover(ctx context.Context, logPath string, timeout time.Duration, alive func() error) (Endpoint, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()

	var ep Endpoint
	for {
		if alive != nil {
			if err := alive(); err != nil {
				return Endpoint{}, err
			}
		}
		if b, err := os.ReadFile(logPath); err == nil {
			if m := listenRe.FindSubmatch(b); m != nil {
				ep.URL = string(m[1])
			}
			if m := passwordRe.FindSubmatch(b); m != nil {
				ep.Password = string(m[1])
			}
			if ep.URL != "" && ep.Password != "" {
				return ep, nil
			}
		}
		select {
		case <-ctx.Done():
			if ep.URL != "" {
				// Every opencode 2 server prints one; an address with no
				// password is opencode 1, whose API this no longer speaks.
				return Endpoint{}, fmt.Errorf("opencode reported %s but no server password after %s; canaveral needs opencode 2 or later (see %s)", ep.URL, timeout, logPath)
			}
			return Endpoint{}, fmt.Errorf("timed out after %s waiting for opencode to report a listen address (see %s)", timeout, logPath)
		case <-ticker.C:
		}
	}
}

var client = &http.Client{Timeout: 4 * time.Second}

// modelRef names a model the way opencode does everywhere: on a session, on
// each assistant message.
type modelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant"`
}

type sessionInfo struct {
	ID string `json:"id"`
	// ParentID is set on subagent sessions, which the subagent tool creates
	// in the same directory as the conversation that spawned them. A fork
	// has none: it is a conversation of its own.
	ParentID string `json:"parentID"`
	Location struct {
		Directory string `json:"directory"`
	} `json:"location"`
	Model  modelRef `json:"model"`
	Tokens Tokens   `json:"tokens"`
	Cost   float64  `json:"cost"`
	Time   struct {
		Updated int64 `json:"updated"`
	} `json:"time"`
}

type sessionListResp struct {
	Data []sessionInfo `json:"data"`
}

// structuredError is how opencode reports a failure on a message or a tool
// call.
type structuredError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// sessionMessage is one entry of GET /api/session/{id}/message.
//
// Messages are a tagged union on Type. Only "user" and "assistant" carry
// anything Probe reads; the rest ("idle" closing a run, "location-switched"
// after a move, "shell", "synthetic" and so on) are bookkeeping, and are
// kept only so that "the newest message" means the same thing here as it
// does to opencode.
type sessionMessage struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed"`
	} `json:"time"`
	Model modelRef `json:"model"`
	// Text is a user message's prompt. An assistant's text is in Content.
	Text    string           `json:"text"`
	Content []messagePart    `json:"content"`
	Finish  string           `json:"finish"`
	Error   *structuredError `json:"error"`
	// Retry is set while the turn is waiting to retry a failed provider
	// call, and replaced on the next attempt.
	Retry *struct {
		Attempt int `json:"attempt"`
	} `json:"retry"`
}

// messagePart is one piece of an assistant message's content. Only text and
// tool parts are used; reasoning carries nothing a status view needs.
type messagePart struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// Name is the tool's name, for a tool part.
	Name  string `json:"name"`
	State struct {
		// Status is streaming | running | completed | error.
		Status string `json:"status"`
		// Input is the call's arguments: an object, except while the
		// model is still streaming it, when it is the raw partial string.
		Input json.RawMessage `json:"input"`
	} `json:"state"`
	Time struct {
		Created int64 `json:"created"`
		Ran     int64 `json:"ran"`
	} `json:"time"`

	// Title describes the call and Todos holds a todowrite call's list.
	// Both are derived from Input by recentMessages, which then drops
	// Input: it can be an entire file's contents.
	Title string     `json:"-"`
	Todos []todoItem `json:"-"`
}

// requestTool identifies the message and tool call a question or
// permission request was raised from, which is what lets classify tell a
// genuinely open request from one whose turn has since ended.
type requestTool struct {
	MessageID string `json:"messageID"`
}

// permissionRequest is one entry of GET /api/permission/request.
type permissionRequest struct {
	SessionID string      `json:"sessionID"`
	Action    string      `json:"action"`
	Resources []string    `json:"resources"`
	Message   string      `json:"message"`
	Source    requestTool `json:"source"`
}

// formInfo is one entry of GET /api/session/{id}/form: something the agent
// is waiting for a person to fill in. The question tool is the usual source,
// with one field per question; an MCP server asking for authorization is
// another.
type formInfo struct {
	SessionID string `json:"sessionID"`
	Title     string `json:"title"`
	Metadata  struct {
		Tool requestTool `json:"tool"`
	} `json:"metadata"`
	Fields []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Options     []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"fields"`
}

// todoItem is one entry of a todowrite call's list.
type todoItem struct {
	Content string `json:"content"`
	Status  string `json:"status"`
}

// Probe reports what the agent rooted at c.Dir is currently doing.
//
// Sessions are filtered by location.directory because an opencode server
// exposes the user's entire global session history, not just this workspace's.
func (opencode) Probe(ctx context.Context, c Conn) Health {
	if c.URL == "" {
		return Health{Err: fmt.Errorf("no url")}
	}
	dir := c.Dir

	var list sessionListResp
	if err := getJSON(ctx, c, "/api/session?limit=100&order=desc&directory="+url.QueryEscape(dir), &list); err != nil {
		return Health{Err: err}
	}
	// A server that answered is by definition running, so Live and
	// Reachable are the same question for this harness.
	h := Health{Reachable: true, Live: true}

	mine := sessionsInDir(list.Data, dir)

	// Subagent sessions live in the same directory as the conversation that
	// created them, so a single conversation can look like several
	// sessions. Only top-level ones are candidates for "the" current
	// session: a subagent updates at almost the same instant as its parent,
	// so picking the newest of everything would flip between them and make
	// the reported model, state and numbers jump around at random.
	roots := topLevelSessions(mine)
	h.Sessions = len(roots)
	if len(roots) == 0 {
		h.State = StateIdle
		return h
	}

	newest := roots[0]
	h.Updated = time.UnixMilli(newest.Time.Updated)
	h.SessionID = newest.ID
	// The session's own model is what its next turn will use. The current
	// turn's, when there is one, overrides it below.
	h.Model, h.Variant, h.Provider = newest.Model.ID, newest.Model.Variant, newest.Model.ProviderID

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

	active := activeSessions(ctx, c)

	msgs, err := recentMessages(ctx, c, newest)
	if err != nil {
		// Large or unavailable message previews must not hide live state.
		h.RecentHistory = true
		h.State, h.Pending = classify(ctx, c, newest.ID, familyIDs, active, false, nil, nil)
		h.Busy = h.State == StateBusy || h.State == StateRetrying
		return h
	}

	// Messages drive only the current turn and elapsed work; totals come
	// from the session list above.
	//
	// Worked counts this conversation's recent turns only. A parent's turn stays
	// open while a subagent runs, so its duration already covers that work;
	// adding the subagents' own turns would double-count it.
	now := time.Now()
	turns := scanTurns(msgs, now)
	h.Worked = turns.worked
	h.RecentHistory = len(msgs) >= recentMessageLimit
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
		if cur.Model.ID != "" {
			h.Model, h.Variant, h.Provider = cur.Model.ID, cur.Model.Variant, cur.Model.ProviderID
		}
		h.Busy = cur.Time.Completed == nil
		if h.Busy {
			// Still generating; this turn has not contributed to Worked.
			h.Working = now.Sub(time.UnixMilli(cur.Time.Created))
		}
		if cur.Error != nil {
			h.LastError = cur.Error.Message
		}
	}

	// opencode has no endpoint for the task list: it is whatever the
	// newest finished todowrite call said, which is also how its own TUI
	// finds it. A list that old has scrolled out of the recent window
	// simply is not reported.
	h.Todos = turns.todos
	// A list with nothing left in_progress or pending is a finished plan.
	// opencode never clears it, so without this check it would go on
	// reporting the same "N/N done" bar forever, including once the
	// conversation has moved on to unrelated work. If a newer prompt has
	// arrived since the list was last touched, treat it the same way as a
	// stale LastAssistant above and drop it, rather than showing finished
	// work next to whatever the agent is doing now.
	if h.Todos.resolved() && turns.lastUserIdx > turns.lastTodoWriteIdx {
		h.Todos = Todos{}
	}
	h.State, h.Pending = classify(ctx, c, newest.ID, familyIDs, active, h.Busy, turns.cur, turns.completedByID)
	h.Busy = h.State == StateBusy || h.State == StateRetrying
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
	// completedByID records whether each assistant message has finished,
	// keyed by message ID. classify uses it to tell a genuinely open
	// question or permission request from one whose turn has already
	// ended — see classify's comment for why the pending lists cannot
	// always be trusted alone.
	completedByID map[string]bool
	// cur is the last assistant message seen — the current turn, since
	// messages arrive oldest-first.
	cur                           *sessionMessage
	lastUser, lastAssistant       string
	lastUserIdx, lastAssistantIdx int
	lastTodoWriteIdx              int
	// todos is the list from the newest finished todowrite call.
	todos       Todos
	sincePrompt time.Duration
}

// scanTurns walks a session's messages for the data Probe needs from the
// current turn: how long assistant turns have taken so far, the current
// (last) assistant message, the newest user/assistant text, and the newest
// todo list and when it was written. now anchors SincePrompt.
func scanTurns(msgs []sessionMessage, now time.Time) turnScan {
	t := scanCompletedTurns(msgs)
	scanConversationText(msgs, &t)
	if t.lastUserIdx >= 0 {
		if c := msgs[t.lastUserIdx].Time.Created; c > 0 {
			t.sincePrompt = now.Sub(time.UnixMilli(c))
		}
	}
	return t
}

// scanCompletedTurns computes worked and completedByID, and finds the
// current (last) assistant message. Messages arrive oldest-first, so the
// last one seen with type "assistant" is the current turn.
func scanCompletedTurns(msgs []sessionMessage) turnScan {
	t := turnScan{lastUserIdx: -1, lastAssistantIdx: -1, lastTodoWriteIdx: -1}
	t.completedByID = make(map[string]bool, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		if m.Type != "assistant" {
			continue
		}
		t.completedByID[m.ID] = m.Time.Completed != nil
		if m.Time.Completed != nil {
			t.worked += time.UnixMilli(*m.Time.Completed).Sub(time.UnixMilli(m.Time.Created))
		}
		t.cur = m
	}
	return t
}

// scanConversationText fills in the newest text on each side of the
// conversation, and the newest todo list and when it was written. An
// assistant message can hold several text parts, so simply keeping the last
// non-empty one seen lands on the most recent.
func scanConversationText(msgs []sessionMessage, t *turnScan) {
	for i := range msgs {
		switch msgs[i].Type {
		case "user":
			if strings.TrimSpace(msgs[i].Text) != "" {
				t.lastUser = previewText(msgs[i].Text)
				t.lastUserIdx = i
			}
		case "assistant":
			for _, part := range msgs[i].Content {
				if part.Type == "tool" && strings.EqualFold(part.Name, "todowrite") {
					t.lastTodoWriteIdx = i
					// Only a finished call's list is the list: one still
					// streaming has arguments that may not even parse yet.
					if part.State.Status == "completed" {
						t.todos = Todos{}
						for _, it := range part.Todos {
							t.todos.count(it.Status, it.Content)
						}
					}
				}
				if part.Type == "text" && strings.TrimSpace(part.Text) != "" {
					t.lastAssistant = previewText(part.Text)
					t.lastAssistantIdx = i
				}
			}
		}
	}
}

// currentActivity reports the tool call in flight in the newest message,
// nil if nothing is running. Only the newest message is inspected: earlier
// ones' calls have all finished by definition, and once a run has ended the
// newest message is the "idle" marker, which has none.
func currentActivity(msgs []sessionMessage) *Activity {
	if len(msgs) == 0 {
		return nil
	}
	var act *Activity
	for _, part := range msgs[len(msgs)-1].Content {
		if part.Type != "tool" {
			continue
		}
		switch part.State.Status {
		case "streaming", "running", "pending":
			since := part.Time.Ran
			if since == 0 {
				since = part.Time.Created
			}
			act = &Activity{
				Tool:  part.Name,
				Title: firstLine(part.Title),
				Since: time.UnixMilli(since),
			}
		}
	}
	return act
}

// count folds one task list entry into the tally, mapping the status
// spellings both harnesses use onto the four buckets. Anything unrecognised
// counts as pending, which is the safe way round: an unknown status is
// outstanding work until proven otherwise.
func (t *Todos) count(status, content string) {
	t.Total++
	switch status {
	case "completed":
		t.Completed++
	case "in_progress":
		t.InProgress++
		if t.Current == "" {
			t.Current = content
		}
	case "cancelled":
		t.Cancelled++
	default:
		t.Pending++
	}
}

// activeSessions returns the set of sessions currently running on the
// server, or nil when it cannot be read — which callers must treat as
// "unknown", not as "none".
func activeSessions(ctx context.Context, c Conn) map[string]bool {
	var resp struct {
		Data map[string]struct {
			Type string `json:"type"`
		} `json:"data"`
	}
	if err := getJSON(ctx, c, "/api/session/active", &resp); err != nil {
		return nil
	}
	out := make(map[string]bool, len(resp.Data))
	for id := range resp.Data {
		out[id] = true
	}
	return out
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
// else, including busy: opencode keeps the session running while a tool
// call sits waiting for your answer, but an agent that cannot move without
// you is the thing worth surfacing.
//
// Neither list can be trusted blindly, though: if opencode crashes (or is
// killed) while a question or permission tool call is outstanding, a
// restart can leave that request listed with no one left to answer it and
// nothing to time it out. The message the request names is checked before
// honouring it: once that message has a completed time, its turn has ended
// one way or another (answered through a channel this poll missed, aborted,
// or superseded by a restart) and the request is stale, no matter what the
// list still says.
//
// active is the server's set of running sessions, nil if unknown. When
// known it is authoritative over the transcript's own busy signal: a turn
// left without a completed time by a crash is not running.
func classify(ctx context.Context, c Conn, sessionID string, family, active map[string]bool, busy bool, cur *sessionMessage, completedByID map[string]bool) (State, *Pending) {
	if p := pendingQuestion(ctx, c, sessionID, family, active, completedByID); p != nil {
		return StateWaiting, p
	}
	if p := pendingPermission(ctx, c, sessionID, family, completedByID); p != nil {
		return StateWaiting, p
	}
	if active != nil {
		busy = active[sessionID]
	}
	if !busy {
		return StateIdle, nil
	}
	if cur != nil && cur.Retry != nil && cur.Time.Completed == nil {
		return StateRetrying, nil
	}
	return StateBusy, nil
}

// formCandidates lists the family sessions worth asking for open forms,
// newest conversation first.
//
// Forms can only be listed one session at a time, and a family can hold a
// long tail of finished subagents. A form blocks a running tool call, so
// only running sessions can have one; when the running set is unknown,
// every family member is asked rather than risk missing a question.
func formCandidates(sessionID string, family, active map[string]bool) []string {
	var out []string
	for id := range family {
		if id == sessionID {
			continue
		}
		if active == nil || active[id] {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	if active == nil || active[sessionID] {
		out = append([]string{sessionID}, out...)
	}
	return out
}

// pendingQuestion reports the first genuinely open form raised against a
// session in family, nil if there is none.
func pendingQuestion(ctx context.Context, c Conn, sessionID string, family, active map[string]bool, completedByID map[string]bool) *Pending {
	for _, id := range formCandidates(sessionID, family, active) {
		var forms struct {
			Data []formInfo `json:"data"`
		}
		if err := getJSON(ctx, c, "/api/session/"+url.PathEscape(id)+"/form", &forms); err != nil {
			continue
		}
		for _, form := range forms.Data {
			if form.SessionID == "" {
				form.SessionID = id
			}
			if !family[form.SessionID] {
				continue
			}
			if !toolCallStillOpen(ctx, c, form.SessionID, form.Metadata.Tool.MessageID, sessionID, completedByID) {
				continue
			}
			if p := form.pending(); p != nil {
				return p
			}
		}
	}
	return nil
}

// pending turns a form into the one headline a compact widget has room
// for. A form can carry several questions; only the first is surfaced, and
// the rest are visible in the TUI anyway.
func (f formInfo) pending() *Pending {
	if len(f.Fields) == 0 {
		return nil
	}
	q := f.Fields[0]
	p := &Pending{Kind: BlockQuestion, Header: q.Title, Detail: q.Description}
	if p.Header == "" {
		p.Header = f.Title
	}
	if p.Detail == "" {
		p.Detail = f.Title
	}
	for _, o := range q.Options {
		p.Options = append(p.Options, o.Label)
	}
	return p
}

// pendingPermission reports the first genuinely open permission request
// raised against a session in family, nil if there is none.
//
// Requests are listed per location rather than per session, which suits:
// a conversation and its subagents share a directory, so one request covers
// the whole family.
func pendingPermission(ctx context.Context, c Conn, sessionID string, family map[string]bool, completedByID map[string]bool) *Pending {
	var perms struct {
		Data []permissionRequest `json:"data"`
	}
	path := "/api/permission/request"
	if c.Dir != "" {
		path += "?" + url.QueryEscape("location[directory]") + "=" + url.QueryEscape(c.Dir)
	}
	if err := getJSON(ctx, c, path, &perms); err != nil {
		return nil
	}
	for _, req := range perms.Data {
		if !family[req.SessionID] {
			continue
		}
		if !toolCallStillOpen(ctx, c, req.SessionID, req.Source.MessageID, sessionID, completedByID) {
			continue
		}
		detail := req.Message
		if detail == "" {
			detail = req.Action
		}
		return &Pending{
			Kind:      BlockPermission,
			Header:    req.Action,
			Detail:    detail,
			Resources: req.Resources,
		}
	}
	return nil
}

// toolCallStillOpen reports whether the message a question or permission
// request names is still an open turn, i.e. the request is genuinely
// blocking rather than a leftover from a turn that has already ended.
//
// messageID is empty for a request that did not come from a tool call (an
// MCP server's own form, say); such a request is trusted as-is, since there
// is nothing to check it against and treating "unknown" as "stale" would
// hide real blocks.
//
// newestSessionID/completedByID cover the one session Probe already fetched
// the transcript for, which is the common case (the request belongs to the
// conversation itself, or the conversation is blocked on its own turn). A
// request from a different family member — a subagent asking its own
// question or permission — needs its own message fetched, which happens
// rarely enough that the extra round trip is not worth avoiding.
func toolCallStillOpen(ctx context.Context, c Conn, reqSessionID, messageID, newestSessionID string, completedByID map[string]bool) bool {
	if messageID == "" {
		return true
	}
	if reqSessionID == newestSessionID {
		done, ok := completedByID[messageID]
		if ok {
			return !done
		}
	}
	var msg struct {
		Data sessionMessage `json:"data"`
	}
	if err := getBoundedJSON(ctx, c, "/api/session/"+url.PathEscape(reqSessionID)+"/message/"+url.PathEscape(messageID), &msg); err != nil {
		return true
	}
	return msg.Data.Time.Completed == nil
}

// newRequest builds a request to c's server, authenticated when c carries a
// password.
func newRequest(ctx context.Context, c Conn, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if c.Password != "" {
		req.SetBasicAuth(serverUser, c.Password)
	}
	return req, nil
}

func getJSON(ctx context.Context, c Conn, path string, out any) error {
	req, err := newRequest(ctx, c, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

// Fork copies a session and re-homes the copy to dir, returning the new
// session's ID.
//
// opencode fixes a session's directory when it is created, and a fork
// inherits the original's — so a fork alone would leave the copy pointing at
// whichever worktree the source came from. That directory may since have
// been deleted (leaving an agent hanging in a path that no longer exists),
// and even when it still exists it is the wrong feature's, which hides the
// session from anything scoping sessions by directory.
//
// Moving the copy afterwards fixes both.
func (opencode) Fork(ctx context.Context, c Conn, sessionID, dir string) (string, error) {
	if c.URL == "" {
		return "", fmt.Errorf("no url")
	}
	var forked struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := postJSON(ctx, c, "/api/session/"+url.PathEscape(sessionID)+"/fork", struct{}{}, &forked); err != nil {
		return "", fmt.Errorf("fork %s: %w", sessionID, err)
	}
	id := forked.Data.ID
	if id == "" {
		return "", fmt.Errorf("fork %s: no session id returned", sessionID)
	}

	body := map[string]string{"directory": dir}
	if err := postJSON(ctx, c, "/api/session/"+url.PathEscape(id)+"/move", body, nil); err != nil {
		return "", fmt.Errorf("move forked session into %s: %w", dir, err)
	}
	return id, nil
}

func postJSON(ctx context.Context, c Conn, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := newRequest(ctx, c, http.MethodPost, path, bytes.NewReader(b))
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
		return fmt.Errorf("POST %s: status %d", path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
