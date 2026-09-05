// Package feature creates and reconciles feature workspaces.
//
// Reconcile is idempotent and is the single entry point for both creating a
// feature and repairing one: it brings up whatever is missing and leaves
// whatever is already healthy untouched. `canaveral new`, `canaveral
// <feature>` and `canaveral reset` differ only in whether they create the
// feature and whether they focus the workspace afterwards.
//
// Split across files by concern: this file holds the shared types and the
// top-level Reconcile/Remove orchestration; feature_worktree.go,
// feature_services.go, feature_agents.go and feature_layout.go each own one
// phase of Reconcile; feature_remove.go owns Remove's own phases.
// feature_layout.go is named that, not feature_windows.go, because Go treats
// any "_windows.go" suffix as a GOOS build constraint (built only on
// Windows) — it silently vanished from non-Windows builds under that name.
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
	// Resume maps agent name to an opencode session that agent's window
	// should reopen instead of starting a fresh conversation. Set by Pop
	// from the stash record; nil everywhere else.
	Resume map[string]string
}

// Result summarises what a reconcile pass changed.
type Result struct {
	Feature *state.Feature
	Created bool
	// Restored marks a pass that brought a stashed feature back, which the
	// CLI reports differently: a pop that started nothing because the
	// manifest declares nothing to start has still restored a workspace, and
	// "already up to date" would be describing the wrong event.
	Restored      bool
	StartedSvc    []string
	StartedAgent  []string
	SpawnedWindow []string

	// launched names every unit this pass asked systemd to start, recorded
	// before the request goes out so a launch that is interrupted mid-flight
	// is still known to have possibly happened. Only meaningful for undoing
	// an aborted pass; see abort.
	launched []string
}

// abort undoes a reconcile pass that was cut short by a signal.
//
// Only an interrupt triggers this. An ordinary failure — a ready probe that
// timed out, an agent that never printed a URL — leaves the healthy units of
// the pass running on purpose, because `canaveral reset` adopts them and
// re-running a five-minute application boot to recover from an unrelated
// error is worse than leaving it up. An interrupt is different: the user said
// stop, and services they never asked to keep would otherwise sit there
// holding the feature's ports.
func (res *Result) abort(ctx context.Context, r Reporter) {
	if ctx.Err() == nil || len(res.launched) == 0 {
		return
	}
	// Reverse start order, so dependents go down before their dependencies.
	names := make([]string, 0, len(res.launched))
	for i := len(res.launched) - 1; i >= 0; i-- {
		names = append(names, res.launched[i])
	}
	// unit.Stop deliberately ignores ctx's cancellation; that is the whole
	// reason this can run at all.
	failed := unit.StopAll(ctx, names)
	if n := len(names) - len(failed); n > 0 {
		r.OK("interrupted; stopped %d unit(s) started by this run", n)
	}
	if len(failed) > 0 {
		r.Warn("could not stop %s — run `canaveral prune`", strings.Join(failed, ", "))
	}
	res.StartedSvc, res.StartedAgent = nil, nil
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

	vars := varsFor(ctx, m, f, created, opt.Resume)
	tcMode := m.ToolchainMode()

	// Progress is published from here on. The record already exists, so a
	// watcher has a row to put it on; everything below this point is slow
	// enough to be worth reporting.
	prog := newProgress(f, state.PhaseBooting, reconcileSteps(m, opt))
	defer prog.finish()

	prog.start("worktree")
	if err := ensureWorktree(ctx, m, f, vars, opt, created, r); err != nil {
		return nil, err
	}
	prog.done()
	// The worktree may have just been created, so re-resolve the toolchain there.
	baseEnv, err := toolchain.Env(ctx, tcMode, f.Worktree)
	if err != nil {
		return nil, err
	}

	if err := state.Save(f); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	// Before services, so a precondition failure costs seconds and names
	// itself rather than surfacing as a readiness probe timing out.
	if m.Precheck != "" {
		prog.start("precheck")
	}
	if err := runPrecheck(ctx, m, f, vars, baseEnv, r); err != nil {
		return nil, err
	}
	if m.Precheck != "" {
		prog.done()
	}

	if !opt.NoServices {
		if err := reconcileServices(ctx, m, f, vars, baseEnv, res, r, prog); err != nil {
			res.abort(ctx, r)
			return nil, err
		}
	}
	if !opt.NoAgents {
		if err := reconcileAgents(ctx, m, f, vars, baseEnv, res, r, prog); err != nil {
			res.abort(ctx, r)
			return nil, err
		}
	}
	if err := state.Save(f); err != nil {
		return nil, fmt.Errorf("save state: %w", err)
	}

	if !opt.NoWindows {
		// Agent URLs are only known after agents start, so windows render last.
		vars = varsFor(ctx, m, f, res.Created, opt.Resume)
		if err := reconcileWindows(ctx, m, f, vars, baseEnv, res, r, originalWS, prog); err != nil {
			res.abort(ctx, r)
			return nil, err
		}
		if err := state.Save(f); err != nil {
			return nil, fmt.Errorf("save state: %w", err)
		}
	}
	return res, nil
}

