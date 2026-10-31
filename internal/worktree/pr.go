package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/agent"
)

// MergedPR reports the number of a merged GitHub pull request that carried
// branch's current tip, asking the gh CLI. ok is false whenever there is no
// such pull request or the question cannot be asked at all: no gh, not
// signed in, not a GitHub remote, no network.
//
// This exists for repositories that land work through a merge queue, or with
// squash or rebase merges. There the merge happens on GitHub, so the local
// default branch does not contain it until someone pulls, and with squash or
// rebase it never contains the branch's own commits at all. Ancestry is the
// wrong question for those; "did a pull request with this exact work merge"
// is the right one.
//
// A pull request only counts if its head contains the branch's tip. Matching
// on the branch name alone would accept a branch that has gained commits
// since its pull request merged, or a name reused from an older one.
func MergedPR(ctx context.Context, repo, branch string) (number int, ok bool) {
	tip, err := BranchTip(ctx, repo, branch)
	if err != nil {
		return 0, false
	}
	n, ok, _ := MergedPRAt(ctx, repo, branch, tip)
	return n, ok
}

// BranchTip returns the commit a local branch points at.
func BranchTip(ctx context.Context, repo, branch string) (string, error) {
	tip, err := gitOutput(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", branch, err)
	}
	return tip, nil
}

// MergedPRAt is MergedPR for a tip the caller has already resolved, so a
// cache keyed on the tip does not resolve it twice. err is set when GitHub
// could not be asked at all, which unlike "no merged pull request" is not an
// answer worth remembering.
func MergedPRAt(ctx context.Context, repo, branch, tip string) (number int, ok bool, err error) {
	// Not exec.LookPath: the status bar's `watch` runs with the session's
	// PATH, which lacks mise and with it gh, so every branch read as not
	// merged there while `canaveral status` in a terminal said merged.
	gh, err := agent.LookPath("gh")
	if err != nil {
		return 0, false, fmt.Errorf("find gh: %w", err)
	}

	// Bounded: this runs on the way to tearing a feature down, and a hung
	// network call should cost the user a refusal, not a stuck terminal.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gh, "pr", "list", "--head", branch,
		"--state", "merged", "--json", "number,headRefOid", "--limit", "20")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return 0, false, fmt.Errorf("gh pr list: %w", err)
	}
	var prs []struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, false, fmt.Errorf("parse gh pr list: %w", err)
	}
	for _, pr := range prs {
		if pr.HeadRefOid == tip {
			return pr.Number, true, nil
		}
		// The pull request may have moved on after this branch was last
		// pulled, e.g. a fixup pushed from elsewhere; it still carried this
		// work. An unknown object errors out here and simply does not count.
		if in, err := IsMerged(ctx, repo, tip, pr.HeadRefOid); err == nil && in {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}

// MergedLocally reports whether branch has work of its own and all of it is
// in the repo's default branch, locally or as last fetched from origin: the
// case MergedPR cannot see, a branch merged by hand or by `canaveral merge
// --keep` with no pull request anywhere.
//
// Ancestry alone would also call a branch with no commits merged, since its
// tip is a commit of the default branch it forked from. So it must have work
// of its own first, which the branch's reflog records: an entry for a commit
// made on it. That survives the merge, unlike any count of commits ahead,
// and a branch that was only ever created, rebased or reset has none.
// A branch whose reflog has expired or was never kept reads as not merged.
func MergedLocally(ctx context.Context, repo, branch string) bool {
	if !hasOwnCommits(ctx, repo, branch) {
		return false
	}
	def, err := DefaultBranch(ctx, repo)
	if err != nil || def == branch {
		return false
	}
	for _, target := range []string{"refs/heads/" + def, "refs/remotes/origin/" + def} {
		if m, err := IsMerged(ctx, repo, "refs/heads/"+branch, target); err == nil && m {
			return true
		}
	}
	return false
}

// hasOwnCommits reports whether anything was ever committed on branch,
// according to its reflog. Entries read "commit: …", "commit (amend): …",
// "commit (merge): …", "cherry-pick: …" and so on; a rebase's own entries
// do not count, since rebasing an empty branch gives it nothing of its own.
func hasOwnCommits(ctx context.Context, repo, branch string) bool {
	out, err := gitOutput(ctx, repo, "reflog", "show", "--format=%gs", "refs/heads/"+branch, "--")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "commit") || strings.HasPrefix(line, "cherry-pick") {
			return true
		}
	}
	return false
}
