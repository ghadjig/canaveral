package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// loadManifest finds the project manifest from the current directory.
func loadManifest() (*manifest.Manifest, error) {
	root, err := manifest.Find(".")
	if err != nil {
		return nil, err
	}
	return manifest.Load(root)
}

func runOpen(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("open", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral <feature> [flags]\n\nCreate or reconcile a feature workspace in the background, without\nswitching your view to it. Pass --focus to jump there once it's ready.\n\nFlags:")
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
		return fmt.Errorf("specify a feature name, e.g. `canaveral small-fixes`")
	}
	if len(pos) > 1 {
		return fmt.Errorf("expected one feature name, got %d: %s", len(pos), strings.Join(pos, " "))
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	name := feature.Slug(pos[0])
	if reserved()[name] {
		return fmt.Errorf("%q is a canaveral command; feature names cannot shadow commands", name)
	}

	opt := feature.Options{
		NoWindows: *noWindows, NoServices: *noServices, NoAgents: *noAgents, Base: *base,
	}
	r := reporter{}
	r.Step("%s  %s", color(cBold, m.Name+"/"+name), color(cDim, homeTilde(m.Root)))

	res, err := feature.Reconcile(ctx, m, name, opt, r)
	if err != nil {
		return err
	}
	f := res.Feature

	if len(res.StartedSvc)+len(res.StartedAgent)+len(res.SpawnedWindow) == 0 && !res.Created {
		r.OK("%s already up to date", f.Key())
	} else {
		r.OK("%s ready", color(cBold, f.Key()))
	}
	printFeatureSummary(f)

	if *focus && !*noWindows {
		if err := hypr.Available(ctx); err == nil {
			// The workspace may have been deliberately built on a monitor
			// other than the one the user is on (see reconcileLayoutWindows),
			// so it has to be pulled back to whichever monitor is actually
			// focused right now before switching to it — otherwise
			// --focus would silently make it appear on a screen the user
			// is not even looking at.
			if mon, err := hypr.ActiveMonitor(ctx); err == nil {
				_ = hypr.MoveWorkspaceToMonitor(ctx, f.HyprWorkspace(), mon.Name)
			}
			if err := hypr.Focus(ctx, f.HyprWorkspace()); err != nil {
				r.Warn("%v", err)
			}
		}
	}
	return nil
}

func printFeatureSummary(f *state.Feature) {
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
			return fmt.Errorf("no features exist for %s yet (create one with `canaveral <feature>`)", m.Name)
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
		fmt.Fprintln(os.Stderr, "Usage: canaveral rm <feature...> [flags]\n\nStop a feature and remove its worktree. The branch is deleted too if it has\nalready been fully merged into the default branch; otherwise it is kept.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		keep       = fs.Bool("keep-worktree", false, "leave the worktree on disk")
		force      = fs.Bool("force", false, "remove the worktree even with uncommitted changes")
		keepBranch = fs.Bool("keep-branch", false, "never delete the branch, even if merged")
		all        = fs.Bool("all", false, "remove every feature of the project")
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
	if *all {
		if names, err = state.List(m.Name); err != nil {
			return err
		}
	}
	if len(names) == 0 {
		return fmt.Errorf("specify a feature name, or use --all")
	}

	r := reporter{}
	for _, n := range names {
		name := feature.Slug(n)
		f, err := state.Load(m.Name, name)
		if err != nil {
			r.Warn("%s: %v", name, err)
			continue
		}
		r.Step("removing %s", color(cBold, f.Key()))
		if err := feature.Remove(ctx, f, *keep, *force, *keepBranch, r); err != nil {
			if len(names) == 1 {
				return err
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
[[agent]]
name = "main"
tool = "opencode"

# Windows opened on the feature's Hyprland workspace, grouped as tabs.
# "run" executes inside a terminal rooted at the worktree; "exec" is a GUI app.
[[window]]
name = "opencode"
run  = "opencode attach {{.Agent.main}}"

[[window]]
name = "terminal"
run  = ""

[[window]]
name = "serverlogs"
run  = "canaveral logs {{.Feature}} web -f"

[[window]]
name = "chrome"
exec = "google-chrome --new-window {{.URL.web}}"
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
	body := fmt.Sprintf(starterTemplate, filepath.Base(abs), link, cp, detectService(abs))
	if err := os.WriteFile(out, []byte(body), 0o644); err != nil {
		return err
	}
	r := reporter{}
	r.OK("wrote %s", homeTilde(out))
	r.Info("review it, then run: canaveral <feature>")
	return nil
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
