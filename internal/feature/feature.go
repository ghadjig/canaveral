// Package feature creates and reconciles feature workspaces.
//
// Reconcile is idempotent and is the single entry point for both creating a
// feature and repairing one: it brings up whatever is missing and leaves
// whatever is already healthy untouched. `canaveral <feature>` and
// `canaveral reset` differ only in whether they focus the workspace afterwards.
package feature

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/probe"
	"github.com/bandito/canaveral/internal/skills"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/toolchain"
	"github.com/bandito/canaveral/internal/unit"
	"github.com/bandito/canaveral/internal/worktree"
)

// Reporter receives progress messages so the CLI controls presentation.
type Reporter interface {
	Step(format string, a ...any)
	OK(format string, a ...any)
	Info(format string, a ...any)
	Warn(format string, a ...any)
}

// Options tunes a reconcile pass.
type Options struct {
	// NoWindows skips the Hyprland layer, for headless use.
	NoWindows bool
	// NoServices skips starting services.
	NoServices bool
	// NoAgents skips starting agent servers.
	NoAgents bool
	// Base is the git ref new feature branches start from.
	Base string
}

// Result summarises what a reconcile pass changed.
type Result struct {
	Feature       *state.Feature
	Created       bool
	StartedSvc    []string
	StartedAgent  []string
	SpawnedWindow []string
}

// pendingSpawn is a window whose Spawn arguments have been fully prepared
// (templates rendered, profile seeded) but which has not been created yet.
type pendingSpawn struct {
	name string
	spec hypr.SpawnSpec
}

var slugRe = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// Slug normalises a user-supplied feature name.
//
// A "/" is preserved as a namespace separator, like a git ref (e.g.
// `canaveral onboarding/ask-for-name`) — grouping related features under a
// shared branch/worktree namespace and, more usefully, letting them share a
// namespace-scoped skill (see internal/skills) so an agent working on one
// doesn't start from zero on the next. Each segment is slugged
// independently; empty segments (from a leading, trailing, or doubled "/")
// are simply dropped rather than erroring.
func Slug(s string) string {
	parts := strings.Split(s, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		p = slugRe.ReplaceAllString(p, "-")
		p = strings.Trim(p, "-")
		if p != "" {
			segs = append(segs, p)
		}
	}
	return strings.Join(segs, "/")
}

// Namespace returns a slugged feature name's parent path — everything before
// the last "/" — or "" if it has none.
//
// Two features share a namespace, and therefore a namespace skill, only when
// their full parent path matches exactly: "onboarding/a" and
// "onboarding/b/c" do not share one, even though both start with
// "onboarding". Nesting more deeply just makes the shared skill more
// specific, the same way a deeper git ref namespace does.
func Namespace(name string) string {
	i := strings.LastIndex(name, "/")
	if i < 0 {
		return ""
	}
	return name[:i]
}

// Reconcile brings a feature to its declared state, creating it if needed.
func Reconcile(ctx context.Context, m *manifest.Manifest, name string, opt Options, r Reporter) (*Result, error) {
	name = Slug(name)
	if name == "" {
		return nil, fmt.Errorf("feature name is empty")
	}
	if err := unit.Available(ctx); err != nil {
		return nil, err
	}

	// Captured before anything else runs, not right before the window step:
	// service and agent startup alone can take several seconds, and on a
	// machine actually being used, the visible workspace can easily have
	// changed by the time a later capture would see it — restoring to that
	// later, already-stale value would silently overwrite wherever the user
	// had legitimately moved on to in the meantime.
	originalWS, _ := hypr.ActiveWorkspaceName(ctx)

	f, created, err := ensureRecord(ctx, m, name)
	if err != nil {
		return nil, err
	}
	res := &Result{Feature: f, Created: created}

	vars := varsFor(ctx, m, f, created)
	tcMode := m.ToolchainMode()

	if err := ensureWorktree(ctx, m, f, vars, opt, created, r); err != nil {
		return nil, err
	}
	// The worktree may have just been created, so re-resolve the toolchain there.
	baseEnv, err := toolchain.Env(ctx, tcMode, f.Worktree)
	if err != nil {
		return nil, err
	}

	if err := state.Save(f); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	if !opt.NoServices {
		if err := reconcileServices(ctx, m, f, vars, baseEnv, res, r); err != nil {
			return nil, err
		}
	}
	if !opt.NoAgents {
		if err := reconcileAgents(ctx, m, f, vars, baseEnv, res, r); err != nil {
			return nil, err
		}
	}
	if err := state.Save(f); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	if !opt.NoWindows {
		// Agent URLs are only known after agents start, so windows render last.
		vars = varsFor(ctx, m, f, res.Created)
		if err := reconcileWindows(ctx, m, f, vars, baseEnv, res, r, originalWS); err != nil {
			return nil, err
		}
		if err := state.Save(f); err != nil {
			return nil, fmt.Errorf("save state: %w", err)
		}
	}
	return res, nil
}

