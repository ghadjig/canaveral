package wm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// hyprland drives Hyprland through hyprctl.
type hyprland struct{}

func (hyprland) name() string { return "hyprland" }

// hyprClient is one entry of `hyprctl clients -j`.
type hyprClient struct {
	Address      string `json:"address"`
	Class        string `json:"class"`
	InitialClass string `json:"initialClass"`
	Title        string `json:"title"`
	PID          int    `json:"pid"`
	Workspace    struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	At       [2]int `json:"at"`
	Size     [2]int `json:"size"`
	Floating bool   `json:"floating"`
}

func (c hyprClient) client() Client {
	out := Client{
		Address:      c.Address,
		Class:        c.Class,
		InitialClass: c.InitialClass,
		Title:        c.Title,
		PID:          c.PID,
		At:           c.At,
		Size:         c.Size,
		Floating:     c.Floating,
	}
	out.Workspace.ID = c.Workspace.ID
	out.Workspace.Name = c.Workspace.Name
	return out
}

// available reports whether hyprctl can talk to a running compositor.
func (hyprland) available(ctx context.Context) error {
	if _, err := exec.LookPath("hyprctl"); err != nil {
		return fmt.Errorf("%w: hyprctl not in PATH", ErrUnavailable)
	}
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return fmt.Errorf("%w: HYPRLAND_INSTANCE_SIGNATURE is not set", ErrUnavailable)
	}
	if err := exec.CommandContext(ctx, "hyprctl", "version").Run(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

func (hyprland) clients(ctx context.Context) ([]Client, error) {
	var cs []hyprClient
	if err := hyprJSON(ctx, &cs, "clients"); err != nil {
		return nil, err
	}
	out := make([]Client, len(cs))
	for i, c := range cs {
		out[i] = c.client()
	}
	return out, nil
}

func (hyprland) monitors(ctx context.Context) ([]Monitor, error) {
	var ms []struct {
		Name            string `json:"name"`
		Focused         bool   `json:"focused"`
		ActiveWorkspace struct {
			Name string `json:"name"`
		} `json:"activeWorkspace"`
	}
	if err := hyprJSON(ctx, &ms, "monitors"); err != nil {
		return nil, err
	}
	out := make([]Monitor, len(ms))
	for i, m := range ms {
		out[i].Name = m.Name
		out[i].Focused = m.Focused
		out[i].ActiveWorkspace.Name = m.ActiveWorkspace.Name
	}
	return out, nil
}

func (hyprland) workspaces(ctx context.Context) ([]Workspace, error) {
	var ws []struct {
		ID      int    `json:"id"`
		Name    string `json:"name"`
		Monitor string `json:"monitor"`
	}
	if err := hyprJSON(ctx, &ws, "workspaces"); err != nil {
		return nil, err
	}
	out := make([]Workspace, len(ws))
	for i, w := range ws {
		out[i] = Workspace{ID: w.ID, Name: w.Name, Monitor: w.Monitor}
	}
	return out, nil
}

// activeWorkspaceName returns the name of the currently visible workspace.
//
// Used to save and restore the user's view around the focus shuffling a
// layout requires: focusing a window — confirmed empirically — switches the
// visible workspace to wherever that window lives, even if the user was
// looking at something else entirely.
func (hyprland) activeWorkspaceName(ctx context.Context) (string, error) {
	var v struct {
		Name string `json:"name"`
	}
	if err := hyprJSON(ctx, &v, "activeworkspace"); err != nil {
		return "", err
	}
	return v.Name, nil
}

// nextFreeWorkspaceID returns a numeric workspace ID guaranteed not to
// collide with any workspace Hyprland currently knows about, named or
// numbered, so switching a monitor to it always lands on a genuinely new,
// empty workspace rather than one that already has real content elsewhere.
func (h hyprland) nextFreeWorkspaceID(ctx context.Context) (int, error) {
	ws, err := h.workspaces(ctx)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, w := range ws {
		if w.ID > max {
			max = w.ID
		}
	}
	return max + 1, nil
}

// ensureRules installs the window rules canaveral relies on.
//
// Only a tile rule is needed: XWayland application windows — a browser
// started with --class, for instance — open floating by default, and every
// canaveral window should join the normal tiled layout instead. Windows are
// deliberately left ungrouped; no tab bar is created.
func (hyprland) ensureRules(ctx context.Context) error {
	if err := windowRule(ctx, "windowrule", "match:class ^(canaveral-.*)$, float off"); err == nil {
		return nil
	}
	return windowRule(ctx, "windowrulev2", "tile,class:^(canaveral-.*)$")
}

// spawn launches a window on its feature's workspace without stealing focus.
func (h hyprland) spawn(ctx context.Context, s SpawnSpec) error {
	// Written before the dispatch and removed by the shell that reads it, so
	// nothing of the environment appears in any process's arguments. See
	// createSpawnFile.
	envFile, err := writeEnvFile(s.Env)
	if err != nil {
		return fmt.Errorf("spawn %s: %w", s.Class, err)
	}
	argv, err := buildArgv(s, envFile)
	if err != nil {
		removeSpawnFile(envFile)
		return err
	}
	if err := h.ensurePlacement(ctx, s.Class, s.Workspace); err != nil {
		removeSpawnFile(envFile)
		return fmt.Errorf("spawn %s: %w", s.Class, err)
	}

	// `silent` keeps the new window off-screen-focus so opening a feature does
	// not yank the user away from what they are doing.
	rule := fmt.Sprintf("[workspace name:%s silent]", s.Workspace)
	line := rule + " " + argv

	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "exec", line)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		removeSpawnFile(envFile)
		return fmt.Errorf("spawn %s: %w: %s", s.Class, err, strings.TrimSpace(stderr.String()))
	}
	if res := strings.TrimSpace(string(out)); res != "" && res != "ok" {
		removeSpawnFile(envFile)
		return fmt.Errorf("spawn %s: hyprctl said %q", s.Class, res)
	}
	return nil
}

