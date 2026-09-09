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
	"github.com/bandito/canaveral/internal/worktree"
)

func TestScratchKeepsRequestedWork(t *testing.T) {
	for _, keepWorktree := range []bool{false, true} {
		t.Run(map[bool]string{false: "branch", true: "worktree"}[keepWorktree], func(t *testing.T) {
			f := gitFeature(t, false)
			f.Scratch = true
			if _, err := worktree.Ensure(context.Background(), f.Root, f.Worktree, f.Branch, ""); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.Worktree, "dirty"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := Remove(context.Background(), f, keepWorktree, false, !keepWorktree, quietReporter{}); err != nil {
				t.Fatal(err)
			}
			if err := exec.Command("git", "-C", f.Root, "show-ref", "--verify", "refs/heads/"+f.Branch).Run(); err != nil {
				t.Fatalf("branch should survive: %v", err)
			}
			_, err := os.Stat(filepath.Join(f.Worktree, "dirty"))
			if keepWorktree && err != nil || !keepWorktree && !os.IsNotExist(err) {
				t.Fatalf("kept worktree = %v, stat = %v", keepWorktree, err)
			}
		})
	}
}

func TestScratchCollisionLeavesNoDisposableRecord(t *testing.T) {
	for _, collision := range []string{"branch", "path"} {
		t.Run(collision, func(t *testing.T) {
			f := gitFeature(t, false)
			t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
			bin := t.TempDir()
			for _, name := range []string{"systemctl", "systemd-run", "hyprctl"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			m := &manifest.Manifest{Name: f.Project, Root: f.Root, Branch: "main", Toolchain: "none"}
			m.Worktree.Root = t.TempDir()
			name := f.Branch
			if collision == "path" {
				name = "scratch-collision"
				root, err := m.WorktreeRoot()
				if err != nil {
					t.Fatal(err)
				}
				path, err := state.WorktreePathIn(root, m.Name, name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(path, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			_, err := Reconcile(context.Background(), m, name, Options{Scratch: true, NoWindows: true, NoServices: true, NoAgents: true}, quietReporter{})
			if err == nil {
				t.Fatal("scratch should reject existing work")
			}
			if _, err := state.Load(m.Name, name); !errors.Is(err, state.ErrNotFound) {
				t.Fatalf("collision left a disposable record: %v", err)
			}
			if err := exec.Command("git", "-C", f.Root, "show-ref", "--verify", "refs/heads/"+f.Branch).Run(); err != nil {
				t.Fatalf("existing branch lost: %v", err)
			}
		})
	}
}

func TestScratchRemovalCanBeRetriedAfterBranchDeletion(t *testing.T) {
	f := gitFeature(t, false)
	f.Scratch = true
	ctx := context.Background()
	if _, err := worktree.Ensure(ctx, f.Root, f.Worktree, f.Branch, ""); err != nil {
		t.Fatal(err)
	}
	if err := removeWorktreeAndBranch(ctx, f, false, false, false, quietReporter{}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, f, false, false, false, quietReporter{}); err != nil {
		t.Fatalf("retry after branch deletion: %v", err)
	}
}
