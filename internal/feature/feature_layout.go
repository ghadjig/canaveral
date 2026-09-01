package feature

// Window and [layout] reconciliation: spawning declared windows, and,
// when [layout] is enabled, chaining them into an exact-ratio dwindle
// split via preselect/splitratio.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/worktree"
)

func reconcileWindows(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter, originalWS string, prog *progress) error {

	if len(m.Windows) == 0 {
		return nil
	}
	if err := hypr.Available(ctx); err != nil {
		r.Warn("skipping windows: %v", err)
		return nil
	}
	if err := hypr.EnsureRules(ctx); err != nil {
		r.Warn("%v", err)
	}

	clients, err := hypr.Clients(ctx)
	if err != nil {
		return err
	}
	open := hypr.ByClass(clients)
	base := baseEnvFor(m, f, tc)
	layoutFresh := isLayoutFresh(m, f, open)

	var records []state.Window
	pendingByName := map[string]pendingSpawn{}

	for _, w := range m.Windows {
		// Counted here rather than at spawn: this is the only loop that visits
		// every declared window exactly once (spawning is split between the
		// layout and free paths), and seeding a browser profile here is the
		// slow part regardless.
		prog.start("window " + w.Name)
		rec, pending, err := buildWindowSpec(ctx, m, f, w, vars, base, open, r)
		if err != nil {
			return err
		}
		records = append(records, rec)
		if pending != nil {
			pendingByName[w.Name] = *pending
		}
		prog.done()
	}

	spawnFreeWindows(ctx, m, pendingByName, res, r)

	if m.Layout.Enabled() {
		if err := reconcileLayoutWindows(ctx, m, f.HyprWorkspace(), pendingByName, layoutFresh, res, r, originalWS); err != nil {
			return fmt.Errorf("layout: %w", err)
		}
	}

	f.Windows = records
	return nil
}

// isLayoutFresh reports whether every window in [layout] is missing from
// the currently open windows.
//
// The split-ratio chain that gives [layout] its exact column widths only
// makes sense when every one of its windows is being created together:
// each ratio is relative to whichever windows are still undivided at that
// point in the chain, so inserting just one window into an already-tiled
// arrangement cannot reliably reproduce it. When some (but not all) layout
// windows already exist — a `reset` after closing just one of them, say —
// the missing one is still spawned, just via dwindle's ordinary placement
// instead of a re-derived chain.
func isLayoutFresh(m *manifest.Manifest, f *state.Feature, open map[string]hypr.Client) bool {
	if !m.Layout.Enabled() {
		return false
	}
	for _, name := range m.Layout.Order {
		if _, alive := open[hypr.Class(f.Project, f.Name, name)]; alive {
			return false
		}
	}
	return true
}

// buildWindowSpec resolves a single declared window's spawn spec (rendering
// its command template, seeding a browser profile if configured) and the
// state record to persist for it. pending is nil when the window is already
// open — detection is purely by the class canaveral assigns; matching
// anything looser risks adopting one of the user's own windows.
func buildWindowSpec(ctx context.Context, m *manifest.Manifest, f *state.Feature, w manifest.Window,
	vars tmpl.Vars, base map[string]string, open map[string]hypr.Client, r Reporter) (state.Window, *pendingSpawn, error) {

	class := hypr.Class(f.Project, f.Name, w.Name)

	profile, err := state.WindowProfile(f.Project, f.Name, w.Name)
	if err != nil {
		return state.Window{}, nil, err
	}
	wv := vars
	wv.Class, wv.Profile = class, profile

	cmd, err := tmpl.Render("window."+w.Name, w.Command(), wv)
	if err != nil {
		return state.Window{}, nil, err
	}
	dir := f.Worktree
	if w.Dir != "" {
		dir = serviceDir(f, m, w.Dir)
	}
	rec := state.Window{
		Name: w.Name, Class: class, Cmd: cmd, Dir: dir, Workspace: f.HyprWorkspace(),
	}

	if _, alive := open[class]; alive {
		return rec, nil, nil
	}

	if w.ProfileSource != "" {
		src, err := expandHome(w.ProfileSource)
		if err != nil {
			return rec, nil, fmt.Errorf("window %q: profile_source: %w", w.Name, err)
		}
		seed := worktree.Provision{Copy: w.ProfileSeed}
		if err := seed.Apply(ctx, src, profile, r.Info); err != nil {
			return rec, nil, fmt.Errorf("window %q: seeding profile: %w", w.Name, err)
		}
	}

	spec := hypr.SpawnSpec{
		Class:      class,
		Title:      f.Name + " · " + w.Name,
		Workspace:  f.HyprWorkspace(),
		Dir:        dir,
		IsTerminal: w.IsTerminal(),
		Terminal:   m.Terminal,
		Cmd:        cmd,
		Hold:       w.Hold,
		Env:        manifest.MergeEnv(base, m.Env),
	}
	return rec, &pendingSpawn{name: w.Name, spec: spec}, nil
}

