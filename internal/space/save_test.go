package space

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
)

func client(addr, class, ws string, pid int) hypr.Client {
	c := hypr.Client{Address: addr, Class: class, InitialClass: class, PID: pid}
	c.Workspace.Name = ws
	return c
}

func fakeArgv(m map[int][]string) func(int) ([]string, bool) {
	return func(pid int) ([]string, bool) {
		a, ok := m[pid]
		return a, ok
	}
}

func TestCreateEmptyLoadsWithNoWindows(t *testing.T) {
	isolate(t)
	if _, err := CreateEmpty("scratchpad"); err != nil {
		t.Fatal(err)
	}
	// It has to open as it is: `new` from the launcher opens it straight
	// away, before anything has been saved into it.
	m, err := Load("scratchpad")
	if err != nil {
		t.Fatalf("the empty definition does not load: %v", err)
	}
	if len(m.Windows) != 0 {
		t.Errorf("windows = %+v, want none", m.Windows)
	}
}

func TestCaptureSkipsDeclaredWindowsAndOtherWorkspaces(t *testing.T) {
	isolate(t)
	if _, err := Create("notes"); err != nil { // declares browser (match_class chrome) and terminal
		t.Fatal(err)
	}
	m, err := Load("notes")
	if err != nil {
		t.Fatal(err)
	}
	clients := []hypr.Client{
		client("0x1", "google-chrome", "notes", 10),                 // the declared browser
		client("0x2", hypr.Class("notes", "terminal"), "notes", 11), // the declared terminal
		client("0x3", "google-chrome", "notes", 12),                 // a second browser: new
		client("0x4", "org.gnome.Nautilus", "notes", 13),            // new
		client("0x5", "kitty", "elsewhere", 14),                     // not on this workspace
		client("0x6", "gone", "notes", 15),                          // process unreadable
	}
	argv := fakeArgv(map[int][]string{
		12: {"/opt/google/chrome/chrome", "--profile-directory=Profile 1"},
		13: {"nautilus", "--new-window"},
		14: {"kitty"},
	})

	c := Capture(m, "notes", clients, argv)
	got, skipped := c.Windows, c.Skipped
	if len(got) != 2 {
		t.Fatalf("captured %+v, want the second browser and nautilus", got)
	}
	if got[0].Name != "google-chrome" || got[1].Name != "nautilus" {
		t.Errorf("names = %q, %q", got[0].Name, got[1].Name)
	}
	if want := "/opt/google/chrome/chrome '--profile-directory=Profile 1'"; got[0].Exec != want {
		t.Errorf("exec = %q, want %q", got[0].Exec, want)
	}
	if got[1].MatchClass != `(?i)^org\.gnome\.Nautilus$` {
		t.Errorf("match_class = %q", got[1].MatchClass)
	}
	if len(skipped) != 1 {
		t.Errorf("skipped = %v, want the unreadable window", skipped)
	}
}

func TestCaptureDeduplicatesNames(t *testing.T) {
	isolate(t)
	if _, err := CreateEmpty("x"); err != nil {
		t.Fatal(err)
	}
	m, _ := Load("x")
	clients := []hypr.Client{client("a", "kitty", "x", 1), client("b", "kitty", "x", 2)}
	got := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {"kitty"}})).Windows
	if len(got) != 2 || got[0].Name != "kitty" || got[1].Name != "kitty-2" {
		t.Errorf("got %+v", got)
	}
}

func TestAppendKeepsCommentsAndLoads(t *testing.T) {
	isolate(t)
	if _, err := CreateEmpty("x"); err != nil {
		t.Fatal(err)
	}
	m, _ := Load("x")
	// A real file whose path has a space in it: one argument, and it must
	// stay one.
	slicer := filepath.Join(t.TempDir(), "My Slicer.AppImage")
	if err := os.WriteFile(slicer, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	clients := []hypr.Client{client("a", "kitty", "x", 1), client("b", "Slicer", "x", 2)}
	ws := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {slicer}})).Windows
	path, err := Append("x", ws)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "canaveral save x") {
		t.Error("the comments already in the file were not kept")
	}
	m, err = Load("x")
	if err != nil {
		t.Fatalf("saved definition does not load: %v\n%s", err, data)
	}
	if len(m.Windows) != 2 || m.Windows[1].Exec != "'"+slicer+"'" {
		t.Errorf("windows = %+v", m.Windows)
	}

	// A second save of the same windows adds nothing: they are declared now.
	again := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {"x"}})).Windows
	if len(again) != 0 {
		t.Errorf("re-captured already saved windows: %+v", again)
	}
}

