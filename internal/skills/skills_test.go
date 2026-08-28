package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFlattenName(t *testing.T) {
	cases := map[string]string{
		"onboarding":       "onboarding",
		"onboarding/step1": "onboarding-step1",
		"a/b/c":            "a-b-c",
		"epic/sub-epic":    "epic-sub-epic",
	}
	for in, want := range cases {
		if got := FlattenName(in); got != want {
			t.Errorf("FlattenName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDirCreatesScaffold(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir, err := Dir("norules", "onboarding")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	content := string(b)
	if !strings.Contains(content, "name: onboarding") {
		t.Errorf("scaffold missing name frontmatter: %s", content)
	}
	if !strings.Contains(content, "description:") {
		t.Errorf("scaffold missing description frontmatter: %s", content)
	}
}

func TestDirIsIdempotentAndPreservesEdits(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	dir, err := Dir("norules", "onboarding")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	custom := "---\nname: onboarding\ndescription: edited\n---\n\nmy own notes\n"
	if err := os.WriteFile(skillPath, []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	// Calling Dir again (as a sibling feature's reconcile would) must not
	// clobber content the user or an agent already wrote.
	dir2, err := Dir("norules", "onboarding")
	if err != nil {
		t.Fatalf("Dir (2nd call): %v", err)
	}
	if dir2 != dir {
		t.Fatalf("Dir returned different paths: %q vs %q", dir, dir2)
	}
	b, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != custom {
		t.Errorf("SKILL.md was overwritten: got %q, want %q", string(b), custom)
	}
}

func TestDirScopedByNamespaceAndProject(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	a, err := Dir("norules", "onboarding")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Dir("norules", "onboarding/sub")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Dir("other-project", "onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a == c || b == c {
		t.Errorf("expected three distinct directories, got %q %q %q", a, b, c)
	}
}

func TestLinkCreatesSymlinkToSharedDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt1 := t.TempDir()
	wt2 := t.TempDir()

	rel, created, err := Link(wt1, "norules", "onboarding")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if !created {
		t.Error("expected created=true for a fresh link")
	}
	if want := filepath.Join(".claude", "skills", "onboarding"); rel != want {
		t.Errorf("rel = %q, want %q", rel, want)
	}

	// A sibling feature's worktree links to the exact same real directory.
	rel2, created2, err := Link(wt2, "norules", "onboarding")
	if err != nil {
		t.Fatalf("Link (sibling): %v", err)
	}
	if !created2 {
		t.Error("expected created=true for the sibling's own fresh link")
	}
	if rel2 != rel {
		t.Errorf("sibling rel = %q, want %q", rel2, rel)
	}

	target1, err := os.Readlink(filepath.Join(wt1, rel))
	if err != nil {
		t.Fatal(err)
	}
	target2, err := os.Readlink(filepath.Join(wt2, rel2))
	if err != nil {
		t.Fatal(err)
	}
	if target1 != target2 {
		t.Errorf("siblings link to different targets: %q vs %q", target1, target2)
	}

	// Writing through one worktree's link must be visible via the other's.
	if err := os.WriteFile(filepath.Join(target1, "SKILL.md"), []byte("---\nname: onboarding\ndescription: d\n---\nshared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(wt2, rel2, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "---\nname: onboarding\ndescription: d\n---\nshared\n" {
		t.Errorf("sibling did not see the shared edit: %q", string(b))
	}
}

func TestLinkIsIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt := t.TempDir()

	if _, created, err := Link(wt, "norules", "onboarding"); err != nil || !created {
		t.Fatalf("first Link: created=%v err=%v", created, err)
	}
	_, created, err := Link(wt, "norules", "onboarding")
	if err != nil {
		t.Fatalf("second Link: %v", err)
	}
	if created {
		t.Error("second Link should report created=false, link already correct")
	}
}

func TestLinkLeavesRealContentAlone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt := t.TempDir()

	real := filepath.Join(wt, ".claude", "skills", "onboarding")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "SKILL.md"), []byte("tracked by the repo"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, created, err := Link(wt, "norules", "onboarding")
	if err != nil {
		t.Fatalf("Link: %v", err)
	}
	if created {
		t.Error("Link should not report created=true over real, non-symlink content")
	}
	b, err := os.ReadFile(filepath.Join(real, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "tracked by the repo" {
		t.Errorf("real content was overwritten: %q", string(b))
	}
}

func TestSessionRecordRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	if _, ok, err := LatestSession("norules", "onboarding", "main"); err != nil || ok {
		t.Fatalf("expected no session yet, ok=%v err=%v", ok, err)
	}

	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	err := RecordSession("norules", "onboarding", "main", SessionRecord{
		Feature: "onboarding/step1", SessionID: "sess-1", UpdatedAt: t0,
	})
	if err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	rec, ok, err := LatestSession("norules", "onboarding", "main")
	if err != nil || !ok {
		t.Fatalf("LatestSession: ok=%v err=%v", ok, err)
	}
	if rec.SessionID != "sess-1" || rec.Feature != "onboarding/step1" {
		t.Errorf("rec = %+v", rec)
	}
}

func TestSessionRecordLatestWins(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	older := time.Now().Add(-time.Hour).Truncate(time.Second)
	newer := time.Now().Truncate(time.Second)

	if err := RecordSession("norules", "onboarding", "main", SessionRecord{
		Feature: "onboarding/step1", SessionID: "sess-1", UpdatedAt: newer,
	}); err != nil {
		t.Fatal(err)
	}
	// An older record for the same agent must not overwrite the newer one.
	if err := RecordSession("norules", "onboarding", "main", SessionRecord{
		Feature: "onboarding/step2", SessionID: "sess-2", UpdatedAt: older,
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := LatestSession("norules", "onboarding", "main")
	if err != nil || !ok {
		t.Fatalf("LatestSession: ok=%v err=%v", ok, err)
	}
	if rec.SessionID != "sess-1" {
		t.Errorf("expected the newer record sess-1 to win, got %+v", rec)
	}

	// A genuinely newer record does overwrite.
	evenNewer := newer.Add(time.Minute)
	if err := RecordSession("norules", "onboarding", "main", SessionRecord{
		Feature: "onboarding/step2", SessionID: "sess-3", UpdatedAt: evenNewer,
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err = LatestSession("norules", "onboarding", "main")
	if err != nil || !ok {
		t.Fatalf("LatestSession: ok=%v err=%v", ok, err)
	}
	if rec.SessionID != "sess-3" {
		t.Errorf("expected sess-3 to win, got %+v", rec)
	}
}

func TestSessionRecordScopedByAgentName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	now := time.Now().Truncate(time.Second)
	if err := RecordSession("norules", "onboarding", "main", SessionRecord{
		Feature: "onboarding/step1", SessionID: "sess-main", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordSession("norules", "onboarding", "reviewer", SessionRecord{
		Feature: "onboarding/step1", SessionID: "sess-reviewer", UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	main, ok, err := LatestSession("norules", "onboarding", "main")
	if err != nil || !ok || main.SessionID != "sess-main" {
		t.Errorf("main = %+v ok=%v err=%v", main, ok, err)
	}
	reviewer, ok, err := LatestSession("norules", "onboarding", "reviewer")
	if err != nil || !ok || reviewer.SessionID != "sess-reviewer" {
		t.Errorf("reviewer = %+v ok=%v err=%v", reviewer, ok, err)
	}
}