// ensureRecord loads the feature or allocates a new slot, branch and ports.
func ensureRecord(ctx context.Context, m *manifest.Manifest, name string) (*state.Feature, bool, error) {
	if f, err := state.Load(m.Name, name); err == nil {
		// Ports follow the manifest so newly declared services get one, but the
		// slot never moves.
		f.Ports = portsFor(m, f.Slot)
		f.Root = m.Root
		return f, false, nil
	}

	slot, err := state.AllocateSlot(m.Name, name)
	if err != nil {
		return nil, false, err
	}
	wt, err := state.WorktreePath(m.Name, name)
	if err != nil {
		return nil, false, err
	}
	f := &state.Feature{
		Project:   m.Name,
		Name:      name,
		Root:      m.Root,
		Slot:      slot,
		Worktree:  wt,
		Ports:     portsFor(m, slot),
		CreatedAt: time.Now(),
	}
	if m.Database.Isolation == manifest.DBSuffix {
		f.DBSuffix = "_" + strings.ReplaceAll(name, "-", "_")
	}
	branch, err := worktree.RenderBranch(m.Branch, worktree.BranchVars{
		Workspace: m.Name, Feature: name, Agent: name,
	})
	if err != nil {
		return nil, false, err
	}
	f.Branch = branch
	return f, true, nil
}

// portsFor derives this feature's ports from the manifest bases and its slot.
func portsFor(m *manifest.Manifest, slot int) map[string]int {
	if len(m.Ports) == 0 {
		return nil
	}
	out := make(map[string]int, len(m.Ports))
	for name, base := range m.Ports {
		out[name] = base + slot
	}
	return out
}

func varsFor(ctx context.Context, m *manifest.Manifest, f *state.Feature, fresh bool) tmpl.Vars {
	agents := map[string]tmpl.AgentRef{}
	for _, a := range f.Agents {
		ref := tmpl.AgentRef{URL: a.URL}
		if fresh && a.Tool == "opencode" {
			ref.Fork = forkArgsFor(ctx, f.Project, f.Name, a.Name)
		}
		agents[a.Name] = ref
	}
	return tmpl.Vars{
		Project:  m.Name,
		Feature:  f.Name,
		Slot:     f.Slot,
		Branch:   f.Branch,
		Worktree: f.Worktree,
		Root:     m.Root,
		Port:     f.Ports,
		URL:      tmpl.URLsFor(f.Ports),
		Agent:    agents,
		DBSuffix: f.DBSuffix,
	}
}

// forkArgsFor returns "--session <id> --fork" for an opencode agent when a
// namespace sibling has a more recently active session to hand off, or ""
// when there's nothing to fork from — no namespace, no sibling, or none has
// an opencode session on record yet.
//
// Checks two sources and takes whichever is newer: skills.LatestSession,
// which survives a sibling being removed (recorded in Remove, below), and
// every currently-existing sibling's live agent server, for the case where
// it's still running and has moved on since that was last recorded.
//
// Only ever consulted when the feature itself was just created (varsFor's
// fresh argument) — once a feature has its own session building up,
// re-forking on every later reset would silently discard it in favour of a
// sibling's, possibly stale, history.
func forkArgsFor(ctx context.Context, project, name, agentName string) string {
	ns := Namespace(name)
	if ns == "" {
		return ""
	}

	var best skills.SessionRecord
	have := false
	if rec, ok, err := skills.LatestSession(project, ns, agentName); err == nil && ok {
		best, have = rec, true
	}

	siblings, err := state.List(project)
	if err == nil {
		for _, sib := range siblings {
			if sib == name || Namespace(sib) != ns {
				continue
			}
			sf, err := state.Load(project, sib)
			if err != nil {
				continue
			}
			a, ok := sf.Agent(agentName)
			if !ok || a.URL == "" {
				continue
			}
			h := agent.Probe(ctx, a.URL, sf.Worktree)
			if !h.Reachable || h.SessionID == "" {
				continue
			}
			if !have || h.Updated.After(best.UpdatedAt) {
				best = skills.SessionRecord{Feature: sib, SessionID: h.SessionID, UpdatedAt: h.Updated}
				have = true
			}
		}
	}

	if !have {
		return ""
	}
	return fmt.Sprintf("--session %s --fork", best.SessionID)
}

