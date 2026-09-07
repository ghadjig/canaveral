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
	changed, err := changedPaths(ctx, dir, ignore)
	return len(changed) > 0, err
}

// CountDirty is IsDirty's count, for a display that wants "3 uncommitted"
// rather than a yes or no.
func CountDirty(ctx context.Context, dir string, ignore []string) (int, error) {
	changed, err := changedPaths(ctx, dir, ignore)
	return len(changed), err
}

// changedPaths lists the worktree-relative paths git reports as changed,
// minus anything canaveral provisioned.
func changedPaths(ctx context.Context, dir string, ignore []string) ([]string, error) {
	// --untracked-files=all is required, not just the default: git otherwise
	// collapses an entirely-untracked directory into one line for its
	// container (e.g. "?? .claude/" instead of "?? .claude/skills/onboarding"),
	// which would never match an ignore entry for the specific path inside
	// it, and get misreported as dirty.
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil {
		return nil, err
	}

	skip := make(map[string]bool, len(ignore))
	// real holds the same entries resolved through any symlink on the way to
	// them, keyed by the absolute path the filesystem actually reaches.
	//
	// The literal comparison below is not enough on its own, because the name
	// canaveral wrote and the name git reports back need not be the same one.
	// A repo that symlinks .claude/skills to .agents/skills takes a link
	// created at ".claude/skills/<ns>" and reports it as
	// "?? .agents/skills/<ns>" — same inode, different string, no match. The
	// skills symlink then counted as the user's own work, so `merge` and
	// `rebase` refused over a file canaveral had put there itself, and `rm`
	// demanded --force. Resolving both sides asks the question that was meant
	// all along: is this the same file?
	real := make(map[string]bool, len(ignore))
	for _, p := range ignore {
		c := strings.TrimSuffix(filepath.Clean(p), "/")
		skip[c] = true
		if r, ok := resolve(dir, c); ok {
			real[r] = true
		}
	}

	var changed []string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[2:])
		path = strings.Trim(path, "\"")
		// A rename reads "old -> new"; the destination is the path on disk.
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if covered(skip, strings.TrimSuffix(filepath.Clean(path), "/")) {
			continue
		}
		// Only worth resolving once the cheap comparison has failed. A path
		// that no longer exists — a provisioned file the user deleted — will
		// not resolve, and is correctly left counted as a change.
		if r, ok := resolve(dir, path); ok && covered(real, r) {
			continue
		}
		changed = append(changed, path)
	}
	return changed, nil
}

// covered reports whether p is in set, or lies beneath something in it. git
// reports untracked directories with a trailing slash, and an ignored entry
// that is a real directory has its contents listed individually.
func covered(set map[string]bool, p string) bool {
	if set[p] {
		return true
	}
	for entry := range set {
		if strings.HasPrefix(p, entry+"/") {
			return true
		}
	}
	return false
}

