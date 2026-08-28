// Package state persists runtime information about active features.
//
// State is keyed by project and feature, because a single project may have
// several features running at once, each with its own worktree and ports.
package state

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

// ErrNotFound indicates no state exists for the requested feature.
var ErrNotFound = errors.New("feature not found")

// Feature is the persisted record of one active feature workspace.
type Feature struct {
	Project string `json:"project"`
	Name    string `json:"name"`
	// Root is the project's main checkout.
	Root string `json:"root"`
	// Slot is the stable index used to derive ports. It is allocated once and
	// never changes, so a feature keeps the same ports for its whole life.
	Slot      int            `json:"slot"`
	Branch    string         `json:"branch"`
	Worktree  string         `json:"worktree"`
	DBSuffix  string         `json:"db_suffix,omitempty"`
	Ports     map[string]int `json:"ports,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
	// Provisioned lists paths canaveral copied in; they are not user work and
	// must not make the worktree look dirty at teardown.
	Provisioned []string  `json:"provisioned,omitempty"`
	Services    []Service `json:"services,omitempty"`
	Agents      []Agent   `json:"agents,omitempty"`
	Windows     []Window  `json:"windows,omitempty"`
}

// Service is a launched feature service.
type Service struct {
	Name     string `json:"name"`
	Unit     string `json:"unit"`
	Dir      string `json:"dir"`
	Cmd      string `json:"cmd"`
	LogPath  string `json:"log_path"`
	Optional bool   `json:"optional,omitempty"`
}

// Agent is a launched agent server.
type Agent struct {
	Name    string `json:"name"`
	Tool    string `json:"tool"`
	Unit    string `json:"unit"`
	Dir     string `json:"dir"`
	LogPath string `json:"log_path"`
	URL     string `json:"url,omitempty"`
	Port    int    `json:"port,omitempty"`
}

// Window is a declared GUI window belonging to the feature.
type Window struct {
	Name string `json:"name"`
	// Class is the window class canaveral assigns, used to detect whether the
	// window is already open.
	Class string `json:"class"`
	Cmd   string `json:"cmd"`
	Dir   string `json:"dir"`
	// MatchClass identifies GUI windows that do not carry our class. Such a
	// window is matched by class within the feature's own workspace.
	MatchClass string `json:"match_class,omitempty"`
	// Workspace is the Hyprland workspace the window belongs to.
	Workspace string `json:"workspace,omitempty"`
}

// HyprWorkspace is the Hyprland workspace name for the feature.
func (f *Feature) HyprWorkspace() string { return f.Project + ":" + f.Name }

// Key uniquely identifies the feature across projects.
func (f *Feature) Key() string { return f.Project + "/" + f.Name }

// Dir returns the canaveral state directory, creating it if needed.
func Dir() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "canaveral")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func featuresDir(project string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d, "features", project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// LogDir returns the directory holding logs for a feature.
func LogDir(project, feature string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(d, "logs", project, feature)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// WorktreePath returns where a feature's worktree lives.
func WorktreePath(project, feature string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "worktrees", project, feature), nil
}

// WindowProfile returns a private state directory for a window.
//
// A browser needs its own profile directory to start a separate process;
// otherwise it hands the request to the running instance, which ignores the
// class and workspace canaveral asked for.
func WindowProfile(project, feature, window string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "profiles", project, feature, window), nil
}

// path returns the on-disk path for a feature's state file.
//
// A namespaced feature name (containing "/", e.g. "onboarding/step1") maps
// directly onto nested directories here — filepath.Join treats it as
// additional path segments. Because every leaf file carries a ".json"
// suffix that a directory name never does, "onboarding.json" (a flat
// feature literally named "onboarding") and "onboarding/" (the directory
// holding "onboarding/step1"'s state) never collide on disk, even though the
// equivalent git branch names would (git's own ref storage has no such
// suffix to disambiguate them, so that collision surfaces as a git error at
// branch-creation time instead).
func path(project, feature string) (string, error) {
	d, err := featuresDir(project)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, feature+".json"), nil
}

// Save atomically writes the feature state to disk.
func Save(f *Feature) error {
	p, err := path(f.Project, f.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads a feature's state, returning ErrNotFound when absent.
func Load(project, feature string) (*Feature, error) {
	p, err := path(project, feature)
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
	var f Feature
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse state for %s/%s: %w", project, feature, err)
	}
	return &f, nil
}

// Remove deletes the persisted state for a feature.
func Remove(project, feature string) error {
	p, err := path(project, feature)
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
	// A namespaced feature's directory (e.g. "onboarding/" for
	// "onboarding/step1") is otherwise never cleaned up: nothing else
	// removes it once its last feature is gone.
	pruneEmptyDirs(filepath.Dir(p), filepath.Dir(filepath.Dir(p)))
	return nil
}

// pruneEmptyDirs removes dir, and then each empty parent up to but not
// including stop, so a namespace directory left with nothing in it doesn't
// linger forever.
func pruneEmptyDirs(dir, stop string) {
	for dir != stop && dir != "." && dir != string(filepath.Separator) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if os.Remove(dir) != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// List returns the feature names known for a project, sorted.
//
// Namespaced features (containing "/") are stored as nested directories, so
// this walks recursively rather than reading one flat directory.
func List(project string) ([]string, error) {
	d, err := featuresDir(project)
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
		rel = filepath.ToSlash(strings.TrimSuffix(rel, ".json"))
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// LoadProject returns every feature of a project, ordered by slot.
func LoadProject(project string) ([]*Feature, error) {
	names, err := List(project)
	if err != nil {
		return nil, err
	}
	var out []*Feature
	for _, n := range names {
		f, err := Load(project, n)
		if err != nil {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slot < out[j].Slot })
	return out, nil
}

// Projects returns every project with at least one recorded feature.
func Projects() ([]string, error) {
	d, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(d, "features"))
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

// LoadAll returns every feature across every project.
func LoadAll() ([]*Feature, error) {
	projects, err := Projects()
	if err != nil {
		return nil, err
	}
	var out []*Feature
	for _, p := range projects {
		fs, err := LoadProject(p)
		if err != nil {
			continue
		}
		out = append(out, fs...)
	}
	return out, nil
}

// AllocateSlot returns the lowest slot not in use by another feature of the
// project.
//
// Slots are dense and stable: a feature keeps its slot (and therefore its
// ports) until removed, and a removed feature's slot is reused by the next one.
func AllocateSlot(project, feature string) (int, error) {
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
	for slot := 0; ; slot++ {
		if !used[slot] {
			return slot, nil
		}
	}
}

// Agent looks up an agent record by name.
func (f *Feature) Agent(name string) (*Agent, bool) {
	for i := range f.Agents {
		if f.Agents[i].Name == name {
			return &f.Agents[i], true
		}
	}
	return nil, false
}
