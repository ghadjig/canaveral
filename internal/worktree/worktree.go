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
