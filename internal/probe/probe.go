// Package probe implements readiness checks for workspace services.
package probe

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/manifest"
)

// DefaultTimeout applies when a probe does not set one.
const DefaultTimeout = 60 * time.Second

// Attempts back off from interval up to maxInterval, growing by factor.
//
// A fixed interval assumes the probe is cheap, which holds only while the
// target is a local socket. It is not always: a service that tunnels its port
// — devspace holding open a kubectl port forward into a pod, say — pays a new
// TCP connection *and* a pair of multiplexed streams for every attempt,
// because keep-alives are off below. At a flat 300ms that is 3.3 connections
// a second sustained for the whole of a boot, and a slow-starting app with a
// generous ready.timeout can be polled thousands of times. Measured against a
// cold Rails start behind devspace, it drove 115 connections in 35 seconds
// and left the forwarder unable to open further streams ("error creating
// error stream ... Timeout occurred"), which devspace answers by restarting
// the forward — taking the port down rather than bringing it up.
//
// Backing off keeps what the tight interval was for: a service that is
// already warm still answers on the first or second attempt, well inside a
// second. Only the slow case is throttled, and that is the only case where
// the polling ever adds up to anything.
const (
	interval    = 300 * time.Millisecond
	maxInterval = 5 * time.Second
	factor      = 1.5
)

// Alive is called between attempts; returning false aborts the wait early
// (for example when the underlying unit has already died).
type Alive func() error

// ErrTimeout reports that a probe never succeeded before its deadline. Callers
// match on it to attach diagnostics this package cannot reach, chiefly the
// service's own log.
var ErrTimeout = errors.New("readiness probe timed out")

// Wait blocks until the readiness probe succeeds, the timeout expires, or the
// alive check fails. A probe with no configured check returns immediately.
func Wait(ctx context.Context, r manifest.Ready, dir, logPath string, alive Alive) error {
	kind := r.Kind()
	if kind == "" {
		return nil
	}

	// Built once, not per attempt: a log_match probe would otherwise recompile
	// its pattern on every one of the hundreds a long wait makes, and a
	// pattern that cannot compile would be reported as a readiness timeout
	// minutes later instead of immediately. The manifest rejects a bad one
	// before anything starts, so reaching that error here means a caller
	// built a Ready by hand.
	attempt, err := checker(r, dir, logPath)
	if err != nil {
		return err
	}

	timeout := r.Timeout.Or(DefaultTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	delay := interval

	for {
		if alive != nil {
			if err := alive(); err != nil {
				return err
			}
		}
		if err := attempt(ctx); err == nil {
			return nil
		} else if lastErr == nil || ctx.Err() == nil {
			// Past the deadline, check fails with the context's own error,
			// which says nothing about why the service was not ready. The
			// attempt before it — "got status 500", "connection refused" — is
			// the one worth reporting, so do not let it be overwritten.
			lastErr = err
		}

		// Timed from here rather than from a ticker started up front, so the
		// gap is between attempts. check blocks for up to its own timeout
		// when a service accepts the connection and then stalls, and a
		// ticker would have already fired by the time it returned — turning
		// the backoff into no wait at all in exactly the case it exists for.
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("%w (%s) after %s: %w", ErrTimeout, kind, timeout, lastErr)
		case <-t.C:
		}
		delay = next(delay)
	}
}

// next grows a retry delay towards maxInterval.
func next(d time.Duration) time.Duration {
	return min(time.Duration(float64(d)*factor), maxInterval)
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

// checker builds the single-attempt check for a probe.
//
// A constructor rather than a function called per attempt, so one-off work —
// compiling a log_match pattern — happens once and its failure is reported
// before the first attempt rather than as a timeout after the last.
func checker(r manifest.Ready, dir, logPath string) (func(context.Context) error, error) {
	switch r.Kind() {
	case "http":
		return func(ctx context.Context) error { return checkHTTP(ctx, r) }, nil

	case "tcp":
		return func(ctx context.Context) error { return checkTCP(ctx, r) }, nil

	case "log":
		return func(context.Context) error {
			b, err := os.ReadFile(logPath)
			if err != nil {
				return err
			}
			if strings.Contains(string(b), r.Log) {
				return nil
			}
			return fmt.Errorf("log does not yet contain %q", r.Log)
		}, nil

	case "log_match":
		re, err := regexp.Compile(r.LogMatch)
		if err != nil {
			return nil, fmt.Errorf("ready.log_match: %w", err)
		}
		return func(context.Context) error {
			b, err := os.ReadFile(logPath)
			if err != nil {
				return err
			}
			// Against the whole file rather than line by line: a marker worth
			// waiting for is occasionally spread over more than one line, and
			// a caller that wants line anchors can ask for them with (?m).
			if re.Match(b) {
				return nil
			}
			return fmt.Errorf("log does not yet match %q", r.LogMatch)
		}, nil

	case "cmd":
		return func(ctx context.Context) error {
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", r.Cmd)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
			}
			return nil
		}, nil
	}
	return func(context.Context) error { return nil }, nil
}

func checkHTTP(ctx context.Context, r manifest.Ready) error {
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
}

func checkTCP(ctx context.Context, r manifest.Ready) error {
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", r.TCP)
	if err != nil {
		return err
	}
	return conn.Close()
}
