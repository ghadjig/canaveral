// Package wm drives the window manager a feature's windows live in.
//
// Two compositors are supported: Hyprland, through hyprctl, and sway, through
// swaymsg. Everything above this package talks in terms canaveral chose —
// a named workspace per feature, a class per window — and this package is the
// only place that knows how a particular compositor spells them.
//
// Windows are identified by the class canaveral assigns when spawning them.
// Hyprland reports it as initialClass and sway as app_id (or, for XWayland
// windows, window_properties.class); neither changes for the lifetime of the
// window. That makes reconciliation a simple set difference between the
// windows a feature declares and the ones currently open.
package wm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrUnavailable indicates no supported compositor is running, or its control
// binary is missing.
var ErrUnavailable = errors.New("no supported window manager is available")

// Client is an open window.
type Client struct {
	// Address is an opaque handle for this window, meaningful only to the
	// backend that produced it: a pointer for Hyprland, a container id for
	// sway.
	Address string
	// Class is the window's current class, and InitialClass the one it
	// mapped with. Only Hyprland distinguishes the two; sway reports a
	// single app_id, so both carry it.
	Class        string
	InitialClass string
	Title        string
	PID          int
	Workspace    struct {
		ID   int
		Name string
	}
	// At and Size are the window's position and size in layout pixels, and
	// Floating says whether it is outside the tiled layout. Only a layout
	// cares: they are how a column arrangement can be checked against what
	// was asked for, rather than trusted because the commands were accepted.
	At       [2]int
	Size     [2]int
	Floating bool
}

// Monitor is a connected output.
type Monitor struct {
	Name            string
	Focused         bool
	ActiveWorkspace struct {
		Name string
	}
}

// Workspace is one of the compositor's workspaces.
type Workspace struct {
	// ID is the ordinary workspace number, and is <= 0 for a workspace that
	// has a name rather than a number — which is what every canaveral
	// workspace has. See RehomeTarget.
	ID      int
	Name    string
	Monitor string
}

// SpawnSpec describes a window to launch.
type SpawnSpec struct {
	// Class is the window class used for later reconciliation.
	Class string
	Title string
	// Workspace is the named workspace to place the window on.
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

// backend is one compositor's implementation. It is deliberately unexported:
// callers pick nothing, they get whatever is running.
//
// Every method that reaches the compositor is here, so that adding a third
// compositor is a matter of satisfying this list rather than finding call
// sites. The pure helpers — class naming, /proc lookups, client matching —
// are shared and live outside it.
type backend interface {
	// name is the compositor's name, for error messages.
	name() string
	available(ctx context.Context) error

	clients(ctx context.Context) ([]Client, error)
	monitors(ctx context.Context) ([]Monitor, error)
	workspaces(ctx context.Context) ([]Workspace, error)
	activeWorkspaceName(ctx context.Context) (string, error)
	nextFreeWorkspaceID(ctx context.Context) (int, error)

	ensureRules(ctx context.Context) error
	// ensurePlacement installs the rule that sends a window of the given
	// class to the given workspace without focusing it. Part of spawn, and
	// separate only so that a window started outside the compositor's own
	// exec can be placed too — see the live tests.
	ensurePlacement(ctx context.Context, class, workspace string) error
	spawn(ctx context.Context, s SpawnSpec) error
	focus(ctx context.Context, workspace string) error
	closeWindow(ctx context.Context, address string) error
	moveWorkspaceToMonitor(ctx context.Context, workspace, monitor string) error
	releaseWorkspace(ctx context.Context, workspace string) error
	moveWindowToWorkspace(ctx context.Context, address string, workspace int) error
	moveWindowToNamedWorkspace(ctx context.Context, address, workspace string) error
	applyColumns(ctx context.Context, addresses []string, fractions []float64) error
}

// OverrideEnv names the environment variable that forces a particular
// backend, bypassing detection: "hyprland" or "sway".
//
// Detection reads the variables the compositors export into their own
// sessions, which is right for a session started by either of them and wrong
// for anything else — a unit whose environment was imported before the
// compositor started, a nested session, a test. This is the way out that does
// not require guessing.
const OverrideEnv = "CANAVERAL_WM"

// current picks the backend for the compositor this process is running under.
//
// Selection is by environment variable alone, so it costs nothing and every
// entry point can do it. Whether that compositor is actually answering is a
// separate question, asked by Available.
func current() (backend, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(OverrideEnv))); v {
	case "hyprland", "hypr":
		return hyprland{}, nil
	case "sway":
		return swaywm{}, nil
	case "":
	default:
		return nil, fmt.Errorf("%w: %s=%q is not a supported window manager", ErrUnavailable, OverrideEnv, v)
	}

	inHyprland := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") != ""
	inSway := os.Getenv("SWAYSOCK") != ""
	switch {
	case inHyprland && inSway:
		// One session nested inside the other: the inner compositor's
		// variable is set, but so is the outer one's, inherited. The desktop
		// name is set by whichever session actually started this process, so
		// it is the one that breaks the tie.
		if strings.Contains(strings.ToLower(os.Getenv("XDG_CURRENT_DESKTOP")), "sway") {
			return swaywm{}, nil
		}
		return hyprland{}, nil
	case inHyprland:
		return hyprland{}, nil
	case inSway:
		return swaywm{}, nil
	}
	return nil, fmt.Errorf("%w: neither HYPRLAND_INSTANCE_SIGNATURE nor SWAYSOCK is set", ErrUnavailable)
}

