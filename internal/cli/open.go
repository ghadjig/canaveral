package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/config"
	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/registry"
	"github.com/bandito/canaveral/internal/space"
	"github.com/bandito/canaveral/internal/state"
)

// loadManifest finds the project manifest from the current directory.
func loadManifest() (*manifest.Manifest, error) {
	root, err := manifest.Find(".")
	if err != nil {
		return nil, err
	}
	m, err := manifest.Load(root)
	if err != nil {
		return nil, err
	}
	recordProject(m)
	return m, nil
}

// recordProject keeps the global project registry current as a side effect of
// ordinary use: touch a project once and the launcher can address it by name
// from anywhere, forever after. This sits here rather than in a registration
// command because every project resolution in canaveral funnels through
// loadManifest, and an index nobody has to maintain is the only kind that stays
// accurate.
//
// Failures are advisory — no command should stop working because an index could
// not be updated — with one exception. A name conflict means two checkouts
// already share <state>/features/<name>/, and therefore each other's features;
// that is a real problem only the user can resolve, so it is said out loud. On
// stderr, so the machine-readable stdout of `status --json` and `watch` is
// unaffected.
func recordProject(m *manifest.Manifest) {
	if err := registry.Record(m.Name, m.Root); errors.Is(err, registry.ErrConflict) {
		fmt.Fprintf(os.Stderr, "canaveral: %v\n", err)
		fmt.Fprintf(os.Stderr, "canaveral: both share feature state; rename one in its %s\n", manifest.FileName)
	}
}

func runNew(ctx context.Context, args []string) error {
	return openFeature(ctx, "new", args, true, false)
}

// runOpen is the explicit `canaveral open <feature>`. It never resolves a
// space: it is one of the two unambiguous escapes from a name that means
// both things, the other being `canaveral space open <name>`.
func runOpen(ctx context.Context, args []string) error {
	return openFeature(ctx, "open", args, false, false)
}

// runBare handles `canaveral <name>` — the form with no verb at all, which
// means "give me this workspace" whichever kind it turns out to be.
func runBare(ctx context.Context, args []string) error {
	return openFeature(ctx, "open", args, false, true)
}

