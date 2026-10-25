// End-to-end tests of session persistence: writing to the log, resuming in a
// new process and recovering content the guardrail removed from the window.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Fechar() })
	return st
}

func fakeConfig(t *testing.T, url string) config.Config {
	t.Helper()
	return config.Config{BaseURL: url, Model: "fake", APIKey: "k", Janela: 100_000}
}

// ── NewSessionWithStore / Attach / ID ─────────────────────────────────────

func TestNewSessionWithStoreIndexesAndPersists(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101090000-abcd"
	s := NewSessionWithStore(config.Config{}, st, id)
	if s.ID() != id {
		t.Fatalf("ID() = %q, expected %q", s.ID(), id)
	}
	if !s.Persistente() {
		t.Fatal("a session with a store should be persistent")
	}
	// Without conversation, nothing goes to the log: the session is only born
	// with the first event.
	if _, err := st.GetSession(id); err == nil {
		t.Fatal("a session without events should not be indexed")
	}
	if _, err := st.StoreEvent(id, "user_message", map[string]any{"role": "user", "content": "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetSession(id); err != nil {
		t.Fatalf("session not indexed after the first event: %v", err)
	}
}

func TestAttachAfterNewSession(t *testing.T) {
	st := testStore(t)
	// NewSession keeps working WITHOUT a database (-p mode and tests).
	s := NewSession(config.Config{})
	if s.Persistente() {
		t.Fatal("NewSession should not have persistence")
	}
	if s.ID() != "" {
		t.Fatalf("ID() without store = %q", s.ID())
	}
	// Attach binds later: the REPL path when the database opens best-effort.
	s.Attach(st, "bh-20240101090000-ef01")
	if !s.Persistente() || s.ID() != "bh-20240101090000-ef01" {
		t.Fatalf("Attach did not bind: persistent=%v id=%q", s.Persistente(), s.ID())
	}
}

func TestAttachIgnoresBadInput(t *testing.T) {
	st := testStore(t)
	s := NewSession(config.Config{})
	s.Attach(nil, "bh-x") // nil store
	s.Attach(st, "   ")   // empty id
	if s.Persistente() || s.ID() != "" {
		t.Fatalf("Attach with bad input enabled persistence: %v %q", s.Persistente(), s.ID())
	}
}

// ── Send recording events ─────────────────────────────────────────────────

// single-turn provider: plain text, no tool.
func textProvider(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"content": text},
		}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestSendRecordsUserMessageAndAssistant(t *testing.T) {
	srv := textProvider(t, "model answer")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0001"

	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	var sb strings.Builder
	if err := s.Send(context.Background(), RendererDe(&sb), t.TempDir(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d; expected 2 (user_message + assistant): %+v", len(events), events)
	}
	if events[0].Kind != "user_message" {
		t.Fatalf("first event = %q, expected user_message", events[0].Kind)
	}
	if !strings.Contains(events[0].Payload, `"content":"hi"`) {
		t.Fatalf("user_message payload without the prompt: %s", events[0].Payload)
	}
	if events[1].Kind != "assistant" {
		t.Fatalf("second event = %q, expected assistant", events[1].Kind)
	}
	if !strings.Contains(events[1].Payload, `"content":"model answer"`) {
		t.Fatalf("assistant payload without the text: %s", events[1].Payload)
	}
}

// Pasted images go into the user_message payload and come back on resume.
func TestSendWithImagesPersistsAndResumes(t *testing.T) {
	srv := textProvider(t, "seen")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0009"
	img := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(img, []byte("\x89PNGfake"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	if err := s.SendWithImages(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "look [Image 1]", []string{img}); err != nil {
		t.Fatalf("SendWithImages: %v", err)
	}
	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || !strings.Contains(events[0].Payload, `"images"`) {
		t.Fatalf("user_message without images: %+v", events)
	}
	h := eventsToMessages(st, events)
	if len(h) == 0 || h[0].Role != "user" || len(h[0].Images) != 1 || h[0].Images[0] != img {
		t.Fatalf("resumed history lost the image: %+v", h)
	}
}

func TestSendWithoutStoreDoesNotRecord(t *testing.T) {
	srv := textProvider(t, "text")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101100000-0002"
	// Session WITHOUT a store: the turn works and the log stays untouched.
	s := NewSession(fakeConfig(t, srv.URL))
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hi"); err != nil {
		t.Fatalf("Send without store: %v", err)
	}
	if n, _ := st.ContarEventos(id); n != 0 {
		t.Fatalf("%d events recorded for a session without a store", n)
	}
}

func TestClearHistoryRecordsSessionReset(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101100000-0003"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.ClearHistory()
	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "session_reset" {
		t.Fatalf("ClearHistory should record session_reset, got %+v", events)
	}
}

// ── Resume rebuilding the transcript ──────────────────────────────────────

func TestResumeRebuildsTranscript(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0001"
	// First process: conversation with a tool call + result. Send only records
	// assistant when there is committed text (the empty event of a round that
	// ended in a tool call never enters the log), so the conversation here
	// follows that same rule.
	s := NewSessionWithStore(config.Config{}, st, id)
	events := []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "read the file"}},
		{"tool_call", map[string]any{
			"role": "tool_call", "tool_id": "call_1", "tool_name": "read",
			"tool_args": `{"path":"x.txt"}`,
		}},
		{"tool_result", map[string]any{
			"role": "tool_result", "tool_id": "call_1", "tool_name": "read",
			"content": "1: content",
		}},
		{"assistant", map[string]any{"role": "assistant", "content": "done, read it"}},
	}
	for _, e := range events {
		s.record(e.kind, e.payload)
	}

	// Second process: same session, transcript rebuilt from the log.
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := s2.History()
	if len(h) != 4 {
		t.Fatalf("history rebuilt with %d messages; expected 4:\n%+v", len(h), h)
	}
	if h[0].Role != "user" || h[0].Conteudo != "read the file" {
		t.Fatalf("message 0 wrong: %+v", h[0])
	}
	if h[1].Role != "tool_call" || h[1].ToolName != "read" || h[1].ToolArgs != `{"path":"x.txt"}` {
		t.Fatalf("tool_call did not come back intact: %+v", h[1])
	}
	if h[2].Role != "tool_result" || h[2].Conteudo != "1: content" {
		t.Fatalf("tool_result did not come back intact: %+v", h[2])
	}
	if h[3].Role != "assistant" || h[3].Conteudo != "done, read it" {
		t.Fatalf("final assistant wrong: %+v", h[3])
	}
}

