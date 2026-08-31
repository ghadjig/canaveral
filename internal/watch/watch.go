// Package watch maintains a live, push-driven view of every feature's agent
// for external consumers (a Quickshell widget, waybar, anything that reads a
// line of JSON).
//
// The design is deliberately "events trigger, REST decides". opencode pushes
// an event the instant anything happens, which is what makes reacting fast;
// but which event means what has varied across opencode versions, and
// whether a permission is ever requested at all depends on the user's own
// permission config. So an event is treated only as "something changed, go
// look" — the authoritative state always comes from re-reading the HTTP API.
// A slow safety-net refresh covers the case where an event is missed
// entirely, so the view can be briefly stale but never permanently wrong.
package watch

import (
	"sort"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/state"
)

// Status is a feature's headline state, ordered by how much it wants you.
type Status string

const (
	// StatusWaiting means an agent is blocked on a question or a permission
	// request and cannot continue without you.
	StatusWaiting Status = "waiting"
	// StatusError means the last turn failed.
	StatusError Status = "error"
	// StatusRetrying means a provider error is being retried automatically.
	StatusRetrying Status = "retrying"
	// StatusWorking means an agent is actively generating.
	StatusWorking Status = "working"
	// StatusIdle means nothing is pending; work may be finished and waiting
	// for review.
	StatusIdle Status = "idle"
	// StatusOffline means the feature has no reachable agent.
	StatusOffline Status = "offline"
)

// rank orders statuses by urgency for sorting and for picking the single
// status that represents a whole feature (or the whole snapshot).
func rank(s Status) int {
	switch s {
	case StatusWaiting:
		return 0
	case StatusError:
		return 1
	case StatusRetrying:
		return 2
	case StatusWorking:
		return 3
	case StatusIdle:
		return 4
	}
	return 5
}

// NeedsAttention reports whether a status is one a person should act on.
//
// Idle counts: an agent that has finished is waiting for review just as much
// as one that is blocked, it simply is not blocking itself. Working and
// offline do not.
func (s Status) NeedsAttention() bool {
	switch s {
	case StatusWaiting, StatusError, StatusRetrying:
		return true
	}
	return false
}

