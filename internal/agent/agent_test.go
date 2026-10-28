package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetShellPATHCacheForTest must only run between tests, never while
// ShellPATH is being called concurrently.
func resetShellPATHCacheForTest() {
	shellPATH = struct {
		once  sync.Once
		value map[string]string
	}{}
}

// asst builds one assistant message in opencode 2's wire shape: flat and
// typed, with the model as an object rather than a bare "modelID".
func asst(created int64, completed string, tokensIn, tokensOut int, cost float64, extra string) string {
	comp := ""
	if completed != "" {
		comp = `,"completed":` + completed
	}
	return fmt.Sprintf(`{"id":"msg_%d","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},`+
		`"time":{"created":%d%s},"content":[],`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},"cost":%v%s}`,
		created, created, comp, tokensIn, tokensOut, cost, extra)
}

// msgList builds a GET /api/session/{id}/message response from messages
// given oldest-first, the way a conversation reads. The server is asked for
// newest-first, so that is the order they go out in.
func msgList(msgs ...string) string {
	msgs = slices.Clone(msgs)
	slices.Reverse(msgs)
	return `{"data":[` + strings.Join(msgs, ",") + `],"cursor":{}}`
}

// msgWithID builds an assistant message carrying an explicit "id" and,
// optionally, extra top-level fields (e.g. "error":{...}) — needed to
// exercise classify's staleness check, which matches a question or
// permission's source messageID against a specific message.
func msgWithID(id string, created int64, completed string, extra string) string {
	comp := ""
	if completed != "" {
		comp = `,"completed":` + completed
	}
	return fmt.Sprintf(`{"id":%q,"type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},`+
		`"time":{"created":%d%s},"content":[]%s}`,
		id, created, comp, extra)
}

// form builds one entry of GET /api/session/{id}/form as the question tool
// raises it: one field per question, the header as its title. messageID is
// the tool call's message, "" for a form that did not come from one.
func form(session, messageID, header, question string, options ...string) string {
	var opts []string
	for _, o := range options {
		opts = append(opts, fmt.Sprintf(`{"value":%q,"label":%q}`, o, o))
	}
	tool := ""
	if messageID != "" {
		tool = fmt.Sprintf(`,"tool":{"messageID":%q,"id":"call_1"}`, messageID)
	}
	return fmt.Sprintf(`{"id":"frm_1","sessionID":%q,"title":"Questions","metadata":{"kind":"question"%s},`+
		`"fields":[{"key":"q0","type":"string","title":%q,"description":%q,"options":[%s],"custom":true}]}`,
		session, tool, header, question, strings.Join(opts, ","))
}

// perm builds one entry of GET /api/permission/request.
func perm(session, messageID, action string, resources ...string) string {
	source := ""
	if messageID != "" {
		source = fmt.Sprintf(`,"source":{"type":"tool","messageID":%q,"id":"call_1"}`, messageID)
	}
	if resources == nil {
		resources = []string{}
	}
	res := make([]string, len(resources))
	for i, r := range resources {
		res[i] = fmt.Sprintf("%q", r)
	}
	return fmt.Sprintf(`{"id":"per_1","sessionID":%q,"action":%q,"resources":[%s]%s}`,
		session, action, strings.Join(res, ","), source)
}

func list(items ...string) string { return "[" + strings.Join(items, ",") + "]" }

// running builds a GET /api/session/active response naming the given
// sessions as running.
func running(ids ...string) string {
	var entries []string
	for _, id := range ids {
		entries = append(entries, fmt.Sprintf(`%q:{"type":"running"}`, id))
	}
	return `{"data":{` + strings.Join(entries, ",") + `}}`
}

// fakeServer serves the endpoints Probe depends on, all under /api as on a
// real opencode 2 server.
func fakeServer(t *testing.T, sessions, messages string) *httptest.Server {
	t.Helper()
	return fakeServerQ(t, sessions, messages, "", `[]`, `[]`)
}

func fakeServerFull(t *testing.T, sessions, messages, active, permissions string) *httptest.Server {
	t.Helper()
	return fakeServerQ(t, sessions, messages, active, permissions, `[]`)
}

// fakeServerQ is fakeServer with every input spelled out. active is the
// /api/session/active body, or "" for a server that cannot say, which
// leaves the transcript to decide whether a turn is running. permissions and
// forms are JSON arrays; the forms are listed for whichever session is
// asked, as a real server would only for their own.
func fakeServerQ(t *testing.T, sessions, messages, active, permissions, forms string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/api/session/active", func(w http.ResponseWriter, r *http.Request) {
		if active == "" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(active))
	})
	mux.HandleFunc("/api/session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(messages))
	})
	mux.HandleFunc("/api/session/{id}/form", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + forms + `}`))
	})
	mux.HandleFunc("/api/permission/request", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"location":{"directory":"/w/mine"},"data":` + permissions + `}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// Sessions carry their own token and cost totals, exactly as the real