// openFeature reconciles a feature, creating it only when asked to.
//
// Creation is gated behind `canaveral new` because bare dispatch treats any
// unrecognised word as a feature name, and the cost of a typo was wildly
// asymmetric: `canaveral stratus` silently built a worktree, a branch, a
// server and an agent, which then had to be found and torn down. Bare
// dispatch now only ever opens something that already exists, so a mistyped
// command fails in the one way you want it to — immediately and without side
// effects.
func openFeature(ctx context.Context, verb string, args []string, create, allowSpace bool) error {
	fs := flag.NewFlagSet(verb, flag.ContinueOnError)
	fs.Usage = func() {
		if create {
			fmt.Fprintln(os.Stderr, "Usage: canaveral new <feature> [flags]\n\nCreate a feature workspace — worktree, branch, services, agent and windows —\nin the background, without switching your view to it. Pass --focus to jump\nthere once it's ready.\n\nFlags:")
		} else {
			fmt.Fprintln(os.Stderr, "Usage: canaveral <feature> [flags]\n\nReconcile an existing feature, bringing up whatever is missing, without\nswitching your view to it. Pass --focus to jump there once it's ready.\nUse `canaveral new <feature>` to create one.\n\nFlags:")
		}
		fs.PrintDefaults()
	}
	var (
		noWindows  = fs.Bool("no-windows", false, "skip spawning windows")
		noServices = fs.Bool("no-services", false, "skip starting services")
		noAgents   = fs.Bool("no-agents", false, "skip starting agents")
		focus      = fs.Bool("focus", false, "switch to the workspace once everything is ready")
		base       = fs.String("base", "", "base ref for a new feature branch (default: current HEAD)")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		if create {
			return fmt.Errorf("specify a feature name, e.g. `canaveral new small-fixes`")
		}
		return fmt.Errorf("specify a feature name, e.g. `canaveral small-fixes`")
	}
	if len(pos) > 1 {
		return fmt.Errorf("expected one feature name, got %d: %s", len(pos), strings.Join(pos, " "))
	}

	name := feature.Slug(pos[0])
	if reserved()[name] {
		return fmt.Errorf("%q is a canaveral command; feature names cannot shadow commands", name)
	}
	opt := feature.Options{
		NoWindows: *noWindows, NoServices: *noServices, NoAgents: *noAgents, Base: *base,
	}

	m, err := loadManifest()
	if err != nil {
		// Outside a project entirely. A space is the only thing a bare name
		// can mean here, and it is the common way to open one: spaces exist
		// precisely because there is no directory to be standing in.
		if allowSpace && space.Exists(name) {
			return openSpace(ctx, name, opt, *focus && !*noWindows)
		}
		return noProjectError(err, name, allowSpace)
	}
	if allowSpace {
		if err := resolveBareName(m, name); err != nil {
			return err
		}
		// Not a feature of this project, but a space by that name exists.
		if !recordedFeature(m, name) && space.Exists(name) {
			return openSpace(ctx, name, opt, *focus && !*noWindows)
		}
	}
	r := reporter{}

	// A stashed name is restored, not refused, and by both verbs alike:
	// `canaveral new x` and `canaveral x` equally mean "give me this
	// workspace", and a stash is that workspace waiting — its worktree still
	// on disk, its branch, its agent's conversation.
	//
	// `new` in particular has to do this rather than error, because the
	// alternative is worse than an error. The branch and the worktree are
	// still there for worktree.Ensure to adopt, so `new` would happily
	// succeed and hand back the same feature minus everything that had been
	// remembered about it: the session, the slot preference, the stash
	// record itself left orphaned behind it. Saying so out loud is the only
	// part that needs deciding.
	stash, stashErr := state.LoadStash(m.Name, name)
	if stashErr != nil && !errors.Is(stashErr, state.ErrNotFound) {
		return stashErr
	}
	if stashErr == nil {
		r.Step("restoring stashed %s  %s",
			color(cBold, m.Name+"/"+name), color(cDim, humanAgo(stash.StashedAt)))
		res, err := feature.Pop(ctx, m, name, opt, r)
		if err != nil {
			return err
		}
		return reportFeature(ctx, res, r, *focus && !*noWindows)
	}

	// Distinguish "no such feature" from an unreadable record: a corrupt
	// state file must not look like a free name, or `new` would allocate a
	// fresh slot over the top of a feature that still has units running.
	_, loadErr := state.Load(m.Name, name)
	if err := checkFeatureExistence(loadErr, create, m.Name, name); err != nil {
		return err
	}

	r.Step("%s  %s", color(cBold, m.Name+"/"+name), color(cDim, homeTilde(m.Root)))

	res, err := feature.Reconcile(ctx, m, name, opt, r)
	if err != nil {
		return err
	}
	return reportFeature(ctx, res, r, *focus && !*noWindows)
}

// reportFeature prints what a reconcile pass produced and, if asked, focuses
// the workspace. Shared by `new`, bare dispatch and `pop`, which differ in
// how they arrived at a Result and not at all in what they say about one.
func reportFeature(ctx context.Context, res *feature.Result, r reporter, focus bool) error {
	f := res.Feature
	switch {
	case res.Restored:
		r.OK("%s restored", color(cBold, f.Key()))
	case len(res.StartedSvc)+len(res.StartedAgent)+len(res.SpawnedWindow) == 0 && !res.Created:
		r.OK("%s already up to date", f.Key())
	default:
		r.OK("%s ready", color(cBold, f.Key()))
	}
	printFeatureSummary(f)

	if focus {
		focusFeatureWorkspace(ctx, f, r)
	}
	return nil
}