// focus switches to a workspace, following it to whichever monitor owns it.
//
// Confirmed empirically: dispatching a workspace that belongs to another
// monitor moves focus to that monitor rather than dragging the workspace
// across, which is what makes this usable as a "put the user back where they
// were" primitive.
func (hyprland) focus(ctx context.Context, workspace string) error {
	if err := hyprDispatch(ctx, "workspace", workspaceArg(workspace)); err != nil {
		return fmt.Errorf("focus %s: %w", workspace, err)
	}
	return nil
}

func (hyprland) closeWindow(ctx context.Context, address string) error {
	return exec.CommandContext(ctx, "hyprctl", "dispatch", "closewindow", "address:"+address).Run()
}

func (hyprland) moveWorkspaceToMonitor(ctx context.Context, workspace, monitor string) error {
	if err := hyprDispatch(ctx, "moveworkspacetomonitor", "name:"+workspace, monitor); err != nil {
		return fmt.Errorf("move workspace %s to %s: %w", workspace, monitor, err)
	}
	return nil
}

// releaseWorkspace switches any monitor currently displaying the named
// workspace to a fresh, empty numbered workspace instead.
//
// Confirmed empirically: Hyprland will not destroy a workspace while it is a
// monitor's active one, even once it has zero windows left. So once a
// feature's last window closes, its named workspace lingers on whatever
// monitor last displayed it — showing up as a phantom canaveral waybar pill,
// and leaving that monitor's active-window-title module with nothing to
// show. Switching the monitor to a brand new numbered workspace (found by
// focusing it, then dispatching one past the highest ID currently known —
// dispatching a relative "empty workspace" instead was tried and found to
// land on the wrong monitor entirely) lets Hyprland reap the abandoned one
// immediately, exactly as if the user had switched away from it themselves.
func (h hyprland) releaseWorkspace(ctx context.Context, workspace string) error {
	ms, err := h.monitors(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.ActiveWorkspace.Name != workspace {
			continue
		}
		if err := hyprFocusMonitor(ctx, m.Name); err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
		id, err := h.nextFreeWorkspaceID(ctx)
		if err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
		if err := hyprDispatch(ctx, "workspace", strconv.Itoa(id)); err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
	}
	return nil
}

func (hyprland) moveWindowToWorkspace(ctx context.Context, address string, workspace int) error {
	err := hyprDispatch(ctx, "movetoworkspacesilent", fmt.Sprintf("%d,address:%s", workspace, address))
	if err != nil {
		return fmt.Errorf("move window %s to workspace %d: %w", address, workspace, err)
	}
	return nil
}

func (hyprland) moveWindowToNamedWorkspace(ctx context.Context, address, workspace string) error {
	err := hyprDispatch(ctx, "movetoworkspacesilent", fmt.Sprintf("name:%s,address:%s", workspace, address))
	if err != nil {
		return fmt.Errorf("move window %s to workspace %s: %w", address, workspace, err)
	}
	return nil
}

func hyprFocusMonitor(ctx context.Context, monitor string) error {
	if err := hyprDispatch(ctx, "focusmonitor", monitor); err != nil {
		return fmt.Errorf("focus monitor %s: %w", monitor, err)
	}
	return nil
}

// workspaceArg renders a workspace name as an argument to the `workspace`
// dispatcher.
//
// Hyprland's `name:` selector always means a *named* workspace, so `name:4`
// does not switch to workspace 4 — it creates a second workspace that merely
// prints as "4", at a negative ID, alongside the real one. Numeric names
// therefore have to go through as bare IDs. Confirmed empirically:
// `dispatch workspace name:7` produced id -1351 named "7" while the numeric
// 7 sat untouched.
//
// This matters because not every workspace canaveral switches to is one of
// its own: restoring the view after a build returns the user to whatever
// they were on, which is usually a plain number.
func workspaceArg(name string) string {
	if _, err := strconv.Atoi(name); err == nil {
		return name
	}
	return "name:" + name
}

// hyprJSON runs a hyprctl query and decodes its JSON into v.
func hyprJSON(ctx context.Context, v any, args ...string) error {
	out, err := exec.CommandContext(ctx, "hyprctl", append(args, "-j")...).Output()
	if err != nil {
		return fmt.Errorf("hyprctl %s: %w", strings.Join(args, " "), err)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("parse hyprctl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// hyprDispatch runs a dispatcher, reporting anything it wrote to stderr.
func hyprDispatch(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "hyprctl", append([]string{"dispatch"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
