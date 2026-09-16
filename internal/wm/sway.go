package wm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// swaywm drives sway through swaymsg.
//
// Where Hyprland takes a window rule scoped to the process hyprctl starts,
// sway has no equivalent, so every placement here is a `for_window` criteria
// rule matched against the class canaveral assigned. That is the same
// mechanism Hyprland needs for browsers and AppImages anyway — see
// ensurePlacementRule — so the two are less different than they look.
type swaywm struct{}

func (swaywm) name() string { return "sway" }

func (swaywm) available(ctx context.Context) error {
	if _, err := exec.LookPath("swaymsg"); err != nil {
		return fmt.Errorf("%w: swaymsg not in PATH", ErrUnavailable)
	}
	if os.Getenv("SWAYSOCK") == "" {
		return fmt.Errorf("%w: SWAYSOCK is not set", ErrUnavailable)
	}
	if err := exec.CommandContext(ctx, "swaymsg", "-t", "get_version").Run(); err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	return nil
}

// swayNode is one node of `swaymsg -t get_tree`: root, output, workspace,
// split container or window, all described by the same shape.
type swayNode struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Num     int    `json:"num"`
	Focused bool   `json:"focused"`
	// PID is present only on views, which is what tells a window apart from
	// the split containers that hold it. A pointer, because absent and zero
	// have to stay distinguishable.
	PID              *int     `json:"pid"`
	AppID            *string  `json:"app_id"`
	Rect             swayRect `json:"rect"`
	WindowProperties *struct {
		Class    string `json:"class"`
		Instance string `json:"instance"`
	} `json:"window_properties"`
	Nodes         []swayNode `json:"nodes"`
	FloatingNodes []swayNode `json:"floating_nodes"`
}

type swayRect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func (n swayNode) isWindow() bool { return n.PID != nil }

// class is the name a window is recognised by. Wayland views carry an
// app_id; XWayland ones carry a WM_CLASS instead, which sway reports under
// window_properties. canaveral passes the same string as both when it spawns
// a terminal (`--class C,C`), so which one answers here does not change the
// result for a window canaveral opened — it matters only for the match_class
// windows it did not.
func (n swayNode) class() string {
	if n.AppID != nil && *n.AppID != "" {
		return *n.AppID
	}
	if n.WindowProperties != nil {
		if n.WindowProperties.Class != "" {
			return n.WindowProperties.Class
		}
		return n.WindowProperties.Instance
	}
	return ""
}

func (s swaywm) clients(ctx context.Context) ([]Client, error) {
	tree, err := s.tree(ctx)
	if err != nil {
		return nil, err
	}
	places := swayPlaces(tree)
	ids := make([]int, 0, len(places))
	for id := range places {
		ids = append(ids, id)
	}
	// swayPlaces is keyed by container id, so iterating it directly would
	// hand back a different order every call. sway hands out ids in
	// ascending order, so sorting by id is the tree's own creation order —
	// which is what ByClass's "keep the first match" rule needs to mean
	// "the original window" rather than "whichever one came up first".
	slices.Sort(ids)

	out := make([]Client, 0, len(ids))
	for _, id := range ids {
		p := places[id]
		class := p.node.class()
		c := Client{
			Address: strconv.Itoa(p.node.ID),
			// sway reports a single class, and it does not change after the
			// window maps. Both fields carry it so that ByClass and
			// ClassMatches work identically on either compositor.
			Class:        class,
			InitialClass: class,
			Title:        p.node.Name,
			PID:          *p.node.PID,
			At:           [2]int{p.node.Rect.X, p.node.Rect.Y},
			Size:         [2]int{p.node.Rect.Width, p.node.Rect.Height},
			Floating:     p.floating,
		}
		c.Workspace.ID = p.workspaceNum
		c.Workspace.Name = p.workspace
		out = append(out, c)
	}
	return out, nil
}

// swayPlace is where one window sits in the tree.
type swayPlace struct {
	node         swayNode
	workspace    string
	workspaceNum int
	parentID     int
	floating     bool
}

// swayPlaces indexes every window in the tree by container id, recording
// enough of its surroundings to reason about a row without walking again.
//
// Floating windows are included: a match_class application that opens
// floating is still the window canaveral is looking for.
func swayPlaces(root swayNode) map[int]swayPlace {
	places := map[int]swayPlace{}
	var walk func(n, parent, ws swayNode, floating bool)
	walk = func(n, parent, ws swayNode, floating bool) {
		if n.Type == "workspace" {
			ws = n
		}
		if n.isWindow() {
			places[n.ID] = swayPlace{
				node: n, workspace: ws.Name, workspaceNum: ws.Num,
				parentID: parent.ID, floating: floating,
			}
		}
		for _, c := range n.Nodes {
			walk(c, n, ws, floating)
		}
		for _, c := range n.FloatingNodes {
			walk(c, n, ws, true)
		}
	}
	walk(root, swayNode{}, swayNode{}, false)
	return places
}