// /api/session list does.
func sess(id, dir string, updated int64, parent string, in, out int, cost float64) string {
	pid := ""
	if parent != "" {
		pid = fmt.Sprintf(`,"parentID":%q`, parent)
	}
	return fmt.Sprintf(`{"id":%q,"projectID":"prj","location":{"directory":%q},"time":{"created":1,"updated":%d}%s,`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},"cost":%v}`,
		id, dir, updated, pid, in, out, cost)
}

func sessList(ss ...string) string { return `{"data":[` + strings.Join(ss, ",") + `],"cursor":{}}` }

var twoDirSessions = sessList(
	sess("ses_mine", "/w/mine", 200, "", 11, 5, 0.25),
	sess("ses_old", "/w/mine", 100, "", 0, 0, 0),
	sess("ses_other", "/w/other", 300, "", 0, 0, 0),
)

// The server refuses everything without its password; a probe that forgot
// it would see nothing but 401s.
func TestProbeAuthenticatesEveryRequest(t *testing.T) {
	inner := fakeServerQ(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)),
		running(), list(), list(form("ses_mine", "", "Q", "?")))
	var mu sync.Mutex
	var unauthenticated []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != "opencode" || p != "pw" {
			mu.Lock()
			unauthenticated = append(unauthenticated, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(inner.URL, "http://")
		resp, err := http.DefaultTransport.RoundTrip(r.Clone(r.Context()))
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		buf := make([]byte, 64*1024)
		n, _ := resp.Body.Read(buf)
		_, _ = w.Write(buf[:n])
	}))
	defer srv.Close()

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine", Password: "pw"})
	if !h.Reachable {
		t.Fatalf("not reachable: %v", h.Err)
	}
	if len(unauthenticated) > 0 {
		t.Errorf("requests sent without the password: %v", unauthenticated)
	}
}

func TestProbeStateWaitingOnQuestion(t *testing.T) {
	// The assistant asked something via the question tool. opencode keeps
	// the session running (the tool call is still open), but the agent
	// cannot proceed without an answer, so waiting must win over busy.
	msgs := msgList(asst(1, "", 0, 0, 0, ""))
	forms := list(form("ses_mine", "", "Fork trigger", "When should canaveral fork a session?", "Always", "Never"))
	srv := fakeServerQ(t, twoDirSessions, msgs, running("ses_mine"), `[]`, forms)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Fatalf("State = %q, want waiting (question outranks busy)", h.State)
	}
	if h.Pending == nil {
		t.Fatal("Pending = nil, want the question details")
	}
	if h.Pending.Kind != BlockQuestion || h.Pending.Header != "Fork trigger" {
		t.Errorf("Pending = %+v", h.Pending)
	}
	if h.Pending.Detail != "When should canaveral fork a session?" {
		t.Errorf("Detail = %q, want the question itself", h.Pending.Detail)
	}
	if len(h.Pending.Options) != 2 || h.Pending.Options[0] != "Always" {
		t.Errorf("Options = %v", h.Pending.Options)
	}
}

func TestProbeIgnoresPendingForAnotherSession(t *testing.T) {
	// Another session's pending item must not make this agent look blocked.
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	forms := list(form("ses_somebody_else", "", "Nope", "?"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", `[]`, forms)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; another session's question leaked in", h.State)
	}
}

func TestProbeQuestionOutranksPermission(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	forms := list(form("ses_mine", "", "Pick one", "Which?"))
	perms := list(perm("ses_mine", "", "bash", "rm -rf /"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", perms, forms)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Pending == nil || h.Pending.Kind != BlockQuestion {
		t.Errorf("Pending = %+v, want the question to take precedence", h.Pending)
	}
}

func TestProbeIgnoresAStaleQuestionFromAnAbortedTurn(t *testing.T) {
	// Regression test: opencode crashing (or being killed) mid-question can
	// leave the request listed with no live TUI left to answer it, and
	// nothing times it out. Once the message the request names has actually
	// completed (here, aborted) and the conversation has moved on, the
	// request is stale and must not keep reporting the agent as blocked.
	msgs := msgList(
		msgWithID("msg_aborted", 1, "2", `,"finish":"error","error":{"type":"aborted","message":"Aborted"}`),
		asst(3, "4", 0, 0, 0, `,"finish":"stop"`), // the conversation continued past it
	)
	forms := list(form("ses_mine", "msg_aborted", "Stale", "Still here?"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", `[]`, forms)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; the question's turn already ended", h.State)
	}
	if h.Pending != nil {
		t.Errorf("Pending = %+v, want nil for a stale question", h.Pending)
	}
}

func TestProbeHonoursAQuestionStillOpen(t *testing.T) {
	// Same shape as the staleness test above, but the named message has not
	// completed: the question is genuinely still pending and must still
	// block.
	msgs := msgList(msgWithID("msg_open", 1, "", ""))
	forms := list(form("ses_mine", "msg_open", "Pick", "Which?"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", `[]`, forms)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting; the question's message is still open", h.State)
	}
}

func TestProbeIgnoresAStalePermissionFromAnAbortedTurn(t *testing.T) {
	// Same bug, on the permission side: a crash mid-permission-request can
	// leave it listed forever even though its turn is long over.
	msgs := msgList(
		msgWithID("msg_aborted", 1, "2", `,"finish":"error","error":{"type":"aborted","message":"Aborted"}`),
		asst(3, "4", 0, 0, 0, `,"finish":"stop"`),
	)
	perms := list(perm("ses_mine", "msg_aborted", "bash", "rm -rf /"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; the permission's turn already ended", h.State)
	}
	if h.Pending != nil {
		t.Errorf("Pending = %+v, want nil for a stale permission", h.Pending)
	}
}

func TestProbeHonoursAPermissionStillOpen(t *testing.T) {
	msgs := msgList(msgWithID("msg_open", 1, "", ""))
	perms := list(perm("ses_mine", "msg_open", "bash", "echo hi"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting; the permission's message is still open", h.State)
	}
}

func TestProbePendingPermissionDetails(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	perms := list(perm("ses_mine", "", "bash", "echo hi"))
	srv := fakeServerQ(t, twoDirSessions, msgs, "", perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Fatalf("State = %q, want waiting", h.State)
	}
	if h.Pending == nil || h.Pending.Kind != BlockPermission {
		t.Fatalf("Pending = %+v, want a permission block", h.Pending)
	}
	if h.Pending.Header != "bash" {
		t.Errorf("Header = %q, want the action", h.Pending.Header)
	}
	if len(h.Pending.Resources) != 1 || h.Pending.Resources[0] != "echo hi" {
		t.Errorf("Resources = %v", h.Pending.Resources)
	}
}

// Permission requests are listed per location; asking without one gets the
// server's own directory, which is not the feature's.
func TestProbeAsksForPermissionsInTheAgentsDirectory(t *testing.T) {
	var got string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, twoDirSessions)
	})
	mux.HandleFunc("/api/session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, msgList(asst(1, "2", 0, 0, 0, "")))
	})
	mux.HandleFunc("/api/permission/request", func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("location[directory]")
		fmt.Fprint(w, `{"data":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	_ = Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if got != "/w/mine" {
		t.Errorf("location[directory] = %q, want the agent's directory", got)
	}
}

func TestProbeWaitingOutranksRetrying(t *testing.T) {
	msgs := msgList(asst(1, "", 0, 0, 0, `,"retry":{"attempt":1,"at":5,"error":{"type":"rate_limit","message":"rate limited"}}`))
	perms := list(perm("ses_mine", "", "bash"))
	srv := fakeServerQ(t, twoDirSessions, msgs, running("ses_mine"), perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting (you can act; a retry is automatic)", h.State)
	}
}

