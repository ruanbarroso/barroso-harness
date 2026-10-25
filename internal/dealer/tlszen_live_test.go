package dealer

import (
	"os"
	"testing"
)

// TestZenAoVivo hits the real zen free tier (ZEN_LIVE=1 only): the BH
// contract plus Bun's TLS hello must get a 200 from the gate.
func TestZenAoVivo(t *testing.T) {
	if os.Getenv("ZEN_LIVE") != "1" {
		t.Skip("ZEN_LIVE=1 to hit opencode.ai")
	}
	c := Novo(Config{BaseURL: "https://opencode.ai/zen/v1", Model: "big-pickle", APIKey: "public"}).ComConversa("live")
	r, err := c.Rodar("You are terse.", []Mensagem{{Role: "user", Conteudo: "reply only OK"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("zen respondeu: %q", r.Texto)
}