func swayFindNode(n swayNode, id int) (swayNode, bool) {
	if n.ID == id {
		return n, true
	}
	for _, c := range n.Nodes {
		if found, ok := swayFindNode(c, id); ok {
			return found, true
		}
	}
	for _, c := range n.FloatingNodes {
		if found, ok := swayFindNode(c, id); ok {
			return found, true
		}
	}
	return swayNode{}, false
}

func (s swaywm) tree(ctx context.Context) (swayNode, error) {
	var root swayNode
	if err := swayJSON(ctx, &root, "get_tree"); err != nil {
		return swayNode{}, err
	}
	return root, nil
}

func (swaywm) monitors(ctx context.Context) ([]Monitor, error) {
	var outputs []struct {
		Name             string  `json:"name"`
		Active           bool    `json:"active"`
		Focused          bool    `json:"focused"`
		CurrentWorkspace *string `json:"current_workspace"`
	}
	if err := swayJSON(ctx, &outputs, "get_outputs"); err != nil {
		return nil, err
	}
	var out []Monitor
	for _, o := range outputs {
		// A disabled or disconnected output has no workspace and cannot be
		// focused, so it is not somewhere a feature can be put.
		if !o.Active {
			continue
		}
		m := Monitor{Name: o.Name, Focused: o.Focused}
		if o.CurrentWorkspace != nil {
			m.ActiveWorkspace.Name = *o.CurrentWorkspace
		}
		out = append(out, m)
	}
	return out, nil
}

// swayWorkspace is one entry of `swaymsg -t get_workspaces`.
//
// Num is sway's own reading of the name: the leading integer if the name
// starts with one, and -1 otherwise. Every canaveral workspace is named, so
// every canaveral workspace has a Num that RehomeTarget will skip.
type swayWorkspace struct {
	Num     int    `json:"num"`
	Name    string `json:"name"`
	Output  string `json:"output"`
	Focused bool   `json:"focused"`
}

func (swaywm) getWorkspaces(ctx context.Context) ([]swayWorkspace, error) {
	var ws []swayWorkspace
	if err := swayJSON(ctx, &ws, "get_workspaces"); err != nil {
		return nil, err
	}
	return ws, nil
}

func (s swaywm) workspaces(ctx context.Context) ([]Workspace, error) {
	ws, err := s.getWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Workspace, len(ws))
	for i, w := range ws {
		out[i] = Workspace{ID: w.Num, Name: w.Name, Monitor: w.Output}
	}
	return out, nil
}

func (s swaywm) activeWorkspaceName(ctx context.Context) (string, error) {
	ws, err := s.getWorkspaces(ctx)
	if err != nil {
		return "", err
	}
	for _, w := range ws {
		if w.Focused {
			return w.Name, nil
		}
	}
	return "", fmt.Errorf("sway reported no focused workspace")
}

func (s swaywm) nextFreeWorkspaceID(ctx context.Context) (int, error) {
	ws, err := s.getWorkspaces(ctx)
	if err != nil {
		return 0, err
	}
	max := 0
	for _, w := range ws {
		if w.Num > max {
			max = w.Num
		}
	}
	return max + 1, nil
}

// ensureRules keeps canaveral's own windows in the tiled layout.
//
// sway tiles by default, so this only matters for an XWayland window that
// asks to float — a browser started with --class, for instance. The class
// criterion matches XWayland windows and app_id the Wayland ones; sway is
// happy to be given a rule for a criterion no current window satisfies.
func (swaywm) ensureRules(ctx context.Context) error {
	return swayRun(ctx,
		`for_window [app_id="^canaveral-.*$"] floating disable`,
		`for_window [class="^canaveral-.*$"] floating disable`)
}

func (w swaywm) spawn(ctx context.Context, s SpawnSpec) error {
	// The whole launch goes in a file rather than on the command line: it
	// keeps the environment out of world-readable /proc/<pid>/cmdline, and it
	// keeps the command clear of the `;` and `,` sway's command parser splits
	// on. See writeSpawnScript.
	script, err := writeSpawnScript(s)
	if err != nil {
		return fmt.Errorf("spawn %s: %w", s.Class, err)
	}
	if err := w.ensurePlacement(ctx, s.Class, s.Workspace); err != nil {
		removeSpawnFile(script)
		return fmt.Errorf("spawn %s: %w", s.Class, err)
	}
	if err := swayRun(ctx, "exec sh "+script); err != nil {
		removeSpawnFile(script)
		return fmt.Errorf("spawn %s: %w", s.Class, err)
	}
	return nil
}

