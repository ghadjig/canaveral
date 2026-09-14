package hypr

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var addressPattern = regexp.MustCompile(`^0x[0-9a-fA-F]+$`)

// ApplyColumns arranges already-mapped windows into a dwindle column chain.
// Focus changes, tiling and restoration run in one compositor request, with
// cursor warping disabled, rather than holding focus across application startup.
func ApplyColumns(ctx context.Context, addresses []string, ratios []float64) error {
	if len(addresses) < 2 {
		return nil
	}
	if len(ratios) != len(addresses)-1 {
		return fmt.Errorf("column layout needs one ratio per split")
	}
	seen := map[string]bool{}
	for _, addr := range addresses {
		if !addressPattern.MatchString(addr) || seen[addr] {
			return fmt.Errorf("invalid or duplicate column address %q", addr)
		}
		seen[addr] = true
	}
	for _, ratio := range ratios {
		if math.IsNaN(ratio) || ratio < 0.1 || ratio > 1.9 {
			return fmt.Errorf("invalid column ratio %v", ratio)
		}
	}
	clients, err := Clients(ctx)
	if err != nil {
		return err
	}
	workspace := ""
	for _, addr := range addresses {
		found := false
		for _, c := range clients {
			if c.Address != addr {
				continue
			}
			if workspace == "" {
				workspace = c.Workspace.Name
			}
			if workspace == "" || c.Workspace.Name != workspace {
				return fmt.Errorf("column windows must share a workspace")
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("column window %s disappeared", addr)
		}
	}

	// Read immediately before the batch, after every slow spawn has finished.
	// A snapshot taken before service startup sends the user back to a view
	// they may have left minutes ago.
	options := []string{"cursor:no_warps", "dwindle:use_active_for_splits", "dwindle:permanent_direction_override", "binds:workspace_back_and_forth"}
	values := make([]int, len(options))
	for i, option := range options {
		out, err := exec.CommandContext(ctx, "hyprctl", "getoption", option, "-j").Output()
		if err != nil {
			return fmt.Errorf("read %s: %w", option, err)
		}
		var value struct {
			Int *int `json:"int"`
		}
		if err := json.Unmarshal(out, &value); err != nil || value.Int == nil {
			return fmt.Errorf("read %s: expected an integer option", option)
		}
		values[i] = *value.Int
	}
	monitors, err := Monitors(ctx)
	if err != nil {
		return err
	}
	workspaces, err := Workspaces(ctx)
	if err != nil {
		return err
	}
	targetMonitor := ""
	for _, ws := range workspaces {
		if ws.Name == workspace {
			targetMonitor = ws.Monitor
			break
		}
	}
	if targetMonitor == "" {
		return fmt.Errorf("cannot locate column workspace %q", workspace)
	}
	var restore []string
	focused := ""
	for _, m := range monitors {
		// Batch commands are separated by semicolons. Never allow names
		// read from the compositor to introduce an extra command.
		if m.Name == "" || m.ActiveWorkspace.Name == "" || strings.ContainsAny(m.Name+m.ActiveWorkspace.Name, ";\n\r") {
			return fmt.Errorf("cannot restore monitor %q workspace %q", m.Name, m.ActiveWorkspace.Name)
		}
		// Only the target monitor's visible workspace changes. Dispatching
		// an already-active workspace produces an error in Hyprland 0.50,
		// and can toggle a special workspace the user has open there.
		if m.Name == targetMonitor && m.ActiveWorkspace.Name != workspace {
			restore = append(restore, "dispatch focusmonitor "+m.Name, "dispatch workspace "+workspaceArg(m.ActiveWorkspace.Name))
		}
		if m.Focused {
			focused = m.Name
		}
	}
	if focused == "" {
		return fmt.Errorf("cannot restore focus: no focused monitor")
	}
	active, err := exec.CommandContext(ctx, "hyprctl", "activewindow", "-j").Output()
	if err != nil {
		return fmt.Errorf("read active window: %w", err)
	}
	var window Client
	if err := json.Unmarshal(active, &window); err != nil {
		return fmt.Errorf("parse active window: %w", err)
	}
	restore = append(restore, "dispatch focusmonitor "+focused)
	if window.Address != "" {
		if !addressPattern.MatchString(window.Address) {
			return fmt.Errorf("invalid active window address %q", window.Address)
		}
		restore = append(restore, "dispatch focuswindow address:"+window.Address)
	}

	commands := []string{
		"keyword cursor:no_warps 1",
		"keyword dwindle:use_active_for_splits 1",
		"keyword dwindle:permanent_direction_override 0",
		"keyword binds:workspace_back_and_forth 0",
		// settiled uses the active workspace of the window's monitor, so
		// activate the target before removing/reinserting its tiled nodes.
		"dispatch focuswindow address:" + addresses[0],
	}
	// Retile mapped windows instead of waiting for processes while preselect
	// is armed. setfloating removes their old tree; settiled synchronously
	// inserts each beside the previous column within this same IPC batch.
	for _, addr := range addresses {
		commands = append(commands, "dispatch setfloating address:"+addr)
	}
	commands = append(commands, "dispatch settiled address:"+addresses[0])
	// Hyprland 0.55 moved splitratio into layoutmsg and put `exact` after
	// the ratio. The old dispatcher fails after the windows have already
	// opened, leaving a plausible-looking layout despite a failed reconcile.
	for i := 1; i < len(addresses); i++ {
		commands = append(commands,
			"dispatch focuswindow address:"+addresses[i-1],
			"dispatch layoutmsg preselect r",
			"dispatch settiled address:"+addresses[i],
			"dispatch focuswindow address:"+addresses[i-1],
			fmt.Sprintf("dispatch layoutmsg splitratio %.4f exact", ratios[i-1]))
	}
	// A non-direction resets preselection, including when a window vanished
	// during the batch and never consumed its pending split.
	commands = append(commands, "dispatch layoutmsg preselect none")
	commands = append(commands, restore...)
	// Restore no_warps last: restoring the user's window must not move their
	// pointer either. The batch continues through dispatcher errors, so its
	// cleanup remains part of the request even if a window closes meanwhile.
	for i := len(options) - 1; i >= 0; i-- {
		commands = append(commands, "keyword "+options[i]+" "+strconv.Itoa(values[i]))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once submitted, allow cleanup to finish even if creation is cancelled.
	batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(batchCtx, "hyprctl", "--batch", strings.Join(commands, "; ")).CombinedOutput()
	if err != nil {
		return fmt.Errorf("apply column layout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" && line != "ok" {
			return fmt.Errorf("apply column layout: %s", strings.TrimSpace(string(out)))
		}
	}
	return nil
}
