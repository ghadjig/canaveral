package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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

func recentMessages(ctx context.Context, c Conn, session sessionInfo) ([]sessionMessage, error) {
	key := c.URL + "/" + session.ID
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
	// Newest first, because that is the only order in which a limit keeps
	// the recent end of a long conversation rather than its opening.
	u := fmt.Sprintf("/api/session/%s/message?limit=%d&order=desc", url.PathEscape(session.ID), recentMessageLimit)
	var page struct {
		Data []sessionMessage `json:"data"`
	}
	if err := getBoundedJSON(ctx, c, u, &page); err != nil {
		recentCache.Lock()
		if len(recentCache.entries) < 128 {
			recentCache.entries[key] = recentEntry{updated: session.Time.Updated, at: time.Now(), err: err}
		}
		recentCache.Unlock()
		return nil, err
	}
	msgs := page.Data
	if len(msgs) > recentMessageLimit {
		msgs = msgs[:recentMessageLimit]
	}
	// Everything downstream reads oldest-first, as a conversation does.
	slices.Reverse(msgs)
	projectMessages(msgs)
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

// projectMessages reduces msgs, in place, to what status needs: not full
// prompts, reasoning, or tool inputs such as a patch's complete file
// contents, which would otherwise sit in the cache for every agent.
//
// A tool's input is read here, once, for the two things anything later
// wants from it — a title for the call, and a todowrite call's list — and
// then dropped.
func projectMessages(msgs []sessionMessage) {
	for i := range msgs {
		msgs[i].Text = previewText(msgs[i].Text)
		var parts []messagePart
		for _, p := range msgs[i].Content {
			switch p.Type {
			case "text":
				p.Text = previewText(p.Text)
			case "tool":
				input := toolInput(p.State.Input)
				p.Title = previewText(describeInput(input))
				if strings.EqualFold(p.Name, "todowrite") {
					p.Todos = todowriteItems(p.State.Input)
				}
				p.State.Input = nil
			default:
				continue
			}
			parts = append(parts, p)
		}
		msgs[i].Content = parts
	}
}

// toolInput decodes a tool call's arguments, nil when they are not an
// object yet — while the model is still streaming them they arrive as the
// raw partial string.
func toolInput(raw json.RawMessage) map[string]any {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// todowriteItems reads the list out of a todowrite call's arguments.
func todowriteItems(raw json.RawMessage) []todoItem {
	var in struct {
		Todos []todoItem `json:"todos"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	return in.Todos
}

func getBoundedJSON(ctx context.Context, c Conn, path string, out any) error {
	req, err := newRequest(ctx, c, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRecentBytes+1))
	if err != nil {
		return err
	}
	if len(b) > maxRecentBytes {
		return fmt.Errorf("GET %s: status response exceeds %d bytes", path, maxRecentBytes)
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}
