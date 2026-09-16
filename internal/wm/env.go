package wm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// runtimeDir is the private, per-user directory spawn files are written to.
// $XDG_RUNTIME_DIR is tmpfs, mode 0700, and cleared when the session ends, so
// a value written there never reaches persistent storage. Falling back to the
// temp dir keeps this working off a login session, where the file's own 0600
// has to carry it instead.
func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "canaveral")
	}
	return filepath.Join(os.TempDir(), "canaveral")
}

// spawnFileTTL is how long a spawn file can plausibly still be waiting to be
// read.
//
// The shell that consumes one is started by the same dispatch that created it
// and reads it as its first act, so the real window is milliseconds. Anything
// appreciably older than that was never going to be read: the compositor
// answered "ok" but nothing started, or whatever did was killed before its
// first line. Generous, because deleting a file a slow spawn is still on its
// way to would break the spawn, and lingering for a few minutes costs
// nothing — but not unbounded, because the whole point of the file is that
// its contents should not sit around.
const spawnFileTTL = 5 * time.Minute

// spawnFilePrefixes are the basename prefixes sweepSpawnFiles will reap:
// "env-" for Hyprland's environment fragment, "spawn-" for sway's whole
// launch script.
var spawnFilePrefixes = []string{"env-", "spawn-"}

// sweepSpawnFiles removes spawn files old enough to be certainly abandoned.
//
// Every error path in a spawn deletes its own file and the shell deletes it
// on the happy path, so this is for the case neither covers: a dispatch the
// compositor accepted that produced no process. Without it those accumulate
// for the life of the session, each one holding an entire window's
// environment — which for a project whose toolchain exports credentials is
// the thing writing them to a file was meant to stop leaving lying around.
func sweepSpawnFiles(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-spawnFileTTL)
	for _, e := range entries {
		named := slices.ContainsFunc(spawnFilePrefixes, func(p string) bool {
			return strings.HasPrefix(e.Name(), p)
		})
		if e.IsDir() || !named || !strings.HasSuffix(e.Name(), ".sh") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// removeSpawnFile cleans up a file the spawned shell will now never read,
// because the spawn did not happen. On the success path the shell deletes it
// instead.
func removeSpawnFile(path string) {
	if path != "" {
		_ = os.Remove(path)
	}
}

// createSpawnFile creates a private file in the runtime directory and writes
// body to it, sweeping abandoned ones on the way past.
//
// The point of the file is to keep values out of any command line.
// /proc/<pid>/cmdline is world-readable — mode 444, not 400 like
// /proc/<pid>/environ — so an `env K=V` prefix publishes every variable a
// window is given to every process on the machine, for as long as that window
// lives. Measured on this setup: three `sh -c env 'ANTHROPIC_API_KEY=...'
// alacritty` processes, one per window of a single feature, readable by
// anyone. A toolchain that exports API keys and database credentials (mise
// does) hands them straight out.
func createSpawnFile(pattern, body string) (string, error) {
	dir := runtimeDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("spawn dir: %w", err)
	}
	sweepSpawnFiles(dir)
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("spawn file: %w", err)
	}
	defer func() { _ = f.Close() }()
	// CreateTemp is already 0600; stated rather than assumed, since the whole
	// point of this file is that nothing else can read it.
	if err := f.Chmod(0o600); err != nil {
		removeSpawnFile(f.Name())
		return "", fmt.Errorf("spawn file: %w", err)
	}
	if _, err := f.WriteString(body); err != nil {
		removeSpawnFile(f.Name())
		return "", fmt.Errorf("spawn file: %w", err)
	}
	if err := f.Close(); err != nil {
		removeSpawnFile(f.Name())
		return "", fmt.Errorf("close spawn file: %w", err)
	}
	return f.Name(), nil
}

// writeEnvFile writes env as a shell fragment and returns its path, or ""
// when there is nothing to write.
//
// A file is what keeps Hyprland's exec-time workspace rule, which binds to
// the process hyprctl starts and so cannot be given up, while keeping the
// environment off the command line. The spawned shell sources it and deletes
// it immediately, leaving only a path on the command line and nothing at rest.
func writeEnvFile(env map[string]string) (string, error) {
	if len(env) == 0 {
		return "", nil
	}
	return createSpawnFile("env-*.sh", envAssignments(env))
}

// envAssignments renders env as `K=v` lines, one per variable, in a fixed
// order so that spawned command lines stay stable across runs.
func envAssignments(env map[string]string) string {
	var b strings.Builder
	for _, kv := range sortedEnv(env) {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(shellQuote(v))
		b.WriteString("\n")
	}
	return b.String()
}

