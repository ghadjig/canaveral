package feature

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/tmpl"
	"github.com/bandito/canaveral/internal/worktree"
)

func TestLinkNamespaceSkillNoopWithoutANamespace(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "small-fixes", Worktree: t.TempDir()}
	linkNamespaceSkill(f, quietReporter{})
	if len(f.Provisioned) != 0 {
		t.Errorf("Provisioned = %v, want untouched for an unnamespaced feature", f.Provisioned)
	}
}

func TestLinkNamespaceSkillLinksAndRecordsProvisioned(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt := t.TempDir()
	f := &state.Feature{Project: "p", Name: "onboarding/step1", Worktree: wt}

	linkNamespaceSkill(f, quietReporter{})

	if len(f.Provisioned) != 1 {
		t.Fatalf("Provisioned = %v, want exactly one entry", f.Provisioned)
	}
	link := filepath.Join(wt, f.Provisioned[0])
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected a symlink at %s: %v", link, err)
	}
}

func TestRunDatabaseSetupNoopWhenNotDeclared(t *testing.T) {
	f := &state.Feature{Project: "p", Name: "f", Worktree: t.TempDir()}
	m := &manifest.Manifest{}
	if err := runDatabaseSetup(context.Background(), m, f, tmpl.Vars{}, nil, quietReporter{}); err != nil {
		t.Errorf("runDatabaseSetup: %v", err)
	}
}

func TestRunDatabaseSetupRunsTheDeclaredHook(t *testing.T) {
	wt := t.TempDir()
	f := &state.Feature{Project: "p", Name: "f", Worktree: wt}
	m := &manifest.Manifest{}
	m.Database.Setup = "touch marker"

	if err := runDatabaseSetup(context.Background(), m, f, tmpl.Vars{}, nil, quietReporter{}); err != nil {
		t.Fatalf("runDatabaseSetup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "marker")); err != nil {
		t.Errorf("database setup did not run in the worktree: %v", err)
	}
}

func TestProvisionWorktreeCopiesTheManifestByDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, manifest.FileName), []byte("name = \"p\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	m := &manifest.Manifest{Root: root, Name: "p", Toolchain: "none"}
	f := &state.Feature{Project: "p", Name: "f", Worktree: dst}

	if _, err := provisionWorktree(context.Background(), m, f, tmpl.Vars{}, quietReporter{}); err != nil {
		t.Fatalf("provisionWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, manifest.FileName)); err != nil {
		t.Errorf("manifest not copied into worktree: %v", err)
	}
	found := false
	for _, p := range f.Provisioned {
		if p == manifest.FileName {
			found = true
		}
	}
	if !found {
		t.Errorf("Provisioned = %v, want it to include %q", f.Provisioned, manifest.FileName)
	}
}

func TestProvisionWorktreeRunsItsSetupCommand(t *testing.T) {
	root := t.TempDir()
	dst := t.TempDir()
	m := &manifest.Manifest{Root: root, Name: "p", Toolchain: "none"}
	m.Worktree.Setup = "touch installed"
	f := &state.Feature{Project: "p", Name: "f", Worktree: dst}

	if _, err := provisionWorktree(context.Background(), m, f, tmpl.Vars{}, quietReporter{}); err != nil {
		t.Fatalf("provisionWorktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "installed")); err != nil {
		t.Errorf("worktree setup did not run: %v", err)
	}
}

// switchedBranchFeature returns a feature whose worktree exists but has been
// moved onto another branch, the way an agent leaves it after starting the
// next part of a stacked series.
func switchedBranchFeature(t *testing.T) func(created bool) error {
	t.Helper()
	f := gitFeature(t, false)
	ctx := context.Background()
	if _, err := worktree.Ensure(ctx, f.Root, f.Worktree, f.Branch, ""); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", f.Worktree, "checkout", "-q", "-b", "feat-part-2").CombinedOutput(); err != nil {
		t.Fatalf("checkout: %v\n%s", err, out)
	}
	m := &manifest.Manifest{Name: f.Project, Root: f.Root, Toolchain: "none"}
	return func(created bool) error {
		return ensureWorktree(ctx, m, f, tmpl.Vars{}, Options{}, created, quietReporter{})
	}
}

func TestEnsureWorktreeKeepsAnExistingFeatureOnItsSwitchedBranch(t *testing.T) {
	ensure := switchedBranchFeature(t)
	if err := ensure(false); err != nil {
		t.Fatalf("reset refused a feature whose worktree switched branches: %v", err)
	}
}

func TestEnsureWorktreeStillRefusesANewFeatureOnAnotherBranch(t *testing.T) {
	ensure := switchedBranchFeature(t)
	var mismatch *worktree.BranchMismatchError
	if err := ensure(true); !errors.As(err, &mismatch) {
		t.Fatalf("a new feature landing on another branch's worktree should be refused, got %v", err)
	}
}
