package feature

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/unit"
)

func TestPortOf(t *testing.T) {
	cases := map[string]int{
		"http://127.0.0.1:4096":  4096,
		"http://127.0.0.1:4096/": 4096,
		"nonsense":               0,
	}
	for in, want := range cases {
		if got := portOf(in); got != want {
			t.Errorf("portOf(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestForkedSessionForNoNamespaceIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "small-fixes", Worktree: "/wt"},
		state.Agent{Name: "main", Tool: "opencode", URL: "http://x"}); got != "" {
		t.Errorf("forkedSessionFor for an unnamespaced feature = %q, want empty", got)
	}
}

func TestForkedSessionForNothingToForkFromIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "onboarding/step1", Worktree: "/wt"},
		state.Agent{Name: "main", Tool: "opencode", URL: "http://x"}); got != "" {
		t.Errorf("forkedSessionFor with no siblings = %q, want empty", got)
	}
}

func TestForkedSessionForUsesRecordedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	err := skills.RecordSession("norules", "onboarding", "main", skills.SessionRecord{
		Feature: "onboarding/step1", SessionID: "ses_recorded",
		Worktree: t.TempDir(), UpdatedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	var moved string
	srv := forkFakeServer(t, `{"data":[]}`, "ses_forked", &moved)

	got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "onboarding/step2", Worktree: "/wt/step2"},
		state.Agent{Name: "main", Tool: "opencode", URL: srv.URL})
	if want := "ses_forked"; got != want {
		t.Errorf("forkedSessionFor = %q, want %q (the forked copy, not the source)", got, want)
	}
	// The copy must be re-homed, or it would operate in the source's
	// worktree and stay invisible to anything scoping by directory.
	if want := "ses_forked -> /wt/step2"; moved != want {
		t.Errorf("move = %q, want %q", moved, want)
	}
}

func TestForkedSessionForPrefersMoreRecentLiveSibling(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	old := time.Now().Add(-time.Hour)
	if err := skills.RecordSession("norules", "onboarding", "main", skills.SessionRecord{
		Feature: "onboarding/step1", SessionID: "ses_stale",
		Worktree: t.TempDir(), UpdatedAt: old,
	}); err != nil {
		t.Fatal(err)
	}

	sibWorktree := t.TempDir()
	sessions := `{"data":[{"id":"ses_live","location":{"directory":"` + sibWorktree + `"},"time":{"updated":` +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + `}}]}`
	srv := agentFakeServer(t, sessions, `[]`)

	sib := &state.Feature{
		Project: "norules", Name: "onboarding/step1", Worktree: sibWorktree,
		Agents: []state.Agent{{Name: "main", Tool: "opencode", URL: srv.URL}},
	}
	if err := state.Save(sib); err != nil {
		t.Fatal(err)
	}

	var moved string
	fsrv := forkFakeServer(t, `{"data":[]}`, "ses_forked_live", &moved)

	got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "onboarding/step2", Worktree: "/wt/step2"},
		state.Agent{Name: "main", Tool: "opencode", URL: fsrv.URL})
	if want := "ses_forked_live"; got != want {
		t.Errorf("forkedSessionFor = %q, want %q", got, want)
	}
	if !strings.Contains(moved, "-> /wt/step2") {
		t.Errorf("move = %q, want it re-homed into this feature's worktree", moved)
	}
}

func TestForkedSessionForFallsBackToAFreshSessionWhenForkFails(t *testing.T) {
	// Continuity is a convenience; if the fork or move fails the feature
	// should still come up, just without the previous conversation.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := skills.RecordSession("norules", "onboarding", "main", skills.SessionRecord{
		Feature: "onboarding/step1", SessionID: "ses_x",
		Worktree: t.TempDir(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	// Nothing listening: fork cannot succeed.
	if got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "onboarding/step2", Worktree: "/wt"},
		state.Agent{Name: "main", Tool: "opencode", URL: "http://127.0.0.1:1"}); got != "" {
		t.Errorf("forkedSessionFor = %q, want empty when the fork fails", got)
	}
}