// Observability events (guardrail, session_reset, metadata) do NOT enter the
// transcript: they are comments about the conversation, not the conversation.
func TestResumeIgnoresObservabilityEvents(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0009"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("user_message", map[string]any{"role": "user", "content": "hi"})
	s.record("guardrail", map[string]any{"tipo": "proativa", "janela": 100000})
	s.record("session_reset", map[string]any{"role": "system", "content": "history cleared"})
	s.record("assistant", map[string]any{"role": "assistant", "content": "hello"})

	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 2 {
		t.Fatalf("history = %d messages; expected 2 (content only): %+v", len(h), h)
	}
	if h[0].Role != "user" || h[1].Role != "assistant" {
		t.Fatalf("wrong order/roles: %+v", h)
	}
}

func TestResumeWithoutEventsLeavesEmpty(t *testing.T) {
	st := testStore(t)
	s := NewSessionWithStore(config.Config{}, st, "bh-20240101110000-0002")
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume of a session without events: %v", err)
	}
	if len(s.History()) != 0 {
		t.Fatalf("history should be empty, got %d", len(s.History()))
	}
}

func TestResumeRehydratesBlob(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0003"
	large := strings.Repeat("tool output line\n", 2000) // > 16 KiB

	s := NewSessionWithStore(config.Config{}, st, id)
	s.recordResult("call_9", "shell", large, false)

	// The recorded event is short: preview + blob reference.
	events, _ := st.ListEvents(id, 0, 0)
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if !strings.Contains(events[0].Payload, `"blob":`) {
		t.Fatalf("large result did not become a blob: %s", events[0].Payload)
	}
	if len(events[0].Payload) > 1024 {
		t.Fatalf("payload with blob has %d bytes", len(events[0].Payload))
	}

	// Resume returns the WHOLE content — that is the rehydration promise.
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 1 {
		t.Fatalf("history = %d messages", len(h))
	}
	if h[0].Conteudo != large {
		t.Fatalf("blob not rehydrated: got %d of %d bytes", len(h[0].Conteudo), len(large))
	}
}

