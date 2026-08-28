// Package ocevents subscribes to an opencode server's event stream.
//
// opencode exposes a Server-Sent Events endpoint at /event that pushes every
// session, permission and question event as it happens. Watching it is what
// lets canaveral react the instant an agent starts working, finishes, or
// blocks waiting for you — instead of polling every agent's HTTP API on a
// timer, which costs a subprocess-and-request storm per tick and still adds
// up to a second of latency.
package ocevents

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Event is one decoded message from the stream.
//
// opencode uses two envelope shapes: older events carry their payload under
// "properties", newer ".v2" ones under "data". Both are kept as raw JSON so
// callers can decode only the events they care about, and so an unfamiliar
// event never fails the whole stream.
type Event struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// Payload returns whichever envelope field this event actually used.
func (e Event) Payload() json.RawMessage {
	if len(e.Data) > 0 {
		return e.Data
	}
	return e.Properties
}

// streamClient deliberately has no timeout: this connection is meant to stay
// open indefinitely, and http.Client.Timeout covers the whole response body,
// not just the handshake, so any non-zero value would sever a healthy stream.
var streamClient = &http.Client{}

// Subscribe streams events to fn until ctx is cancelled or the connection
// drops. It blocks for the lifetime of the connection; use Watch for a
// long-lived watcher that reconnects on its own.
func Subscribe(ctx context.Context, baseURL string, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(baseURL, "/")+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient.Do(req)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("subscribe %s: status %d", baseURL, resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	// Tool output and file diffs ride along in some events, so the default
	// 64KB line limit is not enough; a single oversized line would otherwise
	// end the stream with an error.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for sc.Scan() {
		line := sc.Text()
		// SSE frames are "data: <payload>" followed by a blank line.
		// Comments (":...") and other fields are not used by opencode.
		payload, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			// An event we cannot parse is not worth tearing the stream down
			// for; skipping keeps the watcher alive across version skew.
			continue
		}
		fn(ev)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("event stream %s: %w", baseURL, err)
	}
	return nil
}

// Watch calls Subscribe in a loop, reconnecting with backoff whenever the
// stream ends, until ctx is cancelled.
//
// An agent's server restarts whenever its feature is reset, so dropping out
// on the first disconnect would silently stop reporting that feature for the
// rest of the session.
func Watch(ctx context.Context, baseURL string, fn func(Event)) error {
	backoff := 500 * time.Millisecond
	const maxBackoff = 10 * time.Second

	for {
		err := Subscribe(ctx, baseURL, fn)
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
		// A clean EOF still means the server went away; retry promptly.
		backoff = 500 * time.Millisecond
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
