package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBranch(t *testing.T) {
	cases := []struct {
		tmpl string
		vars BranchVars
		want string
	}{
		{"canaveral/{{.Feature}}-{{.Agent}}", BranchVars{Feature: "checkout", Agent: "a1"}, "canaveral/checkout-a1"},
		{"{{.Workspace}}/{{.Agent}}", BranchVars{Workspace: "norules", Agent: "a2"}, "norules/a2"},
		{"feat/{{.Feature}}", BranchVars{Feature: "with space"}, "feat/with-space"},
		{"x/{{.Feature}}", BranchVars{Feature: "a~b^c:d"}, "x/a-b-c-d"},
	}
	for _, c := range cases {
		got, err := RenderBranch(c.tmpl, c.vars)
		if err != nil {
			t.Errorf("RenderBranch(%q): %v", c.tmpl, err)
			continue
		}
		if got != c.want {
			t.Errorf("RenderBranch(%q) = %q, want %q", c.tmpl, got, c.want)
		}
	}
}

func TestRenderBranchErrors(t *testing.T) {
	if _, err := RenderBranch("{{.Nope}}", BranchVars{}); err == nil {
		t.Error("unknown field: want error")
	}
	if _, err := RenderBranch("{{", BranchVars{}); err == nil {
		t.Error("bad template: want error")
	}
	if _, err := RenderBranch("///", BranchVars{}); err == nil {
		t.Error("empty result: want error")
	}
}

func TestSafeJoinRejectsEscape(t *testing.T) {
	base := t.TempDir()
	for _, rel := range []string{"../escape", "a/../../escape", "/etc/passwd"} {
		if _, err := safeJoin(base, rel); err == nil {
			t.Errorf("safeJoin(%q) succeeded, want error", rel)
		}
	}
	got, err := safeJoin(base, "a/b")
	if err != nil {
		t.Fatalf("safeJoin: %v", err)
	}
	if !strings.HasPrefix(got, base) {
		t.Errorf("safeJoin = %q, want prefix %q", got, base)
	}
}

