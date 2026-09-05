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
	"github.com/bandito/canaveral/internal/tmpl"
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
	v := varsFor(context.Background(), m, f, false, nil)
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

// TestEnvForRendersProjectEnv is the point of rendering [env] at all: a
// project states its per-feature isolation once, at the top of the manifest,
// and every process the feature runs inherits it. Before this, [env] was
// merged in verbatim at seven call sites and "{{.DBSuffix}}" reached the
// application as those literal nine characters.
func TestEnvForRendersProjectEnv(t *testing.T) {
	m := &manifest.Manifest{
		Name: "norules",
		Env: map[string]string{
			"DATABASE_URL": "postgres://localhost:5432/norules_test{{.DBSuffix}}",
			"REDIS_URL":    "redis://localhost:6379/{{.Slot}}",
			"STATIC":       "no templates here",
		},
	}
	f := &state.Feature{
		Project: "norules", Name: "small-fixes", Slot: 2,
		Worktree: "/wt/sf", Root: "/p", DBSuffix: "_small_fixes",
	}
	env, err := envFor(m, f, nil, tmpl.Vars{Slot: f.Slot, DBSuffix: f.DBSuffix})
	if err != nil {
		t.Fatalf("envFor: %v", err)
	}

	if want := "postgres://localhost:5432/norules_test_small_fixes"; env["DATABASE_URL"] != want {
		t.Errorf("DATABASE_URL = %q, want %q", env["DATABASE_URL"], want)
	}
	// The whole reason a redis database number works: two features on the
	// same server never share one, so a FLUSHDB in one suite cannot reach
	// the other.
	if want := "redis://localhost:6379/2"; env["REDIS_URL"] != want {
		t.Errorf("REDIS_URL = %q, want %q", env["REDIS_URL"], want)
	}
	if env["STATIC"] != "no templates here" {
		t.Errorf("STATIC = %q", env["STATIC"])
	}
	// canaveral's own variables must survive alongside it.
	if env["CANAVERAL_FEATURE"] != "small-fixes" {
		t.Errorf("CANAVERAL_FEATURE = %q", env["CANAVERAL_FEATURE"])
	}
}

// TestEnvForProjectEnvOverridesOwn keeps [env] the last word. A project that
// needs to override a canaveral-set variable is entitled to; silently
// ignoring what the manifest says would be worse than either outcome.
func TestEnvForProjectEnvOverridesOwn(t *testing.T) {
	m := &manifest.Manifest{
		Name: "norules",
		Env:  map[string]string{"CANAVERAL_PORT_WEB": "9999"},
	}
	f := &state.Feature{Project: "norules", Name: "f", Ports: map[string]int{"web": 3001}}
	env, err := envFor(m, f, nil, tmpl.Vars{})
	if err != nil {
		t.Fatalf("envFor: %v", err)
	}
	if env["CANAVERAL_PORT_WEB"] != "9999" {
		t.Errorf("CANAVERAL_PORT_WEB = %q, want the manifest's 9999", env["CANAVERAL_PORT_WEB"])
	}
}

// TestEnvForRejectsUnknownKey covers missingkey=error reaching [env] too. A
// typo that rendered empty would produce "postgres://localhost/norules_test"
// — the *shared* database — which is precisely the collision the suffix
// exists to prevent, arrived at silently.
func TestEnvForRejectsUnknownKey(t *testing.T) {
	m := &manifest.Manifest{
		Name: "norules",
		Env:  map[string]string{"DATABASE_URL": "postgres://localhost/db{{.Port.wbe}}"},
	}
	f := &state.Feature{Project: "norules", Name: "f", Ports: map[string]int{"web": 3001}}
	if _, err := envFor(m, f, nil, tmpl.Vars{Port: map[string]int{"web": 3001}}); err == nil {
		t.Fatal("a misspelled port name in [env] must be an error, not an empty string")
	}
}

// TestEnvForRejectsAgentReference pins the documented restriction. Agent URLs
// are known only after agents start, and services start before them, so an
// [env] that referenced one would resolve differently depending on which
// phase asked — the kind of inconsistency that shows up as one window
// talking to the wrong server hours later.
func TestEnvForRejectsAgentReference(t *testing.T) {
	m := &manifest.Manifest{
		Name: "norules",
		Env:  map[string]string{"AGENT": "{{.Agent.main}}"},
	}
	f := &state.Feature{Project: "norules", Name: "f"}
	if _, err := envFor(m, f, nil, tmpl.Vars{Agent: map[string]tmpl.AgentRef{}}); err == nil {
		t.Fatal("[env] must not be able to reference an agent URL")
	}
}

