package wm

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
//
// The point is the window neither compositor can be told about in advance: a
// browser hands startup to an already-running instance of itself, so the
// process the compositor started is not the one that draws. Only a rule
// matched on the class can place such a window, and this is where that is
// checked against a real one.
func TestBrowserPlacementLive(t *testing.T) {
	if os.Getenv("CANAVERAL_TEST_LIVE_BROWSER") != "1" {
		t.Skip("set CANAVERAL_TEST_LIVE_BROWSER=1 to test browser placement")
	}
	ctx := context.Background()
	b := liveBackend(t, ctx)
	class := fmt.Sprintf("canaveral-placement-test-%d", os.Getpid())
	workspace := class
	if err := EnsureRules(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.ensurePlacement(ctx, class, workspace); err != nil {
		t.Fatal(err)
	}
	before, err := ActiveWorkspaceName(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately launched outside the compositor's own exec, so nothing
	// ties the rule to a process and only the class can place the window.
	// Forced onto XWayland, which is the harder of the two cases: the class
	// arrives as WM_CLASS rather than as an app_id, and both compositors
	// report it through a different field than they do a Wayland window's.
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

// liveBackend returns the backend for the compositor actually running, and
// skips the test when there is none — a live test has nothing to say on a
// headless machine.
func liveBackend(t *testing.T, ctx context.Context) backend {
	t.Helper()
	b, err := current()
	if err != nil {
		t.Skipf("no compositor: %v", err)
	}
	if err := b.available(ctx); err != nil {
		t.Skipf("%s is not answering: %v", b.name(), err)
	}
	return b
}
