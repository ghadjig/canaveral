package space

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/bandito/canaveral/internal/hypr"
	"github.com/bandito/canaveral/internal/manifest"
)

// Captured is what Capture found on a space's workspace.
type Captured struct {
	// Windows are the windows to add, one [[window]] each.
	Windows []manifest.Window
	// Skipped describes windows whose process could not be read, which are
	// left out rather than guessed at.
	Skipped []string
	// Shared names the captured windows whose process also draws windows
	// elsewhere — a browser, or an Electron app such as ChatGPT. What such a
	// window shows is not in its process at all: every window of the
	// application belongs to the one process, whose command line is however
	// the application was first started. The command saved for it is a way
	// to start the application, and nothing more, so it is worth reading.
	Shared []string
}

// Capture describes the windows on a space's workspace that m does not
// already declare, each as a [[window]] that would bring it back. Pass a
// manifest with no windows to capture everything on the workspace.
//
// Every captured window becomes an exec window with a match_class, terminals
// included. Rewriting a terminal as `run = ""` would read better, but such a
// window is recognised by the class canaveral gives it when spawning, which
// the terminal already open here does not carry — so the very next open would
// spawn a second terminal beside the one that was just saved. match_class
// adopts the window that is already there.
//
// argv is hypr.Argv outside tests.
func Capture(m *manifest.Manifest, name string, clients []hypr.Client, argv func(pid int) ([]string, bool)) Captured {
	var here []hypr.Client
	windowsOf := map[int]int{}
	for _, c := range clients {
		windowsOf[c.PID]++
		if c.Workspace.Name == name {
			here = append(here, c)
		}
	}

	// Each declared window claims one open window, so a second browser
	// beside a declared one still counts as new even though the declared
	// one's match_class would match it too.
	claimed := make([]bool, len(here))
	claim := func(match func(hypr.Client) bool) {
		for i, c := range here {
			if !claimed[i] && match(c) {
				claimed[i] = true
				return
			}
		}
	}
	taken := map[string]bool{}
	for _, w := range m.Windows {
		taken[w.Name] = true
		if w.MatchClass != "" {
			re, err := regexp.Compile(w.MatchClass)
			if err != nil {
				continue // the manifest already refused this; LoadSpace would not return it
			}
			claim(func(c hypr.Client) bool { return hypr.ClassMatches(re, c) })
			continue
		}
		class := hypr.Class(name, w.Name)
		claim(func(c hypr.Client) bool { return c.InitialClass == class })
	}

	var out Captured
	for i, c := range here {
		if claimed[i] {
			continue
		}
		args, ok := argv(c.PID)
		class := c.InitialClass
		if class == "" {
			class = c.Class
		}
		if !ok || class == "" {
			out.Skipped = append(out.Skipped, describe(c))
			continue
		}
		wname := uniqueName(windowName(class), taken)
		taken[wname] = true
		out.Windows = append(out.Windows, manifest.Window{
			Name: wname,
			Exec: shellJoin(unrewrite(args)),
			// Case-insensitive because the same application reports
			// "Google-chrome" under XWayland and "google-chrome" natively,
			// and anchored so "code" does not adopt "code-insiders".
			MatchClass: "(?i)^" + regexp.QuoteMeta(class) + "$",
		})
		if windowsOf[c.PID] > 1 {
			out.Shared = append(out.Shared, wname)
		}
	}
	return out
}

// unrewrite undoes a process rewriting its own command line.
//
// Chrome and its relatives overwrite argv to set their process title, which
// leaves /proc/<pid>/cmdline holding the whole command as ONE argument with
// spaces in it. Quoted back faithfully, that is a command naming a program
// called "chrome --new-window …", which does not exist. So a lone argument
// with whitespace is split back into words — unless it is itself the path of
// a file, which is what an AppImage under "My Apps/" looks like and which a
// split would break instead.
func unrewrite(args []string) []string {
	if len(args) != 1 || !strings.ContainsAny(args[0], " \t") || isFile(args[0]) {
		return args
	}
	return strings.Fields(args[0])
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

func describe(c hypr.Client) string {
	if c.Title != "" {
		return fmt.Sprintf("%s (%s)", c.Title, c.Class)
	}
	return c.Class
}

var nonName = regexp.MustCompile(`[^a-z0-9_-]+`)

// windowName turns a window class into a [[window]] name. Reverse-DNS classes
// keep only their last part, because "org.gnome.Nautilus" is a nautilus to
// anyone reading the file.
func windowName(class string) string {
	s := strings.ToLower(class)
	if i := strings.LastIndex(s, "."); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	s = strings.Trim(nonName.ReplaceAllString(s, "-"), "-_")
	if s == "" {
		return "window"
	}
	return s
}

func uniqueName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", base, i); !taken[n] {
			return n
		}
	}
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellJoin rebuilds a command line from its arguments. exec windows run
// through `sh -c`, so an argument holding a space or a quote has to be quoted
// back to exactly one argument again.
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// savedWindow is how a captured window is written. Its own type rather than
// manifest.Window so that only the keys Capture sets are written: encoding
// the manifest type would spell out every empty field.
type savedWindow struct {
	Name       string `toml:"name"`
	Exec       string `toml:"exec"`
	MatchClass string `toml:"match_class"`
}

