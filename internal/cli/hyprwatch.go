package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/hyprevents"
)

// waybarSignal is the real-time signal number waybar's custom/canaveral
// module listens on (SIGRTMIN+8), matching the "signal": 8 entry in
// config.jsonc. Chosen because no other module in this config used a signal
// at the time this was written; canaveral hyprwatch --install re-checks this
// is still true before installing.
const waybarSignal = 8

// relevantEvents are the only events that can change what canaveral's waybar
// module needs to display: which feature workspaces exist, and which one is
// active. Everything else (window focus churn, layer changes, and so on) is
// ignored so the module is not refreshed on unrelated activity.
var relevantEvents = map[string]bool{
	"workspace":          true,
	"workspacev2":        true,
	"createworkspace":    true,
	"createworkspacev2":  true,
	"destroyworkspace":   true,
	"destroyworkspacev2": true,
}

func runHyprwatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hyprwatch", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: canaveral hyprwatch [flags]

Watch Hyprland's event socket and signal waybar's canaveral module the
instant a feature workspace is created, removed, or focus changes, instead
of polling on a timer. Runs in the foreground; use --install to run it as a
persistent systemd user service.

Flags:`)
		fs.PrintDefaults()
	}
	var (
		install = fs.Bool("install", false, "install and start a systemd user unit, then exit")
		verbose = fs.Bool("verbose", false, "log every relevant event to stderr")
	)
	if _, err := parseArgs(fs, args); err != nil {
		return err
	}

	if *install {
		return installHyprwatch(ctx)
	}
	return watchLoop(ctx, *verbose)
}

func watchLoop(ctx context.Context, verbose bool) error {
	// Debounced rather than signalling on every single event: tearing down a
	// feature closes 4 windows in a burst, each producing its own event, and
	// waybar re-running the module 4 times in 20ms is wasted work for a
	// result that will be identical each time.
	pending := debounce(ctx, 120*time.Millisecond, func() { signalWaybar(verbose) })

	// Seeded once at startup so the very first workspace switch after
	// hyprwatch starts is still recognised as a "leave" if it began on a
	// feature workspace.
	lastName, lastID := activeWorkspace(ctx)

	fmt.Fprintln(os.Stderr, "canaveral hyprwatch: watching for workspace changes")
	return hyprevents.Watch(ctx, func(e hyprevents.Event) {
		if e.Type == "workspacev2" {
			// Payload is "id,name".
			if idStr, name, ok := strings.Cut(e.Data, ","); ok && name != lastName {
				id, _ := strconv.Atoi(idStr)
				snapshotLayoutOnLeave(ctx, lastName, lastID, verbose)
				lastName, lastID = name, id
			}
		}
		if !relevantEvents[e.Type] {
			return
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "event: %s>>%s\n", e.Type, e.Data)
		}
		pending()
	})
}

// debounce returns a function that, however many times it is called in
// quick succession, invokes fire only once — after delay has passed since the
// most recent call. It stops entirely once ctx is cancelled.
func debounce(ctx context.Context, delay time.Duration, fire func()) func() {
	trigger := make(chan struct{}, 1)
	go func() {
		var timer *time.Timer
		var timerC <-chan time.Time
		for {
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case <-trigger:
				if timer != nil {
					timer.Stop()
				}
				timer = time.NewTimer(delay)
				timerC = timer.C
			case <-timerC:
				timerC = nil
				fire()
			}
		}
	}()
	return func() {
		select {
		case trigger <- struct{}{}:
		default:
			// A pending debounce window already covers this call.
		}
	}
}

func signalWaybar(verbose bool) {
	out, err := exec.Command("pgrep", "-x", "waybar").Output()
	if err != nil {
		if verbose {
			fmt.Fprintln(os.Stderr, "signal: waybar is not running")
		}
		return
	}
	sig := syscallRTMIN(waybarSignal)
	for _, line := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			continue
		}
		if err := signalPID(pid, sig); err != nil && verbose {
			fmt.Fprintf(os.Stderr, "signal: pid %d: %v\n", pid, err)
		}
	}
}

const hyprwatchUnit = "canaveral-hyprwatch.service"

func installHyprwatch(ctx context.Context) error {
	if err := checkWaybarSignalFree(); err != nil {
		return err
	}

	bin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(bin); err == nil {
		bin = resolved
	}

	unitDir, err := userUnitDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		return err
	}
	unitPath := filepath.Join(unitDir, hyprwatchUnit)

	body := fmt.Sprintf(`[Unit]
Description=canaveral hyprland event watcher
After=graphical-session.target

[Service]
ExecStart=%s hyprwatch
Restart=on-failure
RestartSec=2

[Install]
WantedBy=graphical-session.target
`, bin)

	if err := os.WriteFile(unitPath, []byte(body), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}

	r := reporter{}
	r.OK("wrote %s", unitPath)

	if err := runQuiet(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := runQuiet(ctx, "systemctl", "--user", "enable", "--now", hyprwatchUnit); err != nil {
		return err
	}
	r.OK("%s is enabled and running", hyprwatchUnit)
	r.Info("check it any time with: systemctl --user status %s", hyprwatchUnit)
	return nil
}

// checkWaybarSignalFree warns (without failing) if another waybar module
// already claims the signal number canaveral is about to install, since two
// modules sharing one signal would both refresh on every event, silently.
func checkWaybarSignalFree() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cfg := filepath.Join(home, ".config", "waybar", "config.jsonc")
	b, err := os.ReadFile(cfg)
	if err != nil {
		return nil
	}
	want := fmt.Sprintf(`"signal": %d`, waybarSignal)
	// "custom/canaveral" owning it is expected and fine; anything else sharing
	// the number is worth a heads-up, not a hard failure.
	if strings.Contains(string(b), want) && !strings.Contains(string(b), `"custom/canaveral"`) {
		reporter{}.Warn("signal %d appears used by more than one waybar module; check %s", waybarSignal, cfg)
	}
	return nil
}

func userUnitDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user"), nil
}

func runQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