// A turn interrupted with tool calls in flight kept the calls in the log
// without their results (Bruce, 2026-10-24: the resumed transcript carried
// two tool_use with no tool_result and EVERY later turn was rejected
// upstream — the client only saw silence and times out). Resume closes the
// gap with a synthetic cancelled result, and records it so the next resume
// does not repair again.
func TestResumeClosesOrphanToolCalls(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0004"
	s := NewSessionWithStore(config.Config{}, st, id)
	events := []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "read the files"}},
		{"tool_call", map[string]any{
			"role": "tool_call", "tool_id": "call_1", "tool_name": "read",
			"tool_args": `{"path":"a.txt"}`,
		}},
		{"tool_call", map[string]any{
			"role": "tool_call", "tool_id": "call_2", "tool_name": "glob",
			"tool_args": `{"pattern":"**/*.txt"}`,
		}},
		{"tool_result", map[string]any{
			"role": "tool_result", "tool_id": "call_1", "tool_name": "read",
			"content": "1: aaa",
		}},
	}
	for _, e := range events {
		s.record(e.kind, e.payload)
	}

	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := s2.History()
	if len(h) != 5 {
		t.Fatalf("history = %d messages; want 4 replayed + 1 synthetic:\n%+v", len(h), h)
	}
	if h[3].Role != "tool_result" || h[3].ToolID != "call_1" || h[3].Conteudo != "1: aaa" {
		t.Fatalf("the real result changed position: %+v", h[3])
	}
	if h[4].Role != "tool_result" || h[4].ToolID != "call_2" || h[4].ToolName != "glob" || !h[4].IsErro {
		t.Fatalf("synthetic result is not the cancelled call_2: %+v", h[4])
	}
	if !strings.Contains(h[4].Conteudo, "interrupted") {
		t.Fatalf("synthetic result without the interruption note: %q", h[4].Conteudo)
	}

	// The repair entered the log: a third process must rebuild the SAME
	// transcript without appending new synthetic results again.
	eventsAfter, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	nResults := 0
	for _, e := range eventsAfter {
		if e.Kind == "tool_result" {
			nResults++
		}
	}
	if nResults != 2 {
		t.Fatalf("%d tool_result events after the repair; want 2", nResults)
	}
	s3 := NewSessionWithStore(config.Config{}, st, id)
	if err := s3.Resume(); err != nil {
		t.Fatalf("third process Resume: %v", err)
	}
	if len(s3.History()) != 5 {
		t.Fatalf("third process rebuilt %d messages (re-repaired?):\n%+v", len(s3.History()), s3.History())
	}
}

// A tool_result whose tool_call never entered the log must NOT be repaired:
// the runtime never writes results without their calls (Send commits both
// ends together), and observability-first tools (TestResumeRehydratesBlob
// above, context stats) rely on a lone tool_result staying intact. Only a
// MISSING result is repaired (TestResumeClosesOrphanToolCalls), because that
// is what makes the resumed transcript invalid for providers that validate
// the pairing.
func TestResumeKeepsLoneResult(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101110000-0005"
	s := NewSessionWithStore(config.Config{}, st, id)
	events := []struct {
		kind    string
		payload map[string]any
	}{
		{"user_message", map[string]any{"role": "user", "content": "hi"}},
		{"tool_result", map[string]any{
			"role": "tool_result", "tool_id": "call_ghost", "tool_name": "read",
			"content": "1: lone",
		}},
	}
	for _, e := range events {
		s.record(e.kind, e.payload)
	}
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	h := s2.History()
	if len(h) != 2 || h[1].Role != "tool_result" || h[1].Conteudo != "1: lone" {
		t.Fatalf("lone tool_result was rewritten: %+v", h)
	}
}

// ── Search / Retrieve ─────────────────────────────────────────────────────

func TestSearchAndRetrieve(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101120000-0001"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("user_message", map[string]any{"role": "user", "content": "I need the path of FileX"})
	toolSeq, _ := st.StoreEvent(id, "tool_result", map[string]any{
		"role": "tool_result", "tool_name": "read", "content": "C:\\data\\FileX.txt",
	})

	// Case-insensitive search over the content field.
	hits := s.Search("filex", 10)
	if len(hits) == 0 {
		t.Fatal("Search found nothing")
	}
	var foundTool bool
	for _, h := range hits {
		if h.Kind == "tool_result" {
			foundTool = true
		}
	}
	if !foundTool {
		t.Fatalf("Search did not find the tool_result: %+v", hits)
	}

	// Retrieve returns the full event.
	e, err := s.Retrieve(toolSeq)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if e.Seq != toolSeq {
		t.Fatalf("Retrieve returned seq %d, expected %d", e.Seq, toolSeq)
	}
	if !strings.Contains(e.Payload, `C:\\data\\FileX.txt`) {
		t.Fatalf("Retrieve lost the content: %s", e.Payload)
	}
	if _, err := s.Retrieve(99999); err == nil {
		t.Fatal("Retrieve of a missing seq should fail")
	}
}

