package hyprevents

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// startFakeSocket spins up a fake Hyprland event socket and returns its path.
// Every connection accepted is fed the given lines, then closed.
func startFakeSocket(t *testing.T, lines []string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".socket2.sock")

	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for _, line := range lines {
					if _, err := conn.Write([]byte(line + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return path
}

// withFakeInstance points HYPRLAND_INSTANCE_SIGNATURE / XDG_RUNTIME_DIR at a
// directory containing a fake socket, mirroring Hyprland's real layout of
// $XDG_RUNTIME_DIR/hypr/$SIG/.socket2.sock.
func withFakeInstance(t *testing.T, lines []string) {
	t.Helper()
	runtimeDir := t.TempDir()
	sig := "test-instance"
	hyprDir := filepath.Join(runtimeDir, "hypr", sig)
	if err := os.MkdirAll(hyprDir, 0o755); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("unix", filepath.Join(hyprDir, ".socket2.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for _, line := range lines {
					if _, err := conn.Write([]byte(line + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()

	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", sig)
}

func TestSocketPathUsesEnv(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "abc123")
	got, err := SocketPath()
	if err != nil {
		t.Fatalf("SocketPath: %v", err)
	}
	want := "/run/user/1000/hypr/abc123/.socket2.sock"
	if got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
}

func TestSocketPathMissingSignature(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	if _, err := SocketPath(); err == nil {
		t.Fatal("SocketPath succeeded without HYPRLAND_INSTANCE_SIGNATURE, want error")
	}
}

func TestSubscribeParsesEvents(t *testing.T) {
	withFakeInstance(t, []string{
		"workspace>>2",
		"createworkspacev2>>-1337,norules:small-fixes",
		"openwindow>>0x1,norules:small-fixes,canaveral-x,title with >> inside it",
	})

	var got []Event
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Subscribe(ctx, func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	want := []Event{
		{Type: "workspace", Data: "2"},
		{Type: "createworkspacev2", Data: "-1337,norules:small-fixes"},
		// Cut on the first ">>" only, so a payload that itself contains ">>"
		// (a window title can) is preserved intact rather than truncated.
		{Type: "openwindow", Data: "0x1,norules:small-fixes,canaveral-x,title with >> inside it"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSubscribeUnavailable(t *testing.T) {
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", "")
	if err := Subscribe(context.Background(), func(Event) {}); err == nil {
		t.Fatal("Subscribe succeeded with no Hyprland instance, want error")
	}
}

func TestSubscribeStopsOnContextCancel(t *testing.T) {
	// A socket that never sends anything and never closes: Subscribe must
	// still return promptly once ctx is cancelled, rather than blocking
	// forever on the read.
	withFakeInstance(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Subscribe(ctx, func(Event) {}) }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Subscribe returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Subscribe did not return within 3s of context cancellation")
	}
}

func TestWatchReconnectsAfterDisconnect(t *testing.T) {
	// Simulates Hyprland briefly restarting: the socket vanishes, then
	// reappears at the same path. Watch must pick back up without the
	// caller doing anything.
	runtimeDir := t.TempDir()
	sig := "test-instance"
	hyprDir := filepath.Join(runtimeDir, "hypr", sig)
	if err := os.MkdirAll(hyprDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("HYPRLAND_INSTANCE_SIGNATURE", sig)
	sockPath := filepath.Join(hyprDir, ".socket2.sock")

	serve := func(lines []string) net.Listener {
		l, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		go func() {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			for _, line := range lines {
				conn.Write([]byte(line + "\n"))
			}
		}()
		return l
	}

	l1 := serve([]string{"workspace>>1"})

	var (
		mu  sync.Mutex
		got []Event
	)
	record := func(e Event) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	}
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go Watch(ctx, record)

	time.Sleep(400 * time.Millisecond)
	l1.Close()
	os.Remove(sockPath)
	time.Sleep(300 * time.Millisecond)

	l2 := serve([]string{"workspace>>2"})
	defer l2.Close()

	deadline := time.After(4 * time.Second)
	for {
		if count() >= 2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("did not observe reconnect within deadline, got %d event(s)", count())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
