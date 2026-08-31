// Package worktree manages per-agent git worktrees for isolated workspaces.
package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

// BranchVars are the values available to a manifest branch template.
type BranchVars struct {
	Workspace string
	Feature   string
	Agent     string
}

// RenderBranch evaluates a branch name template.
func RenderBranch(tmpl string, v BranchVars) (string, error) {
	t, err := template.New("branch").Option("missingkey=error").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse branch template: %w", err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		return "", fmt.Errorf("render branch template: %w", err)
	}
	name := sanitizeRef(b.String())
	if name == "" {
		return "", errors.New("branch template produced an empty name")
	}
	return name, nil
}

// sanitizeRef removes characters git refuses in ref names.
func sanitizeRef(s string) string {
	s = strings.TrimSpace(s)
	repl := strings.NewReplacer(" ", "-", "~", "-", "^", "-", ":", "-",
		"?", "-", "*", "-", "[", "-", "\\", "-", "..", "-", "@{", "-")
	s = repl.Replace(s)
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return strings.Trim(s, "/-.")
}

// IsRepo reports whether dir is inside a git working tree.
func IsRepo(ctx context.Context, dir string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// Result describes a prepared agent working directory.
type Result struct {
	Dir     string
	Branch  string
	Created bool
}

// Ensure creates (or reuses) a worktree at path checked out on branch.
//
// Reusing an existing worktree is deliberate: re-running `canaveral up` after a
// crash must not discard uncommitted agent work.
func Ensure(ctx context.Context, repo, path, branch, base string) (Result, error) {
	res := Result{Dir: path, Branch: branch}

	if st, err := os.Stat(filepath.Join(path, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
		cur, err := currentBranch(ctx, path)
		if err != nil {
			return res, err
		}
		if cur != branch {
			return res, fmt.Errorf("worktree %s is on branch %q, expected %q; "+
				"remove it or pick another feature name", path, cur, branch)
		}
		return res, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return res, err
	}

	args := []string{"-C", repo, "worktree", "add"}
	if branchExists(ctx, repo, branch) {
		args = append(args, path, branch)
	} else {
		args = append(args, "-b", branch, path)
		if base != "" {
			args = append(args, base)
		}
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("git worktree add: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	res.Created = true
	return res, nil
}

// Remove detaches a worktree. It refuses to delete one holding uncommitted work
// unless force is set.
func Remove(ctx context.Context, repo, path string, force bool, ignore []string) error {
	if !force {
		dirty, err := IsDirty(ctx, path, ignore)
		if err == nil && dirty {
			return fmt.Errorf("worktree %s has uncommitted changes; "+
				"commit them or re-run with --force", path)
		}
	}
	// git's own worktree remove refuses on any untracked file, including ones
	// our ignore-aware IsDirty already cleared (the copied manifest, built
	// assets). Once canaveral has decided removal is safe, --force is passed to
	// git unconditionally so that decision is not second-guessed.
	args := []string{"-C", repo, "worktree", "remove", "--force", path}
	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "is not a working tree") || os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("git worktree remove %s: %w: %s", path, err, msg)
	}
	return nil
}

// IsDirty reports whether a working tree has changes, ignoring paths canaveral
// itself provisioned.
//
// Without the exclusions the copied manifest and build artifacts would make
// every worktree permanently dirty, so teardown would always demand --force and
// the check could no longer protect real work.
func IsDirty(ctx context.Context, dir string, ignore []string) (bool, error) {
	// --untracked-files=all is required, not just the default: git otherwise
	// collapses an entirely-untracked directory into one line for its
	// container (e.g. "?? .claude/" instead of "?? .claude/skills/onboarding"),
	// which would never match an ignore entry for the specific path inside
	// it, and get misreported as dirty.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return false, err
	}
	skip := make(map[string]bool, len(ignore))
	for _, p := range ignore {
		skip[strings.TrimSuffix(filepath.Clean(p), "/")] = true
	}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[2:])
		path = strings.Trim(path, "\"")
		if skip[strings.TrimSuffix(filepath.Clean(path), "/")] {
			continue
		}
		// git reports untracked directories with a trailing slash.
		covered := false
		for ig := range skip {
			if strings.HasPrefix(path, ig+"/") {
				covered = true
				break
			}
		}
		if !covered {
			return true, nil
		}
	}
	return false, nil
}

