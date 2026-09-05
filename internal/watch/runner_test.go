package watch

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bandito/canaveral/internal/agent"
	"github.com/bandito/canaveral/internal/state"
)

// testRunner builds a Runner with the network and disk seams stubbed out.
func testRunner(t *testing.T, features []*state.Feature, probe func(ctx context.Context, tool string, c agent.Conn) agent.Health) *Runner {
	t.Helper()
	r := NewRunner(Options{Debounce: 10 * time.Millisecond, Rescan: time.Hour, Safety: time.Hour})
	r.load = func(string) ([]*state.Feature, error) { return features, nil }
	r.probe = probe
	return r
}

func decodeLines(t *testing.T, s string) []Snapshot {
	t.Helper()
	var out []Snapshot
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(line), &snap); err != nil {
			t.Fatalf("decode %q: %v", line, err)
		}
		out = append(out, snap)
	}
	return out
}

func TestRunEmitsAnInitialSnapshotImmediately(t *testing.T) {
	// A widget that has just launched must have something to render rather
	// than staying blank until the first event happens to arrive.
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		return agent.Health{Reachable: true, State: agent.StateIdle}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var buf bytes.Buffer
	if err := r.Run(ctx, &buf); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snaps := decodeLines(t, buf.String())
	if len(snaps) == 0 {
		t.Fatal("no snapshot emitted at startup")
	}
	if len(snaps[0].Features) != 1 || snaps[0].Features[0].Key != "norules/alpha" {
		t.Errorf("first snapshot = %+v", snaps[0].Features)
	}
}

func TestRunFlushesTheStartupSnapshotBeforeExiting(t *testing.T) {
	// Regression test: the startup snapshot must reach the consumer while
	// the process is still running. Buffered without an explicit flush it
	// would sit unsent until enough later output filled the buffer, so a
	// widget watching a quiet set of features would stay blank
	// indefinitely — and a test that only inspected output after shutdown
	// would not notice, because exiting flushes.
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		return agent.Health{Reachable: true, State: agent.StateIdle}
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	go func() {
		_ = r.Run(ctx, bufio.NewWriter(pw))
		_ = pw.Close()
	}()

	type res struct {
		line string
		err  error
	}
	got := make(chan res, 1)
	go func() {
		line, err := bufio.NewReader(pr).ReadString('\n')
		got <- res{line, err}
	}()

	select {
	case v := <-got:
		if v.err != nil {
			t.Fatalf("reading startup snapshot: %v", v.err)
		}
		var snap Snapshot
		if err := json.Unmarshal([]byte(v.line), &snap); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(snap.Features) != 1 {
			t.Errorf("got %d features, want 1", len(snap.Features))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("startup snapshot never arrived while running (not flushed)")
	}
}

func TestRunEmitsOnStateChange(t *testing.T) {
	var mu sync.Mutex
	st := agent.StateIdle
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		mu.Lock()
		defer mu.Unlock()
		return agent.Health{Reachable: true, State: st}
	})

	ctx, cancel := context.WithCancel(context.Background())
	var buf lockedBuffer
	done := make(chan struct{})
	go func() { _ = r.Run(ctx, &buf); close(done) }()

	time.Sleep(80 * time.Millisecond)
	mu.Lock()
	st = agent.StateWaiting
	mu.Unlock()
	r.wake()

	time.Sleep(150 * time.Millisecond)
	cancel()
	<-done

	snaps := decodeLines(t, buf.String())
	if len(snaps) < 2 {
		t.Fatalf("got %d snapshots, want at least 2 (initial + change): %s", len(snaps), buf.String())
	}
	last := snaps[len(snaps)-1]
	if last.Features[0].Status != StatusWaiting {
		t.Errorf("final status = %q, want waiting", last.Features[0].Status)
	}
	if last.Summary.NeedsAttention != 1 {
		t.Errorf("NeedsAttention = %d, want 1", last.Summary.NeedsAttention)
	}
}

