package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/state"
)

func TestStateLabelAgentStates(t *testing.T) {
	cases := []struct {
		r    row
		want string
	}{
		{row{Kind: kindAgent, State: "active", AgentState: string(agent.StateIdle)}, "idle"},
		{row{Kind: kindAgent, State: "active", AgentState: string(agent.StateBusy)}, "busy"},
		{row{Kind: kindAgent, State: "active", AgentState: string(agent.StateWaiting)}, "waiting"},
		{row{Kind: kindAgent, State: "active", AgentState: string(agent.StateRetrying)}, "retrying"},
		{row{Kind: kindAgent, State: "active", AgentState: string(agent.StateIdle), LastError: "boom"}, "error"},
		{row{Kind: kindAgent, State: "active", Detail: "unreachable"}, "no-api"},
		{row{Kind: kindService, State: "active"}, "active"},
	}
	for _, c := range cases {
		if got := stateLabel(c.r); got != c.want {
			t.Errorf("stateLabel(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestIdleDetail(t *testing.T) {
	cases := []struct {
		r    row
		want string
	}{
		{row{Kind: kindAgent, Idle: 90 * time.Second}, "1m30s"},
		{row{Kind: kindAgent, Idle: 0}, "-"},
		{row{Kind: kindService, Idle: 90 * time.Second}, "-"}, // only agents report idle
	}
	for _, c := range cases {
		if got := idleDetail(c.r); got != c.want {
			t.Errorf("idleDetail(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestWorkedDetail(t *testing.T) {
	cases := []struct {
		r    row
		want string
	}{
		{row{Kind: kindAgent, Worked: 5 * time.Minute}, "5m0s"},
		// The per-message timer resets every tool round trip, so it is not
		// shown; "on this" is the timer that means something.
		{row{Kind: kindAgent, Working: 12 * time.Second}, "-"},
		{row{Kind: kindAgent, Worked: 5 * time.Minute, Working: 12 * time.Second}, "5m0s"},
		{row{Kind: kindAgent}, "-"},
		{row{Kind: kindWindow, Worked: time.Minute}, "-"},
	}
	for _, c := range cases {
		if got := workedDetail(c.r); got != c.want {
			t.Errorf("workedDetail(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

func TestCollectBranchStatus(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "init")
	run("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "feature work")

	features := []*state.Feature{{Name: "small-fixes", Worktree: dir}}
	got := collectBranchStatus(context.Background(), features)
	bs, ok := got["small-fixes"]
	if !ok {
		t.Fatalf("no branch status for small-fixes, got %v", got)
	}
	if bs.Ahead != 1 || bs.Behind != 0 {
		t.Errorf("Ahead=%d Behind=%d, want 1/0", bs.Ahead, bs.Behind)
	}
}

func TestCollectBranchStatusSkipsFeaturesWithoutAWorktree(t *testing.T) {
	features := []*state.Feature{{Name: "no-worktree", Worktree: ""}}
	got := collectBranchStatus(context.Background(), features)
	if _, ok := got["no-worktree"]; ok {
		t.Error("expected no entry for a feature with no worktree")
	}
}

func TestCollectOrdersRowsByFeatureThenServiceAgentWindow(t *testing.T) {
	f1 := &state.Feature{
		Project: "collect-test", Name: "f1",
		Services: []state.Service{{Name: "web", Unit: "canaveral-collect-test-f1-svc-web-nonexistent"}},
		Agents:   []state.Agent{{Name: "main", Unit: "canaveral-collect-test-f1-agent-main-nonexistent"}},
		Windows:  []state.Window{{Name: "term", Class: "canaveral-collect-test-f1-term-nonexistent"}},
	}
	f2 := &state.Feature{
		Project: "collect-test", Name: "f2",
		Services: []state.Service{{Name: "jobs", Unit: "canaveral-collect-test-f2-svc-jobs-nonexistent"}},
	}

	rows := collect(context.Background(), []*state.Feature{f1, f2})
	want := []struct {
		feature string
		kind    rowKind
		name    string
	}{
		{"f1", kindService, "web"},
		{"f1", kindAgent, "main"},
		{"f1", kindWindow, "term"},
		{"f2", kindService, "jobs"},
	}
	if len(rows) != len(want) {
		t.Fatalf("len(rows) = %d, want %d: %+v", len(rows), len(want), rows)
	}
	for i, w := range want {
		if rows[i].Feature != w.feature || rows[i].Kind != w.kind || rows[i].Name != w.name {
			t.Errorf("rows[%d] = %+v, want feature=%s kind=%s name=%s", i, rows[i], w.feature, w.kind, w.name)
		}
	}
}

func TestCollectServiceRowReportsGoneForANonexistentUnit(t *testing.T) {
	f := &state.Feature{
		Project: "collect-test", Name: "f",
		Services: []state.Service{{Name: "web", Unit: "canaveral-collect-test-f-svc-web-nonexistent"}},
	}
	rows := collect(context.Background(), []*state.Feature{f})
	if len(rows) != 1 || rows[0].State != "gone" {
		t.Errorf("rows = %+v, want a single gone service row", rows)
	}
}

func TestCollectAgentRowSkipsProbingAnInactiveUnit(t *testing.T) {
	// Nothing should reach out over HTTP for an agent whose unit is not
	// even running — a.URL is deliberately unreachable to catch that.
	f := &state.Feature{
		Project: "collect-test", Name: "f",
		Agents: []state.Agent{{
			Name: "main", Unit: "canaveral-collect-test-f-agent-main-nonexistent",
			URL: "http://127.0.0.1:1",
		}},
	}
	rows := collect(context.Background(), []*state.Feature{f})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].State != "gone" {
		t.Errorf("State = %q, want gone", rows[0].State)
	}
	if rows[0].AgentState != "" {
		t.Errorf("AgentState = %q, want empty: probing must be skipped for an inactive unit", rows[0].AgentState)
	}
}

func TestWindowRowUnknownWithoutAWindowList(t *testing.T) {
	got := windowRow(&state.Feature{Name: "f"}, state.Window{Name: "term"}, nil, false)
	if got.State != "unknown" {
		t.Errorf("State = %q, want unknown", got.State)
	}
}

func TestWindowRowOpenWhenClassMatches(t *testing.T) {
	w := state.Window{Name: "term", Class: "canaveral-p-f-term"}
	clients := []hypr.Client{{InitialClass: "canaveral-p-f-term"}}
	got := windowRow(&state.Feature{Name: "f"}, w, clients, true)
	if got.State != "open" {
		t.Errorf("State = %q, want open", got.State)
	}
}

func TestWindowRowClosedWhenClassAbsent(t *testing.T) {
	w := state.Window{Name: "term", Class: "canaveral-p-f-term"}
	got := windowRow(&state.Feature{Name: "f"}, w, nil, true)
	if got.State != "closed" {
		t.Errorf("State = %q, want closed", got.State)
	}
}

func TestApplyPendingPrefersOptionsOverResources(t *testing.T) {
	r := &row{}
	applyPending(r, &agent.Pending{
		Kind: agent.BlockQuestion, Header: "h", Detail: "d",
		Options:   []string{"yes", "no"},
		Resources: []string{"/etc/passwd"},
	})
	if r.PendKind != string(agent.BlockQuestion) || r.PendHeader != "h" || r.PendDetail != "d" {
		t.Errorf("row = %+v", r)
	}
	if r.PendExtra != "yes / no" {
		t.Errorf("PendExtra = %q, want the joined options", r.PendExtra)
	}
}

func TestApplyPendingFallsBackToResources(t *testing.T) {
	r := &row{}
	applyPending(r, &agent.Pending{Kind: agent.BlockPermission, Resources: []string{"a", "b"}})
	if r.PendExtra != "a, b" {
		t.Errorf("PendExtra = %q, want the joined resources", r.PendExtra)
	}
}

func TestApplyPendingNilIsANoop(t *testing.T) {
	r := &row{State: "active"}
	applyPending(r, nil)
	if r.PendKind != "" || r.PendExtra != "" {
		t.Errorf("row mutated by a nil Pending: %+v", r)
	}
}

func TestIdleForNeverReportsHugeDurationForANeverUsedAgent(t *testing.T) {
	// Regression test: Health.Updated is the zero Time when an agent has
	// never had a session, and time.Since of a zero Time is a
	// multi-million-hour nonsense duration if not guarded against.
	got := idleFor(agent.Health{Reachable: true})
	if got != 0 {
		t.Errorf("idleFor(never used) = %v, want 0", got)
	}
}

func TestIdleForZeroWhileBusy(t *testing.T) {
	got := idleFor(agent.Health{Busy: true, Updated: time.Now().Add(-time.Hour)})
	if got != 0 {
		t.Errorf("idleFor(busy) = %v, want 0", got)
	}
}

func TestIdleForReportsElapsedSinceLastUpdate(t *testing.T) {
	got := idleFor(agent.Health{Updated: time.Now().Add(-90 * time.Second)})
	if got < 89*time.Second || got > 91*time.Second {
		t.Errorf("idleFor = %v, want ~90s", got)
	}
}

func TestAgentSummaryLine(t *testing.T) {
	cases := []struct {
		r    row
		want string
	}{
		{
			row{Kind: kindAgent, Name: "main", State: "active", AgentState: string(agent.StateIdle)},
			"  agent main: idle",
		},
		{
			// The per-message generation timer is not surfaced; "on this"
			// (measured from the user's message) is the meaningful one.
			row{Kind: kindAgent, Name: "main", State: "active", AgentState: string(agent.StateBusy),
				Worked: 5 * time.Minute, SincePrompt: 90 * time.Second},
			"  agent main: busy · worked 5m0s · on this 1m30s",
		},
		{
			row{Kind: kindAgent, Name: "main", State: "active", AgentState: string(agent.StateWaiting), Idle: 90 * time.Second, Sessions: 3},
			"  agent main: waiting · idle 1m30s · 3 session(s)",
		},
	}
	for _, c := range cases {
		if got := agentSummaryLine(c.r); got != c.want {
			t.Errorf("agentSummaryLine(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
}

// TestStateLabelPlainNeverContainsANSICodes is a regression test for a real
// bug: tabwriter measures raw byte length, invisible ANSI escape codes
// included. A colored cell in a table where the header (or other cells)
// aren't colored the same way inflates that column's computed width, and
// every column after it drifts out of alignment with its header. Forcing
// color on here (as a real terminal would) and asserting the table-safe
// variant never emits an escape code is what would have caught it.
func TestStateLabelPlainNeverContainsANSICodes(t *testing.T) {
	old := useColor
	useColor = true
	defer func() { useColor = old }()

	rows := []row{
		{Kind: kindAgent, State: "active", AgentState: string(agent.StateBusy)},
		{Kind: kindAgent, State: "active", AgentState: string(agent.StateWaiting)},
		{Kind: kindAgent, State: "active", AgentState: string(agent.StateRetrying)},
		{Kind: kindAgent, State: "active", AgentState: string(agent.StateIdle)},
		{Kind: kindAgent, State: "active", Detail: "unreachable"},
		{Kind: kindService, State: "active"},
		{Kind: kindService, State: "gone"},
		{Kind: kindWindow, State: "open"},
		{Kind: kindWindow, State: "closed"},
		{Kind: kindService, State: "inactive"},
	}
	for _, r := range rows {
		if got := stateLabelPlain(r); strings.Contains(got, "\033") {
			t.Errorf("stateLabelPlain(%+v) = %q, contains an ANSI escape code", r, got)
		}
	}
}

// TestStateLabelIsColoredWhenUseColorIsOn confirms the colored variant still
// does its job for the free-form (non-tabwriter) agentSummaryLine.
func TestStateLabelIsColoredWhenUseColorIsOn(t *testing.T) {
	old := useColor
	useColor = true
	defer func() { useColor = old }()

	got := stateLabel(row{Kind: kindAgent, State: "active", AgentState: string(agent.StateBusy)})
	if !strings.Contains(got, "\033") {
		t.Errorf("stateLabel = %q, want an ANSI escape code when useColor is on", got)
	}
}

func TestAgentSummaryLineShowsTodoProgress(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateBusy),
		TodoTotal:  9, TodoDone: 6, Sessions: 1,
	})
	want := "  agent main: busy · todo 6/9 · 1 session(s)"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestAgentSummaryLineShowsCurrentTaskOnItsOwnLine(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateBusy),
		TodoTotal:  9, TodoDone: 6, TodoNow: "Wire the notch widget",
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "todo 6/9") {
		t.Errorf("first line missing progress: %q", lines[0])
	}
	if !strings.Contains(lines[1], "task: Wire the notch widget") {
		t.Errorf("second line missing the current task: %q", lines[1])
	}
}

func TestAgentSummaryLineOmitsTodosWhenUnused(t *testing.T) {
	// Most short one-shot sessions never build a list; the line must not
	// grow a stray "todo 0/0".
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateIdle),
	})
	if strings.Contains(got, "todo") {
		t.Errorf("got %q, want no todo segment", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("got a second line with no current task: %q", got)
	}
}

func TestAgentSummaryLineShowsCurrentToolActivity(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateBusy),
		ActTool:    "bash", ActTitle: "bin/rails test",
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "now:") || !strings.Contains(lines[1], "bash: bin/rails test") {
		t.Errorf("activity line = %q", lines[1])
	}
}

func TestAgentSummaryLineShowsBothTaskAndActivity(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateBusy),
		TodoTotal:  9, TodoDone: 6, TodoNow: "Add a smoke test",
		ActTool: "bash", ActTitle: "bin/rails test",
	})
	if n := len(strings.Split(got, "\n")); n != 3 {
		t.Fatalf("got %d lines, want 3 (summary, task, activity):\n%s", n, got)
	}
}

func TestAgentSummaryLineShowsBothSidesOfTheConversation(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateIdle),
		LastUser:   "cancel workflow shouldn't be here",
		LastAgent:  "Removed the cancel button from the onboarding nav.",
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "you:") || !strings.Contains(lines[1], "cancel workflow") {
		t.Errorf("user line = %q", lines[1])
	}
	if !strings.Contains(lines[2], "said:") || !strings.Contains(lines[2], "Removed the cancel button") {
		t.Errorf("assistant line = %q", lines[2])
	}
}

