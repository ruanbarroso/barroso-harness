package dealer

import (
	"encoding/json"
	"errors"
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

// The BK refuses a forced tool_choice for claude-*-5-5 (adaptive reasoning)
// with a 400 `tool_choice_nao_suportado`. The runtime must recognize it by
// the structured code (and by the message of other gateways), and nothing else.
func TestToolChoiceRejeitado(t *testing.T) {
	bk := []byte(`{"error":{"type":"invalid_request_error","code":"tool_choice_nao_suportado","message":"\"claude-sonnet-5-5\" só aceita \"tool_choice\" ausente, \"auto\" ou \"none\""}}`)
	he := &httpError{status: 400, code: upstreamErrorCode(bk), summary: resumoComCodigo(bk)}
	if !ToolChoiceRejeitado(he) {
		t.Fatalf("BK 400 not recognized: %v", he)
	}
	other := &httpError{status: 400, summary: "tool_choice is not supported with thinking enabled"}
	if !ToolChoiceRejeitado(other) {
		t.Fatal("generic tool_choice 400 not recognized")
	}
	pe := newProviderError(0, "invalid_request_error", "tool_choice unsupported for this model")
	if !ToolChoiceRejeitado(pe) {
		t.Fatal("stream error not recognized")
	}
	for _, e := range []error{
		&httpError{status: 500, code: "tool_choice_nao_suportado", summary: "x"},
		&httpError{status: 400, summary: "context_length_exceeded"},
		&httpError{status: 400, summary: "invalid tools[0].function.parameters"},
		newProviderError(0, "server_error", "boom"),
		errors.New("tool_choice_nao_suportado"),
		nil,
	} {
		if ToolChoiceRejeitado(e) {
			t.Fatalf("false positive: %v", e)
		}
	}
}
