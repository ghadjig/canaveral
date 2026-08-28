// Package tmpl renders per-feature placeholders in manifest strings.
//
// Manifest values are written once but must resolve differently for every
// feature, because each feature gets its own worktree and its own ports. A
// service declared as
//
//	ready.http = "http://localhost:{{.Port.web}}/up"
//
// resolves to :3000 for the first feature and :3001 for the next.
package tmpl

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"text/template"
)

// Vars is the context available to every manifest template.
type Vars struct {
	// Project is the manifest name, e.g. "norules".
	Project string
	// Feature is the slugged feature name, e.g. "small-fixes".
	Feature string
	// Slot is the feature's stable index, used for port offsets.
	Slot int
	// Branch is the feature's git branch.
	Branch string
	// Worktree is the feature's checkout directory.
	Worktree string
	// Root is the project's main checkout.
	Root string
	// Port maps logical service names to this feature's allocated ports.
	Port map[string]int
	// URL maps the same names to http://localhost:<port>.
	URL map[string]string
	// Agent maps agent names to their opencode server URLs. Populated only
	// after agents have started. {{.Agent.main}} renders as the URL;
	// {{.Agent.main.Fork}} renders extra flags to splice into the same
	// window's own attach command (see AgentRef).
	Agent map[string]AgentRef
	// DBSuffix is the per-feature database suffix, empty when shared.
	DBSuffix string
	// Class is the window class canaveral assigns. GUI applications must be
	// told to adopt it so the window can be recognised later.
	Class string
	// Profile is a private per-window state directory, which a browser needs in
	// order to start its own process rather than handing the request to a
	// running instance.
	Profile string
}

// AgentRef is an agent's template value: a plain string (its opencode server
// URL) by default, with an additional named field for advanced use.
//
// {{.Agent.main}} renders as the URL — String() is what text/template calls
// to print a non-basic value directly — while {{.Agent.main.Fork}} accesses
// Fork specifically.
type AgentRef struct {
	URL string
	// Fork is "--session <id> --fork", when a namespace sibling has a more
	// recently active session for this agent name to hand off, or "" when
	// there is nothing to fork from. Meant to be spliced into the window's
	// own attach command, e.g.
	// `run = "opencode attach {{.Agent.main}} {{.Agent.main.Fork}}"`.
	Fork string
}

func (a AgentRef) String() string { return a.URL }

// URLsFor builds the URL map from a port map.
func URLsFor(ports map[string]int) map[string]string {
	out := make(map[string]string, len(ports))
	for name, p := range ports {
		out[name] = fmt.Sprintf("http://localhost:%d", p)
	}
	return out
}

// Render evaluates a single template string.
//
// missingkey=error means a typo such as {{.Port.wbe}} fails loudly at startup
// instead of rendering an empty string and producing a broken URL.
func Render(what, s string, v Vars) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New(what).Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("%s: parse template: %w", what, err)
	}
	var b bytes.Buffer
	if err := t.Execute(&b, v); err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	return b.String(), nil
}

// RenderMap renders every value of a string map, leaving keys untouched.
func RenderMap(what string, m map[string]string, v Vars) (map[string]string, error) {
	if len(m) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(m))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r, err := Render(what+"."+k, m[k], v)
		if err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, nil
}
