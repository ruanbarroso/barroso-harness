package dealer

import (
	"encoding/json"
	"strings"
	"testing"
)

var compactToolSchema = []map[string]any{{"type": "function", "function": map[string]any{
	"name": "context_compact", "description": "d",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{"state": map[string]any{"type": "string"}}},
}}}

// The runtime forces context_compact on the wire, not only in the prompt: a
// model that ignores a written demand is the common case on the fleet.
func TestForcedToolChoiceOnBothProtocols(t *testing.T) {
	for _, proto := range []string{"chat", "responses"} {
		c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: proto}).ComToolChoice("context_compact")
		b, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, compactToolSchema)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		tc, ok := m["tool_choice"].(map[string]any)
		if !ok || tc["type"] != "function" {
			t.Fatalf("%s: tool_choice = %v; want a forced function: %s", proto, m["tool_choice"], b)
		}
		name := tc["name"]
		if fn, ok := tc["function"].(map[string]any); ok {
			name = fn["name"]
		}
		if name != "context_compact" {
			t.Fatalf("%s: forced name = %v", proto, name)
		}
	}
}

// A forced name the catalog does not have must not reach the provider (it
// would reject the whole request); a plain client stays on "auto".
func TestForcedToolChoiceFallsBackToAuto(t *testing.T) {
	for _, proto := range []string{"chat", "responses"} {
		missing := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: proto}).ComToolChoice("nope")
		plain := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: proto})
		for name, c := range map[string]*Cliente{"missing": missing, "plain": plain} {
			b, _ := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, compactToolSchema)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if m["tool_choice"] != "auto" {
				t.Fatalf("%s/%s: tool_choice = %v; want auto", proto, name, m["tool_choice"])
			}
		}
	}
}

// The harness-initiated summarization call must carry tool_choice "none" on
// both protocols — the one non-auto mode every provider accepts, including
// claude-*-5-5 with adaptive reasoning — and keep the tools in the request.
func TestSemChamadasSendsToolChoiceNone(t *testing.T) {
	for _, proto := range []string{"chat", "responses"} {
		c := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: proto}).SemChamadas()
		b, err := c.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, compactToolSchema)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["tool_choice"] != "none" {
			t.Fatalf("%s: tool_choice = %v; want none: %s", proto, m["tool_choice"], b)
		}
		if tl, _ := m["tools"].([]any); len(tl) == 0 || !strings.Contains(string(b), `"name":"context_compact"`) {
			t.Fatalf("%s: tools must stay in the request: %s", proto, b)
		}
	}
	// The option does not leak into the original client.
	base := Novo(Config{BaseURL: "https://x/v1", Model: "m", Protocol: "chat"})
	_ = base.SemChamadas()
	b, _ := base.Corpo("sys", []Mensagem{{Role: "user", Conteudo: "hi"}}, compactToolSchema)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["tool_choice"] != "auto" {
		t.Fatalf("base client changed: %v", m["tool_choice"])
	}
}
