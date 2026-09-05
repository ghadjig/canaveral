package tmpl

import (
	"strings"
	"testing"
)

func vars() Vars {
	ports := map[string]int{"web": 3001, "vite": 5174}
	return Vars{
		Project:  "norules",
		Feature:  "small-fixes",
		Slot:     1,
		Branch:   "small-fixes",
		Worktree: "/wt/small-fixes",
		Root:     "/p/norules",
		Port:     ports,
		URL:      URLsFor(ports),
		Agent: map[string]AgentRef{
			"main":     {URL: "http://127.0.0.1:4096"},
			"reviewer": AgentRef{URL: "http://127.0.0.1:4097"}.WithSession("--session abc"),
		},
		DBSuffix: "_small_fixes",
	}
}

func TestRenderPortsAndURLs(t *testing.T) {
	cases := map[string]string{
		"bin/rails server -p {{.Port.web}}":                               "bin/rails server -p 3001",
		"{{.URL.web}}/up":                                                 "http://localhost:3001/up",
		"localhost:{{.Port.vite}}":                                        "localhost:5174",
		"opencode attach {{.Agent.main}}":                                 "opencode attach http://127.0.0.1:4096",
		"opencode attach {{.Agent.reviewer}} {{.Agent.reviewer.Session}}": "opencode attach http://127.0.0.1:4097 --session abc",
		// Fork is the former spelling of Session and must keep rendering
		// the same thing, or every manifest written before the rename
		// silently stops resuming anything.
		"opencode attach {{.Agent.reviewer}} {{.Agent.reviewer.Fork}}": "opencode attach http://127.0.0.1:4097 --session abc",
		"attach {{.Agent.main}} {{.Agent.main.Session}}":               "attach http://127.0.0.1:4096 ",
		"cd {{.Worktree}}":          "cd /wt/small-fixes",
		"{{.Project}}/{{.Feature}}": "norules/small-fixes",
		"db{{.DBSuffix}}":           "db_small_fixes",
		"no placeholders here":      "no placeholders here",
	}
	for in, want := range cases {
		got, err := Render("t", in, vars())
		if err != nil {
			t.Errorf("Render(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Render(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderUndefinedPortFailsLoudly(t *testing.T) {
	// A typo must not silently produce "http://localhost:/up".
	_, err := Render("service.web.ready.http", "http://localhost:{{.Port.wbe}}/up", vars())
	if err == nil {
		t.Fatal("Render succeeded with an undefined port, want error")
	}
	// The error must name both the field and the bad key so it is actionable.
	if !strings.Contains(err.Error(), "service.web.ready.http") || !strings.Contains(err.Error(), "wbe") {
		t.Errorf("error should name field and key: %v", err)
	}
}

func TestRenderUnknownFieldErrors(t *testing.T) {
	if _, err := Render("t", "{{.Nope}}", vars()); err == nil {
		t.Error("unknown field: want error")
	}
}

func TestRenderBadTemplateErrors(t *testing.T) {
	if _, err := Render("t", "{{", vars()); err == nil {
		t.Error("bad template: want error")
	}
}

// TestRenderLiteralBraces pins the only way to get a literal "{{" past the
// renderer, which VERSIONS.md tells people to use when [env] carries a
// placeholder meant for some other tool.
//
// It works because missingkey=error is consulted in exactly one place — when
// a field name is resolved against a map — and a string constant never gets
// there. That is a property of text/template rather than of this package, so
// it is pinned here: nothing else would notice if a Go release changed it,
// and the failure mode is a feature that refuses to open.
func TestRenderLiteralBraces(t *testing.T) {
	cases := map[string]string{
		`{{"{{"}}`:                   "{{",
		`{{"{{"}}.Port.web{{"}}"}}`:  "{{.Port.web}}",
		`prefix {{"{{"}}x{{"}}"}}`:   "prefix {{x}}",
		`{{"{{"}} and {{.Port.web}}`: "{{ and 3001",
	}
	for in, want := range cases {
		got, err := Render("env.SOME_TEMPLATE", in, vars())
		if err != nil {
			t.Errorf("Render(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("Render(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderMap(t *testing.T) {
	got, err := RenderMap("service.web.env", map[string]string{
		"PORT":     "{{.Port.web}}",
		"BASE_URL": "{{.URL.web}}",
		"PLAIN":    "value",
	}, vars())
	if err != nil {
		t.Fatalf("RenderMap: %v", err)
	}
	want := map[string]string{"PORT": "3001", "BASE_URL": "http://localhost:3001", "PLAIN": "value"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("RenderMap[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestRenderMapPropagatesError(t *testing.T) {
	if _, err := RenderMap("env", map[string]string{"A": "{{.Nope}}"}, vars()); err == nil {
		t.Error("RenderMap should propagate template errors")
	}
}

func TestRenderMapEmpty(t *testing.T) {
	got, err := RenderMap("env", nil, vars())
	if err != nil || got != nil {
		t.Errorf("RenderMap(nil) = %v, %v", got, err)
	}
}

func TestURLsFor(t *testing.T) {
	got := URLsFor(map[string]int{"web": 3000})
	if got["web"] != "http://localhost:3000" {
		t.Errorf("URLsFor = %v", got)
	}
	if len(URLsFor(nil)) != 0 {
		t.Error("URLsFor(nil) should be empty")
	}
}
