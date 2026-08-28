package cli

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestDebounceCoalescesBurst(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	fires := 0
	trigger := debounce(ctx, 60*time.Millisecond, func() {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	// A burst of 10 calls in quick succession — exactly what tearing down a
	// 4-window feature produces, times over — must collapse to one fire.
	for i := 0; i < 10; i++ {
		trigger()
		time.Sleep(5 * time.Millisecond)
	}

	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 1 {
		t.Errorf("fires = %d, want exactly 1 for a coalesced burst", got)
	}
}

func TestDebounceFiresAgainAfterQuietPeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	fires := 0
	trigger := debounce(ctx, 40*time.Millisecond, func() {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	trigger()
	time.Sleep(120 * time.Millisecond) // let the first debounce window fire
	trigger()
	time.Sleep(120 * time.Millisecond) // let the second one fire too

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 2 {
		t.Errorf("fires = %d, want 2 for two separate, well-spaced triggers", got)
	}
}

func TestDebounceStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	fires := 0
	trigger := debounce(ctx, 30*time.Millisecond, func() {
		mu.Lock()
		fires++
		mu.Unlock()
	})

	trigger()
	cancel() // cancel before the debounce delay elapses
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	got := fires
	mu.Unlock()
	if got != 0 {
		t.Errorf("fires = %d, want 0: cancellation should suppress the pending fire", got)
	}
}

func TestRelevantEventsScope(t *testing.T) {
	// This set is deliberately narrow: only events that change which feature
	// workspaces exist or which is active should trigger a refresh. Anything
	// broader (window focus churn, layer events) would make the waybar
	// module refresh on unrelated desktop activity.
	want := []string{
		"workspace", "workspacev2",
		"createworkspace", "createworkspacev2",
		"destroyworkspace", "destroyworkspacev2",
	}
	if len(relevantEvents) != len(want) {
		t.Fatalf("relevantEvents has %d entries, want %d: %v", len(relevantEvents), len(want), relevantEvents)
	}
	for _, e := range want {
		if !relevantEvents[e] {
			t.Errorf("relevantEvents missing %q", e)
		}
	}
	for _, unrelated := range []string{"activewindow", "openlayer", "closelayer", "openwindow", "closewindow", "focusedmon"} {
		if relevantEvents[unrelated] {
			t.Errorf("relevantEvents should not include %q", unrelated)
		}
	}
}

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
