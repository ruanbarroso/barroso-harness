package dealer

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

// chatServer answers every request with the raw stream.
func chatServer(t *testing.T, stream string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, stream)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func chatClient(cfg Config) *Cliente {
	cfg.Protocol = ProtocoloChat
	return Novo(cfg)
}

func chatRun(t *testing.T, stream string) (*Rodada, error, int32) {
	t.Helper()
	srv, n := chatServer(t, stream)
	r, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
	return r, err, n.Load()
}

// A stream that ends with neither [DONE] nor a finish_reason is a cut
// connection: unexpected EOF, retried — never delivered as an answer.
func TestChatNoTerminalIsUnexpectedEOF(t *testing.T) {
	shrink(t)
	_, err, n := chatRun(t, "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v", err)
	}
	if n != int32(maxAttempts) {
		t.Fatalf("requests = %d; want %d (transient)", n, maxAttempts)
	}
	if _, err, _ = chatRun(t, ""); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("empty stream: %v", err)
	}
	// A usage-only tail without a terminal is still a cut stream.
	_, err, _ = chatRun(t, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("usage-only: %v", err)
	}
}

// Both legitimate terminals stay accepted: [DONE] alone, and a finish_reason
// with the stream just closing after it.
func TestChatTerminalsAccepted(t *testing.T) {
	r, err, _ := chatRun(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n")
	if err != nil || r.Texto != "a" || r.Fim != "" {
		t.Fatalf("[DONE] only: %+v %v", r, err)
	}
	r, err, _ = chatRun(t, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n")
	if err != nil || r.Texto != "a" || r.Fim != "stop" {
		t.Fatalf("finish_reason only: %+v %v", r, err)
	}
}

// HTTP 200 carrying an error chunk used to be swallowed (the chunk decoded
// into an empty struct) and the round ended as an empty answer.
func TestChat200ErrorChunkIsSurfaced(t *testing.T) {
	shrink(t)
	stream := "data: {\"error\":{\"message\":\"This model's maximum context length is 8192 tokens\",\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\"}}\n\n"
	_, err, n := chatRun(t, stream)
	var pe *providerError
	if !errors.As(err, &pe) || pe.kind != "context_length_exceeded" || n != 1 || transient(err) {
		t.Fatalf("requests=%d err=%v", n, err)
	}
	assertOverflowRecognizable(t, err)
	// After some content, too.
	stream = "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: {\"error\":{\"message\":\"nope\",\"code\":\"invalid_api_key\"}}\n\n"
	if _, err, _ = chatRun(t, stream); err == nil || !strings.Contains(err.Error(), "invalid_api_key") {
		t.Fatalf("err = %v", err)
	}
	// A bare JSON body served as 200 (no SSE framing).
	if _, err, _ = chatRun(t, "{\"error\":{\"message\":\"gateway down\",\"type\":\"server_error\"}}\n"); err == nil || !strings.Contains(err.Error(), "gateway down") {
		t.Fatalf("bare body: %v", err)
	}
	// A string error and a numeric code.
	if _, err, _ = chatRun(t, "data: {\"error\":\"plain text failure\"}\n\n"); err == nil || !strings.Contains(err.Error(), "plain text failure") {
		t.Fatalf("string error: %v", err)
	}
}

func TestChat200TransientErrorChunkRetries(t *testing.T) {
	shrink(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, "data: {\"error\":{\"message\":\"overloaded\",\"type\":\"server_error\"}}\n\n")
			return
		}
		io.WriteString(w, okStream)
	}))
	defer srv.Close()
	r, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("s", nil, nil)
	if err != nil || r.Texto != "ok" || calls.Load() != 2 {
		t.Fatalf("requests=%d err=%v", calls.Load(), err)
	}
	// A tool call whose arguments mention "error" is not an error chunk.
	r, err, _ = chatRun(t, "data: {\"choices\":[{\"delta\":{\"content\":\"an \\\"error\\\": here\"},\"finish_reason\":\"stop\"}]}\n\n")
	if err != nil || r.Texto != "an \"error\": here" {
		t.Fatalf("%+v %v", r, err)
	}
}

