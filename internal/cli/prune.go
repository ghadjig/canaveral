package cli

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
)

// runPrune stops feature units that outlived the feature they belong to, and
// finishes any removal that never got to.
//
// Both are what an interrupted or failed teardown leaves behind: a unit is
// what used to hold a feature's port after the state describing it is gone;
// a stale "removing" phase is what happens when the state file outlives the
// process that was supposed to delete it — see feature.Reap.
func runPrune(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral prune [flags]\n\nStop leftover service and agent units whose feature no longer exists, and\nfinish any removal an interrupted `rm` never got to.\n\nA unit is what an interrupted or failed teardown leaves behind: a server\nstill holding a feature's port, serving a worktree that has been deleted,\nready to answer the next feature's readiness probe from the grave. A stuck\n\"removing\" phase is the other half of the same failure — `rm`'s own\nterminal window closing under it, a crash, a reboot — left behind because\nthe state file recording progress is deliberately the last thing `rm`\ndeletes.\n\nFlags:")
		fs.PrintDefaults()
	}
	dry := fs.Bool("dry-run", false, "list what would be stopped, and stop nothing")
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	r := reporter{}

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
	if len(orphans) == 0 {
		r.OK("no orphaned units")
	} else {
		for _, u := range orphans {
			if st, err := unit.Query(ctx, u); err == nil && st.MainPID > 0 {
				r.Info("%s  pid %d  %s", u, st.MainPID, st.ActiveState)
			} else {
				r.Info("%s", u)
			}
		}
		if *dry {
			r.OK("%d orphaned unit(s) would be stopped", len(orphans))
		} else {
			r.Step("stopping %d orphaned unit(s)", len(orphans))
			failed := unit.StopAll(ctx, orphans)
			r.OK("stopped %d unit(s)", len(orphans)-len(failed))
			if len(failed) > 0 {
				return fmt.Errorf("could not stop %d unit(s): %v", len(failed), failed)
			}
		}
	}

	var stuck []*state.Feature
	for _, f := range all {
		if f.Phase == state.PhaseRemoving && !f.InPhase() {
			stuck = append(stuck, f)
		}
	}
	if len(stuck) == 0 {
		r.OK("no stuck removals")
		return nil
	}
	for _, f := range stuck {
		r.Info("%s  stuck removing since %s", f.Key(), humanAgo(f.PhaseSince))
	}
	if *dry {
		r.OK("%d stuck removal(s) would be finished", len(stuck))
		return nil
	}
	done, err := feature.Reap(ctx, r)
	if err != nil {
		return err
	}
	r.OK("finished %d removal(s)", len(done))
	return nil
}
