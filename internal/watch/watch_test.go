package watch

import (
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/state"
)

func feat(name string, agents ...string) *state.Feature {
	f := &state.Feature{Project: "norules", Name: name, Branch: name}
	for _, a := range agents {
		f.Agents = append(f.Agents, state.Agent{Name: a, Tool: "opencode", URL: "http://x"})
	}
	return f
}

func TestStatusForMapsHealth(t *testing.T) {
	cases := []struct {
		h    agent.Health
		want Status
	}{
		{agent.Health{Reachable: false}, StatusOffline},
		{agent.Health{Reachable: true, State: agent.StateWaiting}, StatusWaiting},
		{agent.Health{Reachable: true, State: agent.StateRetrying}, StatusRetrying},
		{agent.Health{Reachable: true, State: agent.StateBusy}, StatusWorking},
		{agent.Health{Reachable: true, State: agent.StateIdle}, StatusIdle},
		{agent.Health{Reachable: true, State: agent.StateIdle, LastError: "boom"}, StatusError},
		// A failed earlier turn must not mask what the agent is doing now.
		{agent.Health{Reachable: true, State: agent.StateBusy, LastError: "boom"}, StatusWorking},
		{agent.Health{Reachable: true, State: agent.StateWaiting, LastError: "boom"}, StatusWaiting},
	}
	for _, c := range cases {
		if got := statusFor(c.h); got != c.want {
			t.Errorf("statusFor(%+v) = %q, want %q", c.h, got, c.want)
		}
	}
}

func TestBuildKeepsSinceWhileStatusIsUnchanged(t *testing.T) {
	// Regression guard for a real property of opencode's stream: a single
	// turn emits several identical session.status "busy" events. If Since
	// were refreshed on every rebuild, a "how long has it been working"
	// gauge would keep snapping back to zero mid-turn.
	t0 := time.Now().Add(-5 * time.Minute)
	prev := &Feature{Status: StatusWorking, Since: t0}
	h := map[string]agent.Health{"main": {Reachable: true, State: agent.StateBusy}}

	got := Build(feat("f", "main"), h, prev, time.Now())
	if got.Status != StatusWorking {
		t.Fatalf("Status = %q", got.Status)
	}
	if !got.Since.Equal(t0) {
		t.Errorf("Since = %v, want it carried over as %v", got.Since, t0)
	}
}

func TestBuildResetsSinceOnRealTransition(t *testing.T) {
	t0 := time.Now().Add(-5 * time.Minute)
	now := time.Now()
	prev := &Feature{Status: StatusWorking, Since: t0}
	h := map[string]agent.Health{"main": {Reachable: true, State: agent.StateIdle}}

	got := Build(feat("f", "main"), h, prev, now)
	if got.Status != StatusIdle {
		t.Fatalf("Status = %q, want idle", got.Status)
	}
	if !got.Since.Equal(now) {
		t.Errorf("Since = %v, want reset to %v on transition", got.Since, now)
	}
}

func TestBuildSetsSinceForANewFeature(t *testing.T) {
	now := time.Now()
	h := map[string]agent.Health{"main": {Reachable: true, State: agent.StateIdle}}
	got := Build(feat("f", "main"), h, nil, now)
	if !got.Since.Equal(now) {
		t.Errorf("Since = %v, want %v", got.Since, now)
	}
}

func TestBuildUsesMostUrgentAgentForFeatureStatus(t *testing.T) {
	h := map[string]agent.Health{
		"main":     {Reachable: true, State: agent.StateIdle},
		"reviewer": {Reachable: true, State: agent.StateWaiting},
	}
	got := Build(feat("f", "main", "reviewer"), h, nil, time.Now())
	if got.Status != StatusWaiting {
		t.Errorf("Status = %q, want waiting (the most urgent agent wins)", got.Status)
	}
	if len(got.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(got.Agents))
	}
	if got.Agents[0].Name != "main" {
		t.Errorf("agents not sorted by name: %v", got.Agents)
	}
}

func TestBuildCarriesPendingThrough(t *testing.T) {
	p := &agent.Pending{Kind: agent.BlockQuestion, Header: "Pick one", Detail: "Which way?"}
	h := map[string]agent.Health{"main": {Reachable: true, State: agent.StateWaiting, Pending: p}}
	got := Build(feat("f", "main"), h, nil, time.Now())
	if got.Agents[0].Pending == nil || got.Agents[0].Pending.Header != "Pick one" {
		t.Errorf("Pending = %+v, want it surfaced for display", got.Agents[0].Pending)
	}
}