// baseEnvFor layers canaveral's own variables over the toolchain environment.
func baseEnvFor(m *manifest.Manifest, f *state.Feature, tc map[string]string) map[string]string {
	own := map[string]string{
		"CANAVERAL_PROJECT":  f.Project,
		"CANAVERAL_FEATURE":  f.Name,
		"CANAVERAL_WORKTREE": f.Worktree,
		"CANAVERAL_ROOT":     f.Root,
	}
	if f.DBSuffix != "" && m.Database.SuffixEnv != "" {
		own[m.Database.SuffixEnv] = f.DBSuffix
	}
	for name, p := range f.Ports {
		own["CANAVERAL_PORT_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))] = fmt.Sprint(p)
	}
	return manifest.MergeEnv(tc, own)
}

func ensureWorktree(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, opt Options, created bool, r Reporter) error {

	if !worktree.IsRepo(ctx, m.Root) {
		return fmt.Errorf("%s is not a git repository; canaveral needs one to create feature worktrees", m.Root)
	}
	res, err := worktree.Ensure(ctx, m.Root, f.Worktree, f.Branch, opt.Base)
	if err != nil {
		return err
	}
	if !res.Created {
		if created {
			r.Info("reusing existing worktree %s", f.Worktree)
		}
		return nil
	}
	r.OK("worktree %s on %s", f.Worktree, f.Branch)

	tc, err := toolchain.Env(ctx, m.ToolchainMode(), f.Worktree)
	if err != nil {
		return err
	}
	env := baseEnvFor(m, f, tc)

	setup, err := tmpl.Render("worktree.setup", m.Worktree.Setup, vars)
	if err != nil {
		return err
	}
	f.Provisioned = append(append([]string{}, m.Worktree.Copy...), manifest.FileName)
	prov := worktree.Provision{
		Link: m.Worktree.Link,
		// The manifest is copied so the worktree is self-describing even when
		// canaveral.toml is untracked and therefore absent from the checkout.
		Copy:         f.Provisioned,
		Setup:        setup,
		SetupTimeout: m.Worktree.SetupTimeout.Duration,
		Env:          manifest.MergeEnv(env, m.Env),
	}
	if err := prov.Apply(ctx, m.Root, f.Worktree, r.Info); err != nil {
		return err
	}

	if ns := Namespace(f.Name); ns != "" {
		rel, linked, err := skills.Link(f.Worktree, f.Project, ns)
		if err != nil {
			r.Warn("namespace skill: %v", err)
		} else {
			f.Provisioned = append(f.Provisioned, rel)
			if linked {
				r.OK("linked namespace skill %s", ns)
			}
		}
	}

	// Database setup runs after provisioning so .env and credentials exist.
	dbSetup, err := tmpl.Render("database.setup", m.Database.Setup, vars)
	if err != nil {
		return err
	}
	if strings.TrimSpace(dbSetup) != "" {
		if f.DBSuffix != "" {
			r.Info("preparing databases (suffix %s)", f.DBSuffix)
		} else {
			r.Info("preparing databases")
		}
		db := worktree.Provision{
			Setup:        dbSetup,
			SetupTimeout: m.Database.SetupTimeout.Duration,
			Env:          manifest.MergeEnv(env, m.Env),
		}
		if err := db.Apply(ctx, m.Root, f.Worktree, r.Info); err != nil {
			return err
		}
	}
	return nil
}

func reconcileServices(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter) error {

	base := baseEnvFor(m, f, tc)
	var records []state.Service

	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		return err
	}

	for _, s := range m.Services {
		unitName := unit.Name(f.Project+"-"+f.Name, "svc", s.Name)
		logPath := filepath.Join(logDir, "svc-"+s.Name+".log")

		dir := serviceDir(f, m, s.Dir)
		cmd, err := tmpl.Render("service."+s.Name+".cmd", s.Cmd, vars)
		if err != nil {
			return err
		}
		rec := state.Service{
			Name: s.Name, Unit: unitName, Dir: dir, Cmd: cmd,
			LogPath: logPath, Optional: s.Optional,
		}

		if st, err := unit.Query(ctx, unitName); err == nil && st.Running() {
			records = append(records, rec)
			continue
		}

		svcEnv, err := tmpl.RenderMap("service."+s.Name+".env", s.Env, vars)
		if err != nil {
			return err
		}
		ready, err := renderReady(s.Name, s.Ready, vars)
		if err != nil {
			return err
		}

		unit.Reset(ctx, unitName)
		r.Step("service %s  %s", s.Name, cmd)
		if err := unit.Start(ctx, unit.Spec{
			Name:        unitName,
			Description: fmt.Sprintf("canaveral %s/%s service %s", f.Project, f.Name, s.Name),
			Dir:         dir,
			Cmd:         cmd,
			Env:         manifest.MergeEnv(base, m.Env, svcEnv),
			LogPath:     logPath,
		}); err != nil {
			if s.Optional {
				r.Warn("optional service %s: %v", s.Name, err)
				continue
			}
			return err
		}

		if k := ready.Kind(); k != "" {
			if err := probe.Wait(ctx, ready, dir, logPath, aliveCheck(ctx, unitName, logPath)); err != nil {
				_ = unit.Stop(ctx, unitName)
				unit.Reset(ctx, unitName)
				if s.Optional {
					r.Warn("optional service %s: %v", s.Name, err)
					continue
				}
				return fmt.Errorf("service %q: %w", s.Name, err)
			}
			r.OK("service %s ready", s.Name)
		} else {
			r.OK("service %s started", s.Name)
		}
		res.StartedSvc = append(res.StartedSvc, s.Name)
		records = append(records, rec)
	}
	f.Services = records
	return nil
}