// spawnFreeWindows spawns every pending window not managed by [layout]
// exactly as before: independently, in manifest order, no chaining.
func spawnFreeWindows(ctx context.Context, m *manifest.Manifest, pendingByName map[string]pendingSpawn, res *Result, r Reporter) {
	inOrder := map[string]bool{}
	for _, name := range m.Layout.Order {
		inOrder[name] = true
	}
	for _, w := range m.Windows {
		p, isPending := pendingByName[w.Name]
		if !isPending || inOrder[w.Name] {
			continue
		}
		if err := hypr.Spawn(ctx, p.spec); err != nil {
			r.Warn("window %s: %v", w.Name, err)
			continue
		}
		r.OK("window %s", w.Name)
		res.SpawnedWindow = append(res.SpawnedWindow, w.Name)
	}
}

// reconcileLayoutWindows spawns [layout]'s windows.
//
// When every one of them is missing (layoutFresh), they are spawned in Order
// with `preselect` chaining them into a single left-to-right dwindle split,
// then the exact ratios from splitRatioChain are applied. Otherwise, any
// still-missing ones are spawned without chaining (see reconcileWindows for
// why a partial chain cannot be reliably re-derived).
//
// Hyprland's splitratio and preselect dispatchers both operate on whichever
// window is currently focused, and focusing a window switches what is
// displayed — confirmed empirically — even though this whole process runs in
// the background by default and should not disturb whatever the user is
// actually looking at. Two things limit the damage: as soon as the
// workspace exists, it is relocated to any monitor other than the one the
// user is currently focused on (confirmed empirically that this neither
// changes what is shown on their monitor nor steals their keyboard focus —
// it only affects what the *other* monitor displays), so on a multi-monitor
// setup the user's own screen is never touched at all; and on a single
// monitor, where that is not possible, the original view is saved and
// restored once everything is done, and the spawn+preselect+splitratio
// sequence is structured to need as few focus switches as physically
// possible (one per window instead of two).
func reconcileLayoutWindows(ctx context.Context, m *manifest.Manifest, hyprWorkspace string,
	pending map[string]pendingSpawn, layoutFresh bool, res *Result, r Reporter, originalWS string) error {

	var missing []string
	for _, name := range m.Layout.Order {
		if _, ok := pending[name]; ok {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	restore := func() {
		if originalWS == "" {
			return
		}
		if cur, err := hypr.ActiveWorkspaceName(ctx); err == nil && cur != originalWS {
			_ = hypr.Focus(ctx, originalWS)
		}
	}
	defer restore()

	if !layoutFresh {
		spawnMissingIndependently(ctx, missing, pending, res, r)
		return nil
	}

	return spawnLayoutChain(ctx, m, hyprWorkspace, pending, res, r)
}

// spawnMissingIndependently spawns each still-missing layout window on its
// own, without the preselect chaining that builds an exact-ratio layout —
// used when the layout is not "fresh" (see reconcileLayoutWindows' doc for
// why a partial chain cannot be reliably re-derived).
func spawnMissingIndependently(ctx context.Context, missing []string, pending map[string]pendingSpawn, res *Result, r Reporter) {
	for _, name := range missing {
		p := pending[name]
		if err := hypr.Spawn(ctx, p.spec); err != nil {
			r.Warn("window %s: %v", name, err)
			continue
		}
		r.OK("window %s", name)
		res.SpawnedWindow = append(res.SpawnedWindow, name)
	}
}

// spawnLayoutChain builds [layout] from scratch: every window in Order is
// spawned in sequence, each preselected to open beside the previous one so
// the result is a single left-to-right dwindle split, then the exact
// ratios from splitRatioChain are applied.
func spawnLayoutChain(ctx context.Context, m *manifest.Manifest, hyprWorkspace string,
	pending map[string]pendingSpawn, res *Result, r Reporter) error {

	ratios := splitRatioChain(m.Layout.Order, m.Layout.Fractions())
	addresses := make(map[string]string, len(m.Layout.Order))
	for i, name := range m.Layout.Order {
		p := pending[name]
		if i > 0 {
			if err := chainAfter(ctx, addresses[m.Layout.Order[i-1]], m.Layout.Order[i-1], name); err != nil {
				return err
			}
		}
		if err := hypr.Spawn(ctx, p.spec); err != nil {
			return fmt.Errorf("window %q: %w", name, err)
		}
		r.OK("window %s", name)
		res.SpawnedWindow = append(res.SpawnedWindow, name)

		addr, err := waitForClass(ctx, p.spec.Class, 5*time.Second)
		if err != nil {
			return fmt.Errorf("window %q: %w", name, err)
		}
		addresses[name] = addr

		if i == 0 {
			// The workspace only comes into existence once this first window
			// creates it, so this is the first moment it can be relocated.
			// Doing so before any focus-shuffling begins means every
			// subsequent preselect/splitratio focus change plays out on a
			// monitor the user is not actively looking at, leaving whichever
			// one they are using completely undisturbed for the entire
			// operation — not just restored afterwards.
			relocateToSecondaryMonitor(ctx, hyprWorkspace, r)
		}

		// Applied here, immediately, rather than in a second pass over all
		// windows: the previous window is already focused right now (it was
		// focused a moment ago for preselect, and the spawn above was
		// silent, so focus never moved off it) — reusing that avoids a
		// second focus switch for the same window later, halving the total
		// number of visible workspace jumps this whole operation causes.
		if i > 0 {
			if err := hypr.SplitRatioExact(ctx, ratios[i-1]); err != nil {
				return fmt.Errorf("splitratio for %q: %w", m.Layout.Order[i-1], err)
			}
		}
	}
	return nil
}

// chainAfter focuses the previously-spawned window and preselects the
// direction the next spawn should open in, continuing the dwindle chain.
// Hyprland's preselect dispatcher operates on whichever window is currently
// focused, which is why this must run immediately before each spawn rather
// than once up front.
func chainAfter(ctx context.Context, prevAddr, prevName, nextName string) error {
	if prevAddr == "" {
		return fmt.Errorf("could not locate %q to chain %q after it", prevName, nextName)
	}
	if err := hypr.FocusWindow(ctx, prevAddr); err != nil {
		return fmt.Errorf("focus %q: %w", prevName, err)
	}
	if err := hypr.Preselect(ctx, "r"); err != nil {
		return fmt.Errorf("preselect after %q: %w", prevName, err)
	}
	return nil
}

// relocateToSecondaryMonitor moves the just-created workspace onto any
// monitor other than the one the user is currently focused on, confirmed
// empirically to neither change what is shown on their monitor nor steal
// their keyboard focus — it only affects what the *other* monitor displays.
// A no-op when there is no secondary monitor to use.
func relocateToSecondaryMonitor(ctx context.Context, hyprWorkspace string, r Reporter) {
	sec, ok, err := hypr.SecondaryMonitor(ctx)
	if err != nil || !ok {
		return
	}
	if err := hypr.MoveWorkspaceToMonitor(ctx, hyprWorkspace, sec.Name); err != nil {
		r.Warn("could not build on a secondary monitor: %v", err)
	}
}

// waitForClass polls for a window of the given class to appear, returning
// its address. Spawn's own exec dispatch returns before the window is
// necessarily mapped, and the next step (focusing it) needs its address.
func waitForClass(ctx context.Context, class string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		clients, err := hypr.Clients(ctx)
		if err == nil {
			for _, c := range clients {
				if c.InitialClass == class {
					return c.Address, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("window with class %q did not appear within %s", class, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// splitRatioChain converts a set of column fractions into the sequence of
// dwindle split ratios that produces them, one per split point (every window
// except the last, which simply occupies whatever remains).
//
// dwindle splits recursively: the first split separates order[0] from
// everything after it, the second separates order[1] from everything after
// that, and so on. So each ratio must be relative not to the whole width but
// to whatever fraction of it is still undivided at that point — order[i]'s
// share of order[i:] — which is why this cannot just use each fraction
// directly. The ratio-to-fraction relationship itself (fraction = ratio / 2)
// was confirmed empirically: it is not documented, and "ratio 1.0" is an even
// 50/50 split, not "the focused window gets 100%" as the name might suggest.
//
// Fractions do not need to sum to 1.0 — true of [layout.current], which
// reflects whatever a user actually resized one window to, not a recomputed
// partition of the rest — because each step only ever looks at the sum of
// the fractions from that point on, which naturally renormalizes as it goes.
func splitRatioChain(order []string, fractions map[string]float64) []float64 {
	if len(order) == 0 {
		return nil
	}
	remaining := 0.0
	for _, name := range order {
		remaining += fractions[name]
	}

	ratios := make([]float64, 0, len(order)-1)
	for i := 0; i < len(order)-1; i++ {
		f := fractions[order[i]]
		var ratio float64
		if remaining > 0 {
			ratio = 2 * f / remaining
		}
		// Dwindle's ratio range is roughly (0, 2); clamp defensively so a
		// pathological input (for example a current value hand-edited to
		// something silly) cannot send a nonsense value to hyprctl.
		if ratio < 0.1 {
			ratio = 0.1
		}
		if ratio > 1.9 {
			ratio = 1.9
		}
		ratios = append(ratios, ratio)
		remaining -= f
	}
	return ratios
}

// expandHome resolves a leading "~" to the current user's home directory, the
// way manifest.toml values commonly reference it (e.g. "~/.config/google-chrome").
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if p == "~" {
		return home, nil
	}
	return filepath.Join(home, p[2:]), nil
}
