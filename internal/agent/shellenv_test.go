package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestShellEnvRecoversExportsFromBashrc(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	home := t.TempDir()
	// Noninteractive launchers do not read this file; an interactive probe
	// must, including after the usual early return in a real .bashrc.
	rc := `case $- in *i*) ;; *) return;; esac
printf 'startup banner\nFAKE_VARIABLE=not-an-export\n'
export EDITOR='vim -f'
export VISUAL='code --wait'
export CANAVERAL_TEST_EXPORT='first line
second=value'
export CUSTOM_EXPORT='first line
second=value'
export CANAVERAL_FEATURE=wrong-feature
export OPENCODE_PORT=1234
`
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("SHELL", bash)
	t.Setenv("EDITOR", "")
	t.Setenv("VISUAL", "")
	t.Setenv("CUSTOM_EXPORT", "")
	t.Setenv("CANAVERAL_FEATURE", "parent")
	t.Setenv("OPENCODE_PORT", "9999")
	t.Setenv("BASH_ENV", "")
	env := shellEnvVia("-ic")
	for k, want := range map[string]string{"EDITOR": "vim -f", "VISUAL": "code --wait", "CUSTOM_EXPORT": "first line\nsecond=value"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"FAKE_VARIABLE", "CANAVERAL_FEATURE", "OPENCODE_PORT", "PWD", "SHLVL"} {
		if _, ok := env[k]; ok {
			t.Errorf("unexpected inherited variable %s", k)
		}
	}
}

func TestShellEnvMergesStartupExportsAndReturnsCopies(t *testing.T) {
	resetShellPATHCacheForTest()
	t.Cleanup(resetShellPATHCacheForTest)
	shell := filepath.Join(t.TempDir(), "shell")
	script := `#!/bin/sh
printf '\000canaveral-shell-env\000'
if [ "$1" = '-ic' ]; then
  printf 'EDITOR=vim\000PATH=/interactive\000CUSTOM_EXPORT=interactive\000'
else
  printf 'EDITOR=caller\000PATH=/login\000LOGIN_EXPORT=yes\000'
fi
`
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	t.Setenv("PATH", "/caller")
	t.Setenv("EDITOR", "caller")
	t.Setenv("CUSTOM_EXPORT", "")
	t.Setenv("LOGIN_EXPORT", "")
	t.Setenv("INVOCATION_ID", "parent-unit")
	t.Setenv("CANAVERAL_PORT_WEB", "8888")
	t.Setenv("DB_SUFFIX", "_parent")
	env := ShellEnv()
	for k, want := range map[string]string{"EDITOR": "vim", "CUSTOM_EXPORT": "interactive", "LOGIN_EXPORT": "yes", "PATH": "/caller:/interactive:/login"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	for _, k := range []string{"INVOCATION_ID", "CANAVERAL_PORT_WEB", "DB_SUFFIX"} {
		if _, ok := env[k]; ok {
			t.Errorf("inherited parent identity %s", k)
		}
	}
	env["EDITOR"] = "mutated"
	if ShellEnv()["EDITOR"] != "vim" {
		t.Fatal("caller mutated cached shell environment")
	}
}

func TestShellEnvFallsBackToCallerWhenShellFails(t *testing.T) {
	t.Setenv("SHELL", "/nonexistent/canaveral-test-shell")
	t.Setenv("EDITOR", "emacs")
	t.Setenv("PATH", "/caller")
	env := computeShellEnv()
	if env["EDITOR"] != "emacs" || env["PATH"] != "/caller" {
		t.Fatalf("caller environment lost: EDITOR=%q PATH=%q", env["EDITOR"], env["PATH"])
	}
}
