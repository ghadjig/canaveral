package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tomledit"
)

// layoutEpsilon is how far a fraction has to move before it counts as an
// actual change worth writing to disk. Without this, floor/ceil pixel
// rounding on every ordinary workspace switch would rewrite canaveral.toml
// (and dirty it in git) even though nothing was really resized.
const layoutEpsilon = 0.005

// splitWorkspaceName splits canaveral's "project:feature" workspace naming
// convention. Anything else (a plain numbered workspace, a special
// workspace) is not ours to snapshot.
func splitWorkspaceName(ws string) (project, feature string, ok bool) {
	project, feature, ok = strings.Cut(ws, ":")
	if !ok || project == "" || feature == "" {
		return "", "", false
	}
	return project, feature, true
}

// snapshotLayoutOnLeave is called with the workspace that was just switched
// away from (name and Hyprland's own numeric ID for it). If it belongs to a
// canaveral feature whose project declares a [layout], the live on-screen
// size of each layout window is read back and, if it has actually changed,
// written into that project's canaveral.toml as [layout.current] — so the
// next feature opened for this project starts from what you last arranged,
// not the original default.
//
// This is triggered by workspace-change events because Hyprland's IPC has no
// window-resize or window-move event at all (verified empirically before
// building this); reacting when you leave a feature's workspace is the
// closest available approximation to "as soon as you change something".
//
// The workspace ID (not just its name) is checked against every matched
// window's current workspace ID before writing anything. Workspace names are
// reused across a remove-then-recreate of the same feature, but the ID is
// not, so this is what stops a snapshot for the old, already-torn-down
// instance from firing late and capturing a brand-new instance's windows
// instead — a real race hit while testing this against rapid rm+recreate.
func snapshotLayoutOnLeave(ctx context.Context, leftWorkspace string, leftID int, verbose bool) {
	project, feature, ok := splitWorkspaceName(leftWorkspace)
	if !ok {
		return
	}
	f, err := state.Load(project, feature)
	if err != nil {
		return // not a currently-known feature (already removed, or never one)
	}
	m, err := manifest.Load(f.Root)
	if err != nil || !m.Layout.Enabled() {
		return
	}

	clients, err := hypr.Clients(ctx)
	if err != nil {
		return
	}
	byClass := hypr.ByClass(clients)
	monitors, err := hypr.Monitors(ctx)
	if err != nil {
		return
	}

	values := make(map[string]float64, len(m.Layout.Order))
	for _, name := range m.Layout.Order {
		class := hypr.Class(project, feature, name)
		c, ok := byClass[class]
		if !ok {
			// A closed or never-opened window: its size tells us nothing, and
			// writing a partial snapshot would corrupt the other fractions'
			// meaning (they no longer sum to 1). Skip the whole snapshot.
			return
		}
		if c.Workspace.ID != leftID {
			// This class now belongs to a different (newer) instance of the
			// same feature than the one we were asked to snapshot.
			return
		}
		mon, ok := hypr.MonitorAt(monitors, c.At[0], c.At[1])
		if !ok {
			return
		}
		_, _, totalW, _ := mon.UsableArea()
		if totalW <= 0 {
			return
		}
		values[name] = float64(c.Size[0]) / float64(totalW)
	}

	if !layoutChanged(m.Layout.Fractions(), values) {
		return
	}

	path := filepath.Join(f.Root, manifest.FileName)
	if err := tomledit.ReplaceTable(path, "layout.current", m.Layout.Order, values, "layout.default"); err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "layout snapshot for %s: %v\n", leftWorkspace, err)
		}
		return
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "layout snapshot: updated %s for %s\n", path, leftWorkspace)
	}
}

// layoutChanged reports whether any fraction moved by more than layoutEpsilon.
func layoutChanged(prev, next map[string]float64) bool {
	if len(prev) != len(next) {
		return true
	}
	for name, v := range next {
		pv, ok := prev[name]
		if !ok || math.Abs(pv-v) > layoutEpsilon {
			return true
		}
	}
	return false
}

// activeWorkspace queries the currently active workspace's name and Hyprland
// ID once, used to seed hyprwatch's notion of "where we were" at startup.
func activeWorkspace(ctx context.Context) (name string, id int) {
	out, err := exec.CommandContext(ctx, "hyprctl", "activeworkspace", "-j").Output()
	if err != nil {
		return "", 0
	}
	var v struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", 0
	}
	return v.Name, v.ID
}
