package wm

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// swayColumnPasses is how many measure-and-resize rounds applyColumns will
// run before giving up on the widths it was asked for.
//
// The first pass is exact in theory and lands within a percentage point in
// practice: sway's `resize set ... ppt` takes an integer, and it measures
// against the parent's width while distributing against the children's, which
// differ by whatever gaps are configured. A second pass, starting from what
// the first actually produced, absorbs both. A third is there for the case
// where a window hit its minimum size and the compositor refused a resize
// outright, so that the error below reports a real failure rather than
// rounding.
const swayColumnPasses = 3

// swayColumnTolerance is how far a column may end up from the width it asked
// for before that counts as a failure, as a fraction of the workspace.
// A little over one percentage point: below that the integer ppt argument
// cannot express the difference in the first place.
const swayColumnTolerance = 0.015

// applyColumns arranges already-mapped windows into a horizontal row of the
// requested widths.
//
// sway has no equivalent of dwindle's recursive split ratios; a row is a flat
// set of siblings under a splith parent, and widths are set individually. So
// the work is to get the windows into that shape — tiled, siblings, in the
// declared order — and then to solve for the resize arguments that land on
// the requested fractions.
func (s swaywm) applyColumns(ctx context.Context, addresses []string, fractions []float64) error {
	ids, err := swayConIDs(addresses)
	if err != nil {
		return err
	}
	if err := s.swayFlattenRow(ctx, ids); err != nil {
		return err
	}
	if err := s.swayOrderRow(ctx, ids); err != nil {
		return err
	}
	return s.swaySetWidths(ctx, ids, normalise(fractions))
}

