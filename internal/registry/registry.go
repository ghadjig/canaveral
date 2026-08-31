// Package registry records where canaveralized projects live, so canaveral can
// address a project by name from outside its directory.
//
// Every other lookup in canaveral starts from the current directory
// (manifest.Find walks upwards) or from a project name that some caller already
// knew. Neither works for a launcher bound to a global hotkey: it has no
// meaningful working directory and no project in hand, only a few characters
// the user has typed. This is the index that turns those characters into a
// root path.
//
// The file is a cache, not configuration. It is written as a side effect of
// using canaveral normally, and Prune and Scan can rebuild it from the
// filesystem at any time, so losing it costs nothing but the most-recently-used
// ordering.
package registry

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

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// FileName is the registry's basename inside the canaveral state directory.
const FileName = "projects.json"

// ErrConflict reports that a different root is already registered under a name.
//
// This is worth refusing rather than silently repointing, because a project's
// name is also its key in the state directory: two checkouts claiming one name
// do not merely confuse the registry, they already share
// <state>/features/<name>/ and therefore each other's features.
var ErrConflict = errors.New("project name already registered to a different root")

// Project is one registered project checkout.
type Project struct {
	Name string `json:"name"`
	// Root is the project's main checkout — the directory holding its
	// canaveral.toml, never a feature worktree.
	Root string `json:"root"`
	// LastUsed orders the launcher. Bumped by Record, which runs whenever a
	// command resolves a manifest, so the ordering tracks real use rather than
	// registration order.
	LastUsed time.Time `json:"last_used,omitempty"`
}

// Alive reports whether the project's root still holds a manifest.
//
// A registry entry outlives the directory it points at: repositories get moved,
// renamed and deleted without telling canaveral. Readers use this to dim or
// skip an entry; Prune uses it to drop one.
func (p Project) Alive() bool {
	if p.Root == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(p.Root, manifest.FileName))
	return err == nil
}

// Path returns the registry file's location.
func Path() (string, error) {
	d, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, FileName), nil
}

// read returns exactly what is on disk, with no derived entries.
func read() ([]Project, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read project registry: %w", err)
	}
	var out []Project
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse project registry: %w", err)
	}
	return out, nil
}