// content_filter with nothing to show used to look like an empty answer and
// the runtime regenerated the very same request.
func TestChatContentFilter(t *testing.T) {
	shrink(t)
	_, err, n := chatRun(t, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n")
	if err == nil || !strings.Contains(err.Error(), "content_filter") || transient(err) || n != 1 {
		t.Fatalf("requests=%d err=%v", n, err)
	}
	// Partial text: delivered as a cut round (never a final answer).
	r, err, _ := chatRun(t, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"content_filter\"}]}\n\ndata: [DONE]\n\n")
	if err != nil || !r.Cortada || r.Fim != "content_filter" || r.Texto != "partial" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestChatUsageReasoningTokens(t *testing.T) {
	r, err, _ := chatRun(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":64},"completion_tokens_details":{"reasoning_tokens":20}}}`,
		`data: [DONE]`, "",
	}, "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (&Usage{PromptTokens: 100, OutputTokens: 30, CachedTokens: 64, ReasoningTokens: 20}); !reflect.DeepEqual(r.Usage, want) {
		t.Fatalf("usage = %+v", r.Usage)
	}
}

// The per-line cap used to be 4 MiB.
func TestChatLargeLine(t *testing.T) {
	big := strings.Repeat("z", 5<<20)
	payload, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": big}}}})
	r, err, _ := chatRun(t, "data: "+string(payload)+"\n\ndata: [DONE]\n\n")
	if err != nil || len(r.Texto) != 5<<20 {
		t.Fatalf("err=%v len=%d", err, len(r.Texto))
	}
}

// Explicit Chat selection keeps the legacy body: chat fields, official
// decoys and stream_options include_usage. An empty preference is tested as
// Responses separately; this pins the explicit Chat wire contract.
func TestChatBodyContractUnchangedByProtocolField(t *testing.T) {
	for _, p := range []string{"chat", "chat_completions"} {
		b, err := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: p, Effort: "low"}).Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if _, ok := m["messages"]; !ok || m["reasoning_effort"] != "low" || m["tool_choice"] != "auto" {
			t.Fatalf("protocol %q: %s", p, b)
		}
		if _, ok := m["input"]; ok {
			t.Fatalf("protocol %q leaked Responses fields: %s", p, b)
		}
		if !strings.Contains(string(b), `"stream_options":{"include_usage":true}`) || !strings.Contains(string(b), "placeholder") {
			t.Fatalf("protocol %q lost the chat fingerprint contract: %s", p, b)
		}
	}
}

// Duplicated ids must be renamed BEFORE results are re-paired: pairing by a
// still-duplicated id attached both results to the last owner.
func TestChatBodyDuplicateIDsRepairOrder(t *testing.T) {
	c := chatClient(Config{BaseURL: "https://x/v1", Model: "m"})
	msgs := []Mensagem{
		{Role: "user", Conteudo: "go"},
		{Role: "tool_call", ToolID: "dup", ToolName: "shell", ToolArgs: `{"n":1}`},
		{Role: "tool_result", ToolID: "dup", Conteudo: "one"},
		{Role: "tool_call", ToolID: "dup", ToolName: "shell", ToolArgs: `{"n":2}`},
		{Role: "tool_result", ToolID: "dup", Conteudo: "two"},
	}
	raw, err := c.Corpo("sys", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var rest []int
	for i, m := range body.Messages {
		if m.Role != "system" {
			rest = append(rest, i)
		}
	}
	if len(rest) != 5 {
		t.Fatalf("messages = %+v", body.Messages)
	}
	m := func(i int) (string, string, string, string) {
		x := body.Messages[rest[i]]
		id := x.ToolCallID
		if len(x.ToolCalls) > 0 {
			id = x.ToolCalls[0].ID
		}
		return x.Role, id, x.Content, ""
	}
	r1, id1, _, _ := m(1)
	r2, res1, c2, _ := m(2)
	r3, id2, _, _ := m(3)
	r4, res2, c4, _ := m(4)
	if r1 != "assistant" || id1 != "dup" || r2 != "tool" || res1 != "dup" || c2 != "one" {
		t.Fatalf("first pair: %s %s / %s %s %s", r1, id1, r2, res1, c2)
	}
	if r3 != "assistant" || id2 == "dup" || id2 == "" || r4 != "tool" || res2 != id2 || c4 != "two" {
		t.Fatalf("second pair: %s %s / %s %s %s", r3, id2, r4, res2, c4)
	}
}
