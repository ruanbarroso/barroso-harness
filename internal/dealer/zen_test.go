package dealer

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestIDOpencodeFormato pins the canonical shape: `prefixo_` + 12 lowercase
// hex + 14 base62, and the 48-bit prefix reconstructs, upstream-style, as
// ~(recent ms * 0x1000 + small counter) — a forged or clock-less id would not
// match any candidate within a ±2 s window with a counter in [1, 64].
func TestIDOpencodeFormato(t *testing.T) {
	re := regexp.MustCompile(`^(ses|msg|call|gen)_([0-9a-f]{12})([0-9A-Za-z]{14})$`)
	for _, pref := range []string{"ses", "msg", "call", "gen"} {
		id := idOpencode(pref)
		m := re.FindStringSubmatch(id)
		if m == nil {
			t.Fatalf("id %q does not match the canonical shape", id)
		}
		if len(id) != len(pref)+1+12+14 {
			t.Fatalf("id %q has the wrong total length", id)
		}
		n, err := strconv.ParseUint(m[2], 16, 64)
		if err != nil {
			t.Fatalf("hex prefix of %q: %v", id, err)
		}
		x := ^n & 0xFFFFFFFFFFFF
		honesto := false
		for s := uint64(0); s <= 2000; s++ {
			base := uint64(time.Now().UnixMilli()) - s
			for c := uint64(1); c <= 64; c++ {
				if (base*0x1000+c)&0xFFFFFFFFFFFF == x {
					honesto = true
				}
			}
		}
		if !honesto {
			t.Fatalf("prefix of %q does not decode to a honest (ms, counter) pair: x=%d", id, x)
		}
	}
}

// TestIDOpencodeContadorPorMs: two ids inside the same millisecond have
// consecutive counters (the upstream validates the counter range).
func TestIDOpencodeContadorPorMs(t *testing.T) {
	re := regexp.MustCompile(`_([0-9a-f]{12})`)
	for i := 0; i < 50; i++ {
		a, b := idOpencode("ses"), idOpencode("ses")
		na, _ := strconv.ParseUint(re.FindStringSubmatch(a)[1], 16, 64)
		nb, _ := strconv.ParseUint(re.FindStringSubmatch(b)[1], 16, 64)
		if na != nb {
			time.Sleep(300 * time.Microsecond) // crossed the millisecond; retry
			continue
		}
		xa, xb := ^na&0xFFFFFFFFFFFF, ^nb&0xFFFFFFFFFFFF
		if xb-xa == 1 {
			return // consecutive counters in the same ms: right
		}
		t.Fatalf("ids inside the same ms must count by one: %s / %s", a, b)
	}
}

// TestUserAgentForma pins the UA shape and the override knob.
func TestUserAgentForma(t *testing.T) {
	os.Unsetenv("ZEN_USER_AGENT")
	ua := UserAgent()
	parts := strings.Split(ua, "/")
	if len(parts) != 4 || parts[0] != "opencode" {
		t.Fatalf("UserAgent() = %q; want opencode/<canal>/<versão>/<cliente>", ua)
	}
	if got := uaForma("1.2.3"); got != "opencode/latest/1.2.3/cli" {
		t.Fatalf("uaForma = %q", got)
	}
	t.Setenv("ZEN_USER_AGENT", "opencode/latest/8.8.8/test")
	if UserAgent() != "opencode/latest/8.8.8/test" {
		t.Fatalf("ZEN_USER_AGENT override ignored: %q", UserAgent())
	}
	os.Unsetenv("ZEN_USER_AGENT")
}

// TestSessaoEstavel: one conversation, one id, for the whole lifetime — and
// one-shot (empty key) traffic mints fresh ids.
func TestSessaoEstavel(t *testing.T) {
	a := sessaoEstavel("conv-1")
	if b := sessaoEstavel("conv-1"); b != a {
		t.Fatal("same conversation must reuse the same session id")
	}
	if b := sessaoEstavel("conv-2"); b == a {
		t.Fatal("different conversations must not share a session id")
	}
	if sessaoEstavel("") == sessaoEstavel("") {
		t.Fatal("one-shot traffic must mint a fresh id per call")
	}
	if !strings.HasPrefix(a, "ses_") || len(a) != 30 {
		t.Fatalf("session id shape: %q", a)
	}
}