func TestProbeNoPendingLeavesPendingNil(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`))
	srv := fakeServerFull(t, twoDirSessions, msgs, "", `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Pending != nil {
		t.Errorf("Pending = %+v, want nil when nothing is blocked", h.Pending)
	}
}

func TestProbeStateBusy(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "", 0, 0, 0, "")), running("ses_mine"), `[]`)
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.State != StateBusy {
		t.Errorf("State = %q, want busy", h.State)
	}
}

func TestProbeStateIdle(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), running(), `[]`)
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.State != StateIdle {
		t.Errorf("State = %q, want idle", h.State)
	}
}

// A turn a crash left without a completed time looks open in the
// transcript, but the server knows it is not running anything.
func TestProbeTrustsTheServerOverAnUnfinishedTranscript(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "", 0, 0, 0, "")), running("ses_other"), `[]`)
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateIdle || h.Busy {
		t.Errorf("State = %q, Busy = %v, want idle: the session is not running", h.State, h.Busy)
	}
}

func TestProbeStateRetrying(t *testing.T) {
	msgs := msgList(asst(1, "", 0, 0, 0, `,"retry":{"attempt":2,"at":5,"error":{"type":"rate_limit","message":"rate limited"}}`))
	srv := fakeServerFull(t, twoDirSessions, msgs, running("ses_mine"), `[]`)
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.State != StateRetrying {
		t.Errorf("State = %q, want retrying", h.State)
	}
}

