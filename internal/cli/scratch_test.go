package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/state"
)

func TestScratchLifecycle(t *testing.T) {
	clearFeatureEnv(t)
	isolateDirs(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("CANAVERAL_AGENT", "")
	bin := t.TempDir()
	for _, name := range []string{"systemctl", "systemd-run", "hyprctl"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo := t.TempDir()
	t.Chdir(repo)
	git := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git(repo, "init", "-q", "-b", "main")
	git(repo, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "--allow-empty", "-qm", "initial")
	base := git(repo, "rev-parse", "HEAD")
	git(repo, "checkout", "-qb", "experiment")
	git(repo, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "--allow-empty", "-qm", "experiment")
	head := git(repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "canaveral.toml"), []byte("name = 'scratch-test'\nbranch = 'main'\ntoolchain = 'none'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	flags := []string{"--no-windows", "--no-services", "--no-agents"}
	if err := runScratch(ctx, flags); err != nil {
		t.Fatal(err)
	}
	first, err := state.List("scratch-test")
	if err != nil || len(first) != 1 {
		t.Fatalf("features = %v, %v", first, err)
	}
	if err := runScratch(ctx, append(flags, "--base", "HEAD")); err != nil {
		t.Fatal(err)
	}
	names, err := state.List("scratch-test")
	if err != nil || len(names) != 2 {
		t.Fatalf("features = %v, %v; want two fresh workspaces", names, err)
	}
	for _, name := range names {
		f, err := state.Load("scratch-test", name)
		if err != nil {
			t.Fatal(err)
		}
		if !f.Scratch || !strings.HasPrefix(name, "scratch-") || f.Branch != name {
			t.Fatalf("scratch identity = %+v", f)
		}
		want := head
		if name == first[0] {
			want = base
		}
		if got := git(f.Worktree, "rev-parse", "HEAD"); got != want {
			t.Fatalf("base = %s, want %s", got, want)
		}
		if err := runOpen(ctx, append([]string{name}, flags...)); err != nil {
			t.Fatal(err)
		}
		f, err = state.Load(f.Project, name)
		if err != nil || !f.Scratch {
			t.Fatalf("reopen lost scratch marker: %+v, %v", f, err)
		}
		git(f.Worktree, "-c", "user.name=test", "-c", "user.email=test@test", "commit", "--allow-empty", "-qm", "throwaway commit")
		if err := os.WriteFile(filepath.Join(f.Worktree, "throwaway.txt"), []byte("uncommitted work"), 0o644); err != nil {
			t.Fatal(err)
		}
		if name == first[0] {
			// Current-worktree removal needs no generated name either.
			t.Chdir(f.Worktree)
			if err := runRm(ctx, nil); err != nil {
				t.Fatal(err)
			}
			t.Chdir(repo)
		} else {
			if err := runStash(ctx, []string{name}); err != nil {
				t.Fatal(err)
			}
			if err := runPop(ctx, append([]string{name}, flags...)); err != nil {
				t.Fatal(err)
			}
			if err := runStash(ctx, []string{name}); err != nil {
				t.Fatal(err)
			}
			if err := runRm(ctx, []string{name}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := os.Stat(f.Worktree); !os.IsNotExist(err) {
			t.Fatalf("scratch worktree remains: %v", err)
		}
		if got := git(repo, "branch", "--list", f.Branch); got != "" {
			t.Fatalf("scratch branch remains: %s", got)
		}
		if _, err := state.Load(f.Project, name); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("scratch state remains: %v", err)
		}
		if _, err := state.LoadStash(f.Project, name); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("scratch stash remains: %v", err)
		}
	}
}

func TestScratchRejectsNamesAndUnknownFlags(t *testing.T) {
	for _, args := range [][]string{{"my-branch"}, {"--unknown"}} {
		if err := runScratch(context.Background(), args); err == nil {
			t.Fatalf("scratch %v should fail", args)
		}
	}
}