// write atomically replaces the registry file.
func write(projects []Project) error {
	p, err := Path()
	if err != nil {
		return err
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	b, err := json.MarshalIndent(projects, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load returns every known project, sorted by name.
//
// Entries recorded on disk are merged with entries derived from feature state:
// every feature record carries the root of the project it belongs to, so a
// project that predates the registry is still addressable without anyone having
// to run a scan. Derived entries are not written back — the file stays a record
// of what canaveral actually touched, and the derivation costs one directory
// walk to repeat.
func Load() ([]Project, error) {
	projects, err := read()
	if err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(projects))
	for _, p := range projects {
		known[p.Name] = true
	}
	for _, d := range derived() {
		if !known[d.Name] {
			projects = append(projects, d)
			known[d.Name] = true
		}
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Name < projects[j].Name })
	return projects, nil
}

// derived reconstructs projects from feature state records.
//
// state.Projects lists directory names only, which is not enough: a project
// with no features at all leaves an empty directory behind, and a name on its
// own cannot be opened. The root comes from the feature records themselves.
func derived() []Project {
	features, err := state.LoadAll()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []Project
	for _, f := range features {
		if f.Project == "" || f.Root == "" || seen[f.Project] {
			continue
		}
		seen[f.Project] = true
		out = append(out, Project{Name: f.Project, Root: f.Root})
	}
	return out
}

// MRU returns every known project, most recently used first.
//
// Ties and never-used entries fall back to alphabetical order, so the list is
// stable rather than arbitrary before anything has been used.
func MRU() ([]Project, error) {
	projects, err := Load()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(projects, func(i, j int) bool {
		a, b := projects[i], projects[j]
		if a.LastUsed.Equal(b.LastUsed) {
			return a.Name < b.Name
		}
		return a.LastUsed.After(b.LastUsed)
	})
	return projects, nil
}

// Find looks up a project by exact name.
func Find(name string) (Project, bool, error) {
	projects, err := Load()
	if err != nil {
		return Project{}, false, err
	}
	for _, p := range projects {
		if p.Name == name {
			return p, true, nil
		}
	}
	return Project{}, false, nil
}

// Record registers a project and bumps its last-used time.
//
// Callers treat a failure here as advisory: the registry is a convenience, and
// no command should stop working because its index could not be updated.
//
// A name already registered to a *live* different root returns ErrConflict and
// writes nothing. If the registered root has since disappeared the entry is
// repointed instead, which is the common benign case — a repository that moved.
func Record(name, root string) error {
	if name == "" || root == "" {
		return fmt.Errorf("record project: name and root are both required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("record project %s: %w", name, err)
	}
	// A feature worktree is never a project root, and inside one it looks
	// exactly like it is: canaveral provisions a copy of the manifest there, so
	// manifest.Find walking up from "." stops at the worktree and reports it as
	// the root. Recording that would claim the project lives in its own
	// feature, and then conflict with the real checkout on every command run
	// from anywhere else. The real root is recorded whenever a command runs
	// outside a worktree, and is recoverable from feature state regardless.
	if isLinkedWorktree(abs) {
		return nil
	}
	projects, err := read()
	if err != nil {
		return err
	}
	for i, p := range projects {
		if p.Name != name {
			continue
		}
		if p.Root != abs {
			if p.Alive() {
				return fmt.Errorf("%w: %s is %s, not %s", ErrConflict, name, p.Root, abs)
			}
			projects[i].Root = abs
		}
		projects[i].LastUsed = time.Now()
		return write(projects)
	}
	projects = append(projects, Project{Name: name, Root: abs, LastUsed: time.Now()})
	return write(projects)
}

// Add registers a project by the path of its checkout, reading the name from
// its manifest rather than guessing it from the directory name.
func Add(dir string) (Project, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Project{}, err
	}
	if isLinkedWorktree(abs) {
		return Project{}, fmt.Errorf("%s is a feature worktree, not a project checkout", abs)
	}
	m, err := manifest.Load(abs)
	if err != nil {
		return Project{}, err
	}
	if err := Record(m.Name, abs); err != nil {
		return Project{}, err
	}
	return Project{Name: m.Name, Root: abs}, nil
}

// Forget removes a project from the registry, leaving the checkout alone.
//
// Returns false when the name was only ever derived from feature state, since
// there is nothing on disk to remove and the entry will simply come back.
func Forget(name string) (bool, error) {
	projects, err := read()
	if err != nil {
		return false, err
	}
	for i, p := range projects {
		if p.Name == name {
			return true, write(append(projects[:i:i], projects[i+1:]...))
		}
	}
	return false, nil
}

// Prune drops entries whose root no longer holds a manifest, returning them.
func Prune() ([]Project, error) {
	projects, err := read()
	if err != nil {
		return nil, err
	}
	var kept, dropped []Project
	for _, p := range projects {
		if p.Alive() {
			kept = append(kept, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	return dropped, write(kept)
}

// maxScanDepth bounds Scan's descent below its starting directory. Deep enough
// for the usual ~/code/<org>/<repo> arrangements, shallow enough that pointing
// it at a home directory terminates.
const maxScanDepth = 6

// skipDirs are never descended into. Large, never project roots, and in
// node_modules' case capable of containing thousands of directories that exist
// only to slow the walk down.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"target": true, "dist": true, "build": true, ".cache": true,
}

// Scan walks a directory tree for project checkouts and registers each one.
//
// Two rules keep feature worktrees out of the results, which matters because
// canaveral copies the manifest into every worktree it provisions — a naive
// walk of a projects directory would "find" one project per worktree, each
// claiming the same name:
//
//   - a directory holding a manifest is registered and not descended into, so
//     the conventional <project>/worktrees/<feature> layout is never reached;
//   - a directory whose .git is a file rather than a directory is a linked
//     worktree, and is skipped outright. That catches worktrees configured to
//     live somewhere else entirely, which the first rule cannot.
//
// Conflicts are reported rather than raised: one badly-named checkout in a tree
// of fifty should not abort the scan.
func Scan(dir string) (found []Project, conflicts []error, err error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, err
	}
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is not worth failing a whole scan over.
			return nil //nolint:nilerr // deliberate: skip and continue
		}
		if !e.IsDir() {
			return nil
		}
		if p != root {
			base := e.Name()
			if skipDirs[base] || strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			if depth(root, p) > maxScanDepth {
				return filepath.SkipDir
			}
		}
		if isLinkedWorktree(p) {
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(p, manifest.FileName)); statErr != nil {
			return nil
		}
		proj, addErr := Add(p)
		if addErr != nil {
			conflicts = append(conflicts, addErr)
		} else {
			found = append(found, proj)
		}
		// Registered or not, a manifest here means anything below is a
		// worktree or a subdirectory, never another project root.
		return filepath.SkipDir
	})
	return found, conflicts, err
}

// depth counts path segments between root and p.
func depth(root, p string) int {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(rel), "/"))
}

// isLinkedWorktree reports whether dir is a git worktree rather than a normal
// checkout. git writes a plain file named .git holding a gitdir: pointer in a
// linked worktree, where a main checkout has a directory.
func isLinkedWorktree(dir string) bool {
	info, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil && !info.IsDir()
}
