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

// msgWithID builds an assistant message carrying an explicit "id" and,
// optionally, extra top-level fields (e.g. "error":{...}) — needed to
// exercise classify's staleness check, which matches a question or
// permission's tool.messageID against a specific message.
func msgWithID(id string, created int64, completed string, extra string) string {
	comp := ""
	if completed != "" {
		comp = `,"completed":` + completed
	}
	return fmt.Sprintf(`{"info":{"id":%q,"role":"assistant","modelID":"m1","time":{"created":%d%s}%s}}`,
		id, created, comp, extra)
}

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
	return fakeServerT(t, sessions, messages, status, permissions, questions, `[]`)
}

func fakeServerT(t *testing.T, sessions, messages, status, permissions, questions, todos string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(sessions))
	})
	mux.HandleFunc("/session/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/todo") {
			_, _ = w.Write([]byte(todos))
			return
		}
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

// Sessions carry their own token and cost totals, exactly as the real
// /api/session list does.
func sess(id, dir string, updated int64, parent string, in, out int, cost float64) string {
	pid := ""
	if parent != "" {
		pid = fmt.Sprintf(`,"parentID":%q`, parent)
	}
	return fmt.Sprintf(`{"id":%q,"location":{"directory":%q},"time":{"updated":%d}%s,`+
		`"tokens":{"input":%d,"output":%d,"reasoning":0,"cache":{"read":0,"write":0}},"cost":%v}`,
		id, dir, updated, pid, in, out, cost)
}

func sessList(ss ...string) string { return `{"data":[` + strings.Join(ss, ",") + `]}` }

var twoDirSessions = sessList(
	sess("ses_mine", "/w/mine", 200, "", 11, 5, 0.25),
	sess("ses_old", "/w/mine", 100, "", 0, 0, 0),
	sess("ses_other", "/w/other", 300, "", 0, 0, 0),
)

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

func TestProbeIgnoresAStaleQuestionFromAnAbortedTurn(t *testing.T) {
	// Regression test: opencode crashing (or being killed) mid-question can
	// leave /question listing that request forever — there is no live TUI
	// left to answer it, and nothing times it out. Once the message the
	// request names has actually completed (here, aborted) and the
	// conversation has moved on, the request is stale and must not keep
	// reporting the agent as blocked.
	msgs := msgList(
		msgWithID("msg_aborted", 1, "2", `,"error":{"name":"MessageAbortedError","data":{"message":"Aborted"}}`),
		asst(3, "4", 0, 0, 0, `,"finish":"stop"`), // the conversation continued past it
	)
	questions := `[{"sessionID":"ses_mine","tool":{"messageID":"msg_aborted"},` +
		`"questions":[{"header":"Stale","question":"Still here?","options":[]}]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, `[]`, questions)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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
	questions := `[{"sessionID":"ses_mine","tool":{"messageID":"msg_open"},` +
		`"questions":[{"header":"Pick","question":"Which?","options":[]}]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, `[]`, questions)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting; the question's message is still open", h.State)
	}
}

