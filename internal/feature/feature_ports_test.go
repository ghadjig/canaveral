package feature

import (
	"context"
	"os"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
)

// TestMain makes every port look free unless a test says otherwise, so no
// test's slot depends on what this machine happens to be listening on.
func TestMain(m *testing.M) {
	portBusy = func(int) bool { return false }
	os.Exit(m.Run())
}

func busyOnly(t *testing.T, ports ...int) {
	t.Helper()
	held := map[int]bool{}
	for _, p := range ports {
		held[p] = true
	}
	prev := portBusy
	portBusy = func(p int) bool { return held[p] }
	t.Cleanup(func() { portBusy = prev })
}

// A slot no feature holds can still have its ports taken by something
// outside canaveral; norules got 3007 while a devspace port-forward had it,
// and every open then died on Rails' "Address already in use".
func TestEnsureRecordSkipsSlotsWithBusyPorts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CANAVERAL_ROOT", "")
	t.Setenv("CANAVERAL_FEATURE", "")
	busyOnly(t, 3000, 3001)

	m := &manifest.Manifest{Name: "norules", Root: t.TempDir(), Branch: "{{.Feature}}",
		Ports: map[string]int{"web": 3000}}
	f, created, err := ensureRecord(context.Background(), m, "fresh")
	if err != nil {
		t.Fatalf("ensureRecord: %v", err)
	}
	if !created {
		t.Fatal("new feature not reported as created")
	}
	if f.Slot != 2 || f.Ports["web"] != 3002 {
		t.Errorf("slot=%d ports=%v, want slot 2 on 3002", f.Slot, f.Ports)
	}
}

func TestRelocateBusySlotMovesAStoppedFeature(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	busyOnly(t, 3007)

	m := &manifest.Manifest{Name: "norules", Ports: map[string]int{"web": 3000}}
	other := &state.Feature{Project: "norules", Name: "other", Slot: 0}
	f := &state.Feature{Project: "norules", Name: "stuck", Slot: 7, Ports: map[string]int{"web": 3007}}
	for _, s := range []*state.Feature{other, f} {
		if err := state.Save(s); err != nil {
			t.Fatal(err)
		}
	}

	if err := relocateBusySlot(context.Background(), m, f, quietReporter{}); err != nil {
		t.Fatalf("relocateBusySlot: %v", err)
	}
	if f.Slot != 1 || f.Ports["web"] != 3001 {
		t.Errorf("slot=%d ports=%v, want slot 1 on 3001", f.Slot, f.Ports)
	}
}

func TestRelocateBusySlotLeavesFreePortsAlone(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	busyOnly(t)

	m := &manifest.Manifest{Name: "norules", Ports: map[string]int{"web": 3000}}
	f := &state.Feature{Project: "norules", Name: "fine", Slot: 4, Ports: map[string]int{"web": 3004}}
	if err := relocateBusySlot(context.Background(), m, f, quietReporter{}); err != nil {
		t.Fatalf("relocateBusySlot: %v", err)
	}
	if f.Slot != 4 {
		t.Errorf("slot = %d, want 4: nothing holds its ports", f.Slot)
	}
}
