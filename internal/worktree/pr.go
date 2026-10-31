package worktree

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"
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
	if _, err := exec.LookPath("gh"); err != nil {
		return 0, false
	}
	tip, err := gitOutput(ctx, repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil || tip == "" {
		return 0, false
	}

	// Bounded: this runs on the way to tearing a feature down, and a hung
	// network call should cost the user a refusal, not a stuck terminal.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "pr", "list", "--head", branch,
		"--state", "merged", "--json", "number,headRefOid", "--limit", "20")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	var prs []struct {
		Number     int    `json:"number"`
		HeadRefOid string `json:"headRefOid"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return 0, false
	}
	for _, pr := range prs {
		if pr.HeadRefOid == tip {
			return pr.Number, true
		}
		// The pull request may have moved on after this branch was last
		// pulled, e.g. a fixup pushed from elsewhere; it still carried this
		// work. An unknown object errors out here and simply does not count.
		if in, err := IsMerged(ctx, repo, tip, pr.HeadRefOid); err == nil && in {
			return pr.Number, true
		}
	}
	return 0, false
}
