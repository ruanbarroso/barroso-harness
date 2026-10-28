package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// failingAfterToolProvider scripts the gateway of conversation 92120: the
// first request asks for a tool, the second fails terminally (a non-retryable
// status, so the test does not sit through the backoff ladder), and every
// request after that answers with text. It records the messages of each
// request so the test can see what the model was given.
func failingAfterToolProvider(t *testing.T) (*httptest.Server, func() [][]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var reqs [][]map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, req.Messages)
		mu.Unlock()
		emit := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		switch n {
		case 0:
			w.Header().Set("Content-Type", "text/event-stream")
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "probe", "arguments": `{}`},
			}}}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"gateway exploded"}}`))
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "continuing"}, "finish_reason": "stop"}}})
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() [][]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([][]map[string]any(nil), reqs...)
	}
}

func probeTool() tools.Spec {
	return tools.Spec{Name: "probe", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) { return "probe-output-42", nil }}
}

// Regression (conversation 92120, 2026-10-23): a turn that failed at the
// gateway after doing work was discarded whole, and the next message reached
// the model without the request it was meant to continue. The failed turn
// must stay: prompt, tool call and tool result all go into the next request.
func TestFailedTurnKeepsPromptAndCompletedRounds(t *testing.T) {
	srv, requests := failingAfterToolProvider(t)
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	s.Injetar(probeTool())
	cwd := t.TempDir()

	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), cwd, "validate the passthrough"); err == nil {
		t.Fatal("the second request fails terminally; the turn must report the error")
	}

	h := s.History()
	var roles []string
	for _, m := range h {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,tool_call,tool_result" {
		t.Fatalf("history after the failed turn = %s; want user,tool_call,tool_result", got)
	}
	if h[0].Conteudo != "validate the passthrough" || h[2].Conteudo != "probe-output-42" {
		t.Fatalf("failed turn lost its content: %+v", h)
	}

	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), cwd, "prossiga"); err != nil {
		t.Fatalf("follow-up turn: %v", err)
	}
	reqs := requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d; want tool round + failure + follow-up", len(reqs))
	}
	follow, _ := json.Marshal(reqs[2])
	for _, want := range []string{"validate the passthrough", "probe-output-42", "prossiga"} {
		if !strings.Contains(string(follow), want) {
			t.Fatalf("follow-up request is missing %q:\n%s", want, follow)
		}
	}
}

// The failed request itself never enters the transcript: no empty assistant,
// no dangling tool_call without its result.
func TestFailedRequestLeavesNoPartialRound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
	}))
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hello"); err == nil {
		t.Fatal("expected the provider error")
	}
	h := s.History()
	if len(h) != 1 || h[0].Role != "user" || h[0].Conteudo != "hello" {
		t.Fatalf("history = %+v; want only the user prompt", h)
	}
}
