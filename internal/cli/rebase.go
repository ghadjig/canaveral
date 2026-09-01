package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/worktree"
)

// runRebase fetches from the remote and replays a feature's branch on top of
// the default branch, in the feature's own worktree.
//
// This is the first half of `canaveral merge`, on its own and repeatable: the
// point is to keep a long-running feature close to the branch it will land on,
// so the conflicts arrive a few at a time instead of all at once at the end.
// The fetch is what makes it worth a command — rebasing onto a local `main`
// that was last updated a week ago catches up with nothing.
func runRebase(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("rebase", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral rebase [feature] [flags]\n\nFetch from the remote, then replay a feature's branch on top of the\ndefault branch (origin/main, or origin/master). Defaults to whichever\nfeature's worktree you're currently in. Refuses if the worktree has\nuncommitted changes.\n\nA conflicted rebase is left in progress, to finish with `git rebase\n--continue` or throw away with `git rebase --abort`.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		onto    = fs.String("onto", "", "branch or ref to rebase onto (default: the remote's default branch)")
		remote  = fs.String("remote", "origin", "remote to fetch from")
		noFetch = fs.Bool("no-fetch", false, "skip the fetch, rebasing onto what was already fetched")
	)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("expected at most one feature name, got %d: %s", len(pos), strings.Join(pos, " "))
	}

	m, err := loadManifest()
	if err != nil {
		return err
	}
	f, err := featureFromArgs(m, pos)
	if err != nil {
		return err
	}

	if err := ensureRebasable(ctx, f); err != nil {
		return err
	}

	r := reporter{}
	hasRemote := worktree.HasRemote(ctx, f.Worktree, *remote)
	if !*noFetch && hasRemote {
		r.Step("fetching %s", *remote)
		if err := worktree.Fetch(ctx, f.Worktree, *remote); err != nil {
			return err
		}
		r.OK("fetched")
	}

	target, err := resolveRebaseTarget(ctx, f.Worktree, *remote, *onto, hasRemote)
	if err != nil {
		return err
	}
	if target == f.Branch {
		return fmt.Errorf("feature branch %q is already %q", f.Branch, target)
	}

	r.Step("rebasing %s onto %s", color(cBold, f.Branch), target)
	if err := worktree.RebaseKeepingConflicts(ctx, f.Worktree, target); err != nil {
		if errors.Is(err, worktree.ErrRebaseConflict) {
			r.Warn("conflicts in %s", homeTilde(f.Worktree))
			r.Info("resolve them, then `git rebase --continue`; `git rebase --abort` undoes the lot")
		}
		return err
	}
	r.OK("rebased onto %s", target)

	if st, err := worktree.Status(ctx, f.Worktree); err == nil {
		r.Info("%s", st.Label())
	}
	return nil
}

// ensureRebasable refuses to rebase a feature that already has a rebase in
// progress, or whose worktree has uncommitted changes.
func ensureRebasable(ctx context.Context, f *state.Feature) error {
	if worktree.RebaseInProgress(ctx, f.Worktree) {
		return fmt.Errorf("%s already has a rebase in progress; finish it with `git rebase --continue` or drop it with `git rebase --abort`", f.Key())
	}
	dirty, err := worktree.IsDirty(ctx, f.Worktree, f.Provisioned)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%s has uncommitted changes; commit or discard them before rebasing", f.Key())
	}
	return nil
}

// resolveRebaseTarget picks what to rebase onto: onto if given, otherwise
// the remote's default branch when there is one to track, otherwise the
// local default branch.
func resolveRebaseTarget(ctx context.Context, dir, remote, onto string, hasRemote bool) (string, error) {
	if onto != "" {
		return onto, nil
	}
	if hasRemote {
		return worktree.RemoteDefaultBranch(ctx, dir, remote)
	}
	// No remote to track, so the local default branch is as current as
	// anything gets here.
	return worktree.DefaultBranch(ctx, dir)
}
