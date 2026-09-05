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
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/config"
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
	Ports map[string]int `toml:"ports"`
	// Precheck is a command run on every open, before any service starts,
	// aborting the open when it exits non-zero.
	//
	// Distinct from worktree.setup and database.setup, which provision a
	// worktree once when it is created. What belongs here is whatever must be
	// true *each time* a feature comes up and is not a property of the
	// worktree: a database server that is running, migrations that match the
	// branch, a VPN that is connected. Those stop being true while nobody is
	// looking, and the alternative to asserting them here is discovering it
	// from a readiness probe several silent minutes later.
	//
	// Runs in the feature's worktree with the resolved toolchain on PATH, so
	// `bin/rails` and friends work exactly as they do for a service.
	Precheck string `toml:"precheck"`
	// PrecheckTimeout bounds the precheck command. Defaults to 5 minutes:
	// this runs on every open, so it should fail fast rather than wedge one.
	PrecheckTimeout Duration  `toml:"precheck_timeout"`
	Database        Database  `toml:"database"`
	Worktree        Worktree  `toml:"worktree"`
	Services        []Service `toml:"service"`
	Agents          []Agent   `toml:"agent"`
	Windows         []Window  `toml:"window"`
	Layout          Layout    `toml:"layout"`

	// Root is the absolute directory containing the manifest. Not read from TOML.
	Root string `toml:"-"`
}

// Layout forces a fixed column arrangement for windows instead of leaving
// them to Hyprland's normal tiling.
//
// Each named window becomes its own full-height column, left to right in
// Order, sized as a fraction of the monitor's usable width. The layout is
// applied when a feature's windows are first spawned; resizing them
// afterwards is yours to keep for that session and is not recorded, so
// Default is what every feature of a project starts from.
type Layout struct {
	// Order lists window names left to right. Every entry must be a declared
	// window name, and every declared window must appear exactly once.
	Order []string `toml:"order"`
	// Default maps window name to fraction of monitor width. Must sum to 1.0
	// (within floating point tolerance) and cover exactly the names in Order.
	Default map[string]float64 `toml:"default"`
}

// Enabled reports whether a layout is configured at all. An empty Layout
// (the zero value, when [layout] is absent from the manifest) leaves windows
// to Hyprland's normal tiling untouched.
func (l Layout) Enabled() bool { return len(l.Order) > 0 }

// Fractions returns the configured width fraction for each window in Order.
func (l Layout) Fractions() map[string]float64 { return l.Default }

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
	// Root is where this project's worktrees are created. A relative path
	// is resolved against the project root, so "worktrees" (the default)
	// keeps them beside the code and reachable as ./worktrees/<feature>;
	// an absolute path (with ~ expanded) puts them wherever you like.
	//
	// The special value "state" opts back into canaveral's own state
	// directory instead, leaving the repo completely untouched. Putting
	// worktrees in the repo costs a .gitignore line and means
	// non-gitignore-aware tools (plain grep -r, find) will descend into
	// every feature's copy.
	Root string `toml:"root"`
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
	// Discover reads back ports this service chose for itself, for the
	// services that are not told which port to use.
	Discover Discover `toml:"discover"`
	// Optional services may fail without aborting the feature.
	Optional bool `toml:"optional"`
}

// Discover describes how to learn the ports a service picked for itself.
//
// Ports normally come from [ports] and a feature's slot, which is enough when
// the service is told what to bind. It is not enough when the service decides
// for itself and only says so afterwards — a dev server given :0, a tunnel
// handed a random public port, a wrapper that derives its port forwards from
// a hash of the branch name. Declaring the port canaveral wishes it would use
// does not make it true; reading back the one it actually used does.
//
// Discovery happens after the service starts and before its readiness probe,
// so `ready.http` may refer to a port discovered from that same service.
// Discovered ports join {{.Port}} and {{.URL}} and are exported as
// CANAVERAL_PORT_*, so nothing downstream needs to know which kind it got.
type Discover struct {
	// Port maps a logical name to a regular expression matched against the
	// service's log. Each must have exactly one capture group, holding the
	// port. Prefer TOML literal strings so backslashes need no escaping:
	//
	//	discover.port.web = 'Port mappings: (\d+):3000'
	Port map[string]string `toml:"port"`
	// Cmd is an alternative source for anything a regular expression cannot
	// express: a command run in the service's directory that prints
	// `name=port` lines. It is retried until it exits zero having reported at
	// least one port, so a script that cannot answer yet should fail rather
	// than print nothing.
	//
	// Mutually exclusive with Port. Names it reports are not known until it
	// runs, so unlike Port they cannot be checked against [ports] up front.
	Cmd string `toml:"cmd"`
	// Timeout bounds discovery. Defaults to a minute — what is being waited
	// for is a line of output, not a booted application.
	Timeout Duration `toml:"timeout"`
}