// resolve gives the absolute path rel actually reaches from dir, following
// symlinks, or false when there is nothing there to follow.
func resolve(dir, rel string) (string, bool) {
	p, err := filepath.EvalSymlinks(filepath.Join(dir, rel))
	if err != nil {
		return "", false
	}
	return p, true
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

// BaseRef resolves the ref a new feature branch should start from: the local
// default branch, or its remote-tracking counterpart when there is no local
// copy of it.
//
// This exists because the alternative is whatever the main checkout happens to
// have checked out, which is what `git worktree add -b` uses when given no
// start point, and which is nobody's intention. A feature cut while the
// checkout sat on an old topic branch inherited that branch's tree: in the
// case that prompted this, 4434 commits behind master and five weeks older
// than a script the feature's own service needed, so the service could not
// start and no amount of `canaveral reset` would have helped.
//
// DefaultBranch names the branch that `merge` merges into and that `rm`
// checks a feature against before deleting it. Starting there means a feature
// forks from, and lands back on, the same place.
//
// The remote fallback covers a fresh clone that has never checked the default
// branch out locally: origin/HEAD names it, but refs/heads does not have it
// yet.
func BaseRef(ctx context.Context, repo string) (string, error) {
	name, err := DefaultBranch(ctx, repo)
	if err != nil {
		return "", err
	}
	if branchExists(ctx, repo, name) {
		return name, nil
	}
	if remote := "origin/" + name; refExists(ctx, repo, remote) {
		return remote, nil
	}
	return "", fmt.Errorf("default branch %q exists neither locally nor on origin", name)
}

// refExists reports whether any ref — branch, tag, remote-tracking — resolves.
func refExists(ctx context.Context, repo, ref string) bool {
	return exec.CommandContext(ctx, "git", "-C", repo,
		"rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil
}

// HasRemote reports whether the repository has a remote of the given name.
func HasRemote(ctx context.Context, dir, remote string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir,
		"config", "--get", "remote."+remote+".url").Run() == nil
}

// Fetch updates the remote-tracking refs for remote.
func Fetch(ctx context.Context, dir, remote string) error {
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "fetch", remote)
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git fetch %s: %w: %s", remote, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// RemoteDefaultBranch returns the remote-tracking ref for the remote's default
// branch, like "origin/main" — the right thing to rebase onto, since it is
// what the remote actually holds rather than whatever a stale local branch of
// the same name happens to point at.
func RemoteDefaultBranch(ctx context.Context, dir, remote string) (string, error) {
	if out, err := exec.CommandContext(ctx, "git", "-C", dir,
		"symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" {
			return name, nil
		}
	}
	// origin/HEAD is only written at clone time, and never for a remote added
	// afterwards, so falling back to the conventional names is the common path
	// rather than the exception.
	for _, name := range []string{"main", "master"} {
		ref := remote + "/" + name
		if revExists(ctx, dir, "refs/remotes/"+ref) {
			return ref, nil
		}
	}
	return "", fmt.Errorf("could not determine %s's default branch (tried %s/HEAD, %s/main, %s/master); pass --onto explicitly",
		remote, remote, remote, remote)
}

func revExists(ctx context.Context, dir, rev string) bool {
	return exec.CommandContext(ctx, "git", "-C", dir,
		"rev-parse", "--verify", "--quiet", rev).Run() == nil
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

// ErrRebaseConflict reports a rebase that stopped on a conflict and was left
// in progress for the user to finish or abort by hand.
var ErrRebaseConflict = errors.New("rebase stopped on a conflict")

// Rebase replays dir's checked-out branch on top of onto, aborting cleanly
// (rather than leaving a half-finished rebase behind) if it conflicts.
func Rebase(ctx context.Context, dir, onto string) error {
	return rebase(ctx, dir, onto, true)
}

// RebaseKeepingConflicts is Rebase, but leaves a conflicted rebase in progress
// so it can be resolved with `git rebase --continue`, returning an error
// wrapping ErrRebaseConflict. Aborting is right when the rebase is one step of
// a larger operation that must not half-finish; it is wrong when the rebase is
// the whole point of the command, since throwing the work away is exactly what
// the user does not want in the case they most needed the command for.
func RebaseKeepingConflicts(ctx context.Context, dir, onto string) error {
	return rebase(ctx, dir, onto, false)
}

func rebase(ctx context.Context, dir, onto string, abortOnConflict bool) error {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rebase", onto)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		if abortOnConflict {
			_ = exec.CommandContext(ctx, "git", "-C", dir, "rebase", "--abort").Run()
			return fmt.Errorf("rebase onto %s: %w: %s", onto, err, strings.TrimSpace(out.String()))
		}
		if RebaseInProgress(ctx, dir) {
			return fmt.Errorf("rebase onto %s: %w: %s", onto, ErrRebaseConflict, strings.TrimSpace(out.String()))
		}
		// Failed before touching anything — a bad ref, say. Nothing is in
		// progress, so there is nothing to resolve or abort.
		return fmt.Errorf("rebase onto %s: %w: %s", onto, err, strings.TrimSpace(out.String()))
	}
	return nil
}

// RebaseInProgress reports whether dir has a rebase stopped part-way through.
func RebaseInProgress(ctx context.Context, dir string) bool {
	for _, name := range []string{"rebase-merge", "rebase-apply"} {
		out, err := exec.CommandContext(ctx, "git", "-C", dir,
			"rev-parse", "--git-path", name).Output()
		if err != nil {
			continue
		}
		path := strings.TrimSpace(string(out))
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
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