func TestAgentSummaryLineOmitsMessagesWhenAbsent(t *testing.T) {
	got := agentSummaryLine(row{Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateIdle)})
	if strings.Contains(got, "you:") || strings.Contains(got, "said:") {
		t.Errorf("got %q, want no message lines", got)
	}
}

func TestAgentSummaryLineShowsWhatItIsBlockedOn(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateWaiting),
		PendKind:   "permission", PendHeader: "external_directory",
		PendDetail: "external_directory",
		PendExtra:  "/home/x/gems/ruby_llm/*",
	})
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[1], "needs:") || !strings.Contains(lines[1], "permission: external_directory") {
		t.Errorf("needs line = %q", lines[1])
	}
	// detail duplicating the header must not be repeated
	if strings.Count(lines[1], "external_directory") != 1 {
		t.Errorf("header repeated: %q", lines[1])
	}
	if !strings.Contains(lines[1], "ruby_llm") {
		t.Errorf("missing the resource it wants: %q", lines[1])
	}
}

func TestAgentSummaryLineShowsQuestionOptions(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateWaiting),
		PendKind:   "question", PendHeader: "Fork trigger",
		PendDetail: "When should canaveral fork a session?",
		PendExtra:  "Always / Never",
	})
	if !strings.Contains(got, "question: Fork trigger") || !strings.Contains(got, "Always / Never") {
		t.Errorf("got %q", got)
	}
}

