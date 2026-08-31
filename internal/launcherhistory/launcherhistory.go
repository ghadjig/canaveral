// Package launcherhistory remembers lines typed into the popup launcher, so a
// command used once can be found again by typing the first few characters of
// it rather than retyping the whole thing.
//
// This is deliberately separate from registry, which remembers *projects*: a
// history entry is a whole line — project, command and arguments together —
// and answers "what did I type" rather than "where does this project live".
package launcherhistory

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

// FileName is the history file's basename inside the canaveral state directory.
const FileName = "launcher-history.json"

// maxEntries bounds how much is kept on disk. Generous, since the cost is one
// small JSON file, and Recent trims further for display.
const maxEntries = 50

// Entry is one previously typed line.
type Entry struct {
	Line     string    `json:"line"`
	LastUsed time.Time `json:"last_used"`
}

// path returns the history file's location.
func path() (string, error) {
	d, err := state.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, FileName), nil
}

// read returns exactly what is on disk, most-recent-first.
func read() ([]Entry, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read launcher history: %w", err)
	}
	var out []Entry
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse launcher history: %w", err)
	}
	return out, nil
}

// write atomically replaces the history file.
func write(entries []Entry) error {
	p, err := path()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Record adds a line to the history, or bumps it to the front if it is
// already there. Blank lines are ignored: they mean the launcher was closed
// or nothing was run.
//
// Failures are advisory, same as registry.Record — history is a convenience,
// and no run should fail because it could not be remembered.
func Record(line string) error {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	entries, err := read()
	if err != nil {
		return err
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.Line != line {
			kept = append(kept, e)
		}
	}
	kept = append(kept, Entry{Line: line, LastUsed: time.Now()})
	sort.Slice(kept, func(i, j int) bool { return kept[i].LastUsed.After(kept[j].LastUsed) })
	if len(kept) > maxEntries {
		kept = kept[:maxEntries]
	}
	return write(kept)
}

// Recent returns up to limit lines, most recently used first.
func Recent(limit int) ([]Entry, error) {
	entries, err := read()
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastUsed.After(entries[j].LastUsed) })
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}