func swayConIDs(addresses []string) ([]int, error) {
	ids := make([]int, 0, len(addresses))
	seen := map[int]bool{}
	for _, addr := range addresses {
		id, err := strconv.Atoi(addr)
		if err != nil || id <= 0 || seen[id] {
			return nil, fmt.Errorf("invalid or duplicate column address %q", addr)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

// swayFlattenRow tiles any floating column window and forces their container
// into a horizontal layout, so that what follows has a row to work with.
func (s swaywm) swayFlattenRow(ctx context.Context, ids []int) error {
	root, err := s.tree(ctx)
	if err != nil {
		return err
	}
	places := swayPlaces(root)
	workspace := ""
	var commands []string
	for _, id := range ids {
		p, ok := places[id]
		if !ok {
			return fmt.Errorf("column window %d disappeared", id)
		}
		if workspace == "" {
			workspace = p.workspace
		}
		if p.workspace == "" || p.workspace != workspace {
			return fmt.Errorf("column windows must share a workspace")
		}
		if p.floating {
			commands = append(commands, fmt.Sprintf("[con_id=%d] floating disable", id))
		}
	}
	// Applied to the first column's parent, which is the row. A workspace
	// left tabbed or stacked by the user's config would otherwise swallow
	// every column behind a single title bar.
	commands = append(commands, fmt.Sprintf("[con_id=%d] layout splith", ids[0]))
	return swayRun(ctx, commands...)
}

// swayRow is the state of a row of columns as sway currently has it.
type swayRow struct {
	// widths are the pixel widths of every tiled child of the row's parent,
	// ours and anyone else's.
	widths []int
	// parentWidth is what sway measures a `ppt` argument against.
	parentWidth int
	// slots holds, for each of our windows in tree order, its index among
	// those children.
	slots []int
	// ours maps a container id to its position in slots.
	ours map[int]int
}

// swayReadRow measures the row the given windows form, and fails if they are
// not siblings — which is the one shape this cannot repair, since sway has no
// command that lifts a window out of a nested split without focusing it.
func (s swaywm) swayReadRow(ctx context.Context, ids []int) (swayRow, error) {
	root, err := s.tree(ctx)
	if err != nil {
		return swayRow{}, err
	}
	places := swayPlaces(root)
	parentID := 0
	for i, id := range ids {
		p, ok := places[id]
		if !ok {
			return swayRow{}, fmt.Errorf("column window %d disappeared", id)
		}
		if p.floating {
			return swayRow{}, fmt.Errorf("column window %d is still floating", id)
		}
		if i == 0 {
			parentID = p.parentID
		} else if p.parentID != parentID {
			return swayRow{}, fmt.Errorf("column windows are not siblings; close them and reopen the feature to rebuild the layout")
		}
	}
	parent, ok := swayFindNode(root, parentID)
	if !ok {
		return swayRow{}, fmt.Errorf("cannot locate the container holding the columns")
	}

	row := swayRow{parentWidth: parent.Rect.Width, ours: map[int]int{}}
	wanted := map[int]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	for i, child := range parent.Nodes {
		row.widths = append(row.widths, child.Rect.Width)
		if wanted[child.ID] {
			row.ours[child.ID] = len(row.slots)
			row.slots = append(row.slots, i)
		}
	}
	if len(row.slots) != len(ids) {
		return swayRow{}, fmt.Errorf("cannot locate every column among its siblings")
	}
	return row, nil
}

// swayOrderRow puts the columns into the declared left-to-right order.
//
// Swapping is used rather than `move left`/`move right`, which at the edge of
// a workspace sends the window to the next output instead of doing nothing.
// Only the slots our own windows already occupy are touched, so a window the
// user opened on the same workspace keeps its place in the row.
func (s swaywm) swayOrderRow(ctx context.Context, ids []int) error {
	row, err := s.swayReadRow(ctx, ids)
	if err != nil {
		return err
	}
	// current[i] is the id presently in the i'th slot, in tree order.
	current := make([]int, len(ids))
	for id, slot := range row.ours {
		current[slot] = id
	}
	at := map[int]int{}
	for slot, id := range current {
		at[id] = slot
	}

	var commands []string
	for i, want := range ids {
		if current[i] == want {
			continue
		}
		j := at[want]
		commands = append(commands, fmt.Sprintf("[con_id=%d] swap container with con_id %d", current[i], want))
		at[current[i]], at[want] = j, i
		current[i], current[j] = current[j], current[i]
	}
	if len(commands) == 0 {
		return nil
	}
	return swayRun(ctx, commands...)
}

// swaySetWidths resizes the columns to the requested fractions.
func (s swaywm) swaySetWidths(ctx context.Context, ids []int, fractions []float64) error {
	var last []float64
	for pass := 0; pass < swayColumnPasses; pass++ {
		row, err := s.swayReadRow(ctx, ids)
		if err != nil {
			return err
		}
		commands, reached, err := swayResizeCommands(row, ids, fractions)
		if err != nil {
			return err
		}
		last = reached
		if swayColumnsClose(reached, fractions) {
			return nil
		}
		if err := swayRun(ctx, commands...); err != nil {
			return err
		}
	}
	// Measured after the final pass rather than predicted, so the message
	// reports what the user is actually looking at.
	row, err := s.swayReadRow(ctx, ids)
	if err != nil {
		return err
	}
	if _, reached, err := swayResizeCommands(row, ids, fractions); err == nil {
		last = reached
	}
	if swayColumnsClose(last, fractions) {
		return nil
	}
	return fmt.Errorf("column widths settled at %s instead of %s; a window may have hit its minimum size",
		formatFractions(last), formatFractions(fractions))
}

func swayColumnsClose(reached, want []float64) bool {
	if len(reached) != len(want) {
		return false
	}
	for i := range want {
		if math.Abs(reached[i]-want[i]) > swayColumnTolerance {
			return false
		}
	}
	return true
}

func formatFractions(fs []float64) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = fmt.Sprintf("%.0f%%", f*100)
	}
	return strings.Join(parts, "/")
}

// swayResizeCommands solves for the `resize set` arguments that land the
// columns on the requested fractions, and reports the fractions they
// currently occupy.
//
// `resize set width N ppt` does not simply assign a width. sway computes the
// delta from the container's current width and then subtracts an *equal*
// share of it from every sibling — including the ones an earlier command in
// the same request already placed (container_resize_tiled in
// sway/commands/resize.c). Issuing each column's own fraction in turn
// therefore leaves every column but the last one wrong.
//
// It is, however, linear, so it can be solved rather than iterated at. With m
// siblings in the row and a delta d applied to one of them, that one gains d
// and each of the other m-1 loses d/(m-1). Asking for the deltas that leave
// our n columns exactly where we want them, and that between them cancel out
// (so the siblings we were not asked about keep their widths), gives
//
//	d_k = (target_k - current_k) * (m-1)/m
//
// applied in any order, each command's argument being the container's width
// at that moment plus its delta. The loop below walks that forward, so the
// numbers handed to sway account for what the preceding commands did.
func swayResizeCommands(row swayRow, ids []int, fractions []float64) ([]string, []float64, error) {
	m := len(row.widths)
	if m < 2 {
		return nil, nil, fmt.Errorf("column windows have no siblings to resize against")
	}
	total := 0
	for _, w := range row.widths {
		total += w
	}
	if total <= 0 {
		return nil, nil, fmt.Errorf("sway reported a row of zero width")
	}
	parentWidth := row.parentWidth
	if parentWidth <= 0 {
		parentWidth = total
	}

	// Fractions of the space the children share, which is what sway's own
	// width_fraction is relative to.
	current := make([]float64, m)
	for i, w := range row.widths {
		current[i] = float64(w) / float64(total)
	}
	reached := make([]float64, len(ids))
	// The columns keep whatever share of the row they already hold between
	// them; only their proportions are ours to set. When they are the whole
	// row — the ordinary case — that share is 1 and the fractions are taken
	// literally. When it is not, a window the user opened here keeps its
	// width instead of being squeezed out by fractions that were never
	// written with it in mind.
	share := 0.0
	for i, id := range ids {
		reached[i] = current[row.slots[row.ours[id]]]
		share += reached[i]
	}

	target := make([]float64, len(ids))
	delta := make([]float64, len(ids))
	for i := range ids {
		target[i] = fractions[i] * share
		delta[i] = (target[i] - reached[i]) * float64(m-1) / float64(m)
	}

	var commands []string
	for i, id := range ids {
		slot := row.slots[row.ours[id]]
		ppt := int(math.Round((current[slot] + delta[i]) * float64(total) / float64(parentWidth) * 100))
		// sway refuses a resize that would take a sibling below its minimum
		// width, and silently does nothing at all for a 0 or negative
		// argument. Neither is worth asking for.
		if ppt < 1 {
			ppt = 1
		}
		if ppt > 99 {
			ppt = 99
		}
		commands = append(commands, fmt.Sprintf("[con_id=%d] resize set width %d ppt", id, ppt))

		// Mirror sway's own bookkeeping so the next command's argument is
		// computed against the width this one will have produced.
		applied := float64(ppt) * float64(parentWidth) / float64(total) / 100
		d := applied - current[slot]
		for j := range current {
			if j == slot {
				current[j] = applied
			} else {
				current[j] -= d / float64(m-1)
			}
		}
	}
	// Report what the columns occupy now, relative to each other, so the
	// caller can tell a settled layout from one still drifting.
	if share > 0 {
		for i := range reached {
			reached[i] /= share
		}
	}
	return commands, reached, nil
}
