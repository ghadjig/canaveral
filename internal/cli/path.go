package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// resolveFeature loads a feature of the current project by name.
func resolveFeature(name string) (*state.Feature, error) {
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	f, err := state.Load(m.Name, feature.Slug(name))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// featureFromArgs resolves the feature a command should act on from its
// positional arguments: the named feature if one was given (pos must
// already be at most one name — callers reject more before this point),
// otherwise whichever feature currentFeature says the shell belongs to.
//
// Shared by commands that accept an optional feature name and default to
// "wherever you are" when none is given (`merge`, `rebase`).
func featureFromArgs(m *manifest.Manifest, pos []string) (*state.Feature, error) {
	if len(pos) == 1 {
		name := feature.Slug(pos[0])
		f, err := state.Load(m.Name, name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return f, nil
	}
	return currentFeature(m)
}

// currentFeature finds the feature the shell belongs to, for commands willing
// to default to "whichever feature you're currently in" when no name is given
// (e.g. `canaveral merge` run from inside a feature's own worktree).
//
// Two signals, and deliberately only two. The working directory sitting inside
// a worktree is the obvious one. $CANAVERAL_FEATURE is the other: canaveral
// exports it into every process it starts, so a terminal it opened on a
// feature's workspace still knows which feature it belongs to after you have
// cd'd somewhere else entirely.
//
// The focused Hyprland workspace is a third signal, and it is not used here —
// see focusedFeature for why not.
func currentFeature(m *manifest.Manifest) (*state.Feature, error) {
	if f := featureFromDir(m.Name); f != nil {
		return f, nil
	}
	if f := featureFromEnv(); f != nil && f.Project == m.Name {
		return f, nil
	}
	return nil, fmt.Errorf("not inside a feature worktree; specify a feature name")
}

// focusedFeature answers "which feature am I looking at" for commands that only
// navigate, adding the focused Hyprland workspace to currentFeature's signals.
//
// That third signal is what makes a terminal you opened yourself — with a
// keybind, on a feature's workspace, inheriting neither canaveral's environment
// nor its working directory — still resolve to that feature.
//
// It is used only by `path`, never by `merge` or `restart`. A workspace is a
// property of the window, not of the shell: windows get dragged between
// workspaces, and a shell that has been carried somewhere else should not
// silently change which branch a merge lands on. Pointing `cd` at the wrong
// directory is a keystroke to undo; merging the wrong feature is not.
//
// No manifest is needed, and that is the point: this has to work from a
// directory belonging to no project at all. The workspace name carries the
// project, and the registry turns that into a checkout.
func focusedFeature(ctx context.Context) (*state.Feature, error) {
	if f := featureFromDir(""); f != nil {
		return f, nil
	}
	if f := featureFromEnv(); f != nil {
		return f, nil
	}
	if f := featureFromWorkspace(ctx); f != nil {
		return f, nil
	}
	return nil, fmt.Errorf("not in a feature (no worktree in the working directory, no $CANAVERAL_FEATURE, no feature workspace focused)")
}

// featureFromDir finds the feature whose worktree contains the working
// directory. An empty project searches every project.
func featureFromDir(project string) *state.Feature {
	wd, err := os.Getwd()
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(wd); err == nil {
		wd = resolved
	}

	var all []*state.Feature
	if project == "" {
		all, err = state.LoadAll()
	} else {
		all, err = state.LoadProject(project)
	}
	if err != nil {
		return nil
	}

	// Longest match wins. Worktrees do not normally nest, but a [worktree] root
	// pointed at a parent of another one would make two features match, and the
	// deeper one is the answer either way.
	var best *state.Feature
	for _, f := range all {
		wt := f.Worktree
		if wt == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(wt); err == nil {
			wt = resolved
		}
		if wd != wt && !strings.HasPrefix(wd, wt+string(filepath.Separator)) {
			continue
		}
		if best == nil || len(wt) > len(best.Worktree) {
			best = f
		}
	}
	return best
}

// featureFromEnv reads the feature canaveral started this process for.
func featureFromEnv() *state.Feature {
	project, name := os.Getenv("CANAVERAL_PROJECT"), os.Getenv("CANAVERAL_FEATURE")
	if project == "" || name == "" {
		return nil
	}
	f, err := state.Load(project, name)
	if err != nil {
		return nil
	}
	return f
}

// splitWorkspaceName splits canaveral's "project:feature" workspace naming
// convention. Anything else (a plain numbered workspace, a special
// workspace) is not one of ours.
func splitWorkspaceName(ws string) (project, feature string, ok bool) {
	project, feature, ok = strings.Cut(ws, ":")
	if !ok || project == "" || feature == "" {
		return "", "", false
	}
	return project, feature, true
}

// featureFromWorkspace reads the feature whose Hyprland workspace is focused.
func featureFromWorkspace(ctx context.Context) *state.Feature {
	if hypr.Available(ctx) != nil {
		return nil
	}
	ws, err := hypr.ActiveWorkspaceName(ctx)
	if err != nil {
		return nil
	}
	project, name, ok := splitWorkspaceName(ws)
	if !ok {
		return nil
	}
	f, err := state.Load(project, name)
	if err != nil {
		return nil
	}
	return f
}

func runPath(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral path [feature]\n\n"+
			"Print a feature's worktree path, so it can be used from a shell.\n"+
			"With no feature, print the worktree of whichever feature you are in —\n"+
			"by working directory, by $CANAVERAL_FEATURE, or by focused workspace.\n\n"+
			"  cd \"$(canaveral path)\"\n"+
			"  cd \"$(canaveral path small-fixes)\"\n"+
			"  vim \"$(canaveral path small-fixes)/app/models/user.rb\"")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("expected at most one feature, got %d: %s", len(pos), strings.Join(pos, " "))
	}

	// No name means "where am I", which must not need a project in scope: the
	// whole point is answering it from a directory that belongs to none.
	if len(pos) == 0 {
		f, err := focusedFeature(ctx)
		if err != nil {
			return err
		}
		fmt.Println(f.Worktree)
		return nil
	}

	f, err := resolveFeature(pos[0])
	if err != nil {
		return err
	}
	fmt.Println(f.Worktree)
	return nil
}

func runExec(ctx context.Context, args []string) error {
	usage := func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral exec <feature> [--] <command> [args...]\n\n"+
			"Run a command inside a feature's worktree, with the same toolchain and\n"+
			"environment its own services get.\n\n"+
			"  canaveral exec small-fixes -- git rebase main\n"+
			"  canaveral exec small-fixes -- bin/rails test")
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		usage()
		if len(args) == 0 {
			return fmt.Errorf("specify a feature and a command")
		}
		return nil
	}

	// Deliberately not run through parseArgs: everything after the feature
	// belongs to the command being run, so flags like `-la` must reach it
	// untouched rather than being claimed by canaveral.
	name, rest := args[0], args[1:]
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		usage()
		return fmt.Errorf("specify a command to run in %s", name)
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	f, err := state.Load(m.Name, feature.Slug(name))
	if err != nil {
		return err
	}

	env, err := feature.EnvFor(ctx, m, f)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, rest[0], rest[1:]...)
	cmd.Dir = f.Worktree
	cmd.Env = append(os.Environ(), envSlice(env)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		// Surface the command's own exit code rather than wrapping it, so
		// `canaveral exec ... && something` behaves as the shell expects.
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			os.Exit(ee.ExitCode())
		}
		return fmt.Errorf("%s: %w", strings.Join(rest, " "), err)
	}
	return nil
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}
