package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
)

// runPrune stops feature units that outlived the feature they belong to.
//
// A unit is an orphan when no feature anywhere still claims its name. That is
// deliberately a whole-machine question rather than a per-project one: the
// units are all in one systemd user manager, and a corpse holding port 3001
// for a project you are not standing in is exactly as harmful as one for the
// project you are.
func runPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral prune [flags]\n\nStop leftover service and agent units whose feature no longer exists.\n\nThese are what an interrupted or failed teardown leaves behind: a server\nstill holding a feature's port, serving a worktree that has been deleted,\nready to answer the next feature's readiness probe from the grave.\n\nFlags:")
		fs.PrintDefaults()
	}
	dry := fs.Bool("dry-run", false, "list what would be stopped, and stop nothing")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	live, err := unit.List(ctx)
	if err != nil {
		return fmt.Errorf("list units: %w", err)
	}

	all, err := state.LoadAll()
	if err != nil {
		return err
	}
	known := make([]string, 0, len(all))
	for _, f := range all {
		known = append(known, f.Project+"-"+f.Name)
	}

	orphans := unit.Orphans(live, known)
	r := reporter{}
	if len(orphans) == 0 {
		r.OK("no orphaned units")
		return nil
	}

	for _, u := range orphans {
		if st, err := unit.Query(ctx, u); err == nil && st.MainPID > 0 {
			r.Info("%s  pid %d  %s", u, st.MainPID, st.ActiveState)
		} else {
			r.Info("%s", u)
		}
	}
	if *dry {
		r.OK("%d orphaned unit(s) would be stopped", len(orphans))
		return nil
	}

	r.Step("stopping %d orphaned unit(s)", len(orphans))
	failed := unit.StopAll(ctx, orphans)
	r.OK("stopped %d unit(s)", len(orphans)-len(failed))
	if len(failed) > 0 {
		return fmt.Errorf("could not stop %d unit(s): %v", len(failed), failed)
	}
	return nil
}