func TestProbeIgnoresAStalePermissionFromAnAbortedTurn(t *testing.T) {
	// Same bug, on the permission side: a crash mid-permission-request can
	// leave /permission listing it forever even though its turn is long
	// over.
	msgs := msgList(
		msgWithID("msg_aborted", 1, "2", `,"error":{"name":"MessageAbortedError","data":{"message":"Aborted"}}`),
		asst(3, "4", 0, 0, 0, `,"finish":"stop"`),
	)
	perms := `[{"sessionID":"ses_mine","tool":{"messageID":"msg_aborted"},"permission":"bash","patterns":["rm -rf /"]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateIdle {
		t.Errorf("State = %q, want idle; the permission's turn already ended", h.State)
	}
	if h.Pending != nil {
		t.Errorf("Pending = %+v, want nil for a stale permission", h.Pending)
	}
}

func TestProbeHonoursAPermissionStillOpen(t *testing.T) {
	msgs := msgList(msgWithID("msg_open", 1, "", ""))
	perms := `[{"sessionID":"ses_mine","tool":{"messageID":"msg_open"},"permission":"bash","patterns":["echo hi"]}]`
	srv := fakeServerQ(t, twoDirSessions, msgs, `{}`, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.State != StateWaiting {
		t.Errorf("State = %q, want waiting; the permission's message is still open", h.State)
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

func TestProbeExposesModelVariantAndProvider(t *testing.T) {
	msgs := msgList(`{"info":{"role":"assistant","modelID":"claude-sonnet-5","variant":"high",` +
		`"providerID":"github-copilot","time":{"created":1,"completed":2},"finish":"stop"}}`)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

func TestProbeExcludesSubagentSessionsFromTheCount(t *testing.T) {
	// The Task tool gives every subagent its own session in the same
	// directory, so one conversation with three helpers must not read as
	// four separate things you started.
	sessions := sessList(
		sess("ses_root", "/w/mine", 100, "", 10, 10, 1.0),
		sess("ses_sub1", "/w/mine", 101, "ses_root", 20, 20, 2.0),
		sess("ses_sub2", "/w/mine", 102, "ses_root", 30, 30, 3.0),
	)
	srv := fakeServer(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)))

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Worked != 5*time.Second {
		t.Errorf("Worked = %v, want 5s from the conversation's own turns only", h.Worked)
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
	// User messages must not be mistaken for turns when finding the current
	// one, and a non-error finish on an older message must not be reported
	// as an error.
	msgs := msgList(
		asst(1, "2", 5, 5, 0.05, `,"finish":"tool-calls"`),
		`{"info":{"role":"user","time":{"created":2}}}`,
		asst(3, "4", 10, 10, 0.1, `,"finish":"stop"`),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Worked != 2*time.Millisecond {
		t.Errorf("Worked = %v, want 2ms across the two completed turns", h.Worked)
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

func TestProbeReadsTodos(t *testing.T) {
	todos := `[
      {"content":"Verify schemas","status":"completed","priority":"high"},
      {"content":"Fix parsing","status":"completed","priority":"high"},
      {"content":"Wire the widget","status":"in_progress","priority":"medium"},
      {"content":"Update README","status":"pending","priority":"low"},
      {"content":"Dropped idea","status":"cancelled","priority":"low"}
    ]`
	srv := fakeServerT(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)),
		`{}`, `[]`, `[]`, todos)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

// A todo list opencode reports as fully resolved (nothing in_progress or
// pending) describes a task that has already wrapped up. If the
// conversation has since moved on — a new user prompt arrived with no
// further todowrite call — the list belongs to finished, unrelated work and
// must not keep reporting as if it were live progress on whatever the agent
// is doing now.
func TestProbeClearsStaleCompletedTodos(t *testing.T) {
	allDone := `[
      {"content":"Verify schemas","status":"completed","priority":"high"},
      {"content":"Fix parsing","status":"completed","priority":"high"}
    ]`
	msgs := msgList(
		msgWithTool("todowrite", "completed", "", 1),
		textMsg("user", "totally different follow-up ask"),
	)
	srv := fakeServerT(t, twoDirSessions, msgs, `{}`, `[]`, `[]`, allDone)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Todos.Total != 0 {
		t.Errorf("Todos = %+v, want zeroed once a newer prompt makes the finished list stale", h.Todos)
	}
}

// The same finished list, but with no prompt after the last todowrite call,
// must still be reported: the task just finished and nothing has moved on.
func TestProbeKeepsCompletedTodosWithoutNewerPrompt(t *testing.T) {
	allDone := `[
      {"content":"Verify schemas","status":"completed","priority":"high"},
      {"content":"Fix parsing","status":"completed","priority":"high"}
    ]`
	msgs := msgList(
		textMsg("user", "do the thing"),
		msgWithTool("todowrite", "completed", "", 1),
	)
	srv := fakeServerT(t, twoDirSessions, msgs, `{}`, `[]`, `[]`, allDone)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Todos.Total != 2 || h.Todos.Completed != 2 {
		t.Errorf("Todos = %+v, want the just-finished list still reported", h.Todos)
	}
}

// A list that still has work outstanding must be kept even across a newer
// prompt: follow-up guidance on an in-flight task does not make that task's
// own list stale.
func TestProbeKeepsInProgressTodosDespiteNewerPrompt(t *testing.T) {
	stillGoing := `[
      {"content":"Verify schemas","status":"completed","priority":"high"},
      {"content":"Wire the widget","status":"in_progress","priority":"medium"}
    ]`
	msgs := msgList(
		msgWithTool("todowrite", "completed", "", 1),
		textMsg("user", "also handle the edge case"),
	)
	srv := fakeServerT(t, twoDirSessions, msgs, `{}`, `[]`, `[]`, stillGoing)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Todos.Total != 2 || h.Todos.Current != "Wire the widget" {
		t.Errorf("Todos = %+v, want the in-progress list kept", h.Todos)
	}
}

func TestProbeNoTodosIsZeroed(t *testing.T) {
	srv := fakeServerFull(t, twoDirSessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), `{}`, `[]`)
	h := Probe(context.Background(), srv.URL, "/w/mine")
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

// msgWithTool builds an assistant message carrying one tool part.
func msgWithTool(tool, status, title string, start int64) string {
	return fmt.Sprintf(`{"info":{"role":"assistant","modelID":"m1","time":{"created":1}},`+
		`"parts":[{"type":"text"},{"type":"tool","tool":%q,"state":{"status":%q,"title":%q,"time":{"start":%d}}}]}`,
		tool, status, title, start)
}

func TestProbeReportsTheRunningToolAsActivity(t *testing.T) {
	srv := fakeServer(t, twoDirSessions,
		msgList(msgWithTool("bash", "running", "bin/rails test", 1700000000000)))

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity == nil {
		t.Fatal("Activity = nil, want the running tool call")
	}
	if h.Activity.Tool != "bash" || h.Activity.Title != "bin/rails test" {
		t.Errorf("Activity = %+v", h.Activity)
	}
	if h.Activity.Since.UnixMilli() != 1700000000000 {
		t.Errorf("Since = %v", h.Activity.Since)
	}
}

func TestProbeTreatsPendingToolsAsActivity(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(msgWithTool("edit", "pending", "app/models/user.rb", 1)))
	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity == nil || h.Activity.Tool != "edit" {
		t.Errorf("Activity = %+v, want the pending call", h.Activity)
	}
}

func TestProbeNoActivityWhenToolsHaveFinished(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(msgWithTool("bash", "completed", "ls", 1)))
	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity != nil {
		t.Errorf("Activity = %+v, want nil once the call has completed", h.Activity)
	}
}

func TestProbeActivityIgnoresEarlierTurns(t *testing.T) {
	// Only the newest turn can have a call in flight; a stale "running"
	// left on an older message must not be reported as current.
	srv := fakeServer(t, twoDirSessions, msgList(
		msgWithTool("bash", "running", "old and stuck", 1),
		msgWithTool("grep", "completed", "done", 2),
	))
	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity != nil {
		t.Errorf("Activity = %+v, want nil (the newest turn has nothing running)", h.Activity)
	}
}

func TestProbeActivityTitleIsTrimmedToOneLine(t *testing.T) {
	srv := fakeServer(t, twoDirSessions,
		msgList(msgWithTool("bash", "running", "cd /somewhere \u0026\u0026 \\n  bin/rails test \\n  --verbose", 1)))
	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity == nil {
		t.Fatal("Activity = nil")
	}
	if strings.Contains(h.Activity.Title, "\n") {
		t.Errorf("Title still multi-line: %q", h.Activity.Title)
	}
}

func TestProbeActivityFallsBackToInputWhileRunning(t *testing.T) {
	// A running call has no title yet — only its arguments — and that is
	// exactly the call worth describing.
	msgs := msgList(`{"info":{"role":"assistant","modelID":"m1","time":{"created":1}},` +
		`"parts":[{"type":"tool","tool":"bash","state":{"status":"running",` +
		`"input":{"command":"bin/rails test","timeout":120000},"time":{"start":5}}}]}`)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.Activity == nil {
		t.Fatal("Activity = nil")
	}
	if h.Activity.Title != "bin/rails test" {
		t.Errorf("Title = %q, want the command from the input", h.Activity.Title)
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
	return fmt.Sprintf(`{"info":{"role":%q,"modelID":"m1","time":{"created":1,"completed":2}},`+
		`"parts":[{"type":"text","text":%q}]}`, role, text)
}

func TestProbeReportsLastMessageFromEachSide(t *testing.T) {
	msgs := msgList(
		textMsg("user", "first thing I asked"),
		textMsg("assistant", "first reply"),
		textMsg("user", "cancel workflow shouldn't be here"),
		textMsg("assistant", "Removed the cancel button from the onboarding nav."),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.LastAssistant != "real answer" {
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
	perms := `[{"sessionID":"ses_sub","permission":"external_directory","patterns":["/gems/ruby_llm/*"]}]`
	srv := fakeServerQ(t, sessions, msgList(asst(1, "", 0, 0, 0, "")), `{}`, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

func TestProbeIgnoresPendingFromAnUnrelatedConversation(t *testing.T) {
	// Widening the match to the family must not widen it to the whole
	// server: another conversation's prompt is not this one's problem.
	sessions := sessList(
		sess("ses_root", "/w/mine", 200, "", 0, 0, 0),
		sess("ses_other_root", "/w/mine", 100, "", 0, 0, 0),
		sess("ses_other_sub", "/w/mine", 50, "ses_other_root", 0, 0, 0),
	)
	perms := `[{"sessionID":"ses_other_sub","permission":"bash","patterns":[]}]`
	srv := fakeServerQ(t, sessions, msgList(asst(1, "2", 0, 0, 0, `,"finish":"stop"`)), `{}`, perms, `[]`)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

	h := Probe(context.Background(), srv.URL, "/w/mine")
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

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.LastAssistant != "Wrapped the parser so unsupported files are skipped." {
		t.Errorf("LastAssistant = %q, want the reply to the current prompt", h.LastAssistant)
	}
}

func TestProbeShowsPartialReplyDuringTheSameTurn(t *testing.T) {
	// An agent often narrates before calling tools. That text belongs to
	// the current prompt, so it should show even while it is still working.
	msgs := msgList(
		textMsg("user", "do the thing"),
		`{"info":{"role":"assistant","modelID":"m1","time":{"created":1}},`+
			`"parts":[{"type":"text","text":"Looking into it now."},`+
			`{"type":"tool","tool":"bash","state":{"status":"running","input":{"command":"ls"},"time":{"start":1}}}]}`,
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
	if h.LastAssistant != "Looking into it now." {
		t.Errorf("LastAssistant = %q, want the in-turn narration", h.LastAssistant)
	}
}

func TestProbeShowsAssistantTextWithNoUserMessageAtAll(t *testing.T) {
	srv := fakeServer(t, twoDirSessions, msgList(textMsg("assistant", "standalone")))
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.LastAssistant != "standalone" {
		t.Errorf("LastAssistant = %q", h.LastAssistant)
	}
}

func TestProbeSincePromptMeasuresFromTheUserMessage(t *testing.T) {
	// One prompt produces many assistant messages, one per tool round trip.
	// The timer a person wants runs from their message, not from whichever
	// sub-turn happens to be generating.
	start := time.Now().Add(-5 * time.Minute).UnixMilli()
	msgs := msgList(
		fmt.Sprintf(`{"info":{"role":"user","time":{"created":%d}},"parts":[{"type":"text","text":"do it"}]}`, start),
		asst(start+1000, fmt.Sprint(start+2000), 0, 0, 0, `,"finish":"stop"`),
		asst(time.Now().Add(-2*time.Second).UnixMilli(), "", 0, 0, 0, ""),
	)
	srv := fakeServer(t, twoDirSessions, msgs)

	h := Probe(context.Background(), srv.URL, "/w/mine")
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
	if h := Probe(context.Background(), srv.URL, "/w/mine"); h.SincePrompt != 0 {
		t.Errorf("SincePrompt = %v, want 0", h.SincePrompt)
	}
}