// ensurePlacement installs the rules that put a window on its feature's
// workspace without taking focus there.
//
// Both are matched on the window rather than on the process, because sway has
// nothing like Hyprland's exec-time rule to match a process with. Rules are
// installed before the spawn, since a window can map before swaymsg returns,
// and sway discards a duplicate for_window or no_focus rather than
// accumulating one per open (criteria_already_exists), so reinstalling on
// every reconcile is free.
func (swaywm) ensurePlacement(ctx context.Context, class, workspace string) error {
	if err := swaySafeCriteria(class); err != nil {
		return fmt.Errorf("window class %q: %w", class, err)
	}
	if err := swaySafeCriteria(workspace); err != nil {
		return fmt.Errorf("workspace %q: %w", workspace, err)
	}
	match := `"^` + regexp.QuoteMeta(class) + `$"`
	ws := strconv.Quote(workspace)
	return swayRun(ctx,
		`for_window [app_id=`+match+`] move container to workspace `+ws,
		`for_window [class=`+match+`] move container to workspace `+ws,
		`no_focus [app_id=`+match+`]`,
		`no_focus [class=`+match+`]`)
}

func (swaywm) focus(ctx context.Context, workspace string) error {
	cmd, err := swayWorkspaceCommand(workspace)
	if err != nil {
		return fmt.Errorf("focus %s: %w", workspace, err)
	}
	if err := swayRun(ctx, cmd); err != nil {
		return fmt.Errorf("focus %s: %w", workspace, err)
	}
	return nil
}

func (swaywm) closeWindow(ctx context.Context, address string) error {
	sel, err := swayConSelector(address)
	if err != nil {
		return err
	}
	return swayRun(ctx, sel+" kill")
}

// moveWorkspaceToMonitor reassigns a workspace's output.
//
// sway's `move workspace to output` acts on the focused workspace and has no
// criteria form, so a workspace the user is not looking at has to be focused
// first and the previous view put back afterwards. All three commands go in
// one request, so sway never renders the intermediate state.
func (s swaywm) moveWorkspaceToMonitor(ctx context.Context, workspace, monitor string) error {
	if err := swaySafeCriteria(monitor); err != nil {
		return fmt.Errorf("output %q: %w", monitor, err)
	}
	target, err := swayWorkspaceCommand(workspace)
	if err != nil {
		return fmt.Errorf("move workspace %s to %s: %w", workspace, monitor, err)
	}
	current, err := s.activeWorkspaceName(ctx)
	if err != nil {
		return fmt.Errorf("move workspace %s to %s: %w", workspace, monitor, err)
	}
	move := "move workspace to output " + strconv.Quote(monitor)
	if current == workspace {
		return swayRun(ctx, move)
	}
	back, err := swayWorkspaceCommand(current)
	if err != nil {
		return fmt.Errorf("move workspace %s to %s: %w", workspace, monitor, err)
	}
	if err := swayRun(ctx, target, move, back); err != nil {
		return fmt.Errorf("move workspace %s to %s: %w", workspace, monitor, err)
	}
	return nil
}

// releaseWorkspace switches any output still showing the named workspace to a
// fresh numbered one, so sway can reap the empty workspace behind it.
func (s swaywm) releaseWorkspace(ctx context.Context, workspace string) error {
	ms, err := s.monitors(ctx)
	if err != nil {
		return err
	}
	for _, m := range ms {
		if m.ActiveWorkspace.Name != workspace {
			continue
		}
		if err := swaySafeCriteria(m.Name); err != nil {
			return fmt.Errorf("release %s: output %q: %w", workspace, m.Name, err)
		}
		// Re-read per output: the previous iteration has just created a
		// workspace, so a number computed once would collide on the second.
		id, err := s.nextFreeWorkspaceID(ctx)
		if err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
		err = swayRun(ctx,
			"focus output "+strconv.Quote(m.Name),
			fmt.Sprintf("workspace --no-auto-back-and-forth number %d", id))
		if err != nil {
			return fmt.Errorf("release %s: %w", workspace, err)
		}
	}
	return nil
}