// projectRoot resolves the project's main checkout.
//
// m.Root is wherever canaveral.toml was found by walking up from the cwd, so a
// command run inside a feature worktree finds the *provisioned* manifest and
// reports the worktree as the project. Storing that would make merge try to
// check the feature's own branch out over itself, so ask git for the real main
// checkout instead and only fall back to m.Root when git cannot say.
func projectRoot(ctx context.Context, m *manifest.Manifest) string {
	if root, err := worktree.MainCheckout(ctx, m.Root); err == nil {
		return root
	}
	return m.Root
}

// ensureRecord loads the feature or allocates a new slot, branch and ports.
func ensureRecord(ctx context.Context, m *manifest.Manifest, name string) (*state.Feature, bool, error) {
	if f, err := state.Load(m.Name, name); err == nil {
		// Ports and the database suffix follow the manifest, but the slot never
		// moves. Both are derived values: recomputing them is what lets a
		// newly declared service get a port, and what lets a project switch
		// [database] isolation without every feature that already exists
		// silently keeping the old answer.
		f.Ports = portsFor(m, f.Slot)
		f.DBSuffix = dbSuffixFor(m, name)
		f.Root = projectRoot(ctx, m)
		return f, false, nil
	}

	slot, err := state.AllocateSlot(m.Name, name)
	if err != nil {
		return nil, false, err
	}
	// [worktree] root lets a project keep its worktrees beside the code
	// instead of in canaveral's state directory.
	root, err := m.WorktreeRoot()
	if err != nil {
		return nil, false, err
	}
	wt, err := state.WorktreePathIn(root, m.Name, name)
	if err != nil {
		return nil, false, err
	}
	f := &state.Feature{
		Project:   m.Name,
		Name:      name,
		Root:      projectRoot(ctx, m),
		Slot:      slot,
		Worktree:  wt,
		Ports:     portsFor(m, slot),
		CreatedAt: time.Now(),
	}
	f.DBSuffix = dbSuffixFor(m, name)
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

// dbSuffixFor derives a feature's database suffix from the manifest and the
// feature's name, or "" when the project shares one database.
//
// The result is appended to a database name by the application's own config,
// so it has to be usable as an unquoted SQL identifier: leading underscore,
// then letters, digits and underscores only. A slugged feature name is
// already lowercase alphanumerics plus "-" and "/", so replacing those two is
// enough — but both must be replaced. Only "-" was, which meant a namespaced
// feature like "profile/working-hours" produced "_profile/working_hours" and
// a database name containing a slash.
//
// That did not fail loudly, which is why it survived. Postgres rejects such a
// name unless quoted, and Rails does not quote it — but MySQL accepts it and
// percent-encodes the slash on disk, so the databases really are created, as
// "norules_test_profile@002fworking_hours". The name then breaks anything that
// handles it as a path or as text: mysqldump output, a backup script, an S3
// key named after the database suddenly gaining a directory separator.
//
// Takes the manifest rather than a bool so the shared case is decided in one
// place. Callers recompute this on every load, so a project that switches
// isolation moves its existing features with it.
func dbSuffixFor(m *manifest.Manifest, name string) string {
	if m.Database.Isolation != manifest.DBSuffix {
		return ""
	}
	return "_" + strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, name)
}

