package manifest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadDefaults(t *testing.T) {
	dir := write(t, t.TempDir(), `
[[agent]]
name = "main"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Name != filepath.Base(dir) {
		t.Errorf("Name = %q, want %q", m.Name, filepath.Base(dir))
	}
	if m.Agents[0].Tool != "opencode" {
		t.Errorf("Tool = %q, want opencode", m.Agents[0].Tool)
	}
	if m.Branch == "" {
		t.Error("Branch default not applied")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]string{
		"unknown key":          "nme = \"typo\"\n",
		"duplicate service":    "[[service]]\nname=\"a\"\ncmd=\"x\"\n[[service]]\nname=\"a\"\ncmd=\"y\"\n",
		"duplicate agent":      "[[agent]]\nname=\"a\"\n[[agent]]\nname=\"a\"\n",
		"service without cmd":  "[[service]]\nname=\"a\"\n",
		"bad isolation":        "isolation = \"nope\"\n",
		"unsupported tool":     "[[agent]]\nname=\"a\"\ntool=\"claude\"\n",
		"bad name":             "name = \"has space\"\n",
		"bad duration":         "[[service]]\nname=\"a\"\ncmd=\"x\"\nready.timeout=\"soon\"\n",
		"exec without class":   "[[window]]\nname=\"w\"\nexec=\"chrome\"\n",
		"run with profile":     "[[window]]\nname=\"w\"\nrun=\"\"\nprofile_source=\"~/.config/x\"\n",
		"profile without seed": "[[window]]\nname=\"w\"\nexec=\"c --class={{.Class}}\"\nprofile_source=\"~/.config/x\"\n",

		"discover cmd and port together": "[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.cmd=\"./ports\"\ndiscover.port.web='(\\d+)'\n",
		"discover pattern will not compile": "[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.port.web='port (\\d+'\n",
		"discover pattern without capture group": "[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.port.web='listening'\n",
		"discover pattern with two capture groups": "[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.port.web='(\\w+) (\\d+)'\n",
		"discover port also in [ports]": "[ports]\nweb = 3000\n[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.port.web='port (\\d+)'\n",
		"discover port with invalid name": "[[service]]\nname=\"a\"\ncmd=\"x\"\n" +
			"discover.port.'has space'='port (\\d+)'\n",
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			dir := write(t, t.TempDir(), body)
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load(%q) succeeded, want error", body)
			}
		})
	}
}

func TestDiscoverAccepted(t *testing.T) {
	dir := write(t, t.TempDir(), `
[ports]
admin = 4000

[[service]]
name = "devspace"
cmd  = "bin/devspace dev-branch"
discover.port.web     = 'Port mappings: (\d+):3000'
discover.port.webpack = 'Port mappings:.*, (\d+):3808'
discover.timeout      = "90s"
ready.http = "{{.URL.web}}/up"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := m.Services[0].Discover
	if !d.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	if got := d.Timeout.Or(time.Minute); got != 90*time.Second {
		t.Errorf("Timeout = %s, want 90s", got)
	}
	want := []string{"web", "webpack"}
	got := d.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Names()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDiscoverCmdHasNoDeclaredNames(t *testing.T) {
	// Under discover.cmd the names are whatever it prints, so there is
	// nothing to check against [ports] up front — and nothing to report.
	dir := write(t, t.TempDir(), `
[[service]]
name = "web"
cmd  = "run"
discover.cmd = "./ports"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := m.Services[0].Discover
	if !d.Enabled() {
		t.Error("Enabled() = false, want true")
	}
	if n := d.Names(); n != nil {
		t.Errorf("Names() = %v, want nil", n)
	}
}

func TestDiscoverAbsentIsDisabled(t *testing.T) {
	dir := write(t, t.TempDir(), "[[service]]\nname=\"web\"\ncmd=\"run\"\n")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Services[0].Discover.Enabled() {
		t.Error("Enabled() = true for a service with no discover block")
	}
}

func TestReadyKind(t *testing.T) {
	cases := []struct {
		r    Ready
		want string
	}{
		{Ready{}, ""},
		{Ready{HTTP: "http://x"}, "http"},
		{Ready{TCP: "h:1"}, "tcp"},
		{Ready{Log: "ready"}, "log"},
		{Ready{Cmd: "true"}, "cmd"},
	}
	for _, c := range cases {
		if got := c.r.Kind(); got != c.want {
			t.Errorf("Kind() = %q, want %q", got, c.want)
		}
	}
}

func TestDurationParsing(t *testing.T) {
	dir := write(t, t.TempDir(), `
[[service]]
name = "a"
cmd = "x"
ready.tcp = "localhost:1"
ready.timeout = "90s"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.Services[0].Ready.Timeout.Or(time.Second); got != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", got)
	}
	var empty Duration
	if got := empty.Or(5 * time.Second); got != 5*time.Second {
		t.Errorf("Or fallback = %v, want 5s", got)
	}
}

func TestFindWalksUp(t *testing.T) {
	root := t.TempDir()
	write(t, root, "name = \"x\"\n")
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := Find(nested)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	// t.TempDir may be a symlink (/tmp -> /private/tmp); compare resolved paths.
	wantResolved, _ := filepath.EvalSymlinks(root)
	gotResolved, _ := filepath.EvalSymlinks(got)
	if gotResolved != wantResolved {
		t.Errorf("Find = %q, want %q", gotResolved, wantResolved)
	}
}

func TestFindMissing(t *testing.T) {
	// Cleared explicitly: Find falls back to $CANAVERAL_ROOT, and every
	// process canaveral spawns has it set. Inheriting it from the environment
	// made this test fail whenever it was run from inside a feature worktree
	// — which is how this project is normally developed.
	t.Setenv("CANAVERAL_ROOT", "")

	if _, err := Find(t.TempDir()); err == nil {
		t.Fatal("Find succeeded in empty dir, want error")
	}
}

func TestFindFallsBackToCanaveralRoot(t *testing.T) {
	// The fallback is what makes commands work inside a feature worktree,
	// where the untracked canaveral.toml is absent and the upward walk finds
	// nothing.
	root := t.TempDir()
	write(t, root, "name = \"x\"\n")
	t.Setenv("CANAVERAL_ROOT", root)

	got, err := Find(t.TempDir())
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if got != root {
		t.Errorf("Find = %q, want %q", got, root)
	}

	// A CANAVERAL_ROOT that holds no manifest is not a project, so the error
	// stands rather than the path being returned on faith.
	t.Setenv("CANAVERAL_ROOT", t.TempDir())
	if _, err := Find(t.TempDir()); err == nil {
		t.Error("Find succeeded with a manifest-less CANAVERAL_ROOT, want error")
	}
}

func TestResolveDir(t *testing.T) {
	cases := []struct{ base, dir, want string }{
		{"/w", "", "/w"},
		{"/w", ".", "/w"},
		{"/w", "api", "/w/api"},
		{"/w", "../sibling", "/sibling"},
		{"/w", "/abs", "/abs"},
	}
	for _, c := range cases {
		if got := ResolveDir(c.base, c.dir); got != c.want {
			t.Errorf("ResolveDir(%q,%q) = %q, want %q", c.base, c.dir, got, c.want)
		}
	}
}

func TestMergeEnvLaterWins(t *testing.T) {
	got := MergeEnv(
		map[string]string{"A": "1", "B": "1"},
		map[string]string{"B": "2"},
		nil,
		map[string]string{"C": "3"},
	)
	want := map[string]string{"A": "1", "B": "2", "C": "3"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("MergeEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("MergeEnv len = %d, want %d", len(got), len(want))
	}
}

func TestWindowRunVsExec(t *testing.T) {
	dir := write(t, t.TempDir(), `
[[window]]
name = "terminal"
run  = ""

[[window]]
name = "editor"
run  = "opencode attach {{.Agent.main}}"

[[window]]
name = "chrome"
exec = "google-chrome --class={{.Class}} {{.URL.web}}"
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// run = "" must still mean "terminal", which is why Run is a *string.
	if !m.Windows[0].IsTerminal() || m.Windows[0].Command() != "" {
		t.Errorf("empty run should be a plain terminal: %+v", m.Windows[0])
	}
	if !m.Windows[1].IsTerminal() {
		t.Error("run should imply terminal")
	}
	if m.Windows[2].IsTerminal() {
		t.Error("exec should not be a terminal")
	}
	if m.Windows[2].Command() != "google-chrome --class={{.Class}} {{.URL.web}}" {
		t.Errorf("Command = %q", m.Windows[2].Command())
	}
}

func TestDatabaseDefaults(t *testing.T) {
	dir := write(t, t.TempDir(), "name = \"x\"\n")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Database.Isolation != DBShared {
		t.Errorf("Isolation = %q, want shared default", m.Database.Isolation)
	}
	if m.Database.SuffixEnv != "" {
		t.Errorf("SuffixEnv should stay empty when shared, got %q", m.Database.SuffixEnv)
	}
}

func TestDatabaseSuffixDefaultsEnvName(t *testing.T) {
	dir := write(t, t.TempDir(), "[database]\nisolation = \"suffix\"\n")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Database.SuffixEnv != "DB_SUFFIX" {
		t.Errorf("SuffixEnv = %q, want DB_SUFFIX", m.Database.SuffixEnv)
	}
}

func TestPortsParsed(t *testing.T) {
	dir := write(t, t.TempDir(), "[ports]\nweb = 3000\nvite = 5173\n")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Ports["web"] != 3000 || m.Ports["vite"] != 5173 {
		t.Errorf("Ports = %v", m.Ports)
	}
}

func TestBranchDefault(t *testing.T) {
	dir := write(t, t.TempDir(), "name = \"x\"\n")
	m, _ := Load(dir)
	if m.Branch != "{{.Feature}}" {
		t.Errorf("Branch = %q, want {{.Feature}}", m.Branch)
	}
}

func TestWindowProfileSeed(t *testing.T) {
	dir := write(t, t.TempDir(), `
[[window]]
name = "chrome"
exec = "google-chrome --class={{.Class}} {{.URL.web}}"
profile_source = "~/.config/google-chrome"
profile_seed = ["Local State", "Default/Bookmarks"]
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w := m.Windows[0]
	if w.ProfileSource != "~/.config/google-chrome" {
		t.Errorf("ProfileSource = %q", w.ProfileSource)
	}
	if len(w.ProfileSeed) != 2 || w.ProfileSeed[1] != "Default/Bookmarks" {
		t.Errorf("ProfileSeed = %v", w.ProfileSeed)
	}
}

func TestLayoutValid(t *testing.T) {
	dir := write(t, t.TempDir(), `
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
`)
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !m.Layout.Enabled() {
		t.Fatal("Enabled() = false, want true")
	}
	got := m.Layout.Fractions()
	if got["chrome"] != 0.6 || got["opencode"] != 0.4 {
		t.Errorf("Fractions() = %v", got)
	}
}

func TestLayoutRejects(t *testing.T) {
	base := `
[[window]]
name = "chrome"
exec = "google-chrome --class={{.Class}}"
[[window]]
name = "opencode"
run = ""
`
	cases := map[string]string{
		"undeclared window in order": base + "\n[layout]\norder=[\"chrome\",\"missing\"]\n[layout.default]\nchrome=0.5\nmissing=0.5\n",
		"duplicate in order":         base + "\n[layout]\norder=[\"chrome\",\"chrome\"]\n[layout.default]\nchrome=1.0\n",
		"default missing entry":      base + "\n[layout]\norder=[\"chrome\",\"opencode\"]\n[layout.default]\nchrome=1.0\n",
		"default sums wrong":         base + "\n[layout]\norder=[\"chrome\",\"opencode\"]\n[layout.default]\nchrome=0.6\nopencode=0.6\n",
		"default required":           base + "\n[layout]\norder=[\"chrome\",\"opencode\"]\n",
		"fraction out of range":      base + "\n[layout]\norder=[\"chrome\",\"opencode\"]\n[layout.default]\nchrome=1.5\nopencode=-0.5\n",
		"default without order":      base + "\n[layout]\n[layout.default]\nchrome=1.0\n",
	}
	for label, body := range cases {
		t.Run(label, func(t *testing.T) {
			dir := write(t, t.TempDir(), body)
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load(%s) succeeded, want error", label)
			}
		})
	}
}

func TestLayoutDisabledByDefault(t *testing.T) {
	dir := write(t, t.TempDir(), "name = \"x\"\n")
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Layout.Enabled() {
		t.Error("Enabled() should be false when [layout] is absent")
	}
}

func TestLayoutRejectsUnknownKeys(t *testing.T) {
	// [layout.current] was once written back automatically after a resize.
	// It is gone, and a manifest still carrying one should say so loudly
	// rather than silently ignoring geometry the user expects to be used.
	dir := write(t, t.TempDir(), `
[[window]]
name = "chrome"
exec = "google-chrome --class={{.Class}}"
[[window]]
name = "opencode"
run = ""

[layout]
order = ["chrome", "opencode"]
[layout.default]
chrome = 0.5
opencode = 0.5
[layout.current]
chrome = 0.62
opencode = 0.4
`)
	if _, err := Load(dir); err == nil {
		t.Fatal("Load should reject a leftover [layout.current]")
	}
}

func TestWorktreeRootDefaultsToWorktreesBesideTheProject(t *testing.T) {
	m := &Manifest{Root: "/p/norules"}
	got, err := m.WorktreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/p/norules/worktrees" {
		t.Errorf("WorktreeRoot = %q, want worktrees beside the project by default", got)
	}
}

func TestWorktreeRootStateOptsIntoCanaveralsStateDir(t *testing.T) {
	m := &Manifest{Root: "/p/norules"}
	m.Worktree.Root = "state"
	got, err := m.WorktreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("WorktreeRoot = %q, want empty (canaveral's state dir)", got)
	}
}

func TestWorktreeRootRelativeIsAgainstTheProject(t *testing.T) {
	// Relative must not depend on the working directory, or the setting
	// would mean different things from different shells.
	m := &Manifest{Root: "/p/norules"}
	m.Worktree.Root = "worktrees"
	got, err := m.WorktreeRoot()
	if err != nil {
		t.Fatal(err)
	}
	if got != "/p/norules/worktrees" {
		t.Errorf("WorktreeRoot = %q", got)
	}
}

func TestWorktreeRootAbsoluteAndTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	m := &Manifest{Root: "/p/norules"}

	m.Worktree.Root = "/srv/wt"
	if got, _ := m.WorktreeRoot(); got != "/srv/wt" {
		t.Errorf("absolute = %q", got)
	}
	m.Worktree.Root = "~/dev/wt"
	if got, _ := m.WorktreeRoot(); got != filepath.Join(home, "dev/wt") {
		t.Errorf("tilde = %q", got)
	}
}
