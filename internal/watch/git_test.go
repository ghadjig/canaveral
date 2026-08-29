package watch

import (
	"context"
	"errors"
	"testing"

	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/worktree"
)

func gitFeature(project, name, dir string) *state.Feature {
	return &state.Feature{Project: project, Name: name, Worktree: dir}
}

func TestGitCacheRefreshReportsChange(t *testing.T) {
	c := newGitCache()
	stat := worktree.BranchStatus{Base: "origin/main", Ahead: 2, Insertions: 10, Deletions: 3}
	c.status = func(ctx context.Context, dir string) (worktree.BranchStatus, error) {
		return stat, nil
	}
	fs := []*state.Feature{gitFeature("p", "f", "/tmp/x")}

	if !c.refresh(context.Background(), fs) {
		t.Fatal("first refresh should report a change")
	}
	got := c.get("p/f")
	if got == nil {
		t.Fatal("expected a measurement to be cached")
	}
	if got.Ahead != 2 || got.Insertions != 10 || got.Deletions != 3 || got.Base != "origin/main" {
		t.Fatalf("cached the wrong values: %+v", got)
	}

	// Nothing moved, so nothing should be re-emitted.
	if c.refresh(context.Background(), fs) {
		t.Error("an unchanged refresh should not report a change")
	}

	stat.Ahead = 3
	if !c.refresh(context.Background(), fs) {
		t.Error("a changed commit count should report a change")
	}
}

// A transient git failure (an index.lock during a commit, say) must not blank
// the numbers a widget is already showing.
func TestGitCacheKeepsLastGoodOnError(t *testing.T) {
	c := newGitCache()
	fail := false
	c.status = func(ctx context.Context, dir string) (worktree.BranchStatus, error) {
		if fail {
			return worktree.BranchStatus{}, errors.New("index.lock exists")
		}
		return worktree.BranchStatus{Base: "main", Ahead: 7}, nil
	}
	fs := []*state.Feature{gitFeature("p", "f", "/tmp/x")}

	c.refresh(context.Background(), fs)
	fail = true
	if c.refresh(context.Background(), fs) {
		t.Error("a failed remeasure should not look like a change")
	}
	got := c.get("p/f")
	if got == nil || got.Ahead != 7 {
		t.Fatalf("last good value should survive a failure, got %+v", got)
	}
}

// A feature with no worktree has nothing to measure and must not appear.
func TestGitCacheSkipsFeaturesWithoutWorktree(t *testing.T) {
	c := newGitCache()
	called := 0
	c.status = func(ctx context.Context, dir string) (worktree.BranchStatus, error) {
		called++
		return worktree.BranchStatus{}, nil
	}
	c.refresh(context.Background(), []*state.Feature{gitFeature("p", "f", "")})
	if called != 0 {
		t.Errorf("worktree-less feature should not be measured, called %d times", called)
	}
	if c.get("p/f") != nil {
		t.Error("worktree-less feature should not be cached")
	}
}

// Zero is a real answer ("nothing committed yet"), and has to be
// distinguishable from "not measured".
func TestGitCacheZeroIsDistinctFromUnmeasured(t *testing.T) {
	c := newGitCache()
	c.status = func(ctx context.Context, dir string) (worktree.BranchStatus, error) {
		return worktree.BranchStatus{Base: "main"}, nil
	}
	if c.get("p/f") != nil {
		t.Fatal("nothing measured yet should be nil")
	}
	c.refresh(context.Background(), []*state.Feature{gitFeature("p", "f", "/tmp/x")})
	got := c.get("p/f")
	if got == nil {
		t.Fatal("a measured all-zero status should still be cached")
	}
	if got.Ahead != 0 || got.Uncommitted != 0 {
		t.Fatalf("expected zeros, got %+v", got)
	}
}
