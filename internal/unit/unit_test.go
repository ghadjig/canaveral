package unit

import (
	"context"
	"os"
	"os/exec"
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

func TestIsFeatureUnitExcludesCanaveralsOwn(t *testing.T) {
	cases := map[string]bool{
		"canaveral-norules-startus-svc-web":    true,
		"canaveral-norules-startus-agent-main": true,
		"canaveral-hyprwatch":                  false,
		"unrelated-svc-web":                    false,
		"canaveral-norules-startus":            false,
	}
	for in, want := range cases {
		if got := IsFeatureUnit(in); got != want {
			t.Errorf("IsFeatureUnit(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestFeaturePrefixesDoNotClaimSiblings(t *testing.T) {
	// The whole reason prefixes are per-kind: "login" must not swallow
	// "login-fixes", or removing one would reap the other's units.
	live := []string{
		"canaveral-p-login-svc-web",
		"canaveral-p-login-fixes-svc-web",
	}
	got := Orphans(live, []string{"p-login"})
	if len(got) != 1 || got[0] != "canaveral-p-login-fixes-svc-web" {
		t.Errorf("Orphans = %v, want only login-fixes", got)
	}
}

func TestOrphansSkipsClaimedAndNonFeatureUnits(t *testing.T) {
	live := []string{
		"canaveral-hyprwatch",
		"canaveral-demo-alive-svc-web",
		"canaveral-demo-alive-agent-main",
		"canaveral-demo-dead-svc-web",
	}
	got := Orphans(live, []string{"demo-alive"})
	if len(got) != 1 || got[0] != "canaveral-demo-dead-svc-web" {
		t.Errorf("Orphans = %v, want only the dead feature's unit", got)
	}
}

func TestOrphansWithNoKnownFeaturesReapsEveryFeatureUnit(t *testing.T) {
	live := []string{"canaveral-hyprwatch", "canaveral-p-f-svc-web"}
	got := Orphans(live, nil)
	if len(got) != 1 || got[0] != "canaveral-p-f-svc-web" {
		t.Errorf("Orphans = %v, want the feature unit only", got)
	}
}

func TestStopSurvivesACancelledContext(t *testing.T) {
	// Teardown runs precisely when the user has hit Ctrl-C, so a cancelled
	// context must not turn it into a no-op. Proven via the same detach
	// helper Stop uses: the command has to actually run.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tctx, tcancel := teardown(ctx)
	defer tcancel()
	if err := exec.CommandContext(tctx, "true").Run(); err != nil {
		t.Fatalf("teardown context did not permit execution: %v", err)
	}
	// Sanity: the original context would have refused.
	if err := exec.CommandContext(ctx, "true").Run(); err == nil {
		t.Fatal("cancelled context unexpectedly ran the command")
	}
}