// varsFor builds the template context for a feature.
//
// resume names, per agent, a session that agent's window should reopen rather
// than starting a new conversation — this feature's own, restored by
// `canaveral pop`. It takes precedence over forking a namespace sibling's:
// a popped feature already has its own history, and handing it a neighbour's
// instead would be a strictly worse answer to the same question.
func varsFor(ctx context.Context, m *manifest.Manifest, f *state.Feature, fresh bool, resume map[string]string) tmpl.Vars {
	agents := map[string]tmpl.AgentRef{}
	for _, a := range f.Agents {
		ref := tmpl.AgentRef{URL: a.URL}
		switch {
		case resume[a.Name] != "":
			ref = ref.WithSession(resume[a.Name])
		case fresh && a.Tool == "opencode" && a.URL != "":
			ref = ref.WithSession(forkedSessionFor(ctx, f.Project, f.Name, a.Name, a.URL, f.Worktree))
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

// baseEnvFor layers canaveral's own variables over the toolchain environment.
func baseEnvFor(m *manifest.Manifest, f *state.Feature, tc map[string]string) map[string]string {
	own := map[string]string{
		"CANAVERAL_PROJECT":  f.Project,
		"CANAVERAL_FEATURE":  f.Name,
		"CANAVERAL_WORKTREE": f.Worktree,
		"CANAVERAL_ROOT":     f.Root,
	}
	// Every process canaveral spawns — systemd --user units (services,
	// agents) and window terminals via hyprctl alike — starts from
	// canaveral's own PATH, since none of them runs through a shell that
	// would read an rc file. Launched from a Hyprland keybind or the
	// quickshell launcher, that is the compositor session's PATH, missing
	// whatever an rc file adds (~/.opencode/bin, ~/.cargo/bin, npm's global
	// bin).
	//
	// A toolchain PATH does not make this unnecessary, which is what the
	// first version of this got wrong: `mise env` derives its PATH by
	// prepending shims to the PATH *it* inherits, i.e. the same truncated
	// one, so under mise the gap survived and only showed up as a window
	// whose command was not on PATH vanishing the instant it spawned.
	// Merging is append-only, so mise's shims keep their precedence.
	if p := agent.ShellPATH(); p != "" {
		own["PATH"] = agent.MergePATH(tc["PATH"], p)
	}
	if f.DBSuffix != "" && m.Database.SuffixEnv != "" {
		own[m.Database.SuffixEnv] = f.DBSuffix
	}
	for name, p := range f.Ports {
		own["CANAVERAL_PORT_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))] = fmt.Sprint(p)
	}
	return manifest.MergeEnv(tc, own)
}

// envFor is baseEnvFor plus the manifest's [env], with its templates
// rendered for this feature.
//
// Rendering [env] is what lets a project state its isolation once, at the top
// of the manifest, instead of repeating it in every service:
//
//	[env]
//	DATABASE_URL = "postgres://localhost/app_test{{.DBSuffix}}"
//	REDIS_URL    = "redis://localhost:6379/{{.Slot}}"
//
// Every process canaveral starts for a feature is built from here — services,
// agents, window terminals, precheck, the setup hooks and `canaveral exec` —
// and that is the point. A spec run typed into a feature's own terminal has
// to reach the same database the service does, or the isolation only holds
// for some of the ways the code gets run, which is worse than not having it:
// it holds right up until the moment someone runs the suite by hand.
//
// [env] cannot reference {{.Agent...}}. Agent URLs are only known once agents
// have started, and services start before them, so such a value would resolve
// differently depending on which phase asked. missingkey=error makes that a
// startup failure rather than a silent inconsistency.
func envFor(m *manifest.Manifest, f *state.Feature, tc map[string]string, vars tmpl.Vars) (map[string]string, error) {
	rendered, err := tmpl.RenderMap("env", m.Env, vars)
	if err != nil {
		return nil, err
	}
	// [env] last: a project that wants to override one of canaveral's own
	// variables — a port, the database suffix variable — is entitled to.
	return manifest.MergeEnv(baseEnvFor(m, f, tc), rendered), nil
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

// EnvFor returns the environment a command run inside a feature's worktree
// should see: the project's resolved toolchain, canaveral's own variables
// (ports, worktree, project root) and the manifest's rendered [env].
//
// Exported so `canaveral exec` runs commands under exactly the same
// environment the feature's own services and agents get, rather than
// whatever the calling shell happens to have. That equivalence is what makes
// `canaveral exec <feature> bin/rspec` hit the feature's own database rather
// than the project's shared one.
func EnvFor(ctx context.Context, m *manifest.Manifest, f *state.Feature) (map[string]string, error) {
	tc, err := toolchain.Env(ctx, m.ToolchainMode(), f.Worktree)
	if err != nil {
		return nil, err
	}
	// fresh=false: agent URLs are not needed to render [env] — referencing
	// one there is an error anyway — and asking for them would mean an HTTP
	// round trip to every agent just to set up a shell command.
	return envFor(m, f, tc, varsFor(ctx, m, f, false, nil))
}