func TestAgentSummaryLineShowsTimeSinceThePrompt(t *testing.T) {
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState:  string(agent.StateBusy),
		SincePrompt: 3*time.Minute + 37*time.Second,
	})
	if !strings.Contains(got, "on this 3m37s") {
		t.Errorf("got %q, want the prompt timer", got)
	}
}

func TestAgentSummaryLineOmitsPromptTimerWhenIdle(t *testing.T) {
	// Once it has finished, "idle" already says how long it has been sitting.
	got := agentSummaryLine(row{
		Kind: kindAgent, Name: "main", State: "active",
		AgentState: string(agent.StateIdle), SincePrompt: time.Hour,
	})
	if strings.Contains(got, "on this") {
		t.Errorf("got %q, want no prompt timer when idle", got)
	}
}

func TestRunStatusReportsNoFeaturesYet(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "no-features-yet"))

	out := captureStdout(t, func() {
		if err := runStatus(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no features yet") {
		t.Errorf("out = %q", out)
	}
}

func TestRunStatusJSONReportsNoFeaturesYetAsEmptyArray(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "no-features-json"))

	out := captureStdout(t, func() {
		if err := runStatus(context.Background(), []string{"--json"}); err != nil {
			t.Fatal(err)
		}
	})
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("out = %q, want an empty JSON array", out)
	}
}

