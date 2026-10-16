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

// Capture works out which windows on a space's workspace its definition does
// not already account for, and describes each as a [[window]] that would
// bring it back.
//
// Every captured window becomes an exec window with a match_class, terminals
// included. Rewriting a terminal as `run = ""` would read better, but such a
// window is recognised by the class canaveral gives it when spawning, which
// the terminal already open here does not carry — so the very next open would
// spawn a second terminal beside the one that was just saved. match_class
// adopts the window that is already there.
//
// argv is hypr.Argv outside tests. A window whose process cannot be read is
// returned in skipped rather than guessed at.
func Capture(m *manifest.Manifest, name string, clients []hypr.Client, argv func(pid int) ([]string, bool)) (windows []manifest.Window, skipped []string) {
	var here []hypr.Client
	for _, c := range clients {
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
			skipped = append(skipped, describe(c))
			continue
		}
		wname := uniqueName(windowName(class), taken)
		taken[wname] = true
		windows = append(windows, manifest.Window{
			Name: wname,
			Exec: shellJoin(args),
			// Case-insensitive because the same application reports
			// "Google-chrome" under XWayland and "google-chrome" natively,
			// and anchored so "code" does not adopt "code-insiders".
			MatchClass: "(?i)^" + regexp.QuoteMeta(class) + "$",
		})
	}
	return windows, skipped
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

// Append adds windows to a space's definition and returns its path.
//
// Appended rather than re-encoded, because the definition is a file you have
// written: re-encoding it would drop every comment in it, the starter's
// included. The result is checked with the same loader that opens a space
// before it replaces the original, so a save can never leave behind a
// definition that no longer opens.
func Append(name string, windows []manifest.Window) (string, error) {
	p, err := Path(name)
	if err != nil {
		return "", err
	}
	old, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s: %w", name, ErrNotFound)
		}
		return "", err
	}
	if len(windows) == 0 {
		return p, nil
	}
	if m, err := manifest.LoadSpace(p, name); err == nil && m.Layout.Enabled() {
		return "", fmt.Errorf("%s: %w; add the new windows to it by hand", p, ErrLayout)
	}

	var buf bytes.Buffer
	buf.Write(old)
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteString("\n# Saved from the open workspace by `canaveral save`.\n")
	saved := struct {
		Window []savedWindow `toml:"window"`
	}{}
	for _, w := range windows {
		saved.Window = append(saved.Window, savedWindow{Name: w.Name, Exec: w.Exec, MatchClass: w.MatchClass})
	}
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(saved); err != nil {
		return "", fmt.Errorf("encode windows: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(p), "."+name+"-*.toml")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if _, err := manifest.LoadSpace(tmp.Name(), name); err != nil {
		return "", fmt.Errorf("saved definition would not load, left %s unchanged: %w", p, err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return "", err
	}
	return p, nil
}

// ErrLayout is returned when a definition has a [layout] that new windows
// would have to be added to by hand: the layout must name every window, and
// appending cannot edit a table the file already has.
var ErrLayout = errors.New("the definition has a [layout], which must list every window")