// Available reports whether a supported compositor is running and answering.
//
// Every caller treats a failure here as "skip the window layer and carry on",
// so the error says which compositor was expected and why it did not answer.
func Available(ctx context.Context) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.available(ctx)
}

// Clients returns every open window.
func Clients(ctx context.Context) ([]Client, error) {
	b, err := current()
	if err != nil {
		return nil, err
	}
	return b.clients(ctx)
}

// Monitors returns every currently connected output.
func Monitors(ctx context.Context) ([]Monitor, error) {
	b, err := current()
	if err != nil {
		return nil, err
	}
	return b.monitors(ctx)
}

// ActiveMonitor returns the currently focused monitor.
//
// A newly created named workspace appears on whichever monitor is focused at
// the time, so this is where a feature's windows are about to show up.
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
	return Monitor{}, errors.New("no monitors reported")
}

// Workspaces lists every workspace the compositor currently knows about.
func Workspaces(ctx context.Context) ([]Workspace, error) {
	b, err := current()
	if err != nil {
		return nil, err
	}
	return b.workspaces(ctx)
}

// ActiveWorkspaceName returns the name of the currently focused workspace.
func ActiveWorkspaceName(ctx context.Context) (string, error) {
	b, err := current()
	if err != nil {
		return "", err
	}
	return b.activeWorkspaceName(ctx)
}

// NextFreeWorkspaceID returns an ordinary workspace number guaranteed not to
// collide with any workspace the compositor currently knows about, for
// callers that need somewhere empty to put windows.
func NextFreeWorkspaceID(ctx context.Context) (int, error) {
	b, err := current()
	if err != nil {
		return 0, err
	}
	return b.nextFreeWorkspaceID(ctx)
}

// EnsureRules installs the window rules canaveral relies on.
func EnsureRules(ctx context.Context) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.ensureRules(ctx)
}

// Spawn launches a window on its feature's workspace without stealing focus.
func Spawn(ctx context.Context, s SpawnSpec) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.spawn(ctx, s)
}

// Focus switches to a workspace, following it to whichever monitor owns it.
func Focus(ctx context.Context, workspace string) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.focus(ctx, workspace)
}

// Close closes a window by address.
func Close(ctx context.Context, address string) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.closeWindow(ctx, address)
}

// MoveWorkspaceToMonitor reassigns which monitor a workspace belongs to.
//
// This does not necessarily make the workspace the visible one on that
// monitor — a monitor can own several workspaces, only one of which is
// displayed at a time. Follow it with Focus to show it.
func MoveWorkspaceToMonitor(ctx context.Context, workspace, monitor string) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.moveWorkspaceToMonitor(ctx, workspace, monitor)
}

// ReleaseWorkspace switches any monitor currently displaying the named
// workspace to a fresh, empty numbered workspace instead.
//
// Neither compositor destroys a workspace while it is a monitor's active one,
// even once it has zero windows left, so a feature's workspace outlives the
// feature unless something switches away from it. Confirmed empirically on
// Hyprland; sway's workspace_consider_destroy has the same precondition.
func ReleaseWorkspace(ctx context.Context, workspace string) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.releaseWorkspace(ctx, workspace)
}

// MoveWindowToWorkspace moves one window to an ordinary numbered workspace
// without following it, so tearing a feature down does not drag the user's
// view along with it.
func MoveWindowToWorkspace(ctx context.Context, address string, workspace int) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.moveWindowToWorkspace(ctx, address, workspace)
}

// MoveWindowToNamedWorkspace moves a window onto a named workspace without
// following it there.
//
// Needed because a placement rule can be evaded: an application that hands
// the request off to an already-running instance of itself — or re-execs, as
// an AppImage does once it has mounted its own payload — draws its window
// from somewhere the rule never matched. Such a window has to be found
// afterwards and moved by hand.
func MoveWindowToNamedWorkspace(ctx context.Context, address, workspace string) error {
	b, err := current()
	if err != nil {
		return err
	}
	return b.moveWindowToNamedWorkspace(ctx, address, workspace)
}

// ApplyColumns arranges already-mapped windows into a left-to-right row
// occupying the given fractions of the workspace's width.
//
// Fractions are one per window, in the same order as addresses, and are
// expected to sum to roughly 1. They are the contract because they are what
// the user wrote in [layout]; how a compositor is persuaded to produce them —
// dwindle split ratios on Hyprland, percentage-point resizes on sway — is
// each backend's own problem.
func ApplyColumns(ctx context.Context, addresses []string, fractions []float64) error {
	if len(addresses) < 2 {
		return nil
	}
	if len(fractions) != len(addresses) {
		return fmt.Errorf("column layout needs one fraction per window")
	}
	if err := validFractions(fractions); err != nil {
		return err
	}
	b, err := current()
	if err != nil {
		return err
	}
	return b.applyColumns(ctx, addresses, fractions)
}
