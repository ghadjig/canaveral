package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
)

func TestWaitNoProbeReturnsImmediately(t *testing.T) {
	if err := Wait(context.Background(), manifest.Ready{}, "", "", nil); err != nil {
		t.Errorf("empty probe: %v", err)
	}
}

func TestWaitHTTPBecomesReady(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fail twice, then succeed, to exercise the retry loop.
		if hits.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := manifest.Ready{HTTP: srv.URL, Status: 200}
	r.Timeout.Duration = 5 * time.Second
	if err := Wait(context.Background(), r, "", "", nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if hits.Load() < 3 {
		t.Errorf("hits = %d, want at least 3", hits.Load())
	}
}

func TestWaitHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := manifest.Ready{HTTP: srv.URL, Status: 200}
	r.Timeout.Duration = 600 * time.Millisecond
	err := Wait(context.Background(), r, "", "", nil)
	if err == nil {
		t.Fatal("Wait succeeded, want timeout")
	}
}

func TestWaitAbortsWhenProcessDies(t *testing.T) {
	// A dead unit must abort the wait immediately rather than burning the
	// full timeout.
	sentinel := errors.New("process exited")
	r := manifest.Ready{TCP: "127.0.0.1:1"}
	r.Timeout.Duration = 30 * time.Second

	start := time.Now()
	err := Wait(context.Background(), r, "", "", func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("took %v, should abort promptly", elapsed)
	}
}

func TestWaitLogProbe(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "svc.log")
	if err := os.WriteFile(logPath, []byte("booting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = os.WriteFile(logPath, []byte("booting\nListening on 3000\n"), 0o644)
	}()

	r := manifest.Ready{Log: "Listening on"}
	r.Timeout.Duration = 5 * time.Second
	if err := Wait(context.Background(), r, dir, logPath, nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestWaitCmdProbe(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ready")
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(marker, nil, 0o644)
	}()

	r := manifest.Ready{Cmd: "test -f ready"}
	r.Timeout.Duration = 5 * time.Second
	if err := Wait(context.Background(), r, dir, "", nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestWaitHTTPAcceptsRedirect(t *testing.T) {
	// Rails root paths commonly 302; with the default expectation of 200 a
	// non-5xx response should still count as ready.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()

	r := manifest.Ready{HTTP: srv.URL, Status: 200}
	r.Timeout.Duration = 2 * time.Second
	if err := Wait(context.Background(), r, "", "", nil); err != nil {
		t.Errorf("redirect should be treated as ready: %v", err)
	}
}

func TestWaitTCPBecomesReady(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	r := manifest.Ready{TCP: srv.Listener.Addr().String()}
	r.Timeout.Duration = 3 * time.Second
	if err := Wait(context.Background(), r, "", "", nil); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
