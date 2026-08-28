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

// State classifies what an agent is doing right now, from most to least
// urgent: retrying (hit a provider error and is auto-retrying), busy
// (actively generating), waiting (idle, but has a permission request — e.g.
// "may I run this command?" — sitting unanswered), idle (nothing pending).
//
// "Waiting" only ever means a pending permission request: opencode's API has
// no signal for "the assistant asked a free-text clarifying question and
// stopped", so that case is indistinguishable from plain idle.
type State string

const (
	StateIdle     State = "idle"
	StateBusy     State = "busy"
	StateWaiting  State = "waiting"
	StateRetrying State = "retrying"
)

// Health summarises what an agent server is doing right now.
type Health struct {
	Reachable bool
	// Busy reports that the newest session has an assistant message that has
	// not finished generating. Kept alongside State for callers that only
	// care about the busy/idle distinction.
	Busy bool
	// State is the fuller busy/waiting/idle/retrying classification.
	State State
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

type messageInfo struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Time struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed"`
	} `json:"time"`
	Finish string  `json:"finish"`
	Tokens Tokens  `json:"tokens"`
	Cost   float64 `json:"cost"`
	Model  struct {
		ID string `json:"id"`
	} `json:"model"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type messageListResp struct {
	Data []messageInfo `json:"data"`
}

type permissionListResp struct {
	Data []json.RawMessage `json:"data"`
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

	var msgs messageListResp
	if err := getJSON(ctx, baseURL+"/api/session/"+url.PathEscape(newest.ID)+"/message", &msgs); err != nil {
		h.State = StateIdle
		return h
	}

	// Token and cost totals live on assistant messages; the session list
	// reports them as zero. Only the newest session is summed, since fetching
	// messages for every session would be an N+1 on every status refresh.
	first := true
	now := time.Now()
	for _, m := range msgs.Data {
		if m.Type != "assistant" {
			continue
		}
		h.Tokens.Input += m.Tokens.Input
		h.Tokens.Output += m.Tokens.Output
		h.Tokens.Reasoning += m.Tokens.Reasoning
		h.Tokens.Cache.Read += m.Tokens.Cache.Read
		h.Tokens.Cache.Write += m.Tokens.Cache.Write
		h.Cost += m.Cost

		created := time.UnixMilli(m.Time.Created)
		if m.Time.Completed != nil {
			h.Worked += time.UnixMilli(*m.Time.Completed).Sub(created)
		} else if first {
			// Still generating — this turn hasn't added to Worked yet.
			h.Working = now.Sub(created)
		}

		if first {
			// Messages arrive newest-first, so this is the current turn.
			first = false
			h.Model = m.Model.ID
			h.Busy = m.Time.Completed == nil
			if m.Finish == "error" && m.Error != nil {
				h.LastError = m.Error.Message
			}
		}
	}

	h.State = classify(ctx, baseURL, newest.ID, h.Busy)
	return h
}

// classify determines the fuller State beyond the plain busy/idle Health.Busy
// already carries: a pending permission request means "waiting" even though
// the session itself is idle from opencode's own point of view, and a
// provider retry outranks a plain busy classification.
func classify(ctx context.Context, baseURL, sessionID string, busy bool) State {
	var statuses map[string]sessionStatusInfo
	if err := getJSON(ctx, baseURL+"/session/status", &statuses); err == nil {
		if s, ok := statuses[sessionID]; ok && s.Type == "retry" {
			return StateRetrying
		}
	}
	if !busy {
		var perms permissionListResp
		if err := getJSON(ctx, baseURL+"/api/session/"+url.PathEscape(sessionID)+"/permission", &perms); err == nil && len(perms.Data) > 0 {
			return StateWaiting
		}
	}
	if busy {
		return StateBusy
	}
	return StateIdle
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
