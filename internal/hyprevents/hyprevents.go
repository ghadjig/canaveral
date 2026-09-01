// Package hyprevents subscribes to Hyprland's event socket.
//
// Hyprland exposes a Unix socket (commonly called socket2) that streams every
// compositor event as a line of "type>>data". This is what lets callers react
// instantly to workspace and window changes instead of polling hyprctl on a
// timer.
package hyprevents

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrUnavailable indicates the event socket could not be located or reached.
var ErrUnavailable = errors.New("hyprland event socket is not available")

// Event is one line from the event stream, split into its type and payload.
//
// The payload's structure depends on Type; callers that need individual
// fields should split Data on commas themselves (documented per event at
// https://wiki.hypr.land/IPC/).
type Event struct {
	Type string
	Data string
}

// SocketPath locates Hyprland's event socket for the current session.
func SocketPath() (string, error) {
	sig := os.Getenv("HYPRLAND_INSTANCE_SIGNATURE")
	if sig == "" {
		return "", fmt.Errorf("%w: HYPRLAND_INSTANCE_SIGNATURE is not set", ErrUnavailable)
	}
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(base, "hypr", sig, ".socket2.sock"), nil
}

// Subscribe connects to the event socket and sends every event to fn until
// ctx is cancelled or the connection is lost.
//
// Subscribe blocks for the lifetime of the connection; callers that want a
// resilient, long-lived watcher should use Watch instead, which reconnects
// automatically.
func Subscribe(ctx context.Context, fn func(Event)) error {
	path, err := SocketPath()
	if err != nil {
		return err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer conn.Close()

	// Closing the connection when ctx is done is what makes the blocking
	// Scanner read below actually return promptly on cancellation.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	sc := bufio.NewScanner(conn)
	// Hyprland's payload for events like windowtitlev2 can carry a full
	// window title; the default 64KiB scanner buffer is generous but not
	// unbounded, so grow it defensively rather than truncating silently.
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		typ, data, ok := strings.Cut(line, ">>")
		if !ok {
			continue
		}
		fn(Event{Type: typ, Data: data})
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("event stream: %w", err)
	}
	return nil
}

// Watch calls Subscribe in a loop, reconnecting with backoff if the socket
// disappears (for example across a Hyprland restart), until ctx is cancelled.
func Watch(ctx context.Context, fn func(Event)) error {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second

	for {
		err := Subscribe(ctx, fn)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
			continue
		}
		// A clean EOF (Hyprland exited/reloaded) is also worth retrying.
		backoff = 500 * time.Millisecond
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