func TestRunEmitsTeardownOfAFeatureLeavingAPhase(t *testing.T) {
	// A feature torn down straight out of a "removing" phase must still be
	// reported gone. The lifecycle path sees it vanish first and asks for a
	// full refresh; the regression was that it also rebuilt the view in the
	// meantime, recording the feature-removed world as the baseline, so the
	// woken refresh compared equal and emitted nothing — and every consumer
	// kept drawing a feature that no longer existed.
	var mu sync.Mutex
	f := feat("alpha", "main")
	f.Phase, f.PhaseSince = "removing", time.Now()
	features := []*state.Feature{f}

	r := testRunner(t, features, func(context.Context, string, agent.Conn) agent.Health {
		return agent.Health{Reachable: true, State: agent.StateIdle}
	})
	// The load seam has to return the live set, so the teardown below is seen.
	r.load = func(string) ([]*state.Feature, error) {
		mu.Lock()
		defer mu.Unlock()
		return features, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var buf lockedBuffer
	done := make(chan struct{})
	go func() { _ = r.Run(ctx, &buf); close(done) }()

	// Let at least one phase tick (200ms) observe the feature in its phase, so
	// it is recorded in the in-phase set. The bug only fires on the transition
	// out of a KNOWN phase; tearing down before the first tick would take the
	// ordinary path and hide it.
	time.Sleep(320 * time.Millisecond)

	// Tear it down: the next phase ticker sees a formerly-in-phase feature
	// vanish, and must drive an emit that reports it gone.
	mu.Lock()
	features = []*state.Feature{}
	mu.Unlock()

	time.Sleep(320 * time.Millisecond)
	cancel()
	<-done

	snaps := decodeLines(t, buf.String())
	if len(snaps) < 2 {
		t.Fatalf("got %d snapshots, want at least 2 (initial + teardown): %s", len(snaps), buf.String())
	}
	if n := len(snaps[len(snaps)-1].Features); n != 0 {
		t.Errorf("final snapshot still has %d features, want 0 (teardown not emitted): %s", n, buf.String())
	}
}

func TestRunDoesNotReEmitWhenNothingChanged(t *testing.T) {
	// Waking repeatedly with an unchanged world must stay quiet, otherwise
	// a busy turn's event burst would spam the consumer with identical
	// snapshots.
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		return agent.Health{Reachable: true, State: agent.StateBusy}
	})

	ctx, cancel := context.WithCancel(context.Background())
	var buf lockedBuffer
	done := make(chan struct{})
	go func() { _ = r.Run(ctx, &buf); close(done) }()

	time.Sleep(50 * time.Millisecond)
	for i := 0; i < 5; i++ {
		r.wake()
		time.Sleep(30 * time.Millisecond)
	}
	cancel()
	<-done

	if n := len(decodeLines(t, buf.String())); n != 1 {
		t.Errorf("got %d snapshots, want only the initial one: %s", n, buf.String())
	}
}

func TestRefreshPreservesSinceAcrossRepeatedBusyEvents(t *testing.T) {
	// End-to-end version of the Build-level guarantee: opencode emits
	// several identical "busy" statuses per turn, and the gauge must not
	// reset each time.
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		return agent.Health{Reachable: true, State: agent.StateBusy}
	})

	ctx := context.Background()
	first, _ := r.refresh(ctx)
	time.Sleep(20 * time.Millisecond)
	second, changed := r.refresh(ctx)

	if changed {
		t.Error("second refresh reported a change with an unchanged world")
	}
	if !first.Features[0].Since.Equal(second.Features[0].Since) {
		t.Errorf("Since moved: %v -> %v", first.Features[0].Since, second.Features[0].Since)
	}
}

func TestRefreshDetectsChange(t *testing.T) {
	var mu sync.Mutex
	st := agent.StateIdle
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		mu.Lock()
		defer mu.Unlock()
		return agent.Health{Reachable: true, State: st}
	})

	ctx := context.Background()
	if _, changed := r.refresh(ctx); !changed {
		t.Error("first refresh should report a change (nothing to nothing is still new)")
	}
	mu.Lock()
	st = agent.StateWaiting
	mu.Unlock()
	if _, changed := r.refresh(ctx); !changed {
		t.Error("refresh after a real state change should report changed")
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.Debounce <= 0 || o.Rescan <= 0 || o.Safety <= 0 {
		t.Errorf("defaults not applied: %+v", o)
	}
}

// lockedBuffer is a bytes.Buffer safe for the runner goroutine to write
// while the test reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestRefreshEmitsWhenOnlyTodoProgressChanged(t *testing.T) {
	// Progress within an unchanged status still matters: the agent stays
	// "working" from task 5 to task 6, but a progress gauge has to move.
	var mu sync.Mutex
	done := 1
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		mu.Lock()
		defer mu.Unlock()
		return agent.Health{
			Reachable: true, State: agent.StateBusy,
			Todos: agent.Todos{Total: 3, Completed: done, InProgress: 1, Current: "step"},
		}
	})

	ctx := context.Background()
	if _, changed := r.refresh(ctx); !changed {
		t.Fatal("first refresh should report changed")
	}
	if _, changed := r.refresh(ctx); changed {
		t.Fatal("identical refresh should not report changed")
	}
	mu.Lock()
	done = 2
	mu.Unlock()
	if _, changed := r.refresh(ctx); !changed {
		t.Error("todo progress moved but no snapshot would be emitted")
	}
}

func TestRefreshEmitsWhenTheRunningToolChanges(t *testing.T) {
	// Moving from one command to the next is exactly the kind of progress a
	// widget should show, even though the status stays "working".
	var mu sync.Mutex
	title := "bin/rails test"
	f := feat("alpha", "main")
	r := testRunner(t, []*state.Feature{f}, func(context.Context, string, agent.Conn) agent.Health {
		mu.Lock()
		defer mu.Unlock()
		return agent.Health{
			Reachable: true, State: agent.StateBusy,
			Activity: &agent.Activity{Tool: "bash", Title: title},
		}
	})

	ctx := context.Background()
	r.refresh(ctx)
	if _, changed := r.refresh(ctx); changed {
		t.Fatal("identical refresh should not report changed")
	}
	mu.Lock()
	title = "bin/rubocop"
	mu.Unlock()
	if _, changed := r.refresh(ctx); !changed {
		t.Error("the running command changed but no snapshot would be emitted")
	}
}
