// Package hypr drives Hyprland through hyprctl.
//
// Windows are identified by the class canaveral assigns when spawning them,
// which Hyprland reports as initialClass and never changes for the lifetime of
// the window. That makes reconciliation a simple set difference between the
// windows a feature declares and the ones currently open.
package hypr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// ErrUnavailable indicates Hyprland is not running or hyprctl is missing.
var ErrUnavailable = errors.New("hyprland is not available")

// Client is an open window as reported by hyprctl.
type Client struct {
	Address      string `json:"address"`
	Class        string `json:"class"`
	InitialClass string `json:"initialClass"`
	Title        string `json:"title"`
	InitialTitle string `json:"initialTitle"`
	PID          int    `json:"pid"`
	Workspace    struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"workspace"`
	Grouped  []string `json:"grouped"`
	At       [2]int   `json:"at"`
	Size     [2]int   `json:"size"`
	Floating bool     `json:"floating"`
}

// Available reports whether hyprctl can talk to a running compositor.
func Available(ctx context.Context) error {
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

// Clients returns every open window.
func Clients(ctx context.Context) ([]Client, error) {
	out, err := exec.CommandContext(ctx, "hyprctl", "clients", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("hyprctl clients: %w", err)
	}
	var cs []Client
	if err := json.Unmarshal(out, &cs); err != nil {
		return nil, fmt.Errorf("parse hyprctl clients: %w", err)
	}
	return cs, nil
}

// ByClass indexes clients by their initial class.
func ByClass(cs []Client) map[string]Client {
	m := make(map[string]Client, len(cs))
	for _, c := range cs {
		// Keep the first match so a duplicate never hides the original window.
		if _, ok := m[c.InitialClass]; !ok {
			m[c.InitialClass] = c
		}
	}
	return m
}

// SpawnSpec describes a window to launch.
type SpawnSpec struct {
	// Class is the window class used for later reconciliation.
	Class string
	Title string
	// Workspace is the named Hyprland workspace to place the window on.
	Workspace string
	// Dir is the working directory.
	Dir string
	// IsTerminal wraps Cmd in a terminal emulator.
	IsTerminal bool
	// Terminal is the emulator binary to use.
	Terminal string
	// Cmd is the command line. Empty with Terminal set opens a plain shell.
	Cmd string
	// Hold keeps the terminal open after Cmd exits.
	Hold bool
	// Env is exported to the spawned process.
	Env map[string]string
}

// TerminalBin is the default terminal emulator for Terminal windows.
var TerminalBin = "alacritty"

// Spawn launches a window on its feature's workspace without stealing focus.
func Spawn(ctx context.Context, s SpawnSpec) error {
	argv, err := buildArgv(s)
	if err != nil {
		return err
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
		return fmt.Errorf("spawn %s: %w: %s", s.Class, err, strings.TrimSpace(stderr.String()))
	}
	if res := strings.TrimSpace(string(out)); res != "" && res != "ok" {
		return fmt.Errorf("spawn %s: hyprctl said %q", s.Class, res)
	}
	return nil
}

// buildArgv renders the shell command hyprctl should execute.
func buildArgv(s SpawnSpec) (string, error) {
	if s.Class == "" {
		return "", errors.New("spawn: class is required")
	}

	// Environment is applied to the terminal itself rather than to the inner
	// command, so an interactive shell started with no command still inherits
	// CANAVERAL_ROOT and friends.
	var prefix strings.Builder
	if len(s.Env) > 0 {
		prefix.WriteString("env ")
		for _, kv := range sortedEnv(s.Env) {
			prefix.WriteString(shellQuote(kv))
			prefix.WriteString(" ")
		}
	}

	if !s.IsTerminal {
		if strings.TrimSpace(s.Cmd) == "" {
			return "", fmt.Errorf("window %s: exec command is empty", s.Class)
		}
		// GUI applications set their own class; the command is used verbatim.
		inner := prefix.String() + s.Cmd
		if s.Dir != "" {
			inner = fmt.Sprintf("cd %s && %s", shellQuote(s.Dir), inner)
		}
		return "sh -c " + shellQuote(inner), nil
	}

	term := s.Terminal
	if term == "" {
		term = TerminalBin
	}
	parts := []string{
		term,
		"--class", s.Class + "," + s.Class,
	}
	if s.Title != "" {
		parts = append(parts, "-T", s.Title)
	}
	if s.Dir != "" {
		parts = append(parts, "--working-directory", s.Dir)
	}
	if strings.TrimSpace(s.Cmd) != "" {
		inner := s.Cmd
		if s.Hold {
			// Keep the pane alive so a crash is readable instead of vanishing.
			inner += "; echo; echo '[canaveral] command exited, press enter to close'; read _"
		}
		parts = append(parts, "-e", "sh", "-c", inner)
	}

	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return prefix.String() + strings.Join(quoted, " "), nil
}

func sortedEnv(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	// Deterministic ordering keeps spawned command lines stable across runs.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Monitor is an output as reported by hyprctl.
type Monitor struct {
	Name            string `json:"name"`
	Width           int    `json:"width"`
	Height          int    `json:"height"`
	X               int    `json:"x"`
	Y               int    `json:"y"`
	Focused         bool   `json:"focused"`
	Reserved        [4]int `json:"reserved"` // left, top, right, bottom, in pixels
	ActiveWorkspace struct {
		Name string `json:"name"`
	} `json:"activeWorkspace"`
}

// UsableArea returns the monitor's geometry minus space reserved by bars and
// similar layer-shell surfaces (waybar, in this setup).
func (m Monitor) UsableArea() (x, y, width, height int) {
	x = m.X + m.Reserved[0]
	y = m.Y + m.Reserved[1]
	width = m.Width - m.Reserved[0] - m.Reserved[2]
	height = m.Height - m.Reserved[1] - m.Reserved[3]
	return
}

// Monitors returns every currently connected output.
func Monitors(ctx context.Context) ([]Monitor, error) {
	out, err := exec.CommandContext(ctx, "hyprctl", "monitors", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("hyprctl monitors: %w", err)
	}
	var ms []Monitor
	if err := json.Unmarshal(out, &ms); err != nil {
		return nil, fmt.Errorf("parse hyprctl monitors: %w", err)
	}
	return ms, nil
}

// ActiveMonitor returns the currently focused monitor.
//
// A newly created named workspace appears on whichever monitor is focused at
// the time, so this is the geometry a layout's fractions should be computed
// against.
func ActiveMonitor(ctx context.Context) (Monitor, error) {
	ms, err := Monitors(ctx)
	if err != nil {
		return Monitor{}, err
	}
	for _, m := range ms {
		if m.Focused {
			return m, nil
		}
	}
	if len(ms) > 0 {
		return ms[0], nil
	}
	return Monitor{}, fmt.Errorf("no monitors reported")
}

// SecondaryMonitor returns any connected monitor other than the currently
// focused one, and false if there is only a single monitor.
//
// Used to build a feature's layout somewhere other than the monitor the user
// is actually looking at: confirmed empirically that focusing a window (which
// splitratio and preselect both require) only changes what is displayed on
// that window's own monitor, and does not steal keyboard focus away from
// wherever the user actually has it. Building on a monitor they are not
// using leaves their real screen completely undisturbed for the whole
// operation, not just restored afterwards.
func SecondaryMonitor(ctx context.Context) (Monitor, bool, error) {
	ms, err := Monitors(ctx)
	if err != nil {
		return Monitor{}, false, err
	}
	for _, m := range ms {
		if !m.Focused {
			return m, true, nil
		}
	}
	return Monitor{}, false, nil
}

// MoveWorkspaceToMonitor reassigns which monitor a workspace belongs to.
//
// This does not necessarily make the workspace the visible one on that
// monitor (a monitor can own several workspaces, only one of which is
// displayed at a time) — only focusing a window on it does that.
func MoveWorkspaceToMonitor(ctx context.Context, workspace, monitor string) error {
	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "moveworkspacetomonitor",
		"name:"+workspace, monitor)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("move workspace %s to %s: %w: %s", workspace, monitor, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// MonitorAt returns whichever monitor's rectangle contains the point (x, y),
// which is how a window's own monitor is found without assuming it is on
// whichever one currently has focus (it usually is not, by the time you have
// switched away from its workspace).
func MonitorAt(ms []Monitor, x, y int) (Monitor, bool) {
	for _, m := range ms {
		if x >= m.X && x < m.X+m.Width && y >= m.Y && y < m.Y+m.Height {
			return m, true
		}
	}
	return Monitor{}, false
}

// ReleaseWorkspace switches any monitor currently displaying the named
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
func ReleaseWorkspace(ctx context.Context, workspace string) error {
	ms, err := Monitors(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.ActiveWorkspace.Name != workspace {
			continue
		}
		if err := focusMonitor(ctx, m.Name); err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
		id, err := nextFreeWorkspaceID(ctx)
		if err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
		if err := switchToWorkspaceID(ctx, id); err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
	}
	return nil
}

func focusMonitor(ctx context.Context, monitor string) error {
	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "focusmonitor", monitor)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("focus monitor %s: %w: %s", monitor, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func switchToWorkspaceID(ctx context.Context, id int) error {
	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "workspace", strconv.Itoa(id))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("workspace %d: %w: %s", id, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// nextFreeWorkspaceID returns a numeric workspace ID guaranteed not to
// collide with any workspace Hyprland currently knows about, named or
// numbered, so switching a monitor to it always lands on a genuinely new,
// empty workspace rather than one that already has real content elsewhere.
func nextFreeWorkspaceID(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "hyprctl", "workspaces", "-j").Output()
	if err != nil {
		return 0, fmt.Errorf("hyprctl workspaces: %w", err)
	}
	var ws []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(out, &ws); err != nil {
		return 0, fmt.Errorf("parse hyprctl workspaces: %w", err)
	}
	max := 0
	for _, w := range ws {
		if w.ID > max {
			max = w.ID
		}
	}
	return max + 1, nil
}

// ActiveWorkspaceName returns the name of the currently visible workspace.
//
// Used to save and restore the user's view around the focus-shuffling that
// SplitRatioExact and Preselect require (both are relative to whichever
// window is currently focused, and hyprctl dispatch focuswindow — confirmed
// empirically — switches the visible workspace to wherever that window
// lives, even if the user was looking at something else entirely).
func ActiveWorkspaceName(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "hyprctl", "activeworkspace", "-j").Output()
	if err != nil {
		return "", fmt.Errorf("hyprctl activeworkspace: %w", err)
	}
	var v struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", fmt.Errorf("parse hyprctl activeworkspace: %w", err)
	}
	return v.Name, nil
}

// FocusWindow focuses a window by address.
//
// This changes the visible workspace if the window lives on a different one
// than whatever is currently shown — there is no way to run
// splitratio/preselect against a specific window without this.
func FocusWindow(ctx context.Context, address string) error {
	return exec.CommandContext(ctx, "hyprctl", "dispatch", "focuswindow", "address:"+address).Run()
}

// Preselect arms the dwindle layout to split the next window that opens on
// this workspace in a specific direction from whatever is currently focused,
// instead of dwindle's default behaviour of alternating split direction
// based on window aspect ratio (which produces a 2x2-ish grid for 4 windows,
// not a left-to-right chain of columns).
func Preselect(ctx context.Context, direction string) error {
	return exec.CommandContext(ctx, "hyprctl", "dispatch", "layoutmsg", "preselect "+direction).Run()
}

// SplitRatioExact sets the dwindle split ratio between the focused window and
// its sibling.
//
// The relationship between the ratio argument and the resulting size
// fraction was confirmed empirically (fraction = ratio / 2; ratio 1.0 is an
// even 50/50 split, not — as the name might suggest — "give the focused
// window 100%"), since Hyprland does not document the exact mapping.
func SplitRatioExact(ctx context.Context, ratio float64) error {
	return exec.CommandContext(ctx, "hyprctl", "dispatch", "splitratio",
		fmt.Sprintf("exact %.4f", ratio)).Run()
}

// EnsureRules installs the window rules canaveral relies on.
//
// Only a tile rule is needed: XWayland application windows — a browser
// started with --class, for instance — open floating by default, and every
// canaveral window should join the normal tiled layout instead. Windows are
// deliberately left ungrouped; no tab bar is created.
func EnsureRules(ctx context.Context) error {
	rule := "tile,class:^(canaveral-.*)$"
	cmd := exec.CommandContext(ctx, "hyprctl", "keyword", "windowrulev2", rule)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install rule %q: %w: %s", rule, err,
			strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Focus switches to a feature's workspace.
func Focus(ctx context.Context, workspace string) error {
	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "workspace", "name:"+workspace)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("focus %s: %w: %s", workspace, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Close closes a window by address.
func Close(ctx context.Context, address string) error {
	return exec.CommandContext(ctx, "hyprctl", "dispatch", "closewindow", "address:"+address).Run()
}

// IsSelf reports whether c is the window hosting the calling process — i.e.
// whether c.PID is an ancestor of (or is) os.Getpid(). This lets callers that
// are about to close a batch of windows they own recognize and defer closing
// the one they're themselves running in, since dispatching closewindow
// against it can terminate the calling process immediately.
func IsSelf(c Client) bool {
	if c.PID <= 0 {
		return false
	}
	pid := os.Getpid()
	for depth := 0; depth < 64 && pid > 1; depth++ {
		if pid == c.PID {
			return true
		}
		ppid, err := parentPID(pid)
		if err != nil {
			return false
		}
		pid = ppid
	}
	return false
}

// parentPID reads the parent PID of pid from /proc, for walking the process
// ancestry chain in IsSelf.
func parentPID(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// Format: pid (comm) state ppid ... — comm may itself contain spaces or
	// parens, so find the last ')' and split from there.
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, err
	}
	return ppid, nil
}

var classSafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// ClassPrefix is the shared prefix of every window class for a feature.
func ClassPrefix(project, feature string) string {
	return fmt.Sprintf("canaveral-%s-%s-", clean(project), clean(feature))
}

func clean(s string) string {
	return strings.Trim(classSafe.ReplaceAllString(s, "-"), "-")
}

// Class builds the window class for a feature's window.
func Class(project, feature, window string) string {
	return ClassPrefix(project, feature) + clean(window)
}

// Workspace is one entry of `hyprctl workspaces`.
type Workspace struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Monitor string `json:"monitor"`
	Windows int    `json:"windows"`
}

// Workspaces lists every workspace the compositor currently knows about.
func Workspaces(ctx context.Context) ([]Workspace, error) {
	out, err := exec.CommandContext(ctx, "hyprctl", "workspaces", "-j").Output()
	if err != nil {
		return nil, fmt.Errorf("hyprctl workspaces: %w", err)
	}
	var ws []Workspace
	if err := json.Unmarshal(out, &ws); err != nil {
		return nil, fmt.Errorf("parse hyprctl workspaces: %w", err)
	}
	return ws, nil
}

// RehomeTarget picks where windows stranded on a dying workspace should go:
// the lowest-numbered ordinary workspace already on the same monitor, so they
// stay on the screen the user had them on. Named workspaces are skipped —
// they belong to other features — as is the workspace being torn down.
//
// Returns 0 when the monitor has no ordinary workspace to fall back to, and
// the caller should pick a fresh one instead.
func RehomeTarget(ws []Workspace, monitor, leaving string) int {
	best := 0
	for _, w := range ws {
		if w.Monitor != monitor || w.Name == leaving || w.ID <= 0 {
			continue
		}
		// Ordinary workspaces are the numeric ones; a named workspace is
		// some other feature's.
		if strconv.Itoa(w.ID) != w.Name {
			continue
		}
		if best == 0 || w.ID < best {
			best = w.ID
		}
	}
	return best
}

// MoveWindowToWorkspace moves one window without following it, so tearing a
// feature down does not drag the user's view along with it.
func MoveWindowToWorkspace(ctx context.Context, address string, workspace int) error {
	cmd := exec.CommandContext(ctx, "hyprctl", "dispatch", "movetoworkspacesilent",
		fmt.Sprintf("%d,address:%s", workspace, address))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("move window %s to workspace %d: %w: %s", address, workspace, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// NextFreeWorkspaceID is the exported form of nextFreeWorkspaceID, for
// callers that need somewhere guaranteed empty to put windows.
func NextFreeWorkspaceID(ctx context.Context) (int, error) { return nextFreeWorkspaceID(ctx) }