func TestSearchWithoutPersistenceReturnsNil(t *testing.T) {
	s := NewSession(config.Config{})
	if hits := s.Search("anything", 10); hits != nil {
		t.Fatalf("Search without store returned %+v", hits)
	}
	if _, err := s.Retrieve(1); err == nil {
		t.Fatal("Retrieve without store should fail")
	}
	// Resume without store is a no-op, not an error.
	if err := s.Resume(); err != nil {
		t.Fatalf("Resume without store: %v", err)
	}
}

// ── the model's tools ─────────────────────────────────────────────────────

func TestSessionSpecsSearchAndRetrieve(t *testing.T) {
	st := testStore(t)
	const id = "bh-20240101130000-0001"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.record("tool_result", map[string]any{
		"role": "tool_result", "tool_name": "shell",
		"content": "exit_code: 0\ngit status --short",
	})

	specs := SessaoSpecs(s)
	if len(specs) != 3 {
		t.Fatalf("SessaoSpecs returned %d specs; expected 3", len(specs))
	}
	byName := map[string]tools.Spec{}
	for _, sp := range specs {
		byName[sp.Name] = sp
	}
	search, ok := byName["session_search"]
	if !ok {
		t.Fatal("session_search is not in the specs")
	}
	if !strings.Contains(strings.ToLower(search.Description), "compact") {
		t.Fatalf("session_search description does not mention compaction:\n%s", search.Description)
	}
	rtr, ok := byName["session_retrieve"]
	if !ok {
		t.Fatal("session_retrieve is not in the specs")
	}
	list, ok := byName["session_list"]
	if !ok {
		t.Fatal("session_list is not in the specs")
	}
	if !strings.Contains(strings.ToLower(list.Description), "metadata only") {
		t.Fatalf("session_list description must say it lists metadata only:\n%s", list.Description)
	}
	if !strings.Contains(strings.ToLower(rtr.Description), "seq") {
		t.Fatalf("session_retrieve description does not mention seq:\n%s", rtr.Description)
	}
	// Schemas with the promised parameters.
	if _, ok := search.Schema["properties"].(map[string]any)["query"]; !ok {
		t.Fatal("session_search without query parameter")
	}
	if _, ok := search.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_search without limit parameter")
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["seq"]; !ok {
		t.Fatal("session_retrieve without seq parameter")
		// Optional sessionID: omitted means THIS session. The scope must be
		// stated in the parameter, not left to the tool name.
		for _, sp := range []tools.Spec{search, rtr} {
			p, ok := sp.Schema["properties"].(map[string]any)["sessionID"]
			if !ok {
				t.Fatalf("%s without sessionID parameter", sp.Name)
			}
			desc, _ := p.(map[string]any)["description"].(string)
			if !strings.Contains(strings.ToLower(desc), "this session") {
				t.Fatalf("%s sessionID must say it defaults to this session: %q", sp.Name, desc)
			}
		}
	}
	if _, ok := rtr.Schema["properties"].(map[string]any)["limit"]; !ok {
		t.Fatal("session_retrieve without limit parameter")
	}

	// Exec of session_search finds the recorded content.
	output, err := search.Exec(nil, t.TempDir(), json.RawMessage(`{"query":"git status"}`))
	if err != nil {
		t.Fatalf("exec session_search: %v", err)
	}
	if !strings.Contains(output, "git status") {
		t.Fatalf("session_search did not return the excerpt:\n%s", output)
	}

	// Exec of session_retrieve returns the whole event.
	output, err = rtr.Exec(nil, t.TempDir(), json.RawMessage(`{"seq":1}`))
	if err != nil {
		t.Fatalf("exec session_retrieve: %v", err)
	}
	if !strings.Contains(output, "shell") {
		t.Fatalf("session_retrieve did not return the event:\n%s", output)
	}
}

