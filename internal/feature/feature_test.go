package feature

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/manifest"
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

	if !strings.HasPrefix(env["PATH"], "/toolchain/bin") {
		t.Errorf("toolchain PATH should stay in front, got %q", env["PATH"])
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

// TestBaseEnvForFillsPATHWhenToolchainHasNone covers the window/service PATH
// bug: without a toolchain-resolved PATH, a spawned window or unit would
// otherwise see nothing explicit at all and fall back to whatever PATH
// launched canaveral (a Hyprland keybind's, missing rc-file additions like
// opencode's own install directory). baseEnvFor must fill that gap with
// agent.ShellPATH() instead of leaving it unset.
func TestBaseEnvForFillsPATHWhenToolchainHasNone(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{Project: "norules", Name: "f"}
	env := baseEnvFor(m, f, nil)
	want := agent.ShellPATH()
	if env["PATH"] != want {
		t.Errorf("PATH = %q, want agent.ShellPATH() = %q", env["PATH"], want)
	}
}

// TestBaseEnvForExtendsToolchainPATH covers the harder half of the same bug.
// `mise env` builds its PATH by prepending shims to the PATH it inherits,
// which is canaveral's own truncated one, so "the toolchain resolved a PATH"
// never meant "the PATH is complete". Skipping the merge in that case left
// every mise project's windows without opencode on PATH — they spawned and
// died instantly, while windows running something from /usr/bin opened fine.
func TestBaseEnvForExtendsToolchainPATH(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	f := &state.Feature{Project: "norules", Name: "f"}
	// A shim directory plus one entry of the truncated PATH mise inherited.
	tc := map[string]string{"PATH": "/shims" + string(filepath.ListSeparator) + "/usr/bin"}
	env := baseEnvFor(m, f, tc)

	got := filepath.SplitList(env["PATH"])
	if len(got) < 2 || got[0] != "/shims" || got[1] != "/usr/bin" {
		t.Fatalf("toolchain entries must keep their order and precedence, got %v", got)
	}
	for _, want := range filepath.SplitList(agent.ShellPATH()) {
		if !slices.Contains(got, want) {
			t.Errorf("PATH is missing %q from agent.ShellPATH(): %v", want, got)
		}
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
