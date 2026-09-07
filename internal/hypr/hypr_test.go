package hypr

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestClassIsSanitised(t *testing.T) {
	cases := []struct{ scope, window, want string }{
		// A feature's scope is "project/feature", and the slash sanitises to
		// the same hyphen the two halves used to be joined by — so classes
		// are spelled exactly as they were before scope became one string,
		// and a window open across the change is still recognised.
		{"norules/small-fixes", "chrome", "canaveral-norules-small-fixes-chrome"},
		{"nor ules/add/tasks", "server logs", "canaveral-nor-ules-add-tasks-server-logs"},
		// A space has no project, so its scope is the bare name.
		{"3d-printing", "onshape", "canaveral-3d-printing-onshape"},
	}
	for _, c := range cases {
		if got := Class(c.scope, c.window); got != c.want {
			t.Errorf("Class(%q,%q) = %q, want %q", c.scope, c.window, got, c.want)
		}
	}
}

func TestClassPrefixGroupsOnlyOwnFeature(t *testing.T) {
	// The group rule is scoped to one feature so two features open at once are
	// never tabbed into each other.
	p := ClassPrefix("norules/small-fixes")
	if !strings.HasPrefix(Class("norules/small-fixes", "terminal"), p) {
		t.Error("class must carry the feature prefix")
	}
	if strings.HasPrefix(Class("norules/other-feature", "terminal"), p) {
		t.Error("a different feature must not share the prefix")
	}
	// A space must not be swept up by a project whose name it matches.
	if strings.HasPrefix(Class("norules", "terminal"), p) {
		t.Error("a space must not share a feature's prefix")
	}
}

func TestByClassKeepsFirstMatch(t *testing.T) {
	cs := []Client{
		{Address: "0x1", InitialClass: "canaveral-a", Title: "first"},
		{Address: "0x2", InitialClass: "canaveral-a", Title: "duplicate"},
		{Address: "0x3", InitialClass: "canaveral-b"},
	}
	got := ByClass(cs)
	if got["canaveral-a"].Address != "0x1" {
		t.Errorf("duplicate should not shadow the original: %+v", got["canaveral-a"])
	}
	if len(got) != 2 {
		t.Errorf("len = %d, want 2", len(got))
	}
}

// The whole point of match_class is that the class alone is ambiguous: the
// user's own copy of the same application carries it too. The workspace is
// what disambiguates, since no two features share one.
func TestMatchInWorkspaceIgnoresTheSameClassElsewhere(t *testing.T) {
	re := regexp.MustCompile("^BambuStudio$")
	cs := []Client{
		{Address: "0xmine", Class: "BambuStudio"},
		{Address: "0xtheirs", Class: "BambuStudio"},
	}
	cs[0].Workspace.Name = "other"
	cs[1].Workspace.Name = "3d"

	got, ok := MatchInWorkspace(cs, re, "3d")
	if !ok {
		t.Fatal("a matching window on the feature's workspace should be found")
	}
	if got.Address != "0xtheirs" {
		t.Errorf("adopted %s, want the window on the feature's own workspace", got.Address)
	}
	if _, ok := MatchInWorkspace(cs, re, "empty"); ok {
		t.Error("no window is on that workspace; nothing should be adopted")
	}
}

func TestClassMatchesEitherClass(t *testing.T) {
	re := regexp.MustCompile("^Google-chrome$")
	if !ClassMatches(re, Client{Class: "Google-chrome", InitialClass: ""}) {
		t.Error("the current class should match")
	}
	if !ClassMatches(re, Client{Class: "", InitialClass: "Google-chrome"}) {
		t.Error("the initial class should match")
	}
	if ClassMatches(re, Client{Class: "chromium", InitialClass: "chromium"}) {
		t.Error("an unrelated class must not match")
	}
}

func TestParseRealHyprctlClients(t *testing.T) {
	// Shape captured from hyprctl clients -j on Hyprland 0.50.1.
	raw := `[{"address":"0x55d1","class":"Alacritty","initialClass":"canaveral-p-f-terminal",
	  "title":"t","initialTitle":"f · terminal","pid":12345,
	  "workspace":{"id":5,"name":"norules:small-fixes"},"grouped":["0x55d1","0x55d2"]}]`
	var cs []Client
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := cs[0]
	if c.InitialClass != "canaveral-p-f-terminal" {
		t.Errorf("InitialClass = %q", c.InitialClass)
	}
	if c.Workspace.Name != "norules:small-fixes" {
		t.Errorf("Workspace.Name = %q", c.Workspace.Name)
	}
	if len(c.Grouped) != 2 {
		t.Errorf("Grouped = %v", c.Grouped)
	}
}