// TestDBSuffixForNamespacedFeature covers a suffix that has to survive being
// pasted into a database name. Feature names keep "/" as a namespace
// separator, and only "-" used to be replaced, so "profile/working-hours"
// yielded "norules_test_profile/working_hours".
//
// That did not fail loudly, which is how it survived: MySQL accepts the name
// and percent-encodes the slash on disk (@002f), so the databases really do
// get created and only downstream handling of the name breaks.
func TestDBSuffixForNamespacedFeature(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	m.Database.Isolation = manifest.DBSuffix
	cases := map[string]string{
		"small-fixes":             "_small_fixes",
		"profile/working-hours":   "_profile_working_hours",
		"onboarding/ask-for-name": "_onboarding_ask_for_name",
		"feat/api/v2":             "_feat_api_v2",
		"plain":                   "_plain",
	}
	for in, want := range cases {
		if got := dbSuffixFor(m, in); got != want {
			t.Errorf("dbSuffixFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDBSuffixForIsIdentifierSafe states the actual contract rather than a
// list of examples: whatever the feature is called, the suffix must be
// appendable to an unquoted SQL identifier.
func TestDBSuffixForIsIdentifierSafe(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	m.Database.Isolation = manifest.DBSuffix
	for _, name := range []string{"a/b", "a-b", "a.b", "Ä/ö", "x/y/z"} {
		got := dbSuffixFor(m, name)
		if !strings.HasPrefix(got, "_") {
			t.Errorf("dbSuffixFor(%q) = %q, want a leading underscore", name, got)
		}
		for _, r := range got {
			ok := r == '_' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
			if !ok {
				t.Errorf("dbSuffixFor(%q) = %q contains %q, not identifier-safe", name, got, r)
			}
		}
	}
}

// TestDBSuffixForSharedIsEmpty keeps the shared case decided in one place, so
// no caller has to remember to check the isolation mode itself.
func TestDBSuffixForSharedIsEmpty(t *testing.T) {
	m := &manifest.Manifest{Name: "norules"}
	m.Database.Isolation = manifest.DBShared
	if got := dbSuffixFor(m, "profile/working-hours"); got != "" {
		t.Errorf("dbSuffixFor under shared isolation = %q, want empty", got)
	}
}

// TestEnsureRecordRecomputesDBSuffix is the half of the namespaced-suffix fix
// that correcting dbSuffixFor alone does not deliver.
//
// The suffix is written into the feature's state file when it is created, and
// nothing used to recompute it — so a feature created before the fix kept
// "_profile/working_hours" for the rest of its life no matter how many times
// it was reopened, and the databases it had already created under that name
// stayed in use. Ports were always recomputed from the manifest for exactly
// this reason; the suffix is the same kind of derived value and now follows.
func TestEnsureRecordRecomputesDBSuffix(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// This test runs inside a canaveral worktree, so these are set in the
	// ambient environment and would otherwise leak in.
	t.Setenv("CANAVERAL_ROOT", "")
	t.Setenv("CANAVERAL_FEATURE", "")

	const name = "profile/working-hours"
	m := &manifest.Manifest{Name: "norules", Root: t.TempDir()}
	m.Database.Isolation = manifest.DBSuffix

	// A record as an older canaveral would have written it: slash and all.
	stale := &state.Feature{
		Project: "norules", Name: name, Slot: 3,
		Worktree: "/wt/pwh", Branch: name,
		DBSuffix: "_profile/working_hours",
	}
	if err := state.Save(stale); err != nil {
		t.Fatalf("save: %v", err)
	}

	f, created, err := ensureRecord(context.Background(), m, name)
	if err != nil {
		t.Fatalf("ensureRecord: %v", err)
	}
	if created {
		t.Fatal("existing feature reported as created")
	}
	if want := "_profile_working_hours"; f.DBSuffix != want {
		t.Errorf("DBSuffix = %q, want %q", f.DBSuffix, want)
	}
	// The slot is identity, not a derived value, and must survive untouched.
	if f.Slot != 3 {
		t.Errorf("Slot = %d, want 3 — reopening must not move a feature's ports", f.Slot)
	}
}

// TestEnsureRecordFollowsIsolationChange covers the quieter failure of the
// same bug. norules switched [database] isolation from "shared" to "suffix"
// after its features already existed; without recomputing, each of them kept
// the empty suffix it was created with and went on sharing the one database —
// the exact collision the switch was made to prevent, now invisible.
func TestEnsureRecordFollowsIsolationChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	t.Setenv("CANAVERAL_FEATURE", "")

	const name = "small-fixes"
	shared := &state.Feature{
		Project: "norules", Name: name, Slot: 0,
		Worktree: "/wt/sf", Branch: name, DBSuffix: "",
	}
	if err := state.Save(shared); err != nil {
		t.Fatalf("save: %v", err)
	}

	m := &manifest.Manifest{Name: "norules", Root: t.TempDir()}
	m.Database.Isolation = manifest.DBSuffix
	f, _, err := ensureRecord(context.Background(), m, name)
	if err != nil {
		t.Fatalf("ensureRecord: %v", err)
	}
	if want := "_small_fixes"; f.DBSuffix != want {
		t.Errorf("switching to suffix isolation left DBSuffix = %q, want %q", f.DBSuffix, want)
	}

	// And back again: switching to shared must stop isolating, not strand the
	// feature on a database the manifest no longer describes.
	m.Database.Isolation = manifest.DBShared
	f, _, err = ensureRecord(context.Background(), m, name)
	if err != nil {
		t.Fatalf("ensureRecord: %v", err)
	}
	if f.DBSuffix != "" {
		t.Errorf("switching to shared left DBSuffix = %q, want empty", f.DBSuffix)
	}
}
