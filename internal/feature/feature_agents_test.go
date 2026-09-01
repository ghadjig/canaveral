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

func TestForkArgsForNoNamespaceIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := forkArgsFor(context.Background(), "norules", "small-fixes", "main", "http://x", "/wt"); got != "" {
		t.Errorf("forkArgsFor for an unnamespaced feature = %q, want empty", got)
	}
}

func TestForkArgsForNothingToForkFromIsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := forkArgsFor(context.Background(), "norules", "onboarding/step1", "main", "http://x", "/wt"); got != "" {
		t.Errorf("forkArgsFor with no siblings = %q, want empty", got)
	}
}

func TestForkArgsForUsesRecordedSession(t *testing.T) {
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

	got := forkArgsFor(context.Background(), "norules", "onboarding/step2", "main", srv.URL, "/wt/step2")
	if want := "--session ses_forked"; got != want {
		t.Errorf("forkArgsFor = %q, want %q (the forked copy, not the source)", got, want)
	}
	// The copy must be re-homed, or it would operate in the source's
	// worktree and stay invisible to anything scoping by directory.
	if want := "ses_forked -> /wt/step2"; moved != want {
		t.Errorf("move = %q, want %q", moved, want)
	}
}

func TestForkArgsForPrefersMoreRecentLiveSibling(t *testing.T) {
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

	got := forkArgsFor(context.Background(), "norules", "onboarding/step2", "main", fsrv.URL, "/wt/step2")
	if want := "--session ses_forked_live"; got != want {
		t.Errorf("forkArgsFor = %q, want %q", got, want)
	}
	if !strings.Contains(moved, "-> /wt/step2") {
		t.Errorf("move = %q, want it re-homed into this feature's worktree", moved)
	}
}

func TestForkArgsForFallsBackToAFreshSessionWhenForkFails(t *testing.T) {
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
	if got := forkArgsFor(context.Background(), "norules", "onboarding/step2", "main", "http://127.0.0.1:1", "/wt"); got != "" {
		t.Errorf("forkArgsFor = %q, want empty when the fork fails", got)
	}
}

func TestForkArgsForOnlyMatchesSameNamespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if err := skills.RecordSession("norules", "other-epic", "main", skills.SessionRecord{
		Feature: "other-epic/step1", SessionID: "ses_unrelated", UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	got := forkArgsFor(context.Background(), "norules", "onboarding/step1", "main", "http://x", "/wt")
	if got != "" {
		t.Errorf("forkArgsFor leaked across namespaces: got %q, want empty", got)
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
