package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// pinHookBin makes the hooks name bin rather than the test binary.
func pinHookBin(t *testing.T, bin string) {
	t.Helper()
	old := hookBin
	hookBin = func() (string, error) { return bin, nil }
	t.Cleanup(func() { hookBin = old })
}

// hookDir points the hook state at a temporary runtime directory.
func hookDir(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
}

// fireHook runs one hook event through the recorder, as Claude Code would.
func fireHook(t *testing.T, event map[string]any) {
	t.Helper()
	if _, ok := event["session_id"]; !ok {
		event["session_id"] = "s1"
	}
	b, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordClaudeHook(strings.NewReader(string(b))); err != nil {
		t.Fatal(err)
	}
}

var askInput = map[string]any{"questions": []any{map[string]any{
	"header": "Next step", "question": "Push the fixes?",
	"options": []any{map[string]any{"label": "Push"}, map[string]any{"label": "Wait"}},
}}}

func nothingAnswered(string) bool { return false }

func TestClaudeSettingsInstallTheHooks(t *testing.T) {
	pinHookBin(t, "/home/me/.local/bin/canaveral")
	var got struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct{ Type, Command string }
		}
	}
	if err := json.Unmarshal([]byte(claudeSettings()), &got); err != nil {
		t.Fatal(err)
	}
	for _, ev := range claudeHookEvents {
		groups := got.Hooks[ev]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s hooks = %+v, want one command", ev, groups)
		}
		if c := groups[0].Hooks[0].Command; c != "/home/me/.local/bin/canaveral claude-hook" {
			t.Errorf("%s command = %q", ev, c)
		}
	}
}

func TestClaudeHookQuestionIsPendingUntilAnswered(t *testing.T) {
	hookDir(t)
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": askInput})
	p := claudeHookPending("s1", nothingAnswered)
	if p == nil || p.Kind != BlockQuestion || p.Header != "Next step" || p.Detail != "Push the fixes?" || len(p.Options) != 2 {
		t.Fatalf("pending = %+v, want the question", p)
	}
	fireHook(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1"})
	if p := claudeHookPending("s1", nothingAnswered); p != nil {
		t.Errorf("pending after the answer = %+v, want nil", p)
	}
	if _, err := os.Stat(claudeHookPath("s1")); !os.IsNotExist(err) {
		t.Errorf("state file left behind with nothing in it: %v", err)
	}
}

func TestClaudeHookPermissionTakesThePreToolUseID(t *testing.T) {
	hookDir(t)
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Read", "tool_use_id": "t1"})
	if p := claudeHookPending("s1", nothingAnswered); p != nil {
		t.Fatalf("a call that just runs is pending: %+v", p)
	}
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "tool_use_id": "t2"})
	fireHook(t, map[string]any{"hook_event_name": "PermissionRequest", "tool_name": "Bash", "tool_input": map[string]any{"command": "git push\n--force"}})
	p := claudeHookPending("s1", nothingAnswered)
	if p == nil || p.Kind != BlockPermission || p.Header != "Bash" || p.Detail != "git push" {
		t.Fatalf("pending = %+v, want permission for git push", p)
	}
	// Denied: no hook fires, but the transcript gets the call's result.
	if p := claudeHookPending("s1", func(id string) bool { return id == "t2" }); p != nil {
		t.Errorf("pending once the transcript has the result = %+v, want nil", p)
	}
	// Another call finishing first leaves the dialog up.
	fireHook(t, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_use_id": "t1"})
	if p := claudeHookPending("s1", nothingAnswered); p == nil {
		t.Error("an unrelated call finishing cleared the permission dialog")
	}
}

func TestClaudeHookTurnEndClearsEverything(t *testing.T) {
	for _, ev := range []string{"Stop", "UserPromptSubmit"} {
		t.Run(ev, func(t *testing.T) {
			hookDir(t)
			fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "ExitPlanMode", "tool_use_id": "t1", "tool_input": map[string]any{"plan": "do it"}})
			if p := claudeHookPending("s1", nothingAnswered); p == nil || p.Header != "approve plan" {
				t.Fatalf("pending = %+v, want the plan", p)
			}
			fireHook(t, map[string]any{"hook_event_name": ev})
			if p := claudeHookPending("s1", nothingAnswered); p != nil {
				t.Errorf("pending after %s = %+v, want nil", ev, p)
			}
		})
	}
}

func TestClaudeHookIgnoresSubagentsAndOddSessionIDs(t *testing.T) {
	hookDir(t)
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "agent_id": "a1", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": askInput})
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "session_id": "../escape", "tool_name": "AskUserQuestion", "tool_use_id": "t2", "tool_input": askInput})
	if p := claudeHookPending("s1", nothingAnswered); p != nil {
		t.Errorf("a subagent's question is pending: %+v", p)
	}
	entries, err := os.ReadDir(claudeHookDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".lock") {
			t.Errorf("unexpected state file %s", e.Name())
		}
	}
}

// The transcript shows only the prompt while a question is up; the hooks
// are what turn that from working into waiting.
func TestClaudeHooksMakeAnOpenTurnWait(t *testing.T) {
	hookDir(t)
	fireHook(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "AskUserQuestion", "tool_use_id": "t1", "tool_input": askInput})

	var e claudeEntry
	if err := json.Unmarshal([]byte(claudePrompt), &e); err != nil {
		t.Fatal(err)
	}
	for _, live := range []bool{true, false} {
		h := Health{Reachable: true, Live: live, State: StateIdle}
		s := scanClaude([]claudeEntry{e}, time.Now(), live, &h)
		applyClaudeHooks("s1", s, &h)
		want := StateWaiting
		if !live {
			want = StateIdle
		}
		if h.State != want {
			t.Errorf("live=%v: state = %q, want %q", live, h.State, want)
		}
	}
}