func TestProbeUsesTheLastAssistantMessageAsTheCurrentTurn(t *testing.T) {
	// Regression test: a conversation reads oldest-first. Treating the
	// first message as "current" reported the state of the session's
	// opening turn forever, so a working agent looked permanently idle.
	msgs := msgList(
		asst(1000, "2000", 5, 5, 0.01, `,"finish":"stop"`),
		asst(3000, "", 0, 0, 0, ""), // newest, still generating
	)
	srv := fakeServerFull(t, twoDirSessions, msgs, "", `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
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
	srv := fakeServerFull(t, twoDirSessions, msgs, "", `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
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
	srv := fakeServer(t, twoDirSessions, msgList(asst(1, "2", 10, 5, 0.25, `,"finish":"stop"`)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
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
		t.Errorf("Tokens.Total = %d, want 16 (from the session list)", got)
	}
	if h.Cost != 0.25 {
		t.Errorf("Cost = %v, want 0.25", h.Cost)
	}
}

func TestProbeExposesModelVariantAndProvider(t *testing.T) {
	msgs := msgList(`{"id":"m","type":"assistant","agent":"build",` +
		`"model":{"id":"claude-sonnet-5","providerID":"github-copilot","variant":"high"},` +
		`"time":{"created":1,"completed":2},"content":[],"finish":"stop"}`)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Model != "claude-sonnet-5" {
		t.Errorf("Model = %q", h.Model)
	}
	if h.Variant != "high" {
		t.Errorf("Variant = %q, want the reasoning effort", h.Variant)
	}
	if h.Provider != "github-copilot" {
		t.Errorf("Provider = %q", h.Provider)
	}
}

// A conversation with no turn yet still has a model: the one its first turn
// will use, which is what the TUI shows.
func TestProbeFallsBackToTheSessionsModel(t *testing.T) {
	sessions := `{"data":[{"id":"ses_mine","location":{"directory":"/w/mine"},"time":{"updated":1},` +
		`"model":{"id":"gpt-6","providerID":"openai","variant":"low"},` +
		`"tokens":{"input":0,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},"cost":0}]}`
	srv := fakeServer(t, sessions, msgList())

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Model != "gpt-6" || h.Provider != "openai" || h.Variant != "low" {
		t.Errorf("model = %q/%q/%q, want the session's", h.Provider, h.Model, h.Variant)
	}
}

func TestProbeExcludesSubagentSessionsFromTheCount(t *testing.T) {
	// The subagent tool gives every subagent its own session in the same
	// directory, so one conversation with three helpers must not read as
	// four separate things you started.
	sessions := sessList(
		sess("ses_root", "/w/mine", 100, "", 10, 10, 1.0),
		sess("ses_sub1", "/w/mine", 101, "ses_root", 20, 20, 2.0),
		sess("ses_sub2", "/w/mine", 102, "ses_root", 30, 30, 3.0),
	)
	srv := fakeServer(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Sessions != 1 {
		t.Errorf("Sessions = %d, want 1 top-level conversation", h.Sessions)
	}
	if h.SubSessions != 2 {
		t.Errorf("SubSessions = %d, want 2", h.SubSessions)
	}
}

func TestProbePicksTheNewestRootNotANewerSubagent(t *testing.T) {
	// A subagent updates a beat after its parent, so selecting the newest
	// session overall would flip to the subagent and report its model and
	// state instead of the conversation's.
	sessions := sessList(
		sess("ses_root", "/w/mine", 100, "", 0, 0, 0),
		sess("ses_sub", "/w/mine", 999, "ses_root", 0, 0, 0),
	)
	srv := fakeServer(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.SessionID != "ses_root" {
		t.Errorf("SessionID = %q, want the root conversation even though a subagent is newer", h.SessionID)
	}
}

func TestProbeAggregatesCostAcrossTheSubagentFamily(t *testing.T) {
	// Subagent spend is real spend on this feature; counting only the
	// parent understated a live feature by roughly three times.
	sessions := sessList(
		sess("ses_root", "/w/mine", 100, "", 1, 1, 1.0),
		sess("ses_sub1", "/w/mine", 101, "ses_root", 2, 2, 2.0),
		sess("ses_deep", "/w/mine", 102, "ses_sub1", 4, 4, 4.0), // nested subagent
		sess("ses_other_root", "/w/mine", 50, "", 100, 100, 99.0),
	)
	srv := fakeServer(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Cost != 7.0 {
		t.Errorf("Cost = %v, want 7.0 (root + both descendants, not the unrelated root)", h.Cost)
	}
	if got := h.Tokens.Total(); got != 14 {
		t.Errorf("Tokens.Total = %d, want 14", got)
	}
	if h.SubSessions != 2 {
		t.Errorf("SubSessions = %d, want 2 (transitive)", h.SubSessions)
	}
}

func TestProbeWorkedExcludesSubagentTurns(t *testing.T) {
	// A parent's turn stays open while its subagent runs, so the parent's
	// duration already covers that work; adding the subagent's own turns
	// would double-count wall-clock time.
	sessions := sessList(
		sess("ses_root", "/w/mine", 100, "", 0, 0, 0),
		sess("ses_sub", "/w/mine", 101, "ses_root", 0, 0, 0),
	)
	srv := fakeServer(t, sessions, msgList(asst(0, "5000", 0, 0, 0, `,"finish":"stop"`)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Worked != 5*time.Second {
		t.Errorf("Worked = %v, want 5s from the conversation's own turns only", h.Worked)
	}
}

func TestProbeBusyWhenNotCompleted(t *testing.T) {
	// A missing time.completed is the signal that generation is in flight,
	// when the server cannot be asked directly.
	msgs := msgList(asst(1, "", 0, 0, 0, ""))
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if !h.Busy {
		t.Error("Busy = false, want true when time.completed is absent")
	}
}

func TestProbeSurfacesError(t *testing.T) {
	msgs := msgList(asst(1, "2", 0, 0, 0, `,"finish":"error","error":{"type":"api","message":"HTTP 401 bad model","status":401}`))
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.LastError != "HTTP 401 bad model" {
		t.Errorf("LastError = %q, want the provider message", h.LastError)
	}
}

func TestProbeSumsAssistantMessagesOnly(t *testing.T) {
	// Other message types must not be mistaken for turns when finding the
	// current one, and a non-error finish on an older message must not be
	// reported as an error.
	msgs := msgList(
		asst(1, "2", 5, 5, 0.05, `,"finish":"tool-calls"`),
		`{"id":"u","type":"user","time":{"created":2},"text":"go on"}`,
		asst(3, "4", 10, 10, 0.1, `,"finish":"stop"`),
		`{"id":"i","type":"idle","time":{"created":5},"outcome":"succeeded"}`,
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Worked != 2*time.Millisecond {
		t.Errorf("Worked = %v, want 2ms across the two completed turns", h.Worked)
	}
	if h.Busy {
		t.Error("Busy = true; the current turn has finished")
	}
	// finish=="tool-calls" on an older message must not be reported as an error.
	if h.LastError != "" {
		t.Errorf("LastError = %q, want empty", h.LastError)
	}
}

func TestProbeNoSessionsForDirectory(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList())
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/nothing-here"})
	if !h.Reachable {
		t.Fatal("want reachable")
	}
	if h.Sessions != 0 {
		t.Errorf("Sessions = %d, want 0", h.Sessions)
	}
}

func TestProbeUnreachable(t *testing.T) {
	h := Probe(context.Background(), "opencode", Conn{URL: "http://127.0.0.1:1", Dir: "/w"})
	if h.Reachable {
		t.Error("Reachable = true for a dead endpoint")
	}
	if h.Err == nil {
		t.Error("Err = nil, want a connection error")
	}
}

func TestProbeEmptyURL(t *testing.T) {
	if h := Probe(context.Background(), "opencode", Conn{URL: "", Dir: "/w"}); h.Err == nil {
		t.Error("empty URL should report an error")
	}
}

// The web UI answers any path outside /api with a 200 and an HTML page. A
// probe that lands there must say it failed rather than report an empty,
// idle agent.
func TestProbeReportsAnHTMLAnswerAsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/html")
		fmt.Fprint(w, "<!doctype html><html></html>")
	}))
	defer srv.Close()
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w"})
	if h.Reachable || h.Err == nil {
		t.Errorf("Reachable = %v, Err = %v; want an error", h.Reachable, h.Err)
	}
}

// The log of a real opencode 2.1 server, started the way ServeCmd starts
// one.
const serveLog = "server listening on http://127.0.0.1:39777\n" +
	"server password zHXKQC4fGQ4JKuGnzlUxkbo2cDHc9WE66zkIRo8Fk3Y\n"

func TestDiscoverReadsAddressAndPassword(t *testing.T) {
	log := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(log, []byte(serveLog), 0o600); err != nil {
		t.Fatal(err)
	}
	ep, err := (opencode{}).Discover(context.Background(), log, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ep.URL != "http://127.0.0.1:39777" {
		t.Errorf("URL = %q", ep.URL)
	}
	if ep.Password != "zHXKQC4fGQ4JKuGnzlUxkbo2cDHc9WE66zkIRo8Fk3Y" {
		t.Errorf("Password = %q", ep.Password)
	}
}

// opencode 1 printed an address and no password. Waiting it out must end in
// an error that says what is wrong, not a generic timeout.
func TestDiscoverNamesAnOpencodeWithoutAPassword(t *testing.T) {
	log := filepath.Join(t.TempDir(), "agent.log")
	if err := os.WriteFile(log, []byte("opencode server listening on http://127.0.0.1:39777\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := (opencode{}).Discover(context.Background(), log, 300*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "opencode 2") {
		t.Errorf("err = %v, want it to name the version needed", err)
	}
}

// Without the blank override, a password in a manifest's [env] would reach
// the server, which then prints none — and Discover would wait for one in
// vain.
func TestEnvForBlanksTheServerPassword(t *testing.T) {
	env := (opencode{}).EnvFor(Selection{})
	for _, k := range []string{"OPENCODE_PASSWORD", "OPENCODE_SERVER_PASSWORD"} {
		if v, ok := env[k]; !ok || v != "" {
			t.Errorf("%s = %q (set: %v), want explicitly blank", k, v, ok)
		}
	}
}

func TestServeCmdQuotesPath(t *testing.T) {
	got := (opencode{}).ServeCmd("/home/a b/opencode")
	want := "exec '/home/a b/opencode' serve --hostname 127.0.0.1 --port 0"
	if got != want {
		t.Errorf("ServeCmd = %q, want %q", got, want)
	}
}

// opencode 2 has no `attach`: the TUI connects with --server, and takes its
// directory as an argument.
func TestAttachArgvConnectsToTheServer(t *testing.T) {
	c := Conn{URL: "http://127.0.0.1:4096", Dir: "/wt", Password: "pw"}
	if got, want := strings.Join((opencode{}).AttachArgv(c, false), " "), "opencode --server http://127.0.0.1:4096 /wt"; got != want {
		t.Errorf("AttachArgv = %q, want %q", got, want)
	}
	if got, want := strings.Join((opencode{}).AttachArgv(c, true), " "), "opencode --server http://127.0.0.1:4096 /wt --continue"; got != want {
		t.Errorf("AttachArgv --continue = %q, want %q", got, want)
	}
	for _, a := range (opencode{}).AttachArgv(c, true) {
		if strings.Contains(a, "pw") {
			t.Errorf("the password is in argv, where any user can read it: %q", a)
		}
	}
	if env := (opencode{}).ClientEnv(c); env["OPENCODE_PASSWORD"] != "pw" {
		t.Errorf("ClientEnv = %v, want the password in OPENCODE_PASSWORD", env)
	}
}

func TestAttachCommandQuotesOnlyWhatNeedsIt(t *testing.T) {
	c := Conn{URL: "http://127.0.0.1:4096", Dir: "/home/me/my project"}
	got := AttachCommand(opencode{}, c, "--session ses_1")
	want := "opencode --server http://127.0.0.1:4096 '/home/me/my project' --session ses_1"
	if got != want {
		t.Errorf("AttachCommand = %q, want %q", got, want)
	}
	pinHookBin(t, "/bin/canaveral")
	if got, want := AttachCommand(claude{}, Conn{Dir: "/wt"}, ""), "claude --settings '"; !strings.HasPrefix(got, want) {
		t.Errorf("AttachCommand(claude) = %q, want it to start %q", got, want)
	}
}

func TestForkMovesTheCopyIntoTheNewWorktree(t *testing.T) {
	var moved string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/session/ses_src/fork", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"id":"ses_copy","location":{"directory":"/wt/old"}}}`)
	})
	mux.HandleFunc("POST /api/session/ses_copy/move", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Directory string `json:"directory"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		moved = body.Directory
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, err := (opencode{}).Fork(context.Background(), Conn{URL: srv.URL}, "ses_src", "/wt/new")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ses_copy" {
		t.Errorf("id = %q, want ses_copy", id)
	}
	if moved != "/wt/new" {
		t.Errorf("moved to %q, want /wt/new", moved)
	}
}

// todos builds a finished todowrite call carrying items, each a
// "status:content" pair.
func todos(created int64, items ...string) string {
	var out []string
	for _, it := range items {
		status, content, _ := strings.Cut(it, ":")
		out = append(out, fmt.Sprintf(`{"content":%q,"status":%q,"priority":"medium"}`, content, status))
	}
	return fmt.Sprintf(`{"id":"msg_todo_%d","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},`+
		`"time":{"created":%d,"completed":%d},"finish":"tool-calls","content":[`+
		`{"type":"tool","id":"call_t","name":"todowrite","state":{"status":"completed","input":{"todos":[%s]},`+
		`"content":[{"type":"text","text":"ok"}]},"time":{"created":%d,"ran":%d,"completed":%d}}]}`,
		created, created, created+1, strings.Join(out, ","), created, created, created+1)
}

// opencode 2 has no todo endpoint: the list is the newest finished
// todowrite call's, which is also how its own TUI finds it.
func TestProbeReadsTodos(t *testing.T) {
	msgs := msgList(
		todos(1, "completed:Stale list", "pending:Superseded"),
		todos(2,
			"completed:Verify schemas",
			"completed:Fix parsing",
			"in_progress:Wire the widget",
			"pending:Update README",
			"cancelled:Dropped idea",
		),
		asst(5, "6", 0, 0, 0, `,"finish":"stop"`),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	got := h.Todos
	if got.Total != 5 || got.Completed != 2 || got.InProgress != 1 || got.Pending != 1 || got.Cancelled != 1 {
		t.Errorf("Todos = %+v", got)
	}
	if got.Current != "Wire the widget" {
		t.Errorf("Current = %q, want the in-progress task", got.Current)
	}
	// completed(2) + cancelled(1) of 5
	if d := got.Done(); d < 0.59 || d > 0.61 {
		t.Errorf("Done() = %v, want ~0.6", d)
	}
}

func userMsg(text string) string {
	return fmt.Sprintf(`{"id":"msg_u","type":"user","time":{"created":1},"text":%q}`, text)
}

// A todo list opencode reports as fully resolved (nothing in_progress or
// pending) describes a task that has already wrapped up. If the
// conversation has since moved on — a new user prompt arrived with no
// further todowrite call — the list belongs to finished, unrelated work and
// must not keep reporting as if it were live progress on whatever the agent
// is doing now.
func TestProbeClearsStaleCompletedTodos(t *testing.T) {
	msgs := msgList(
		todos(1, "completed:Verify schemas", "completed:Fix parsing"),
		userMsg("totally different follow-up ask"),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Todos.Total != 0 {
		t.Errorf("Todos = %+v, want zeroed once a newer prompt makes the finished list stale", h.Todos)
	}
}

// The same finished list, but with no prompt after the last todowrite call,
// must still be reported: the task just finished and nothing has moved on.
func TestProbeKeepsCompletedTodosWithoutNewerPrompt(t *testing.T) {
	msgs := msgList(
		userMsg("do the thing"),
		todos(1, "completed:Verify schemas", "completed:Fix parsing"),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Todos.Total != 2 || h.Todos.Completed != 2 {
		t.Errorf("Todos = %+v, want the just-finished list still reported", h.Todos)
	}
}

// A list that still has work outstanding must be kept even across a newer
// prompt: follow-up guidance on an in-flight task does not make that task's
// own list stale.
func TestProbeKeepsInProgressTodosDespiteNewerPrompt(t *testing.T) {
	msgs := msgList(
		todos(1, "completed:Verify schemas", "in_progress:Wire the widget"),
		userMsg("also handle the edge case"),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Todos.Total != 2 || h.Todos.Current != "Wire the widget" {
		t.Errorf("Todos = %+v, want the in-progress list kept", h.Todos)
	}
}

func TestProbeNoTodosIsZeroed(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), "", `[]`)
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Todos.Total != 0 || h.Todos.Current != "" {
		t.Errorf("Todos = %+v, want zeroed when the agent uses no list", h.Todos)
	}
	if h.Todos.Done() != 0 {
		t.Errorf("Done() = %v, want 0 with no tasks (no divide by zero)", h.Todos.Done())
	}
}

func TestTodosDoneCountsCancelledAsResolved(t *testing.T) {
	// Abandoning a task must not leave a progress bar permanently short.
	td := Todos{Total: 2, Completed: 1, Cancelled: 1}
	if d := td.Done(); d != 1 {
		t.Errorf("Done() = %v, want 1", d)
	}
}

// msgWithTool builds an assistant message carrying one tool part, whose
// input describes the call as command.
func msgWithTool(tool, status, command string, start int64) string {
	return fmt.Sprintf(`{"id":"msg_%d","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},"time":{"created":1},`+
		`"content":[{"type":"text","text":""},{"type":"tool","id":"call_1","name":%q,`+
		`"state":{"status":%q,"input":{"command":%q},"metadata":{}},"time":{"created":%d,"ran":%d}}]}`,
		start, tool, status, command, start, start)
}

func TestProbeReportsTheRunningToolAsActivity(t *testing.T) {
	srv := fakeServer(t, twoDirSessions,
		msgList(msgWithTool("shell", "running", "bin/rails test", 1700000000000)))

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Activity == nil {
		t.Fatal("Activity = nil, want the running tool call")
	}
	if h.Activity.Tool != "shell" || h.Activity.Title != "bin/rails test" {
		t.Errorf("Activity = %+v", h.Activity)
	}
	if h.Activity.Since.UnixMilli() != 1700000000000 {
		t.Errorf("Since = %v", h.Activity.Since)
	}
}

// While the model is still writing a call's arguments they arrive as the
// raw partial string, not an object. That must neither fail the decode of
// the whole transcript nor hide that a call is under way.
func TestProbeTreatsStreamingToolsAsActivity(t *testing.T) {
	msgs := msgList(`{"id":"m","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},"time":{"created":1},` +
		`"content":[{"type":"tool","id":"call_1","name":"edit","state":{"status":"streaming","input":"{\"filePath\":\"app/mo"},` +
		`"time":{"created":7}}]}`)
	srv := fakeServer(t, twoDirSessions, msgs)
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Activity == nil || h.Activity.Tool != "edit" {
		t.Fatalf("Activity = %+v, want the streaming call", h.Activity)
	}
	if h.Activity.Since.UnixMilli() != 7 {
		t.Errorf("Since = %v, want the call's creation before it has run", h.Activity.Since)
	}
}

func TestProbeNoActivityWhenToolsHaveFinished(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(msgWithTool("shell", "completed", "ls", 1)))
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Activity != nil {
		t.Errorf("Activity = %+v, want nil once the call has completed", h.Activity)
	}
}

func TestProbeActivityIgnoresEarlierTurns(t *testing.T) {
	// Only the newest turn can have a call in flight; a stale "running"
	// left on an older message must not be reported as current.
	srv := fakeServer(t, twoDirSessions, msgList(
		msgWithTool("shell", "running", "old and stuck", 1),
		msgWithTool("grep", "completed", "done", 2),
	))
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Activity != nil {
		t.Errorf("Activity = %+v, want nil (the newest turn has nothing running)", h.Activity)
	}
}

func TestProbeActivityTitleIsTrimmedToOneLine(t *testing.T) {
	srv := fakeServer(t, twoDirSessions,
		msgList(msgWithTool("shell", "running", "cd /somewhere && \n  bin/rails test \n  --verbose", 1)))
	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.Activity == nil {
		t.Fatal("Activity = nil")
	}
	if strings.Contains(h.Activity.Title, "\n") {
		t.Errorf("Title still multi-line: %q", h.Activity.Title)
	}
}

func TestDescribeInputPrefersKnownKeys(t *testing.T) {
	cases := []struct {
		in   map[string]any
		want string
	}{
		{map[string]any{"command": "ls"}, "ls"},
		{map[string]any{"filePath": "a.go"}, "a.go"},
		{map[string]any{"pattern": "func .*"}, "func .*"},
		{map[string]any{"timeout": 5}, ""},  // nothing descriptive
		{map[string]any{"command": 42}, ""}, // wrong type, not a crash
		{nil, ""},
	}
	for _, c := range cases {
		if got := describeInput(c.in); got != c.want {
			t.Errorf("describeInput(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func textMsg(role, text string) string {
	if role == "user" {
		return userMsg(text)
	}
	return fmt.Sprintf(`{"id":"msg_a","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},`+
		`"time":{"created":1,"completed":2},"content":[{"type":"text","text":%q}]}`, text)
}

func TestProbeReportsLastMessageFromEachSide(t *testing.T) {
	msgs := msgList(
		textMsg("user", "first thing I asked"),
		textMsg("assistant", "first reply"),
		textMsg("user", "cancel workflow shouldn't be here"),
		textMsg("assistant", "Removed the cancel button from the onboarding nav."),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.LastUser != "cancel workflow shouldn't be here" {
		t.Errorf("LastUser = %q", h.LastUser)
	}
	if h.LastAssistant != "Removed the cancel button from the onboarding nav." {
		t.Errorf("LastAssistant = %q", h.LastAssistant)
	}
}

func TestProbeIgnoresEmptyTextParts(t *testing.T) {
	msgs := msgList(
		textMsg("assistant", "real answer"),
		textMsg("assistant", "   "),
	)
	srv := fakeServer(t, twoDirSessions, msgs)
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.LastAssistant != "real answer" {
		t.Errorf("LastAssistant = %q, want the last non-empty text", h.LastAssistant)
	}
}

func TestPreviewTextFlattensAndStripsMarkdown(t *testing.T) {
	got := previewText("**http://localhost:3000/x**\n\nPlease use   this one, on `port 3000`")
	want := "http://localhost:3000/x Please use this one, on port 3000"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestPreviewTextCaps(t *testing.T) {
	got := previewText(strings.Repeat("x", 500))
	if len([]rune(got)) > 301 {
		t.Errorf("length %d, want capped", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("want an ellipsis to show it was cut: %q", got[len(got)-10:])
	}
}

func TestProbeSeesAPermissionBlockingASubagent(t *testing.T) {
	// Regression test for a real miss: a subagent stopped for permission
	// while the TUI showed a prompt, but canaveral reported only "busy".
	// Pinning the pending lookup to the parent session hid it — a blocked
	// subagent blocks everything above it, since the parent's turn is
	// sitting waiting for that subagent's result.
	sessions := sessList(
		sess("ses_root", "/w/mine", 200, "", 0, 0, 0),
		sess("ses_sub", "/w/mine", 100, "ses_root", 0, 0, 0),
	)
	perms := list(perm("ses_sub", "", "external_directory", "/gems/ruby_llm/*"))
	srv := fakeServerQ(t, sessions, msgList(asst(1, "", 0, 0, 0, "")), running("ses_root", "ses_sub"), perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting {
		t.Fatalf("State = %q, want waiting; a blocked subagent blocks the conversation", h.State)
	}
	if h.Pending == nil || h.Pending.Header != "external_directory" {
		t.Fatalf("Pending = %+v", h.Pending)
	}
	if len(h.Pending.Resources) != 1 || h.Pending.Resources[0] != "/gems/ruby_llm/*" {
		t.Errorf("Resources = %v", h.Pending.Resources)
	}
}

// Forms are listed one session at a time, so a subagent's question is only
// found by asking that subagent.
func TestProbeSeesAQuestionFromASubagent(t *testing.T) {
	sessions := sessList(
		sess("ses_root", "/w/mine", 200, "", 0, 0, 0),
		sess("ses_sub", "/w/mine", 100, "ses_root", 0, 0, 0),
		sess("ses_done", "/w/mine", 90, "ses_root", 0, 0, 0),
	)
	var asked []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sessions) })
	mux.HandleFunc("/api/session/active", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, running("ses_root", "ses_sub"))
	})
	mux.HandleFunc("/api/session/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, msgList(asst(1, "", 0, 0, 0, "")))
	})
	mux.HandleFunc("/api/session/{id}/form", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		asked = append(asked, id)
		mu.Unlock()
		if id == "ses_sub" {
			fmt.Fprint(w, `{"data":`+list(form("ses_sub", "", "Which DB?", "Pick one", "pg"))+`}`)
			return
		}
		fmt.Fprint(w, `{"data":[]}`)
	})
	mux.HandleFunc("/api/permission/request", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"data":[]}`) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateWaiting || h.Pending == nil || h.Pending.Header != "Which DB?" {
		t.Fatalf("State = %q, Pending = %+v; want the subagent's question", h.State, h.Pending)
	}
	if slices.Contains(asked, "ses_done") {
		t.Errorf("asked a finished subagent for forms: %v; only running sessions can have one", asked)
	}
}

func TestProbeIgnoresPendingFromAnUnrelatedConversation(t *testing.T) {
	// Widening the match to the family must not widen it to the whole
	// server: another conversation's prompt is not this one's problem.
	sessions := sessList(
		sess("ses_root", "/w/mine", 200, "", 0, 0, 0),
		sess("ses_other_root", "/w/mine", 100, "", 0, 0, 0),
		sess("ses_other_sub", "/w/mine", 50, "ses_other_root", 0, 0, 0),
	)
	perms := list(perm("ses_other_sub", "", "bash"))
	srv := fakeServerQ(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), "", perms, `[]`)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; another conversation's prompt leaked in", h.State)
	}
}

func TestProbeHidesTheAssistantReplyToAnEarlierPrompt(t *testing.T) {
	// While an agent works on a new prompt, the previous turn's sign-off
	// describes finished work; showing it alongside the new task reads as
	// though it were the current state.
	msgs := msgList(
		textMsg("user", "first thing"),
		textMsg("assistant", "Confirmed fixed. Here's a fresh link."),
		textMsg("user", "if one file is not supported, skip it"),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.LastUser != "if one file is not supported, skip it" {
		t.Errorf("LastUser = %q", h.LastUser)
	}
	if h.LastAssistant != "" {
		t.Errorf("LastAssistant = %q, want empty until it replies to the new prompt", h.LastAssistant)
	}
}

func TestProbeShowsTheAssistantReplyOnceItAnswers(t *testing.T) {
	msgs := msgList(
		textMsg("user", "if one file is not supported, skip it"),
		textMsg("assistant", "Wrapped the parser so unsupported files are skipped."),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.LastAssistant != "Wrapped the parser so unsupported files are skipped." {
		t.Errorf("LastAssistant = %q, want the reply to the current prompt", h.LastAssistant)
	}
}

func TestProbeShowsPartialReplyDuringTheSameTurn(t *testing.T) {
	// An agent often narrates before calling tools. That text belongs to
	// the current prompt, so it should show even while it is still working.
	msgs := msgList(
		textMsg("user", "do the thing"),
		`{"id":"m","type":"assistant","agent":"build","model":{"id":"m1","providerID":"p1"},"time":{"created":1},`+
			`"content":[{"type":"text","text":"Looking into it now."},`+
			`{"type":"tool","id":"c","name":"shell","state":{"status":"running","input":{"command":"ls"},"metadata":{}},"time":{"created":1}}]}`,
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.LastAssistant != "Looking into it now." {
		t.Errorf("LastAssistant = %q, want the in-turn narration", h.LastAssistant)
	}
}

func TestProbeShowsAssistantTextWithNoUserMessageAtAll(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(textMsg("assistant", "standalone")))
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.LastAssistant != "standalone" {
		t.Errorf("LastAssistant = %q", h.LastAssistant)
	}
}

func TestProbeSincePromptMeasuresFromTheUserMessage(t *testing.T) {
	// One prompt produces many assistant messages, one per tool round trip.
	// The timer a person wants runs from their message, not from whichever
	// sub-turn happens to be generating.
	start := time.Now().Add(-5 * time.Minute).UnixMilli()
	msgs := msgList(
		fmt.Sprintf(`{"id":"u","type":"user","time":{"created":%d},"text":"do it"}`, start),
		asst(start+1000, fmt.Sprint(start+2000), 0, 0, 0, `,"finish":"stop"`),
		asst(time.Now().Add(-2*time.Second).UnixMilli(), "", 0, 0, 0, ""),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"})
	if h.SincePrompt < 4*time.Minute || h.SincePrompt > 6*time.Minute {
		t.Errorf("SincePrompt = %v, want ~5m (from the user's message)", h.SincePrompt)
	}
	// The per-message timer is the small resetting one, and must not be
	// confused with it.
	if h.Working > 30*time.Second {
		t.Errorf("Working = %v, want the current sub-turn only (~2s)", h.Working)
	}
}

func TestProbeSincePromptZeroWithNoUserMessage(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)))
	if h := Probe(context.Background(), "opencode", Conn{URL: srv.URL, Dir: "/w/mine"}); h.SincePrompt != 0 {
		t.Errorf("SincePrompt = %v, want 0", h.SincePrompt)
	}
}

// TestResolveFallsBackToInteractiveShellPATH reproduces the Hyprland/
// quickshell case: the process's own PATH lacks opencode, but an
// interactive (non-login) shell's PATH — set up by ~/.bashrc, the file
// bash only reads this way, and where opencode's own installer adds
// itself — finds it.
func TestResolveFallsBackToInteractiveShellPATH(t *testing.T) {
	resetShellPATHCacheForTest()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, cannot exercise the shell fallback")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	emptyDir := filepath.Join(tmp, "empty")
	homeDir := filepath.Join(tmp, "home")
	for _, d := range []string{binDir, emptyDir, homeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	fake := filepath.Join(binDir, "opencode")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rc := "export PATH=\"$PATH:" + binDir + "\"\n"
	if err := os.WriteFile(filepath.Join(homeDir, ".bashrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}

	// A process's own PATH does not see the fake opencode, nor does a login
	// shell (no .bash_profile to read it from): only an interactive
	// non-login shell, via HOME/.bashrc, does.
	t.Setenv("PATH", emptyDir)
	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", bashPath)

	got, err := (opencode{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want success via the shell fallback", err)
	}
	want, err := filepath.Abs(fake)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Resolve() = %q, want %q", got, want)
	}
}

// TestResolveFallsBackToLoginShellPATH covers the opposite layout: PATH set
// from a login shell's profile file rather than an interactive shell's rc.
func TestResolveFallsBackToLoginShellPATH(t *testing.T) {
	resetShellPATHCacheForTest()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found in PATH, cannot exercise the shell fallback")
	}

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	emptyDir := filepath.Join(tmp, "empty")
	homeDir := filepath.Join(tmp, "home")
	for _, d := range []string{binDir, emptyDir, homeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	fake := filepath.Join(binDir, "opencode")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho fake\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "export PATH=\"$PATH:" + binDir + "\"\n"
	if err := os.WriteFile(filepath.Join(homeDir, ".bash_profile"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	// A process's own PATH does not see the fake opencode: only a login
	// shell, via HOME/.bash_profile, does.
	t.Setenv("PATH", emptyDir)
	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", bashPath)

	got, err := (opencode{}).Resolve()
	if err != nil {
		t.Fatalf("Resolve() error = %v, want success via the login-shell fallback", err)
	}
	want, err := filepath.Abs(fake)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Resolve() = %q, want %q", got, want)
	}
}

// TestResolveReportsErrorWhenNotFoundAnywhere ensures the fallback does not
// swallow a genuine "not installed" error.
func TestResolveReportsErrorWhenNotFoundAnywhere(t *testing.T) {
	resetShellPATHCacheForTest()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not found in PATH, cannot exercise the shell fallback")
	}

	tmp := t.TempDir()
	emptyDir := filepath.Join(tmp, "empty")
	homeDir := filepath.Join(tmp, "home")
	for _, d := range []string{emptyDir, homeDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PATH", emptyDir)
	t.Setenv("HOME", homeDir)
	t.Setenv("SHELL", "bash")

	if _, err := (opencode{}).Resolve(); err == nil {
		t.Fatal("Resolve() error = nil, want an error when opencode is nowhere to be found")
	}
}

func TestMergePATHAppendsOnlyWhatIsMissing(t *testing.T) {
	sep := string(filepath.ListSeparator)
	base := "/shims" + sep + "/usr/bin"
	extra := "/usr/bin" + sep + "/home/u/.opencode/bin" + sep + "" + sep + "/usr/bin"

	got := MergePATH(base, extra)
	want := "/shims" + sep + "/usr/bin" + sep + "/home/u/.opencode/bin"
	if got != want {
		t.Errorf("MergePATH(%q, %q) = %q, want %q", base, extra, got, want)
	}
}

// base must keep its precedence: a toolchain pins tool versions by putting
// its shims first, and reordering here would silently resolve a different
// ruby or node than the project asked for.
func TestMergePATHKeepsBaseFirst(t *testing.T) {
	sep := string(filepath.ListSeparator)
	got := MergePATH("/shims", "/usr/bin"+sep+"/shims")
	if want := "/shims" + sep + "/usr/bin"; got != want {
		t.Errorf("MergePATH = %q, want %q", got, want)
	}
}

func TestMergePATHWithEmptySides(t *testing.T) {
	if got := MergePATH("", "/usr/bin"); got != "/usr/bin" {
		t.Errorf("MergePATH(\"\", %q) = %q, want %q", "/usr/bin", got, "/usr/bin")
	}
	if got := MergePATH("/usr/bin", ""); got != "/usr/bin" {
		t.Errorf("MergePATH(%q, \"\") = %q, want %q", "/usr/bin", got, "/usr/bin")
	}
	if got := MergePATH("", ""); got != "" {
		t.Errorf("MergePATH(\"\", \"\") = %q, want \"\"", got)
	}
}

// ShellPATH starts from a PATH that a few rc files have often each appended
// to, so base can arrive with duplicates of its own.
func TestMergePATHDropsDuplicatesWithinBase(t *testing.T) {
	sep := string(filepath.ListSeparator)
	got := MergePATH("/a"+sep+"/b"+sep+"/a", "/b")
	if want := "/a" + sep + "/b"; got != want {
		t.Errorf("MergePATH = %q, want %q", got, want)
	}
}
