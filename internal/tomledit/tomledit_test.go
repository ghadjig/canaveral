package tomledit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bandito/canaveral/internal/manifest"
)

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "canaveral.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplaceExistingTablePreservesEverythingElse(t *testing.T) {
	p := writeFile(t, `# top comment
name = "norules"

[worktree]
link = ["node_modules"] # a comment that must survive

[layout]
order = ["chrome", "opencode"]

[layout.default]
chrome = 0.6
opencode = 0.4

[layout.current]
chrome = 0.5
opencode = 0.5

[[agent]]
name = "main"
`)
	err := ReplaceTable(p, "layout.current",
		[]string{"chrome", "opencode"},
		map[string]float64{"chrome": 0.7, "opencode": 0.3},
		"layout.default")
	if err != nil {
		t.Fatalf("ReplaceTable: %v", err)
	}

	got := read(t, p)
	for _, want := range []string{
		"# top comment",
		`name = "norules"`,
		`link = ["node_modules"] # a comment that must survive`,
		`order = ["chrome", "opencode"]`,
		"chrome = 0.6", // default section untouched
		"opencode = 0.4",
		`[[agent]]`,
		`name = "main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing unrelated content %q\n--- got ---\n%s", want, got)
		}
	}
	if !strings.Contains(got, "chrome = 0.7") || !strings.Contains(got, "opencode = 0.3") {
		t.Errorf("current section was not updated:\n%s", got)
	}
	// The stale values must actually be gone, not just appended alongside.
	if strings.Contains(got, "chrome = 0.5") {
		t.Errorf("stale current value still present:\n%s", got)
	}
}

func TestReplaceInsertsTableWhenAbsent(t *testing.T) {
	p := writeFile(t, `name = "norules"

[layout]
order = ["chrome", "opencode"]

[layout.default]
chrome = 0.6
opencode = 0.4

[[agent]]
name = "main"
`)
	err := ReplaceTable(p, "layout.current",
		[]string{"chrome", "opencode"},
		map[string]float64{"chrome": 0.55, "opencode": 0.45},
		"layout.default")
	if err != nil {
		t.Fatalf("ReplaceTable: %v", err)
	}

	got := read(t, p)
	if !strings.Contains(got, "[layout.current]") {
		t.Fatalf("new table was not inserted:\n%s", got)
	}
	if !strings.Contains(got, "chrome = 0.55") {
		t.Errorf("new table missing values:\n%s", got)
	}
	// It must land after [layout.default] and before [[agent]], not at EOF
	// past unrelated later sections.
	defaultIdx := strings.Index(got, "[layout.default]")
	currentIdx := strings.Index(got, "[layout.current]")
	agentIdx := strings.Index(got, "[[agent]]")
	if !(defaultIdx < currentIdx && currentIdx < agentIdx) {
		t.Errorf("table inserted in wrong position:\n%s", got)
	}
}

func TestReplaceInsertsAtEndOfFileWithNoAnchor(t *testing.T) {
	p := writeFile(t, `name = "solo"
`)
	err := ReplaceTable(p, "layout.current", []string{"a"}, map[string]float64{"a": 1}, "layout.default")
	if err != nil {
		t.Fatalf("ReplaceTable: %v", err)
	}
	got := read(t, p)
	if !strings.Contains(got, "[layout.current]") || !strings.Contains(got, "a = 1") {
		t.Errorf("table not appended:\n%s", got)
	}
}

func TestReplaceIsIdempotent(t *testing.T) {
	p := writeFile(t, `name = "x"

[layout.default]
a = 0.5
b = 0.5
`)
	values := map[string]float64{"a": 0.3, "b": 0.7}
	if err := ReplaceTable(p, "layout.current", []string{"a", "b"}, values, "layout.default"); err != nil {
		t.Fatal(err)
	}
	first := read(t, p)
	if err := ReplaceTable(p, "layout.current", []string{"a", "b"}, values, "layout.default"); err != nil {
		t.Fatal(err)
	}
	second := read(t, p)
	if first != second {
		t.Errorf("re-applying the same values changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestReplacePreservesKeyOrder(t *testing.T) {
	p := writeFile(t, "name = \"x\"\n")
	err := ReplaceTable(p, "layout.current",
		[]string{"chrome", "opencode", "terminal", "serverlogs"},
		map[string]float64{"serverlogs": 0.2, "chrome": 0.4, "terminal": 0.2, "opencode": 0.2},
		"layout.default")
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	// Must appear in the caller's declared order, not map iteration order.
	order := []int{
		strings.Index(got, "chrome"),
		strings.Index(got, "opencode"),
		strings.Index(got, "terminal"),
		strings.Index(got, "serverlogs"),
	}
	for i := 1; i < len(order); i++ {
		if order[i-1] > order[i] {
			t.Fatalf("keys out of order:\n%s", got)
		}
	}
}

func TestFormatFloatIsMinimal(t *testing.T) {
	cases := map[float64]string{
		0.4:      "0.4",
		0.333333: "0.3333",
		1:        "1",
		0:        "0",
		0.2:      "0.2",
	}
	for in, want := range cases {
		if got := formatFloat(in); got != want {
			t.Errorf("formatFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestNoOpWhenValuesUnchanged(t *testing.T) {
	p := writeFile(t, `name = "x"

[layout.current]
a = 0.5
`)
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	beforeContent := read(t, p)

	if err := ReplaceTable(p, "layout.current", []string{"a"}, map[string]float64{"a": 0.5}, "layout.default"); err != nil {
		t.Fatal(err)
	}
	if read(t, p) != beforeContent {
		t.Error("content changed even though values were identical")
	}
	_ = before
}

func TestArrayOfTablesHeaderStopsBodyScan(t *testing.T) {
	// Regression test: [[agent]] (an array-of-tables header) must count as a
	// section boundary even though it never matches a plain "[table]" name,
	// otherwise replacing an earlier table swallows everything after it.
	p := writeFile(t, `[layout.current]
chrome = 0.5

[[service]]
name = "web"

[[agent]]
name = "main"
`)
	if err := ReplaceTable(p, "layout.current", []string{"chrome"}, map[string]float64{"chrome": 0.6}, ""); err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	for _, want := range []string{`[[service]]`, `name = "web"`, `[[agent]]`, `name = "main"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("array-of-tables content lost:\n%s", got)
		}
	}
	if !strings.Contains(got, "chrome = 0.6") {
		t.Errorf("value not updated:\n%s", got)
	}
}

