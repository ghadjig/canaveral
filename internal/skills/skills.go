// Package skills manages namespace-scoped SKILL.md files shared across every
// feature under the same canaveral namespace.
//
// `canaveral onboarding/step1` and `canaveral onboarding/step2` share one
// skill (see internal/feature.Namespace for exactly how sharing is scoped),
// so an agent starting the second feature isn't starting from zero on
// whatever the first one already worked out — decisions, gotchas, where
// things live.
//
// A namespace's skill is canaveral-managed state, like its worktrees and
// browser profiles, not part of the git repo: it lives under
// ~/.local/state/canaveral/skills/<project>/<namespace>/SKILL.md, and every
// feature under that namespace gets it symlinked into its own worktree at
// .claude/skills/<flattened-namespace>/. That location and file format
// (SKILL.md with name/description frontmatter) is a convention opencode and
// Claude Code both already read natively — no cooperation from either tool
// is required for this to work, and it works the same regardless of which
// one (or which future one) you're using.
//
// Because it lives outside any single feature's worktree, it survives that
// feature being removed and recreated, and because it's a symlink rather
// than a copy, every feature under the namespace that's open at once sees
// edits to it immediately, not just the one that happened to write them.
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/bandito/canaveral/internal/state"
)

var flattenRe = regexp.MustCompile(`/+`)

// FlattenName turns a "/"-separated namespace into the flat, hyphenated form
// opencode and Claude Code require for a skill directory name (lowercase
// alphanumeric, single-hyphen separated — no slashes). feature.Slug already
// guarantees every segment is in that alphabet, so this only has to replace
// the separator.
func FlattenName(namespace string) string {
	return flattenRe.ReplaceAllString(namespace, "-")
}

// Dir returns the canaveral-managed directory holding a namespace's shared
// skill, creating it — and a starter SKILL.md, if one doesn't exist yet —
// on first use.
func Dir(project, namespace string) (string, error) {
	base, err := state.Dir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "skills", project, namespace)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	if _, err := os.Stat(skillPath); os.IsNotExist(err) {
		if err := os.WriteFile(skillPath, []byte(scaffold(namespace)), 0o644); err != nil {
			return "", fmt.Errorf("write %s: %w", skillPath, err)
		}
	} else if err != nil {
		return "", err
	}
	return dir, nil
}

func scaffold(namespace string) string {
	return fmt.Sprintf(`---
name: %s
description: Shared notes for the %q canaveral namespace, automatically visible to every feature under it.
---

Add anything here worth remembering across features in this namespace —
decisions, gotchas, where things live in the codebase — so the next one
doesn't start from zero.
`, FlattenName(namespace), namespace)
}

// Link ensures <worktree>/.claude/skills/<flattened-namespace>/ is a symlink
// to the namespace's shared skill directory, so opencode's and Claude
// Code's own skill discovery picks it up with no other configuration.
//
// rel is the worktree-relative link path — suitable for state.Feature's
// Provisioned list, so the untracked symlink never makes `rm` think there's
// uncommitted work to lose. created reports whether a new link was made:
// false if one already existed, whether because an earlier reconcile made it
// or because the repo itself already tracks real content at that path — in
// the latter case Link leaves it alone rather than risk deleting something
// that isn't canaveral's.
func Link(worktree, project, namespace string) (rel string, created bool, err error) {
	flat := FlattenName(namespace)
	rel = filepath.Join(".claude", "skills", flat)
	target := filepath.Join(worktree, rel)

	real, err := Dir(project, namespace)
	if err != nil {
		return rel, false, err
	}

	if fi, lerr := os.Lstat(target); lerr == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if existing, rerr := os.Readlink(target); rerr == nil && existing == real {
				return rel, false, nil // already correctly linked
			}
		}
		return rel, false, nil
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return rel, false, err
	}
	if err := os.Symlink(real, target); err != nil {
		return rel, false, fmt.Errorf("link skill %q: %w", namespace, err)
	}
	return rel, true, nil
}

// SessionRecord is the last known opencode session for one agent in a
// namespace, recorded so a later sibling feature can fork it — even after
// the feature that had it is long gone.
type SessionRecord struct {
	Feature   string `json:"feature"`
	SessionID string `json:"session_id"`
	// Worktree is where the session was created. opencode fixes a
	// session's directory at creation — a fork inherits it, and --dir does
	// not override it — so a session is only safe to resume while this
	// path still exists. Empty on records written before this was tracked,
	// which are therefore treated as unusable.
	Worktree  string    `json:"worktree,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// sessionsPath lives alongside SKILL.md in the same namespace directory —
// it is the same durable, canaveral-managed, per-namespace state, just a
// second file rather than a second storage location.
func sessionsPath(project, namespace string) (string, error) {
	dir, err := Dir(project, namespace)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sessions.json"), nil
}

// RecordSession persists the newest known session for an agent in a
// namespace, if it is more recent than whatever was already recorded.
//
// Called opportunistically (best-effort, on `rm` and `status`) rather than
// continuously, so this is intentionally a "latest wins" upsert, not an
// append-only log.
func RecordSession(project, namespace, agentName string, rec SessionRecord) error {
	if rec.SessionID == "" {
		return nil
	}
	p, err := sessionsPath(project, namespace)
	if err != nil {
		return err
	}
	all, err := readSessions(p)
	if err != nil {
		return err
	}
	if existing, ok := all[agentName]; ok && !rec.UpdatedAt.After(existing.UpdatedAt) {
		return nil
	}
	if all == nil {
		all = map[string]SessionRecord{}
	}
	all[agentName] = rec
	return writeSessions(p, all)
}

// LatestSession returns the most recently recorded session for an agent in a
// namespace, if any.
func LatestSession(project, namespace, agentName string) (SessionRecord, bool, error) {
	p, err := sessionsPath(project, namespace)
	if err != nil {
		return SessionRecord{}, false, err
	}
	all, err := readSessions(p)
	if err != nil {
		return SessionRecord{}, false, err
	}
	rec, ok := all[agentName]
	return rec, ok, nil
}

func readSessions(p string) (map[string]SessionRecord, error) {
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all map[string]SessionRecord
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return all, nil
}

func writeSessions(p string, all map[string]SessionRecord) error {
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
