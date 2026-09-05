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
	// Agent maps agent names to their servers. Populated only after agents
	// have started. {{.Agent.main}} renders as the URL, which is empty for a
	// harness that has no server; {{.Agent.main.Session}} renders extra
	// flags to splice into the same window's own command (see AgentRef).
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

// AgentRef is an agent's template value: a plain string (its server URL, for
// a harness that has one) by default, with additional named fields for
// advanced use.
//
// {{.Agent.main}} renders as the URL — String() is what text/template calls
// to print a non-basic value directly — while {{.Agent.main.Session}}
// accesses Session specifically.
type AgentRef struct {
	URL string
	// Session is the flags that make this agent open an existing
	// conversation rather than start a new one, and "" when it should not.
	// The spelling is the harness's own — "--session <id>" for opencode,
	// "--resume <id>" for Claude Code — so a manifest window splices it in
	// without knowing which tool it got:
	//
	//	run = "opencode attach {{.Agent.main}} {{.Agent.main.Session}}"
	//	run = "claude {{.Agent.main.Session}}"
	//
	// Two situations fill it, and they are mutually exclusive because they
	// concern opposite halves of a feature's life. A freshly created feature
	// under a namespace gets a fork of whichever sibling's conversation is
	// most recent, so the agent does not start from zero on work its
	// neighbours already did. A feature restored by `canaveral pop` gets its
	// own conversation back, exactly the one it was in when stashed.
	Session string
	// Fork is the former name of Session and carries the same value. Kept so
	// manifests written against `{{.Agent.main.Fork}}` keep working; new ones
	// should say Session, which is what the flag has always actually done —
	// the fork itself happens in canaveral, before the window is ever
	// rendered, and what lands here is only ever a session to open.
	Fork string
}

// WithSession returns a copy of the ref carrying flags (as rendered by
// agent.Harness.SessionFlag), or the ref unchanged when they are empty.
// Setting both spellings in one place is what keeps Session and its Fork
// alias from drifting apart.
func (a AgentRef) WithSession(flags string) AgentRef {
	if flags == "" {
		return a
	}
	a.Session = flags
	a.Fork = flags
	return a
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
