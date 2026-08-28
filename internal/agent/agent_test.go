package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeServer serves the two endpoints Probe depends on.
func fakeServer(t *testing.T, sessions, messages string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(messages))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const twoDirSessions = `{"data":[
  {"id":"ses_mine","location":{"directory":"/w/mine"},"time":{"updated":200}},
  {"id":"ses_old","location":{"directory":"/w/mine"},"time":{"updated":100}},
  {"id":"ses_other","location":{"directory":"/w/other"},"time":{"updated":300}}
]}`

// fakeServerFull additionally serves /session/status and the per-session
// permission list, for tests that exercise State classification.
func fakeServerFull(t *testing.T, sessions, messages, status, permissions string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/api/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/permission") {
			_, _ = w.Write([]byte(permissions))
			return
		}
		_, _ = w.Write([]byte(messages))
	})
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(status))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestProbeStateBusy(t *testing.T) {
	msgs := `{"data":[{"type":"assistant","time":{"created":1},"model":{"id":"m1"}}]}`
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `{"data":[]}`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateBusy {
		t.Errorf("State = %q, want busy", h.State)
	}
}

func TestProbeStateIdle(t *testing.T) {
	msgs := `{"data":[{"type":"assistant","time":{"created":1,"completed":2},"finish":"stop","model":{"id":"m1"}}]}`
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `{"data":[]}`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle", h.State)
	}
}

func TestProbeStateWaitingOnPermission(t *testing.T) {
	// Idle from opencode's own point of view, but there is an unanswered
	// permission request — this must classify as waiting, not idle.
	msgs := `{"data":[{"type":"assistant","time":{"created":1,"completed":2},"finish":"stop","model":{"id":"m1"}}]}`
	perms := `{"data":[{"id":"perm_1","type":"bash"}]}`
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, perms)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting", h.State)
	}
}

func TestProbeStateRetrying(t *testing.T) {
	msgs := `{"data":[{"type":"assistant","time":{"created":1},"model":{"id":"m1"}}]}`
	status := `{"ses_mine":{"type":"retry","attempt":1,"message":"rate limited","next":5}}`
	srv := fakeServerFull(t, twoDirSessions, msgs, status, `{"data":[]}`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateRetrying {
		t.Errorf("State = %q, want retrying", h.State)
	}
}

func TestProbeWorkedSumsCompletedTurnsOnly(t *testing.T) {
	// Two completed turns (1s and 2s) plus one still in flight, which must
	// contribute to Working, not Worked.
	msgs := `{"data":[
      {"type":"assistant","time":{"created":5000},"model":{"id":"m1"}},
      {"type":"assistant","time":{"created":2000,"completed":4000},"finish":"stop","model":{"id":"m1"}},
      {"type":"assistant","time":{"created":0,"completed":1000},"finish":"stop","model":{"id":"m1"}}
    ]}`
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `{"data":[]}`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if want := 3 * time.Second; h.Worked != want {
		t.Errorf("Worked = %v, want %v", h.Worked, want)
	}
	if h.Working <= 0 {
		t.Errorf("Working = %v, want > 0 for the in-flight turn", h.Working)
	}
}

func TestProbeFiltersByDirectory(t *testing.T) {
	// The server exposes the user's whole global history; only sessions rooted
	// in this agent's directory may be counted.
	msgs := `{"data":[{"type":"assistant","time":{"created":1,"completed":2},"finish":"stop","tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1,"write":0}},"cost":0.25,"model":{"id":"m1"}}]}`
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if !h.Reachable {
		t.Fatalf("not reachable: %v", h.Err)
	}
	if h.Sessions != 2 {
		t.Errorf("Sessions = %d, want 2 (other directory excluded)", h.Sessions)
	}
	if h.Busy {
		t.Error("Busy = true, want false for a completed message")
	}
	if h.Model != "m1" {
		t.Errorf("Model = %q, want m1", h.Model)
	}
	if got := h.Tokens.Total(); got != 16 {
		t.Errorf("Tokens.Total = %d, want 16", got)
	}
	if h.Cost != 0.25 {
		t.Errorf("Cost = %v, want 0.25", h.Cost)
	}
}

func TestProbeBusyWhenNotCompleted(t *testing.T) {
	// A missing time.completed is the signal that generation is in flight.
	msgs := `{"data":[{"type":"assistant","time":{"created":1},"model":{"id":"m1"}}]}`
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if !h.Busy {
		t.Error("Busy = false, want true when time.completed is absent")
	}
}

func TestProbeSurfacesError(t *testing.T) {
	msgs := `{"data":[{"type":"assistant","time":{"created":1,"completed":2},"finish":"error","error":{"message":"HTTP 401 bad model"},"model":{"id":"m1"}}]}`
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.LastError != "HTTP 401 bad model" {
		t.Errorf("LastError = %q, want the provider message", h.LastError)
	}
}

func TestProbeSumsAssistantMessagesOnly(t *testing.T) {
	// Token totals live on assistant messages; user messages carry none and the
	// session list reports zero.
	msgs := `{"data":[
      {"type":"assistant","time":{"created":3,"completed":4},"finish":"stop","tokens":{"input":10,"output":10,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.1,"model":{"id":"m1"}},
      {"type":"user","time":{"created":2}},
      {"type":"assistant","time":{"created":1,"completed":2},"finish":"tool-calls","tokens":{"input":5,"output":5,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0.05,"model":{"id":"m1"}}
    ]}`
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if got := h.Tokens.Total(); got != 30 {
		t.Errorf("Tokens.Total = %d, want 30", got)
	}
	if h.Cost < 0.149 || h.Cost > 0.151 {
		t.Errorf("Cost = %v, want ~0.15", h.Cost)
	}
	// finish=="tool-calls" on an older message must not be reported as an error.
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
}

func TestProbeNoSessionsForDirectory(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, `{"data":[]}`)
	h := Probe(context.Background(), srv.URL, "/w/nothing-here")
	if !h.Reachable {
		t.Fatal("want reachable")
	}
	if h.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", h.Sessions)
	}
}

func TestProbeUnreachable(t *testing.T) {
	h := Probe(context.Background(), "http://127.0.0.1:1", "/w")
	if h.Reachable {
		t.Error("Reachable = true for a dead endpoint")
	}
	if h.Err == nil {
		t.Error("Err = nil, want a connection error")
	}
}

func TestProbeEmptyURL(t *testing.T) {
	if h := Probe(context.Background(), "", "/w"); h.Err == nil {
		t.Error("empty URL should report an error")
	}
}

func TestListenReMatchesOpencodeOutput(t *testing.T) {
	log := "Warning: OPENCODE_SERVER_PASSWORD is not set; server is unsecured.\n" +
		"opencode server listening on http://127.0.0.1:39777\n"
	m := listenRe.FindStringSubmatch(log)
	if m == nil {
		t.Fatal("listenRe did not match real opencode output")
	}
	if m[1] != "http://127.0.0.1:39777" {
		t.Errorf("url = %q", m[1])
	}
}

func TestServeCmdQuotesPath(t *testing.T) {
	got := ServeCmd("/home/a b/opencode")
	want := "exec '/home/a b/opencode' serve --hostname 127.0.0.1 --port 0"
	if got != want {
		t.Errorf("ServeCmd = %q, want %q", got, want)
	}
}
