package dealer

import (
	"encoding/json"
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
