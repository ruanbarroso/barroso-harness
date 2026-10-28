package dealer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ── helpers ──────────────────────────────────────────────────────────────

// ev renders one Responses SSE event (with the `event:` line some gateways
// send) from its type and fields.
func ev(typ string, fields map[string]any) string {
	m := map[string]any{"type": typ}
	for k, v := range fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return "event: " + typ + "\ndata: " + string(b) + "\n\n"
}

func respCfg(url string) Config {
	return Config{BaseURL: url, Model: "m", Protocol: "responses"}
}

// respServer answers every request with the events; it records the path and
// the decoded request body of the last one.
func respServer(t *testing.T, events ...string) (*httptest.Server, *atomic.Int32, *http.Request, *map[string]any) {
	t.Helper()
	var n atomic.Int32
	var last http.Request
	body := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		last = *r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, strings.Join(events, ""))
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &last, &body
}

func runResp(t *testing.T, events ...string) (*Rodada, error) {
	t.Helper()
	srv, _, _, _ := respServer(t, events...)
	return Novo(respCfg(srv.URL)).Rodar("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
}

func completed(extra map[string]any) string {
	resp := map[string]any{"status": "completed", "output": []any{}}
	for k, v := range extra {
		resp[k] = v
	}
	return ev("response.completed", map[string]any{"response": resp})
}

func jstr(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ── protocol selector / endpoint ─────────────────────────────────────────

func TestProtocoloSelector(t *testing.T) {
	for in, want := range map[string]string{
		"": ProtocoloResponses, "chat": ProtocoloChat, "chat_completions": ProtocoloChat,
		" Chat-Completions ": ProtocoloChat, "completions": ProtocoloChat,
		"responses": ProtocoloResponses, " RESPONSES ": ProtocoloResponses,
	} {
		if got := Novo(Config{Protocol: in}).Protocolo(); got != want {
			t.Errorf("Protocolo(%q) = %q; want %q", in, got, want)
		}
	}
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "messages"})
	if _, err := c.Corpo("s", nil, nil); err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("Corpo with unknown protocol: %v", err)
	}
	if _, err := c.CorpoEstimativa("s", nil, nil); err == nil {
		t.Fatal("CorpoEstimativa with unknown protocol must fail")
	}
	if _, err := c.Rodar("s", nil, nil); err == nil || !strings.Contains(err.Error(), "unknown protocol") {
		t.Fatalf("Rodar with unknown protocol must fail without a fallback: %v", err)
	}
}

func TestNormalizeURLStripsResponses(t *testing.T) {
	for in, want := range map[string]string{
		"https://x.example/v1/responses":   "https://x.example/v1",
		"https://x.example/v1/responses/":  "https://x.example/v1",
		" https://x.example/v1/responses ": "https://x.example/v1",
		"https://x.example/responses":      "https://x.example",
	} {
		if got := NormalizarURL(in); got != want {
			t.Errorf("NormalizarURL(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestEndpointsByProtocol(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.HasSuffix(r.URL.Path, "/responses") {
			io.WriteString(w, completed(nil))
			return
		}
		io.WriteString(w, okStream)
	}))
	defer srv.Close()
	for _, base := range []string{srv.URL + "/v1", srv.URL + "/v1/responses", srv.URL + "/v1/chat/completions"} {
		if _, err := Novo(Config{BaseURL: base, Model: "m", Protocol: "responses"}).Rodar("s", nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := Novo(Config{BaseURL: base, Model: "m", Protocol: ProtocoloChat}).Rodar("s", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"/v1/responses", "/v1/chat/completions", "/v1/responses", "/v1/chat/completions", "/v1/responses", "/v1/chat/completions"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v\nwant %v", paths, want)
	}
}

func TestResponsesUnavailableFallsBackAndCaches(t *testing.T) {
	shrink(t)
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode Chat body: %v", err)
		}
		if _, ok := body["messages"]; !ok {
			t.Errorf("fallback body is not Chat Completions: %v", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, okStream)
	}))
	defer srv.Close()

	client := Novo(Config{BaseURL: srv.URL + "/v1", Model: "m"})
	first, err := client.Rodar("sys", nil, nil)
	if err != nil || first.Texto != "ok" || !first.ProtocolFallback {
		t.Fatalf("first round = %+v, err %v", first, err)
	}
	second, err := client.Rodar("sys", nil, nil)
	if err != nil || second.Texto != "ok" || second.ProtocolFallback {
		t.Fatalf("cached Chat round = %+v, err %v", second, err)
	}
	want := []string{"/v1/responses", "/v1/chat/completions", "/v1/chat/completions"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v; want %v", paths, want)
	}
}

