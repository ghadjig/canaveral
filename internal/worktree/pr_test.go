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