func TestRunStatusListsANamedFeature(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "status-named", "small-fixes"))

	out := captureStdout(t, func() {
		if err := runStatus(context.Background(), []string{"small-fixes"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "small-fixes") {
		t.Errorf("out = %q, want the feature name", out)
	}
}

func TestRunStatusUnknownFeatureErrors(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "status-unknown"))

	err := runStatus(context.Background(), []string{"does-not-exist"})
	if err == nil {
		t.Error("runStatus should fail asking for a feature that was never created")
	}
}

func TestResolveStatusTargetsDefaultsToTheWholeProject(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "status-targets", "a", "b"))

	got, err := resolveStatusTargets(nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2: %+v", len(got), got)
	}
}

func TestRunLsReportsNoFeaturesYet(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "ls-no-features"))

	out := captureStdout(t, func() {
		if err := runLs(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no features yet") {
		t.Errorf("out = %q", out)
	}
}

func TestRunLsNamesOnlyPrintsOneNamePerLine(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Chdir(completeProject(t, "ls-names", "small-fixes", "onboarding/step1"))

	out := captureStdout(t, func() {
		if err := runLs(context.Background(), []string{"--names"}); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %v, want exactly two feature names", lines)
	}
	got := map[string]bool{lines[0]: true, lines[1]: true}
	if !got["small-fixes"] || !got["onboarding/step1"] {
		t.Errorf("lines = %v, want small-fixes and onboarding/step1", lines)
	}
}

func TestRunLsListsAFeatureWithItsPorts(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := completeProject(t, "ls-feature")
	f := &state.Feature{
		Project: "ls-feature", Name: "small-fixes", Root: root, Branch: "small-fixes",
		Ports:    map[string]int{"web": 3001},
		Services: []state.Service{{Name: "web", Unit: "canaveral-ls-feature-small-fixes-svc-web-nonexistent"}},
	}
	if err := state.Save(f); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	out := captureStdout(t, func() {
		if err := runLs(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "small-fixes") {
		t.Errorf("out = %q, want the feature name", out)
	}
	if !strings.Contains(out, "3001") {
		t.Errorf("out = %q, want its ports summarized", out)
	}
}

func TestRunLsAllCoversEveryProject(t *testing.T) {
	clearFeatureEnv(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	completeProject(t, "ls-all-a", "one")
	root := completeProject(t, "ls-all-b", "two")
	t.Chdir(root)

	out := captureStdout(t, func() {
		if err := runLs(context.Background(), []string{"--all", "--names"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("out = %q, want features from both projects", out)
	}
}
