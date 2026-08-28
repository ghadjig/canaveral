package worktree

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// BranchStatus summarises how a feature's branch relates to the project's
// default branch, compared against its current tip (not the commit the
// feature branch originally forked from) — so upstream changes since then
// show up as "behind", the same way `git status` reports it.
type BranchStatus struct {
	// Base is the default branch compared against (e.g. "main" or
	// "origin/main"), auto-detected — never configured, since a wrong guess
	// only affects a status display, not anything destructive.
	Base string
	// Ahead is the number of commits on this branch not on Base — also its
	// commit count for telemetry purposes.
	Ahead int
	// Behind is the number of commits on Base not on this branch.
	Behind       int
	FilesChanged int
	Insertions   int
	Deletions    int
}

// Label summarises Ahead/Behind as a short phrase.
func (s BranchStatus) Label() string {
	switch {
	case s.Ahead == 0 && s.Behind == 0:
		return "up to date"
	case s.Ahead > 0 && s.Behind == 0:
		return fmt.Sprintf("ahead %d", s.Ahead)
	case s.Ahead == 0 && s.Behind > 0:
		return fmt.Sprintf("behind %d", s.Behind)
	default:
		return fmt.Sprintf("diverged (ahead %d, behind %d)", s.Ahead, s.Behind)
	}
}

// Status computes dir's branch status against the project's default branch,
// auto-detected from dir.
func Status(ctx context.Context, dir string) (BranchStatus, error) {
	base, err := defaultBranch(ctx, dir)
	if err != nil {
		return BranchStatus{}, err
	}
	s := BranchStatus{Base: base}

	// rev-list's three-dot form is the standard way to count commits unique
	// to each side of two current tips; unlike `diff`, there is no two-dot
	// vs three-dot distinction to worry about here.
	out, err := gitOutput(ctx, dir, "rev-list", "--left-right", "--count", base+"...HEAD")
	if err != nil {
		return BranchStatus{}, fmt.Errorf("rev-list: %w", err)
	}
	if fields := strings.Fields(out); len(fields) == 2 {
		s.Behind, _ = strconv.Atoi(fields[0])
		s.Ahead, _ = strconv.Atoi(fields[1])
	}

	// Two-dot, deliberately: diff Base's current tip directly against HEAD,
	// not `base...HEAD`'s merge-base semantics, which would ignore anything
	// that landed on Base after this branch forked from it.
	if diff, err := gitOutput(ctx, dir, "diff", "--shortstat", base, "HEAD"); err == nil {
		s.FilesChanged, s.Insertions, s.Deletions = parseShortstat(diff)
	}
	return s, nil
}

var shortstatRe = regexp.MustCompile(`(\d+) files? changed(?:, (\d+) insertions?\(\+\))?(?:, (\d+) deletions?\(-\))?`)

func parseShortstat(s string) (files, ins, del int) {
	m := shortstatRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, 0
	}
	files, _ = strconv.Atoi(m[1])
	if m[2] != "" {
		ins, _ = strconv.Atoi(m[2])
	}
	if m[3] != "" {
		del, _ = strconv.Atoi(m[3])
	}
	return files, ins, del
}

// defaultBranch guesses the project's main branch: origin/HEAD's target if
// the worktree has a remote tracking it, else whichever of main/master
// exists.
func defaultBranch(ctx context.Context, dir string) (string, error) {
	if out, err := gitOutput(ctx, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); err == nil {
		if name := strings.TrimPrefix(out, "refs/remotes/"); name != out {
			return name, nil
		}
	}
	for _, cand := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, err := gitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", cand); err == nil {
			return cand, nil
		}
	}
	return "", fmt.Errorf("could not determine the project's default branch (tried origin/HEAD, main, master)")
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