func TestBuildNoAgentsIsOffline(t *testing.T) {
	got := Build(feat("f"), nil, nil, time.Now())
	if got.Status != StatusOffline {
		t.Errorf("Status = %q, want offline for a feature with no agents", got.Status)
	}
}

func TestNeedsAttention(t *testing.T) {
	cases := map[Status]bool{
		StatusWaiting:  true,
		StatusError:    true,
		StatusRetrying: true,
		StatusWorking:  false,
		StatusIdle:     false,
		StatusOffline:  false,
	}
	for s, want := range cases {
		if got := s.NeedsAttention(); got != want {
			t.Errorf("%q.NeedsAttention() = %v, want %v", s, got, want)
		}
	}
}

func TestSummariseOrdersMostUrgentFirst(t *testing.T) {
	now := time.Now()
	in := []Feature{
		{Key: "a", Status: StatusIdle, Since: now},
		{Key: "b", Status: StatusWaiting, Since: now},
		{Key: "c", Status: StatusWorking, Since: now},
		{Key: "d", Status: StatusError, Since: now},
	}
	got := Summarise(in, now)
	want := []string{"b", "d", "c", "a"} // waiting, error, working, idle
	for i, k := range want {
		if got.Features[i].Key != k {
			t.Errorf("position %d = %q, want %q (order: %v)", i, got.Features[i].Key, k, keys(got.Features))
		}
	}
}

func TestSummariseTieBreaksByLongestWaiting(t *testing.T) {
	now := time.Now()
	older := now.Add(-10 * time.Minute)
	in := []Feature{
		{Key: "fresh", Status: StatusWaiting, Since: now},
		{Key: "stale", Status: StatusWaiting, Since: older},
	}
	got := Summarise(in, now)
	if got.Features[0].Key != "stale" {
		t.Errorf("first = %q, want the longest-ignored one first", got.Features[0].Key)
	}
}

func TestSummariseCounts(t *testing.T) {
	now := time.Now()
	in := []Feature{
		{Key: "a", Status: StatusWaiting, Since: now},
		{Key: "b", Status: StatusWaiting, Since: now},
		{Key: "c", Status: StatusWorking, Since: now},
		{Key: "d", Status: StatusIdle, Since: now},
		{Key: "e", Status: StatusError, Since: now},
		{Key: "f", Status: StatusOffline, Since: now},
	}
	s := Summarise(in, now).Summary
	if s.Total != 6 {
		t.Errorf("Total = %d, want 6", s.Total)
	}
	if s.Waiting != 2 || s.Working != 1 || s.Idle != 1 || s.Errored != 1 {
		t.Errorf("counts = %+v", s)
	}
	// waiting(2) + error(1); working, idle and offline do not want a person.
	if s.NeedsAttention != 3 {
		t.Errorf("NeedsAttention = %d, want 3", s.NeedsAttention)
	}
	if s.Status != StatusWaiting {
		t.Errorf("Status = %q, want the most urgent (waiting)", s.Status)
	}
}

func TestSummariseOldestAttentionSince(t *testing.T) {
	now := time.Now()
	oldest := now.Add(-30 * time.Minute)
	in := []Feature{
		{Key: "a", Status: StatusWaiting, Since: now},
		{Key: "b", Status: StatusError, Since: oldest},
		// Idle is older still, but does not need attention, so must not win.
		{Key: "c", Status: StatusIdle, Since: now.Add(-2 * time.Hour)},
	}
	s := Summarise(in, now).Summary
	if s.OldestAttentionSince == nil {
		t.Fatal("OldestAttentionSince = nil, want the oldest attention-needing feature")
	}
	if !s.OldestAttentionSince.Equal(oldest) {
		t.Errorf("OldestAttentionSince = %v, want %v", *s.OldestAttentionSince, oldest)
	}
}

func TestSummariseOldestAttentionSinceAbsentWhenNothingNeedsAttention(t *testing.T) {
	now := time.Now()
	in := []Feature{
		{Key: "a", Status: StatusIdle, Since: now.Add(-time.Hour)},
		{Key: "b", Status: StatusWorking, Since: now},
	}
	if s := Summarise(in, now).Summary; s.OldestAttentionSince != nil {
		t.Errorf("OldestAttentionSince = %v, want nil", *s.OldestAttentionSince)
	}
}

func TestSummariseEmpty(t *testing.T) {
	s := Summarise(nil, time.Now())
	if s.Summary.Total != 0 || s.Summary.NeedsAttention != 0 {
		t.Errorf("summary = %+v, want zeroed", s.Summary)
	}
	if s.Summary.Status != StatusOffline {
		t.Errorf("Status = %q, want offline for an empty set", s.Summary.Status)
	}
}

