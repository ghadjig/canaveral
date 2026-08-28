package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// asst builds one assistant message in the real wire shape: a
// {"info": {...}} entry, with "role" and "modelID", returned in a bare
// array ordered oldest-first.
func asst(created int64, completed string, tokensIn, tokensOut int, cost float64, extra string) string {
	comp := ""
	if completed != "" {
		comp = `,"completed":` + completed
	}
	return fmt.Sprintf(`{"info":{"role":"assistant","modelID":"m1","time":{"created":%d%s},`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},"cost":%v%s}}`,
		created, comp, tokensIn, tokensOut, cost, extra)
}

func msgList(msgs ...string) string { return "[" + strings.Join(msgs, ",") + "]" }

// fakeServer serves the endpoints Probe depends on, on the same API surface
// the real server uses: /api/session for the session list, but the bare
// /session/{id}/message, /question and /permission for everything else.
func fakeServer(t *testing.T, sessions, messages string) *httptest.Server {
	t.Helper()
	return fakeServerQ(t, sessions, messages, `{}`, `[]`, `[]`)
}

func fakeServerFull(t *testing.T, sessions, messages, status, permissions string) *httptest.Server {
	t.Helper()
	return fakeServerQ(t, sessions, messages, status, permissions, `[]`)
}

func fakeServerQ(t *testing.T, sessions, messages, status, permissions, questions string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(messages))
	})
	mux.HandleFunc("/session/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(status))
	})
	mux.HandleFunc("/permission", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(permissions))
	})
	mux.HandleFunc("/question", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(questions))
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

func TestProbeStateWaitingOnQuestion(t *testing.T) {
	// The assistant asked something via the question tool. opencode keeps
	// the session "busy" (the tool call is still open), but the agent
	// cannot proceed without an answer, so waiting must win over busy.
	msgs := msgList(asst(1, "", 0, 0, 0, ""))
	questions := `[{"sessionID":"ses_mine","questions":[{"header":"Fork trigger","question":"When should canaveral fork a session?","options":[{"label":"Always"},{"label":"Never"}]}]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, `[]`, questions)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Fatalf("State = %q, want waiting (question outranks busy)", h.State)
	}
	if h.Pending == nil {
		t.Fatal("Pending = nil, want the question details")
	}
	if h.Pending.Kind != BlockQuestion || h.Pending.Header != "Fork trigger" {
		t.Errorf("Pending = %+v", h.Pending)
	}
	if len(h.Pending.Options) != 2 || h.Pending.Options[0] != "Always" {
		t.Errorf("Options = %v", h.Pending.Options)
	}
}

func TestProbeIgnoresPendingForAnotherSession(t *testing.T) {
	// /question and /permission are server-wide; another session's pending
	// item must not make this agent look blocked.
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	questions := `[{"sessionID":"ses_somebody_else","questions":[{"header":"Nope","question":"?","options":[]}]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, `[]`, questions)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; another session's question leaked in", h.State)
	}
}

func TestProbeQuestionOutranksPermission(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	questions := `[{"sessionID":"ses_mine","questions":[{"header":"Pick one","question":"Which?","options":[]}]}]`
	perms := `[{"sessionID":"ses_mine","permission":"bash","patterns":["rm -rf /"]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, perms, questions)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Pending == nil || h.Pending.Kind != BlockQuestion {
		t.Errorf("Pending = %+v, want the question to take precedence", h.Pending)
	}
}

func TestProbePendingPermissionDetails(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	perms := `[{"sessionID":"ses_mine","permission":"bash","patterns":["echo hi"]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Fatalf("State = %q, want waiting", h.State)
	}
	if h.Pending == nil || h.Pending.Kind != BlockPermission {
		t.Fatalf("Pending = %+v, want a permission block", h.Pending)
	}
	if len(h.Pending.Resources) != 1 || h.Pending.Resources[0] != "echo hi" {
		t.Errorf("Resources = %v", h.Pending.Resources)
	}
}

func TestProbeWaitingOutranksRetrying(t *testing.T) {
	msgs := msgList(asst(1, "", 0, 0, 0, ""))
	status := `{"ses_mine":{"type":"retry","attempt":1,"message":"rate limited","next":5}}`
	perms := `[{"sessionID":"ses_mine","permission":"bash","patterns":[]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, status, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting (you can act; a retry is automatic)", h.State)
	}
}

func TestProbeNoPendingLeavesPendingNil(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Pending != nil {
		t.Errorf("Pending = %+v, want nil when nothing is blocked", h.Pending)
	}
}

func TestProbeStateBusy(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "", 0, 0, 0, "")), `{}`, `[]`)
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.State != StateBusy {
		t.Errorf("State = %q, want busy", h.State)
	}
}