func TestResponsesUnavailableRoutesFallback(t *testing.T) {
	shrink(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "method not allowed", status: http.StatusMethodNotAllowed},
		{name: "not implemented", status: http.StatusNotImplemented},
		{name: "named missing route", status: http.StatusBadRequest, body: `{"error":{"code":"unsupported_endpoint","message":"Responses endpoint is not supported"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var responses, chats atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/responses":
					responses.Add(1)
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case "/v1/chat/completions":
					chats.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, okStream)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			round, err := Novo(Config{BaseURL: srv.URL + "/v1", Model: "m"}).Rodar("sys", nil, nil)
			if err != nil || round.Texto != "ok" || !round.ProtocolFallback || responses.Load() != 1 || chats.Load() != 1 {
				t.Fatalf("responses=%d chats=%d round=%+v err=%v", responses.Load(), chats.Load(), round, err)
			}
		})
	}
}

func TestResponsesFallbackOnlyForUnavailableRoute(t *testing.T) {
	shrink(t)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "invalid request", status: http.StatusBadRequest, body: `{"error":{"code":"invalid_request_error","message":"bad input"}}`},
		{name: "authentication", status: http.StatusUnauthorized, body: `{"error":{"code":"invalid_api_key","message":"unauthorized"}}`},
		{name: "model not found", status: http.StatusNotFound, body: `{"error":{"code":"model_not_found","message":"model not found"}}`},
		{name: "unknown model", status: http.StatusNotFound, body: `{"error":{"code":"unknown_model","message":"unknown model id"}}`},
		{name: "unsupported model", status: http.StatusNotFound, body: `{"error":{"code":"unsupported_model","message":"model not available"}}`},
		{name: "server failure", status: http.StatusServiceUnavailable, body: "temporarily unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var responses, chats atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/responses":
					responses.Add(1)
					w.Header().Set("X-Should-Retry", "false")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				case "/v1/chat/completions":
					chats.Add(1)
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, okStream)
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			_, err := Novo(Config{BaseURL: srv.URL + "/v1", Model: "m", Protocol: ProtocoloResponses}).Rodar("sys", nil, nil)
			if err == nil || responses.Load() != 1 || chats.Load() != 0 {
				t.Fatalf("responses=%d chats=%d err=%v", responses.Load(), chats.Load(), err)
			}
		})
	}
}

// ── request body ─────────────────────────────────────────────────────────

func TestResponsesBodyExactJSON(t *testing.T) {
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses", Effort: "high", MaxTokens: 1000, DisableParallelToolCalls: true})
	tools := []map[string]any{{"type": "function", "function": map[string]any{
		"name": "grep", "description": "d",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
	}}}
	msgs := []Mensagem{
		{Role: "user", Conteudo: "hi", HookContext: "ctx"},
		{Role: "assistant", Conteudo: "looking"},
		{Role: "tool_call", ToolID: "a1", ToolName: "grep", ToolArgs: `{"q":"x"}`},
		{Role: "tool_result", ToolID: "a1", Conteudo: "out"},
	}
	got, err := c.Corpo("sys", msgs, tools)
	if err != nil {
		t.Fatal(err)
	}
	instr := jstr(t, comBlocoEnv("sys"))
	hook := jstr(t, "hi\n\n<hook-context>\nctx\n</hook-context>")
	want := `{"model":"m","instructions":` + instr + `,"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":` + hook + `}]},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"looking"}]},` +
		`{"type":"function_call","call_id":"a1","name":"grep","arguments":"{\"q\":\"x\"}"},` +
		`{"type":"function_call_output","call_id":"a1","output":"out"}],` +
		`"tools":[{"type":"function","name":"grep","description":"d","parameters":{"properties":{"q":{"type":"string"}},"type":"object"},"strict":false}],` +
		`"tool_choice":"auto","parallel_tool_calls":false,"reasoning":{"effort":"high"},` +
		`"include":["reasoning.encrypted_content"],` +
		`"max_output_tokens":1000,"stream":true,"store":false}`
	if string(got) != want {
		t.Fatalf("body mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestResponsesBodyOptionalFieldsAndNoChatFingerprint(t *testing.T) {
	for _, effort := range []string{"", "none", "NONE"} {
		c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses", Effort: effort, DisableParallelToolCalls: true})
		b, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"reasoning", "tools", "tool_choice", "parallel_tool_calls", "previous_response_id", "messages", "stream_options", "reasoning_effort"} {
			if _, ok := m[k]; ok {
				t.Errorf("effort=%q: field %q must be absent: %s", effort, k, b)
			}
		}
		if !reflect.DeepEqual(m["include"], []any{"reasoning.encrypted_content"}) {
			t.Errorf("encrypted reasoning not requested: %v", m["include"])
		}
		if m["store"] != false || m["stream"] != true {
			t.Errorf("store/stream = %v/%v", m["store"], m["stream"])
		}
		if int(m["max_output_tokens"].(float64)) != TetoPadrao(effort) {
			t.Errorf("max_output_tokens = %v", m["max_output_tokens"])
		}
		// No chat fingerprint: no decoy read/shell tools, no brake note.
		if strings.Contains(string(b), "placeholder") || strings.Contains(string(b), "Read the contents of a file") {
			t.Errorf("chat fingerprint leaked into the Responses body: %s", b)
		}
	}
	// Tools given, parallel allowed: tool_choice only, no parallel flag.
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses"})
	b, _ := c.Corpo("sys", nil, []map[string]any{{"name": "flat", "parameters": map[string]any{"type": "object"}}})
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	tl := m["tools"].([]any)
	if len(tl) != 1 || tl[0].(map[string]any)["name"] != "flat" || tl[0].(map[string]any)["type"] != "function" {
		t.Fatalf("flat tool not preserved: %s", b)
	}
	if m["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", m["tool_choice"])
	}
	if _, ok := m["parallel_tool_calls"]; ok {
		t.Fatalf("parallel_tool_calls must be absent unless disabled: %s", b)
	}
}

func TestResponsesBodyImagesEstimateVsActual(t *testing.T) {
	dir := t.TempDir()
	img := dir + string(os.PathSeparator) + "shot.png"
	png := []byte("\x89PNG\r\n\x1a\nfake")
	if err := os.WriteFile(img, png, 0o644); err != nil {
		t.Fatal(err)
	}
	missing := dir + string(os.PathSeparator) + "gone.png"
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses"})
	msgs := []Mensagem{{Role: "user", Conteudo: "see [Image 1] and [Image 2]", Images: []string{img, missing}}}

	parts := func(b []byte) []map[string]any {
		var body struct {
			Input []struct {
				Role    string           `json:"role"`
				Content []map[string]any `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Input) != 1 || body.Input[0].Role != "user" {
			t.Fatalf("input = %+v", body.Input)
		}
		return body.Input[0].Content
	}

	actual, err := c.Corpo("sys", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := parts(actual)
	if len(p) != 2 || p[0]["type"] != "input_text" || p[1]["type"] != "input_image" {
		t.Fatalf("parts = %v", p)
	}
	if !strings.Contains(p[0]["text"].(string), "[Image 1: "+img+"]") || !strings.Contains(p[0]["text"].(string), "[Image 2: "+missing+" (file missing") {
		t.Fatalf("text = %q", p[0]["text"])
	}
	if want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png); p[1]["image_url"] != want {
		t.Fatalf("image_url = %v", p[1]["image_url"])
	}

	est, err := c.CorpoEstimativa("sys", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(est), base64.StdEncoding.EncodeToString(png)) {
		t.Fatal("estimate must not inline the image")
	}
	ep := parts(est)
	// The estimate stands in for each ATTACHED image (it does not read files).
	if len(ep) != 3 || ep[1]["type"] != "input_image" || !strings.Contains(ep[1]["image_url"].(string), imageEstimateStandIn) {
		t.Fatalf("estimate parts = %v", ep)
	}
}

