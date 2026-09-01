package feature

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/unit"
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

func TestIsLayoutFreshFalseWhenLayoutDisabled(t *testing.T) {
	m := &manifest.Manifest{}
	f := &state.Feature{Project: "p", Name: "f"}
	if isLayoutFresh(m, f, nil) {
		t.Error("a manifest with no [layout] must never be considered fresh")
	}
}

func TestIsLayoutFreshTrueWhenNothingIsOpenYet(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	f := &state.Feature{Project: "p", Name: "f"}
	if !isLayoutFresh(m, f, map[string]hypr.Client{}) {
		t.Error("layout should be fresh when none of its windows are open")
	}
}

func TestIsLayoutFreshFalseWhenOneWindowAlreadyOpen(t *testing.T) {
	m := &manifest.Manifest{}
	m.Layout.Order = []string{"chrome", "terminal"}
	f := &state.Feature{Project: "p", Name: "f"}
	open := map[string]hypr.Client{
		hypr.Class("p", "f", "chrome"): {},
	}
	if isLayoutFresh(m, f, open) {
		t.Error("a partially-open layout must not be treated as fresh")
	}
}

func TestBuildWindowSpecForAnAlreadyOpenWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "chrome", Exec: "chromium --class={{.Class}}"}
	class := hypr.Class("p", "f", "chrome")
	open := map[string]hypr.Client{class: {}}

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, open, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending != nil {
		t.Error("an already-open window must not produce a pending spawn")
	}
	if rec.Name != "chrome" || rec.Class != class || rec.Dir != f.Worktree {
		t.Errorf("rec = %+v", rec)
	}
}

func TestBuildWindowSpecForAMissingWindow(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p", Terminal: "alacritty"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "chrome", Exec: "chromium --class={{.Class}}"}

	rec, pending, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if pending == nil {
		t.Fatal("a window that is not open must produce a pending spawn")
	}
	class := hypr.Class("p", "f", "chrome")
	if pending.spec.Class != class || pending.spec.Cmd != "chromium --class="+class {
		t.Errorf("spec = %+v", pending.spec)
	}
	if pending.spec.IsTerminal {
		t.Error("an exec window must not be wrapped in a terminal")
	}
	if rec.Class != class {
		t.Errorf("rec.Class = %q, want %q", rec.Class, class)
	}
}

func TestBuildWindowSpecUsesADeclaredSubdir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := &manifest.Manifest{Root: "/p"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: "/wt"}
	w := manifest.Window{Name: "api", Exec: "app --class={{.Class}}", Dir: "api"}

	rec, _, err := buildWindowSpec(context.Background(), m, f, w, tmpl.Vars{}, nil, map[string]hypr.Client{}, quietReporter{})
	if err != nil {
		t.Fatalf("buildWindowSpec: %v", err)
	}
	if rec.Dir != filepath.Join(f.Worktree, "api") {
		t.Errorf("rec.Dir = %q, want %q", rec.Dir, filepath.Join(f.Worktree, "api"))
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

func TestUnitsForOrdersAgentsFirstThenServicesInReverse(t *testing.T) {
	// Reverse service order matters: a service started later may depend on an
	// earlier one, so it has to go down first.
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: "a"}, {Unit: "b"}, {Unit: "c"}},
		Agents:   []state.Agent{{Unit: "agent"}},
	}
	got := UnitsFor(context.Background(), f)
	want := []string{"agent", "c", "b", "a"}
	if len(got) < len(want) {
		t.Fatalf("UnitsFor = %v, want at least %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("UnitsFor[%d] = %q, want %q (full: %v)", i, got[i], w, got)
		}
	}
}

func TestUnitsForDeduplicates(t *testing.T) {
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: "dup"}, {Unit: "dup"}},
		Agents:   []state.Agent{{Unit: "dup"}},
	}
	if got := UnitsFor(context.Background(), f); len(got) != 1 || got[0] != "dup" {
		t.Errorf("UnitsFor = %v, want [dup]", got)
	}
}