func TestEditedFileStaysValidManifest(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "canaveral.toml")
	body := `name = "norules"

[[window]]
name = "chrome"
exec = "google-chrome --class={{.Class}}"

[[window]]
name = "opencode"
run = ""

[layout]
order = ["chrome", "opencode"]

[layout.default]
chrome = 0.6
opencode = 0.4
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceTable(p, "layout.current", []string{"chrome", "opencode"},
		map[string]float64{"chrome": 0.65, "opencode": 0.35}, "layout.default"); err != nil {
		t.Fatal(err)
	}

	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatalf("edited file no longer parses as a valid manifest: %v\n%s", err, read(t, p))
	}
	got := m.Layout.Fractions()
	if got["chrome"] != 0.65 || got["opencode"] != 0.35 {
		t.Errorf("Fractions() = %v, want the updated current values", got)
	}
}

func TestInsertedTableHasBlankLineOnBothSides(t *testing.T) {
	// Regression test: the file must read like it was hand-formatted, with a
	// blank line separating the new table from whatever follows it, not just
	// from whatever precedes it.
	p := writeFile(t, `[layout.default]
chrome = 0.4

[[window]]
name = "opencode"
`)
	err := ReplaceTable(p, "layout.current", []string{"chrome"}, map[string]float64{"chrome": 0.5}, "layout.default")
	if err != nil {
		t.Fatal(err)
	}
	got := read(t, p)
	want := "[layout.default]\nchrome = 0.4\n\n[layout.current]\nchrome = 0.5\n\n[[window]]\nname = \"opencode\"\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}
