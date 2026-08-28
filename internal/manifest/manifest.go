// Package manifest parses and validates canaveral.toml project manifests.
//
// A manifest describes a project. Each feature worked on inside that project
// becomes an independent workspace: its own git worktree and branch, its own
// allocated ports, optionally its own databases, and its own set of windows.
package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/bandito/canaveral/internal/toolchain"
)

// FileName is the canonical manifest filename looked up in a project root.
const FileName = "canaveral.toml"

// DBIsolation controls whether features share a database.
type DBIsolation string

const (
	// DBShared points every feature at the project's normal database.
	DBShared DBIsolation = "shared"
	// DBSuffix gives each feature its own databases by exporting a suffix
	// variable the app's database config interpolates into the database name.
	DBSuffix DBIsolation = "suffix"
)

// Manifest is the parsed contents of a canaveral.toml file.
type Manifest struct {
	Name string `toml:"name"`
	// Branch is a template evaluated with Vars to name a feature's branch.
	Branch string `toml:"branch"`
	// Toolchain selects per-directory version-manager resolution.
	Toolchain string `toml:"toolchain"`
	// Terminal is the emulator used for `run` windows. Defaults to $TERMINAL,
	// then alacritty.
	Terminal string            `toml:"terminal"`
	Env      map[string]string `toml:"env"`
	// Ports maps a logical name to the base port for feature slot 0.
	Ports    map[string]int `toml:"ports"`
	Database Database       `toml:"database"`
	Worktree Worktree       `toml:"worktree"`
	Services []Service      `toml:"service"`
	Agents   []Agent        `toml:"agent"`
	Windows  []Window       `toml:"window"`
	Layout   Layout         `toml:"layout"`

	// Root is the absolute directory containing the manifest. Not read from TOML.
	Root string `toml:"-"`
}

// Layout forces a fixed column arrangement for windows instead of leaving
// them to Hyprland's normal tiling.
//
// Each named window becomes its own full-height column, left to right in
// Order, sized as a fraction of the monitor's usable width. Current is
// updated automatically by `canaveral hyprwatch` when you leave a feature's
// workspace after resizing its windows, so the arrangement you actually
// ended up with is what the next feature starts from — Default is only the
// starting point for the very first feature of a project.
type Layout struct {
	// Order lists window names left to right. Every entry must be a declared
	// window name, and every declared window must appear exactly once.
	Order []string `toml:"order"`
	// Default maps window name to fraction of monitor width. Must sum to 1.0
	// (within floating point tolerance) and cover exactly the names in Order.
	Default map[string]float64 `toml:"default"`
	// Current holds the last actually-observed arrangement, in the same
	// shape as Default. Left empty until canaveral first snapshots it; you
	// would not normally hand-write this section.
	Current map[string]float64 `toml:"current"`
}

// Enabled reports whether a layout is configured at all. An empty Layout
// (the zero value, when [layout] is absent from the manifest) leaves windows
// to Hyprland's normal tiling untouched.
func (l Layout) Enabled() bool { return len(l.Order) > 0 }

// Fractions returns Current if it is fully populated for every window in
// Order, otherwise Default.
func (l Layout) Fractions() map[string]float64 {
	if len(l.Current) == len(l.Order) {
		complete := true
		for _, name := range l.Order {
			if _, ok := l.Current[name]; !ok {
				complete = false
				break
			}
		}
		if complete {
			return l.Current
		}
	}
	return l.Default
}

// Database configures how features share the project's database server.
type Database struct {
	// Isolation defaults to shared, which requires no application changes.
	Isolation DBIsolation `toml:"isolation"`
	// SuffixEnv is the variable set to the per-feature suffix under
	// isolation = "suffix". The application's database config must interpolate it.
	SuffixEnv string `toml:"suffix_env"`
	// Setup runs once in a new worktree to create or migrate the databases.
	Setup string `toml:"setup"`
	// SetupTimeout bounds the setup command.
	SetupTimeout Duration `toml:"setup_timeout"`
}

