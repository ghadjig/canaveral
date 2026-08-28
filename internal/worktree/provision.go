package worktree

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Provision brings gitignored-but-required files into a fresh worktree and runs
// the setup command.
//
// git worktree add checks out tracked files only, so without this an agent's
// worktree is missing .env, credential keys and installed dependencies.
type Provision struct {
	Link         []string
	Copy         []string
	Setup        string
	SetupTimeout time.Duration
	Env          map[string]string
}

// Apply provisions dst from the main checkout at src.
func (p Provision) Apply(ctx context.Context, src, dst string, log func(string, ...any)) error {
	for _, rel := range p.Link {
		if err := linkPath(src, dst, rel, log); err != nil {
			return err
		}
	}
	for _, rel := range p.Copy {
		if err := copyPath(src, dst, rel, log); err != nil {
			return err
		}
	}
	if strings.TrimSpace(p.Setup) == "" {
		return nil
	}

	timeout := p.SetupTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log("running worktree setup: %s", p.Setup)
	cmd := exec.CommandContext(runCtx, "/bin/sh", "-c", p.Setup)
	cmd.Dir = dst
	cmd.Env = append(os.Environ(), envSlice(p.Env)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("worktree setup failed in %s: %w\n%s", dst, err, indent(string(out), 15))
	}
	return nil
}

func envSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

// safeJoin rejects paths that would escape the worktree.
func safeJoin(base, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("worktree path %q must be relative", rel)
	}
	joined := filepath.Join(base, rel)
	clean := filepath.Clean(joined)
	if clean != base && !strings.HasPrefix(clean, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("worktree path %q escapes the worktree", rel)
	}
	return clean, nil
}

func linkPath(src, dst, rel string, log func(string, ...any)) error {
	from, err := safeJoin(src, rel)
	if err != nil {
		return err
	}
	to, err := safeJoin(dst, rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(from); err != nil {
		// A missing optional artifact is not fatal; the agent may not need it.
		log("skip link %s (not present in main checkout)", rel)
		return nil
	}
	if _, err := os.Lstat(to); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := os.Symlink(from, to); err != nil {
		return fmt.Errorf("link %s: %w", rel, err)
	}
	log("linked %s", rel)
	return nil
}

func copyPath(src, dst, rel string, log func(string, ...any)) error {
	from, err := safeJoin(src, rel)
	if err != nil {
		return err
	}
	to, err := safeJoin(dst, rel)
	if err != nil {
		return err
	}
	st, err := os.Lstat(from)
	if err != nil {
		log("skip copy %s (not present in main checkout)", rel)
		return nil
	}

	if st.IsDir() {
		// Merge rather than skip. A tracked .keep file means the directory
		// already exists in the worktree while its gitignored contents (built
		// assets, for example) are missing.
		n, err := copyDir(from, to)
		if err != nil {
			return fmt.Errorf("copy %s: %w", rel, err)
		}
		if n > 0 {
			log("copied %s (%d file(s))", rel, n)
		}
		return nil
	}

	// Existing files are left alone so re-provisioning never discards edits.
	if _, err := os.Lstat(to); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	if err := copyFile(from, to, st); err != nil {
		return fmt.Errorf("copy %s: %w", rel, err)
	}
	log("copied %s", rel)
	return nil
}

func copyFile(from, to string, st os.FileInfo) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

// copyDir merges src into dst, returning how many files it created.
// Entries already present in dst are preserved.
func copyDir(from, to string) (int, error) {
	n := 0
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			dest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if err := os.Symlink(dest, target); err != nil {
				return err
			}
			n++
			return nil
		}
		if err := copyFile(path, target, info); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}

func indent(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}
