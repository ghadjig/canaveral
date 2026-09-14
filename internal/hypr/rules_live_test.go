package hypr

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Opt-in: opens an isolated browser profile on the running compositor.
func TestBrowserPlacementLive(t *testing.T) {
	if os.Getenv("CANAVERAL_TEST_LIVE_BROWSER") != "1" {
		t.Skip("set CANAVERAL_TEST_LIVE_BROWSER=1 to test browser placement")
	}
	ctx := context.Background()
	class := fmt.Sprintf("canaveral-placement-test-%d", os.Getpid())
	workspace := class
	if err := EnsureRules(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ensurePlacementRule(ctx, class, workspace); err != nil {
		t.Fatal(err)
	}
	before, err := ActiveWorkspaceName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately launch outside hyprctl exec: the class rule must work
	// even when no PID-scoped exec rule can be associated with the browser.
	cmd := exec.Command("google-chrome", "--user-data-dir="+t.TempDir(), "--ozone-platform=x11", "--class="+class,
		"--no-first-run", "--no-default-browser-check", "about:blank")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if clients, err := Clients(ctx); err == nil {
			for _, c := range clients {
				if c.InitialClass == class {
					_ = Close(ctx, c.Address)
				}
			}
		}
		// Chrome's workers can still write the profile after its main process
		// exits. Stop the isolated process group before TempDir cleans it up.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		time.Sleep(200 * time.Millisecond)
	})
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		clients, err := Clients(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range clients {
			if c.InitialClass != class {
				continue
			}
			if c.Workspace.Name != workspace || c.Floating {
				t.Fatalf("browser placement: workspace=%q floating=%v", c.Workspace.Name, c.Floating)
			}
			after, err := ActiveWorkspaceName(ctx)
			if err != nil || after != before {
				t.Fatalf("active workspace changed: %q -> %q, %v", before, after, err)
			}
			t.Log("browser mapped tiled on its named workspace without switching the active workspace")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("browser window did not appear")
}
