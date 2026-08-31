// Package unit manages transient systemd --user services and reads their cgroup stats.
package unit

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Prefix namespaces every unit canaveral creates.
const Prefix = "canaveral"

// ErrNotFound indicates the unit is not known to systemd.
var ErrNotFound = errors.New("unit not found")

// Spec describes a transient service to start.
type Spec struct {
	Name        string // full unit name without the .service suffix
	Description string
	Dir         string
	Cmd         string // executed via `sh -lc`
	Env         map[string]string
	LogPath     string
}

// Name builds a deterministic unit name for a workspace resource.
func Name(workspace, kind, name string) string {
	return fmt.Sprintf("%s-%s-%s-%s", Prefix, sanitize(workspace), kind, sanitize(name))
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Start launches the spec as a transient systemd user service.
func Start(ctx context.Context, s Spec) error {
	if s.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o755); err != nil {
			return fmt.Errorf("create log dir: %w", err)
		}
		// Truncate so port discovery and log probes never match a previous run.
		if err := os.WriteFile(s.LogPath, nil, 0o644); err != nil {
			return fmt.Errorf("reset log: %w", err)
		}
	}

	args := []string{
		"--user",
		"--unit=" + s.Name,
		"--service-type=exec",
		"--quiet",
	}
	if s.Description != "" {
		args = append(args, "--description="+s.Description)
	}
	if s.Dir != "" {
		args = append(args, "--working-directory="+s.Dir)
	}
	if s.LogPath != "" {
		args = append(args,
			"-p", "StandardOutput=append:"+s.LogPath,
			"-p", "StandardError=append:"+s.LogPath,
		)
	}
	// mixed sends SIGTERM to the main process but SIGKILL to the whole cgroup,
	// which is what we want for supervisors like foreman that spawn children.
	args = append(args, "-p", "KillMode=mixed", "-p", "TimeoutStopSec=15")

	env := inheritEnv(s.Env)
	for _, k := range sortedKeys(env) {
		args = append(args, "--setenv="+k+"="+env[k])
	}
	// A non-login shell is deliberate: `sh -l` re-sources /etc/profile, which
	// overwrites the PATH we inject and hides version-manager toolchains.
	args = append(args, "--", "/bin/sh", "-c", s.Cmd)

	// systemd-run blocks until the start job completes and has no timeout of
	// its own, so a wedged user manager or session bus hangs the caller
	// forever — and --quiet means it does so without printing a thing. Bound
	// it, so the worst case is an error the user can act on.
	runCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "systemd-run", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// systemd-run is a D-Bus client, not a supervisor. Cancelling ctx
		// kills the client, but the manager has already been asked to start
		// the unit and happily goes on doing so — so an interrupted launch
		// leaves a live process behind that no caller ever learns about.
		// Undo it here, where the unit name is still known.
		if runCtx.Err() != nil {
			_ = Stop(ctx, s.Name)
			Reset(ctx, s.Name)
		}
		if ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("systemd-run %s: no answer from the systemd user manager after %s "+
				"(try `systemctl --user is-system-running`)", s.Name, startTimeout)
		}
		return fmt.Errorf("systemd-run %s: %w: %s", s.Name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// startTimeout bounds a launch. Starting is not slow — with Type=exec the job
// completes as soon as the child has exec'd — so a wait this long already
// means systemd is not answering rather than that the service is warming up.
const startTimeout = 30 * time.Second

// stopTimeout bounds a teardown command. It has to clear the units' own
// TimeoutStopSec=15 with room to spare, or a service that ignores SIGTERM
// would look like a stop failure even though systemd is about to SIGKILL it.
const stopTimeout = 30 * time.Second

// teardown detaches from ctx's cancellation.
//
// Teardown almost always runs *because* something went wrong, and the most
// common something is the user pressing Ctrl-C — which cancels the very
// context the stop would otherwise inherit. exec.CommandContext on a
// cancelled context never execs at all, so every cleanup path in canaveral
// used to become a silent no-op at exactly the moment it was needed, leaving
// live units behind that nothing would ever stop.
func teardown(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), stopTimeout)
}

