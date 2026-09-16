package wm

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// liveFractions is the arrangement TestColumnsLive asks for. Deliberately
// uneven and not a round number of percentage points, so a layout that came
// out equal-width, mirrored, or rounded to the nearest tenth all read as
// failures rather than as a pass.
var liveFractions = []float64{0.45, 0.32, 0.23}

// Opt-in because this opens real windows on the running compositor.
func TestColumnsLive(t *testing.T) {
	if os.Getenv("CANAVERAL_TEST_LIVE_COLUMNS") != "1" {
		t.Skip("set CANAVERAL_TEST_LIVE_COLUMNS=1 to test the running compositor")
	}
	ctx := context.Background()
	b := liveBackend(t, ctx)
	workspace := fmt.Sprintf("canaveral-column-test-%d", os.Getpid())
	var addresses []string
	t.Cleanup(func() {
		for _, addr := range addresses {
			_ = Close(ctx, addr)
		}
		_ = ReleaseWorkspace(ctx, workspace)
	})
	for i := range liveFractions {
		class := fmt.Sprintf("%s-%d", workspace, i)
		if err := Spawn(ctx, SpawnSpec{Class: class, Workspace: workspace, IsTerminal: true, Terminal: "alacritty", Cmd: "sleep 60"}); err != nil {
			t.Fatal(err)
		}
		addresses = append(addresses, waitForLiveClass(t, ctx, class))
	}

	cursor, active := liveCursor(t, b), liveActiveWindow(t, b)
	monitors, err := Monitors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyColumns(ctx, addresses, liveFractions); err != nil {
		t.Fatal(err)
	}
	// The user must be left exactly where they were. Hyprland's
	// implementation gets there by saving and restoring what it disturbs;
	// sway's by never focusing anything in the first place. Either way the
	// promise is the same one.
	if got := liveCursor(t, b); cursor != nil && !reflect.DeepEqual(cursor, got) {
		t.Errorf("cursor moved: %v -> %v (physical mouse movement also affects this check)", cursor, got)
	}
	if got := liveActiveWindow(t, b); active != "" && got != active {
		t.Errorf("focus moved: %v -> %v", active, got)
	}
	after, err := Monitors(ctx)
	if err != nil || !reflect.DeepEqual(monitors, after) {
		t.Errorf("monitors changed: %v -> %v, %v", monitors, after, err)
	}

	time.Sleep(time.Second)
	clients, err := Clients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	columns := make([]Client, 0, len(addresses))
	for _, addr := range addresses {
		for _, c := range clients {
			if c.Address != addr {
				continue
			}
			columns = append(columns, c)
			if c.Floating || c.Workspace.Name != workspace {
				t.Errorf("window escaped tiled workspace: %+v", c)
			}
		}
	}
	if len(columns) != len(liveFractions) {
		t.Fatalf("missing columns: %+v", columns)
	}
	for i := 1; i < len(columns); i++ {
		if columns[i].At[0] <= columns[i-1].At[0] || columns[i].At[1] != columns[0].At[1] {
			t.Errorf("columns are not left-to-right: %+v", columns)
		}
	}

	// The widths themselves, which is the whole point of [layout] and the
	// part a "the commands were accepted" check cannot speak to. Measured
	// against the row rather than the screen so gaps and borders, which
	// belong to the user's config, do not read as an error.
	total := 0
	for _, c := range columns {
		total += c.Size[0]
	}
	for i, c := range columns {
		got := float64(c.Size[0]) / float64(total)
		if math.Abs(got-liveFractions[i]) > 0.03 {
			t.Errorf("column %d is %.0f%% of the row, want %.0f%%", i, got*100, liveFractions[i]*100)
		}
	}
	t.Logf("original cursor %v and focus %v; columns: %+v", cursor, active, columns)
}

func waitForLiveClass(t *testing.T, ctx context.Context, class string) string {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		clients, err := Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range clients {
			if c.InitialClass == class {
				return c.Address
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("window %s did not map", class)
	return ""
}

// liveCursor returns the pointer position, or nil where the compositor has no
// way to report it. Only Hyprland does; a nil reading skips the check rather
// than inventing a pass.
func liveCursor(t *testing.T, b backend) map[string]any {
	t.Helper()
	if b.name() != "hyprland" {
		return nil
	}
	out, err := exec.Command("hyprctl", "cursorpos", "-j").Output()
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(out, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// liveActiveWindow returns an opaque handle for whatever has focus, or "" if
// nothing does.
func liveActiveWindow(t *testing.T, b backend) string {
	t.Helper()
	switch b.name() {
	case "hyprland":
		out, err := exec.Command("hyprctl", "activewindow", "-j").Output()
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Address string `json:"address"`
		}
		if err := json.Unmarshal(out, &value); err != nil {
			t.Fatal(err)
		}
		return value.Address
	case "sway":
		root, err := swaywm{}.tree(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		return swayFocusedID(root)
	}
	return ""
}

func swayFocusedID(n swayNode) string {
	if n.Focused && n.isWindow() {
		return fmt.Sprint(n.ID)
	}
	for _, c := range append(append([]swayNode{}, n.Nodes...), n.FloatingNodes...) {
		if id := swayFocusedID(c); id != "" {
			return id
		}
	}
	return ""
}
