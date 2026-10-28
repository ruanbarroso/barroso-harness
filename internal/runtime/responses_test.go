package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

func TestResponsesSessionReplaysRawOutputAcrossTurnsAndResume(t *testing.T) {
	const reasoning = `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"opaque"}`
	const firstMessage = `{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"first answer"}]}`
	var requests int
	var secondInput []json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.Error(w, "unexpected endpoint "+r.URL.Path, http.StatusNotFound)
			return
		}
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests++
		if requests == 2 {
			secondInput = append([]json.RawMessage(nil), body.Input...)
		}
		output := []any{map[string]any{
			"id": "msg_2", "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": "second answer"}},
		}}
		if requests == 1 {
			var rawReasoning, rawMessage json.RawMessage
			_ = json.Unmarshal([]byte(reasoning), &rawReasoning)
			_ = json.Unmarshal([]byte(firstMessage), &rawMessage)
			output = []any{rawReasoning, rawMessage}
		}
		payload, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"status": "completed", "output": output,
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 3,
					"output_tokens_details": map[string]any{"reasoning_tokens": 1}},
			},
		})
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", payload)
	}))
	defer srv.Close()

	const sessionID = "bh-20261028000000-rsp1"
	st := testStore(t)
	cfg := config.Config{
		CurrentProvider: "test",
		Providers: map[string]*config.Provider{
			"test": {BaseURL: srv.URL + "/v1", APIKey: "k", Protocol: config.ProtocoloResponses},
		},
		Model: "fake", Janela: 100_000,
	}
	s := NewSessionWithStore(cfg, st, sessionID)
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "first prompt"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "second prompt"); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	if len(secondInput) < 4 {
		t.Fatalf("second input too short: %s", secondInput)
	}
	if string(secondInput[1]) != reasoning || string(secondInput[2]) != firstMessage {
		t.Fatalf("raw first response items were not replayed intact and in order:\n%s", secondInput)
	}

	resumed := NewSessionWithStore(cfg, st, sessionID)
	if err := resumed.Resume(); err != nil {
		t.Fatalf("resume: %v", err)
	}
	history := resumed.History()
	if len(history) != 4 {
		t.Fatalf("resumed history length = %d, want 4: %+v", len(history), history)
	}
	if got := history[1].ResponsesOutput; len(got) != 2 || string(got[0]) != reasoning || string(got[1]) != firstMessage {
		t.Fatalf("first Responses output not preserved by resume: %s", got)
	}
	if got := history[3].Conteudo; got != "second answer" {
		t.Fatalf("final resumed answer = %q", got)
	}
}