func TestForkedSessionForOnlyMatchesSameNamespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := skills.RecordSession("norules", "other-epic", "main", skills.SessionRecord{
		Feature: "other-epic/step1", SessionID: "ses_unrelated", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got := forkedSessionFor(context.Background(),
		&state.Feature{Project: "norules", Name: "onboarding/step1", Worktree: "/wt"},
		state.Agent{Name: "main", Tool: "opencode", URL: "http://x"})
	if got != "" {
		t.Errorf("forkedSessionFor leaked across namespaces: got %q, want empty", got)
	}
}

// forkFakeServer answers Probe's endpoints plus fork and move, returning
// the ID it hands back from the fork so a test can assert on it.
func forkFakeServer(t *testing.T, sessions, newID string, moved *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/fork") {
			_, _ = w.Write([]byte(`{"id":"` + newID + `"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	})
	mux.HandleFunc("/experimental/control-plane/move-session", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			SessionID   string `json:"sessionID"`
			Destination struct {
				Directory string `json:"directory"`
			} `json:"destination"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if moved != nil {
			*moved = body.SessionID + " -> " + body.Destination.Directory
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// agentFakeServer serves the endpoints agent.Probe depends on, mirroring
// internal/agent's own test helper (kept local to avoid an inter-package
// test dependency).
func agentFakeServer(t *testing.T, sessions, messages string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(messages))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestReconcileAgentsNoopWithoutDeclaredAgents(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "reconcile-agents-noop"}
	prog := newProgress(f, state.PhaseBooting, 0)
	res := &Result{}
	err := reconcileAgents(context.Background(), &manifest.Manifest{}, f, tmpl.Vars{}, nil, res, quietReporter{}, prog)
	if err != nil {
		t.Fatalf("reconcileAgents: %v", err)
	}
	if len(f.Agents) != 0 {
		t.Errorf("f.Agents = %v, want none", f.Agents)
	}
}

func TestReconcileAgentsAdoptsAnAlreadyRunningUnit(t *testing.T) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("no systemctl")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("no opencode binary")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ctx := context.Background()

	f := &state.Feature{Project: "reconcile-agents-test", Name: "adopt", Worktree: t.TempDir()}
	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logDir, "agent-main.log")
	unitName := unit.Name(f.Project+"-"+f.Name, "agent", "main")

	// Start a real, long-running unit under the exact name reconcileAgents
	// will look up, so unit.Query reports it Running and reconcileAgents
	// takes the adopt path rather than starting a new one.
	if err := unit.Start(ctx, unit.Spec{
		Name: unitName, Cmd: "sleep 60", Dir: f.Worktree, LogPath: logPath,
	}); err != nil {
		t.Fatalf("unit.Start: %v", err)
	}
	t.Cleanup(func() {
		_ = unit.Stop(context.Background(), unitName)
		unit.Reset(context.Background(), unitName)
	})

	// unit.Start truncates LogPath; write the line DiscoverURL looks for
	// only now, simulating an agent that already announced its address in
	// a previous reconcile.
	if err := os.WriteFile(logPath, []byte("listening on http://127.0.0.1:9999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := &manifest.Manifest{Agents: []manifest.Agent{{Name: "main", Tool: "opencode", Dir: "."}}}
	prog := newProgress(f, state.PhaseBooting, 1)
	res := &Result{}
	if err := reconcileAgents(ctx, m, f, tmpl.Vars{}, nil, res, quietReporter{}, prog); err != nil {
		t.Fatalf("reconcileAgents: %v", err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("f.Agents = %v, want exactly one", f.Agents)
	}
	if f.Agents[0].URL != "http://127.0.0.1:9999" {
		t.Errorf("adopted URL = %q, want recovered from the log", f.Agents[0].URL)
	}
	if len(res.StartedAgent) != 0 {
		t.Errorf("StartedAgent = %v, adopting a running unit must not report a fresh start", res.StartedAgent)
	}
}

// fakeBin puts an executable of the given name on a fresh PATH, so a test
// can exercise a harness without the real tool being installed.
func fakeBin(t *testing.T, name string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// An agent whose harness has no server is started by its own window, not by
// canaveral. Reconciling one must record it — status, watch and session
// continuity all key off the record — while starting no unit at all.
func TestReconcileAgentsRecordsAnUnsupervisedAgentWithoutAUnit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fakeBin(t, "claude")

	root := t.TempDir()
	f := &state.Feature{Project: "unsupervised-test", Name: "main", Root: root, Worktree: t.TempDir()}
	m := &manifest.Manifest{Root: root, Agents: []manifest.Agent{{Name: "main", Tool: "claude", Dir: "."}}}
	prog := newProgress(f, state.PhaseBooting, 1)
	res := &Result{}

	if err := reconcileAgents(context.Background(), m, f, tmpl.Vars{}, nil, res, quietReporter{}, prog); err != nil {
		t.Fatalf("reconcileAgents: %v", err)
	}
	if len(f.Agents) != 1 {
		t.Fatalf("f.Agents = %v, want exactly one", f.Agents)
	}
	got := f.Agents[0]
	if got.Unit != "" || got.URL != "" || got.Port != 0 {
		t.Errorf("agent = %+v, want no unit, URL or port for a harness canaveral does not start", got)
	}
	if got.Dir != f.Worktree {
		t.Errorf("Dir = %q, want the worktree — it is what scopes the conversation", got.Dir)
	}
	if len(res.launched) != 0 || len(res.StartedAgent) != 0 {
		t.Errorf("launched = %v, StartedAgent = %v, want nothing started", res.launched, res.StartedAgent)
	}
}

// A tool canaveral has no harness for must fail by name and before anything
// is started. Load rejects one, so the only way here is a state file written
// by a newer canaveral — which is exactly when a clear message matters.
func TestReconcileAgentsFailsOnAnUnknownTool(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	f := &state.Feature{Project: "unknown-tool-test", Name: "main", Worktree: t.TempDir()}
	m := &manifest.Manifest{Agents: []manifest.Agent{{Name: "main", Tool: "nano-banana", Dir: "."}}}
	prog := newProgress(f, state.PhaseBooting, 1)

	err := reconcileAgents(context.Background(), m, f, tmpl.Vars{}, nil, &Result{}, quietReporter{}, prog)
	if err == nil {
		t.Fatal("reconcileAgents succeeded with a tool it has no harness for")
	}
	if !strings.Contains(err.Error(), "nano-banana") || !strings.Contains(err.Error(), "main") {
		t.Errorf("error = %v, want it to name both the agent and the tool", err)
	}
}

// The flag that reopens a conversation is the harness's to spell, so a
// manifest window splices in {{.Agent.main.Session}} without knowing which
// tool it got.
func TestVarsForUsesTheHarnessesOwnSessionFlag(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cases := map[string]string{
		"opencode": "--session ses_1",
		"claude":   "--resume ses_1",
	}
	for tool, want := range cases {
		t.Run(tool, func(t *testing.T) {
			f := &state.Feature{
				Project: "p", Name: "f", Worktree: t.TempDir(),
				Agents: []state.Agent{{Name: "main", Tool: tool}},
			}
			v := varsFor(context.Background(), &manifest.Manifest{}, f, false,
				map[string]string{"main": "ses_1"})
			if got := v.Agent["main"].Session; got != want {
				t.Errorf("Session = %q, want %q", got, want)
			}
			// Fork is the former spelling and must keep agreeing with it.
			if got := v.Agent["main"].Fork; got != want {
				t.Errorf("Fork = %q, want %q", got, want)
			}
		})
	}
}
