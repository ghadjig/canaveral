// Package space manages workspaces that have no project behind them.
//
// canaveral's usual unit of work is a feature of a project: a git worktree, a
// branch, ports, and the windows you declared. Plenty of the things you sit
// down to do are not that. 3D printing is a slicer and a browser on
// onshape.com; there is no repository, nothing to check out, and no branch
// the work could possibly land on — but the *workspace* is exactly as real,
// and setting it up by hand every time is exactly as tedious.
//
// A space is that workspace with the git half removed. Its definition lives
// in canaveral's own config directory rather than in a checkout, because
// there is no checkout to put it in:
//
//	~/.config/canaveral/spaces/3d-printing.toml
//
// The file is a manifest with the same [[window]], [[service]], [[agent]],
// [ports], [env] and [layout] sections a project uses, minus the four that
// only mean something with a repository — see manifest.validateKind. Sharing
// the type is the point: everything downstream of feature.Reconcile is about
// windows, units and Hyprland, none of which ever cared whether git was
// involved.
package space

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bandito/canaveral/internal/config"
	"github.com/bandito/canaveral/internal/manifest"
)

// ErrNotFound indicates no space is defined by that name.
var ErrNotFound = errors.New("space not found")

// ext is the definition file's extension. Named after the space, so the file
// name is the identity and there is no name key to disagree with it.
const ext = ".toml"

// Dir returns the directory holding space definitions.
//
// Under config rather than state because a space *is* configuration: you
// write it, you keep it, and canaveral only reads it. What canaveral itself
// records about a running space — its slot, units and windows — lives in the
// state directory with every other feature's.
//
// Not created, for the same reason a space's `dir` is not: only Create has
// been asked to make anything. Listing, completing or opening a space must
// leave no trace in a config directory, and shell completion runs this on
// every other keystroke.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "spaces"), nil
}

// Path returns a space's definition file, whether or not it exists.
func Path(name string) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+ext), nil
}

// validName refuses anything that would not round-trip through a file name.
//
// Spaces are deliberately flat, unlike feature names: a "/" here would put
// the definition in a subdirectory and make the name ambiguous with the
// namespace syntax, which a space cannot participate in anyway — there is no
// sibling worktree to share a skill with.
func validName(name string) error {
	if name == "" {
		return errors.New("space name is required")
	}
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid space name %q: use letters, digits, dots, dashes and underscores", name)
	}
	return nil
}

var nameRe = manifest.NameRe

// Exists reports whether a space is defined by that name.
func Exists(name string) bool {
	p, err := Path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(p)
	return err == nil
}

// Load reads and validates a space's definition.
func Load(name string) (*manifest.Manifest, error) {
	p, err := Path(name)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(p); err != nil {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return manifest.LoadSpace(p, name)
}

// List returns the names of every defined space, sorted.
//
// Only the names: a definition that no longer parses should not stop the
// others being listed, and the listing is exactly where you would go to find
// out that one exists at all.
func List() ([]string, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ext) {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ext))
	}
	sort.Strings(out)
	return out, nil
}

// Create writes a starter definition and returns its path. It refuses to
// overwrite an existing one: a space is something you have edited, and the
// only copy of it is this file.
func Create(name string) (string, error) {
	p, err := Path(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("space %q already exists at %s", name, p)
		}
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(starter(name)); err != nil {
		return "", err
	}
	return p, nil
}

// Remove deletes a space's definition.
func Remove(name string) error {
	p, err := Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return err
	}
	return nil
}

// starter is the file Create writes: enough to open something the first time,
// with the rest shown as comments rather than guessed at.
func starter(name string) string {
	return fmt.Sprintf(`# canaveral space: %s
#
# A workspace with no project behind it. Open it from anywhere with
#
#     canaveral %s
#
# Windows are opened on a Hyprland workspace of the same name. "run" executes
# inside a terminal; "exec" launches a GUI application, which must be told to
# adopt {{.Class}} so canaveral can recognise the window later.

# Where windows, services and agents open. Defaults to your home directory.
# dir = "~/models"

[[window]]
name = "browser"
exec = "google-chrome --class={{.Class}} --new-window https://example.com"

[[window]]
name = "terminal"
run  = ""

# Ports, services and agents all work here exactly as they do in a project:
#
# [ports]
# web = 8080
#
# [[agent]]
# name = "main"
#
# [layout]
# order = ["browser", "terminal"]
# [layout.default]
# browser  = 0.6
# terminal = 0.4
`, name, name)
}
