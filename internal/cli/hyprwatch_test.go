package cli

import (
	"testing"
)

func TestSplitWorkspaceName(t *testing.T) {
	cases := []struct {
		in            string
		project, feat string
		ok            bool
	}{
		{"norules:small-fixes", "norules", "small-fixes", true},
		{"norules:feat:with:colons", "norules", "feat:with:colons", true},
		{"1", "", "", false},
		{"", "", "", false},
		{"norules:", "", "", false},
		{":feature", "", "", false},
	}
	for _, c := range cases {
		project, feat, ok := splitWorkspaceName(c.in)
		if ok != c.ok {
			t.Errorf("splitWorkspaceName(%q) ok=%v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok && (project != c.project || feat != c.feat) {
			t.Errorf("splitWorkspaceName(%q) = (%q,%q), want (%q,%q)", c.in, project, feat, c.project, c.feat)
		}
	}
}

func TestLayoutChangedDetectsRealMovement(t *testing.T) {
	prev := map[string]float64{"chrome": 0.4, "opencode": 0.2, "terminal": 0.2, "serverlogs": 0.2}
	next := map[string]float64{"chrome": 0.5, "opencode": 0.2, "terminal": 0.15, "serverlogs": 0.15}
	if !layoutChanged(prev, next) {
		t.Error("layoutChanged should detect a real 10-point shift")
	}
}

func TestLayoutChangedIgnoresRoundingNoise(t *testing.T) {
	// A pixel or two of rounding error on an ordinary workspace switch must
	// not be treated as an intentional resize, or every switch would dirty
	// canaveral.toml.
	prev := map[string]float64{"chrome": 0.4, "opencode": 0.6}
	next := map[string]float64{"chrome": 0.4008, "opencode": 0.5992}
	if layoutChanged(prev, next) {
		t.Error("layoutChanged flagged a sub-epsilon rounding difference as a change")
	}
}

func TestLayoutChangedOnKeySetMismatch(t *testing.T) {
	prev := map[string]float64{"chrome": 0.4, "opencode": 0.6}
	next := map[string]float64{"chrome": 0.4}
	if !layoutChanged(prev, next) {
		t.Error("layoutChanged should treat a different key set as changed")
	}
}
