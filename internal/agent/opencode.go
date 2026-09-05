package agent

// The opencode harness. opencode runs headless as an HTTP server that
// canaveral starts in a systemd unit and then interrogates over REST, which
// is why it can report far more than a harness with no server: pending
// questions and permission requests, retry state, and per-session token and
// cost totals all come straight from the running process.
//
// Note the mixed API surface below: the session list lives under /api, while
// messages, todos, questions and permissions do not. That is opencode's, not
// a typo — the bare paths are the ones verified to actually return data.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func (opencode) EnvFor(sel Selection) map[string]string {
	env := map[string]string{}
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
// --dir is passed even though the caller already runs this in c.Dir: an
// opencode client takes its session scope from the flag, not from its own
// working directory.
func (opencode) AttachArgv(c Conn, cont bool) []string {
	argv := []string{"opencode", "attach", c.URL, "--dir", c.Dir}
	if cont {
		argv = append(argv, "--continue")
	}
	return argv
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
	return ocevents.Watch(ctx, c.URL, func(ev ocevents.Event) {
		if relevantEvent(ev.Type) {
			fn()
		}
	})
}

// relevantEvent filters opencode's event firehose down to the ones that can
// change a feature's headline state.
//
// opencode emits a great deal per turn (token deltas, individual message
// parts); reacting to all of it would mean re-probing every agent dozens of
// times a second for no visible difference. The list is deliberately broad
// because an event only triggers a re-read — being slightly over-inclusive
// costs one HTTP round trip, while being under-inclusive would mean missing
// a state change entirely.
func relevantEvent(t string) bool {
	switch t {
	case "session.idle", "session.error", "session.status", "session.created", "session.deleted",
		"permission.asked", "permission.replied",
		"permission.v2.asked", "permission.v2.replied",
		"question.asked", "question.replied", "question.rejected",
		"question.v2.asked", "question.v2.replied", "question.v2.rejected",
		"todo.updated",
		"session.next.tool.called", "session.next.tool.success", "session.next.tool.failed",
		"server.connected":
		return true
	}
	return false
}

// listenRe matches the line opencode prints once its HTTP server is bound.
var listenRe = regexp.MustCompile(`listening on (https?://[^\s]+)`)

// DiscoverURL polls the agent log until opencode reports its listen address.
//
// Parsing the log rather than pre-allocating a port avoids a TOCTOU race where
// another process claims the port between our probe and opencode's bind.
func (opencode) DiscoverURL(ctx context.Context, logPath string, timeout time.Duration, alive func() error) (string, error) {
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

var client = &http.Client{Timeout: 4 * time.Second}

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

// Probe reports what the agent rooted at c.Dir is currently doing.
//
// Sessions are filtered by location.directory because an opencode server
// exposes the user's entire global session history, not just this workspace's.
func (opencode) Probe(ctx context.Context, c Conn) Health {
	baseURL, dir := c.URL, c.Dir
	if baseURL == "" {
		return Health{Err: fmt.Errorf("no url")}
	}

	var list sessionListResp
	if err := getJSON(ctx, baseURL+"/api/session?limit=100&order=desc", &list); err != nil {
		return Health{Err: err}
	}
	// A server that answered is by definition running, so Live and
	// Reachable are the same question for this harness.
	h := Health{Reachable: true, Live: true}

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
	if h.Todos.resolved() && turns.lastUserIdx > turns.lastTodoWriteIdx {
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

// fetchTodos reads the session's task list. A missing or empty list is not
// an error: most sessions never use one.
func fetchTodos(ctx context.Context, baseURL, sessionID string) Todos {
	var items []todoItem
	if err := getJSON(ctx, baseURL+"/session/"+url.PathEscape(sessionID)+"/todo", &items); err != nil {
		return Todos{}
	}
	var t Todos
	for _, it := range items {
		t.count(it.Status, it.Content)
	}
	return t
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
// Moving the copy afterwards fixes both. The move endpoint is experimental
// and cannot cross projects, which is not a limit here: a project's features
// all live under the same project root.
func (opencode) Fork(ctx context.Context, c Conn, sessionID, dir string) (string, error) {
	baseURL := c.URL
	if baseURL == "" {
		return "", fmt.Errorf("no url")
	}
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
