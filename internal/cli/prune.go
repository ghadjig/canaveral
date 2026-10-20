package cli

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/space"
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
		fmt.Fprintln(os.Stderr, "Usage: canaveral prune [flags]\n\nStop leftover service and agent units whose feature no longer exists, delete\nbrowser profiles nothing owns any more, and finish any removal an\ninterrupted `rm` never got to.\n\nA unit is what an interrupted or failed teardown leaves behind: a server\nstill holding a feature's port, serving a worktree that has been deleted,\nready to answer the next feature's readiness probe from the grave. A stuck\n\"removing\" phase is the other half of the same failure — `rm`'s own\nterminal window closing under it, a crash, a reboot — left behind because\nthe state file recording progress is deliberately the last thing `rm`\ndeletes.\n\nFlags:")
		fs.PrintDefaults()
	}
	dry := fs.Bool("dry-run", false, "list what would be stopped or deleted, and change nothing")
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

	if err := pruneProfiles(*dry, r); err != nil {
		return err
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

// pruneProfiles deletes window profiles that no feature, stash or space owns.
//
// `rm` deletes a feature's own profiles now, but did not always: every
// feature removed before that left a few hundred megabytes of browser
// profile behind, enough to fill a disk. This also catches whatever a
// teardown killed by its own window never reached.
func pruneProfiles(dry bool, r reporter) error {
	orphans, err := state.OrphanedProfiles(func(project string) bool {
		// A defined space owns its directory even while closed, and so does
		// one that is open after its definition was deleted by hand.
		return space.Exists(project) || isOpenSpace(project)
	})
	if err != nil {
		return fmt.Errorf("find orphaned profiles: %w", err)
	}
	if len(orphans) == 0 {
		r.OK("no orphaned browser profiles")
		return nil
	}
	var total int64
	for _, p := range orphans {
		n := dirSize(p)
		total += n
		r.Info("%s  %s", homeTilde(p), humanBytes(uint64(n)))
	}
	if dry {
		r.OK("%s would be freed from %s", humanBytes(uint64(total)), plural(len(orphans), "orphaned profile"))
		return nil
	}
	var failed int
	for _, p := range orphans {
		if err := os.RemoveAll(p); err != nil {
			r.Warn("%v", err)
			failed++
		}
	}
	r.OK("freed %s from %s", humanBytes(uint64(total)), plural(len(orphans)-failed, "orphaned profile"))
	return nil
}

// dirSize sums the sizes of the regular files under p, for reporting.
func dirSize(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(_ string, e fs.DirEntry, err error) error {
		if err == nil && e.Type().IsRegular() {
			if info, err := e.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
