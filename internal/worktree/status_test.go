package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newStatusRepo creates a repo with a "main" branch holding one commit.
func newStatusRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	return dir
}

func commit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("add", "-A")
	run("commit", "-qm", msg)
}

func checkout(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "checkout"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout %v: %v\n%s", args, err, out)
	}
}

func TestStatusUpToDate(t *testing.T) {
	dir := newStatusRepo(t)
	checkout(t, dir, "-q", "-b", "feature")

	s, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Base != "main" {
		t.Errorf("Base = %q, want main", s.Base)
	}
	if s.Ahead != 0 || s.Behind != 0 {
		t.Errorf("Ahead=%d Behind=%d, want 0/0", s.Ahead, s.Behind)
	}
	if s.Label() != "up to date" {
		t.Errorf("Label = %q, want %q", s.Label(), "up to date")
	}
}

func TestStatusAhead(t *testing.T) {
	dir := newStatusRepo(t)
	checkout(t, dir, "-q", "-b", "feature")
	commit(t, dir, "g.txt", "b\nc\n", "add g")

	s, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Ahead != 1 || s.Behind != 0 {
		t.Errorf("Ahead=%d Behind=%d, want 1/0", s.Ahead, s.Behind)
	}
	if s.FilesChanged != 1 || s.Insertions != 2 {
		t.Errorf("FilesChanged=%d Insertions=%d, want 1/2", s.FilesChanged, s.Insertions)
	}
	if want := "ahead 1"; s.Label() != want {
		t.Errorf("Label = %q, want %q", s.Label(), want)
	}
}

func TestStatusBehind(t *testing.T) {
	dir := newStatusRepo(t)
	checkout(t, dir, "-q", "-b", "feature")
	checkout(t, dir, "-q", "main")
	commit(t, dir, "h.txt", "x\n", "advance main")
	checkout(t, dir, "-q", "feature")

	s, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Ahead != 0 || s.Behind != 1 {
		t.Errorf("Ahead=%d Behind=%d, want 0/1", s.Ahead, s.Behind)
	}
	if want := "behind 1"; s.Label() != want {
		t.Errorf("Label = %q, want %q", s.Label(), want)
	}
}

func TestStatusDiverged(t *testing.T) {
	dir := newStatusRepo(t)
	checkout(t, dir, "-q", "-b", "feature")
	commit(t, dir, "feat.txt", "f\n", "feature work")
	checkout(t, dir, "-q", "main")
	commit(t, dir, "main.txt", "m\n", "main work")
	checkout(t, dir, "-q", "feature")

	s, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Ahead != 1 || s.Behind != 1 {
		t.Errorf("Ahead=%d Behind=%d, want 1/1", s.Ahead, s.Behind)
	}
	if want := "diverged (ahead 1, behind 1)"; s.Label() != want {
		t.Errorf("Label = %q, want %q", s.Label(), want)
	}
}

func TestDefaultBranchFallsBackToMaster(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "master")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "init")
	checkout(t, dir, "-q", "-b", "feature")

	s, err := Status(context.Background(), dir)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if s.Base != "master" {
		t.Errorf("Base = %q, want master", s.Base)
	}
}

func TestDefaultBranchNoneFound(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "neither-main-nor-master")
	run("config", "user.email", "t@t")
	run("config", "user.name", "t")
	run("commit", "-q", "--allow-empty", "-m", "init")

	if _, err := Status(context.Background(), dir); err == nil {
		t.Error("expected an error when neither main nor master exists")
	}
}

func TestParseShortstat(t *testing.T) {
	cases := []struct {
		in              string
		files, ins, del int
	}{
		{"1 file changed, 2 insertions(+)", 1, 2, 0},
		{"1 file changed, 2 deletions(-)", 1, 0, 2},
		{"3 files changed, 10 insertions(+), 4 deletions(-)", 3, 10, 4},
		{"", 0, 0, 0},
	}
	for _, c := range cases {
		files, ins, del := parseShortstat(c.in)
		if files != c.files || ins != c.ins || del != c.del {
			t.Errorf("parseShortstat(%q) = %d,%d,%d want %d,%d,%d", c.in, files, ins, del, c.files, c.ins, c.del)
		}
	}
}

func TestBranchStatusLabel(t *testing.T) {
	cases := []struct {
		s    BranchStatus
		want string
	}{
		{BranchStatus{}, "up to date"},
		{BranchStatus{Ahead: 3}, "ahead 3"},
		{BranchStatus{Behind: 5}, "behind 5"},
		{BranchStatus{Ahead: 2, Behind: 4}, "diverged (ahead 2, behind 4)"},
	}
	for _, c := range cases {
		if got := c.s.Label(); got != c.want {
			t.Errorf("Label() = %q, want %q", got, c.want)
		}
	}
}