func TestResponsesBodyDuplicateIDsRepairOrder(t *testing.T) {
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses"})
	msgs := []Mensagem{
		{Role: "user", Conteudo: "go"},
		{Role: "tool_call", ToolID: "dup", ToolName: "shell", ToolArgs: `{"n":1}`},
		{Role: "tool_result", ToolID: "dup", Conteudo: "one"},
		{Role: "tool_call", ToolID: "dup", ToolName: "shell", ToolArgs: `{"n":2}`},
		{Role: "tool_result", ToolID: "dup", Conteudo: "two"},
	}
	b, err := c.Corpo("sys", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
			Args   string `json:"arguments"`
		} `json:"input"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Input) != 5 {
		t.Fatalf("input = %+v", body.Input)
	}
	in := body.Input
	if in[1].Type != "function_call" || in[1].CallID != "dup" || in[2].Type != "function_call_output" || in[2].CallID != "dup" || in[2].Output != "one" {
		t.Fatalf("first pair = %+v %+v", in[1], in[2])
	}
	if in[3].Type != "function_call" || in[3].CallID == "dup" || in[3].CallID == "" ||
		in[4].Type != "function_call_output" || in[4].CallID != in[3].CallID || in[4].Output != "two" || in[3].Args != `{"n":2}` {
		t.Fatalf("second pair = %+v %+v", in[3], in[4])
	}
}

func TestResponsesBodyParallelCallsAndLateResult(t *testing.T) {
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses"})
	msgs := []Mensagem{
		{Role: "user", Conteudo: "go"},
		{Role: "tool_call", ToolID: "a", ToolName: "grep", ToolArgs: `{}`},
		{Role: "tool_call", ToolID: "b", ToolName: "read", ToolArgs: `{}`},
		{Role: "user", Conteudo: "interleaved"},
		{Role: "tool_result", ToolID: "b", Conteudo: "B"},
		{Role: "tool_result", ToolID: "a", Conteudo: "A"},
		{Role: "tool_result", ToolID: "ghost", Conteudo: "dropped"},
	}
	b, err := c.Corpo("sys", msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Input []struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	_ = json.Unmarshal(b, &body)
	var flat []string
	for _, it := range body.Input {
		flat = append(flat, it.Type+":"+it.Role+it.CallID)
	}
	want := []string{"message:user", "function_call:a", "function_call:b", "function_call_output:b", "function_call_output:a", "message:user"}
	if !reflect.DeepEqual(flat, want) {
		t.Fatalf("input = %v\nwant %v", flat, want)
	}
}

func TestResponsesBodyUnknownRole(t *testing.T) {
	c := Novo(respCfg("https://x/v1"))
	if _, err := c.Corpo("s", []Mensagem{{Role: "bogus"}}, nil); err == nil {
		t.Fatal("unknown role must fail")
	}
}

// ── request on the wire ──────────────────────────────────────────────────

func TestResponsesRequestHeadersAndBody(t *testing.T) {
	t.Setenv("ZEN_USER_AGENT", "opencode/latest/9.9.9/test")
	srv, n, last, body := respServer(t, ev("response.output_text.delta", map[string]any{"delta": "ok"}), completed(nil))
	c := Novo(Config{BaseURL: srv.URL + "/v1/", Model: "mod", APIKey: "k", Protocol: "responses"}).ComConversa("conv-resp")
	r, err := c.Rodar("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "ok" || n.Load() != 1 {
		t.Fatalf("texto=%q requests=%d", r.Texto, n.Load())
	}
	if last.URL.Path != "/v1/responses" || last.Method != "POST" {
		t.Fatalf("request %s %s", last.Method, last.URL.Path)
	}
	h := last.Header
	if h.Get("Authorization") != "Bearer k" || h.Get("User-Agent") != "opencode/latest/9.9.9/test" ||
		h.Get("Accept") != "text/event-stream" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if s := h.Get("x-opencode-session"); !strings.HasPrefix(s, "ses_") || h.Get("x-opencode-session-id") != s {
		t.Fatalf("session headers = %v", h)
	}
	if (*body)["model"] != "mod" || (*body)["store"] != false {
		t.Fatalf("body = %v", *body)
	}
}

// ── stream mapping ───────────────────────────────────────────────────────

func TestResponsesStreamTextAndToolFragmented(t *testing.T) {
	r, err := runResp(t,
		ev("response.created", map[string]any{"response": map[string]any{"status": "in_progress"}}),
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "content": []any{}}}),
		ev("response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "delta": "Let me "}),
		ev("response.output_text.delta", map[string]any{"item_id": "msg_1", "output_index": 0, "delta": "look."}),
		ev("response.output_text.done", map[string]any{"item_id": "msg_1", "output_index": 0, "text": "Let me look."}),
		ev("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "Let me look."}}}}),
		ev("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "grep", "arguments": ""}}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 1, "delta": `{"q":`}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "output_index": 1, "delta": `"x"}`}),
		ev("response.function_call_arguments.done", map[string]any{"item_id": "fc_1", "output_index": 1, "arguments": `{"q":"x"}`}),
		ev("response.output_item.done", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "call_1", "name": "grep", "arguments": `{"q":"x"}`}}),
		completed(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "Let me look." {
		t.Fatalf("text duplicated or lost: %q", r.Texto)
	}
	if len(r.Tools) != 1 || r.Tools[0] != (ToolChamada{ID: "call_1", Nome: "grep", Args: `{"q":"x"}`}) {
		t.Fatalf("tools = %+v", r.Tools)
	}
	if r.Fim != "tool_calls" || r.FimPorTexto || r.Cortada {
		t.Fatalf("Fim=%q FimPorTexto=%v Cortada=%v", r.Fim, r.FimPorTexto, r.Cortada)
	}
	if r.TTFT <= 0 || r.Dur < r.TTFT {
		t.Fatalf("ttft %v dur %v", r.TTFT, r.Dur)
	}
}

func TestResponsesParallelInterleavedCalls(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "read"}}),
		ev("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_2", "type": "function_call", "call_id": "c2", "name": "shell"}}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "delta": `{"path":`}),
		// no item_id: correlated by output_index only
		ev("response.function_call_arguments.delta", map[string]any{"output_index": 1, "delta": `{"command":`}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "delta": `"a"}`}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_2", "output_index": 1, "delta": `"ls"}`}),
		// second call is finalized by arguments.done only, the first by item.done
		ev("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "read", "arguments": `{"path":"a"}`}}),
		ev("response.function_call_arguments.done", map[string]any{"item_id": "fc_2", "arguments": `{"command":"ls"}`}),
		completed(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolChamada{{ID: "c1", Nome: "read", Args: `{"path":"a"}`}, {ID: "c2", Nome: "shell", Args: `{"command":"ls"}`}}
	if !reflect.DeepEqual(r.Tools, want) {
		t.Fatalf("tools = %+v\nwant %+v", r.Tools, want)
	}
}

func TestResponsesOutputOrderFollowsOutputIndex(t *testing.T) {
	// Item 1 is finalized before item 0: the order handed over is output order.
	r, err := runResp(t,
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_a", "type": "function_call", "call_id": "ca", "name": "a"}}),
		ev("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_b", "type": "function_call", "call_id": "cb", "name": "b"}}),
		ev("response.output_item.done", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_b", "type": "function_call", "call_id": "cb", "name": "b", "arguments": "{}"}}),
		ev("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_a", "type": "function_call", "call_id": "ca", "name": "a", "arguments": "{}"}}),
		completed(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 2 || r.Tools[0].ID != "ca" || r.Tools[1].ID != "cb" {
		t.Fatalf("tools = %+v", r.Tools)
	}
}

func TestResponsesTerminalObjectFinalizesCalls(t *testing.T) {
	// No item.done and no arguments.done: the response.completed object lists
	// the final items, which finalizes the call (and carries unstreamed text).
	r, err := runResp(t,
		ev("response.output_item.added", map[string]any{"output_index": 1, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "shell"}}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "delta": `{"command":`}),
		completed(map[string]any{"output": []any{
			map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}},
			map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "shell", "arguments": `{"command":"ls"}`},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "hello" || len(r.Tools) != 1 || r.Tools[0] != (ToolChamada{ID: "c1", Nome: "shell", Args: `{"command":"ls"}`}) {
		t.Fatalf("texto=%q tools=%+v", r.Texto, r.Tools)
	}
}

func TestResponsesUnfinalizedCallIsNotReturned(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_item.added", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "shell"}}),
		ev("response.function_call_arguments.delta", map[string]any{"item_id": "fc_1", "delta": `{"command":"l`}),
		completed(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Tools) != 0 {
		t.Fatalf("a call that never finalized must not reach the runtime: %+v", r.Tools)
	}
}

func TestResponsesRawOutputIsReplayedWithoutDuplicateCalls(t *testing.T) {
	c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "responses"})
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"secret","summary":[]}`),
		json.RawMessage(`{"type":"message","id":"msg_1","phase":"final","content":[{"type":"output_text","text":"ok"}]}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}"}`),
	}
	body, err := c.Corpo("s", []Mensagem{
		{Role: "assistant", Conteudo: "ok", ResponsesOutput: items},
		{Role: "tool_call", ToolID: "call_1", ToolName: "read", ToolArgs: "{}"},
		{Role: "tool_result", ToolID: "call_1", Conteudo: "file contents"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req respRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	// 3 raw items + the function_call_output; the function_call must NOT appear
	// twice (once as raw item, once rebuilt from the tool_call message).
	if len(req.Input) != 4 {
		t.Fatalf("input items=%d want=4: %s", len(req.Input), body)
	}
	raw, _ := json.Marshal(req.Input[0])
	var reasoning map[string]any
	if err := json.Unmarshal(raw, &reasoning); err != nil || reasoning["encrypted_content"] != "secret" {
		t.Fatalf("encrypted reasoning not replayed first: %s err=%v", raw, err)
	}
	seenCall, seenOutput := 0, 0
	for _, item := range req.Input {
		b, _ := json.Marshal(item)
		switch {
		case strings.Contains(string(b), `"type":"function_call"`):
			seenCall++
		case strings.Contains(string(b), `"type":"function_call_output"`):
			seenOutput++
		}
	}
	if seenCall != 1 || seenOutput != 1 {
		t.Fatalf("function_call=%d function_call_output=%d: %s", seenCall, seenOutput, body)
	}
}

func TestResponsesTextOnlyFromItemDoneAndNoDuplicates(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}}),
		completed(map[string]any{"output": []any{map[string]any{"id": "msg_1", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "hello"}}}}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "hello" || r.Fim != "stop" || !r.FimPorTexto {
		t.Fatalf("texto=%q fim=%q", r.Texto, r.Fim)
	}
}