// checkFeatureExistence distinguishes "no such feature" from a corrupt
// state file and, depending on create, from a feature that already exists.
// A corrupt state file must never look like a free name, or `new` would
// allocate a fresh slot over the top of a feature that still has units
// running.
func checkFeatureExistence(loadErr error, create bool, projectName, name string) error {
	switch {
	case loadErr != nil && !errors.Is(loadErr, state.ErrNotFound):
		return loadErr
	case create && loadErr == nil:
		return fmt.Errorf("feature %q already exists; run `canaveral %s` to bring it up to date", name, name)
	case !create && loadErr != nil:
		return unknownFeature(projectName, name)
	}
	return nil
}

// focusFeatureWorkspace switches to f's workspace, first pulling it back
// onto whichever monitor is actually focused right now.
//
// The workspace may have been deliberately built on a monitor other than
// the one the user is on (see reconcileLayoutWindows), so it has to be
// relocated before switching to it — otherwise --focus would silently make
// it appear on a screen the user is not even looking at.
func focusFeatureWorkspace(ctx context.Context, f *state.Feature, r reporter) {
	if err := hypr.Available(ctx); err != nil {
		return
	}
	if mon, err := hypr.ActiveMonitor(ctx); err == nil {
		_ = hypr.MoveWorkspaceToMonitor(ctx, f.HyprWorkspace(), mon.Name)
	}
	if err := hypr.Focus(ctx, f.HyprWorkspace()); err != nil {
		r.Warn("%v", err)
	}
}

// unknownFeature explains a name that is neither a command nor a feature.
//
// The overwhelmingly likely cause is a mistyped command — `stratus` for
// `status` — so guess at that first and only then mention creating a feature,
// which is almost certainly not what was wanted.
func unknownFeature(project, name string) error {
	msg := fmt.Sprintf("no feature %q in %s", name, project)
	if s := nearest(name, commandNames()); s != "" {
		msg += fmt.Sprintf("\n  did you mean `canaveral %s`?", s)
	} else if known, err := state.List(project); err == nil {
		if s := nearest(name, known); s != "" {
			msg += fmt.Sprintf("\n  did you mean `canaveral %s`?", s)
		}
	}
	return fmt.Errorf("%s\n  create it with `canaveral new %s`", msg, name)
}

// isOpenSpace reports whether a space by that name is currently up. Checked
// alongside space.Exists so that a space whose definition was deleted by hand
// can still be torn down.
func isOpenSpace(name string) bool {
	_, ok := spaceRecord(name)
	return ok
}

// rmSpace is `canaveral rm <space>`: close it, and keep the definition.
//
// `rm` on a feature deletes the worktree because canaveral made it. It made
// nothing for a space, so there is nothing here to delete, and silently
// removing the definition would throw away the only copy of something you
// wrote. Deleting that is `canaveral space rm`, which says so and asks.
func rmSpace(ctx context.Context, name string, keepWorktree, keepBranch bool) error {
	r := reporter{}
	if keepWorktree || keepBranch {
		r.Info("%s is a space: it has no worktree or branch, so those flags do nothing", name)
	}
	if err := closeSpace(ctx, name, r); err != nil {
		return err
	}
	if space.Exists(name) {
		r.Info("definition kept; delete it with `canaveral space rm %s`", name)
	}
	return nil
}

// resolveBareName refuses a name that is both a feature of the current
// project and a space, pointing at the unambiguous form of each.
//
// Picking one silently is the failure mode `canaveral restart` already
// avoids for a name that is both a service and a feature: whichever it
// chose, the other would be unreachable by the form you actually typed.
func resolveBareName(m *manifest.Manifest, name string) error {
	if !recordedFeature(m, name) || !space.Exists(name) {
		return nil
	}
	return fmt.Errorf(
		"%q is both a feature of %s and a space; say which you mean:\n"+
			"  canaveral open %s        (the feature)\n"+
			"  canaveral space open %s  (the space)",
		name, m.Name, name, name)
}

