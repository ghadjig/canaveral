package wm

import (
	"context"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// swaySim is sway's own tiled-resize arithmetic, transcribed from
// container_resize_tiled and resize_set_tiled in sway/commands/resize.c.
//
// It exists because the solver in swayResizeCommands is written against that
// arithmetic rather than against the obvious reading of "set this container's
// width": sway takes the delta from *every* sibling in equal shares,
// including ones an earlier command in the same request already placed. A
// test that asserted the commands looked reasonable would have passed for the
// naive version too, which produces the wrong layout for three columns or
// more. Gaps are zero here, which is the case the compositor's own ppt
// arithmetic is exact for.
type swaySim struct {
	widths []int
	total  int
}

func newSwaySim(total, n int) *swaySim {
	s := &swaySim{total: total, widths: make([]int, n)}
	for i := range s.widths {
		s.widths[i] = total / n
	}
	s.widths[0] += total - sum(s.widths)
	return s
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}

// resizeSet applies `resize set width <ppt> ppt` to the container at index.
func (s *swaySim) resizeSet(index, ppt int) {
	m := len(s.widths)
	if m < 2 {
		return
	}
	amount := s.total*ppt/100 - s.widths[index]
	siblingAmount := int(math.Ceil(float64(amount) / float64(m-1)))
	for i := range s.widths {
		change := -siblingAmount
		if i == index {
			change = amount
		}
		// sway refuses the whole resize rather than part of it.
		if s.widths[i]+change < 100 {
			return
		}
	}
	fractions := make([]float64, m)
	for i, w := range s.widths {
		fractions[i] = float64(w) / float64(s.total)
	}
	amountFraction := float64(amount) / float64(s.total)
	for i := range fractions {
		if i == index {
			fractions[i] += amountFraction
		} else {
			fractions[i] -= amountFraction / float64(m-1)
		}
	}
	for i, f := range fractions {
		s.widths[i] = int(math.Round(f * float64(s.total)))
	}
}

// row describes the simulated state the way swayReadRow would report it,
// with our columns occupying the first len(ids) slots.
func (s *swaySim) row(n int) swayRow {
	r := swayRow{widths: append([]int{}, s.widths...), parentWidth: s.total, ours: map[int]int{}}
	for i := 0; i < n; i++ {
		r.ours[swaySimID(i)] = i
		r.slots = append(r.slots, i)
	}
	return r
}

func swaySimID(i int) int { return 10 + i }

var swayResizeRe = regexp.MustCompile(`^\[con_id=(\d+)\] resize set width (\d+) ppt$`)

// apply runs the commands through the simulator, checking their syntax on the
// way past: a command sway cannot parse is a command sway silently ignores.
func (s *swaySim) apply(t *testing.T, commands []string) {
	t.Helper()
	for _, c := range commands {
		m := swayResizeRe.FindStringSubmatch(c)
		if m == nil {
			t.Fatalf("command %q is not a resize sway would accept", c)
		}
		id, _ := strconv.Atoi(m[1])
		ppt, _ := strconv.Atoi(m[2])
		s.resizeSet(id-10, ppt)
	}
}

func TestSwayResizeCommandsLandOnTheRequestedWidths(t *testing.T) {
	cases := [][]float64{
		{0.5, 0.5},
		{0.7, 0.3},
		{1.0 / 3, 1.0 / 3, 1.0 / 3},
		{0.45, 0.32, 0.23},
		{0.6, 0.2, 0.2},
		{0.25, 0.25, 0.25, 0.25},
		{0.4, 0.3, 0.2, 0.1},
	}
	for _, fractions := range cases {
		t.Run(fmt.Sprint(fractions), func(t *testing.T) {
			n := len(fractions)
			sim := newSwaySim(3840, n)
			ids := make([]int, n)
			for i := range ids {
				ids[i] = swaySimID(i)
			}

			commands, _, err := swayResizeCommands(sim.row(n), ids, fractions)
			if err != nil {
				t.Fatalf("swayResizeCommands: %v", err)
			}
			sim.apply(t, commands)

			// One pass, from equal columns, must already be inside the
			// tolerance the caller stops at — otherwise applyColumns spends
			// three round trips on every layout it builds.
			for i, want := range fractions {
				got := float64(sim.widths[i]) / float64(sum(sim.widths))
				if math.Abs(got-want) > swayColumnTolerance {
					t.Errorf("column %d = %.4f after one pass, want %.4f (widths %v)", i, got, want, sim.widths)
				}
			}
		})
	}
}

// The naive implementation — hand each column its own fraction in turn — is
// exact only when the columns it has not reached yet happen to be equal to
// each other, which is why two even columns work and a real layout does not:
// each command drags the ones before it back by an equal share of its own
// delta. This pins that difference down, so that the solver cannot quietly be
// replaced by the obvious thing. Should it ever fail, sway no longer
// redistributes onto already-placed siblings and swayResizeCommands can go
// back to being a loop over the fractions.
func TestSwayResizeCommandsAreNotJustTheFractions(t *testing.T) {
	want := []float64{0.4, 0.3, 0.2, 0.1}
	sim := newSwaySim(4000, 4)
	sim.apply(t, []string{
		"[con_id=10] resize set width 40 ppt",
		"[con_id=11] resize set width 30 ppt",
		"[con_id=12] resize set width 20 ppt",
		"[con_id=13] resize set width 10 ppt",
	})
	worst, at := 0.0, 0
	for i := range want {
		if d := math.Abs(float64(sim.widths[i])/float64(sum(sim.widths)) - want[i]); d > worst {
			worst, at = d, i
		}
	}
	if worst <= swayColumnTolerance {
		t.Errorf("issuing the fractions directly was within %.3f of %v — the solver is no longer needed", worst, want)
	}
	t.Logf("issuing the fractions directly misses by %.3f at column %d: %v", worst, at, sim.widths)
}

// A window the user opened on the feature's workspace is not canaveral's to
// resize. The columns share out whatever width they already hold between
// them, and the stray keeps its own.
func TestSwayResizeCommandsLeaveAStrayWindowAlone(t *testing.T) {
	// Three columns holding 900px between them, plus a 300px window nobody
	// asked about, in a 1200px row.
	row := swayRow{
		widths:      []int{300, 300, 300, 300},
		parentWidth: 1200,
		slots:       []int{0, 1, 2},
		ours:        map[int]int{10: 0, 11: 1, 12: 2},
	}
	sim := &swaySim{widths: append([]int{}, row.widths...), total: 1200}
	commands, _, err := swayResizeCommands(row, []int{10, 11, 12}, []float64{0.5, 0.3, 0.2})
	if err != nil {
		t.Fatalf("swayResizeCommands: %v", err)
	}
	sim.apply(t, commands)

	ourTotal := sim.widths[0] + sim.widths[1] + sim.widths[2]
	for i, want := range []float64{0.5, 0.3, 0.2} {
		if got := float64(sim.widths[i]) / float64(ourTotal); math.Abs(got-want) > 0.02 {
			t.Errorf("column %d = %.3f of the columns, want %.3f (widths %v)", i, got, want, sim.widths)
		}
	}
	if math.Abs(float64(sim.widths[3])-300) > 24 {
		t.Errorf("the stray window went from 300px to %dpx", sim.widths[3])
	}
}

func TestSwayResizeCommandsRejectARowItCannotWorkOn(t *testing.T) {
	ids := []int{10, 11}
	fractions := []float64{0.5, 0.5}
	t.Run("no siblings", func(t *testing.T) {
		row := swayRow{widths: []int{100}, parentWidth: 100, slots: []int{0}, ours: map[int]int{10: 0}}
		if _, _, err := swayResizeCommands(row, ids[:1], fractions[:1]); err == nil {
			t.Error("a lone container cannot be resized against anything")
		}
	})
	t.Run("zero width", func(t *testing.T) {
		row := swayRow{widths: []int{0, 0}, parentWidth: 0, slots: []int{0, 1}, ours: map[int]int{10: 0, 11: 1}}
		if _, _, err := swayResizeCommands(row, ids, fractions); err == nil {
			t.Error("a row of zero width should be reported, not divided by")
		}
	})
}

// swayColumnTree renders a workspace holding windows of the given widths, in
// the given order, as `swaymsg -t get_tree` would.
func swayColumnTree(ids []int, widths []int) string {
	var nodes []string
	x := 0
	for i, id := range ids {
		nodes = append(nodes, fmt.Sprintf(
			`{"id":%d,"type":"con","name":"w%d","pid":%d,"app_id":"canaveral-p-f-w%d","rect":{"x":%d,"y":0,"width":%d,"height":1000}}`,
			id, i, 100+i, i, x, widths[i]))
		x += widths[i]
	}
	return fmt.Sprintf(`{"id":1,"type":"root","name":"root","nodes":[
	  {"id":2,"type":"output","name":"DP-1","rect":{"x":0,"y":0,"width":%d,"height":1000},"nodes":[
	    {"id":3,"type":"workspace","name":"p:f","num":-1,"layout":"splith",
	     "rect":{"x":0,"y":0,"width":%d,"height":1000},"nodes":[%s]}]}]}`,
		x, x, strings.Join(nodes, ","))
}

// The ordinary case: windows spawned into a fresh workspace in [layout]
// order, already the width they were asked to be. Nothing should be resized,
// and nothing should be swapped — a layout that rebuilds itself on every
// reconcile would be a layout the user cannot adjust.
func TestSwayApplyColumnsSettlesWithoutChurn(t *testing.T) {
	log := installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_TREE", swayColumnTree([]int{10, 11, 12}, []int{450, 320, 230}))
	err := ApplyColumns(context.Background(), []string{"10", "11", "12"}, []float64{0.45, 0.32, 0.23})
	if err != nil {
		t.Fatalf("ApplyColumns: %v", err)
	}
	got := swayCommands(t, log)
	// Only the one that costs nothing to repeat: forcing the row horizontal,
	// in case the user's config left the workspace tabbed or stacked.
	if len(got) != 1 || got[0] != "[con_id=10] layout splith" {
		t.Errorf("sent %v, want just the layout command", got)
	}
}

func TestSwayApplyColumnsOrdersAndTilesFirst(t *testing.T) {
	log := installFakeSwaymsg(t)
	// The windows mapped in the wrong order, and the middle one is floating.
	tree := swayColumnTree([]int{12, 10, 11}, []int{330, 330, 340})
	tree = strings.Replace(tree,
		`{"id":3,"type":"workspace","name":"p:f","num":-1,"layout":"splith",`,
		`{"id":3,"type":"workspace","name":"p:f","num":-1,"layout":"splith","floating_nodes":[{"id":13,"type":"floating_con","name":"stray","pid":9,"app_id":"other","rect":{"x":0,"y":0,"width":100,"height":100}}],`,
		1)
	t.Setenv("FAKE_SWAY_TREE", tree)

	// Deliberately already at the requested widths, so the run stops after
	// ordering and the assertion is about the swaps alone.
	err := ApplyColumns(context.Background(), []string{"10", "11", "12"}, []float64{0.33, 0.33, 0.34})
	if err != nil {
		t.Fatalf("ApplyColumns: %v", err)
	}
	got := swayCommands(t, log)
	if len(got) != 2 {
		t.Fatalf("sent %v, want a layout request then a swap request", got)
	}
	// `swap` rather than `move left`: at the edge of a workspace sway's
	// `move left` hands the window to the next output instead of doing
	// nothing, which would throw a column onto another screen.
	want := "[con_id=12] swap container with con_id 10; [con_id=12] swap container with con_id 11"
	if got[1] != want {
		t.Errorf("swaps = %q,\nwant     %q", got[1], want)
	}
	if strings.Contains(got[1], "move left") || strings.Contains(got[1], "move right") {
		t.Error("ordering must not use directional moves")
	}
}

// Nested splits are the one shape this cannot repair without focusing
// something, so it says so instead of half-applying a layout.
func TestSwayApplyColumnsRefusesWindowsThatAreNotSiblings(t *testing.T) {
	installFakeSwaymsg(t)
	t.Setenv("FAKE_SWAY_TREE", `{"id":1,"type":"root","name":"root","nodes":[
	  {"id":2,"type":"output","name":"DP-1","rect":{"x":0,"y":0,"width":1000,"height":1000},"nodes":[
	    {"id":3,"type":"workspace","name":"p:f","num":-1,"layout":"splith","rect":{"x":0,"y":0,"width":1000,"height":1000},"nodes":[
	      {"id":10,"type":"con","name":"a","pid":1,"app_id":"a","rect":{"x":0,"y":0,"width":500,"height":1000}},
	      {"id":20,"type":"con","name":null,"layout":"splitv","rect":{"x":500,"y":0,"width":500,"height":1000},"nodes":[
	        {"id":11,"type":"con","name":"b","pid":2,"app_id":"b","rect":{"x":500,"y":0,"width":500,"height":500}}]}]}]}]}`)
	err := ApplyColumns(context.Background(), []string{"10", "11"}, []float64{0.5, 0.5})
	if err == nil || !strings.Contains(err.Error(), "not siblings") {
		t.Fatalf("err = %v, want it to say the windows are not siblings", err)
	}
}

func TestSwayApplyColumnsRejectsBadAddresses(t *testing.T) {
	log := installFakeSwaymsg(t)
	for _, addresses := range [][]string{
		{"10", "10"},                 // the same window twice
		{"10", "0x7f"},               // a Hyprland address in a sway state file
		{"10", "11; exec something"}, // an injection attempt
		{"10", "-1"},                 // not a container id
	} {
		if err := ApplyColumns(context.Background(), addresses, []float64{0.5, 0.5}); err == nil {
			t.Errorf("addresses %v should be refused", addresses)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Error("a rejected address must not reach sway at all")
	}
}

// A layout that cannot be produced — a window at its minimum size, most
// likely — has to say so. Reporting success would leave the user looking at
// an arrangement they did not ask for with nothing to explain it.
func TestSwayApplyColumnsReportsWidthsItCouldNotReach(t *testing.T) {
	installFakeSwaymsg(t)
	// The fake never changes its tree, so no number of passes will move it.
	t.Setenv("FAKE_SWAY_TREE", swayColumnTree([]int{10, 11}, []int{500, 500}))
	err := ApplyColumns(context.Background(), []string{"10", "11"}, []float64{0.8, 0.2})
	if err == nil {
		t.Fatal("ApplyColumns should report a layout it could not produce")
	}
	if !strings.Contains(err.Error(), "50%/50%") || !strings.Contains(err.Error(), "80%/20%") {
		t.Errorf("err = %v, want it to name what it got and what it wanted", err)
	}
}
