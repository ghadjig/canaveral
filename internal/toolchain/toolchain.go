// Package toolchain resolves per-directory development environments.
//
// Version managers such as mise and asdf select tool versions from config files
// in (or above) the working directory, and normally apply them through an
// interactive shell hook. Units started by canaveral have no such hook, so the
// environment must be resolved explicitly. Without this, a project pinning
// ruby 3.4.7 would silently run against whatever version happens to be first on
// the inherited PATH.
package toolchain

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Mode selects which resolver to use.
type Mode string

const (
	// ModeAuto detects a supported version manager, falling back to none.
	ModeAuto Mode = "auto"
	// ModeMise forces mise resolution and reports an error if it fails.
	ModeMise Mode = "mise"
	// ModeNone inherits the caller's environment unchanged.
	ModeNone Mode = "none"
)

// ParseMode validates a manifest toolchain value.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(strings.ToLower(strings.TrimSpace(s))); m {
	case "", ModeAuto:
		return ModeAuto, nil
	case ModeMise, ModeNone:
		return m, nil
	default:
		return "", fmt.Errorf("invalid toolchain %q (want \"auto\", \"mise\" or \"none\")", s)
	}
}

// miseConfigNames are the files that cause mise to activate for a directory.
var miseConfigNames = []string{
	"mise.toml", ".mise.toml", "mise.local.toml", ".mise.local.toml",
	".tool-versions", ".ruby-version", ".node-version", ".python-version",
}

// findMiseConfig walks up from dir looking for a mise configuration file.
func findMiseConfig(dir string) bool {
	d := dir
	for {
		for _, n := range miseConfigNames {
			if _, err := os.Stat(filepath.Join(d, n)); err == nil {
				return true
			}
		}
		if _, err := os.Stat(filepath.Join(d, ".mise", "config.toml")); err == nil {
			return true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return false
		}
		d = parent
	}
}

type cacheKey struct {
	mode Mode
	dir  string
}

var (
	mu    sync.Mutex
	cache = map[cacheKey]map[string]string{}
)

// Env returns environment overrides to apply when running commands in dir.
//
// Results are cached per (mode, dir) because resolution shells out and `up`
// asks for the same directory repeatedly.
func Env(ctx context.Context, mode Mode, dir string) (map[string]string, error) {
	if mode == ModeNone {
		return nil, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	key := cacheKey{mode, abs}

	mu.Lock()
	if v, ok := cache[key]; ok {
		mu.Unlock()
		return v, nil
	}
	mu.Unlock()

	env, err := resolveMise(ctx, mode, abs)
	if err != nil {
		return nil, err
	}

	mu.Lock()
	cache[key] = env
	mu.Unlock()
	return env, nil
}

func resolveMise(ctx context.Context, mode Mode, dir string) (map[string]string, error) {
	bin, err := exec.LookPath("mise")
	if err != nil {
		if mode == ModeMise {
			return nil, fmt.Errorf("toolchain=\"mise\" but mise is not in PATH: %w", err)
		}
		return nil, nil
	}
	if mode == ModeAuto && !findMiseConfig(dir) {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "env", "--json", "-C", dir)
	// Run detached from any inherited mise state so the result depends only on dir.
	cmd.Env = append(os.Environ(), "MISE_QUIET=1")
	out, err := cmd.Output()
	if err != nil {
		if mode == ModeMise {
			return nil, fmt.Errorf("mise env failed for %s: %w", dir, err)
		}
		return nil, nil
	}

	var env map[string]string
	if err := json.Unmarshal(out, &env); err != nil {
		if mode == ModeMise {
			return nil, fmt.Errorf("parse mise env for %s: %w", dir, err)
		}
		return nil, nil
	}
	return env, nil
}