// Agent is one agent's live state within a feature.
type Agent struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	URL    string `json:"url,omitempty"`
	Model  string `json:"model,omitempty"`
	// Variant is the provider-specific reasoning effort ("high", "low"),
	// empty when the provider has no such notion.
	Variant  string `json:"variant,omitempty"`
	Provider string `json:"provider,omitempty"`
	// SubAgents is how many subagent sessions the current conversation has
	// spawned. Their tokens and cost are already folded into the totals
	// below.
	SubAgents int     `json:"subagents,omitempty"`
	Tokens    int64   `json:"tokens,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
	// Pending is what this agent is blocked on, when Status is waiting.
	Pending *agent.Pending `json:"pending,omitempty"`
	// Error is the last turn's failure message, when Status is error.
	Error string `json:"error,omitempty"`
	// Worked is total generation time in the current session, in seconds,
	// cumulative across every prompt in it.
	Worked float64 `json:"worked_seconds,omitempty"`
	// SincePrompt is how long since the user's most recent message, in
	// seconds — "how long has it been working on what I asked". Unlike the
	// per-message generation timer, this does not reset on each tool round
	// trip.
	SincePrompt float64 `json:"since_prompt_seconds,omitempty"`
	// Activity is the tool call in flight right now — the most literal
	// answer to "what is it doing" — absent when nothing is running.
	Activity *agent.Activity `json:"activity,omitempty"`
	// LastUser and LastAssistant are the most recent thing said on each
	// side, collapsed to one line — enough to see what an agent was asked
	// for and what it last concluded without attaching to it.
	LastUser      string `json:"last_user,omitempty"`
	LastAssistant string `json:"last_assistant,omitempty"`
	// Todos is the agent's self-reported task list for the current session,
	// omitted when it is not using one. This is the only genuine progress
	// signal available — neither opencode nor Claude Code exposes a
	// percentage or an ETA.
	Todos *Todos `json:"todos,omitempty"`
}

// Todos is a session's task list plus the derived completion fraction, so a
// consumer does not have to recompute it.
type Todos struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	InProgress int `json:"in_progress"`
	Pending    int `json:"pending"`
	Cancelled  int `json:"cancelled"`
	// Current is the task in progress right now, empty when none is.
	Current string `json:"current,omitempty"`
	// Done is Completed+Cancelled over Total, in [0,1].
	Done float64 `json:"done"`
}

// Git is a feature's branch position relative to the project's default
// branch, plus how much uncommitted work is lying around.
//
// It is refreshed on a slower cadence than everything else in a snapshot —
// see gitCache in runner.go — so it can lag a commit by up to Options.Git.
// A nil Git means "not measured yet", which is deliberately distinct from a
// measured all-zero, since "+0 -0, no commits" is a thing worth showing.
type Git struct {
	// Base is what the comparison is against, e.g. "origin/main".
	Base         string `json:"base,omitempty"`
	Ahead        int    `json:"ahead"`
	Behind       int    `json:"behind"`
	FilesChanged int    `json:"files_changed"`
	Insertions   int    `json:"insertions"`
	Deletions    int    `json:"deletions"`
	// Uncommitted counts working-tree changes, which the commit-based
	// counts above cannot see.
	Uncommitted int `json:"uncommitted"`
}

// Feature is one feature's live state.
type Feature struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Branch  string `json:"branch,omitempty"`
	// Workspace is the Hyprland workspace to switch to, so a widget can
	// make its rows clickable.
	Workspace string `json:"workspace,omitempty"`
	// WSlot is the feature's stable widget slot, 1-indexed and global across
	// projects. Emitted so a status bar can label and order features by the
	// same number the jump keybinds use, instead of deriving a position from
	// whichever workspaces happen to exist right now — which reshuffles as
	// features come and go.
	WSlot  int    `json:"ws_slot,omitempty"`
	Status Status `json:"status"`
	// Since is when this feature entered its current Status. A consumer is
	// expected to render the elapsed time itself and tick locally, which is
	// why snapshots are emitted on change rather than on a timer.
	Since time.Time `json:"since"`
	// CreatedAt is when the feature was first created.
	CreatedAt time.Time `json:"created_at"`
	Agents    []Agent   `json:"agents,omitempty"`
	Git       *Git      `json:"git,omitempty"`
}

// NeedsAttention reports whether this feature wants a person.
func (f Feature) NeedsAttention() bool { return f.Status.NeedsAttention() }

// Snapshot is one complete view, emitted as a single line of JSON.
type Snapshot struct {
	Time     time.Time `json:"time"`
	Features []Feature `json:"features"`
	Summary  Summary   `json:"summary"`
}

// Summary is the aggregate a compact widget can render without walking the
// feature list.
type Summary struct {
	Total          int `json:"total"`
	NeedsAttention int `json:"needs_attention"`
	Waiting        int `json:"waiting"`
	Working        int `json:"working"`
	Idle           int `json:"idle"`
	Errored        int `json:"errored"`
	// Status is the most urgent status across all features, which is what a
	// single-colour notch should reflect.
	Status Status `json:"status"`
	// OldestAttentionSince is when the longest-waiting feature that needs
	// attention entered that state, and is absent when nothing does. It is
	// a pointer because encoding/json's omitempty has no effect on a
	// time.Time, which would otherwise serialise as a year-1 timestamp that
	// a consumer has to know to special-case.
	OldestAttentionSince *time.Time `json:"oldest_attention_since,omitempty"`
}

// statusFor collapses an agent health probe into a feature-level status.
func statusFor(h agent.Health) Status {
	if !h.Reachable {
		return StatusOffline
	}
	switch h.State {
	case agent.StateWaiting:
		return StatusWaiting
	case agent.StateRetrying:
		return StatusRetrying
	case agent.StateBusy:
		return StatusWorking
	}
	// An errored turn only matters once the agent has stopped; while it is
	// busy or blocked, those are the more useful things to report.
	if h.LastError != "" {
		return StatusError
	}
	return StatusIdle
}

// worst returns the most urgent of the given statuses, or StatusOffline when
// there are none.
func worst(ss []Status) Status {
	out := StatusOffline
	for _, s := range ss {
		if rank(s) < rank(out) {
			out = s
		}
	}
	return out
}

// Build assembles a feature's view from its record and its agents' probes.
//
// prev is the previous view of the same feature, if any: Since is carried
// over whenever the status has not actually changed, so a gauge measuring
// "how long has it been in this state" is not reset by the repeated
// same-state events opencode emits while a turn runs (confirmed live: a
// single turn produces several identical session.status "busy" events).
func Build(f *state.Feature, healths map[string]agent.Health, prev *Feature, now time.Time) Feature {
	out := Feature{
		Project:   f.Project,
		Name:      f.Name,
		Key:       f.Key(),
		Branch:    f.Branch,
		Workspace: f.HyprWorkspace(),
		WSlot:     f.WSlot,
		CreatedAt: f.CreatedAt,
	}

	var statuses []Status
	for _, a := range f.Agents {
		h, ok := healths[a.Name]
		if !ok {
			continue
		}
		st := statusFor(h)
		statuses = append(statuses, st)
		ag := Agent{
			Name:      a.Name,
			Status:    st,
			URL:       a.URL,
			Model:     h.Model,
			Variant:   h.Variant,
			Provider:  h.Provider,
			SubAgents: h.SubSessions,
			Activity:  h.Activity,

			LastUser:      h.LastUser,
			LastAssistant: h.LastAssistant,
			Tokens:        h.Tokens.Total(),
			Cost:          h.Cost,
			Pending:       h.Pending,
			Error:         h.LastError,
			Worked:        h.Worked.Seconds(),
		}
		if h.Todos.Total > 0 {
			ag.Todos = &Todos{
				Total:      h.Todos.Total,
				Completed:  h.Todos.Completed,
				InProgress: h.Todos.InProgress,
				Pending:    h.Todos.Pending,
				Cancelled:  h.Todos.Cancelled,
				Current:    h.Todos.Current,
				Done:       h.Todos.Done(),
			}
		}
		out.Agents = append(out.Agents, ag)
	}
	sort.Slice(out.Agents, func(i, j int) bool { return out.Agents[i].Name < out.Agents[j].Name })

	out.Status = worst(statuses)
	switch {
	case prev == nil, prev.Status != out.Status:
		out.Since = now
	default:
		out.Since = prev.Since
	}
	return out
}

// Summarise computes the aggregate and orders features most-urgent first,
// breaking ties by how long they have been in that state (longest first) so
// the thing that has been ignored longest is always at the top.
func Summarise(features []Feature, now time.Time) Snapshot {
	sort.SliceStable(features, func(i, j int) bool {
		if ri, rj := rank(features[i].Status), rank(features[j].Status); ri != rj {
			return ri < rj
		}
		return features[i].Since.Before(features[j].Since)
	})

	s := Summary{Total: len(features), Status: worst(nil)}
	for _, f := range features {
		switch f.Status {
		case StatusWaiting:
			s.Waiting++
		case StatusWorking:
			s.Working++
		case StatusIdle:
			s.Idle++
		case StatusError:
			s.Errored++
		}
		if f.NeedsAttention() {
			s.NeedsAttention++
			if s.OldestAttentionSince == nil || f.Since.Before(*s.OldestAttentionSince) {
				since := f.Since
				s.OldestAttentionSince = &since
			}
		}
		if rank(f.Status) < rank(s.Status) {
			s.Status = f.Status
		}
	}
	return Snapshot{Time: now, Features: features, Summary: s}
}
