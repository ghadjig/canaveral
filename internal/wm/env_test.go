package wm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildArgvTerminal(t *testing.T) {
	got, err := buildArgv(SpawnSpec{
		Class: "canaveral-p-f-logs", Title: "f · logs",
		Dir: "/wt/f", IsTerminal: true, Cmd: "canaveral logs f web -f",
	}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	for _, want := range []string{
		"'alacritty'",
		"'canaveral-p-f-logs,canaveral-p-f-logs'",
		"'--working-directory' '/wt/f'",
		"'canaveral logs f web -f'",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("argv missing %q:\n%s", want, got)
		}
	}
}

func TestBuildArgvPlainShell(t *testing.T) {
	// run = "" means a bare shell: no -e flag at all.
	got, err := buildArgv(SpawnSpec{Class: "c", Dir: "/wt", IsTerminal: true, Cmd: ""}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if strings.Contains(got, "-e") {
		t.Errorf("plain shell should not pass -e: %s", got)
	}
}

func TestBuildArgvHoldKeepsPaneOpen(t *testing.T) {
	got, err := buildArgv(SpawnSpec{Class: "c", IsTerminal: true, Cmd: "false", Hold: true}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if !strings.Contains(got, "read _") {
		t.Errorf("hold should keep the pane open: %s", got)
	}
}

func TestBuildArgvExecGUI(t *testing.T) {
	got, err := buildArgv(SpawnSpec{
		Class: "c", Dir: "/wt", Cmd: "google-chrome --new-window http://localhost:3001",
	}, "")
	if err != nil {
		t.Fatalf("buildArgv: %v", err)
	}
	if strings.Contains(got, "alacritty") {
		t.Errorf("exec window must not be wrapped in a terminal: %s", got)
	}
	if !strings.Contains(got, "google-chrome") {
		t.Errorf("argv missing command: %s", got)
	}
}

func TestBuildArgvRejectsEmptyExec(t *testing.T) {
	if _, err := buildArgv(SpawnSpec{Class: "c"}, ""); err == nil {
		t.Error("empty exec command should error")
	}
}

func TestBuildArgvRequiresClass(t *testing.T) {
	if _, err := buildArgv(SpawnSpec{IsTerminal: true}, ""); err == nil {
		t.Error("missing class should error")
	}
}

// The leak this guards against: /proc/<pid>/cmdline is mode 444, so anything
// on a spawned window's command line is readable by every process on the
// machine for as long as that window lives. An `env K=V` prefix put API keys
// and database credentials there.
func TestBuildArgvKeepsEnvValuesOutOfTheCommandLine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	secret := "sk-ant-not-a-real-key"
	env := map[string]string{"ANTHROPIC_API_KEY": secret, "CANAVERAL_ROOT": "/r"}

	path, err := writeEnvFile(env)
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	for _, spec := range []SpawnSpec{
		{Class: "c", IsTerminal: true, Cmd: "true", Dir: "/wt", Env: env},
		{Class: "c", Cmd: "google-chrome --new-window http://x", Dir: "/wt", Env: env},
	} {
		got, err := buildArgv(spec, path)
		if err != nil {
			t.Fatalf("buildArgv: %v", err)
		}
		if strings.Contains(got, secret) {
			t.Errorf("secret leaked into argv:\n%s", got)
		}
		if strings.Contains(got, "ANTHROPIC_API_KEY=") {
			t.Errorf("env assignment leaked into argv:\n%s", got)
		}
		if !strings.Contains(got, path) {
			t.Errorf("argv should reference the env file:\n%s", got)
		}
	}
}

func TestEnvFileIsPrivateSortedAndSelfDeleting(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := writeEnvFile(map[string]string{"B": "2", "A": "1", "C": "it's"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Owner-only: the whole point is that this is less readable than the
	// command line it replaces.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("env file mode = %o, want 600", perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "A='1'\nB='2'\nC='it'\\''s'\n"
	if string(b) != want {
		t.Errorf("env file =\n%q\nwant\n%q", b, want)
	}
	// The spawned shell, not canaveral, is what removes it — canaveral has
	// returned long before the shell runs.
	if pre := envPrefix(path); !strings.Contains(pre, "rm -f") || !strings.Contains(pre, "set -a") {
		t.Errorf("env prefix should export and then delete: %s", pre)
	}
	out, err := exec.Command("/bin/sh", "-c", envPrefix(path)+`exec /bin/sh -c 'printf "%s\n" "$A" "$B" "$C"'`).CombinedOutput()
	if err != nil {
		t.Fatalf("source env file: %v: %s", err, out)
	}
	if string(out) != "1\n2\nit's\n" {
		t.Errorf("exported environment = %q", out)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("env file should be removed, stat error = %v", err)
	}
}

func TestWriteEnvFileSkipsAnEmptyEnvironment(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := writeEnvFile(nil)
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	if path != "" {
		t.Errorf("path = %q, want empty", path)
	}
	if envPrefix("") != "" {
		t.Errorf("empty path should produce no prefix")
	}
}

func TestShellQuoteEscapesQuotes(t *testing.T) {
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}

// Every error path in a spawn deletes its own file and the shell deletes it on
// the happy path, so what accumulates is the case neither covers: a dispatch
// the compositor accepted that produced no process. Fifty of those were found
// in a live session, each holding a full window environment.
func TestWriteEnvFileSweepsAbandonedFiles(t *testing.T) {
	runtime := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtime)
	dir := filepath.Join(runtime, "canaveral")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	old := time.Now().Add(-2 * spawnFileTTL)
	// Both kinds of abandoned file: Hyprland's environment fragment and
	// sway's whole launch script, which holds the same secrets.
	var stale []string
	for _, name := range []string{"env-stale.sh", "spawn-stale.sh"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("SECRET=1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		stale = append(stale, p)
	}
	// A file young enough to still be on its way to a shell, and one that is
	// not ours at all: neither may be touched.
	fresh := filepath.Join(dir, "env-fresh.sh")
	if err := os.WriteFile(fresh, []byte("K=v\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(dir, "something-else")
	if err := os.WriteFile(other, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, old, old); err != nil {
		t.Fatal(err)
	}

	path, err := writeEnvFile(map[string]string{"A": "b"})
	if err != nil {
		t.Fatalf("writeEnvFile: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	for _, p := range stale {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("an abandoned spawn file must be swept: %s", filepath.Base(p))
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("a file young enough to still be read must be left alone")
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("only canaveral's own spawn files may be swept")
	}
}

// sway's spawn script is the whole launch, so it has to survive being run as
// a script: export the environment, delete itself, and exec the command.
func TestSpawnScriptExportsRunsAndDeletesItself(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	secret := "sk-ant-not-a-real-key"
	marker := filepath.Join(t.TempDir(), "ran")
	path, err := writeSpawnScript(SpawnSpec{
		Class: "canaveral-p-f-web",
		Cmd:   "printf '%s' \"$ANTHROPIC_API_KEY\" > " + marker,
		Env:   map[string]string{"ANTHROPIC_API_KEY": secret},
	})
	if err != nil {
		t.Fatalf("writeSpawnScript: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("spawn script mode = %o, want 600", perm)
	}

	if out, err := exec.Command("/bin/sh", path).CombinedOutput(); err != nil {
		t.Fatalf("run spawn script: %v: %s", err, out)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the script's command did not run: %v", err)
	}
	if string(got) != secret {
		t.Errorf("command saw ANTHROPIC_API_KEY=%q, want the value it was given", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("spawn script should delete itself, stat error = %v", err)
	}
}

// The reason the script exists at all. sway cuts a command list at the first
// unquoted `;` or `,` before anything shell-like has had a say, so a command
// line carrying either — which every environment-bearing spawn does, and every
// terminal spawn does via `--class C,C` — cannot be handed to `swaymsg exec`
// directly. Only the path may cross that boundary.
func TestSpawnScriptPathCarriesNothingSwayWouldSplitOn(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	path, err := writeSpawnScript(SpawnSpec{
		Class: "canaveral-p-f-term", IsTerminal: true, Cmd: "echo hi; echo bye",
		Env: map[string]string{"A": "1"},
	})
	if err != nil {
		t.Fatalf("writeSpawnScript: %v", err)
	}
	defer func() { _ = os.Remove(path) }()

	if err := checkSwayCommandSafe(path); err != nil {
		t.Errorf("spawn script path is not safe to hand to sway: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The characters sway would split on are all still in the script, which
	// is exactly why they must not be on the command line.
	if !strings.Contains(string(body), ";") || !strings.Contains(string(body), ",") {
		t.Errorf("expected the script to hold what the command line cannot:\n%s", body)
	}
}

func TestSpawnScriptRejectsAnUnquotableRuntimeDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "run dir;with trouble"))
	path, err := writeSpawnScript(SpawnSpec{Class: "c", Cmd: "true"})
	if err == nil {
		_ = os.Remove(path)
		t.Fatal("a runtime directory sway cannot be given should be refused, not mangled")
	}
	if !strings.Contains(err.Error(), "sway") {
		t.Errorf("err = %v, want it to say why", err)
	}
}