// recordedFeature reports whether the project has this feature on record,
// active or stashed. Deliberately not "could have": a name that names
// nothing yet is free for a space to answer to.
func recordedFeature(m *manifest.Manifest, name string) bool {
	if _, err := state.Load(m.Name, name); err == nil {
		return true
	}
	_, err := state.LoadStash(m.Name, name)
	return err == nil
}

// noProjectError explains a bare name typed outside any project. The
// manifest error is the true one, but on its own it sends you looking for a
// canaveral.toml when what you meant was a space that does not exist yet.
func noProjectError(manifestErr error, name string, allowSpace bool) error {
	if !allowSpace {
		return manifestErr
	}
	list, err := space.List()
	if err != nil || len(list) == 0 {
		return fmt.Errorf("%w\n  no spaces defined either; `canaveral space new %s` makes one that needs no project",
			manifestErr, name)
	}
	if s := nearest(name, list); s != "" {
		return fmt.Errorf("%w\n  did you mean the space `canaveral %s`?", manifestErr, s)
	}
	return fmt.Errorf("%w\n  spaces: %s", manifestErr, strings.Join(list, ", "))
}

func printFeatureSummary(f *state.Feature) {
	if f.Space {
		// No branch and no worktree to report: a space has neither, and its
		// directory is one you already keep rather than something canaveral
		// made and should account for.
		printPortSummary(f)
		return
	}
	fmt.Printf("    %s %s\n", dim("branch  "), f.Branch)
	fmt.Printf("    %s %s\n", dim("worktree"), homeTilde(f.Worktree))
	if len(f.Ports) > 0 {
		names := sortedPortNames(f.Ports)
		var parts []string
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s=%d", n, f.Ports[n]))
		}
		fmt.Printf("    %s %s\n", dim("ports   "), strings.Join(parts, "  "))
	}
	if f.DBSuffix != "" {
		fmt.Printf("    %s %s\n", dim("db      "), "suffix "+f.DBSuffix)
	}
}

func printPortSummary(f *state.Feature) {
	if len(f.Ports) == 0 {
		return
	}
	var parts []string
	for _, n := range sortedPortNames(f.Ports) {
		parts = append(parts, fmt.Sprintf("%s=%d", n, f.Ports[n]))
	}
	fmt.Printf("    %s %s\n", dim("ports   "), strings.Join(parts, "  "))
}

func sortedPortNames(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func runReset(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("reset", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral reset [feature...] [flags]\n\nBring up whatever is missing: dead services, agents and closed windows.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		all       = fs.Bool("all", false, "reset every feature of the project")
		noWindows = fs.Bool("no-windows", false, "skip windows")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}

	names := pos
	if *all || len(names) == 0 {
		names, err = state.List(m.Name)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return fmt.Errorf("no features exist for %s yet (create one with `canaveral new <feature>`)", m.Name)
		}
	}

	r := reporter{}
	for _, n := range names {
		name := feature.Slug(n)
		if _, err := state.Load(m.Name, name); err != nil {
			r.Warn("%s: not a known feature, skipping", name)
			continue
		}
		r.Step("reset %s", color(cBold, m.Name+"/"+name))
		res, err := feature.Reconcile(ctx, m, name, feature.Options{NoWindows: *noWindows}, r)
		if err != nil {
			return err
		}
		n := len(res.StartedSvc) + len(res.StartedAgent) + len(res.SpawnedWindow)
		if n == 0 {
			r.OK("nothing missing")
		} else {
			r.OK("restored %d item(s)", n)
		}
	}
	return nil
}

