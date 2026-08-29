package watch

import (
	"context"
	"sync"
	"time"

	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/worktree"
)

// gitCache holds each feature's branch status, refreshed on its own slow
// ticker rather than as part of a snapshot rebuild.
//
// This exists because the two have wildly different costs. A rebuild is
// cheap — small JSON files and HTTP probes — and runs on a 150ms debounce
// plus a 3s rescan. worktree.Status is not cheap: it spawns three to six git
// subprocesses per feature and touches the object store. Computing it inline
// would put dozens of git processes a second into a daemon that otherwise
// does no repository I/O at all.
//
// So the snapshot always reports the last measured value, and a background
// refresh updates it every Options.Git. A stale "+148 -32" for a few seconds
// is not a problem; a status bar that makes the machine work is.
type gitCache struct {
	mu    sync.Mutex
	byKey map[string]*Git

	// status is swappable so tests do not need real git repositories.
	status func(ctx context.Context, dir string) (worktree.BranchStatus, error)
}

func newGitCache() *gitCache {
	return &gitCache{
		byKey:  map[string]*Git{},
		status: worktree.Status,
	}
}

// get returns the last measured status for a feature, or nil if it has not
// been measured yet.
func (c *gitCache) get(key string) *Git {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byKey[key]
}

// refresh remeasures every feature concurrently and reports whether anything
// changed, so the caller can decide whether a snapshot is worth emitting.
//
// Features are measured in parallel for the same reason agents are probed in
// parallel: serial git would make the refresh duration scale with the number
// of features.
func (c *gitCache) refresh(ctx context.Context, features []*state.Feature) bool {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		next = map[string]*Git{}
	)
	for _, f := range features {
		if f.Worktree == "" {
			continue
		}
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := c.status(ctx, f.Worktree)
			if err != nil {
				// Leave the previous value in place rather than blanking the
				// card: a transient git failure (an index.lock during a
				// commit, say) should not make the numbers disappear.
				if prev := c.get(f.Key()); prev != nil {
					mu.Lock()
					next[f.Key()] = prev
					mu.Unlock()
				}
				return
			}
			g := &Git{
				Base:         s.Base,
				Ahead:        s.Ahead,
				Behind:       s.Behind,
				FilesChanged: s.FilesChanged,
				Insertions:   s.Insertions,
				Deletions:    s.Deletions,
				Uncommitted:  s.Uncommitted,
			}
			mu.Lock()
			next[f.Key()] = g
			mu.Unlock()
		}()
	}
	wg.Wait()

	c.mu.Lock()
	changed := !sameGitMap(c.byKey, next)
	c.byKey = next
	c.mu.Unlock()
	return changed
}

func sameGitMap(a, b map[string]*Git) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !sameGit(av, bv) {
			return false
		}
	}
	return true
}

func sameGit(a, b *Git) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return *a == *b
}

// runGit keeps the cache warm until ctx is cancelled, waking the runner
// whenever the numbers actually move.
func (r *Runner) runGit(ctx context.Context) {
	measure := func() {
		features, err := r.load(r.opt.Project)
		if err != nil {
			return
		}
		if r.git.refresh(ctx, features) {
			r.wake()
		}
	}

	// Measure once up front so the first snapshot after startup carries real
	// numbers rather than nothing.
	measure()

	t := time.NewTicker(r.opt.Git)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			measure()
		}
	}
}
