// Package parked finds a project's workspaces that are not running but could
// be woken: stashed features, and dormant worktrees.
//
// A dormant worktree is a git worktree sitting where canaveral would put a
// feature's, with no canaveral record behind it at all — neither active nor
// stashed. They come from a state directory that was wiped or moved to a new
// machine, `rm --keep-worktree`, or a worktree added by hand. Bare dispatch
// wakes one exactly as it pops a stash, so they are listed alongside stashes:
// from where you are standing the two are the same thing, work on disk that
// is one command away from having its windows back.
package parked

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/feature"
	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/registry"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/worktree"
)

// Kind says how a parked workspace came to be parked, which decides what
// waking it restores.
type Kind string

const (
	// Stashed is a feature parked by `canaveral stash`: its record, slot
	// preference and agent sessions were kept.
	Stashed Kind = "stashed"
	// Dormant is a worktree with no record at all. Waking one adopts it on
	// whatever branch it has checked out, with a fresh slot and a fresh
	// agent conversation.
	Dormant Kind = "dormant"
)

// Workspace is one parked workspace.
type Workspace struct {
	Project  string `json:"project"`
	Name     string `json:"name"`
	Key      string `json:"key"`
	Kind     Kind   `json:"kind"`
	Branch   string `json:"branch,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	// Since is when a stash was parked, or when a dormant worktree's git
	// state was last touched — the nearest thing it has to "last worked
	// on", which is what a list of these is sorted by.
	Since time.Time `json:"since"`
}

// Project returns a project's parked workspaces, most recently parked first.
//
// A dormant worktree that cannot be listed — git missing, the checkout
// moved — costs only the dormant half of the answer, never the stashes.
func Project(ctx context.Context, m *manifest.Manifest) ([]Workspace, error) {
	out, err := stashes(m.Name)
	if err != nil {
		return nil, err
	}
	d, err := Dormants(ctx, m)
	out = append(out, d...)
	sortNewest(out)
	return out, err
}

// All returns every parked workspace across every project canaveral knows
// about, most recently parked first.
//
// Stashes come from the state directory and so cover every project that has
// one, registered or not. Dormant worktrees need the project's checkout to
// look in, so only registered projects that still exist are searched.
func All(ctx context.Context) ([]Workspace, error) {
	ss, err := state.LoadAllStashes()
	if err != nil {
		return nil, err
	}
	out := fromStashes(ss)
	projects, _ := registry.Load()
	for _, p := range projects {
		if !p.Alive() {
			continue
		}
		m, err := manifest.Load(p.Root)
		if err != nil {
			continue
		}
		d, _ := Dormants(ctx, m)
		out = append(out, d...)
	}
	sortNewest(out)
	return out, nil
}

// Dormants returns m's dormant worktrees, sorted by name.
func Dormants(ctx context.Context, m *manifest.Manifest) ([]Workspace, error) {
	if m.Space {
		return nil, nil
	}
	main, dir, err := locate(ctx, m)
	if err != nil {
		return nil, err
	}
	trees, err := worktree.List(ctx, main)
	if err != nil {
		return nil, err
	}
	var out []Workspace
	for _, t := range trees {
		name, ok := nameUnder(dir, t.Path)
		// Detached and prunable trees are left out rather than offered: a
		// detached HEAD has no branch to adopt and a prunable one has no
		// directory to open, so either would only ever be offered to fail.
		if !ok || t.Branch == "" || t.Prunable || recorded(m.Name, name) {
			continue
		}
		out = append(out, Workspace{
			Project:  m.Name,
			Name:     name,
			Key:      m.Name + "/" + name,
			Kind:     Dormant,
			Branch:   t.Branch,
			Worktree: t.Path,
			Since:    lastTouched(t.Path),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// FindDormant returns the dormant worktree a feature name would wake, if
// there is one.
//
// Cheaper than Dormants and deliberately so: it is asked on every bare
// dispatch of a name with no record, which is mostly typos, and a typo should
// not cost a `git worktree list`. The path a name maps to is fixed, so one
// stat answers "is there anything there" before git is asked anything.
func FindDormant(ctx context.Context, m *manifest.Manifest, name string) (Workspace, bool) {
	if m.Space || name == "" || recorded(m.Name, name) {
		return Workspace{}, false
	}
	_, dir, err := locate(ctx, m)
	if err != nil {
		return Workspace{}, false
	}
	path := filepath.Join(dir, filepath.FromSlash(name))
	if _, err := os.Stat(filepath.Join(path, ".git")); err != nil {
		return Workspace{}, false
	}
	// A directory that merely contains a .git is not necessarily a
	// worktree of this repository; only git can say that it is.
	trees, err := worktree.List(ctx, path)
	if err != nil {
		return Workspace{}, false
	}
	for _, t := range trees {
		if filepath.Clean(t.Path) != filepath.Clean(path) || t.Branch == "" || t.Prunable {
			continue
		}
		return Workspace{
			Project:  m.Name,
			Name:     name,
			Key:      m.Name + "/" + name,
			Kind:     Dormant,
			Branch:   t.Branch,
			Worktree: t.Path,
			Since:    lastTouched(t.Path),
		}, true
	}
	return Workspace{}, false
}

// locate returns the project's main checkout and the directory its feature
// worktrees live under.
//
// Both are resolved from the main checkout rather than m.Root, because run
// from inside a feature worktree m.Root is that worktree — it carries its
// own copy of the manifest — and a relative [worktree] root would then point
// at a worktrees/ directory nested inside it.
func locate(ctx context.Context, m *manifest.Manifest) (main, dir string, err error) {
	main, err = worktree.MainCheckout(ctx, m.Root)
	if err != nil {
		return "", "", err
	}
	mm := *m
	mm.Root = main
	root, err := mm.WorktreeRoot()
	if err != nil {
		return "", "", err
	}
	if root != "" {
		return main, root, nil
	}
	// The "state" opt-out: <state>/worktrees/<project>/<feature>.
	dir, err = state.WorktreePath(m.Name, "")
	return main, dir, err
}

// nameUnder maps a worktree path to the feature name that would address it,
// reporting false for anything a feature name could not reach — outside the
// worktree directory, or a directory name that does not survive slugging.
func nameUnder(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	name := filepath.ToSlash(rel)
	if feature.Slug(name) != name {
		return "", false
	}
	return name, true
}

// recorded reports whether canaveral has any record of the name, active or
// stashed. An unreadable record counts as one: a corrupt state file must not
// make its worktree look abandoned, or waking it would allocate a second
// slot over a feature that may still have units running.
func recorded(project, name string) bool {
	if _, err := state.Load(project, name); !errors.Is(err, state.ErrNotFound) {
		return true
	}
	_, err := state.LoadStash(project, name)
	return !errors.Is(err, state.ErrNotFound)
}

// lastTouched approximates when a worktree was last worked in.
//
// The index is rewritten by nearly every git operation, including a plain
// `git status`, which makes it a better answer than the directory's own mtime
// — that only moves when a top-level entry is added or removed.
func lastTouched(path string) time.Time {
	if gitdir := linkedGitDir(path); gitdir != "" {
		if st, err := os.Stat(filepath.Join(gitdir, "index")); err == nil {
			return st.ModTime()
		}
	}
	if st, err := os.Stat(path); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}

// linkedGitDir reads the "gitdir:" pointer a linked worktree's .git file
// holds, or "" when it is not one.
func linkedGitDir(path string) string {
	b, err := os.ReadFile(filepath.Join(path, ".git"))
	if err != nil {
		return ""
	}
	dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	return dir
}

func stashes(project string) ([]Workspace, error) {
	ss, err := state.LoadStashes(project)
	if err != nil {
		return nil, err
	}
	return fromStashes(ss), nil
}

func fromStashes(ss []*state.Stash) []Workspace {
	out := make([]Workspace, 0, len(ss))
	for _, s := range ss {
		f := s.Feature
		out = append(out, Workspace{
			Project:  f.Project,
			Name:     f.Name,
			Key:      f.Key(),
			Kind:     Stashed,
			Branch:   f.Branch,
			Worktree: f.Worktree,
			Since:    s.StashedAt,
		})
	}
	return out
}

func sortNewest(ws []Workspace) {
	sort.SliceStable(ws, func(i, j int) bool { return ws[i].Since.After(ws[j].Since) })
}

// ForProject is Project by name, or All for an empty name — the shape a
// caller holding only a project name (the watch runner) needs.
//
// An unregistered or vanished checkout still yields its stashes, which come
// from the state directory; only the dormant half needs a checkout to look in.
func ForProject(ctx context.Context, project string) ([]Workspace, error) {
	if project == "" {
		return All(ctx)
	}
	p, found, err := registry.Find(project)
	if err == nil && found && p.Alive() {
		if m, err := manifest.Load(p.Root); err == nil {
			return Project(ctx, m)
		}
	}
	return stashes(project)
}
