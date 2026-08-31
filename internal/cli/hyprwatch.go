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

	"github.com/bandito/canaveral/internal/hyprevents"
)

func runHyprwatch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("hyprwatch", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: canaveral hyprwatch [flags]

Watch Hyprland's event socket and record a feature's window layout the
moment you leave its workspace, so the next reset restores the column
widths you last dragged rather than the manifest's defaults. Runs in the
foreground; use --install to run it as a persistent systemd user service.

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
	// Seeded once at startup so the very first workspace switch after
	// hyprwatch starts is still recognised as a "leave" if it began on a
	// feature workspace.
	lastName, lastID := activeWorkspace(ctx)

	fmt.Fprintln(os.Stderr, "canaveral hyprwatch: watching for workspace changes")
	return hyprevents.Watch(ctx, func(e hyprevents.Event) {
		if e.Type != "workspacev2" {
			return
		}
		// Payload is "id,name".
		idStr, name, ok := strings.Cut(e.Data, ",")
		if !ok || name == lastName {
			return
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "event: %s>>%s\n", e.Type, e.Data)
		}
		id, _ := strconv.Atoi(idStr)
		snapshotLayoutOnLeave(ctx, lastName, lastID, verbose)
		lastName, lastID = name, id
	})
}

const hyprwatchUnit = "canaveral-hyprwatch.service"

func installHyprwatch(ctx context.Context) error {
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
Description=canaveral hyprland layout watcher
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
