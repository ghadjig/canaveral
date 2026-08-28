// Package probe implements readiness checks for workspace services.
package probe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
)

// DefaultTimeout applies when a probe does not set one.
const DefaultTimeout = 60 * time.Second

const interval = 300 * time.Millisecond

// Alive is called between attempts; returning false aborts the wait early
// (for example when the underlying unit has already died).
type Alive func() error

// Wait blocks until the readiness probe succeeds, the timeout expires, or the
// alive check fails. A probe with no configured check returns immediately.
func Wait(ctx context.Context, r manifest.Ready, dir, logPath string, alive Alive) error {
	kind := r.Kind()
	if kind == "" {
		return nil
	}

	timeout := r.Timeout.Or(DefaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if alive != nil {
			if err := alive(); err != nil {
				return err
			}
		}
		if err := check(ctx, r, dir, logPath); err == nil {
			return nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("readiness probe (%s) timed out after %s: %w", kind, timeout, lastErr)
		case <-ticker.C:
		}
	}
}

var client = &http.Client{
	Timeout: 3 * time.Second,
	Transport: &http.Transport{
		// Dev servers routinely use self-signed certs.
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives: true,
	},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func check(ctx context.Context, r manifest.Ready, dir, logPath string) error {
	switch r.Kind() {
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.HTTP, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		want := r.Status
		if want == 0 {
			want = 200
		}
		// Treat any non-5xx as ready when the caller asked for the default 200
		// but the app redirects (very common for Rails root paths).
		if resp.StatusCode == want || (want == 200 && resp.StatusCode < 500) {
			return nil
		}
		return fmt.Errorf("got status %d, want %d", resp.StatusCode, want)

	case "tcp":
		d := net.Dialer{Timeout: 2 * time.Second}
		conn, err := d.DialContext(ctx, "tcp", r.TCP)
		if err != nil {
			return err
		}
		return conn.Close()

	case "log":
		b, err := os.ReadFile(logPath)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), r.Log) {
			return nil
		}
		return fmt.Errorf("log does not yet contain %q", r.Log)

	case "cmd":
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", r.Cmd)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return nil
}