func TestProbeStateIdle(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), `{}`, `[]`)
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.State != StateIdle {
		t.Errorf("State = %q, want idle", h.State)
	}
}

func TestProbeStateRetrying(t *testing.T) {
	status := `{"ses_mine":{"type":"retry","attempt":1,"message":"rate limited","next":5}}`
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "", 0, 0, 0, "")), status, `[]`)
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.State != StateRetrying {
		t.Errorf("State = %q, want retrying", h.State)
	}
}

func TestProbeUsesTheLastAssistantMessageAsTheCurrentTurn(t *testing.T) {
	// Regression test: messages come back oldest-first. Treating the first
	// as "current" reported the state of the session's opening turn
	// forever, so a working agent looked permanently idle.
	msgs := msgList(
		asst(1000, "2000", 5, 5, 0.01, `,"finish":"stop","modelID":"old-model"`),
		asst(3000, "", 0, 0, 0, ""), // newest, still generating
	)
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if !h.Busy {
		t.Error("Busy = false; the newest turn is still generating")
	}
	if h.Working <= 0 {
		t.Errorf("Working = %v, want > 0", h.Working)
	}
	if h.Worked != time.Second {
		t.Errorf("Worked = %v, want 1s (only the completed turn counts)", h.Worked)
	}
}

func TestProbeWorkedSumsCompletedTurnsOnly(t *testing.T) {
	msgs := msgList(
		asst(0, "1000", 0, 0, 0, `,"finish":"stop"`),
		asst(2000, "4000", 0, 0, 0, `,"finish":"stop"`),
		asst(5000, "", 0, 0, 0, ""),
	)
	srv := fakeServerFull(t, twoDirSessions, msgs, `{}`, `[]`)

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
	msgs := msgList(`{"info":{"role":"assistant","modelID":"m1","time":{"created":1,"completed":2},"finish":"stop",` +
		`"tokens":{"input":10,"output":5,"reasoning":0,"cache":{"read":1,"write":0}},"cost":0.25}}`)
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
	msgs := msgList(asst(1, "", 0, 0, 0, ""))
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if !h.Busy {
		t.Error("Busy = false, want true when time.completed is absent")
	}
}

func TestProbeSurfacesError(t *testing.T) {
	msgs := msgList(`{"info":{"role":"assistant","modelID":"m1","time":{"created":1,"completed":2},"finish":"error",` +
		`"error":{"name":"UnknownError","data":{"message":"HTTP 401 bad model"}}}}`)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.LastError != "HTTP 401 bad model" {
		t.Errorf("LastError = %q, want the provider message", h.LastError)
	}
}

func TestProbeSumsAssistantMessagesOnly(t *testing.T) {
	// Token totals live on assistant messages; user messages carry none and the
	// session list reports zero.
	msgs := msgList(
		asst(1, "2", 5, 5, 0.05, `,"finish":"tool-calls"`),
		`{"info":{"role":"user","time":{"created":2}}}`,
		asst(3, "4", 10, 10, 0.1, `,"finish":"stop"`),
	)
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
	srv := fakeServer(t, twoDirSessions, `[]`)
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
