package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
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

func runPath(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("path", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral path <feature>\n\n"+
			"Print a feature's worktree path, so it can be used from a shell:\n\n"+
			"  cd \"$(canaveral path small-fixes)\"\n"+
			"  vim \"$(canaveral path small-fixes)/app/models/user.rb\"")
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fmt.Errorf("specify one feature, e.g. `canaveral path small-fixes`")
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
