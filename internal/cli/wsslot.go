package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/state"
)

// wsSlotJSON is waybar's custom-module contract: one line of JSON, where
// class selects the CSS rule that styles the pill.
type wsSlotJSON struct {
	Text  string `json:"text"`
	Class string `json:"class"`
}

func runWSSlot(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ws-slot", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: canaveral ws-slot [n] [flags]\n\n"+
			"Map a stable, 1-indexed slot number to a feature's workspace, for a\n"+
			"status bar with one widget per slot. Without a number, list every slot.\n\n"+
			"A feature keeps its slot for as long as it exists, so a slot means the\n"+
			"same workspace tomorrow. Removing a feature frees its number for reuse.\n\n"+
			"  canaveral ws-slot          # every slot, one per line\n"+
			"  canaveral ws-slot 2        # the workspace name in slot 2\n"+
			"  canaveral ws-slot 2 --json # waybar custom-module JSON\n\nFlags:")
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "emit waybar JSON ({\"text\",\"class\"}) instead of a bare name")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return fmt.Errorf("expected at most one slot number, got %d", len(pos))
	}

	features, err := state.EnsureWSlots()
	if err != nil {
		return err
	}

	if len(pos) == 0 {
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(features)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "%s\t%s\t%s\n", "SLOT", "WORKSPACE", "FEATURE")
		for _, f := range features {
			fmt.Fprintf(tw, "%d\t%s\t%s\n", f.WSlot, f.HyprWorkspace(), f.Key())
		}
		return tw.Flush()
	}

	n, err := strconv.Atoi(pos[0])
	if err != nil {
		return fmt.Errorf("slot must be a number, got %q", pos[0])
	}

	var found *state.Feature
	for _, f := range features {
		if f.WSlot == n {
			found = f
			break
		}
	}

	if !*asJSON {
		if found == nil {
			// Empty, not an error: a bar with six fixed widgets asks about
			// slots that hold nothing most of the time.
			return nil
		}
		fmt.Println(found.HyprWorkspace())
		return nil
	}

	out := wsSlotJSON{Class: "hidden"}
	if found != nil {
		out.Text = found.HyprWorkspace()
		out.Class = "inactive"
		// Only ask Hyprland once a slot is actually occupied, so the widget
		// still renders without a compositor running.
		if active, err := hypr.ActiveWorkspaceName(ctx); err == nil && active == out.Text {
			out.Class = "active"
		}
	}
	// Encoder writes the trailing newline waybar reads as end-of-line.
	return json.NewEncoder(os.Stdout).Encode(out)
}
