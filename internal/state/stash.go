package state

// Stashed features: a parked workspace, kept whole, addressable by name.
//
// A stash lives in a separate tree from an active feature rather than as a
// flag on the record, and that separation is load-bearing. Every enumeration
// in canaveral — List, LoadProject, LoadAll, and therefore `ls`, `status`,
// `watch`, slot allocation and widget numbering — reads the features tree, so
// moving the record out of it is what makes a stashed feature stop consuming
// a port slot, a widget number and a row in every listing, without a single
// one of those callers having to learn the word "stashed". A boolean on the
// record would have needed each of them to remember to check it, and the one
// that forgot would have been the bug.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stash is a feature workspace parked whole: its worktree left on disk, its
// branch untouched, its agent conversations remembered, and everything that
// costs something to keep running — units, windows, ports — released.
//
// The embedded Feature is the record exactly as it was when stashed, so a
// pop restores the same branch, the same worktree and the same declared
// windows rather than deriving fresh ones. Slot is the one field deliberately
// not honoured verbatim: it was released on stash and may have been taken
// since, so Pop re-allocates and merely prefers the old number.
type Stash struct {
	Feature   *Feature  `json:"feature"`
	StashedAt time.Time `json:"stashed_at"`
	// Sessions maps agent name to the opencode session that agent was last
	// working in, captured while it was still reachable.
	//
	// The worktree stays on disk across a stash, so opencode's own storage
	// still holds these sessions and still scopes them to the same directory
	// — this is the pointer to which of them to reopen, not a copy of the
	// conversation. Empty for an agent that was unreachable or had no
	// session yet, which is not an error: a fresh conversation is a fine
	// outcome, and refusing to stash over it would be absurd.
	Sessions map[string]string `json:"sessions,omitempty"`
}

// stashDir returns the directory holding a project's stashed features.
func stashDir(project string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d, "stashed", project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// stashPath returns the on-disk path for a stashed feature, laid out exactly
// like the active one (see path) so a namespaced name nests the same way and
// leaf files stay distinguishable from namespace directories by their
// ".json" suffix.
func stashPath(project, feature string) (string, error) {
	d, err := stashDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, feature+".json"), nil
}

// SaveStash atomically writes a stash record.
func SaveStash(s *Stash) error {
	if s.Feature == nil {
		return fmt.Errorf("stash has no feature record")
	}
	p, err := stashPath(s.Feature.Project, s.Feature.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadStash reads a stashed feature, returning ErrNotFound when absent.
func LoadStash(project, feature string) (*Stash, error) {
	p, err := stashPath(project, feature)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s/%s: %w", project, feature, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	var s Stash
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("parse stash for %s/%s: %w", project, feature, err)
	}
	if s.Feature == nil {
		return nil, fmt.Errorf("stash for %s/%s has no feature record", project, feature)
	}
	// The name and project are what the file was found by; trusting the
	// record over the path would let a hand-edited or moved file restore
	// itself somewhere else entirely.
	s.Feature.Project, s.Feature.Name = project, feature
	return &s, nil
}

// RemoveStash deletes a stash record.
func RemoveStash(project, feature string) error {
	p, err := stashPath(project, feature)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	pruneEmptyDirs(filepath.Dir(p), filepath.Dir(filepath.Dir(p)))
	return nil
}

// ListStashes returns the stashed feature names of a project, sorted.
func ListStashes(project string) ([]string, error) {
	d, err := stashDir(project)
	if err != nil {
		return nil, err
	}
	var names []string
	err = filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			return nil
		}
		rel, err := filepath.Rel(d, p)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(strings.TrimSuffix(rel, ".json")))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// LoadStashes returns every stash of a project, newest first.
//
// Ordered by when it was stashed rather than by slot, because a stash has no
// live slot to order by and "most recently parked" is the one `canaveral
// pop` with no argument means.
func LoadStashes(project string) ([]*Stash, error) {
	names, err := ListStashes(project)
	if err != nil {
		return nil, err
	}
	var out []*Stash
	for _, n := range names {
		s, err := LoadStash(project, n)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StashedAt.After(out[j].StashedAt) })
	return out, nil
}

// StashProjects returns every project with at least one stashed feature.
func StashProjects() ([]string, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(d, "stashed"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// LoadAllStashes returns every stash across every project, newest first.
func LoadAllStashes() ([]*Stash, error) {
	projects, err := StashProjects()
	if err != nil {
		return nil, err
	}
	var out []*Stash
	for _, p := range projects {
		ss, err := LoadStashes(p)
		if err != nil {
			continue
		}
		out = append(out, ss...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StashedAt.After(out[j].StashedAt) })
	return out, nil
}

// AllocateSlotPreferring is AllocateSlot with a first choice.
//
// A stashed feature releases its slot, and therefore its ports, so that
// stashing actually frees something; but a popped feature coming back on the
// ports it left with is worth having — a bookmarked localhost:3002, a
// terminal still sitting on the old log — so its old number is reclaimed
// whenever nothing has taken it in the meantime. When something has, the
// lowest free slot is used and the ports simply move, which is no worse than
// the feature having been created fresh today.
//
// prefer < 0 means no preference, which is exactly AllocateSlot.
func AllocateSlotPreferring(project, feature string, prefer int) (int, error) {
	existing, err := LoadProject(project)
	if err != nil {
		return 0, err
	}
	used := map[int]bool{}
	for _, f := range existing {
		if f.Name == feature {
			return f.Slot, nil
		}
		used[f.Slot] = true
	}
	if prefer >= 0 && !used[prefer] {
		return prefer, nil
	}
	for slot := 0; ; slot++ {
		if !used[slot] {
			return slot, nil
		}
	}
}
