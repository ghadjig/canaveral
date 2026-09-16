package wm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeSwaymsg writes a fake `swaymsg` into a temp dir, prepends it to
// PATH and pins backend selection to sway.
//
// It answers a query with $FAKE_SWAY_<TYPE>, and for a command it appends the
// payload to $FAKE_SWAY_LOG and replies with $FAKE_SWAY_REPLY (a success by
// default), exiting $FAKE_SWAY_EXIT. That is the whole contract with swaymsg:
// a raw JSON reply on stdout and an exit status.
func installFakeSwaymsg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "commands.log")
	script := `#!/bin/sh
query=""
while [ $# -gt 0 ]; do
  case "$1" in
    -r|--raw) shift ;;
    -t) query="$2"; shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
if [ -n "$query" ]; then
  case "$query" in
    get_version)    exit "${FAKE_SWAY_VERSION_EXIT:-0}" ;;
    get_tree)       printf '%s' "$FAKE_SWAY_TREE" ;;
    get_outputs)    printf '%s' "$FAKE_SWAY_OUTPUTS" ;;
    get_workspaces) printf '%s' "$FAKE_SWAY_WORKSPACES" ;;
    *)              exit 1 ;;
  esac
  exit "${FAKE_SWAY_QUERY_EXIT:-0}"
fi
printf '%s\n' "$1" >> "$FAKE_SWAY_LOG"
if [ -n "$FAKE_SWAY_REPLY" ]; then
  printf '%s' "$FAKE_SWAY_REPLY"
else
  printf '[{"success":true}]'
fi
exit "${FAKE_SWAY_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(dir, "swaymsg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_SWAY_LOG", log)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	useSway(t)
	return log
}

// useSway pins backend selection, which otherwise reads the environment of
// whatever session the test is running in.
func useSway(t *testing.T) {
	t.Helper()
	t.Setenv(OverrideEnv, "sway")
	t.Setenv("SWAYSOCK", "/run/user/1000/sway-ipc.test.sock")
}

func swayCommands(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("read swaymsg recording: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A tree with the shape swaymsg -t get_tree actually returns: root, output,
// workspace, then windows. One Wayland window carrying an app_id, one
// XWayland window carrying WM_CLASS instead, and one floating.
const swayTestTree = `{
  "id": 1, "type": "root", "name": "root", "nodes": [
    {"id": 2, "type": "output", "name": "DP-1", "rect": {"x":0,"y":0,"width":3840,"height":2160}, "nodes": [
      {"id": 3, "type": "workspace", "name": "norules:small-fixes", "num": -1,
       "layout": "splith", "rect": {"x":0,"y":30,"width":3840,"height":2130},
       "nodes": [
         {"id": 10, "type": "con", "name": "editor", "pid": 111,
          "app_id": "canaveral-norules-small-fixes-editor",
          "rect": {"x":0,"y":30,"width":2000,"height":2130}},
         {"id": 11, "type": "con", "name": "Bambu Studio", "pid": 222,
          "app_id": null, "window_properties": {"class": "BambuStudio", "instance": "bambustudio"},
          "rect": {"x":2000,"y":30,"width":1840,"height":2130}}
       ],
       "floating_nodes": [
         {"id": 12, "type": "floating_con", "name": "popup", "pid": 333,
          "app_id": "some-dialog", "rect": {"x":100,"y":100,"width":400,"height":300}}
       ]}
    ]}
  ]}`

func TestSwayClientsReadsTheTree(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_TREE", swayTestTree)
	got, err := Clients(context.Background())
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d clients, want 3: %+v", len(got), got)
	}
	// Ascending container id, which is sway's creation order — ByClass keeps
	// the first match, so this has to be stable.
	if got[0].Address != "10" || got[1].Address != "11" || got[2].Address != "12" {
		t.Errorf("clients are not in container-id order: %+v", got)
	}

	editor := got[0]
	if editor.Class != "canaveral-norules-small-fixes-editor" || editor.InitialClass != editor.Class {
		t.Errorf("app_id should fill both class fields: %+v", editor)
	}
	if editor.PID != 111 || editor.Title != "editor" || editor.Floating {
		t.Errorf("editor = %+v", editor)
	}
	if editor.Workspace.Name != "norules:small-fixes" || editor.Workspace.ID != -1 {
		t.Errorf("editor workspace = %+v", editor.Workspace)
	}
	if editor.Size != [2]int{2000, 2130} || editor.At != [2]int{0, 30} {
		t.Errorf("editor geometry = at %v size %v", editor.At, editor.Size)
	}

	// An XWayland window has no app_id at all, so match_class has to find it
	// under window_properties instead — this is the BambuStudio case the
	// whole match_class feature exists for.
	if got[1].Class != "BambuStudio" {
		t.Errorf("XWayland class = %q, want BambuStudio", got[1].Class)
	}
	// Floating windows are reported too: an application that opens floating
	// is still the one canaveral is waiting for.
	if !got[2].Floating || got[2].Workspace.Name != "norules:small-fixes" {
		t.Errorf("floating window = %+v", got[2])
	}
}

func TestSwayClientsErrorsOnBadJSON(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_TREE", "not json")
	if _, err := Clients(context.Background()); err == nil {
		t.Error("Clients should fail on unparseable JSON")
	}
}

func TestSwayAvailable(t *testing.T) {
	t.Run("succeeds", func(t *testing.T) {
		installFakeSwaymsg(t)
		if err := Available(context.Background()); err != nil {
			t.Errorf("Available: %v", err)
		}
	})
	t.Run("without SWAYSOCK", func(t *testing.T) {
		installFakeSwaymsg(t)
		t.Setenv("SWAYSOCK", "")
		if err := Available(context.Background()); err == nil {
			t.Error("Available should fail without SWAYSOCK")
		}
	})
	t.Run("without swaymsg on PATH", func(t *testing.T) {
		useSway(t)
		t.Setenv("PATH", t.TempDir())
		if err := Available(context.Background()); err == nil {
			t.Error("Available should fail when swaymsg is not on PATH")
		}
	})
	t.Run("when sway does not answer", func(t *testing.T) {
		installFakeSwaymsg(t)
		t.Setenv("FAKE_SWAY_VERSION_EXIT", "1")
		if err := Available(context.Background()); err == nil {
			t.Error("Available should fail when swaymsg cannot reach sway")
		}
	})
}

func TestSwayWorkspacesAndActive(t *testing.T) {
	installFakeSwaymsg(t)
	// sway derives num from the name: a plain number gets it, anything else
	// gets -1. Every canaveral workspace is named, so every one is -1 — which
	// is what keeps RehomeTarget from dumping stray windows into another
	// feature.
	t.Setenv("FAKE_SWAY_WORKSPACES", `[
	  {"num":1,"name":"1","output":"DP-1","focused":false},
	  {"num":7,"name":"7","output":"DP-2","focused":false},
	  {"num":-1,"name":"norules:small-fixes","output":"DP-1","focused":true}]`)

	ws, err := Workspaces(context.Background())
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	want := []Workspace{
		{ID: 1, Name: "1", Monitor: "DP-1"},
		{ID: 7, Name: "7", Monitor: "DP-2"},
		{ID: -1, Name: "norules:small-fixes", Monitor: "DP-1"},
	}
	for i := range want {
		if ws[i] != want[i] {
			t.Errorf("workspace %d = %+v, want %+v", i, ws[i], want[i])
		}
	}
	if got := RehomeTarget(ws, "DP-1", "norules:small-fixes"); got != 1 {
		t.Errorf("RehomeTarget = %d, want the ordinary workspace 1", got)
	}

	name, err := ActiveWorkspaceName(context.Background())
	if err != nil || name != "norules:small-fixes" {
		t.Errorf("ActiveWorkspaceName = %q, %v", name, err)
	}
	id, err := NextFreeWorkspaceID(context.Background())
	if err != nil || id != 8 {
		t.Errorf("NextFreeWorkspaceID = %d, %v; want one past the highest", id, err)
	}
}

func TestSwayActiveWorkspaceNameErrorsWithNoneFocused(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_WORKSPACES", `[{"num":1,"name":"1","output":"DP-1"}]`)
	if _, err := ActiveWorkspaceName(context.Background()); err == nil {
		t.Error("ActiveWorkspaceName should fail when sway reports nothing focused")
	}
}

func TestSwayMonitorsSkipInactiveOutputs(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_OUTPUTS", `[
	  {"name":"DP-1","active":true,"focused":true,"current_workspace":"norules:f"},
	  {"name":"HDMI-A-1","active":false,"focused":false,"current_workspace":null}]`)
	got, err := Monitors(context.Background())
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	// A disconnected output has no workspace and cannot be focused, so it is
	// nowhere a feature could be put.
	if len(got) != 1 || got[0].Name != "DP-1" || !got[0].Focused {
		t.Fatalf("got = %+v, want only the active output", got)
	}
	if got[0].ActiveWorkspace.Name != "norules:f" {
		t.Errorf("ActiveWorkspace = %q", got[0].ActiveWorkspace.Name)
	}
	m, err := ActiveMonitor(context.Background())
	if err != nil || m.Name != "DP-1" {
		t.Errorf("ActiveMonitor = %+v, %v", m, err)
	}
}

// The placement rules are what make sway work at all: there is no exec-time
// rule to attach to the process, so the class has to carry it. Both the
// Wayland and the XWayland criterion must be installed, and the window must
// not steal focus when it maps.
func TestSwaySpawnInstallsPlacementRulesBeforeExec(t *testing.T) {
	log := installFakeSwaymsg(t)
	err := Spawn(context.Background(), SpawnSpec{
		Class: "canaveral-p-f-chrome", Workspace: "p:f",
		Cmd: "google-chrome --new-window http://localhost:3000",
		Env: map[string]string{"A": "1"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	calls := swayCommands(t, log)
	if len(calls) != 2 {
		t.Fatalf("want a rules request then an exec request, got %d: %v", len(calls), calls)
	}
	for _, want := range []string{
		`for_window [app_id="^canaveral-p-f-chrome$"] move container to workspace "p:f"`,
		`for_window [class="^canaveral-p-f-chrome$"] move container to workspace "p:f"`,
		`no_focus [app_id="^canaveral-p-f-chrome$"]`,
		`no_focus [class="^canaveral-p-f-chrome$"]`,
	} {
		if !strings.Contains(calls[0], want) {
			t.Errorf("rules request missing %q:\n%s", want, calls[0])
		}
	}
	// Nothing but `exec sh <path>` may cross into sway: the command itself
	// carries semicolons and commas sway's parser would split on.
	if !strings.HasPrefix(calls[1], "exec sh /") || strings.ContainsAny(calls[1], ";,'\"") {
		t.Errorf("exec request = %q, want a bare `exec sh <path>`", calls[1])
	}
	if strings.Contains(calls[1], "google-chrome") {
		t.Errorf("the command must stay in the script, not the request: %q", calls[1])
	}
}

func TestSwaySpawnCleansUpWhenSwayRefuses(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_REPLY", `[{"success":false,"error":"Unknown/invalid command"}]`)
	t.Setenv("FAKE_SWAY_EXIT", "2")
	err := Spawn(context.Background(), SpawnSpec{
		Class: "c", Workspace: "p:f", Cmd: "true", Env: map[string]string{"A": "secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "Unknown/invalid command") {
		t.Errorf("err = %v, want sway's own complaint", err)
	}
	// The script holds the whole environment, so a spawn that never happened
	// must not leave it lying in the runtime directory.
	entries, err := os.ReadDir(runtimeDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed spawn left spawn files: %v", entries)
	}
}

func TestSwayCommandsUseTheRightSelectors(t *testing.T) {
	cases := []struct {
		name string
		run  func(context.Context) error
		want string
	}{
		{"focus", func(ctx context.Context) error { return Focus(ctx, "norules:f") },
			`workspace --no-auto-back-and-forth "norules:f"`},
		// Unlike Hyprland, sway needs no special case for a numeric name:
		// `workspace "4"` is workspace 4, not a new workspace called "4".
		{"focus a numbered workspace", func(ctx context.Context) error { return Focus(ctx, "4") },
			`workspace --no-auto-back-and-forth "4"`},
		{"close", func(ctx context.Context) error { return Close(ctx, "12") },
			`[con_id=12] kill`},
		{"move to a named workspace", func(ctx context.Context) error { return MoveWindowToNamedWorkspace(ctx, "12", "p:f") },
			`[con_id=12] move container to workspace "p:f"`},
		{"move to a numbered workspace", func(ctx context.Context) error { return MoveWindowToWorkspace(ctx, "12", 3) },
			`[con_id=12] move container to workspace number 3`},
		{"rules", func(ctx context.Context) error { return EnsureRules(ctx) },
			`for_window [app_id="^canaveral-.*$"] floating disable`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := installFakeSwaymsg(t)
			if err := c.run(context.Background()); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if got := swayCommands(t, log); !strings.Contains(got[0], c.want) {
				t.Errorf("sent %q, want it to contain %q", got[0], c.want)
			}
		})
	}
}

// A window closed between the snapshot and the command is ordinary; what
// matters is that the caller is told, with sway's own words, rather than just
// an exit status.
func TestSwayRunReportsWhichCommandFailed(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_REPLY", `[{"success":false,"error":"No matching node."}]`)
	t.Setenv("FAKE_SWAY_EXIT", "2")
	err := Close(context.Background(), "12")
	if err == nil {
		t.Fatal("Close should report a refused command")
	}
	if !strings.Contains(err.Error(), "No matching node.") || !strings.Contains(err.Error(), "con_id=12") {
		t.Errorf("err = %v, want it to name the command and sway's reason", err)
	}
}

// Everything canaveral generates is already safe — see manifest.NameRe — so
// this is about a hand-edited state file. A workspace name carrying a `;`
// would otherwise end the command and start a new one of the user's choosing.
func TestSwayRejectsNamesItCannotQuote(t *testing.T) {
	log := installFakeSwaymsg(t)
	for _, bad := range []string{"p:f; exec rm -rf ~", `p:f" exec danger`, "p:f, exec danger", "two words", ""} {
		if err := MoveWindowToNamedWorkspace(context.Background(), "12", bad); err == nil {
			t.Errorf("workspace %q should be refused", bad)
		}
		if err := Focus(context.Background(), bad); err == nil {
			t.Errorf("focusing workspace %q should be refused", bad)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("a rejected name must not reach sway at all")
	}
}

