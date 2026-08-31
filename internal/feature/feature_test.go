package feature

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"small-fixes":                "small-fixes",
		"change-onboarding-workflow": "change-onboarding-workflow",
		"Add Tasks Per Role":         "add-tasks-per-role",
		"feat/API_v2":                "feat/api-v2",
		"  spaced  ":                 "spaced",
		"---":                        "",
		"onboarding/ask-for-name":    "onboarding/ask-for-name",
		"Onboarding/Ask For Name":    "onboarding/ask-for-name",
		"a/b/c":                      "a/b/c",
		"/leading":                   "leading",
		"trailing/":                  "trailing",
		"double//slash":              "double/slash",
		"a/---/b":                    "a/b",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNamespace(t *testing.T) {
	cases := map[string]string{
		"small-fixes":         "",
		"onboarding/step1":    "onboarding",
		"a/b/c":               "a/b",
		"epic/sub-epic/step1": "epic/sub-epic",
	}
	for in, want := range cases {
		if got := Namespace(in); got != want {
			t.Errorf("Namespace(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPortsForOffsetsBySlot(t *testing.T) {
	m := &manifest.Manifest{Ports: map[string]int{"web": 3000, "vite": 5173}}

	// Each feature's slot shifts every declared port by the same amount, so
	// three features never collide.
	slot0 := portsFor(m, 0)
	slot1 := portsFor(m, 1)
	slot2 := portsFor(m, 2)

	if slot0["web"] != 3000 || slot1["web"] != 3001 || slot2["web"] != 3002 {
		t.Errorf("web ports = %d/%d/%d", slot0["web"], slot1["web"], slot2["web"])
	}
	if slot1["vite"] != 5174 {
		t.Errorf("vite slot 1 = %d, want 5174", slot1["vite"])
	}
}

func TestPortsForNoDeclarations(t *testing.T) {
	if got := portsFor(&manifest.Manifest{}, 3); got != nil {
		t.Errorf("portsFor with no [ports] = %v, want nil", got)
	}
}

func TestVarsForExposesPortsAndAgents(t *testing.T) {
	m := &manifest.Manifest{Name: "norules", Root: "/p/norules"}
	f := &state.Feature{
		Project: "norules", Name: "small-fixes", Slot: 1,
		Branch: "small-fixes", Worktree: "/wt/sf",
		Ports:  map[string]int{"web": 3001},
		Agents: []state.Agent{{Name: "main", URL: "http://127.0.0.1:4096"}},
	}
	v := varsFor(context.Background(), m, f, false)
	if v.Port["web"] != 3001 {
		t.Errorf("Port.web = %d", v.Port["web"])
	}
	if v.URL["web"] != "http://localhost:3001" {
		t.Errorf("URL.web = %q", v.URL["web"])
	}
	if v.Agent["main"].URL != "http://127.0.0.1:4096" {
		t.Errorf("Agent.main = %q", v.Agent["main"])
	}
}

func TestBaseEnvForExportsPortsAndSuffix(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	m.Database.SuffixEnv = "DB_SUFFIX"
	f := &state.Feature{
		Project: "norules", Name: "small-fixes", Worktree: "/wt/sf", Root: "/p",
		Ports: map[string]int{"web": 3001, "side-car": 4001}, DBSuffix: "_small_fixes",
	}
	env := baseEnvFor(m, f, map[string]string{"PATH": "/toolchain/bin"})

	if env["PATH"] != "/toolchain/bin" {
		t.Errorf("toolchain PATH should pass through, got %q", env["PATH"])
	}
	if env["CANAVERAL_FEATURE"] != "small-fixes" {
		t.Errorf("CANAVERAL_FEATURE = %q", env["CANAVERAL_FEATURE"])
	}
	if env["CANAVERAL_PORT_WEB"] != "3001" {
		t.Errorf("CANAVERAL_PORT_WEB = %q", env["CANAVERAL_PORT_WEB"])
	}
	// Dashes are not valid in environment variable names.
	if env["CANAVERAL_PORT_SIDE_CAR"] != "4001" {
		t.Errorf("CANAVERAL_PORT_SIDE_CAR = %q", env["CANAVERAL_PORT_SIDE_CAR"])
	}
	if env["DB_SUFFIX"] != "_small_fixes" {
		t.Errorf("DB_SUFFIX = %q", env["DB_SUFFIX"])
	}
}

func TestBaseEnvForSharedDatabaseSetsNoSuffix(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{Project: "norules", Name: "f"}
	env := baseEnvFor(m, f, nil)
	if _, ok := env["DB_SUFFIX"]; ok {
		t.Error("shared database must not export DB_SUFFIX")
	}
}

func TestServiceDirMapsIntoWorktree(t *testing.T) {
	m := &manifest.Manifest{Root: "/p/norules"}
	f := &state.Feature{Worktree: "/wt/sf"}

	if got := serviceDir(f, m, "."); got != "/wt/sf" {
		t.Errorf("root dir = %q, want the worktree", got)
	}
	// Sub-directory layouts (api/, ios/) must be preserved inside the worktree.
	if got := serviceDir(f, m, "api"); got != "/wt/sf/api" {
		t.Errorf("sub dir = %q, want /wt/sf/api", got)
	}
	// A path outside the project cannot live in the worktree.
	if got := serviceDir(f, m, "/elsewhere"); got != "/elsewhere" {
		t.Errorf("absolute dir = %q, want /elsewhere", got)
	}
}

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

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	cases := map[string]string{
		"~/.config/google-chrome": filepath.Join(home, ".config/google-chrome"),
		"~":                       home,
		"/absolute/path":          "/absolute/path",
		"relative/path":           "relative/path",
	}
	for in, want := range cases {
		got, err := expandHome(in)
		if err != nil {
			t.Errorf("expandHome(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("expandHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitRatioChainMatchesLiveVerifiedValues(t *testing.T) {
	// These exact numbers (0.8, 0.6667, 1.0) were verified against a real
	// dwindle workspace: applying them via hyprctl dispatch splitratio to 4
	// chained windows produced columns of 39.75%/19.8%/19.8%/19.7% of the
	// monitor width — a 40/20/20/20 split within rounding.
	order := []string{"chrome", "opencode", "terminal", "serverlogs"}
	fractions := map[string]float64{
		"chrome": 0.4, "opencode": 0.2, "terminal": 0.2, "serverlogs": 0.2,
	}
	got := splitRatioChain(order, fractions)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (one per split point, none for the last window)", len(got))
	}
	want := []float64{0.8, 0.6667, 1.0}
	for i, w := range want {
		if diff := got[i] - w; diff > 0.001 || diff < -0.001 {
			t.Errorf("ratio[%d] = %v, want %v", i, got[i], w)
		}
	}
}

func TestSplitRatioChainEvenSplitIsNeutral(t *testing.T) {
	// An even split at any point in the chain must be ratio 1.0 (confirmed
	// empirically to mean "50/50", despite "1.0" not obviously meaning that).
	got := splitRatioChain([]string{"a", "b"}, map[string]float64{"a": 0.5, "b": 0.5})
	if len(got) != 1 || got[0] != 1.0 {
		t.Errorf("got = %v, want [1.0]", got)
	}
}

func TestSplitRatioChainHandlesImperfectSum(t *testing.T) {
	// [layout.current] reflects a live, hand-resized window and rarely sums
	// to exactly 1.0 (only one column was resized; the others kept their old
	// size). Each step only looks at the remaining sum from that point on,
	// so this must not error or divide by zero, and should still produce a
	// sensible (clamped, finite) ratio.
	order := []string{"chrome", "opencode", "terminal", "serverlogs"}
	fractions := map[string]float64{
		"chrome": 0.34, "opencode": 0.2, "terminal": 0.2, "serverlogs": 0.2, // sums to 0.94
	}
	got := splitRatioChain(order, fractions)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	for i, r := range got {
		if r <= 0 || r >= 2 {
			t.Errorf("ratio[%d] = %v, out of the valid (0,2) dwindle range", i, r)
		}
	}
}

func TestSplitRatioChainClampsExtremeInput(t *testing.T) {
	// A hand-edited or corrupted current value must not be forwarded to
	// hyprctl as a nonsense ratio.
	got := splitRatioChain([]string{"a", "b"}, map[string]float64{"a": 100, "b": 0.001})
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0] > 1.9 {
		t.Errorf("ratio = %v, want clamped to <= 1.9", got[0])
	}
}

func TestSplitRatioChainSingleWindow(t *testing.T) {
	// One window in [layout] means nothing to split at all.
	got := splitRatioChain([]string{"solo"}, map[string]float64{"solo": 1.0})
	if len(got) != 0 {
		t.Errorf("got = %v, want empty", got)
	}
}

func TestSplitRatioChainEmptyOrder(t *testing.T) {
	if got := splitRatioChain(nil, nil); got != nil {
		t.Errorf("got = %v, want nil", got)
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

func TestUpsertServiceReplacesInPlace(t *testing.T) {
	list := []state.Service{
		{Name: "web", Unit: "old-web"},
		{Name: "jobs", Unit: "jobs"},
	}
	got := upsertService(list, state.Service{Name: "web", Unit: "new-web"}, true)
	if len(got) != 2 {
		t.Fatalf("got %d services, want 2: %+v", len(got), got)
	}
	var web *state.Service
	for i := range got {
		if got[i].Name == "web" {
			web = &got[i]
		}
	}
	if web == nil || web.Unit != "new-web" {
		t.Errorf("web not replaced: %+v", got)
	}
}

func TestUpsertServiceDropsFailedOptional(t *testing.T) {
	list := []state.Service{{Name: "web"}, {Name: "css", Optional: true}}
	// An optional service that did not come back must leave state, or status
	// would keep claiming it is running.
	got := upsertService(list, state.Service{Name: "css", Optional: true}, false)
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("failed optional service still recorded: %+v", got)
	}
}

func TestUpsertServiceAddsWhenAbsent(t *testing.T) {
	got := upsertService(nil, state.Service{Name: "web"}, true)
	if len(got) != 1 || got[0].Name != "web" {
		t.Errorf("service not added: %+v", got)
	}
}

func TestServiceNamesReportsEmpty(t *testing.T) {
	if got := serviceNames(&manifest.Manifest{}); len(got) != 1 || got[0] != "(none declared)" {
		t.Errorf("serviceNames on empty manifest = %v", got)
	}
	m := &manifest.Manifest{Services: []manifest.Service{{Name: "web"}, {Name: "jobs"}}}
	got := serviceNames(m)
	if len(got) != 2 || got[0] != "web" || got[1] != "jobs" {
		t.Errorf("serviceNames = %v", got)
	}
}
