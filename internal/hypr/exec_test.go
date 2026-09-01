package hypr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// installFakeHyprctl writes a fake `hyprctl` script into a temp dir and
// prepends it to PATH, so exec.LookPath("hyprctl") resolves to it instead of
// any real compositor. Its behavior for each subcommand is controlled by the
// FAKE_HYPRCTL_* environment variables it reads — set whichever ones a given
// test needs via t.Setenv; unset ones default to a quiet success.
func installFakeHyprctl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  version)
    exit "${FAKE_HYPRCTL_VERSION_EXIT:-0}"
    ;;
  clients)
    printf '%s' "$FAKE_HYPRCTL_CLIENTS_JSON"
    exit "${FAKE_HYPRCTL_CLIENTS_EXIT:-0}"
    ;;
  monitors)
    printf '%s' "$FAKE_HYPRCTL_MONITORS_JSON"
    exit "${FAKE_HYPRCTL_MONITORS_EXIT:-0}"
    ;;
  workspaces)
    printf '%s' "$FAKE_HYPRCTL_WORKSPACES_JSON"
    exit "${FAKE_HYPRCTL_WORKSPACES_EXIT:-0}"
    ;;
  activeworkspace)
    printf '%s' "$FAKE_HYPRCTL_ACTIVEWORKSPACE_JSON"
    exit "${FAKE_HYPRCTL_ACTIVEWORKSPACE_EXIT:-0}"
    ;;
  dispatch)
    printf '%s' "$FAKE_HYPRCTL_DISPATCH_STDOUT"
    printf '%s' "$FAKE_HYPRCTL_DISPATCH_STDERR" >&2
    exit "${FAKE_HYPRCTL_DISPATCH_EXIT:-0}"
    ;;
  keyword)
    exit "${FAKE_HYPRCTL_KEYWORD_EXIT:-0}"
    ;;
  *)
    exit 1
    ;;
esac
`
	path := filepath.Join(dir, "hyprctl")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

func TestAvailableFailsWithoutHyprctlInPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := Available(context.Background()); err == nil {
		t.Error("Available should fail when hyprctl is not on PATH")
	}
}

func TestAvailableFailsWithoutInstanceSignature(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	if err := Available(context.Background()); err == nil {
		t.Error("Available should fail without HYPRLAND_INSTANCE_SIGNATURE")
	}
}

func TestAvailableFailsWhenHyprctlVersionFails(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "test")
	t.Setenv("FAKE_HYPRCTL_VERSION_EXIT", "1")
	if err := Available(context.Background()); err == nil {
		t.Error("Available should fail when hyprctl version fails")
	}
}

func TestAvailableSucceeds(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "test")
	if err := Available(context.Background()); err != nil {
		t.Errorf("Available: %v", err)
	}
}

func TestClientsParsesOutput(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_CLIENTS_JSON", `[{"address":"0x1","initialClass":"canaveral-p-f-term"}]`)
	got, err := Clients(context.Background())
	if err != nil {
		t.Fatalf("Clients: %v", err)
	}
	if len(got) != 1 || got[0].InitialClass != "canaveral-p-f-term" {
		t.Errorf("got = %+v", got)
	}
}

func TestClientsErrorsOnNonZeroExit(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_CLIENTS_EXIT", "1")
	if _, err := Clients(context.Background()); err == nil {
		t.Error("Clients should fail when hyprctl exits non-zero")
	}
}

func TestClientsErrorsOnInvalidJSON(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_CLIENTS_JSON", "not json")
	if _, err := Clients(context.Background()); err == nil {
		t.Error("Clients should fail on unparseable JSON")
	}
}

func TestSpawnSucceedsOnOKResponse(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_DISPATCH_STDOUT", "ok")
	err := Spawn(context.Background(), SpawnSpec{Class: "c", Workspace: "norules:f", Cmd: "true"})
	if err != nil {
		t.Errorf("Spawn: %v", err)
	}
}

func TestSpawnFailsOnNonOKResponse(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_DISPATCH_STDOUT", "some hyprctl error")
	err := Spawn(context.Background(), SpawnSpec{Class: "c", Workspace: "norules:f", Cmd: "true"})
	if err == nil || !strings.Contains(err.Error(), "some hyprctl error") {
		t.Errorf("err = %v, want it to surface hyprctl's response", err)
	}
}

func TestSpawnFailsOnNonZeroExit(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	t.Setenv("FAKE_HYPRCTL_DISPATCH_STDERR", "boom")
	err := Spawn(context.Background(), SpawnSpec{Class: "c", Workspace: "norules:f", Cmd: "true"})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want it to include stderr", err)
	}
}

func TestMonitorsParsesOutput(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[{"name":"eDP-1","focused":true},{"name":"DP-3"}]`)
	got, err := Monitors(context.Background())
	if err != nil {
		t.Fatalf("Monitors: %v", err)
	}
	if len(got) != 2 || got[0].Name != "eDP-1" || !got[0].Focused {
		t.Errorf("got = %+v", got)
	}
}

