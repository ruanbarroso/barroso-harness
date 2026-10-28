package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func TestLooksLikeToolText(t *testing.T) {
	yes := []string{
		"Registrando.\n\n(tool_call) name=okf_create arguments={\"concept_id\": \"x\", \"body\": \"" + strings.Repeat("a ", 2000) + "\"}",
		"tool_call name=context_read argument{}",
		`{"tool_uses":[{"recipient_name":"functions.edit_many","parameters":{"edits":[{"path":"C:\\repo\\file.go","oldString":"before","newString":"after"}]}},{"recipient_name":"functions.shell","parameters":{"command":"git status --short"}}]}:functions.shell (commentary) * * * (tool call; no content)`,
		"Vou ler.\n<tool_call>\n<function=read>\n<parameter=path>a</parameter>\n</function>\n</tool_call>",
	}
	for _, s := range yes {
		if !looksLikeToolText(s) {
			t.Errorf("should detect: %.80q", s)
		}
	}
	no := []string{
		"Pronto, tudo verde.",
		"O modelo escreveu (tool_call) name=okf_create arguments={...} como texto. Quer que eu corrija?",
		`{"tool_uses":[],"note":"not an invocation"}`,
		"{\"a\": 1}",
	}
	for _, s := range no {
		if looksLikeToolText(s) {
			t.Errorf("should not detect: %q", s)
		}
	}
}

// A final round with a leaked call is re-asked with the notice, and the next
// round's real answer ends the turn; the leaked text is not delivered alone.
func TestToolTextIsReasked(t *testing.T) {
	leaked := "(tool_call) name=shell arguments={\"command\": \"echo hi\"}"
	srv, seen := cutProvider(t, []step{{leaked, false, "stop"}, {"done", false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	v := *seen
	if len(v) != 2 || !strings.HasPrefix(v[1].Last, "[tool call as text]") {
		t.Fatalf("requests = %+v", v)
	}
	if !strings.Contains(sb.String(), "done") {
		t.Fatalf("output:\n%s", sb.String())
	}
}

// A leaked transport envelope is suppressed even when the same provider
// response also contains valid structured tool calls. The real call still runs.
func TestToolUsesEnvelopeWithStructuredToolsIsSuppressed(t *testing.T) {
	leaked := `{"tool_uses":[{"recipient_name":"functions.edit_many","parameters":{"edits":[]}},{"recipient_name":"functions.shell","parameters":{"command":"git status"}}]}:functions.shell (commentary) * * * (tool call; no content)`
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		first := requests.Add(1) == 1
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		if first {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": leaked}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "probe-1", "type": "function",
				"function": map[string]any{"name": "probe", "arguments": `{}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	var ran atomic.Bool
	s := NewSession(config.Config{BaseURL: srv.URL, Model: "fake", APIKey: "k", Janela: 100_000})
	s.Injetar(tools.Spec{Name: "probe", Schema: map[string]any{"type": "object"}, Exec: func(context.Context, string, json.RawMessage) (string, error) {
		ran.Store(true)
		return "ran", nil
	}})
	r := &notaRenderer{}
	if err := s.Send(context.Background(), r, t.TempDir(), "run the probe"); err != nil {
		t.Fatal(err)
	}
	if !ran.Load() {
		t.Fatal("the valid structured tool call did not execute")
	}
	if got := strings.Join(r.got, ","); strings.Contains(got, "tool_uses") || strings.Contains(got, "functions.edit_many") || !strings.Contains(got, "start:probe") || !strings.Contains(got, "resposta:done") {
		t.Fatalf("events exposed leaked content or lost the real call/answer: %q", got)
	}
	for _, message := range s.History() {
		if strings.Contains(message.Conteudo, "tool_uses") || strings.Contains(message.Conteudo, "functions.edit_many") {
			t.Fatalf("leaked envelope entered transcript: %+v", message)
		}
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("provider requests = %d, want 2 (one tool round + final answer)", got)
	}
}

// The re-ask is bounded: a model that keeps leaking gets its text delivered.
func TestToolTextGivesUpAfterCap(t *testing.T) {
	leaked := "(tool_call) name=shell arguments={\"command\": \"echo hi\"}"
	srv, seen := cutProvider(t, []step{{leaked, false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	if n := len(*seen); n != 1+maxToolTextRetries {
		t.Fatalf("requests = %d; want %d", n, 1+maxToolTextRetries)
	}
}