// serviceDir keeps sub-directory layouts intact inside the worktree.
func serviceDir(f *state.Feature, m *manifest.Manifest, dir string) string {
	full := manifest.ResolveDir(m.Root, dir)
	rel, err := filepath.Rel(m.Root, full)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		if rel == "." {
			return f.Worktree
		}
		return full
	}
	return filepath.Join(f.Worktree, rel)
}

func renderReady(name string, ready manifest.Ready, vars tmpl.Vars) (manifest.Ready, error) {
	var err error
	if ready.HTTP, err = tmpl.Render("service."+name+".ready.http", ready.HTTP, vars); err != nil {
		return ready, err
	}
	if ready.TCP, err = tmpl.Render("service."+name+".ready.tcp", ready.TCP, vars); err != nil {
		return ready, err
	}
	if ready.Cmd, err = tmpl.Render("service."+name+".ready.cmd", ready.Cmd, vars); err != nil {
		return ready, err
	}
	return ready, nil
}

func reconcileAgents(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter) error {

	if len(m.Agents) == 0 {
		return nil
	}
	bin, err := agent.Resolve()
	if err != nil {
		return err
	}
	base := baseEnvFor(m, f, tc)
	logDir, err := state.LogDir(f.Project, f.Name)
	if err != nil {
		return err
	}

	var records []state.Agent
	for _, a := range m.Agents {
		unitName := unit.Name(f.Project+"-"+f.Name, "agent", a.Name)
		logPath := filepath.Join(logDir, "agent-"+a.Name+".log")
		dir := serviceDir(f, m, a.Dir)

		rec := state.Agent{
			Name: a.Name, Tool: a.Tool, Unit: unitName, Dir: dir, LogPath: logPath,
		}
		if st, err := unit.Query(ctx, unitName); err == nil && st.Running() {
			if prev, ok := f.Agent(a.Name); ok {
				rec.URL, rec.Port = prev.URL, prev.Port
			}
			if rec.URL == "" {
				// The unit is alive but we lost its URL; recover it from the log.
				if u, err := agent.DiscoverURL(ctx, logPath, 5*time.Second, nil); err == nil {
					rec.URL, rec.Port = u, portOf(u)
				}
			}
			records = append(records, rec)
			continue
		}

		agentEnv, err := tmpl.RenderMap("agent."+a.Name+".env", a.Env, vars)
		if err != nil {
			return err
		}
		env := manifest.MergeEnv(base, m.Env, agentEnv)
		if a.Model != "" {
			env["OPENCODE_MODEL"] = a.Model
		}
		if a.Agent != "" {
			env["OPENCODE_AGENT"] = a.Agent
		}

		unit.Reset(ctx, unitName)
		r.Step("agent %s", a.Name)
		if err := unit.Start(ctx, unit.Spec{
			Name:        unitName,
			Description: fmt.Sprintf("canaveral %s/%s agent %s", f.Project, f.Name, a.Name),
			Dir:         dir,
			Cmd:         agent.ServeCmd(bin),
			Env:         env,
			LogPath:     logPath,
		}); err != nil {
			return err
		}
		url, err := agent.DiscoverURL(ctx, logPath, 45*time.Second, aliveCheck(ctx, unitName, logPath))
		if err != nil {
			return fmt.Errorf("agent %q: %w", a.Name, err)
		}
		rec.URL, rec.Port = url, portOf(url)
		r.OK("agent %s listening on %s", a.Name, url)
		res.StartedAgent = append(res.StartedAgent, a.Name)
		records = append(records, rec)
	}
	f.Agents = records
	return nil
}