func TestSessionSpecsWithoutPersistenceExplain(t *testing.T) {
	s := NewSession(config.Config{})
	for _, sp := range SessaoSpecs(s) {
		args := `{}`
		if sp.Name == "session_search" {
			args = `{"query":"x"}`
		} else if sp.Name == "session_retrieve" {
			args = `{"seq":1}`
		} else if sp.Name == "session_list" {
			args = `{}`
		}
		output, err := sp.Exec(nil, t.TempDir(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s without store returned an error: %v", sp.Name, err)
		}
		if !strings.Contains(output, "persistence") {
			t.Fatalf("%s without store should explain there is no log:\n%s", sp.Name, output)
		}
	}
}

// Send includes the session tools in the catalog sent to the provider.
func TestSendExposesSessionToolsInSchema(t *testing.T) {
	var sawSearch, sawRetrieve, sawList bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, tl := range req.Tools {
			switch tl.Function.Name {
			case "session_search":
				sawSearch = true
			case "session_retrieve":
				sawRetrieve = true
			case "session_list":
				sawList = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	st := testStore(t)
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, "bh-20240101130000-0002")
	_ = s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hi")
	if !sawSearch || !sawRetrieve || !sawList {
		t.Fatalf("provider did not see the session tools (search=%v retrieve=%v list=%v)", sawSearch, sawRetrieve, sawList)
	}
}

// A resumed session already knows the context size: it is the prompt_tokens
// of the last request left in the transcript.
func TestLastContext(t *testing.T) {
	s := NewSession(config.Config{})
	if s.UltimoContexto() != 0 {
		t.Fatal("an empty session should have context 0")
	}
	s.history = []dealer.Mensagem{
		{Role: "user", Conteudo: "hi"},
		{Role: "assistant", Conteudo: "a", Usage: &dealer.Usage{PromptTokens: 100}},
		{Role: "user", Conteudo: "again"},
		{Role: "assistant", Conteudo: "b", Usage: &dealer.Usage{PromptTokens: 15139}},
		{Role: "user", Conteudo: "question without an answer yet"},
	}
	if got := s.UltimoContexto(); got != 15139 {
		t.Fatalf("UltimoContexto = %d", got)
	}
}

// A large result in a session without a database (the subagent case) must
// not crash the process.
func TestLargeResultWithoutStoreDoesNotCrash(t *testing.T) {
	s := NewSession(config.Config{})
	if p := s.recordResult("c1", "read", strings.Repeat("x", blobThreshold+10), false); p != nil {
		t.Fatalf("without store it should not record: %v", p)
	}
}

// ── cross-session log tools ───────────────────────────────────────────────

// session_search/session_retrieve read another session when given its id, and
// session_list returns the ids (metadata only) that make that possible.
func TestLogToolsReadAnotherSession(t *testing.T) {
	st := testStore(t)
	// Two sessions, each with one event to find.
	other := NewSessionWithStore(config.Config{}, st, "bh-20240102090000-aaaa")
	other.record("user_message", map[string]any{"role": "user", "content": "the connector was restarted"})
	mine := NewSessionWithStore(config.Config{}, st, "bh-20240103090000-bbbb")
	mine.record("user_message", map[string]any{"role": "user", "content": "my own note about the mitra connector"})

	specs := SessaoSpecs(mine)
	byName := map[string]tools.Spec{}
	for _, sp := range specs {
		byName[sp.Name] = sp
	}
	run := func(name, args string) string {
		out, err := byName[name].Exec(context.Background(), t.TempDir(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s(%s): %v", name, args, err)
		}
		return out
	}

	// session_list names the other session and marks mine.
	list := run("session_list", `{}`)
	if !strings.Contains(list, "bh-20240102090000-aaaa") || !strings.Contains(list, "[current]") {
		t.Fatalf("session_list must list the other id and mark the current one:\n%s", list)
	}
	if strings.Contains(list, "connector was restarted") {
		t.Fatal("session_list returns metadata only: no conversation content")
	}

	// Default (omitted id) searches MY session only.
	def := run("session_search", `{"query":"connector"}`)
	if !strings.Contains(def, "my own note") || strings.Contains(def, "was restarted") {
		t.Fatalf("omitting sessionID must search this session:\n%s", def)
	}
	if !strings.Contains(def, "current") {
		t.Fatalf("a result must name the session it came from:\n%s", def)
	}

	// With the id, the other session's log is reachable, and the full event
	// comes back from session_retrieve with the same id.
	cross := run("session_search", `{"query":"connector","sessionID":"bh-20240102090000-aaaa"}`)
	if !strings.Contains(cross, "was restarted") || strings.Contains(cross, "my own note") {
		t.Fatalf("sessionID must read that session:\n%s", cross)
	}
	// The seq comes from a "seq N [...]" line, not from the header.
	seq := 0
	for _, line := range strings.Split(cross, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "seq ") {
			fmt.Sscanf(line[4:], "%d", &seq)
			break
		}
	}
	if seq == 0 {
		t.Fatalf("could not read the seq back:\n%s", cross)
	}
	got := run("session_retrieve", fmt.Sprintf(`{"seq":%d,"sessionID":"bh-20240102090000-aaaa"}`, seq))
	if !strings.Contains(got, "connector was restarted") {
		t.Fatalf("session_retrieve of another session:\n%s", got)
	}

	// A wrong id is an honest "no session", not a crash.
	bad := run("session_search", `{"query":"x","sessionID":"nope"}`)
	if !strings.Contains(bad, "no session with id") {
		t.Fatalf("unknown id:\n%s", bad)
	}
}

// Regression (conversation 100135, session bh-20261024224403-789b, 2026-10-25):
// after a process restart Resume rehydrated the whole log (the guardrail's
// in-memory stubs never reach it), and most of the weight sat in tool_call
// arguments and results under 2 KiB — nothing prune can stub. The turn died
// with "pruning did not free enough". The guardrail now forces a mechanical
// compaction, persisted so the next Resume starts small too.
func TestGuardrailForcesCompactionWhenPruneCannot(t *testing.T) {
	srv := textProvider(t, "back on track")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101140000-0003"
	cfg := fakeConfig(t, srv.URL)
	cfg.Janela = 40_000

	seed := NewSessionWithStore(cfg, st, id)
	seed.record("user_message", map[string]any{"role": "user", "content": "first request: fix the gateway"})
	for i := 0; i < 60; i++ {
		cid := fmt.Sprintf("c%d", i)
		seed.record("tool_call", map[string]any{"role": "tool_call", "tool_id": cid, "tool_name": "write",
			"tool_args": `{"content":"` + strings.Repeat("a", 4000) + `"}`})
		seed.record("tool_result", map[string]any{"role": "tool_result", "tool_id": cid, "tool_name": "write", "content": "ok"})
	}
	seed.record("assistant", map[string]any{"role": "assistant", "content": "gateway fixed, flag removed"})

	s := NewSessionWithStore(cfg, st, id)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "keep the caller check"); err != nil {
		t.Fatalf("turn should survive through a forced compaction: %v", err)
	}
	h := s.History()
	if len(h) == 0 || !isSummary(h[0].Conteudo) {
		t.Fatalf("history should start with the forced state, got %+v", h[:min(len(h), 2)])
	}
	if !strings.Contains(h[0].Conteudo, "gateway fixed, flag removed") || !strings.Contains(h[0].Conteudo, "first request") {
		t.Fatalf("forced state lost the conversation's words:\n%s", h[0].Conteudo)
	}

	// A new process resumes from the compaction, not from the full log.
	again := NewSessionWithStore(cfg, st, id)
	if err := again.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := historyBytes(again.History()); got > 40_000 {
		t.Fatalf("resume should re-apply the forced compaction, history has %d bytes", got)
	}
}

func TestForcedStateKeepsNewestWithinCap(t *testing.T) {
	var h []dealer.Mensagem
	h = append(h, dealer.Mensagem{Role: "user", Conteudo: summaryPrefix + "old state"})
	for i := 0; i < 200; i++ {
		h = append(h, dealer.Mensagem{Role: "assistant", Conteudo: fmt.Sprintf("note %03d %s", i, strings.Repeat("é", 300))})
	}
	h = append(h, dealer.Mensagem{Role: "user", Conteudo: "current"})
	got := forcedState(h)
	if len(got) > maxForcedState {
		t.Fatalf("state over the cap: %d", len(got))
	}
	if !strings.Contains(got, "old state") || !strings.Contains(got, "note 199") || strings.Contains(got, "note 000") {
		t.Fatalf("expected previous state + newest notes only")
	}
	if forcedState([]dealer.Mensagem{{Role: "user", Conteudo: "only"}}) != "" {
		t.Fatal("nothing before the current request: no state")
	}
}
