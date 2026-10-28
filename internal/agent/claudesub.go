package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Claude Code used to write a subagent's entries into its parent's
// transcript, marked isSidechain, and scanSidechain still reads those. It now
// gives each subagent a file of its own instead:
//
//	<project>/<session>.jsonl                      the conversation
//	<project>/<session>/subagents/agent-<id>.jsonl one per subagent
//	<project>/<session>/subagents/agent-<id>.meta.json
//
// and the meta file says whether it has finished.

// subagentsDir is where session's subagent files live.
func subagentsDir(transcriptPath string) string {
	return filepath.Join(strings.TrimSuffix(transcriptPath, ".jsonl"), "subagents")
}

// claudeSubMeta is the part of a subagent's meta file canaveral reads.
type claudeSubMeta struct {
	Description string `json:"description"`
	Finished    bool   `json:"finished"`
}

// subUsage is one subagent file's token total, cached by size and
// modification time: a session's subagents run to megabytes of JSON between
// them, nearly all of it finished and unchanging, and Probe runs every time
// the conversation's own transcript is written to.
type subUsage struct {
	size    int64
	modTime time.Time
	tokens  Tokens
}

var (
	subUsageMu    sync.Mutex
	subUsageCache = map[string]subUsage{}
)

// scanSubagents adds the session's subagents to h: how many there have been,
// how many are running, and what they spent in tokens. Their dollar cost
// needs no adding, as the run's cost-state total already covers it, and
// these files carry none per message to add.
//
// A subagent is running when its meta file does not say it finished and the
// Claude Code process it belongs to is still alive; one that was killed with
// its parent never gets to say so.
func scanSubagents(transcriptPath string, live bool, h *Health) {
	dir := subagentsDir(transcriptPath)
	zero := 0
	h.SubRunning = &zero
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var running []Activity
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(dir, name)
		h.SubSessions++
		if u, ok := subagentUsage(path); ok {
			h.Tokens.add(u.tokens)
		}
		metaPath := strings.TrimSuffix(path, ".jsonl") + ".meta.json"
		var meta claudeSubMeta
		b, err := os.ReadFile(metaPath)
		if err != nil || json.Unmarshal(b, &meta) != nil {
			continue
		}
		if live && !meta.Finished {
			act := Activity{Tool: "Agent", Title: firstLine(meta.Description)}
			// Written when the subagent starts and again when it finishes,
			// so until then its time is the start.
			if info, err := os.Stat(metaPath); err == nil {
				act.Since = info.ModTime()
			}
			running = append(running, act)
		}
	}
	n := len(running)
	h.SubRunning = &n
	// The call that started a subagent reaches the transcript only once it
	// returns, so while it runs the turn shows nothing in flight at all.
	if h.Busy && h.Activity == nil && len(running) > 0 {
		h.Activity = &running[len(running)-1]
	}
}

// subagentUsage returns what one subagent file spent, from the cache when the
// file has not changed since it was last read.
func subagentUsage(path string) (subUsage, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return subUsage{}, false
	}
	subUsageMu.Lock()
	u, ok := subUsageCache[path]
	subUsageMu.Unlock()
	if ok && u.size == info.Size() && u.modTime.Equal(info.ModTime()) {
		return u, true
	}

	entries, err := readTranscript(path)
	if err != nil {
		return subUsage{}, false
	}
	var h Health
	s := claudeScan{counted: map[string]bool{}}
	for i := range entries {
		if entries[i].Type == "assistant" {
			s.addUsage(&h, &entries[i])
		}
	}
	u = subUsage{size: info.Size(), modTime: info.ModTime(), tokens: h.Tokens}
	subUsageMu.Lock()
	subUsageCache[path] = u
	subUsageMu.Unlock()
	return u, true
}