func runRm(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral rm [feature...] [flags]\n\nStop a feature and remove its worktree. Defaults to whichever feature's\nworktree you're currently in.\n\nRefuses to remove a feature whose branch has not been merged into the\ndefault branch; merge it first, or pass --force. Once removed, the branch\nis deleted too if it was fully merged, and kept otherwise.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		keep       = fs.Bool("keep-worktree", false, "leave the worktree on disk")
		force      = fs.Bool("force", false, "remove even with uncommitted changes or an unmerged branch")
		keepBranch = fs.Bool("keep-branch", false, "never delete the branch, even if merged")
		all        = fs.Bool("all", false, "remove every feature of the project")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	m, err := loadManifest()

	// A space is reachable from anywhere and belongs to no project, so `rm`
	// has to reach one — the usual place to type this is somewhere with no
	// project at all. The project is asked first all the same: an explicit
	// verb always means the project's own, and `canaveral space close` is
	// the form that always means the space. Resolving the other way round
	// would let a space quietly shadow a feature of the same name and tear
	// down the wrong workspace.
	if len(pos) == 1 && !*all {
		name := feature.Slug(pos[0])
		mine := err == nil && recordedFeature(m, name)
		if !mine && (space.Exists(name) || isOpenSpace(name)) {
			return rmSpace(ctx, name, *keep, *keepBranch)
		}
	}
	if err != nil {
		return err
	}
	names := pos
	if *all {
		if names, err = state.List(m.Name); err != nil {
			return err
		}
		// --all means every feature of the project, and a stashed one is
		// still one of those: it holds a worktree and a branch just as an
		// active one does, and leaving them behind after `rm --all` would
		// make the flag quietly untrue.
		if stashed, err := state.ListStashes(m.Name); err == nil {
			names = append(names, stashed...)
		}
	}
	if len(names) == 0 {
		// Default to the feature you are standing in, the same way `merge`
		// and `restart` do. Removing a feature from inside its own worktree
		// is the common case — you finish, you tear it down — and having to
		// name what you are already in was busywork.
		f, err := currentFeature(m)
		if err != nil {
			return fmt.Errorf("not inside a feature worktree; specify a feature name, or use --all")
		}
		names = []string{f.Name}
	}

	r := reporter{}
	for _, n := range names {
		name := feature.Slug(n)
		f, err := state.Load(m.Name, name)
		if err != nil {
			// A stashed feature has no active record, but it does still own
			// a worktree and a branch, so `rm` has to reach it — otherwise
			// stashing something would be a way to make it undeletable
			// without editing the state directory by hand. No flag to say
			// which tree to look in: a name is only ever in one of them.
			if s, ok := stashedFeature(m, name); ok {
				r.Step("removing stashed %s", color(cBold, s.Feature.Key()))
				if err := feature.DiscardStash(ctx, s, *keep, *force, *keepBranch, r); err != nil {
					if len(names) == 1 {
						return err
					}
					r.Warn("%s: %v", name, oneLine(err.Error()))
				}
				continue
			}
			r.Warn("%s: %v", name, err)
			continue
		}
		r.Step("removing %s", color(cBold, f.Key()))
		if err := feature.Remove(ctx, f, *keep, *force, *keepBranch, r); err != nil {
			if len(names) == 1 {
				return err
			}
			// Across a batch the full explanation repeats badly; one line
			// each is enough to see what was left alone and why.
			if errors.Is(err, feature.ErrUnmerged) {
				r.Warn("%s: not merged into the default branch, skipping", name)
				continue
			}
			r.Warn("%v", err)
		}
	}
	return nil
}

const starterTemplate = `# canaveral project manifest
name = "%s"

# Each feature gets its own worktree on this branch.
branch = "{{.Feature}}"

# Per-feature ports. Feature slot 0 gets the base, slot 1 gets base+1, ...
[ports]
web = 3000

# "shared" points every feature at the project's normal database.
# "suffix" exports DB_SUFFIX so each feature gets its own databases; the
# application's database config must interpolate it.
[database]
isolation = "shared"

# A fresh worktree holds tracked files only. Bring across what the app needs.
[worktree]
link = [%s]
copy = [%s]

%s
%s
[[window]]
name = "terminal"
run  = ""

[[window]]
name = "serverlogs"
run  = "canaveral logs {{.Feature}} web -f"

[[window]]
name = "chrome"
exec = "google-chrome --new-window {{.URL.web}}"
# Chrome hands --new-window to the browser it already has running, which never
# reads --class, so match the class it carries instead.
match_class = "(?i)^google-chrome$"
`

func runInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral init [path] [flags]\n\nWrite a starter canaveral.toml.\n\nFlags:")
		fs.PrintDefaults()
	}
	force := fs.Bool("force", false, "overwrite an existing canaveral.toml")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}

	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	out := filepath.Join(abs, manifest.FileName)
	if _, err := os.Stat(out); err == nil && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", out)
	}

	link, cp := detectArtifacts(abs)
	body := fmt.Sprintf(starterTemplate, filepath.Base(abs), link, cp,
		detectService(abs), starterAgent(config.DefaultAgentTool()))
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return err
	}
	r := reporter{}
	r.OK("wrote %s", homeTilde(out))
	r.Info("review it, then run: canaveral new <feature>")
	return nil
}

// starterAgent writes the [[agent]] block and the window that opens it, for
// whichever agent this machine defaults to.
//
// The window command comes from the harness rather than being hardcoded,
// because the two tools are opened in entirely different ways: opencode
// attaches to the server canaveral started for it, while Claude Code *is*
// the program the window runs. Deriving it from AttachArgv keeps the starter
// manifest correct for any harness added later without anyone remembering to
// come back here.
func starterAgent(tool string) string {
	h, err := agent.For(tool)
	if err != nil {
		// Unreachable via the config layer, which validates the name; a
		// commented-out block is still a better starting point than nothing.
		return "# [[agent]]\n# name = \"main\"\n# tool = \"opencode\"\n"
	}
	// The placeholders are manifest templates, rendered per feature at open
	// time — see internal/tmpl.
	argv := h.AttachArgv(agent.Conn{URL: "{{.Agent.main}}", Dir: "{{.Worktree}}"}, false)
	cmd := strings.Join(append(argv, "{{.Agent.main.Session}}"), " ")

	return fmt.Sprintf(`[[agent]]
name = "main"
tool = %q

# Windows opened on the feature's Hyprland workspace, grouped as tabs.
# "run" executes inside a terminal rooted at the worktree; "exec" is a GUI app.
[[window]]
name = %q
run  = %q
`, tool, tool, cmd)
}

// detectArtifacts guesses which gitignored paths a worktree will need.
func detectArtifacts(dir string) (link, copy string) {
	var links, copies []string
	for _, c := range []string{"node_modules", ".bundle", "vendor/bundle", "config/master.key", "storage"} {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			links = append(links, fmt.Sprintf("%q", c))
		}
	}
	for _, c := range []string{".env", ".env.local"} {
		if _, err := os.Stat(filepath.Join(dir, c)); err == nil {
			copies = append(copies, fmt.Sprintf("%q", c))
		}
	}
	return strings.Join(links, ", "), strings.Join(copies, ", ")
}

// detectService produces a best-guess [[service]] block for common stacks.
func detectService(dir string) string {
	type guess struct{ marker, name, cmd, ready string }
	guesses := []guess{
		{"bin/rails", "web", "bin/rails server -p {{.Port.web}}", `ready.http = "{{.URL.web}}/up"`},
		{"Procfile.dev", "web", "foreman start -f Procfile.dev", `ready.tcp = "localhost:{{.Port.web}}"`},
		{"package.json", "web", "npm run dev", `ready.tcp = "localhost:{{.Port.web}}"`},
	}
	for _, g := range guesses {
		if _, err := os.Stat(filepath.Join(dir, g.marker)); err != nil {
			continue
		}
		b := fmt.Sprintf("[[service]]\nname = %q\ncmd  = %q\nenv  = { PORT = \"{{.Port.web}}\" }\n", g.name, g.cmd)
		if g.ready != "" {
			b += g.ready + "\nready.timeout = \"120s\"\n"
		}
		return b
	}
	return "# [[service]]\n# name = \"web\"\n# cmd  = \"bin/dev\"\n# env  = { PORT = \"{{.Port.web}}\" }\n# ready.http = \"{{.URL.web}}/up\"\n"
}
