package feature

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// countingGH puts a gh on PATH that answers with out and records each call
// in a file, returning a function that reports how many calls were made.
func countingGH(t *testing.T, out string) func() int {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := fmt.Sprintf("#!/bin/sh\necho x >> %q\ncat <<'JSON'\n%s\nJSON\n", log, out)
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "x")
	}
}

func branchTip(t *testing.T, repo, branch string) string {
	t.Helper()
	b, err := exec.Command("git", "-C", repo, "rev-parse", branch).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestMergedPRRemembersAMergeForTheSameTip(t *testing.T) {
	f := gitFeature(t, false)
	calls := countingGH(t, fmt.Sprintf(`[{"number":9,"headRefOid":%q}]`, branchTip(t, f.Root, f.Branch)))
	for i := 0; i < 3; i++ {
		if n, ok := MergedPR(context.Background(), f.Root, f.Branch); !ok || n != 9 {
			t.Fatalf("MergedPR = %d, %v; want 9, true", n, ok)
		}
	}
	if got := calls(); got != 1 {
		t.Errorf("gh called %d times, want 1: a merge does not unmerge", got)
	}
}

func TestMergedPRAsksAgainOnceANewCommitLands(t *testing.T) {
	f := gitFeature(t, false)
	calls := countingGH(t, fmt.Sprintf(`[{"number":9,"headRefOid":%q}]`, branchTip(t, f.Root, f.Branch)))
	MergedPR(context.Background(), f.Root, f.Branch)
	gitIn(t, f.Root, "checkout", "-q", f.Branch)
	gitIn(t, f.Root, "commit", "-q", "--allow-empty", "-m", "more")
	if _, ok := MergedPR(context.Background(), f.Root, f.Branch); ok {
		t.Error("a commit made after the pull request merged is not merged")
	}
	if got := calls(); got != 2 {
		t.Errorf("gh called %d times, want 2", got)
	}
}

func TestMergedPRRemembersNotMergedOnlyBriefly(t *testing.T) {
	f := gitFeature(t, false)
	calls := countingGH(t, `[]`)
	MergedPR(context.Background(), f.Root, f.Branch)
	MergedPR(context.Background(), f.Root, f.Branch)
	if got := calls(); got != 1 {
		t.Fatalf("gh called %d times within the TTL, want 1", got)
	}
	// Age the entry past the TTL rather than sleeping through it.
	c := loadMergedCache()
	for k, e := range c {
		e.Checked = time.Now().Add(-unmergedTTL - time.Second)
		c[k] = e
	}
	saveMergedCache(c)
	MergedPR(context.Background(), f.Root, f.Branch)
	if got := calls(); got != 2 {
		t.Errorf("gh called %d times after the TTL, want 2", got)
	}
}