func (swaywm) moveWindowToWorkspace(ctx context.Context, address string, workspace int) error {
	sel, err := swayConSelector(address)
	if err != nil {
		return err
	}
	// `move container` does not follow the window, so tearing a feature down
	// does not drag the user's view along with it.
	err = swayRun(ctx, fmt.Sprintf("%s move container to workspace number %d", sel, workspace))
	if err != nil {
		return fmt.Errorf("move window %s to workspace %d: %w", address, workspace, err)
	}
	return nil
}

func (swaywm) moveWindowToNamedWorkspace(ctx context.Context, address, workspace string) error {
	sel, err := swayConSelector(address)
	if err != nil {
		return err
	}
	if err := swaySafeCriteria(workspace); err != nil {
		return fmt.Errorf("workspace %q: %w", workspace, err)
	}
	err = swayRun(ctx, sel+" move container to workspace "+strconv.Quote(workspace))
	if err != nil {
		return fmt.Errorf("move window %s to workspace %s: %w", address, workspace, err)
	}
	return nil
}

// swayWorkspaceCommand renders a switch to a workspace by name.
//
// --no-auto-back-and-forth matters because canaveral switches to workspaces
// the user may already be on — restoring a view, or opening a feature that is
// already in front of them. With workspace_auto_back_and_forth enabled, sway
// would read that as "go back" and send them somewhere else entirely.
func swayWorkspaceCommand(workspace string) (string, error) {
	if err := swaySafeCriteria(workspace); err != nil {
		return "", err
	}
	return "workspace --no-auto-back-and-forth " + strconv.Quote(workspace), nil
}

// swayConSelector renders an address as a criteria prefix. Addresses come
// from Clients, so this only fails on a hand-edited state file.
func swayConSelector(address string) (string, error) {
	id, err := strconv.Atoi(address)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("invalid sway window address %q", address)
	}
	return fmt.Sprintf("[con_id=%d]", id), nil
}

// swaySafeCriteria rejects a string that cannot be embedded in a sway
// command.
//
// sway splits a command list on `;` and `,` before the shell-like quoting is
// resolved, and its criteria are delimited by `[`, `]` and quotes, so any of
// these in a workspace or output name could turn one command into two.
// Whitespace is out as well: a for_window rule stores its command as text and
// rejoins it with single spaces (join_args), so a quoted argument containing
// one does not survive being stored. None of this can occur in a name
// canaveral generates — see manifest.NameRe — so this catches a hand-edited
// state file rather than trusting one.
func swaySafeCriteria(s string) error {
	if s == "" {
		return fmt.Errorf("must not be empty")
	}
	if strings.ContainsAny(s, ";,\"'\\[]") || strings.ContainsFunc(s, unicode.IsSpace) {
		return fmt.Errorf(`must not contain whitespace or any of ;,"'\[]`)
	}
	return nil
}

// swayJSON runs a swaymsg query and decodes its raw JSON reply into v.
func swayJSON(ctx context.Context, v any, msgType string) error {
	out, err := exec.CommandContext(ctx, "swaymsg", "-r", "-t", msgType).Output()
	if err != nil {
		return fmt.Errorf("swaymsg -t %s: %w", msgType, err)
	}
	if err := json.Unmarshal(out, v); err != nil {
		return fmt.Errorf("parse swaymsg -t %s: %w", msgType, err)
	}
	return nil
}

// swayRun sends commands as a single request and reports any sway refused.
//
// swaymsg exits non-zero when a command fails, but the reply is what says
// which one and why, and a command can report failure for reasons a caller
// should tolerate ("No matching node." for a window that closed in the
// meantime). Both are read so the error names the command rather than just
// the exit status.
func swayRun(ctx context.Context, commands ...string) error {
	payload := strings.Join(commands, "; ")
	out, err := exec.CommandContext(ctx, "swaymsg", "-r", "--", payload).Output()
	var replies []struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if jsonErr := json.Unmarshal(out, &replies); jsonErr != nil {
		if err != nil {
			return fmt.Errorf("swaymsg %s: %w", payload, err)
		}
		return fmt.Errorf("parse swaymsg reply to %s: %w", payload, jsonErr)
	}
	var failed []string
	for i, r := range replies {
		if r.Success {
			continue
		}
		cmd := payload
		if i < len(commands) {
			cmd = commands[i]
		}
		failed = append(failed, fmt.Sprintf("%s: %s", cmd, r.Error))
	}
	if len(failed) > 0 {
		return fmt.Errorf("sway rejected %s", strings.Join(failed, "; "))
	}
	if err != nil {
		return fmt.Errorf("swaymsg %s: %w", payload, err)
	}
	return nil
}
