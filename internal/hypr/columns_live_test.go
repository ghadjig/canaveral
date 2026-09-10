package hypr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

// Opt-in because this opens real windows on the running compositor.
func TestColumnsLive(t *testing.T) {
	if os.Getenv("CANAVERAL_TEST_LIVE_COLUMNS") != "1" {
		t.Skip("set CANAVERAL_TEST_LIVE_COLUMNS=1 to test the running Hyprland")
	}
	ctx := context.Background()
	workspace := fmt.Sprintf("canaveral-column-test-%d", os.Getpid())
	var addresses []string
	t.Cleanup(func() {
		for _, addr := range addresses {
			_ = Close(ctx, addr)
		}
	})
	for i := range 3 {
		class := fmt.Sprintf("%s-%d", workspace, i)
		if err := Spawn(ctx, SpawnSpec{Class: class, Workspace: workspace, IsTerminal: true, Terminal: "alacritty", Cmd: "sleep 60"}); err != nil {
			t.Fatal(err)
		}
		found := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
			clients, err := Clients(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range clients {
				if c.InitialClass == class {
					addresses = append(addresses, c.Address)
					found = true
					break
				}
			}
			if found {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !found {
			t.Fatalf("window %s did not map", class)
		}
	}
	snapshot := func(command string) map[string]any {
		t.Helper()
		out, err := exec.Command("hyprctl", command, "-j").Output()
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(out, &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	cursor, active := snapshot("cursorpos"), snapshot("activewindow")["address"]
	monitors, err := Monitors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyColumns(ctx, addresses, []float64{0.8, 1}); err != nil {
		t.Fatal(err)
	}
	if got := snapshot("cursorpos"); !reflect.DeepEqual(cursor, got) {
		t.Errorf("cursor moved: %v -> %v (physical mouse movement also affects this check)", cursor, got)
	}
	if got := snapshot("activewindow")["address"]; got != active {
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
	var columns []Client
	for _, addr := range addresses {
		for _, c := range clients {
			if c.Address == addr {
				columns = append(columns, c)
				if c.Floating || c.Workspace.Name != workspace {
					t.Errorf("window escaped tiled workspace: %+v", c)
				}
			}
		}
	}
	if len(columns) != 3 {
		t.Fatalf("missing columns: %+v", columns)
	}
	for i := 1; i < len(columns); i++ {
		if columns[i].At[0] <= columns[i-1].At[0] || columns[i].At[1] != columns[0].At[1] {
			t.Errorf("columns are not left-to-right: %+v", columns)
		}
	}
	t.Logf("original cursor %v and focus %v; columns: %+v", cursor, active, columns)
}