func TestProvisionLinkAndCopy(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	if err := os.WriteFile(filepath.Join(src, ".env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "config", "master.key"), []byte("key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "node_modules", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := Provision{
		Link: []string{"node_modules", "config/master.key", "does-not-exist"},
		Copy: []string{".env"},
	}
	if err := p.Apply(context.Background(), src, dst, func(string, ...any) {}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// node_modules must be a symlink, not a copy.
	st, err := os.Lstat(filepath.Join(dst, "node_modules"))
	if err != nil {
		t.Fatalf("node_modules: %v", err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Error("node_modules is not a symlink")
	}

	// Nested link target must be created with parent dirs.
	if _, err := os.Stat(filepath.Join(dst, "config", "master.key")); err != nil {
		t.Errorf("config/master.key: %v", err)
	}

	// .env must be a real copy, independent of the source.
	envPath := filepath.Join(dst, ".env")
	est, err := os.Lstat(envPath)
	if err != nil {
		t.Fatalf(".env: %v", err)
	}
	if est.Mode()&os.ModeSymlink != 0 {
		t.Error(".env should be copied, not linked")
	}
	b, err := os.ReadFile(envPath)
	if err != nil || string(b) != "SECRET=1" {
		t.Errorf(".env contents = %q, %v", b, err)
	}

	// A missing source is skipped rather than failing the run.
	if _, err := os.Lstat(filepath.Join(dst, "does-not-exist")); err == nil {
		t.Error("missing source should not create a link")
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, ".env"), []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := Provision{Copy: []string{".env"}}
	noop := func(string, ...any) {}
	if err := p.Apply(context.Background(), src, dst, noop); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	// Simulate the agent editing its copy; re-provisioning must not clobber it.
	if err := os.WriteFile(filepath.Join(dst, ".env"), []byte("A=2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(context.Background(), src, dst, noop); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dst, ".env"))
	if string(b) != "A=2" {
		t.Errorf(".env = %q, want preserved A=2", b)
	}
}

func TestProvisionSetupFailureReportsOutput(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	p := Provision{Setup: "echo boom-marker >&2; exit 3"}
	err := p.Apply(context.Background(), src, dst, func(string, ...any) {})
	if err == nil {
		t.Fatal("Apply succeeded, want error")
	}
	if !strings.Contains(err.Error(), "boom-marker") {
		t.Errorf("error missing command output: %v", err)
	}
}

func TestProvisionRejectsEscapingPath(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	p := Provision{Link: []string{"../outside"}}
	if err := p.Apply(context.Background(), src, dst, func(string, ...any) {}); err == nil {
		t.Fatal("Apply with escaping path succeeded, want error")
	}
}

func TestProvisionMergesIntoExistingDirectory(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()

	// The main checkout has a built asset alongside a tracked .keep.
	if err := os.MkdirAll(filepath.Join(src, "app", "assets", "builds"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".keep": "", "tailwind.css": "body{}"} {
		if err := os.WriteFile(filepath.Join(src, "app", "assets", "builds", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The worktree already has the directory because .keep is tracked.
	if err := os.MkdirAll(filepath.Join(dst, "app", "assets", "builds"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "app", "assets", "builds", ".keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	p := Provision{Copy: []string{"app/assets/builds"}}
	if err := p.Apply(context.Background(), src, dst, func(string, ...any) {}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// An existing directory must not cause the gitignored contents to be skipped.
	got, err := os.ReadFile(filepath.Join(dst, "app", "assets", "builds", "tailwind.css"))
	if err != nil {
		t.Fatalf("tailwind.css not copied into existing directory: %v", err)
	}
	if string(got) != "body{}" {
		t.Errorf("contents = %q", got)
	}
}

func TestProvisionDirMergePreservesEdits(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "d", "a.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dst, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "d", "a.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := Provision{Copy: []string{"d"}}
	if err := p.Apply(context.Background(), src, dst, func(string, ...any) {}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dst, "d", "a.txt"))
	if string(got) != "edited" {
		t.Errorf("re-provisioning clobbered an edit: %q", got)
	}
}

func TestMainCheckoutFromLinkedWorktree(t *testing.T) {
	repo := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run(repo, "init", "-q", "-b", "main")
	run(repo, "config", "user.email", "t@t")
	run(repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", "-A")
	run(repo, "commit", "-qm", "init")

	// A feature worktree, carrying a provisioned canaveral.toml exactly as
	// canaveral leaves it. Walking up for that file would stop here and call
	// the worktree the project; MainCheckout must not.
	wt := filepath.Join(t.TempDir(), "feat")
	run(repo, "worktree", "add", "-q", "-b", "feat", wt)
	if err := os.WriteFile(filepath.Join(wt, "canaveral.toml"), []byte("name='x'"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := MainCheckout(context.Background(), wt)
	if err != nil {
		t.Fatalf("MainCheckout: %v", err)
	}
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	gotResolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if gotResolved != want {
		t.Errorf("MainCheckout(worktree) = %q, want %q", gotResolved, want)
	}

	// And it agrees when already in the main checkout.
	got, err = MainCheckout(context.Background(), repo)
	if err != nil {
		t.Fatalf("MainCheckout(repo): %v", err)
	}
	if gotResolved, _ := filepath.EvalSymlinks(got); gotResolved != want {
		t.Errorf("MainCheckout(repo) = %q, want %q", gotResolved, want)
	}
}

func TestIsDirtyIgnoresProvisionedPaths(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	// Mirror a real repo: the builds directory is tracked via .keep while its
	// contents are gitignored.
	if err := os.MkdirAll(filepath.Join(dir, "app", "assets", "builds"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"tracked.txt", "app/assets/builds/.keep"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(f)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	// Files canaveral provisions must not count as user work, otherwise
	// teardown would always demand --force.
	if err := os.WriteFile(filepath.Join(dir, "canaveral.toml"), []byte("name='x'"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "assets", "builds", "t.css"), []byte("a{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	ignore := []string{"canaveral.toml", "app/assets/builds"}
	dirty, err := IsDirty(context.Background(), dir, ignore)
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if dirty {
		t.Error("provisioned files should not make the worktree dirty")
	}

	// Real work must still be protected.
	if err := os.WriteFile(filepath.Join(dir, "feature.rb"), []byte("code"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = IsDirty(context.Background(), dir, ignore)
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if !dirty {
		t.Error("uncommitted work must still be reported as dirty")
	}
}

func TestIsDirtyIgnoresProvisionedSymlinkInsideUntrackedDir(t *testing.T) {
	// Regression test: a provisioned path that is the *only* thing inside an
	// otherwise entirely-untracked directory (like .claude/skills/onboarding,
	// a namespace skill symlink) must not count as dirty. git's default
	// status output collapses a wholly-untracked directory into one line for
	// its container ("?? .claude/"), which never matches an ignore entry for
	// the specific path inside it.
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	elsewhere := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(dir, ".claude", "skills", "onboarding")); err != nil {
		t.Fatal(err)
	}

	dirty, err := IsDirty(context.Background(), dir, []string{".claude/skills/onboarding"})
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if dirty {
		t.Error("a provisioned symlink inside an otherwise-untracked directory should not count as dirty")
	}

	// A genuinely untracked file alongside it must still be caught.
	if err := os.WriteFile(filepath.Join(dir, ".claude", "notes.txt"), []byte("real work"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirty, err = IsDirty(context.Background(), dir, []string{".claude/skills/onboarding"})
	if err != nil {
		t.Fatalf("IsDirty: %v", err)
	}
	if !dirty {
		t.Error("real untracked work alongside the provisioned symlink must still be reported as dirty")
	}
}

func TestRemoveSucceedsWithOnlyProvisionedFiles(t *testing.T) {
	// Regression test: git's own `worktree remove` refuses on any untracked
	// file, including ones canaveral itself provisioned (the copied manifest,
	// built assets). Once IsDirty has cleared the worktree, Remove must not
	// need --force from the caller.
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := Ensure(context.Background(), repo, wt, "feature-x", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	// Simulate what canaveral provisions into every worktree.
	if err := os.WriteFile(filepath.Join(wt, "canaveral.toml"), []byte("name='x'"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(context.Background(), repo, wt, false, []string{"canaveral.toml"}); err != nil {
		t.Fatalf("Remove without --force should succeed when only ignored paths are dirty: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Error("worktree directory should be gone")
	}
}

func TestRemoveStillRefusesRealWorkWithoutForce(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")

	wt := filepath.Join(t.TempDir(), "wt")
	if _, err := Ensure(context.Background(), repo, wt, "feature-y", ""); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "real-work.rb"), []byte("code"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Remove(context.Background(), repo, wt, false, []string{"canaveral.toml"}); err == nil {
		t.Fatal("Remove should refuse real uncommitted work without --force")
	}
	if _, err := os.Stat(wt); err != nil {
		t.Error("worktree should still exist after a refused Remove")
	}
}