func reconcileWindows(ctx context.Context, m *manifest.Manifest, f *state.Feature,
	vars tmpl.Vars, tc map[string]string, res *Result, r Reporter, originalWS string) error {

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

	// The split-ratio chain that gives [layout] its exact column widths only
	// makes sense when every one of its windows is being created together:
	// each ratio is relative to whichever windows are still undivided at
	// that point in the chain, so inserting just one window into an already-
	// tiled arrangement cannot reliably reproduce it. When some (but not
	// all) layout windows already exist — a `reset` after closing just one
	// of them, say — the missing one is still spawned, just via dwindle's
	// ordinary placement instead of a re-derived chain.
	layoutFresh := m.Layout.Enabled()
	if layoutFresh {
		for _, name := range m.Layout.Order {
			if _, alive := open[hypr.Class(f.Project, f.Name, name)]; alive {
				layoutFresh = false
				break
			}
		}
	}

	var records []state.Window
	pendingByName := map[string]pendingSpawn{}

	for _, w := range m.Windows {
		class := hypr.Class(f.Project, f.Name, w.Name)

		profile, err := state.WindowProfile(f.Project, f.Name, w.Name)
		if err != nil {
			return err
		}
		wv := vars
		wv.Class, wv.Profile = class, profile

		cmd, err := tmpl.Render("window."+w.Name, w.Command(), wv)
		if err != nil {
			return err
		}
		dir := f.Worktree
		if w.Dir != "" {
			dir = serviceDir(f, m, w.Dir)
		}
		records = append(records, state.Window{
			Name: w.Name, Class: class, Cmd: cmd, Dir: dir, Workspace: f.HyprWorkspace(),
		})

		// Detection is purely by the class canaveral assigns. Matching anything
		// looser risks adopting one of the user's own windows.
		if _, alive := open[class]; alive {
			continue
		}

		if w.ProfileSource != "" {
			src, err := expandHome(w.ProfileSource)
			if err != nil {
				return fmt.Errorf("window %q: profile_source: %w", w.Name, err)
			}
			seed := worktree.Provision{Copy: w.ProfileSeed}
			if err := seed.Apply(ctx, src, profile, r.Info); err != nil {
				return fmt.Errorf("window %q: seeding profile: %w", w.Name, err)
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
		pendingByName[w.Name] = pendingSpawn{name: w.Name, spec: spec}
	}

	// Windows not managed by [layout] spawn exactly as before: independently,
	// in manifest order, no chaining.
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

	if m.Layout.Enabled() {
		if err := reconcileLayoutWindows(ctx, m, f.HyprWorkspace(), pendingByName, layoutFresh, res, r, originalWS); err != nil {
			return fmt.Errorf("layout: %w", err)
		}
	}

	f.Windows = records
	return nil
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
		for _, name := range missing {
			p := pending[name]
			if err := hypr.Spawn(ctx, p.spec); err != nil {
				r.Warn("window %s: %v", name, err)
				continue
			}
			r.OK("window %s", name)
			res.SpawnedWindow = append(res.SpawnedWindow, name)
		}
		return nil
	}

	ratios := splitRatioChain(m.Layout.Order, m.Layout.Fractions())
	addresses := make(map[string]string, len(m.Layout.Order))
	for i, name := range m.Layout.Order {
		p := pending[name]
		if i > 0 {
			prevAddr := addresses[m.Layout.Order[i-1]]
			if prevAddr == "" {
				return fmt.Errorf("could not locate %q to chain %q after it", m.Layout.Order[i-1], name)
			}
			if err := hypr.FocusWindow(ctx, prevAddr); err != nil {
				return fmt.Errorf("focus %q: %w", m.Layout.Order[i-1], err)
			}
			if err := hypr.Preselect(ctx, "r"); err != nil {
				return fmt.Errorf("preselect after %q: %w", m.Layout.Order[i-1], err)
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
			if sec, ok, err := hypr.SecondaryMonitor(ctx); err == nil && ok {
				if err := hypr.MoveWorkspaceToMonitor(ctx, hyprWorkspace, sec.Name); err != nil {
					r.Warn("could not build on a secondary monitor: %v", err)
				}
			}
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

func aliveCheck(ctx context.Context, unitName, logPath string) probe.Alive {
	return func() error {
		st, err := unit.Query(ctx, unitName)
		if err != nil {
			// systemd garbage-collects transient units once they go inactive, so
			// "not found" means the process exited rather than never started.
			return fmt.Errorf("process exited immediately\n%s", tailIndent(logPath, 15))
		}
		if st.Running() {
			return nil
		}
		return fmt.Errorf("process exited (%s/%s)\n%s",
			st.ActiveState, st.SubState, tailIndent(logPath, 15))
	}
}

func tailIndent(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "    (no log available)"
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}

var portRe = regexp.MustCompile(`:(\d+)/?$`)

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

func portOf(url string) int {
	mm := portRe.FindStringSubmatch(strings.TrimSuffix(url, "/"))
	if mm == nil {
		return 0
	}
	var p int
	fmt.Sscanf(mm[1], "%d", &p)
	return p
}

// Remove tears a feature down: units, windows, worktree and state.
func Remove(ctx context.Context, f *state.Feature, keepWorktree, force bool, r Reporter) error {
	// Best-effort: if this feature is namespaced, remember its newest
	// opencode session before tearing down its agent, so a later sibling
	// under the same namespace can still fork from it even though this
	// feature's own state (and therefore any record of it) is about to be
	// deleted. Recorded here rather than continuously because this is the
	// last point at which the agent is guaranteed to still be reachable.
	if ns := Namespace(f.Name); ns != "" {
		for _, a := range f.Agents {
			if a.Tool != "opencode" || a.URL == "" {
				continue
			}
			h := agent.Probe(ctx, a.URL, f.Worktree)
			if h.Reachable && h.SessionID != "" {
				_ = skills.RecordSession(f.Project, ns, a.Name, skills.SessionRecord{
					Feature: f.Name, SessionID: h.SessionID, UpdatedAt: h.Updated,
				})
			}
		}
	}

	for _, a := range f.Agents {
		_ = unit.Stop(ctx, a.Unit)
		unit.Reset(ctx, a.Unit)
	}
	for i := len(f.Services) - 1; i >= 0; i-- {
		_ = unit.Stop(ctx, f.Services[i].Unit)
		unit.Reset(ctx, f.Services[i].Unit)
	}
	r.OK("stopped %d service(s), %d agent(s)", len(f.Services), len(f.Agents))

	if err := hypr.Available(ctx); err == nil && len(f.Windows) > 0 {
		if clients, err := hypr.Clients(ctx); err == nil {
			open := hypr.ByClass(clients)
			closed := 0
			for _, w := range f.Windows {
				if c, ok := open[w.Class]; ok {
					if err := hypr.Close(ctx, c.Address); err == nil {
						closed++
					}
				}
			}
			if closed > 0 {
				r.OK("closed %d window(s)", closed)
			}
		}
		// Hyprland won't destroy a workspace while it's a monitor's active
		// one, even with zero windows left (confirmed empirically) — so
		// closing the last window above can leave the feature's workspace
		// dangling forever on whatever monitor last displayed it, as a
		// phantom entry in the waybar pill list. Release it explicitly.
		if err := hypr.ReleaseWorkspace(ctx, f.HyprWorkspace()); err != nil {
			r.Warn("%v", err)
		}
	}

	if !keepWorktree && f.Worktree != "" {
		if err := worktree.Remove(ctx, f.Root, f.Worktree, force, f.Provisioned); err != nil {
			return err
		}
		_ = worktree.Prune(ctx, f.Root)
		// The worktree is disposable; the branch holds the work and is kept.
		r.OK("removed worktree, kept branch %s", f.Branch)
	}
	return state.Remove(f.Project, f.Name)
}