// envPrefix is the shell that loads an environment file and then removes it.
//
// `set -a` exports what the file assigns, so plain `K=v` lines become part of
// the environment rather than shell-local variables. The delete is here, in
// the spawned shell, rather than in canaveral: canaveral has already returned
// by the time the shell runs, and deleting from there would race the read.
func envPrefix(path string) string {
	if path == "" {
		return ""
	}
	q := shellQuote(path)
	return "set -a; . " + q + "; set +a; rm -f " + q + "; "
}

// writeSpawnScript writes a whole window launch — environment, then the
// command — as one self-deleting shell script, and returns its path.
//
// sway's command parser splits a command list on `;` and `,` before anything
// else sees it, and its idea of what is quoted is not the shell's, so passing
// a real command line through `swaymsg exec` risks having it cut in half at a
// semicolon that was never meant as a separator. A script has neither
// character in its path, so `exec sh <path>` survives that parser intact.
func writeSpawnScript(s SpawnSpec) (string, error) {
	argv, err := buildArgv(s, "")
	if err != nil {
		return "", err
	}
	// Written with a placeholder for its own path, since a file cannot name
	// itself before it exists; patched in below.
	const self = "@SELF@"
	var b strings.Builder
	if len(s.Env) > 0 {
		b.WriteString("set -a\n")
		b.WriteString(envAssignments(s.Env))
		b.WriteString("set +a\n")
	}
	// Unlinked before the command replaces this shell. The script is already
	// open, so `sh` reads the rest of it from the fd regardless.
	b.WriteString("rm -f " + self + "\n")
	b.WriteString("exec " + argv + "\n")

	path, err := createSpawnFile("spawn-*.sh", "")
	if err != nil {
		return "", err
	}
	if err := checkSwayCommandSafe(path); err != nil {
		removeSpawnFile(path)
		return "", err
	}
	body := strings.Replace(b.String(), self, shellQuote(path), 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		removeSpawnFile(path)
		return "", fmt.Errorf("spawn file: %w", err)
	}
	return path, nil
}

// checkSwayCommandSafe rejects a path that could not be handed to sway
// verbatim. In practice $XDG_RUNTIME_DIR is /run/user/<uid> and the generated
// basename is digits, so this never fires; it fires loudly rather than
// producing a command sway would silently cut in two.
func checkSwayCommandSafe(path string) error {
	if strings.ContainsAny(path, " \t\n\r;,'\"\\[]") {
		return fmt.Errorf("runtime directory path %q cannot be passed to sway", path)
	}
	return nil
}

func sortedEnv(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// buildArgv renders a SpawnSpec as a command line for a compositor's `exec`,
// which both Hyprland and sway hand to `/bin/sh -c`.
//
// envFile is Hyprland's environment fragment, sourced before anything else;
// sway passes "" because its script carries the environment itself.
func buildArgv(s SpawnSpec, envFile string) (string, error) {
	if s.Class == "" {
		return "", errors.New("spawn: class is required")
	}
	// Environment is applied to the terminal itself rather than to the inner
	// command, so an interactive shell started with no command still inherits
	// CANAVERAL_ROOT and friends.
	prefix := envPrefix(envFile)

	if !s.IsTerminal {
		if strings.TrimSpace(s.Cmd) == "" {
			return "", fmt.Errorf("window %s: exec command is empty", s.Class)
		}
		// GUI applications set their own class; the command is used verbatim.
		//
		// The prefix goes before the cd rather than between it and the
		// command, so that `cd && cmd` still means "only run cmd if the
		// directory was there" — threading a `;`-separated preamble through
		// that && would quietly break it.
		inner := s.Cmd
		if s.Dir != "" {
			inner = fmt.Sprintf("cd %s && %s", shellQuote(s.Dir), inner)
		}
		return "sh -c " + shellQuote(prefix+inner), nil
	}

	term := s.Terminal
	if term == "" {
		term = TerminalBin
	}
	parts := []string{
		term,
		"--class", s.Class + "," + s.Class,
	}
	if s.Title != "" {
		parts = append(parts, "-T", s.Title)
	}
	if s.Dir != "" {
		parts = append(parts, "--working-directory", s.Dir)
	}
	if strings.TrimSpace(s.Cmd) != "" {
		inner := s.Cmd
		if s.Hold {
			// Keep the pane alive so a crash is readable instead of vanishing.
			inner += "; echo; echo '[canaveral] command exited, press enter to close'; read _"
		}
		parts = append(parts, "-e", "sh", "-c", inner)
	}

	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = shellQuote(p)
	}
	return prefix + strings.Join(quoted, " "), nil
}
