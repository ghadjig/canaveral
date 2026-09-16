package wm

import (
	"errors"
	"strings"
	"testing"
)

// backendName reports which backend detection picked, or "" plus the error
// when it picked none.
func backendName(t *testing.T) (string, error) {
	t.Helper()
	b, err := current()
	if err != nil {
		return "", err
	}
	return b.name(), nil
}

// clearCompositorEnv unsets every variable detection reads. The worktree this
// runs in is itself a compositor session, so a test that does not do this is
// testing the developer's desktop rather than the code.
func clearCompositorEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{OverrideEnv, "HYPRLAND_INSTANCE_SIGNATURE", "SWAYSOCK", "XDG_CURRENT_DESKTOP"} {
		t.Setenv(k, "")
	}
}

func TestDetectionPicksTheRunningCompositor(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"hyprland", map[string]string{"HYPRLAND_INSTANCE_SIGNATURE": "sig"}, "hyprland"},
		{"sway", map[string]string{"SWAYSOCK": "/run/user/1000/sway-ipc.sock"}, "sway"},
		{
			"sway nested in hyprland",
			map[string]string{
				"HYPRLAND_INSTANCE_SIGNATURE": "sig",
				"SWAYSOCK":                    "/run/user/1000/sway-ipc.sock",
				"XDG_CURRENT_DESKTOP":         "sway",
			},
			"sway",
		},
		{
			// Both variables set with nothing to tell them apart: an
			// inherited SWAYSOCK from a session that is no longer the one in
			// front of the user is the likelier of the two, so the Hyprland
			// signature wins by default.
			"ambiguous",
			map[string]string{
				"HYPRLAND_INSTANCE_SIGNATURE": "sig",
				"SWAYSOCK":                    "/run/user/1000/sway-ipc.sock",
			},
			"hyprland",
		},
		{"override", map[string]string{OverrideEnv: "sway", "HYPRLAND_INSTANCE_SIGNATURE": "sig"}, "sway"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearCompositorEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			got, err := backendName(t)
			if err != nil {
				t.Fatalf("current(): %v", err)
			}
			if got != c.want {
				t.Errorf("backend = %q, want %q", got, c.want)
			}
		})
	}
}

// Nothing above this package inspects the error, only whether there was one,
// so what matters is that a bare login session is a clean "no compositor"
// rather than a backend that then fails obscurely.
func TestDetectionFailsWithNoCompositor(t *testing.T) {
	clearCompositorEnv(t)
	if _, err := backendName(t); !errors.Is(err, ErrUnavailable) {
		t.Errorf("err = %v, want ErrUnavailable", err)
	}
}

func TestDetectionRejectsAnUnknownOverride(t *testing.T) {
	clearCompositorEnv(t)
	t.Setenv(OverrideEnv, "i3")
	_, err := backendName(t)
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	// Naming the value is the whole point: the alternative is silently
	// falling through to detection and driving the wrong compositor.
	if got := err.Error(); !strings.Contains(got, "i3") || !strings.Contains(got, OverrideEnv) {
		t.Errorf("err = %q, want it to name the variable and its value", got)
	}
}
