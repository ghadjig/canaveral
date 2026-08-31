package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/registry"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it. The cli package prints straight to os.Stdout
// rather than through an injectable writer, so this is the only way to
// assert on what a reporter or a printer actually produced.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestPrintProjectsListNames(t *testing.T) {
	out := captureStdout(t, func() {
		err := printProjectsList([]registry.Project{{Name: "a"}, {Name: "b"}}, true, false)
		if err != nil {
			t.Fatal(err)
		}
	})
	if out != "a\nb\n" {
		t.Errorf("out = %q, want %q", out, "a\nb\n")
	}
}

func TestPrintProjectsListJSONHandlesNilAsEmptyArray(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printProjectsList(nil, false, true); err != nil {
			t.Fatal(err)
		}
	})
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("out = %q, want an empty JSON array", out)
	}
}

func TestPrintProjectsListEmptyTablePointsAtScan(t *testing.T) {
	out := captureStdout(t, func() {
		if err := printProjectsList(nil, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "--scan") {
		t.Errorf("out = %q, want a hint about --scan", out)
	}
}

func TestPrintProjectsListTableFlagsAMissingRoot(t *testing.T) {
	out := captureStdout(t, func() {
		projects := []registry.Project{{Name: "gone", Root: "/nonexistent/dir/xyz"}}
		if err := printProjectsList(projects, false, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "gone") || !strings.Contains(out, "(missing)") {
		t.Errorf("out = %q, want the missing project flagged", out)
	}
}

func TestRunProjectsForgetReportsNotFoundForADerivedEntry(t *testing.T) {
	// Mirrors registry.TestForgetADerivedEntryReportsItIsNotInTheFile: a
	// name that was never recorded must warn, not error.
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out := captureStdout(t, func() {
		if err := runProjectsForget(reporter{}, "never-recorded"); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "never-recorded") {
		t.Errorf("out = %q, want the project name in the warning", out)
	}
}

func TestRunProjectsPruneNoopWhenNothingIsDead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out := captureStdout(t, func() {
		if err := runProjectsPrune(reporter{}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "nothing to prune") {
		t.Errorf("out = %q, want it to say there was nothing to prune", out)
	}
}

func TestRunProjectsScanReportsNoneFoundUnderAnEmptyDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	out := captureStdout(t, func() {
		if err := runProjectsScan(reporter{}, dir); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no project checkouts found") {
		t.Errorf("out = %q, want a no-checkouts-found message", out)
	}
}