// Worktree configures how freshly created worktrees are provisioned.
//
// A new git worktree contains only tracked files, so gitignored-but-required
// artifacts (.env, config/master.key, node_modules) must be brought across or
// the feature's checkout will not build.
type Worktree struct {
	// Link creates a symlink in the worktree pointing at the main checkout.
	// Best for large or shared artifacts such as node_modules.
	Link []string `toml:"link"`
	// Copy duplicates a path so the feature can modify it independently.
	Copy []string `toml:"copy"`
	// Setup runs inside the worktree once, after linking and copying.
	Setup string `toml:"setup"`
	// SetupTimeout bounds the setup command.
	SetupTimeout Duration `toml:"setup_timeout"`
}

// Service is a long-running process backing a feature, such as a web server.
type Service struct {
	Name  string            `toml:"name"`
	Cmd   string            `toml:"cmd"`
	Dir   string            `toml:"dir"`
	Env   map[string]string `toml:"env"`
	Ready Ready             `toml:"ready"`
	// Optional services may fail without aborting the feature.
	Optional bool `toml:"optional"`
}

// Ready describes a readiness probe. At most one check kind may be set.
type Ready struct {
	HTTP    string   `toml:"http"`
	TCP     string   `toml:"tcp"`
	Log     string   `toml:"log"`
	Cmd     string   `toml:"cmd"`
	Timeout Duration `toml:"timeout"`
	Status  int      `toml:"status"`
}

// Kind reports which probe type is configured, or "" when the probe is empty.
func (r Ready) Kind() string {
	switch {
	case r.HTTP != "":
		return "http"
	case r.TCP != "":
		return "tcp"
	case r.Log != "":
		return "log"
	case r.Cmd != "":
		return "cmd"
	}
	return ""
}

// Agent is a coding agent server started for a feature.
type Agent struct {
	Name  string            `toml:"name"`
	Tool  string            `toml:"tool"`
	Dir   string            `toml:"dir"`
	Env   map[string]string `toml:"env"`
	Model string            `toml:"model"`
	Agent string            `toml:"agent"`
}

// Window is a GUI window belonging to a feature's Hyprland workspace.
//
// Exactly one of Run or Exec is used: Run wraps a command in a terminal rooted
// at the worktree, Exec launches a GUI application directly.
type Window struct {
	Name string `toml:"name"`
	// Run is a command executed inside a terminal. An empty string with
	// run set to "" and exec unset yields a plain shell.
	Run *string `toml:"run"`
	// Exec launches a GUI application without a terminal.
	Exec string `toml:"exec"`
	// Dir overrides the working directory, which defaults to the worktree.
	Dir string `toml:"dir"`
	// Hold keeps the terminal open after Run exits, for inspecting output.
	Hold bool `toml:"hold"`
	// ProfileSource, if set, seeds {{.Profile}} once from an existing
	// directory (for example your real browser profile), copying only the
	// relative paths listed in ProfileSeed. Existing destination files are
	// never overwritten, so this only ever fills in what is missing, and
	// nothing outside ProfileSeed (passwords, cookies, history) is touched.
	ProfileSource string   `toml:"profile_source"`
	ProfileSeed   []string `toml:"profile_seed"`
}

// IsTerminal reports whether the window should be wrapped in a terminal.
func (w Window) IsTerminal() bool { return w.Run != nil }

// Command returns the command the window executes.
func (w Window) Command() string {
	if w.Run != nil {
		return *w.Run
	}
	return w.Exec
}