// Stop stops a unit and waits for systemd to release it. Missing units are ignored.
//
// Runs to completion even if ctx is already cancelled; see teardown.
func Stop(ctx context.Context, name string) error {
	ctx, cancel := teardown(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, "systemctl", "--user", "stop", name+".service")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if strings.Contains(msg, "not loaded") || strings.Contains(msg, "not found") {
			return nil
		}
		return fmt.Errorf("stop %s: %w: %s", name, err, strings.TrimSpace(msg))
	}
	return nil
}

// Reset clears a failed transient unit so the name can be reused immediately.
func Reset(ctx context.Context, name string) {
	ctx, cancel := teardown(ctx)
	defer cancel()
	_ = exec.CommandContext(ctx, "systemctl", "--user", "reset-failed", name+".service").Run()
}

// StopAll stops every named unit and clears it, returning the units that could
// not be stopped. Order is preserved: callers stop services in reverse start
// order so dependents go down before what they depend on.
func StopAll(ctx context.Context, names []string) []string {
	var failed []string
	for _, n := range names {
		if err := Stop(ctx, n); err != nil {
			failed = append(failed, n)
			continue
		}
		Reset(ctx, n)
	}
	return failed
}

// Status is a point-in-time view of a unit and its resource usage.
type Status struct {
	Name        string
	Loaded      bool
	ActiveState string // active, inactive, failed, activating
	SubState    string
	MainPID     int
	CGroup      string
	Memory      uint64        // bytes, current
	CPU         time.Duration // cumulative
	Procs       int
	Since       time.Time
}

// Running reports whether the unit is currently active.
func (s Status) Running() bool { return s.ActiveState == "active" || s.ActiveState == "activating" }

var showProps = []string{
	"LoadState", "ActiveState", "SubState", "MainPID",
	"ControlGroup", "MemoryCurrent", "CPUUsageNSec", "ActiveEnterTimestampMonotonic",
}

// queryTimeout bounds a status read. `systemctl --user show` is another D-Bus
// round trip, and the readiness wait calls it between probe attempts on the
// caller's context — so an unbounded one there outlives the probe's own
// timeout and the whole start turns into a silent hang.
const queryTimeout = 10 * time.Second

// Query fetches the current status of a unit.
func Query(ctx context.Context, name string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	st := Status{Name: name}
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "show",
		name+".service", "--property="+strings.Join(showProps, ",")).Output()
	if err != nil {
		return st, fmt.Errorf("show %s: %w", name, err)
	}

	props := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			props[k] = v
		}
	}

	st.ActiveState = props["ActiveState"]
	st.SubState = props["SubState"]
	st.Loaded = props["LoadState"] == "loaded"
	st.MainPID, _ = strconv.Atoi(props["MainPID"])
	st.CGroup = props["ControlGroup"]

	if !st.Loaded {
		return st, ErrNotFound
	}

	// systemd reports these as [not set] when the unit is inactive.
	if v, err := strconv.ParseUint(props["MemoryCurrent"], 10, 64); err == nil {
		st.Memory = v
	}
	if v, err := strconv.ParseUint(props["CPUUsageNSec"], 10, 64); err == nil {
		st.CPU = time.Duration(v)
	}
	if st.CGroup != "" {
		st.Procs = countProcs(st.CGroup)
		// Prefer cgroup counters when systemd's cached values are unavailable.
		if st.Memory == 0 {
			st.Memory = readUint(filepath.Join(cgroupRoot, st.CGroup, "memory.current"))
		}
	}
	return st, nil
}

const cgroupRoot = "/sys/fs/cgroup"

func countProcs(cgroup string) int {
	f, err := os.Open(filepath.Join(cgroupRoot, cgroup, "cgroup.procs"))
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) != "" {
			n++
		}
	}
	return n
}

func readUint(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// List returns the names of all canaveral units currently known to systemd,
// running or failed, sorted.
func List(ctx context.Context) ([]string, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "--user", "list-units",
		"--type=service", "--all", "--plain", "--no-legend", "--no-pager",
		Prefix+"-*.service").Output()
	if err != nil {
		return nil, err
	}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		names = append(names, strings.TrimSuffix(f[0], ".service"))
	}
	sort.Strings(names)
	return names, nil
}

