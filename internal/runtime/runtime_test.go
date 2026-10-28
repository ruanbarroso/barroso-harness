// Package runtime — end-to-end tests with a fake provider (SSE): tool loop,
// text streaming, guardrail with a small window and tools.
package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func toolList() []tools.Spec { return tools.SpecsBuiltins() }

// fake httptest provider: a two-round script — asks for shell, then
// closes with text streamed piece by piece with usage at the end.
func fakeProvider() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		wantTool := true
		for _, m := range req.Messages {
			if m.Role == "tool" {
				wantTool = false
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(payload any) {
			b, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if wantTool {
			emit(map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"tool_calls": []map[string]any{{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{
						"name":      "shell",
						"arguments": `{"command":"echo loop-tuple","timeout":10000}`,
					},
				}}},
			}}})
		} else {
			for _, p := range []string{"result ", "received ", "and closing."} {
				emit(map[string]any{"choices": []map[string]any{{
					"delta": map[string]any{"content": p},
				}}})
			}
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{"prompt_tokens": 1200, "completion_tokens": 30}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func TestTurnWithToolAndStreaming(t *testing.T) {
	srv := fakeProvider()
	defer srv.Close()
	var sb strings.Builder
	if err := Turno(&sb, chatTestConfig(srv.URL, "fake", "k"), t.TempDir(), "show that the loop runs"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	out := sb.String()
	for _, want := range []string{"loop-tuple", "[tool shell ok", "result received and closing."} {
		if !strings.Contains(out, want) {
			t.Fatalf("output without %q:\n%s", want, out)
		}
	}
}

func TestTurnRequiresConfig(t *testing.T) {
	var sb strings.Builder
	err := Turno(&sb, config.Config{}, t.TempDir(), "hi")
	if err == nil {
		t.Fatal("turn without config must fail")
	}
}

func TestFileReadTool(t *testing.T) {
	cwd := t.TempDir()
	list := toolList()
	found := false
	for _, s := range list {
		if s.Name == "read" {
			found = true
			output, err := s.Exec(nil, cwd, json.RawMessage(`{"path":"missing.txt"}`))
			if err == nil {
				t.Fatalf("read of a missing file should fail, got %q", output)
			}
		}
	}
	if !found {
		t.Fatal("read is not in the tool list")
	}
}

func TestPrunePreservesTail(t *testing.T) {
	var h []historyMessage
	for i := 0; i < 40; i++ {
		h = append(h, historyMessage{Role: "tool_result", Conteudo: strings.Repeat("x", 4096), ToolName: "read"})
	}
	floor := 0
	pruned := prune(h, &floor, 2000)
	if pruned == nil {
		t.Fatal("pruning should free 2 KiB across 40 results of 4 KiB")
	}
	for i := len(pruned) - 24; i < len(pruned); i++ {
		if len(pruned[i].Conteudo) != 4096 {
			t.Fatalf("untouched tail of 24 was violated at index %d", i)
		}
	}
	if len(pruned[0].Conteudo) >= 4096 {
		t.Fatal("expected the oldest one to be pruned")
	}
}

// Regression (session bh-20261022222739-102c): after a prune that stopped
// early, the next one must still reach the results it did not visit. Moving
// the floor straight to the tail limit made the second guardrail pass fail
// with "pruning did not free enough" while dozens of big results remained.
func TestPruneResumesAfterEarlyStop(t *testing.T) {
	var h []historyMessage
	for i := 0; i < 40; i++ {
		h = append(h, historyMessage{Role: "tool_result", Conteudo: strings.Repeat("x", 4096), ToolName: "read"})
	}
	floor := 0
	first := prune(h, &floor, 2000)
	if first == nil {
		t.Fatal("first prune should free 2 KiB")
	}
	if floor != 1 {
		t.Fatalf("floor should stop right after the one result pruned, got %d", floor)
	}
	second := prune(first, &floor, 2000)
	if second == nil {
		t.Fatal("second prune should still reach the unvisited results")
	}
	if len(second[1].Conteudo) >= 4096 {
		t.Fatal("expected the next oldest result to be pruned")
	}
}

// The guardrail measures its deficit in tokens; prune counts bytes. The
// conversion must ask for chars-per-token times more, or the turn frees ~1/3
// of what it needs and dies one cycle later.
func TestGuardrailDeficitConvertedToBytes(t *testing.T) {
	est := budget.Estimador{CharsPorToken: 3}
	if got := est.Bytes(4375); got != 13125 {
		t.Fatalf("4375 tokens at 3 chars/token = 13125 bytes, got %d", got)
	}
	if got := (budget.Estimador{}).Bytes(10); got != 40 {
		t.Fatalf("uncalibrated estimator assumes 4 chars/token, got %d", got)
	}
	var h []historyMessage
	for i := 0; i < 40; i++ {
		h = append(h, historyMessage{Role: "tool_result", Conteudo: strings.Repeat("x", 4096), ToolName: "read"})
	}
	floor := 0
	pruned := prune(h, &floor, est.Bytes(4375))
	if pruned == nil {
		t.Fatal("prune should free 13 KiB across 16 eligible 4 KiB results")
	}
	freed := 0
	for i := range h {
		freed += len(h[i].Conteudo) - len(pruned[i].Conteudo)
	}
	if est.Tokens(strings.Repeat("x", freed)) < 4375 {
		t.Fatalf("freed %d bytes, below the 4375-token deficit", freed)
	}
}

// Regression (session bh-20261023110329-0a62): 17 messages, two huge read
// results, all inside the protected tail of 24 — prune returned nil and the
// turn died with "pruning did not free enough".
func TestPruneReachesTailInShortConversation(t *testing.T) {
	h := []historyMessage{
		{Role: "user", Conteudo: "olhe a imagem"},
		{Role: "tool_result", Conteudo: strings.Repeat("x", 60000), ToolName: "read"},
		{Role: "tool_result", Conteudo: strings.Repeat("y", 60000), ToolName: "read"},
		{Role: "tool_result", Conteudo: strings.Repeat("z", 60000), ToolName: "read"},
	}
	floor := 0
	pruned := prune(h, &floor, 100000)
	if pruned == nil {
		t.Fatal("prune should reach the tail when older results cannot cover the deficit")
	}
	if len(pruned[1].Conteudo) >= 60000 || len(pruned[2].Conteudo) >= 60000 {
		t.Fatal("expected the two older tail results to be pruned")
	}
	if len(pruned[3].Conteudo) != 60000 {
		t.Fatal("the latest result must be spared")
	}
	if prune(h, &floor, 200000) != nil {
		t.Fatal("a deficit above everything but the latest result cannot be covered")
	}
}

func TestReadImagesOnlyFromRead(t *testing.T) {
	results := []historyMessage{
		{Role: "tool_result", ToolName: "read", Conteudo: tools.ImageAttached + "/tmp/a.png]\nPNG image"},
		{Role: "tool_result", ToolName: "shell", Conteudo: tools.ImageAttached + "/tmp/b.png]\nfake"},
		{Role: "tool_result", ToolName: "read", IsErro: true, Conteudo: tools.ImageAttached + "/tmp/c.png]\n"},
	}
	got := readImages(results)
	if len(got) != 1 || got[0] != "/tmp/a.png" {
		t.Fatalf("want only /tmp/a.png, got %v", got)
	}
}