func TestSwayMoveWorkspaceToMonitorRestoresTheView(t *testing.T) {
	t.Run("a workspace the user is not looking at", func(t *testing.T) {
		log := installFakeSwaymsg(t)
		t.Setenv("FAKE_SWAY_WORKSPACES", `[{"num":2,"name":"2","output":"DP-1","focused":true}]`)
		if err := MoveWorkspaceToMonitor(context.Background(), "norules:f", "DP-2"); err != nil {
			t.Fatalf("MoveWorkspaceToMonitor: %v", err)
		}
		// sway's `move workspace to output` only ever acts on the focused
		// workspace, so the target has to be focused and the user's view put
		// back — in one request, so sway never renders the detour.
		want := `workspace --no-auto-back-and-forth "norules:f"; move workspace to output "DP-2"; workspace --no-auto-back-and-forth "2"`
		if got := swayCommands(t, log); got[0] != want {
			t.Errorf("sent %q,\nwant %q", got[0], want)
		}
	})
	t.Run("the workspace already focused", func(t *testing.T) {
		log := installFakeSwaymsg(t)
		t.Setenv("FAKE_SWAY_WORKSPACES", `[{"num":-1,"name":"norules:f","output":"DP-1","focused":true}]`)
		if err := MoveWorkspaceToMonitor(context.Background(), "norules:f", "DP-2"); err != nil {
			t.Fatalf("MoveWorkspaceToMonitor: %v", err)
		}
		// Nothing to save or restore, so nothing is switched away from and
		// back — which would be a visible flicker for no reason.
		if got := swayCommands(t, log); got[0] != `move workspace to output "DP-2"` {
			t.Errorf("sent %q, want just the move", got[0])
		}
	})
}

