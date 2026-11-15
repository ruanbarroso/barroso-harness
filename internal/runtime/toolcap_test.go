package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// fakeDownload mimics the wpp baixar_arquivo result of conversation 129062:
// JSON whose "conteudo" is the base64 of a ~680 KB file.
func fakeDownload() string {
	raw := make([]byte, 680<<10)
	for i := range raw {
		raw[i] = byte(i*31 + i/7)
	}
	b, _ := json.Marshal(map[string]any{"nome": "image.jpg", "tipo": "image/jpeg", "conteudo": base64.StdEncoding.EncodeToString(raw)})
	return string(b)
}

func TestCapToolResultReplacesBase64Payload(t *testing.T) {
	in := fakeDownload()
	if len(in) < 900<<10 {
		t.Fatalf("fixture too small: %d", len(in))
	}
	got := capToolResult(in)
	if len(got) > maxToolResult {
		t.Fatalf("result of %d bytes above the cap %d", len(got), maxToolResult)
	}
	if len(got) > 1024 {
		t.Fatalf("a pure base64 payload should shrink to a note, got %d bytes", len(got))
	}
	for _, want := range []string{`"nome":"image.jpg"`, "base64 payload of", "omitted"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}

func TestCapToolResultKeepsHeadAndTailOfLongText(t *testing.T) {
	in := "START\n" + strings.Repeat("line of ordinary output\n", 20000) + "END-MARK\n"
	got := capToolResult(in)
	if len(got) > maxToolResult {
		t.Fatalf("above the cap: %d", len(got))
	}
	if !strings.HasPrefix(got, "START") || !strings.HasSuffix(got, "END-MARK\n") || !strings.Contains(got, "cut to fit") {
		t.Fatalf("head/tail/marker lost: %q ... %q", got[:20], got[len(got)-20:])
	}
}

func TestCapToolResultLeavesSmallAndBuiltinSizesAlone(t *testing.T) {
	small := strings.Repeat("x", 64<<10) + "\n\n[output truncated at 65536 bytes; read with smaller start/limit for the rest]"
	if capToolResult(small) != small {
		t.Fatal("a built-in sized result (64 KiB + marker) must pass untouched")
	}
	if capToolResult("ok") != "ok" {
		t.Fatal("tiny result changed")
	}
}

func TestCapToolResultKeepsUTF8Valid(t *testing.T) {
	got := capToolResult(strings.Repeat("é", 200<<10))
	if len(got) > maxToolResult {
		t.Fatalf("above the cap: %d", len(got))
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatal("cut split a rune")
	}
}

func TestShrinkResultsCutsNewestOversizedResult(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "go"},
		{Role: "tool_call", ToolID: "c1", ToolName: "x"},
		{Role: "tool_result", ToolID: "c1", ToolName: "x", Conteudo: strings.Repeat("a b c ", 100000)},
	}
	got := shrinkResults(h, 300_000)
	if got == nil {
		t.Fatal("shrinking the only huge result should free enough")
	}
	if len(got[2].Conteudo) > lastResortKeep {
		t.Fatalf("result still %d bytes", len(got[2].Conteudo))
	}
	if len(h[2].Conteudo) != 600000 {
		t.Fatal("the input slice must not be modified")
	}
	if shrinkResults(h, 10_000_000) != nil {
		t.Fatal("a deficit beyond everything shrinkable must report failure")
	}
}

// Regression (conversation 129062, 2026-11-15): an MCP tool returned ~900 KB;
// the newest result is spared by prune/reduceTurn, so the turn died with
// "request does not fit". The result must now enter the history capped and
// the turn must complete.
func TestHugeToolResultDoesNotKillTheTurn(t *testing.T) {
	big := fakeDownload()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, m := range req.Messages {
			if s, _ := m["content"].(string); len(s) > maxToolResult {
				t.Errorf("a message of %d bytes reached the provider", len(s))
			}
		}
		calls++
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(v any) {
			b, _ := json.Marshal(v)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		if calls == 1 {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "download", "arguments": `{}`},
			}}}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "tool_calls"}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}, "finish_reason": "stop"}}})
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := chatTestConfig(srv.URL, "fake", "k")
	cfg.Janela = 200_000
	s := NewSession(cfg)
	s.Injetar(tools.Spec{Name: "download", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) { return big, nil }})
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "baixe o arquivo"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	for _, m := range s.History() {
		if m.Role == "tool_result" && len(m.Conteudo) > maxToolResult {
			t.Fatalf("history holds a result of %d bytes", len(m.Conteudo))
		}
	}
}

// A log written before the cap (900 KB result in a blob) must come back
// within the cap on Resume, or the conversation stays unusable.
func TestResumeCapsOversizedLoggedResult(t *testing.T) {
	st := testStore(t)
	const id = "bh-20261115170854-cafe"
	s := NewSessionWithStore(config.Config{}, st, id)
	s.recordResult("call_1", "baixar_arquivo", fakeDownload(), false)

	s2 := NewSessionWithStore(config.Config{}, st, id)
	if err := s2.Resume(); err != nil {
		t.Fatal(err)
	}
	h := s2.History()
	if len(h) != 1 || len(h[0].Conteudo) > maxToolResult {
		t.Fatalf("resumed result not capped: %d messages, %d bytes", len(h), len(h[0].Conteudo))
	}
}
