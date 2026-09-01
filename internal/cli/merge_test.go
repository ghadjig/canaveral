package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bandito/canaveral/internal/worktree"
)

func TestEnsureCheckedOutSwitchesBranch(t *testing.T) {
	dir := gitFeatureRepo(t)
	// gitFeatureRepo leaves "feat" checked out.
	if err := ensureCheckedOut(context.Background(), dir, "main"); err != nil {
		t.Fatalf("ensureCheckedOut: %v", err)
	}
	cur, err := worktree.CurrentBranch(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if cur != "main" {
		t.Errorf("current branch = %q, want main", cur)
	}
}

func TestEnsureCheckedOutIsANoopWhenAlreadyThere(t *testing.T) {
	dir := gitFeatureRepo(t)
	if err := ensureCheckedOut(context.Background(), dir, "feat"); err != nil {
		t.Errorf("ensureCheckedOut: %v", err)
	}
}

func TestEnsureCheckedOutRefusesADirtyRoot(t *testing.T) {
	dir := gitFeatureRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureCheckedOut(context.Background(), dir, "main"); err == nil {
		t.Error("ensureCheckedOut should refuse to switch over uncommitted changes")
	}
	cur, err := worktree.CurrentBranch(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if cur != "feat" {
		t.Errorf("current branch = %q, want it left alone on feat", cur)
	}
}
