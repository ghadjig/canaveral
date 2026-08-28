package hypr

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClassIsSanitised(t *testing.T) {
	cases := []struct{ project, feature, window, want string }{
		{"norules", "small-fixes", "chrome", "canaveral-norules-small-fixes-chrome"},
		{"nor ules", "add/tasks", "server logs", "canaveral-nor-ules-add-tasks-server-logs"},
	}
	for _, c := range cases {
		if got := Class(c.project, c.feature, c.window); got != c.want {
			t.Errorf("Class(%q,%q,%q) = %q, want %q", c.project, c.feature, c.window, got, c.want)
		}
	}
}

func TestClassPrefixGroupsOnlyOwnFeature(t *testing.T) {
	// The group rule is scoped to one feature so two features open at once are
	// never tabbed into each other.
	p := ClassPrefix("norules", "small-fixes")
	if !strings.HasPrefix(Class("norules", "small-fixes", "terminal"), p) {
		t.Error("class must carry the feature prefix")
	}
	if strings.HasPrefix(Class("norules", "other-feature", "terminal"), p) {
		t.Error("a different feature must not share the prefix")
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
	})
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
	got, err := buildArgv(SpawnSpec{Class: "c", Dir: "/wt", IsTerminal: true, Cmd: ""})
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if strings.Contains(got, "-e") {
		t.Errorf("plain shell should not pass -e: %s", got)
	}
}

func TestBuildArgvHoldKeepsPaneOpen(t *testing.T) {
	got, err := buildArgv(SpawnSpec{Class: "c", IsTerminal: true, Cmd: "false", Hold: true})
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
	})
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
	if _, err := buildArgv(SpawnSpec{Class: "c"}); err == nil {
		t.Error("empty exec command should error")
	}
}

func TestBuildArgvRequiresClass(t *testing.T) {
	if _, err := buildArgv(SpawnSpec{IsTerminal: true}); err == nil {
		t.Error("missing class should error")
	}
}

func TestBuildArgvEnvIsDeterministic(t *testing.T) {
	spec := SpawnSpec{
		Class: "c", IsTerminal: true, Cmd: "true",
		Env: map[string]string{"B": "2", "A": "1", "C": "3"},
	}
	first, err := buildArgv(spec)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := buildArgv(spec)
		if err != nil {
			t.Fatal(err)
		}
		if again != first {
			t.Fatalf("argv is not deterministic:\n%s\n%s", first, again)
		}
	}
	if strings.Index(first, "A=1") > strings.Index(first, "B=2") {
		t.Errorf("env should be sorted: %s", first)
	}
}

func TestShellQuoteEscapesQuotes(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

func TestMonitorAt(t *testing.T) {
	monitors := []Monitor{
		{Name: "eDP-1", X: 6560, Y: 0, Width: 1920, Height: 1200},
		{Name: "DP-3", X: 8480, Y: 0, Width: 5120, Height: 1440},
	}
	cases := []struct {
		x, y int
		want string
		ok   bool
	}{
		{7000, 500, "eDP-1", true},
		{9000, 500, "DP-3", true},
		{8480, 0, "DP-3", true},   // top-left corner is inclusive
		{13600, 0, "DP-3", false}, // exactly at the right edge is exclusive
		{0, 0, "", false},         // nowhere near either monitor
	}
	for _, c := range cases {
		m, ok := MonitorAt(monitors, c.x, c.y)
		if ok != c.ok {
			t.Errorf("MonitorAt(%d,%d) ok=%v, want %v", c.x, c.y, ok, c.ok)
			continue
		}
		if ok && m.Name != c.want {
			t.Errorf("MonitorAt(%d,%d) = %q, want %q", c.x, c.y, m.Name, c.want)
		}
	}
}

func TestUsableAreaSubtractsReserved(t *testing.T) {
	m := Monitor{X: 8480, Y: 0, Width: 5120, Height: 1440, Reserved: [4]int{0, 38, 0, 0}}
	x, y, w, h := m.UsableArea()
	if x != 8480 || y != 38 || w != 5120 || h != 1402 {
		t.Errorf("UsableArea() = (%d,%d,%d,%d), want (8480,38,5120,1402)", x, y, w, h)
	}
}