func TestAppendRefusesALayout(t *testing.T) {
	dir := isolate(t)
	body := `[[window]]
name = "a"
exec = "a"
match_class = "^a$"

[layout]
order = ["a"]
[layout.default]
a = 1.0
`
	if err := os.WriteFile(dir+"/x.toml", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Load("x")
	if err != nil {
		t.Fatal(err)
	}
	ws := Capture(m, "x", []hypr.Client{client("b", "b", "x", 1)}, fakeArgv(map[int][]string{1: {"b"}})).Windows
	if _, err := Append("x", ws); !errors.Is(err, ErrLayout) {
		t.Errorf("err = %v, want ErrLayout", err)
	}
	data, _ := os.ReadFile(dir + "/x.toml")
	if string(data) != body {
		t.Error("the definition was changed despite the refusal")
	}
}

// Chrome overwrites its argv to set a process title, so /proc hands back the
// whole command as one argument with spaces in it. Quoting that back as one
// word saved a command naming a program that does not exist — what happened
// to the knx and dec2026 spaces.
func TestCaptureSplitsARewrittenCommandLine(t *testing.T) {
	isolate(t)
	argv := fakeArgv(map[int][]string{1: {"/opt/google/chrome/chrome --new-window http://localhost:3053/terms"}})
	got := Capture(&manifest.Manifest{}, "knx", []hypr.Client{client("a", "google-chrome", "knx", 1)}, argv).Windows
	if want := "/opt/google/chrome/chrome --new-window http://localhost:3053/terms"; len(got) != 1 || got[0].Exec != want {
		t.Errorf("got %+v, want exec %q", got, want)
	}
}

// One Chrome process draws every Chrome window, so its command line says
// nothing about what any one of them shows. That has to be said out loud.
func TestCaptureFlagsWindowsWhoseProcessIsShared(t *testing.T) {
	isolate(t)
	clients := []hypr.Client{
		client("a", "google-chrome", "knx", 1),
		client("b", "google-chrome", "elsewhere", 1),
		client("c", "kitty", "knx", 2),
	}
	got := Capture(&manifest.Manifest{}, "knx", clients, fakeArgv(map[int][]string{1: {"chrome"}, 2: {"kitty"}}))
	if len(got.Shared) != 1 || got.Shared[0] != "google-chrome" {
		t.Errorf("Shared = %v, want only the browser", got.Shared)
	}
}

func TestReplaceSwapsWindowsAndKeepsTheRest(t *testing.T) {
	dir := isolate(t)
	body := `# my space
dir = "~/knx"

[ports]
web = 3053

[[window]]
name = "old"
exec = "'chrome --broken'"
match_class = "^chrome$"
profile_seed = [
  "Default/Bookmarks",

  "Local State",
]

# a note about services, kept
[[service]]
name = "web"
cmd = "true"

` + savedMarker + `
[[window]]
name = "older"
exec = "x"
match_class = "^x$"
`
	if err := os.WriteFile(dir+"/knx.toml", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := []manifest.Window{{Name: "google-chrome", Exec: "chrome --new-window", MatchClass: "(?i)^google-chrome$"}}
	if _, err := Replace("knx", ws); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	m, err := Load("knx")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Windows) != 1 || m.Windows[0].Name != "google-chrome" {
		t.Errorf("windows = %+v, want only the replacement", m.Windows)
	}
	if len(m.Services) != 1 || m.Ports["web"] != 3053 {
		t.Errorf("services/ports lost: %+v %+v", m.Services, m.Ports)
	}
	data, _ := os.ReadFile(dir + "/knx.toml")
	for _, keep := range []string{"# my space", "# a note about services, kept"} {
		if !strings.Contains(string(data), keep) {
			t.Errorf("lost %q:\n%s", keep, data)
		}
	}
	if strings.Count(string(data), savedMarker) != 1 {
		t.Errorf("save markers pile up:\n%s", data)
	}
}
