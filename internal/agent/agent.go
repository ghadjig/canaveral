// Package agent manages opencode agent servers within a workspace.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
func Resolve() (string, error) {
	bin, err := exec.LookPath("opencode")
	if err != nil {
		return "", fmt.Errorf("opencode not found in PATH: %w", err)
	}
	return filepath.Abs(bin)
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
	// Working is how long the current turn has been running, set only while
	// State is StateBusy.
	Working time.Duration
	// Todos is the current session's task list, zero when the agent has not
	// used one.
	Todos Todos
	// Activity is the tool call in flight, nil when none is running.
	Activity *Activity
	Err      error
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

// permissionRequest is one entry of GET /permission, the server-wide list of
// pending permission requests.
type permissionRequest struct {
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Patterns   []string `json:"patterns"`
}

// questionRequest is one entry of GET /question, the server-wide list of
// pending questions. A request can carry several questions; only the first
// is surfaced, since a compact widget has room for one headline and the
// rest are visible in the TUI anyway.
type questionRequest struct {
	SessionID string `json:"sessionID"`
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

	want := filepath.Clean(dir)
	mine := make([]sessionInfo, 0, len(list.Data))
	for _, s := range list.Data {
		if want == "" || filepath.Clean(s.Location.Directory) == want {
			mine = append(mine, s)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Time.Updated > mine[j].Time.Updated })

	// Subagent sessions (the Task tool spawns one per subagent) live in the
	// same directory as the conversation that created them, so a single
	// conversation can look like several sessions. Only top-level ones are
	// candidates for "the" current session: a subagent updates at almost
	// the same instant as its parent, so picking the newest of everything
	// would flip between them and make the reported model, state and
	// numbers jump around at random.
	roots := make([]sessionInfo, 0, len(mine))
	for _, x := range mine {
		if x.ParentID == "" {
			roots = append(roots, x)
		}
	}
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
	byParent := map[string][]sessionInfo{}
	for _, x := range mine {
		if x.ParentID != "" {
			byParent[x.ParentID] = append(byParent[x.ParentID], x)
		}
	}
	family := []sessionInfo{newest}
	for queue := []string{newest.ID}; len(queue) > 0; {
		id := queue[0]
		queue = queue[1:]
		for _, child := range byParent[id] {
			family = append(family, child)
			queue = append(queue, child.ID)
		}
	}
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
	// Messages arrive oldest-first, so the *last* assistant message is the
	// current turn — taking the first would report the state of whatever
	// happened at the very start of the session forever.
	//
	// Worked counts this conversation's turns only. A parent's turn stays
	// open while a subagent runs, so its duration already covers that work;
	// adding the subagents' own turns would double-count it.
	now := time.Now()
	var cur *messageInfo
	for i := range msgs {
		m := &msgs[i].Info
		if m.Role != "assistant" {
			continue
		}
		if m.Time.Completed != nil {
			h.Worked += time.UnixMilli(*m.Time.Completed).Sub(time.UnixMilli(m.Time.Created))
		}
		cur = m
	}
	// A tool that is pending or running is the agent's current activity.
	// Only the newest turn is inspected: earlier turns' calls have all
	// finished by definition.
	if len(msgs) > 0 {
		for _, part := range msgs[len(msgs)-1].Parts {
			if part.Type != "tool" {
				continue
			}
			if st := part.State.Status; st == "running" || st == "pending" {
				title := part.State.Title
				if title == "" {
					title = describeInput(part.State.Input)
				}
				h.Activity = &Activity{
					Tool:  part.Tool,
					Title: firstLine(title),
					Since: time.UnixMilli(part.State.Time.Start),
				}
			}
		}
	}

	if cur != nil {
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
	h.State, h.Pending = classify(ctx, baseURL, newest.ID, h.Busy)
	return h
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

func classify(ctx context.Context, baseURL, sessionID string, busy bool) (State, *Pending) {
	// The server-wide lists are used rather than the per-session ones: they
	// are a single request no matter how many sessions exist, and they live
	// on the same API surface as /session/{id}/message, which is the one
	// verified to actually return data.
	var qs []questionRequest
	if err := getJSON(ctx, baseURL+"/question", &qs); err == nil {
		for _, req := range qs {
			if req.SessionID != sessionID {
				continue
			}
			for _, q := range req.Questions {
				p := &Pending{Kind: BlockQuestion, Header: q.Header, Detail: q.Question}
				for _, o := range q.Options {
					p.Options = append(p.Options, o.Label)
				}
				return StateWaiting, p
			}
		}
	}

	var perms []permissionRequest
	if err := getJSON(ctx, baseURL+"/permission", &perms); err == nil {
		for _, req := range perms {
			if req.SessionID != sessionID {
				continue
			}
			return StateWaiting, &Pending{
				Kind:      BlockPermission,
				Header:    req.Permission,
				Detail:    req.Permission,
				Resources: req.Patterns,
			}
		}
	}

	var statuses map[string]sessionStatusInfo
	if err := getJSON(ctx, baseURL+"/session/status", &statuses); err == nil {
		if st, ok := statuses[sessionID]; ok && st.Type == "retry" {
			return StateRetrying, nil
		}
	}
	if busy {
		return StateBusy, nil
	}
	return StateIdle, nil
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
