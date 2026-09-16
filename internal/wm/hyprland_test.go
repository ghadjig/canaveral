package wm

import (
	"encoding/json"
	"math"
	"testing"
)

func TestParseRealHyprctlClients(t *testing.T) {
	// Shape captured from hyprctl clients -j on Hyprland 0.50.1.
	raw := `[{"address":"0x55d1","class":"Alacritty","initialClass":"canaveral-p-f-terminal",
	  "title":"t","initialTitle":"f · terminal","pid":12345,
	  "workspace":{"id":5,"name":"norules:small-fixes"},"grouped":["0x55d1","0x55d2"]}]`
	var cs []hyprClient
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	c := cs[0].client()
	if c.InitialClass != "canaveral-p-f-terminal" {
		t.Errorf("InitialClass = %q", c.InitialClass)
	}
	if c.Class != "Alacritty" {
		t.Errorf("Class = %q", c.Class)
	}
	if c.Workspace.Name != "norules:small-fixes" || c.Workspace.ID != 5 {
		t.Errorf("Workspace = %+v", c.Workspace)
	}
	if c.PID != 12345 || c.Address != "0x55d1" {
		t.Errorf("client = %+v", c)
	}
}

func TestWorkspaceArgKeepsNumericAndNamedApart(t *testing.T) {
	cases := []struct{ name, want string }{
		{"4", "4"},
		{"0", "0"},
		{"-3", "-3"},
		{"norules:small-fixes", "name:norules:small-fixes"},
		{"3d", "name:3d"}, // starts with a digit but is not one
		{"3d-printing", "name:3d-printing"},
		{"", "name:"},
	}
	for _, c := range cases {
		if got := workspaceArg(c.name); got != c.want {
			t.Errorf("workspaceArg(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// The fractions a user writes in [layout] are shares of the whole width;
// dwindle's ratios are shares of whatever is still undivided at each split.
// Getting that conversion wrong produces a layout that looks plausible and is
// wrong by more the further right you look.
//
// These exact numbers (0.8, 0.6667, 1.0) were verified against a real dwindle
// workspace: applying them to four chained windows produced columns of
// 39.75%/19.8%/19.8%/19.7% of the monitor width — a 40/20/20/20 split within
// rounding. An even split at any point in the chain is ratio 1.0, which is
// confirmed to mean "50/50" despite not obviously saying so.
func TestSplitRatioChainRenormalisesAtEachSplit(t *testing.T) {
	got := splitRatioChain([]float64{0.4, 0.2, 0.2, 0.2})
	want := []float64{0.8, 0.6667, 1.0}
	if len(got) != len(want) {
		t.Fatalf("got %d ratios, want %d (one per split point, none for the last window)", len(got), len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 0.001 {
			t.Errorf("ratio[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// Every ratio has to land inside dwindle's usable range whatever the
// fractions were, since a rejected splitratio leaves a half-built layout
// behind rather than failing before anything moved.
func TestSplitRatioChainStaysInDwindlesRange(t *testing.T) {
	for _, fractions := range [][]float64{
		{0.5, 0.5},
		// Validation allows [layout.default] to sum to 1.0 only within a
		// tolerance, so a slightly-off partition has to work too.
		{0.34, 0.2, 0.2, 0.2},
		{0.7, 0.2, 0.1},
		{0.25, 0.25, 0.25, 0.25},
		{0.98, 0.01, 0.01},
		// Hand-edited nonsense that validation would have caught, reaching
		// here anyway through a state file nobody validated.
		{0, 1},
		{-1, 2},
	} {
		for i, r := range splitRatioChain(fractions) {
			if r < 0.1 || r > 1.9 {
				t.Errorf("fractions %v: ratio[%d] = %v, outside dwindle's (0,2)", fractions, i, r)
			}
		}
	}
}

func TestSplitRatioChainIsEmptyForASingleColumn(t *testing.T) {
	if got := splitRatioChain([]float64{1}); len(got) != 0 {
		t.Errorf("splitRatioChain = %v, want no splits for one column", got)
	}
	if got := splitRatioChain(nil); got != nil {
		t.Errorf("splitRatioChain(nil) = %v, want nil", got)
	}
}