func TestMonitorsErrorsOnNonZeroExit(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_EXIT", "1")
	if _, err := Monitors(context.Background()); err == nil {
		t.Error("Monitors should fail when hyprctl exits non-zero")
	}
}

func TestMonitorsErrorsOnInvalidJSON(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", "not json")
	if _, err := Monitors(context.Background()); err == nil {
		t.Error("Monitors should fail on unparseable JSON")
	}
}

func TestActiveMonitorPrefersFocused(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[{"name":"eDP-1"},{"name":"DP-3","focused":true}]`)
	got, err := ActiveMonitor(context.Background())
	if err != nil {
		t.Fatalf("ActiveMonitor: %v", err)
	}
	if got.Name != "DP-3" {
		t.Errorf("ActiveMonitor = %q, want DP-3", got.Name)
	}
}

func TestActiveMonitorFallsBackToFirstWhenNoneFocused(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[{"name":"eDP-1"},{"name":"DP-3"}]`)
	got, err := ActiveMonitor(context.Background())
	if err != nil {
		t.Fatalf("ActiveMonitor: %v", err)
	}
	if got.Name != "eDP-1" {
		t.Errorf("ActiveMonitor = %q, want the first one", got.Name)
	}
}

func TestActiveMonitorErrorsWithNoMonitors(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[]`)
	if _, err := ActiveMonitor(context.Background()); err == nil {
		t.Error("ActiveMonitor should error with no monitors reported")
	}
}

func TestSecondaryMonitorFindsAnUnfocusedOne(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[{"name":"eDP-1","focused":true},{"name":"DP-3"}]`)
	m, ok, err := SecondaryMonitor(context.Background())
	if err != nil {
		t.Fatalf("SecondaryMonitor: %v", err)
	}
	if !ok || m.Name != "DP-3" {
		t.Errorf("m=%+v ok=%v, want DP-3/true", m, ok)
	}
}

func TestSecondaryMonitorFalseWithOnlyOneMonitor(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON", `[{"name":"eDP-1","focused":true}]`)
	_, ok, err := SecondaryMonitor(context.Background())
	if err != nil {
		t.Fatalf("SecondaryMonitor: %v", err)
	}
	if ok {
		t.Error("SecondaryMonitor should be false with a single monitor")
	}
}

func TestMoveWorkspaceToMonitorSucceeds(t *testing.T) {
	installFakeHyprctl(t)
	if err := MoveWorkspaceToMonitor(context.Background(), "norules:f", "eDP-1"); err != nil {
		t.Errorf("MoveWorkspaceToMonitor: %v", err)
	}
}

func TestMoveWorkspaceToMonitorFails(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	t.Setenv("FAKE_HYPRCTL_DISPATCH_STDERR", "no such monitor")
	err := MoveWorkspaceToMonitor(context.Background(), "norules:f", "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "no such monitor") {
		t.Errorf("err = %v, want it to include stderr", err)
	}
}

func TestActiveWorkspaceNameParses(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_ACTIVEWORKSPACE_JSON", `{"name":"norules:small-fixes"}`)
	got, err := ActiveWorkspaceName(context.Background())
	if err != nil {
		t.Fatalf("ActiveWorkspaceName: %v", err)
	}
	if got != "norules:small-fixes" {
		t.Errorf("got = %q", got)
	}
}

func TestActiveWorkspaceNameErrorsOnNonZeroExit(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_ACTIVEWORKSPACE_EXIT", "1")
	if _, err := ActiveWorkspaceName(context.Background()); err == nil {
		t.Error("ActiveWorkspaceName should fail when hyprctl exits non-zero")
	}
}

func TestFocusWindowSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := FocusWindow(context.Background(), "0x1"); err != nil {
		t.Errorf("FocusWindow: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := FocusWindow(context.Background(), "0x1"); err == nil {
		t.Error("FocusWindow should fail when hyprctl exits non-zero")
	}
}

func TestPreselectSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := Preselect(context.Background(), "r"); err != nil {
		t.Errorf("Preselect: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := Preselect(context.Background(), "r"); err == nil {
		t.Error("Preselect should fail when hyprctl exits non-zero")
	}
}

func TestSplitRatioExactSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := SplitRatioExact(context.Background(), 0.8); err != nil {
		t.Errorf("SplitRatioExact: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := SplitRatioExact(context.Background(), 0.8); err == nil {
		t.Error("SplitRatioExact should fail when hyprctl exits non-zero")
	}
}

func TestEnsureRulesSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := EnsureRules(context.Background()); err != nil {
		t.Errorf("EnsureRules: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_KEYWORD_EXIT", "1")
	if err := EnsureRules(context.Background()); err == nil {
		t.Error("EnsureRules should fail when hyprctl exits non-zero")
	}
}

func TestFocusSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := Focus(context.Background(), "norules:f"); err != nil {
		t.Errorf("Focus: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := Focus(context.Background(), "norules:f"); err == nil {
		t.Error("Focus should fail when hyprctl exits non-zero")
	}
}

func TestCloseSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := Close(context.Background(), "0x1"); err != nil {
		t.Errorf("Close: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := Close(context.Background(), "0x1"); err == nil {
		t.Error("Close should fail when hyprctl exits non-zero")
	}
}

func TestWorkspacesParsesOutput(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_JSON", `[{"id":1,"name":"1","monitor":"eDP-1","windows":2}]`)
	got, err := Workspaces(context.Background())
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(got) != 1 || got[0].Windows != 2 {
		t.Errorf("got = %+v", got)
	}
}

func TestWorkspacesErrorsOnNonZeroExit(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_EXIT", "1")
	if _, err := Workspaces(context.Background()); err == nil {
		t.Error("Workspaces should fail when hyprctl exits non-zero")
	}
}

func TestWorkspacesErrorsOnInvalidJSON(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_JSON", "not json")
	if _, err := Workspaces(context.Background()); err == nil {
		t.Error("Workspaces should fail on unparseable JSON")
	}
}

func TestMoveWindowToWorkspaceSucceedsAndFails(t *testing.T) {
	installFakeHyprctl(t)
	if err := MoveWindowToWorkspace(context.Background(), "0x1", 3); err != nil {
		t.Errorf("MoveWindowToWorkspace: %v", err)
	}
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := MoveWindowToWorkspace(context.Background(), "0x1", 3); err == nil {
		t.Error("MoveWindowToWorkspace should fail when hyprctl exits non-zero")
	}
}

func TestNextFreeWorkspaceIDSkipsPastTheHighestKnown(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_JSON", `[{"id":1},{"id":5},{"id":3}]`)
	got, err := NextFreeWorkspaceID(context.Background())
	if err != nil {
		t.Fatalf("NextFreeWorkspaceID: %v", err)
	}
	if got != 6 {
		t.Errorf("got = %d, want 6", got)
	}
}

func TestNextFreeWorkspaceIDWithNoneKnown(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_JSON", `[]`)
	got, err := NextFreeWorkspaceID(context.Background())
	if err != nil {
		t.Fatalf("NextFreeWorkspaceID: %v", err)
	}
	if got != 1 {
		t.Errorf("got = %d, want 1", got)
	}
}

func TestReleaseWorkspaceRelocatesAMonitorShowingIt(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON",
		`[{"name":"eDP-1","activeWorkspace":{"name":"norules:dying"}}]`)
	t.Setenv("FAKE_HYPRCTL_WORKSPACES_JSON", `[{"id":2}]`)
	if err := ReleaseWorkspace(context.Background(), "norules:dying"); err != nil {
		t.Errorf("ReleaseWorkspace: %v", err)
	}
}

func TestReleaseWorkspaceNoopWhenNoMonitorShowsIt(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON",
		`[{"name":"eDP-1","activeWorkspace":{"name":"norules:other"}}]`)
	// If this were not a no-op it would need workspaces -j too; leaving it
	// unset means the test fails loudly if ReleaseWorkspace tries anyway.
	if err := ReleaseWorkspace(context.Background(), "norules:dying"); err != nil {
		t.Errorf("ReleaseWorkspace: %v", err)
	}
}

func TestReleaseWorkspacePropagatesADispatchFailure(t *testing.T) {
	installFakeHyprctl(t)
	t.Setenv("FAKE_HYPRCTL_MONITORS_JSON",
		`[{"name":"eDP-1","activeWorkspace":{"name":"norules:dying"}}]`)
	t.Setenv("FAKE_HYPRCTL_DISPATCH_EXIT", "1")
	if err := ReleaseWorkspace(context.Background(), "norules:dying"); err == nil {
		t.Error("ReleaseWorkspace should surface a failed focusmonitor dispatch")
	}
}

func TestIsSelfTrueForTheCallingProcess(t *testing.T) {
	if !IsSelf(Client{PID: os.Getpid()}) {
		t.Error("IsSelf should be true for the calling process's own PID")
	}
}

func TestIsSelfFalseForAnUnrelatedPID(t *testing.T) {
	// A PID that (almost certainly) does not exist, so parentPID fails
	// immediately and IsSelf must not treat that as a match.
	if IsSelf(Client{PID: 999999999}) {
		t.Error("IsSelf should be false for a PID that cannot be our ancestor")
	}
}

func TestIsSelfFalseForANonPositivePID(t *testing.T) {
	if IsSelf(Client{PID: 0}) {
		t.Error("IsSelf should be false for PID 0")
	}
}
