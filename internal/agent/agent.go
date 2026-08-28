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
	// Sessions is the number of sessions rooted in the agent's directory.
	Sessions int
	Tokens   Tokens
	Cost     float64
	// Model is the model used by the most recent assistant message.
	Model string
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
	Err     error
}

type sessionInfo struct {
	ID       string `json:"id"`
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
	Finish  string  `json:"finish"`
	Tokens  Tokens  `json:"tokens"`
	Cost    float64 `json:"cost"`
	ModelID string  `json:"modelID"`
	Error   *struct {
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
	Info messageInfo `json:"info"`
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

	h.Sessions = len(mine)
	if len(mine) == 0 {
		h.State = StateIdle
		return h
	}

	newest := mine[0]
	h.Updated = time.UnixMilli(newest.Time.Updated)
	h.SessionID = newest.ID

	var msgs []sessionMessage
	if err := getJSON(ctx, baseURL+"/session/"+url.PathEscape(newest.ID)+"/message", &msgs); err != nil {
		h.State = StateIdle
		return h
	}

	// Token and cost totals live on assistant messages; the session list
	// reports them as zero. Only the newest session is summed, since fetching
	// messages for every session would be an N+1 on every status refresh.
	//
	// Messages arrive oldest-first, so the *last* assistant message is the
	// current turn — taking the first would report the state of whatever
	// happened at the very start of the session forever.
	now := time.Now()
	var cur *messageInfo
	for i := range msgs {
		m := &msgs[i].Info
		if m.Role != "assistant" {
			continue
		}
		h.Tokens.Input += m.Tokens.Input
		h.Tokens.Output += m.Tokens.Output
		h.Tokens.Reasoning += m.Tokens.Reasoning
		h.Tokens.Cache.Read += m.Tokens.Cache.Read
		h.Tokens.Cache.Write += m.Tokens.Cache.Write
		h.Cost += m.Cost

		if m.Time.Completed != nil {
			h.Worked += time.UnixMilli(*m.Time.Completed).Sub(time.UnixMilli(m.Time.Created))
		}
		cur = m
	}
	if cur != nil {
		h.Model = cur.ModelID
		h.Busy = cur.Time.Completed == nil
		if h.Busy {
			// Still generating; this turn has not contributed to Worked.
			h.Working = now.Sub(time.UnixMilli(cur.Time.Created))
		}
		if cur.Error != nil {
			h.LastError = cur.Error.Data.Message
		}
	}

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