func keys(fs []Feature) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Key
	}
	return out
}

func TestBuildSurfacesTodos(t *testing.T) {
	h := map[string]agent.Health{"main": {
		Reachable: true, State: agent.StateBusy,
		Todos: agent.Todos{Total: 4, Completed: 2, InProgress: 1, Pending: 1, Current: "Wire the widget"},
	}}
	got := Build(feat("f", "main"), h, nil, time.Now())
	td := got.Agents[0].Todos
	if td == nil {
		t.Fatal("Todos = nil, want them surfaced")
	}
	if td.Total != 4 || td.Completed != 2 || td.Current != "Wire the widget" {
		t.Errorf("Todos = %+v", td)
	}
	if td.Done != 0.5 {
		t.Errorf("Done = %v, want 0.5", td.Done)
	}
}

func TestBuildOmitsTodosWhenUnused(t *testing.T) {
	h := map[string]agent.Health{"main": {Reachable: true, State: agent.StateIdle}}
	got := Build(feat("f", "main"), h, nil, time.Now())
	if got.Agents[0].Todos != nil {
		t.Errorf("Todos = %+v, want nil so the field is omitted entirely", got.Agents[0].Todos)
	}
}

// The two timers are distinct and both have to reach the wire: Worked is
// generation time only, SincePrompt is wall-clock since the user last spoke.
func TestBuildSurfacesBothTimers(t *testing.T) {
	h := map[string]agent.Health{"main": {
		Reachable: true, State: agent.StateBusy,
		Worked:      90 * time.Second,
		SincePrompt: 13*time.Minute + 13*time.Second,
	}}
	got := Build(feat("f", "main"), h, nil, time.Now())
	if got.Agents[0].Worked != 90 {
		t.Errorf("Worked = %v, want 90 seconds", got.Agents[0].Worked)
	}
	if got.Agents[0].SincePrompt != 793 {
		t.Errorf("SincePrompt = %v, want 793 seconds", got.Agents[0].SincePrompt)
	}
}

func TestBuildBootingOverridesAnOfflineAgent(t *testing.T) {
	// A feature being created has no reachable agent yet, so the honest
	// reading of the probes is "offline" — which is exactly the wrong thing to
	// show someone who just asked for it and is watching it appear.
	f := &state.Feature{
		Project: "norules", Name: "small-fixes",
		Agents:     []state.Agent{{Name: "main", URL: "http://127.0.0.1:1"}},
		Phase:      state.PhaseBooting,
		PhaseLabel: "service web",
		PhaseStep:  3, PhaseTotal: 10,
		PhaseSince: time.Now(),
	}
	got := Build(f, map[string]agent.Health{"main": {Reachable: false}}, nil, time.Now())

	if got.Status != StatusBooting {
		t.Errorf("status = %q, want %q", got.Status, StatusBooting)
	}
	if got.Progress == nil {
		t.Fatal("no progress emitted while booting")
	}
	if got.Progress.Step != 3 || got.Progress.Total != 10 || got.Progress.Label != "service web" {
		t.Errorf("progress = %+v, want 3/10 service web", got.Progress)
	}
}

func TestBuildDisbelievesAStalePhase(t *testing.T) {
	// Nothing updates a state file for a process that was killed outright, so
	// an interrupted run leaves its phase set for good. A progress bar frozen
	// forever is worse than none.
	f := &state.Feature{
		Project: "norules", Name: "small-fixes",
		Agents:    []state.Agent{{Name: "main", URL: "http://127.0.0.1:1"}},
		Phase:     state.PhaseBooting,
		PhaseStep: 3, PhaseTotal: 10,
		PhaseSince: time.Now().Add(-2 * state.StalePhaseAfter),
	}
	got := Build(f, map[string]agent.Health{"main": {Reachable: false}}, nil, time.Now())

	if got.Status != StatusOffline {
		t.Errorf("status = %q, want the agents' own %q once the phase is stale", got.Status, StatusOffline)
	}
	if got.Progress != nil {
		t.Errorf("progress = %+v, want none for a stale phase", got.Progress)
	}
}

func TestBootingAndRemovingDoNotDemandAttention(t *testing.T) {
	// Something you asked for, arriving, is not something to be told about.
	for _, s := range []Status{StatusBooting, StatusRemoving} {
		if s.NeedsAttention() {
			t.Errorf("%q needs attention, want not", s)
		}
	}
}