// TestCorpoFingerprint pins the wire contract: read/shell travel as the
// official byte-exact literals, tool_choice is auto, stream is always on,
// and appended decoys get their brake on the system slot.
func TestCorpoFingerprint(t *testing.T) {
	c := chatClient(Config{BaseURL: "https://x/v1", Model: "m", APIKey: "z", Effort: "high"})
	tools := []map[string]any{
		{"type": "function", "function": map[string]any{
			"name": "edit", "description": "custom edit", "parameters": map[string]any{"type": "object"},
		}},
		{"type": "function", "function": map[string]any{
			"name": "read", "description": "custom stub read", "parameters": map[string]any{"type": "object"},
		}},
	}
	raw, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, tools)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice string            `json:"tool_choice"`
		Stream     bool              `json:"stream"`
		Messages   []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("corpo: %v", err)
	}
	if !m.Stream || m.ToolChoice != "auto" {
		t.Fatalf("stream=%v tool_choice=%q; want true/auto", m.Stream, m.ToolChoice)
	}
	achou := 0
	for _, w := range m.Tools {
		var fn struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(w, &fn); err != nil {
			t.Fatalf("tool entry: %v", err)
		}
		switch fn.Function.Name {
		case "read":
			if !strings.Contains(string(w), `"description":"Read the contents of a file or directory`) {
				continue // a stub that passed through would fail the gate
			}
			achou++
		case "shell":
			if !strings.Contains(string(w), `"required":["command"]`) {
				continue
			}
			achou++
		case "edit":
			// the custom tool rides through untouched by name
		default:
			t.Fatalf("unexpected tool on the wire: %s", fn.Function.Name)
		}
	}
	if achou != 2 {
		t.Fatalf("read/shell official literals missing from the wire tools (%d of 2)", achou)
	}
	if m.Messages[0].Role != "system" || !strings.Contains(m.Messages[0].Content, "placeholder") {
		t.Fatalf("decoy brake missing; first message: %+v", m.Messages[0])
	}
	if !strings.HasPrefix(m.Messages[1].Content, "sys\n\n") || !strings.Contains(m.Messages[1].Content, "<env>") {
		t.Fatalf("official system must follow the brake: %+v", m.Messages[1])
	}
}

// TestCorpoSemTools: a request with no tools STILL travels with the official
// pair (auto, appended decoys) — the measured no-tools rejection.
func TestCorpoSemTools(t *testing.T) {
	c := chatClient(Config{BaseURL: "https://x/v1", Model: "m", APIKey: "z"})
	raw, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice string            `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Tools) != 2 || m.ToolChoice != "auto" {
		t.Fatalf("tools=%d tool_choice=%q; want 2/auto", len(m.Tools), m.ToolChoice)
	}
}

// TestDesduplicarIDsDeChamada: history with a repeated call id gets its
// second occurrence and its paired result renamed together.
func TestDesduplicarIDsDeChamada(t *testing.T) {
	list := []oaiMsg{
		{Role: "user", Content: strNil("vai")},
		{Role: "assistant", ToolCalls: []oaiCall{{ID: "x1"}, {ID: "y1"}}},
		{Role: "tool", Content: strNil("ok"), ToolCallID: "x1"},
		{Role: "tool", Content: strNil("ok y"), ToolCallID: "y1"},
		{Role: "user", Content: strNil("de novo")},
		{Role: "assistant", ToolCalls: []oaiCall{{ID: "x1"}}},
		{Role: "tool", Content: strNil("de novo ok"), ToolCallID: "x1"},
	}
	desduplicarIDsDeChamada(list)
	if list[1].ToolCalls[0].ID != "x1" || list[1].ToolCalls[1].ID != "y1" ||
		list[2].ToolCallID != "x1" || list[3].ToolCallID != "y1" {
		t.Fatal("first occurrences must keep their ids (calls and results)")
	}
	segunda := list[5].ToolCalls[0].ID
	if segunda == "x1" || segunda == "" {
		t.Fatalf("second occurrence must be renamed: %q", segunda)
	}
	if list[6].ToolCallID != segunda {
		t.Fatalf("paired result must be renamed together: call=%q result=%q", segunda, list[6].ToolCallID)
	}
}

// TestRodarEnviaContrato: full headers on a real round — session stable per
// conversation, no x-opencode-request, the client identity, the whole pair.
func TestRodarEnviaContrato(t *testing.T) {
	t.Setenv("ZEN_USER_AGENT", "opencode/latest/9.9.9/test")
	var sessao1, sessao2, request1, cliente, projeto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessao1 == "" {
			sessao1 = r.Header.Get("x-opencode-session")
			request1 = r.Header.Get("x-opencode-request")
			cliente = r.Header.Get("x-opencode-client")
			projeto = r.Header.Get("x-opencode-project")
			if r.Header.Get("x-opencode-session-id") != sessao1 {
				t.Error("x-opencode-session-id must travel WITH x-opencode-session")
			}
			if r.Header.Get("x-session-affinity") != sessao1 || r.Header.Get("x-session-id") != sessao1 {
				t.Error("the affinity twins must carry the same session id")
			}
		} else {
			sessao2 = r.Header.Get("x-opencode-session")
		}
		if r.Header.Get("User-Agent") != "opencode/latest/9.9.9/test" {
			t.Errorf("round UA: %q", r.Header.Get("User-Agent"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io := bufio.NewWriter(w)
		fmt.Fprint(io, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
		io.Flush()
	}))
	defer srv.Close()
	c := chatClient(Config{BaseURL: srv.URL, Model: "m", APIKey: "z"}).ComConversa("conv-rodar")
	if _, err := c.Rodar("sys", []Mensagem{{Role: "user", Conteudo: "1"}}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Rodar("sys", []Mensagem{{Role: "user", Conteudo: "2"}}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sessao1, "ses_") || sessao1 != sessao2 {
		t.Fatalf("session must be stable per conversation: %q / %q", sessao1, sessao2)
	}
	if request1 != "" {
		t.Fatalf("x-opencode-request %q: the official CLI 2.1.4 does not send it", request1)
	}
	if cliente != "cli" || projeto != "global" {
		t.Fatalf("client=%q project=%q; want cli/global", cliente, projeto)
	}
}