// FeaturePrefixes returns the name prefixes every unit of a workspace shares.
//
// Two rather than one, per kind, so that "login" does not also claim
// "login-fixes": "canaveral-p-login-svc-" is not a prefix of
// "canaveral-p-login-fixes-svc-web", whereas "canaveral-p-login-" is.
func FeaturePrefixes(workspace string) []string {
	ws := sanitize(workspace)
	return []string{
		fmt.Sprintf("%s-%s-svc-", Prefix, ws),
		fmt.Sprintf("%s-%s-agent-", Prefix, ws),
	}
}

// IsFeatureUnit reports whether a unit name is a feature's service or agent,
// as opposed to one of canaveral's own long-lived units like hyprwatch.
func IsFeatureUnit(name string) bool {
	rest, ok := strings.CutPrefix(name, Prefix+"-")
	if !ok {
		return false
	}
	return strings.Contains(rest, "-svc-") || strings.Contains(rest, "-agent-")
}

// Orphans returns the units in live that no workspace in known lays claim to.
//
// Pure so it can be tested without systemd. Ambiguity is resolved in favour of
// leaving a unit alone: reaping something a live feature still needs is far
// worse than missing a corpse, which the next sweep catches anyway.
func Orphans(live, known []string) []string {
	var prefixes []string
	for _, ws := range known {
		prefixes = append(prefixes, FeaturePrefixes(ws)...)
	}
	var out []string
	for _, u := range live {
		if !IsFeatureUnit(u) {
			continue
		}
		claimed := false
		for _, p := range prefixes {
			if strings.HasPrefix(u, p) {
				claimed = true
				break
			}
		}
		if !claimed {
			out = append(out, u)
		}
	}
	return out
}

// FeatureUnits returns every unit systemd currently holds for a workspace,
// whether or not the feature's state file knows about it.
//
// State is written after a unit starts, so anything that interrupts a
// reconcile in between leaves a unit that no record mentions. Unit names are
// deterministic, so systemd itself can be asked instead — it is the only
// source that cannot be out of date.
//
// The query detaches from ctx's cancellation for the same reason Stop does:
// its only callers are teardown paths, which run precisely when ctx has been
// cancelled, and a listing that returns nothing there would quietly reduce the
// sweep to the state-file behaviour it exists to backstop.
func FeatureUnits(ctx context.Context, workspace string) ([]string, error) {
	ctx, cancel := teardown(ctx)
	defer cancel()

	all, err := List(ctx)
	if err != nil {
		return nil, err
	}
	prefixes := FeaturePrefixes(workspace)
	var out []string
	for _, u := range all {
		for _, p := range prefixes {
			if strings.HasPrefix(u, p) {
				out = append(out, u)
				break
			}
		}
	}
	return out, nil
}

// Available reports whether a usable systemd user manager is reachable.
func Available(ctx context.Context) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return errors.New("systemd-run not found in PATH")
	}
	// Bounded, because this is the check that exists to catch an unreachable
	// manager: hanging on it would be the exact failure it screens for.
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	if err := exec.CommandContext(ctx, "systemctl", "--user", "is-system-running").Run(); err != nil {
		// is-system-running exits non-zero for "degraded", which is still usable.
		out, _ := exec.CommandContext(ctx, "systemctl", "--user", "show", "-p", "Version").Output()
		if len(bytes.TrimSpace(out)) == 0 {
			return errors.New("no systemd user manager reachable (is DBUS_SESSION_BUS_ADDRESS set?)")
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// inheritPassthrough are variables copied from the invoking shell into units.
//
// systemd user services start with a minimal environment, but dev toolchains
// (mise, asdf, rbenv, nvm, ~/.local/bin) live entirely on the caller's PATH.
// Without this, commands that work in the user's terminal fail inside a unit.
var inheritPassthrough = []string{
	"PATH", "LANG", "LC_ALL", "SSH_AUTH_SOCK",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
	"MISE_SHELL", "ASDF_DIR", "ASDF_DATA_DIR",
	"NODE_VERSION", "RBENV_ROOT", "GEM_HOME", "GEM_PATH",
}

// inheritEnv layers explicit spec env on top of inherited shell variables.
func inheritEnv(explicit map[string]string) map[string]string {
	out := make(map[string]string, len(explicit)+len(inheritPassthrough))
	for _, k := range inheritPassthrough {
		if v, ok := os.LookupEnv(k); ok && v != "" {
			out[k] = v
		}
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}