func TestUnitsForSkipsEmptyNames(t *testing.T) {
	// An interrupted reconcile can leave a half-written record; stopping ""
	// would ask systemctl to stop ".service".
	f := &state.Feature{
		Project: "unitsfor-test-project", Name: "nonexistent-feature",
		Services: []state.Service{{Unit: ""}, {Unit: "real"}},
	}
	if got := UnitsFor(context.Background(), f); len(got) != 1 || got[0] != "real" {
		t.Errorf("UnitsFor = %v, want [real]", got)
	}
}

func TestAbortDoesNothingWithoutAnInterrupt(t *testing.T) {
	// An ordinary failure leaves healthy units up on purpose so `reset` can
	// adopt them instead of re-running a slow application boot.
	res := &Result{
		launched:     []string{"canaveral-x-y-svc-web"},
		StartedSvc:   []string{"web"},
		StartedAgent: []string{"main"},
	}
	res.abort(context.Background(), quietReporter{})
	if len(res.StartedSvc) != 1 || len(res.StartedAgent) != 1 {
		t.Error("abort cleared results without an interrupt")
	}
}

// quietReporter satisfies Reporter without printing during tests.
type quietReporter struct{}

func (quietReporter) Step(string, ...any) {}
func (quietReporter) OK(string, ...any)   {}
func (quietReporter) Info(string, ...any) {}
func (quietReporter) Warn(string, ...any) {}

// gitFeature builds a real repo with a feature branch and returns a state
// record pointing at it. mergeTarget shells out to git, so there is no
// meaningful way to test it against a fake.
func gitFeature(t *testing.T, merge bool) *state.Feature {
	t.Helper()
	// These tests run Remove far enough to touch the state directory, and
	// this suite executes inside a real canaveral worktree where XDG_STATE_HOME
	// points at the live one. Isolate it.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	run("checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "work")
	run("checkout", "-q", "main")
	if merge {
		run("merge", "-q", "--no-ff", "-m", "merge feat", "feat")
	}
	return &state.Feature{
		Project: "p", Name: "feat", Root: repo, Branch: "feat",
		Worktree: filepath.Join(repo, "wt"),
	}
}

func TestMergeTargetDetectsUnmergedBranch(t *testing.T) {
	merged, target, ok := mergeTarget(context.Background(), gitFeature(t, false))
	if !ok {
		t.Fatal("mergeTarget could not answer for a plain repo")
	}
	if merged {
		t.Error("a branch with its own commit is not merged into main")
	}
	if target != "main" {
		t.Errorf("target = %q, want main", target)
	}
}

func TestMergeTargetDetectsMergedBranch(t *testing.T) {
	merged, target, ok := mergeTarget(context.Background(), gitFeature(t, true))
	if !ok || !merged {
		t.Errorf("merged = %v, ok = %v, want both true", merged, ok)
	}
	if target != "main" {
		t.Errorf("target = %q, want main", target)
	}
}

func TestMergeTargetTreatsTheDefaultBranchAsMerged(t *testing.T) {
	// A feature sitting on main itself has nothing to be merged into, and
	// must not be blocked from removal by the guard.
	f := gitFeature(t, false)
	f.Branch = "main"
	merged, _, ok := mergeTarget(context.Background(), f)
	if !ok || !merged {
		t.Errorf("merged = %v, ok = %v, want both true", merged, ok)
	}
}

func TestMergeTargetCannotAnswerWithoutARepo(t *testing.T) {
	// ok=false means "do not block": refusing teardown because the repo has
	// no discoverable default branch would be worse than the risk guarded.
	f := &state.Feature{Project: "p", Name: "f", Root: t.TempDir(), Branch: "feat"}
	if _, _, ok := mergeTarget(context.Background(), f); ok {
		t.Error("mergeTarget claimed an answer for a non-repo")
	}
}

func TestMergeTargetCannotAnswerWithoutABranch(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "f", Root: "", Branch: ""}
	if _, _, ok := mergeTarget(context.Background(), f); ok {
		t.Error("mergeTarget claimed an answer with no root or branch")
	}
}

