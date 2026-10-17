package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/space"
)

// runSave writes the windows open on a space's workspace into its definition.
//
// It is the other half of `space new --empty`: rather than describing a
// workspace in TOML up front, you build it by hand once and have canaveral
// write down what you built. A top-level command rather than a `space` verb
// because it is the thing typed from inside the space, where brevity counts.
func runSave(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral save [space] [--replace]\n\n"+
			"Add the windows open on a space's Hyprland workspace to its definition,\n"+
			"so the next `canaveral <space>` opens them again. Windows the definition\n"+
			"already declares are left as they are; only the others are added.\n"+
			"--replace rewrites them all from what is open instead.\n\n"+
			"With no name, save the space you are in — by $CANAVERAL_FEATURE, or by\n"+
			"focused workspace. A name with no definition yet creates one.\n\nFlags:")
		fs.PrintDefaults()
	}
	replace := fs.Bool("replace", false, "make the open windows the space's only windows, rather than adding to them")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		fs.Usage()
		return errors.New("specify at most one space")
	}

	var name string
	if len(pos) == 1 {
		if name, err = newSpaceName(pos[0]); err != nil {
			return err
		}
	} else if name, err = currentSpace(ctx); err != nil {
		return err
	}

	if err := hypr.Available(ctx); err != nil {
		return fmt.Errorf("save %s: %w", name, err)
	}
	clients, err := hypr.Clients(ctx)
	if err != nil {
		return err
	}

	r := reporter{}
	// Created only once there is something to save, so a mistyped name does
	// not leave an empty definition behind. Until then it declares nothing,
	// which is all Capture needs to know about it.
	m, err := space.Load(name)
	switch {
	case errors.Is(err, space.ErrNotFound):
		m = &manifest.Manifest{Name: name, Space: true}
	case err != nil:
		return err
	}
	declared := m
	if *replace {
		// Nothing counts as already saved: every window on the workspace is
		// captured afresh, which is the point of replacing.
		declared = &manifest.Manifest{Name: name, Space: true}
	}

	got := space.Capture(declared, name, clients, hypr.Argv)
	for _, s := range got.Skipped {
		r.Warn("skipped %s: its command line could not be read", s)
	}
	windows := got.Windows
	if len(windows) == 0 {
		if len(m.Windows) > 0 && !*replace {
			r.Info("%s: every open window is already saved", name)
			return nil
		}
		return fmt.Errorf("no windows open on workspace %q to save; open some there first", name)
	}

	if !space.Exists(name) {
		path, err := space.CreateEmpty(name)
		if err != nil {
			return err
		}
		r.OK("wrote %s", homeTilde(path))
	}
	write := space.Append
	if *replace {
		write = space.Replace
	}
	path, err := write(name, windows)
	if err != nil {
		return err
	}
	for _, w := range windows {
		r.OK("saved window %s  %s", color(cBold, w.Name), dim(oneLine(w.Exec)))
	}
	for _, w := range got.Shared {
		r.Warn("%s: its process draws other windows too, so this is how the app was started, not what the window shows; edit in the URL or arguments you want", w)
	}
	r.Info("edit %s to tidy the commands up", homeTilde(path))

	// Reconciled straight away when the space is open, so its record owns the
	// windows that were just saved: otherwise `canaveral space close` would
	// leave them behind until the next open adopted them. Nothing is spawned,
	// because every saved window is matched to the one already there.
	if _, open := spaceRecord(name); open {
		return openSpace(ctx, name, feature.Options{}, false)
	}
	return nil
}

// currentSpace finds the space `save` was run from: the one this process was
// started in, then the one whose workspace is focused — the launcher strips
// canaveral's variables, and from there the focused workspace is the answer.
func currentSpace(ctx context.Context) (string, error) {
	if f := featureFromEnv(); f != nil && f.Space {
		return f.Name, nil
	}
	if hypr.Available(ctx) == nil {
		if ws, err := hypr.ActiveWorkspaceName(ctx); err == nil && space.Exists(ws) {
			return ws, nil
		}
	}
	return "", errors.New("not in a space; name one: `canaveral save <space>`")
}