// Enabled reports whether the service declares any discovery at all.
func (d Discover) Enabled() bool { return len(d.Port) > 0 || d.Cmd != "" }

// Names lists the declared port names in sorted order, or nil under Cmd,
// where the names are whatever the command turns out to print.
func (d Discover) Names() []string {
	if len(d.Port) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.Port))
	for name := range d.Port {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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

// Agent is a coding agent canaveral runs for a feature.
type Agent struct {
	Name string `toml:"name"`
	// Tool names the harness to run, e.g. "opencode" or "claude". Empty
	// means whichever agent this machine defaults to — see internal/config,
	// which is where a preference that belongs to you rather than to the
	// project is recorded.
	Tool string            `toml:"tool"`
	Dir  string            `toml:"dir"`
	Env  map[string]string `toml:"env"`
	// Model overrides the agent's default model.
	Model string `toml:"model"`
	// Agent selects a named persona or mode to start in, e.g. opencode's
	// "build" or "plan". Harnesses that have no such notion — Claude Code
	// picks its subagents per task rather than per session — ignore it.
	Agent string `toml:"agent"`
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

// ServiceNames lists the declared services in manifest order.
func (m *Manifest) ServiceNames() []string {
	out := make([]string, 0, len(m.Services))
	for _, s := range m.Services {
		out = append(out, s.Name)
	}
	return out
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

// normalize fills in defaults and validates every section of a parsed
// manifest. Sections are independent of one another, so each gets its own
// method; this is just their fixed call order.
func (m *Manifest) normalize() error {
	if err := m.normalizeCore(); err != nil {
		return err
	}
	if err := m.normalizeDatabase(); err != nil {
		return err
	}
	if err := m.normalizePorts(); err != nil {
		return err
	}
	if err := m.normalizeServices(); err != nil {
		return err
	}
	if err := m.normalizeAgents(); err != nil {
		return err
	}
	seenWin, err := m.normalizeWindows()
	if err != nil {
		return err
	}
	return m.Layout.validate(seenWin)
}

// normalizeCore fills in defaults for and validates the manifest's top-level
// scalar fields: name, branch template, toolchain mode and terminal.
func (m *Manifest) normalizeCore() error {
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
	return nil
}

// normalizeDatabase fills in defaults for and validates [database].
func (m *Manifest) normalizeDatabase() error {
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
	return nil
}

// normalizePorts validates [ports].
func (m *Manifest) normalizePorts() error {
	for name, base := range m.Ports {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("invalid port name %q", name)
		}
		if base < 1 || base > 65535 {
			return fmt.Errorf("port %q: base %d out of range", name, base)
		}
	}
	return nil
}

// normalizeServices fills in defaults for and validates every [[services]].
func (m *Manifest) normalizeServices() error {
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
		if err := m.validateDiscover(s); err != nil {
			return err
		}
	}
	return nil
}

// validateDiscover checks one service's [discover] block.
//
// The static checks are worth making because the alternative surfaces late
// and misleadingly: a pattern that cannot compile, or captures nothing,
// becomes a discovery timeout minutes into a boot rather than a parse error
// before anything starts.
func (m *Manifest) validateDiscover(s *Service) error {
	d := s.Discover
	if !d.Enabled() {
		return nil
	}
	if d.Cmd != "" && len(d.Port) > 0 {
		return fmt.Errorf("service %q: set either discover.cmd or discover.port, not both", s.Name)
	}
	for _, name := range d.Names() {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("service %q: invalid discover.port name %q", s.Name, name)
		}
		// A name in both places has two answers and no rule for choosing
		// between them that a reader could predict.
		if _, ok := m.Ports[name]; ok {
			return fmt.Errorf("service %q: discover.port.%s also declared in [ports]; "+
				"a port is either allocated or discovered, not both", s.Name, name)
		}
		re, err := regexp.Compile(d.Port[name])
		if err != nil {
			return fmt.Errorf("service %q: discover.port.%s: %w", s.Name, name, err)
		}
		if n := re.NumSubexp(); n != 1 {
			return fmt.Errorf("service %q: discover.port.%s must have exactly one capture "+
				"group holding the port, found %d", s.Name, name, n)
		}
	}
	return nil
}