func TestBuildArgvTerminal(t *testing.T) {
	got, err := buildArgv(SpawnSpec{
		Class: "canaveral-p-f-logs", Title: "f · logs",
		Dir: "/wt/f", IsTerminal: true, Cmd: "canaveral logs f web -f",
	}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	for _, want := range []string{
		"'alacritty'",
		"'canaveral-p-f-logs,canaveral-p-f-logs'",
		"'--working-directory' '/wt/f'",
		"'canaveral logs f web -f'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
}

func TestBuildArgvPlainShell(t *testing.T) {
	// run = "" means a bare shell: no -e flag at all.
	got, err := buildArgv(SpawnSpec{Class: "c", Dir: "/wt", IsTerminal: true, Cmd: ""}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if strings.Contains(got, "-e") {
		t.Errorf("plain shell should not pass -e: %s", got)
	}
}

func TestBuildArgvHoldKeepsPaneOpen(t *testing.T) {
	got, err := buildArgv(SpawnSpec{Class: "c", IsTerminal: true, Cmd: "false", Hold: true}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if !strings.Contains(got, "read _") {
		t.Errorf("hold should keep the pane open: %s", got)
	}
}

func TestBuildArgvExecGUI(t *testing.T) {
	got, err := buildArgv(SpawnSpec{
		Class: "c", Dir: "/wt", Cmd: "google-chrome --new-window http://localhost:3001",
	}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if strings.Contains(got, "alacritty") {
		t.Errorf("exec window must not be wrapped in a terminal: %s", got)
	}
	if !strings.Contains(got, "google-chrome") {
		t.Errorf("argv missing command: %s", got)
	}
}

func TestBuildArgvRejectsEmptyExec(t *testing.T) {
	if _, err := buildArgv(SpawnSpec{Class: "c"}, ""); err == nil {
		t.Error("empty exec command should error")
	}
}

func TestBuildArgvRequiresClass(t *testing.T) {
	if _, err := buildArgv(SpawnSpec{IsTerminal: true}, ""); err == nil {
		t.Error("missing class should error")
	}
}

// The leak this guards against: /proc/<pid>/cmdline is mode 444, so anything
// on a spawned window's command line is readable by every process on the
// machine for as long as that window lives. An `env K=V` prefix put API keys
// and database credentials there.
func TestBuildArgvKeepsEnvValuesOutOfTheCommandLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	secret := "sk-ant-not-a-real-key"
	env := map[string]string{"ANTHROPIC_API_KEY": secret, "CANAVERAL_ROOT": "/r"}

	path, err := writeEnvFile(env)
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	for _, spec := range []SpawnSpec{
		{Class: "c", IsTerminal: true, Cmd: "true", Dir: "/wt", Env: env},
		{Class: "c", Cmd: "google-chrome --new-window http://x", Dir: "/wt", Env: env},
	} {
		got, err := buildArgv(spec, path)
		if err != nil {
			t.Fatalf("buildArgv: %v", err)
		}
		if strings.Contains(got, secret) {
			t.Errorf("secret leaked into argv:\n%s", got)
		}
		if strings.Contains(got, "ANTHROPIC_API_KEY=") {
			t.Errorf("env assignment leaked into argv:\n%s", got)
		}
		if !strings.Contains(got, path) {
			t.Errorf("argv should reference the env file:\n%s", got)
		}
	}
}

func TestEnvFileIsPrivateSortedAndSelfDeleting(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := writeEnvFile(map[string]string{"B": "2", "A": "1", "C": "it's"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Owner-only: the whole point is that this is less readable than the
	// command line it replaces.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file mode = %o, want 600", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "A='1'\nB='2'\nC='it'\\''s'\n"
	if string(b) != want {
		t.Errorf("env file =\n%q\nwant\n%q", b, want)
	}
	// The spawned shell, not canaveral, is what removes it — canaveral has
	// returned long before the shell runs.
	if pre := envPrefix(path); !strings.Contains(pre, "rm -f") || !strings.Contains(pre, "set -a") {
		t.Errorf("env prefix should export and then delete: %s", pre)
	}
	out, err := exec.Command("/bin/sh", "-c", envPrefix(path)+`exec /bin/sh -c 'printf "%s\n" "$A" "$B" "$C"'`).CombinedOutput()
	if err != nil {
		t.Fatalf("source env file: %v: %s", err, out)
	}
	if string(out) != "1\n2\nit's\n" {
		t.Errorf("exported environment = %q", out)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("env file should be removed, stat error = %v", err)
	}
}

func TestWriteEnvFileSkipsAnEmptyEnvironment(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := writeEnvFile(nil)
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
	if envPrefix("") != "" {
		t.Errorf("empty path should produce no prefix")
	}
}

func TestShellQuoteEscapesQuotes(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestUsableAreaSubtractsReserved(t *testing.T) {
	m := Monitor{X: 8480, Y: 0, Width: 5120, Height: 1440, Reserved: [4]int{0, 38, 0, 0}}
	x, y, w, h := m.UsableArea()
	if x != 8480 || y != 38 || w != 5120 || h != 1402 {
		t.Errorf("UsableArea() = (%d,%d,%d,%d), want (8480,38,5120,1402)", x, y, w, h)
	}
}

func TestRehomeTargetPrefersLowestOrdinaryOnSameMonitor(t *testing.T) {
	ws := []Workspace{
		{ID: 5, Name: "5", Monitor: "eDP-1"},
		{ID: 2, Name: "2", Monitor: "eDP-1"},
		{ID: 1, Name: "1", Monitor: "DP-3"},              // other monitor
		{ID: -1337, Name: "norules:x", Monitor: "eDP-1"}, // another feature
		{ID: 9, Name: "norules:dying", Monitor: "eDP-1"}, // the one being torn down
	}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 2 {
		t.Errorf("RehomeTarget = %d, want 2 (lowest ordinary on that monitor)", got)
	}
}

func TestRehomeTargetSkipsNamedWorkspaces(t *testing.T) {
	// A named workspace belongs to another feature; dumping stray windows
	// into it would just move the mess somewhere it does not belong.
	ws := []Workspace{
		{ID: -100, Name: "norules:other", Monitor: "eDP-1"},
		{ID: 7, Name: "7", Monitor: "eDP-1"},
	}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 7 {
		t.Errorf("RehomeTarget = %d, want 7", got)
	}
}

func TestRehomeTargetNoneAvailable(t *testing.T) {
	ws := []Workspace{{ID: 3, Name: "3", Monitor: "DP-3"}}
	if got := RehomeTarget(ws, "eDP-1", "norules:dying"); got != 0 {
		t.Errorf("RehomeTarget = %d, want 0 so the caller picks a fresh one", got)
	}
}

func TestCmdlineReadsTheRunningProcess(t *testing.T) {
	got, ok := Cmdline(os.Getpid())
	if !ok {
		t.Fatal("Cmdline(self) = _, false; want the test binary's own command line")
	}
	if !strings.Contains(got, os.Args[0]) {
		t.Errorf("Cmdline(self) = %q, want it to contain %q", got, os.Args[0])
	}
}

// A pid that does not exist, and the pid 0 that hypr.Client carries when the
// compositor did not report one, must both be "cannot tell" rather than an
// empty command line a caller could mistake for an answer.
func TestCmdlineReportsWhenItCannotTell(t *testing.T) {
	for _, pid := range []int{0, -1, 1 << 30} {
		if got, ok := Cmdline(pid); ok {
			t.Errorf("Cmdline(%d) = %q, true; want _, false", pid, got)
		}
	}
}

func TestWorkspaceArgKeepsNumericAndNamedApart(t *testing.T) {
	cases := []struct{ name, want string }{
		{"4", "4"},
		{"0", "0"},
		{"-3", "-3"},
		{"norules:small-fixes", "name:norules:small-fixes"},
		{"3d", "name:3d"}, // starts with a digit but is not one
		{"3d-printing", "name:3d-printing"},
		{"", "name:"},
	}
	for _, c := range cases {
		if got := workspaceArg(c.name); got != c.want {
			t.Errorf("workspaceArg(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