// savedMarker heads every block of windows save writes, so the file says
// where they came from.
const savedMarker = "# Saved from the open workspace by `canaveral save`."

// Append adds windows to a space's definition and returns its path.
//
// Appended rather than re-encoded, because the definition is a file you have
// written: re-encoding it would drop every comment in it, the starter's
// included. The result is checked with the same loader that opens a space
// before it replaces the original, so a save can never leave behind a
// definition that no longer opens.
func Append(name string, windows []manifest.Window) (string, error) {
	p, old, err := readForSave(name)
	if err != nil || len(windows) == 0 {
		return p, err
	}
	m, err := manifest.LoadSpace(p, name)
	if err != nil {
		return "", err
	}
	if m.Layout.Enabled() {
		return "", fmt.Errorf("%s: %w; add the new windows to it by hand", p, ErrLayout)
	}
	body, err := withWindows(old, windows)
	if err != nil {
		return "", err
	}
	return p, writeChecked(p, name, body, len(m.Windows)+len(windows))
}

// Replace makes windows the only windows a space's definition declares, and
// returns its path.
//
// This is how a save catches up with a workspace that has changed: Append
// only ever adds, and a window it already declares — any Chrome window, once
// one Chrome window is saved — is never looked at again. Every [[window]]
// table is cut out of the file as text, and everything else in it — dir,
// ports, services, agents, and the comments around them — is kept.
func Replace(name string, windows []manifest.Window) (string, error) {
	p, old, err := readForSave(name)
	if err != nil {
		return p, err
	}
	m, err := manifest.LoadSpace(p, name)
	if err != nil {
		return "", err
	}
	if m.Layout.Enabled() {
		return "", fmt.Errorf("%s: %w; replace the windows by hand", p, ErrLayout)
	}
	body, err := withWindows(stripWindows(old), windows)
	if err != nil {
		return "", err
	}
	// The count is what proves the cut was clean: a [[window]] the text scan
	// missed would still load, and quietly survive a replace that claimed to
	// have removed it.
	return p, writeChecked(p, name, body, len(windows))
}

func readForSave(name string) (string, []byte, error) {
	p, err := Path(name)
	if err != nil {
		return "", nil, err
	}
	old, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return "", nil, err
	}
	return p, old, nil
}

// withWindows returns body with windows appended under savedMarker.
func withWindows(body []byte, windows []manifest.Window) ([]byte, error) {
	var buf bytes.Buffer
	buf.Write(body)
	if len(windows) == 0 {
		return buf.Bytes(), nil
	}
	if len(body) > 0 && !bytes.HasSuffix(body, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteString("\n" + savedMarker + "\n")
	saved := struct {
		Window []savedWindow `toml:"window"`
	}{}
	for _, w := range windows {
		saved.Window = append(saved.Window, savedWindow{Name: w.Name, Exec: w.Exec, MatchClass: w.MatchClass})
	}
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(saved); err != nil {
		return nil, fmt.Errorf("encode windows: %w", err)
	}
	return buf.Bytes(), nil
}

// stripWindows cuts every [[window]] table, and every savedMarker, out of a
// definition.
//
// A table runs from its header to the first blank line or the next header,
// whichever comes first, which is how both the starter and save lay them out.
// A multi-line array (profile_seed) is followed to its closing bracket,
// blank lines and all. Anything this misjudges is caught by the window count
// Replace checks afterwards, not trusted.
func stripWindows(body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == savedMarker {
			continue
		}
		if !isWindowHeader(t) {
			out = append(out, lines[i])
			continue
		}
		depth := 0
		for i+1 < len(lines) {
			next := strings.TrimSpace(lines[i+1])
			if depth == 0 && (next == "" || strings.HasPrefix(next, "[")) {
				break
			}
			depth += bracketDepth(next)
			i++
		}
	}
	return []byte(collapseBlank(strings.Join(out, "\n")))
}

func isWindowHeader(t string) bool {
	if i := strings.Index(t, "#"); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	return strings.ReplaceAll(t, " ", "") == "[[window]]"
}

// bracketDepth is how far a line opens (or closes) a TOML array, ignoring
// brackets inside strings and comments.
func bracketDepth(line string) int {
	d := 0
	var quote rune
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#':
			return d
		case r == '[':
			d++
		case r == ']':
			d--
		}
	}
	return d
}

var blankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlank(s string) string {
	return strings.TrimRight(blankRun.ReplaceAllString(s, "\n\n"), "\n") + "\n"
}

// writeChecked replaces p with body, but only once body loads as a space and
// declares exactly want windows.
func writeChecked(p, name string, body []byte, want int) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+name+"-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	m, err := manifest.LoadSpace(tmp.Name(), name)
	if err != nil {
		return fmt.Errorf("saved definition would not load, left %s unchanged: %w", p, err)
	}
	if len(m.Windows) != want {
		return fmt.Errorf("saved definition would declare %d windows, not %d; left %s unchanged — edit it by hand", len(m.Windows), want, p)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// ErrLayout is returned when a definition has a [layout] that new windows
// would have to be added to by hand: the layout must name every window, and
// appending cannot edit a table the file already has.
var ErrLayout = errors.New("the definition has a [layout], which must list every window")
