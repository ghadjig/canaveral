package space

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/hypr"
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

	got, skipped := Capture(m, "notes", clients, argv)
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
	got, _ := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {"kitty"}}))
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
	clients := []hypr.Client{client("a", "kitty", "x", 1), client("b", "Slicer", "x", 2)}
	ws, _ := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {"/apps/My Slicer.AppImage"}}))
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
	if len(m.Windows) != 2 || m.Windows[1].Exec != "'/apps/My Slicer.AppImage'" {
		t.Errorf("windows = %+v", m.Windows)
	}

	// A second save of the same windows adds nothing: they are declared now.
	again, _ := Capture(m, "x", clients, fakeArgv(map[int][]string{1: {"kitty"}, 2: {"x"}}))
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
	ws, _ := Capture(m, "x", []hypr.Client{client("b", "b", "x", 1)}, fakeArgv(map[int][]string{1: {"b"}}))
	if _, err := Append("x", ws); !errors.Is(err, ErrLayout) {
		t.Errorf("err = %v, want ErrLayout", err)
	}
	data, _ := os.ReadFile(dir + "/x.toml")
	if string(data) != body {
		t.Error("the definition was changed despite the refusal")
	}
}
