package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Never fetch an unbounded transcript for a status widget. Tool outputs can
// make a single conversation hundreds of MB even while nothing is happening.
const recentMessageLimit = 16
const maxRecentBytes = 4 * 1024 * 1024

type recentEntry struct {
	updated int64
	at      time.Time
	msgs    []sessionMessage
	err     error
}

var recentCache = struct {
	sync.Mutex
	entries map[string]recentEntry
}{entries: map[string]recentEntry{}}

func invalidateRecent(base string) {
	recentCache.Lock()
	defer recentCache.Unlock()
	for key, e := range recentCache.entries {
		if strings.HasPrefix(key, base+"/") {
			if e.err != nil {
				continue
			}
			e.at = time.Time{}
			recentCache.entries[key] = e
		}
	}
}

func recentMessages(ctx context.Context, base string, session sessionInfo) ([]sessionMessage, error) {
	key := base + "/" + session.ID
	recentCache.Lock()
	entry, ok := recentCache.entries[key]
	recentCache.Unlock()
	// An oversized preview or a failing server must not be retried for every
	// tool event. Lightweight status/permission probes continue meanwhile.
	if ok && entry.err != nil && time.Since(entry.at) < 30*time.Second {
		return nil, entry.err
	}
	if ok && entry.updated == session.Time.Updated && time.Since(entry.at) < 30*time.Second {
		turn := scanCompletedTurns(entry.msgs).cur
		if turn == nil || turn.Time.Completed != nil {
			return entry.msgs, nil
		}
	}
	u := base + "/session/" + url.PathEscape(session.ID) + "/message?limit=16"
	var msgs []sessionMessage
	if err := getBoundedJSON(ctx, u, &msgs); err != nil {
		recentCache.Lock()
		if len(recentCache.entries) < 128 {
			recentCache.entries[key] = recentEntry{updated: session.Time.Updated, at: time.Now(), err: err}
		}
		recentCache.Unlock()
		return nil, err
	}
	if len(msgs) > recentMessageLimit {
		msgs = msgs[len(msgs)-recentMessageLimit:]
	}
	// Keep only the projection used by status, not full prompts, reasoning,
	// or tool inputs such as apply_patch's complete file contents.
	for i := range msgs {
		var parts []messagePart
		for _, p := range msgs[i].Parts {
			switch p.Type {
			case "text":
				p.Text = previewText(p.Text)
			case "tool":
				if p.State.Title == "" {
					p.State.Title = describeInput(p.State.Input)
				}
				p.State.Title = previewText(p.State.Title)
				p.State.Input = nil
			default:
				continue
			}
			parts = append(parts, p)
		}
		msgs[i].Parts = parts
	}
	recentCache.Lock()
	// Bound server/session churn in a long-running watcher.
	if len(recentCache.entries) >= 128 {
		for k, v := range recentCache.entries {
			if time.Since(v.at) >= 30*time.Second {
				delete(recentCache.entries, k)
			}
		}
		if len(recentCache.entries) >= 128 {
			for k := range recentCache.entries {
				delete(recentCache.entries, k)
				break
			}
		}
	}
	recentCache.entries[key] = recentEntry{updated: session.Time.Updated, at: time.Now(), msgs: msgs}
	recentCache.Unlock()
	return msgs, nil
}

func getBoundedJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRecentBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxRecentBytes {
		return fmt.Errorf("GET %s: status response exceeds %d bytes", u, maxRecentBytes)
	}
	return json.Unmarshal(b, out)
}