func TestResponsesReasoningSummary(t *testing.T) {
	r, err := runResp(t,
		ev("response.reasoning_summary_part.added", map[string]any{"item_id": "rs_1"}),
		ev("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "delta": "Think"}),
		ev("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "delta": "ing"}),
		ev("response.reasoning_summary_part.added", map[string]any{"item_id": "rs_1"}),
		ev("response.reasoning_summary_text.delta", map[string]any{"item_id": "rs_1", "delta": "More"}),
		ev("response.output_text.delta", map[string]any{"item_id": "msg_1", "delta": "done"}),
		completed(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Raciocinio != "Thinking\n\nMore" || r.Texto != "done" || r.DurRaciocinio <= 0 {
		t.Fatalf("raciocinio=%q texto=%q dur=%v", r.Raciocinio, r.Texto, r.DurRaciocinio)
	}
}

func TestResponsesUsageCacheReasoningTokens(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_text.delta", map[string]any{"delta": "ok"}),
		completed(map[string]any{"usage": map[string]any{
			"input_tokens": 2823, "output_tokens": 40, "total_tokens": 2863,
			"input_tokens_details":  map[string]any{"cached_tokens": 2816},
			"output_tokens_details": map[string]any{"reasoning_tokens": 25},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := &Usage{PromptTokens: 2823, OutputTokens: 40, CachedTokens: 2816, ReasoningTokens: 25}
	if !reflect.DeepEqual(r.Usage, want) {
		t.Fatalf("usage = %+v; want %+v", r.Usage, want)
	}
	// No usage object: nil, not zeroes.
	if r, err = runResp(t, completed(nil)); err != nil || r.Usage != nil {
		t.Fatalf("usage = %+v err=%v", r.Usage, err)
	}
}

func TestResponsesIncompleteMaxOutputTokens(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_text.delta", map[string]any{"delta": "half a thou"}),
		ev("response.incomplete", map[string]any{"response": map[string]any{
			"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_output_tokens"},
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Cortada || r.Fim != "length" || r.Texto != "half a thou" || r.Usage == nil || r.Usage.OutputTokens != 5 {
		t.Fatalf("%+v", r)
	}
}

func TestResponsesIncompleteUnknownReasonIsNeverFinal(t *testing.T) {
	r, err := runResp(t,
		ev("response.output_item.done", map[string]any{"output_index": 0, "item": map[string]any{"id": "fc_1", "type": "function_call", "call_id": "c1", "name": "read", "arguments": "{}"}}),
		ev("response.incomplete", map[string]any{"response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "max_messages"}}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if r.Cortada || r.Fim != "max_messages" {
		t.Fatalf("Cortada=%v Fim=%q", r.Cortada, r.Fim)
	}
}

func TestResponsesIncompleteContentFilterEmptyIsAnError(t *testing.T) {
	srv, n, _, _ := respServer(t,
		ev("response.incomplete", map[string]any{"response": map[string]any{"status": "incomplete", "incomplete_details": map[string]any{"reason": "content_filter"}}}))
	shrink(t)
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "content_filter") || n.Load() != 1 {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
}

func TestResponsesFailedContextOverflow(t *testing.T) {
	shrink(t)
	srv, n, _, _ := respServer(t,
		ev("response.failed", map[string]any{"response": map[string]any{"status": "failed", "error": map[string]any{
			"code": "context_length_exceeded", "message": "Your input exceeds the context window of this model",
		}}}))
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	if err == nil || n.Load() != 1 {
		t.Fatalf("overflow must not be retried: requests=%d err=%v", n.Load(), err)
	}
	var pe *providerError
	if !errors.As(err, &pe) || pe.kind != "context_length_exceeded" || transient(err) {
		t.Fatalf("err = %#v", err)
	}
	assertOverflowRecognizable(t, err)
}

// assertOverflowRecognizable mirrors runtime.contextOverflow (this package
// cannot import runtime): context/maximum AND length/tokens in the text.
func assertOverflowRecognizable(t *testing.T, err error) {
	t.Helper()
	s := strings.ToLower(err.Error())
	if !((strings.Contains(s, "context") || strings.Contains(s, "maximum")) &&
		(strings.Contains(s, "length") || strings.Contains(s, "tokens"))) {
		t.Fatalf("error not recognizable as context overflow: %q", s)
	}
}

func TestResponsesTopLevelErrorEvent200(t *testing.T) {
	shrink(t)
	// Not transient: invalid request.
	srv, n, _, _ := respServer(t, ev("error", map[string]any{"code": "invalid_request_error", "message": "bad input"}))
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	if err == nil || n.Load() != 1 || !strings.Contains(err.Error(), "invalid_request_error") || !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
	// Transient: server_error then success.
	var calls atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, ev("error", map[string]any{"code": "server_error", "message": "boom"}))
			return
		}
		io.WriteString(w, ev("response.output_text.delta", map[string]any{"delta": "ok"})+completed(nil))
	}))
	defer srv2.Close()
	r, err := Novo(respCfg(srv2.URL)).Rodar("s", nil, nil)
	if err != nil || r.Texto != "ok" || calls.Load() != 2 {
		t.Fatalf("requests=%d err=%v", calls.Load(), err)
	}
}

func TestResponsesNestedErrorObject200(t *testing.T) {
	// {"error":{...}} without a type, as plain gateways send it.
	srv, _, _, _ := respServer(t, "data: "+`{"error":{"message":"maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`+"\n\n")
	shrink(t)
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "context_length_exceeded") || transient(err) {
		t.Fatalf("err = %v", err)
	}
	assertOverflowRecognizable(t, err)
}

func TestResponsesFailedRetriesWhenTransient(t *testing.T) {
	shrink(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			io.WriteString(w, ev("response.failed", map[string]any{"response": map[string]any{"error": map[string]any{"code": "rate_limit_exceeded", "message": "slow down"}}}))
			return
		}
		io.WriteString(w, completed(nil))
	}))
	defer srv.Close()
	if _, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil); err != nil || calls.Load() != 2 {
		t.Fatalf("requests=%d err=%v", calls.Load(), err)
	}
}

func TestResponsesNoTerminalIsUnexpectedEOF(t *testing.T) {
	shrink(t)
	for name, stream := range map[string][]string{
		"cut":       {ev("response.output_text.delta", map[string]any{"delta": "par"})},
		"done-only": {ev("response.output_text.delta", map[string]any{"delta": "par"}), "data: [DONE]\n\n"},
		"empty":     {},
	} {
		srv, n, _, _ := respServer(t, stream...)
		_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if n.Load() != int32(maxAttempts) {
			t.Fatalf("%s: a cut stream is transient and must be retried: %d requests", name, n.Load())
		}
	}
}

func TestResponsesDoneAfterTerminalAccepted(t *testing.T) {
	r, err := runResp(t, ev("response.output_text.delta", map[string]any{"delta": "ok"}), completed(nil), "data: [DONE]\n\n")
	if err != nil || r.Texto != "ok" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestResponsesLargeEvents(t *testing.T) {
	big := strings.Repeat("x", 5<<20) // the completed object echoes more than 4 MiB
	r, err := runResp(t,
		ev("response.output_text.delta", map[string]any{"delta": strings.Repeat("y", 5<<20)}),
		completed(map[string]any{"instructions": big, "usage": map[string]any{"input_tokens": 1, "output_tokens": 2}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Texto) != 5<<20 || r.Usage == nil || r.Usage.OutputTokens != 2 {
		t.Fatalf("len=%d usage=%+v", len(r.Texto), r.Usage)
	}
}

func TestResponsesIgnoresUnknownEventsAndComments(t *testing.T) {
	r, err := runResp(t,
		": ping\n\n",
		ev("response.in_progress", map[string]any{"response": map[string]any{}}),
		ev("response.content_part.added", map[string]any{"item_id": "m"}),
		ev("response.web_search_call.searching", map[string]any{}),
		ev("response.output_text.delta", map[string]any{"item_id": "m", "delta": "ok"}),
		completed(nil),
	)
	if err != nil || r.Texto != "ok" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestResponsesInvalidChunk(t *testing.T) {
	srv, _, _, _ := respServer(t, "data: {not json\n\n")
	shrink(t)
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid chunk") {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesNon200TypedErrors(t *testing.T) {
	shrink(t)
	// 400 with a code: one attempt, code in the summary, overflow recognizable.
	srv, n := statusServer(t, 400, 9, nil, `{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`)
	_, err := Novo(respCfg(srv.URL)).Rodar("s", nil, nil)
	var he *httpError
	if !errors.As(err, &he) || he.status != 400 || n.Load() != 1 {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
	if !strings.Contains(err.Error(), "context_length_exceeded") || !strings.Contains(err.Error(), "maximum context length") {
		t.Fatalf("summary lost the code/message: %v", err)
	}
	assertOverflowRecognizable(t, err)
	// 429 rate limit retries; insufficient_quota does not.
	srv, n = statusServer(t, 429, 1, map[string]string{"Retry-After-Ms": "1"}, `{"error":{"code":"rate_limit_exceeded","message":"slow"}}`)
	// The retry reaches the OK path of statusServer, which speaks Chat; use
	// a Responses success instead.
	srv.Close()
	var calls atomic.Int32
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"slow"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, completed(nil))
	}))
	defer srv2.Close()
	if _, err := Novo(respCfg(srv2.URL)).Rodar("s", nil, nil); err != nil || calls.Load() != 2 {
		t.Fatalf("429 requests=%d err=%v", calls.Load(), err)
	}
	srv3, n3 := statusServer(t, 429, 9, nil, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`)
	if _, err := Novo(respCfg(srv3.URL)).Rodar("s", nil, nil); err == nil || n3.Load() != 1 {
		t.Fatalf("quota requests=%d err=%v", n3.Load(), err)
	}
}

func TestResponsesCancellationMidStream(t *testing.T) {
	shrink(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ev("response.output_text.delta", map[string]any{"delta": "par"}))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := Novo(respCfg(srv.URL)).RodarContext(ctx, "s", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
