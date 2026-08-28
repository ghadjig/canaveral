package unit

import (
	"os"
	"strings"
	"testing"
)

func TestNameIsDeterministicAndSafe(t *testing.T) {
	got := Name("nor/ules", "agent", "a 1")
	if got != "canaveral-nor-ules-agent-a-1" {
		t.Errorf("Name = %q", got)
	}
	// Unit names must be stable across calls so status/down can find them.
	if Name("demo", "svc", "web") != Name("demo", "svc", "web") {
		t.Error("Name is not deterministic")
	}
}

func TestNameSeparatesKinds(t *testing.T) {
	if Name("d", "svc", "x") == Name("d", "agent", "x") {
		t.Error("service and agent units must not collide")
	}
}

func TestInheritEnvIncludesPath(t *testing.T) {
	t.Setenv("PATH", "/custom/bin")
	env := inheritEnv(nil)
	if env["PATH"] != "/custom/bin" {
		t.Errorf("PATH = %q, want inherited /custom/bin", env["PATH"])
	}
}

func TestInheritEnvExplicitWins(t *testing.T) {
	t.Setenv("PATH", "/inherited")
	env := inheritEnv(map[string]string{"PATH": "/override", "FOO": "bar"})
	if env["PATH"] != "/override" {
		t.Errorf("PATH = %q, want explicit override", env["PATH"])
	}
	if env["FOO"] != "bar" {
		t.Errorf("FOO = %q, want bar", env["FOO"])
	}
}

func TestInheritEnvSkipsUnsetAndEmpty(t *testing.T) {
	t.Setenv("LC_ALL", "")
	env := inheritEnv(nil)
	if _, ok := env["LC_ALL"]; ok {
		t.Error("empty variables should not be propagated")
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	got := sortedKeys(map[string]string{"b": "", "a": "", "c": ""})
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("sortedKeys = %v", got)
	}
}

func TestReadUintMissingFile(t *testing.T) {
	if got := readUint("/nonexistent/canaveral/memory.current"); got != 0 {
		t.Errorf("readUint = %d, want 0", got)
	}
}

func TestReadUintParses(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "mem")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("1585152\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got := readUint(f.Name()); got != 1585152 {
		t.Errorf("readUint = %d, want 1585152", got)
	}
}

func TestStatusRunning(t *testing.T) {
	cases := map[string]bool{
		"active":     true,
		"activating": true,
		"inactive":   false,
		"failed":     false,
		"":           false,
	}
	for state, want := range cases {
		if got := (Status{ActiveState: state}).Running(); got != want {
			t.Errorf("Running(%q) = %v, want %v", state, got, want)
		}
	}
}
