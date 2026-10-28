package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// notaRenderer records Nota, the live tool events and Resposta in order.
type notaRenderer struct {
	recRenderer
	mu  sync.Mutex
	got []string
}

func (r *notaRenderer) note(e string) {
	r.mu.Lock()
	r.got = append(r.got, e)
	r.mu.Unlock()
}

func (r *notaRenderer) Nota(text string)                 { r.note("nota:" + text) }
func (r *notaRenderer) Resposta(text string)             { r.note("resposta:" + text) }
func (r *notaRenderer) ToolComecou(_, name, args string) { r.note("start:" + name + ":" + args) }
func (r *notaRenderer) ToolSaida(string, string)         {}
func (r *notaRenderer) ToolTerminou(_, name, _, _ string, _ time.Duration, _ bool) {
	r.note("end:" + name)
}

// textAndToolProvider answers the first round with text AND a tool call (the
// "let me check X" shape), the second with the final text.
func textAndToolProvider() *httptest.Server {
	var mu sync.Mutex
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1))
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) { b, _ := json.Marshal(p); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		if first {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "let me check"}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "c0", "type": "function",
				"function": map[string]any{"name": "probe", "arguments": `{"path":"a.go"}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// Text written alongside tool calls reaches the renderer as Nota BEFORE the
// tool runs, and never as Resposta (which stays the final answer only).
func TestIntermediateTextBecomesNotaBeforeTools(t *testing.T) {
	srv := textAndToolProvider()
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	s.Injetar(tools.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil }})
	rr := &notaRenderer{}
	if err := s.Send(context.Background(), rr, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	want := `nota:let me check,start:probe:{"path":"a.go"},end:probe,resposta:done`
	if got := strings.Join(rr.got, ","); got != want {
		t.Fatalf("events:\n got %q\nwant %q", got, want)
	}
}

// The intermediate text is persisted with its calls: a resumed session
// (session/load, /resume) must show the model it spoke between tools, or it
// learns from its own history to run tools in silence.
func TestIntermediateTextSurvivesResume(t *testing.T) {
	srv := textAndToolProvider()
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101120000-0001"
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	s.Injetar(tools.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) { return "ok", nil }})
	if err := s.Send(context.Background(), &notaRenderer{}, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range s2.History() {
		roles = append(roles, m.Role+":"+m.Conteudo+m.ToolName)
	}
	want := "user:go,assistant:let me check,tool_call:probe,tool_result:okprobe,assistant:done"
	if got := strings.Join(roles, ","); got != want {
		t.Fatalf("resumed history:\n got %q\nwant %q", got, want)
	}
}
