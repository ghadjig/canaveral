package toolchain

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMode(t *testing.T) {
	cases := map[string]Mode{
		"":      ModeAuto,
		"auto":  ModeAuto,
		"AUTO":  ModeAuto,
		"mise":  ModeMise,
		"none":  ModeNone,
		" none": ModeNone,
	}
	for in, want := range cases {
		got, err := ParseMode(in)
		if err != nil {
			t.Errorf("ParseMode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseMode(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := ParseMode("asdf"); err == nil {
		t.Error("ParseMode(asdf): want error")
	}
}

func TestFindMiseConfigWalksUp(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mise.toml"), []byte("[tools]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if !findMiseConfig(nested) {
		t.Error("findMiseConfig should walk up to the parent mise.toml")
	}
}

func TestFindMiseConfigRubyVersion(t *testing.T) {
	dir := t.TempDir()
	if !writeAndCheck(t, dir, ".ruby-version", "3.4.7") {
		t.Error(".ruby-version should activate mise")
	}
}

func writeAndCheck(t *testing.T, dir, name, body string) bool {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return findMiseConfig(dir)
}

func TestFindMiseConfigAbsent(t *testing.T) {
	// A bare temp dir has no config; the walk must terminate at the filesystem
	// root rather than looping.
	dir := t.TempDir()
	sub := filepath.Join(dir, "x")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// This may legitimately be true if a parent of TMPDIR has a config, so only
	// assert that the call returns without hanging.
	_ = findMiseConfig(sub)
}

func TestEnvModeNoneReturnsNil(t *testing.T) {
	env, err := Env(t.Context(), ModeNone, t.TempDir())
	if err != nil {
		t.Fatalf("Env: %v", err)
	}
	if env != nil {
		t.Errorf("Env(ModeNone) = %v, want nil", env)
	}
}