// sway reaps an empty workspace only once nothing is displaying it, exactly
// as Hyprland does, so a feature's last window closing is not enough on its
// own — the output has to be sent somewhere else.
func TestSwayReleaseWorkspaceMovesTheOutputOn(t *testing.T) {
	log := installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_OUTPUTS", `[
	  {"name":"DP-1","active":true,"focused":false,"current_workspace":"norules:dying"},
	  {"name":"DP-2","active":true,"focused":true,"current_workspace":"3"}]`)
	t.Setenv("FAKE_SWAY_WORKSPACES", `[{"num":3,"name":"3","output":"DP-2","focused":true}]`)
	if err := ReleaseWorkspace(context.Background(), "norules:dying"); err != nil {
		t.Fatalf("ReleaseWorkspace: %v", err)
	}
	got := swayCommands(t, log)
	want := `focus output "DP-1"; workspace --no-auto-back-and-forth number 4`
	if len(got) != 1 || got[0] != want {
		t.Errorf("sent %v,\nwant [%q]", got, want)
	}
}

func TestSwayReleaseWorkspaceIsANoopWhenNothingShowsIt(t *testing.T) {
	log := installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_OUTPUTS", `[{"name":"DP-1","active":true,"focused":true,"current_workspace":"3"}]`)
	// Leaving FAKE_SWAY_WORKSPACES unset means this fails loudly if it tries.
	if err := ReleaseWorkspace(context.Background(), "norules:dying"); err != nil {
		t.Errorf("ReleaseWorkspace: %v", err)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("nothing was showing the workspace; nothing should have been sent")
	}
}