func TestRemoveRefusesUnmergedWork(t *testing.T) {
	f := gitFeature(t, false)
	err := Remove(context.Background(), f, false, false, false, quietReporter{})
	if !errors.Is(err, ErrUnmerged) {
		t.Fatalf("Remove err = %v, want ErrUnmerged", err)
	}
	// The refusal must come before anything destructive: state is still there.
	if !strings.Contains(err.Error(), "canaveral merge feat") {
		t.Errorf("error should point at merge: %v", err)
	}
}

func TestRemoveAllowsUnmergedWorkWithForce(t *testing.T) {
	f := gitFeature(t, false)
	// Not a full teardown (no state file, no worktree), but it must get past
	// the guard rather than refusing.
	err := Remove(context.Background(), f, false, true, false, quietReporter{})
	if errors.Is(err, ErrUnmerged) {
		t.Error("--force must override the merge guard")
	}
}

func TestRemoveSkipsTheGuardWhenKeepingTheWorktree(t *testing.T) {
	f := gitFeature(t, false)
	err := Remove(context.Background(), f, true, false, false, quietReporter{})
	if errors.Is(err, ErrUnmerged) {
		t.Error("--keep-worktree leaves the checkout and branch intact; nothing to guard")
	}
}

// gitBranchExists reports whether branch still exists in repo.
func gitBranchExists(t *testing.T, repo, branch string) bool {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "branch", "--list", branch).CombinedOutput()
	if err != nil {
		t.Fatalf("git branch --list: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out)) != ""
}

func TestRemoveDeletesMergedBranchAfterTeardown(t *testing.T) {
	f := gitFeature(t, true)
	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if gitBranchExists(t, f.Root, f.Branch) {
		t.Error("merged branch should have been deleted")
	}
}

func TestRemoveKeepsBranchWhenRequested(t *testing.T) {
	f := gitFeature(t, true)
	if err := Remove(context.Background(), f, false, false, true, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !gitBranchExists(t, f.Root, f.Branch) {
		t.Error("--keep-branch must keep a merged branch around")
	}
}

func TestRemoveKeepsUnmergedBranchRegardlessOfKeepBranch(t *testing.T) {
	// keepBranch only ever opts *out* of deletion; it must never be read as
	// permission to delete an unmerged branch when it is false.
	f := gitFeature(t, false)
	if err := Remove(context.Background(), f, false, true, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !gitBranchExists(t, f.Root, f.Branch) {
		t.Error("unmerged branch must survive Remove even with keepBranch=false")
	}
}

func TestRemoveRecordsNamespaceSessionBeforeTeardown(t *testing.T) {
	f := gitFeature(t, true)
	f.Project = "norules"
	f.Name = "onboarding/step1"

	sessions := `{"data":[{"id":"ses_live","location":{"directory":"` + f.Worktree + `"},"time":{"updated":` +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + `}}]}`
	srv := agentFakeServer(t, sessions, `[]`)
	f.Agents = []state.Agent{{Name: "main", Tool: "opencode", URL: srv.URL}}

	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	rec, ok, err := skills.LatestSession(f.Project, "onboarding", "main")
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if !ok {
		t.Fatal("Remove did not record the namespace session before tearing down")
	}
	if rec.SessionID != "ses_live" {
		t.Errorf("recorded session = %q, want ses_live", rec.SessionID)
	}
	if rec.Feature != f.Name {
		t.Errorf("recorded feature = %q, want %q", rec.Feature, f.Name)
	}
}

func TestRemoveDoesNotRecordSessionForUnnamespacedFeature(t *testing.T) {
	f := gitFeature(t, true)
	f.Project = "norules"
	// f.Name is "feat", not namespaced.

	sessions := `{"data":[{"id":"ses_live","location":{"directory":"` + f.Worktree + `"},"time":{"updated":` +
		strconv.FormatInt(time.Now().UnixMilli(), 10) + `}}]}`
	srv := agentFakeServer(t, sessions, `[]`)
	f.Agents = []state.Agent{{Name: "main", Tool: "opencode", URL: srv.URL}}

	if err := Remove(context.Background(), f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	if _, ok, err := skills.LatestSession(f.Project, "", "main"); err == nil && ok {
		t.Error("Remove recorded a session for a feature with no namespace")
	}
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
