package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH puts a gh on PATH that prints out for any arguments.
func fakeGH(t *testing.T, out string) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\ncat <<'JSON'\n%s\nJSON\n", out)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// prRepo builds a repo with a branch "feat" one commit past main, returning
// the repo and feat's tip.
func prRepo(t *testing.T) (repo, tip string) {
	t.Helper()
	repo = t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	git("commit", "-q", "--allow-empty", "-m", "init")
	git("checkout", "-q", "-b", "feat")
	git("commit", "-q", "--allow-empty", "-m", "work")
	return repo, git("rev-parse", "HEAD")
}

func TestMergedPRMatchesTheBranchTip(t *testing.T) {
	repo, tip := prRepo(t)
	fakeGH(t, fmt.Sprintf(`[{"number":7,"headRefOid":%q}]`, tip))
	n, ok := MergedPR(context.Background(), repo, "feat")
	if !ok || n != 7 {
		t.Errorf("MergedPR = %d, %v; want 7, true", n, ok)
	}
}

func TestMergedPRIgnoresAPullRequestWithoutTheTip(t *testing.T) {
	// The branch name matched, but this pull request merged other work:
	// an older use of the name, or commits added since it merged.
	repo, _ := prRepo(t)
	fakeGH(t, `[{"number":7,"headRefOid":"0000000000000000000000000000000000000001"}]`)
	if n, ok := MergedPR(context.Background(), repo, "feat"); ok {
		t.Errorf("MergedPR = %d, true; want no match", n)
	}
}

func TestMergedPRWithNoPullRequests(t *testing.T) {
	repo, _ := prRepo(t)
	fakeGH(t, `[]`)
	if _, ok := MergedPR(context.Background(), repo, "feat"); ok {
		t.Error("MergedPR matched with no pull requests")
	}
}

// gitRun runs git in repo, failing the test on error.
func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestMergedLocallyAfterAMergeCommit(t *testing.T) {
	repo, _ := prRepo(t)
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--no-ff", "-m", "merge feat", "feat")
	if !MergedLocally(context.Background(), repo, "feat") {
		t.Error("a branch merged into main is merged")
	}
}

func TestMergedLocallyAfterAFastForward(t *testing.T) {
	// Indistinguishable from a branch that never had a commit by ancestry
	// alone: main now points at feat's tip.
	repo, _ := prRepo(t)
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "merge", "-q", "--ff-only", "feat")
	if !MergedLocally(context.Background(), repo, "feat") {
		t.Error("a branch fast-forwarded into main is merged")
	}
}

func TestMergedLocallyIntoOriginOnly(t *testing.T) {
	repo, _ := prRepo(t)
	gitRun(t, repo, "update-ref", "refs/remotes/origin/main", "feat")
	if !MergedLocally(context.Background(), repo, "feat") {
		t.Error("a branch contained in origin/main is merged")
	}
}

func TestMergedLocallyNotForUnmergedWork(t *testing.T) {
	repo, _ := prRepo(t)
	if MergedLocally(context.Background(), repo, "feat") {
		t.Error("a branch with a commit main lacks is not merged")
	}
}

func TestMergedLocallyNotForABranchWithNoCommits(t *testing.T) {
	// Its tip is a commit of main, so ancestry says merged; it has simply
	// not been worked on yet.
	repo, _ := prRepo(t)
	gitRun(t, repo, "branch", "fresh", "main")
	if MergedLocally(context.Background(), repo, "fresh") {
		t.Error("a branch with no commits of its own is not merged")
	}
}

func TestMergedLocallyNotForAnEmptyBranchRebasedForward(t *testing.T) {
	// Rebasing moves the tip without any work having been done, so it must
	// not count as a commit of the branch's own.
	repo, _ := prRepo(t)
	gitRun(t, repo, "branch", "fresh", "main")
	gitRun(t, repo, "checkout", "-q", "main")
	gitRun(t, repo, "commit", "-q", "--allow-empty", "-m", "upstream")
	gitRun(t, repo, "checkout", "-q", "fresh")
	gitRun(t, repo, "rebase", "-q", "main")
	if MergedLocally(context.Background(), repo, "fresh") {
		t.Error("an empty branch rebased onto main is not merged")
	}
}
