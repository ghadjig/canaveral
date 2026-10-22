package feature

// Port availability: keeping a feature off slots whose ports something else
// on the machine already holds.

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/bandito/canaveral/internal/manifest"
	"github.com/bandito/canaveral/internal/state"
	"github.com/bandito/canaveral/internal/unit"
)

// portBusy reports whether something is already listening on port.
//
// A variable so tests can describe the machine they want rather than inherit
// whatever happens to be listening on the one they run on.
var portBusy = func(port int) bool {
	// The wildcard address, not 127.0.0.1: a listener on any address
	// conflicts with it, so this catches a loopback-only Rails server and a
	// 0.0.0.0 port-forward alike.
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return true
	}
	_ = l.Close()
	return false
}

// busyPorts lists the slot-derived ports of slot that are already in use,
// sorted by name for a stable message.
func busyPorts(m *manifest.Manifest, slot int) []string {
	ports := portsFor(m, slot)
	var out []string
	for _, name := range sortedPortNames(ports) {
		if portBusy(ports[name]) {
			out = append(out, fmt.Sprintf("%s %d", name, ports[name]))
		}
	}
	return out
}

// slotUsable is the check slot allocation applies on top of "no other feature
// has it".
func slotUsable(m *manifest.Manifest) func(int) bool {
	return func(slot int) bool { return len(busyPorts(m, slot)) == 0 }
}

// relocateBusySlot moves an existing feature to another slot when its ports
// are held by something outside it.
//
// Allocation already avoids busy ports, but only at the moment a feature is
// created; a dev server or port-forward started afterwards can still land on
// them, and the feature then fails on every open with its first service
// unable to bind. A port is stable for a feature's life because a bookmark or
// a log tail is worth keeping, but not at the price of the feature not coming
// up at all.
//
// Only while none of the feature's own services is running: those hold the
// very ports being checked, and finding them busy is then expected.
func relocateBusySlot(ctx context.Context, m *manifest.Manifest, f *state.Feature, r Reporter) error {
	if m.Space || len(m.Ports) == 0 {
		return nil
	}
	busy := busyPorts(m, f.Slot)
	if len(busy) == 0 {
		return nil
	}
	for _, s := range m.Services {
		if st, err := unit.Query(ctx, unit.Name(f.Project+"-"+f.Name, "svc", s.Name)); err == nil && st.Running() {
			return nil
		}
	}
	// No name, so the feature's own record counts as taken rather than being
	// handed straight back: its slot is the one being moved away from.
	slot, err := state.AllocateSlotWhere(m.Name, "", -1, slotUsable(m))
	if err != nil {
		return err
	}
	r.Info("port(s) %s already in use by another process; moving from slot %d to %d",
		strings.Join(busy, ", "), f.Slot, slot)
	f.Slot = slot
	f.Ports = portsFor(m, slot)
	f.MergeDiscovered()
	return nil
}
