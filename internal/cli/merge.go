package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/worktree"
)

// runMerge rebases a feature's branch onto the target branch, merges it in,
// then tears the feature's workspace down — the branch itself is deleted as
// part of that teardown, since it is now merged (see feature.Remove).
//
// The rebase happens first, in the feature's own worktree, so any conflicts
// are resolved against the feature's branch rather than the target: the merge
// that follows, in the main checkout, is then just a formality (or a genuine
// fast-forward, with --ff-only).
func runMerge(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("merge", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral merge [feature] [flags]\n\nRebase a feature's branch onto the target branch, merge it in, then tear\nthe feature's workspace down. Defaults to whichever feature's worktree\nyou're currently in. Refuses if the worktree has uncommitted changes.\n\nFlags:")
		fs.PrintDefaults()
	}
	var (
		into   = fs.String("into", "", "branch to merge into (default: the repo's default branch)")
		ffOnly = fs.Bool("ff-only", false, "fast-forward only, without a merge commit")
		keep   = fs.Bool("keep", false, "leave the feature's workspace up after merging")
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

	// Resolve the main checkout from the worktree rather than trusting the
	// stored root: features recorded before that path was derived from git
	// have the worktree itself saved there, which would send every command
	// below at the feature's own branch instead of the project's.
	root, err := worktree.MainCheckout(ctx, f.Worktree)
	if err != nil {
		return err
	}

	target := *into
	if target == "" {
		if target, err = worktree.DefaultBranch(ctx, root); err != nil {
			return err
		}
	}
	if target == f.Branch {
		return fmt.Errorf("feature branch %q is already %q", f.Branch, target)
	}

	dirty, err := worktree.IsDirty(ctx, f.Worktree, f.Provisioned)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%s has uncommitted changes; commit or discard them before merging", f.Key())
	}

	r := reporter{}
	r.Step("rebasing %s onto %s", color(cBold, f.Branch), target)
	if err := worktree.Rebase(ctx, f.Worktree, target); err != nil {
		return err
	}
	r.OK("rebased")

	if err := ensureCheckedOut(ctx, root, target); err != nil {
		return err
	}

	r.Step("merging %s into %s", color(cBold, f.Branch), target)
	if err := worktree.MergeBranch(ctx, root, f.Branch, *ffOnly); err != nil {
		return err
	}
	r.OK("merged")

	if *keep {
		return nil
	}
	r.Step("removing %s", color(cBold, f.Key()))
	return feature.Remove(ctx, f, false, false, false, r)
}

// ensureCheckedOut switches the main checkout at root onto target, unless
// it is already there. Refuses when root itself has uncommitted changes,
// since checking out over them would discard them.
func ensureCheckedOut(ctx context.Context, root, target string) error {
	cur, err := worktree.CurrentBranch(ctx, root)
	if err != nil {
		return err
	}
	if cur == target {
		return nil
	}
	dirty, err := worktree.IsDirty(ctx, root, nil)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%s is on %q with uncommitted changes; switch to %q manually and re-run",
			homeTilde(root), cur, target)
	}
	return worktree.Checkout(ctx, root, target)
}
