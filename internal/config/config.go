// Package config reads canaveral's user-level settings.
//
// This is deliberately tiny and deliberately separate from the project
// manifest. A manifest describes one project and is committed alongside it,
// so it is the wrong place to record which coding agent *you* happen to use:
// the same repo gets opened by people running different tools. Anything that
// is a property of the machine rather than the project belongs here.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// FileName is the config file's name inside canaveral's config directory.
const FileName = "config.toml"

// DefaultAgent is the agent tool used when nothing says otherwise.
const DefaultAgent = "opencode"

// AgentEnv overrides the configured default agent for a single invocation.
const AgentEnv = "CANAVERAL_AGENT"

// Config is the whole of canaveral's user configuration.
type Config struct {
	// Agent names the coding agent harness a manifest's [[agent]] blocks
	// get when they don't name one themselves, e.g. "opencode" or "claude".
	Agent string `toml:"agent"`
}

// Dir returns canaveral's config directory, honouring XDG_CONFIG_HOME and
// falling back to ~/.config/canaveral. It is not created: unlike the state
// directory, nothing here is written by canaveral, so an absent directory
// simply means "no config", not "not set up yet".
func Dir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "canaveral"), nil
}

// Path returns the config file's full path.
func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, FileName), nil
}

// Load reads the config file. A missing file is not an error — it is the
// normal case — and yields the zero Config, which every reader treats as
// "use the built-in default".
//
// A malformed file *is* an error, and is returned rather than swallowed:
// silently ignoring a typo would leave you running a different agent than
// the one you asked for, with nothing to explain why.
func Load() (Config, error) {
	p, err := Path()
	if err != nil {
		return Config{}, err
	}
	var c Config
	md, err := toml.DecodeFile(p, &c)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			keys = append(keys, k.String())
		}
		return Config{}, fmt.Errorf("%s: unknown key(s): %s", p, strings.Join(keys, ", "))
	}
	return c, nil
}

// DefaultAgentTool returns the agent tool to use for a manifest [[agent]]
// block that does not name one.
//
// Precedence, narrowest first: CANAVERAL_AGENT, then the config file's
// `agent`, then opencode. The environment wins so a single run can be
// pointed at another harness without editing anything — useful for trying
// one out, and for a wrapper script that opens the same project with
// whichever agent it prefers.
//
// A broken or unreadable config never stops canaveral starting: the built-in
// default stands in. Config errors surface through Load, which `canaveral
// status` and friends report; refusing to open a feature over a stray key in
// a file the project does not even own would be the wrong trade.
func DefaultAgentTool() string {
	if v := os.Getenv(AgentEnv); v != "" {
		return v
	}
	if c, err := Load(); err == nil && c.Agent != "" {
		return c.Agent
	}
	return DefaultAgent
}