// Prune removes administrative files for worktrees whose directories are gone.
func Prune(ctx context.Context, repo string) error {
	return exec.CommandContext(ctx, "git", "-C", repo, "worktree", "prune").Run()
}

func branchExists(ctx context.Context, repo, branch string) bool {
	return exec.CommandContext(ctx, "git", "-C", repo,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("read branch of %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentBranch returns the checked-out branch name for a working tree.
func CurrentBranch(ctx context.Context, dir string) (string, error) {
	return currentBranch(ctx, dir)
}

// MainCheckout returns the repository's primary working tree, given any
// directory inside it — including a linked worktree.
//
// Asking git is the only reliable answer. Locating the project by walking up
// for canaveral.toml finds the *provisioned copy* when run from inside a
// feature worktree, which would report the worktree as if it were the project.
// The common git dir belongs to the main checkout by definition, so its parent
// is the main checkout no matter where the command was run from.
func MainCheckout(ctx context.Context, dir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("resolve main checkout of %s: %w", dir, err)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("resolve main checkout of %s: empty git dir", dir)
	}
	// A bare repo has no working tree to return.
	return filepath.Dir(gitDir), nil
}

// DefaultBranch guesses a repo's main integration branch: the remote's HEAD
// if one is configured, falling back to a local "main" or "master".
func DefaultBranch(ctx context.Context, repo string) (string, error) {
	if out, err := exec.CommandContext(ctx, "git", "-C", repo,
		"symbolic-ref", "--short", "refs/remotes/origin/HEAD").Output(); err == nil {
		if name := strings.TrimPrefix(strings.TrimSpace(string(out)), "origin/"); name != "" {
			return name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if branchExists(ctx, repo, name) {
			return name, nil
		}
	}
	return "", errors.New("could not determine the default branch; pass --into explicitly")
}

// Checkout switches repo's working tree to branch.
func Checkout(ctx context.Context, repo, branch string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "checkout", branch)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git checkout %s: %w: %s", branch, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Rebase replays dir's checked-out branch on top of onto, aborting cleanly
// (rather than leaving a half-finished rebase behind) if it conflicts.
func Rebase(ctx context.Context, dir, onto string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rebase", onto)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_ = exec.CommandContext(ctx, "git", "-C", dir, "rebase", "--abort").Run()
		return fmt.Errorf("rebase onto %s: %w: %s", onto, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// MergeBranch merges branch into repo's currently checked-out branch, aborting
// cleanly on conflict. With ffOnly it refuses to create a merge commit at all;
// otherwise it always creates one (--no-ff), even when a fast-forward would
// do, so the merge point stays visible in history.
func MergeBranch(ctx context.Context, repo, branch string, ffOnly bool) error {
	args := []string{"-C", repo, "merge"}
	if ffOnly {
		args = append(args, "--ff-only")
	} else {
		args = append(args, "--no-ff", "-m", "Merge branch '"+branch+"'")
	}
	args = append(args, branch)

	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		_ = exec.CommandContext(ctx, "git", "-C", repo, "merge", "--abort").Run()
		return fmt.Errorf("merge %s: %w: %s", branch, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// IsMerged reports whether branch's history is fully contained in target.
func IsMerged(ctx context.Context, repo, branch, target string) (bool, error) {
	err := exec.CommandContext(ctx, "git", "-C", repo,
		"merge-base", "--is-ancestor", branch, target).Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// DeleteBranch removes a local branch. force uses -D instead of -d, allowing
// deletion of a branch that is not merged into its upstream.
func DeleteBranch(ctx context.Context, repo, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "branch", flag, branch)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git branch %s %s: %w: %s", flag, branch, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