// normalizeAgents fills in defaults for and validates every [[agents]].
func (m *Manifest) normalizeAgents() error {
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
			a.Tool = config.DefaultAgentTool()
		}
		if !agent.Known(a.Tool) {
			return fmt.Errorf("agent %q: unsupported tool %q (known: %s)",
				a.Name, a.Tool, strings.Join(agent.Tools(), ", "))
		}
		if a.Dir == "" {
			a.Dir = "."
		}
	}
	return nil
}

// normalizeWindows fills in defaults for and validates every [[windows]],
// returning the set of declared window names for Layout.validate to check
// [layout]'s references against.
func (m *Manifest) normalizeWindows() (map[string]bool, error) {
	seenWin := map[string]bool{}
	for i := range m.Windows {
		w := &m.Windows[i]
		if w.Name == "" {
			return nil, fmt.Errorf("window #%d: name is required", i+1)
		}
		if !nameRe.MatchString(w.Name) {
			return nil, fmt.Errorf("window %q: invalid name", w.Name)
		}
		if seenWin[w.Name] {
			return nil, fmt.Errorf("duplicate window name %q", w.Name)
		}
		seenWin[w.Name] = true
		if err := w.validate(); err != nil {
			return nil, err
		}
	}
	return seenWin, nil
}

// validate checks a single [[windows]] entry's own fields, independent of
// any other window: exactly one of run/exec, an exec command that adopts
// canaveral's window class, and profile_source/profile_seed used together.
func (w Window) validate() error {
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
	return nil
}

func (l Layout) validate(declaredWindows map[string]bool) error {
	if !l.Enabled() {
		if len(l.Default) > 0 {
			return fmt.Errorf("layout: default given without order")
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

	return l.validateDefaultFractions(seen)
}

// validateDefaultFractions validates [layout.default]: required when order
// is set, and must partition the windows exactly (fractions sum to 1).
func (l Layout) validateDefaultFractions(seen map[string]bool) error {
	if len(l.Default) == 0 {
		return fmt.Errorf("layout: default is required when order is set")
	}
	sum, err := validateFractions("default", l.Default, seen, len(l.Order))
	if err != nil {
		return err
	}
	const epsilon = 0.01
	if sum < 1-epsilon || sum > 1+epsilon {
		return fmt.Errorf("layout.default: fractions sum to %.3f, want 1.0", sum)
	}
	return nil
}

// validateFractions checks that every name in fractions is in order, every
// fraction is between 0 (exclusive) and 1 (inclusive), and fractions covers
// exactly as many windows as order does. Returns the sum of fractions for
// callers that also need to enforce it summing to 1.
func validateFractions(label string, fractions map[string]float64, seen map[string]bool, orderLen int) (float64, error) {
	sum := 0.0
	for name, frac := range fractions {
		if !seen[name] {
			return 0, fmt.Errorf("layout.%s: %q is not in order", label, name)
		}
		if frac <= 0 || frac > 1 {
			return 0, fmt.Errorf("layout.%s: %q fraction %v must be between 0 and 1", label, name, frac)
		}
		sum += frac
	}
	if len(fractions) != orderLen {
		return 0, fmt.Errorf("layout.%s: covers %d window(s), order has %d", label, len(fractions), orderLen)
	}
	return sum, nil
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

// WorktreeRoot resolves where this project's worktrees belong, or "" to use
// canaveral's own state directory (the "state" opt-out).
//
// Unset defaults to "worktrees", so a project gets worktrees beside its code
// without having to configure anything -- matching how every other feature
// of a project lives in the repo. Relative paths are resolved against the
// project root rather than the working directory, so the setting means the
// same thing no matter where a command is run from.
func (m *Manifest) WorktreeRoot() (string, error) {
	raw := strings.TrimSpace(m.Worktree.Root)
	if raw == "" {
		raw = "worktrees"
	}
	if raw == "state" {
		return "", nil
	}
	if strings.HasPrefix(raw, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~"))
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), nil
	}
	return filepath.Join(m.Root, raw), nil
}