// Duration is a TOML-friendly wrapper around time.Duration accepting "90s" values.
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler for duration strings.
func (d *Duration) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if s == "" {
		d.Duration = 0
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

// Or returns the duration, falling back to def when unset.
func (d Duration) Or(def time.Duration) time.Duration {
	if d.Duration <= 0 {
		return def
	}
	return d.Duration
}

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Find locates the project root holding a canaveral.toml.
//
// The search walks up from start, then falls back to $CANAVERAL_ROOT. The
// fallback matters because commands run inside a feature worktree: a worktree
// contains tracked files only, so an untracked canaveral.toml is absent there
// and the upward walk would fail. Every process canaveral spawns has
// CANAVERAL_ROOT pointing at the real project.
func Find(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	from := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if root := os.Getenv("CANAVERAL_ROOT"); root != "" {
		if _, err := os.Stat(filepath.Join(root, FileName)); err == nil {
			return root, nil
		}
	}
	return "", fmt.Errorf("no %s found in %s or any parent directory", FileName, from)
}

// Load reads and validates the manifest in the given project root directory.
func Load(root string) (*Manifest, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(abs, FileName)
	var m Manifest
	md, err := toml.DecodeFile(path, &m)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	m.Root = abs
	if err := m.normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

func (m *Manifest) normalize() error {
	if m.Name == "" {
		m.Name = filepath.Base(m.Root)
	}
	if !nameRe.MatchString(m.Name) {
		return fmt.Errorf("invalid project name %q", m.Name)
	}
	if m.Branch == "" {
		m.Branch = "{{.Feature}}"
	}
	if _, err := toolchain.ParseMode(m.Toolchain); err != nil {
		return err
	}
	if m.Terminal == "" {
		if t := os.Getenv("TERMINAL"); t != "" {
			m.Terminal = t
		} else {
			m.Terminal = "alacritty"
		}
	}

	switch m.Database.Isolation {
	case "":
		m.Database.Isolation = DBShared
	case DBShared, DBSuffix:
	default:
		return fmt.Errorf("invalid database.isolation %q (want %q or %q)",
			m.Database.Isolation, DBShared, DBSuffix)
	}
	if m.Database.Isolation == DBSuffix && m.Database.SuffixEnv == "" {
		m.Database.SuffixEnv = "DB_SUFFIX"
	}

	for name, base := range m.Ports {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("invalid port name %q", name)
		}
		if base < 1 || base > 65535 {
			return fmt.Errorf("port %q: base %d out of range", name, base)
		}
	}

	seenSvc := map[string]bool{}
	for i := range m.Services {
		s := &m.Services[i]
		if s.Name == "" {
			return fmt.Errorf("service #%d: name is required", i+1)
		}
		if !nameRe.MatchString(s.Name) {
			return fmt.Errorf("service %q: invalid name", s.Name)
		}
		if seenSvc[s.Name] {
			return fmt.Errorf("duplicate service name %q", s.Name)
		}
		seenSvc[s.Name] = true
		if strings.TrimSpace(s.Cmd) == "" {
			return fmt.Errorf("service %q: cmd is required", s.Name)
		}
		if s.Dir == "" {
			s.Dir = "."
		}
		if s.Ready.Status == 0 {
			s.Ready.Status = 200
		}
	}

	seenAgent := map[string]bool{}
	for i := range m.Agents {
		a := &m.Agents[i]
		if a.Name == "" {
			return fmt.Errorf("agent #%d: name is required", i+1)
		}
		if !nameRe.MatchString(a.Name) {
			return fmt.Errorf("agent %q: invalid name", a.Name)
		}
		if seenAgent[a.Name] {
			return fmt.Errorf("duplicate agent name %q", a.Name)
		}
		seenAgent[a.Name] = true
		if a.Tool == "" {
			a.Tool = "opencode"
		}
		if a.Tool != "opencode" {
			return fmt.Errorf("agent %q: unsupported tool %q (only \"opencode\" is supported)", a.Name, a.Tool)
		}
		if a.Dir == "" {
			a.Dir = "."
		}
	}

	seenWin := map[string]bool{}
	for i := range m.Windows {
		w := &m.Windows[i]
		if w.Name == "" {
			return fmt.Errorf("window #%d: name is required", i+1)
		}
		if !nameRe.MatchString(w.Name) {
			return fmt.Errorf("window %q: invalid name", w.Name)
		}
		if seenWin[w.Name] {
			return fmt.Errorf("duplicate window name %q", w.Name)
		}
		seenWin[w.Name] = true
		if w.Run != nil && w.Exec != "" {
			return fmt.Errorf("window %q: set either run or exec, not both", w.Name)
		}
		if w.Run == nil && w.Exec == "" {
			return fmt.Errorf("window %q: one of run or exec is required", w.Name)
		}
		// An exec window must adopt the class canaveral assigns, otherwise there
		// is no safe way to tell whether it is already open.
		if w.Exec != "" && !strings.Contains(w.Exec, "{{.Class}}") {
			return fmt.Errorf("window %q: exec command must pass {{.Class}} to the "+
				"application (for example --class={{.Class}}) so canaveral can "+
				"identify the window it created", w.Name)
		}
		if w.Run != nil && w.ProfileSource != "" {
			return fmt.Errorf("window %q: profile_source applies to exec windows only", w.Name)
		}
		if w.ProfileSource != "" && len(w.ProfileSeed) == 0 {
			return fmt.Errorf("window %q: profile_source is set but profile_seed lists nothing to copy", w.Name)
		}
	}
	if err := m.Layout.validate(seenWin); err != nil {
		return err
	}
	return nil
}

func (l Layout) validate(declaredWindows map[string]bool) error {
	if !l.Enabled() {
		if len(l.Default) > 0 || len(l.Current) > 0 {
			return fmt.Errorf("layout: default/current given without order")
		}
		return nil
	}

	seen := map[string]bool{}
	for _, name := range l.Order {
		if !declaredWindows[name] {
			return fmt.Errorf("layout: order references undeclared window %q", name)
		}
		if seen[name] {
			return fmt.Errorf("layout: order lists %q more than once", name)
		}
		seen[name] = true
	}

	check := func(label string, fractions map[string]float64, required, enforceSum bool) error {
		if len(fractions) == 0 {
			if required {
				return fmt.Errorf("layout: %s is required when order is set", label)
			}
			return nil
		}
		sum := 0.0
		for name, frac := range fractions {
			if !seen[name] {
				return fmt.Errorf("layout.%s: %q is not in order", label, name)
			}
			if frac <= 0 || frac > 1 {
				return fmt.Errorf("layout.%s: %q fraction %v must be between 0 and 1", label, name, frac)
			}
			sum += frac
		}
		if len(fractions) != len(l.Order) {
			return fmt.Errorf("layout.%s: covers %d window(s), order has %d", label, len(fractions), len(l.Order))
		}
		// current is not held to this: it is a live snapshot of whatever the
		// user actually resized floating windows to, which naturally drifts
		// from a perfect partition (resizing one column does not proportionally
		// shrink the others), and rejecting the manifest over that would break
		// the exact feature this section exists for.
		if !enforceSum {
			return nil
		}
		const epsilon = 0.01
		if sum < 1-epsilon || sum > 1+epsilon {
			return fmt.Errorf("layout.%s: fractions sum to %.3f, want 1.0", label, sum)
		}
		return nil
	}
	if err := check("default", l.Default, true, true); err != nil {
		return err
	}
	return check("current", l.Current, false, false)
}

// ToolchainMode returns the validated toolchain mode for the manifest.
func (m *Manifest) ToolchainMode() toolchain.Mode {
	mode, err := toolchain.ParseMode(m.Toolchain)
	if err != nil {
		return toolchain.ModeAuto
	}
	return mode
}

// Service looks up a service by name.
func (m *Manifest) Service(name string) (Service, bool) {
	for _, s := range m.Services {
		if s.Name == name {
			return s, true
		}
	}
	return Service{}, false
}

// ResolveDir resolves a manifest-relative directory against base.
func ResolveDir(base, dir string) string {
	if dir == "" || dir == "." {
		return base
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(base, dir))
}

// MergeEnv layers environment maps left to right, later maps winning.
func MergeEnv(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
