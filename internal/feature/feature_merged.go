package feature

// Remembering which branches merged on GitHub, so asking stays cheap enough
// for `status` to do it every time.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/worktree"
)

// unmergedTTL is how long a "no merged pull request" answer is trusted. Short,
// because that answer goes stale the moment a merge queue lands the branch,
// which is precisely the change worth noticing; long enough that `status
// --watch` is not a gh call per feature per redraw.
const unmergedTTL = 2 * time.Minute

// mergedCacheMaxAge bounds the file: entries for branches long gone would
// otherwise accumulate forever.
const mergedCacheMaxAge = 30 * 24 * time.Hour

type mergedEntry struct {
	Tip     string    `json:"tip"`
	PR      int       `json:"pr,omitempty"`
	Checked time.Time `json:"checked"`
}

// mergedMu serialises this process's read-modify-write of the cache file;
// status asks for every feature concurrently. Another process racing it can
// at worst drop an entry, which only costs a repeated lookup.
var mergedMu sync.Mutex

// MergedPR is worktree.MergedPR with its answers remembered on disk.
//
// A merged answer is kept for as long as the branch still points at the
// same commit: a pull request does not unmerge, and a new commit is new work
// that has to be asked about afresh. Anything else is kept for unmergedTTL.
func MergedPR(ctx context.Context, repo, branch string) (int, bool) {
	tip, err := worktree.BranchTip(ctx, repo, branch)
	if err != nil {
		return 0, false
	}
	key := repo + "\x00" + branch
	now := time.Now()

	mergedMu.Lock()
	cache := loadMergedCache()
	mergedMu.Unlock()
	if e, ok := cache[key]; ok && e.Tip == tip && (e.PR > 0 || now.Sub(e.Checked) < unmergedTTL) {
		return e.PR, e.PR > 0
	}

	// Asked outside the lock: this is the slow part, and features are
	// asked about concurrently.
	n, ok, err := worktree.MergedPRAt(ctx, repo, branch, tip)
	if err != nil {
		// No gh, no network, a timeout: no answer, so nothing to remember
		// as "not merged".
		return 0, false
	}

	mergedMu.Lock()
	defer mergedMu.Unlock()
	cache = loadMergedCache()
	cache[key] = mergedEntry{Tip: tip, PR: n, Checked: now}
	for k, e := range cache {
		if now.Sub(e.Checked) > mergedCacheMaxAge {
			delete(cache, k)
		}
	}
	saveMergedCache(cache)
	return n, ok
}

func mergedCachePath() (string, error) {
	dir, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "merged-prs.json"), nil
}

// loadMergedCache returns the cache, empty when there is none or it cannot
// be read: it is only ever an optimisation.
func loadMergedCache() map[string]mergedEntry {
	out := map[string]mergedEntry{}
	p, err := mergedCachePath()
	if err != nil {
		return out
	}
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func saveMergedCache(c map[string]mergedEntry) {
	p, err := mergedCachePath()
	if err != nil {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	// Renamed into place so a concurrent reader never sees half a file.
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
