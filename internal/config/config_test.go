package config

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points config at a scratch XDG_CONFIG_HOME and clears the
// environment override, so a test never reads the machine's real preference.
func isolate(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	t.Setenv(AgentEnv, "")
	return filepath.Join(base, "canaveral")
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The overwhelmingly common case is no config file at all, which must be
// silent rather than an error every command has to tolerate.
func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load with no config file: %v", err)
	}
	if c.Agent != "" {
		t.Errorf("Agent = %q, want empty", c.Agent)
	}
}

func TestLoadReadsTheAgent(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, "agent = \"claude\"\n")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", c.Agent)
	}
}

// A typo must not be silently ignored: the whole point of the file is to
// change which agent runs, and quietly running the other one is the worst
// possible outcome.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, "agnet = \"claude\"\n")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted an unknown key")
	}
}

func TestDefaultAgentToolPrecedence(t *testing.T) {
	dir := isolate(t)
	if got := DefaultAgentTool(); got != DefaultAgent {
		t.Errorf("DefaultAgentTool() = %q with nothing set, want %q", got, DefaultAgent)
	}

	writeConfig(t, dir, "agent = \"claude\"\n")
	if got := DefaultAgentTool(); got != "claude" {
		t.Errorf("DefaultAgentTool() = %q, want the config file's claude", got)
	}

	// The environment wins, so a single run can be pointed elsewhere
	// without editing anything.
	t.Setenv(AgentEnv, "opencode")
	if got := DefaultAgentTool(); got != "opencode" {
		t.Errorf("DefaultAgentTool() = %q, want CANAVERAL_AGENT to win", got)
	}
}

// A broken config must not stop canaveral starting: the built-in default is
// a working answer, and refusing to open a feature over a stray key in a
// file the project does not even own would be the wrong trade.
func TestDefaultAgentToolSurvivesABrokenConfig(t *testing.T) {
	dir := isolate(t)
	writeConfig(t, dir, "agnet = \"claude\"\n")
	if got := DefaultAgentTool(); got != DefaultAgent {
		t.Errorf("DefaultAgentTool() = %q, want the built-in %q", got, DefaultAgent)
	}
}
