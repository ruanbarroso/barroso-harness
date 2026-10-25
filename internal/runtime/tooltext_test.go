package runtime

import (
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

func TestLooksLikeToolText(t *testing.T) {
	yes := []string{
		"Registrando.\n\n(tool_call) name=okf_create arguments={\"concept_id\": \"x\", \"body\": \"" + strings.Repeat("a ", 2000) + "\"}",
		"tool_call name=context_read argument{}",
		"Vou ler.\n<tool_call>\n<function=read>\n<parameter=path>a</parameter>\n</function>\n</tool_call>",
	}
	for _, s := range yes {
		if !looksLikeToolText(s) {
			t.Errorf("should detect: %.80q", s)
		}
	}
	no := []string{
		"Pronto, tudo verde.",
		"O modelo escreveu (tool_call) name=okf_create arguments={...} como texto. Quer que eu corrija?",
		"{\"a\": 1}",
	}
	for _, s := range no {
		if looksLikeToolText(s) {
			t.Errorf("should not detect: %q", s)
		}
	}
}

// A final round with a leaked call is re-asked with the notice, and the next
// round's real answer ends the turn; the leaked text is not delivered alone.
func TestToolTextIsReasked(t *testing.T) {
	leaked := "(tool_call) name=shell arguments={\"command\": \"echo hi\"}"
	srv, seen := cutProvider(t, []step{{leaked, false, "stop"}, {"done", false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	v := *seen
	if len(v) != 2 || !strings.HasPrefix(v[1].Last, "[tool call as text]") {
		t.Fatalf("requests = %+v", v)
	}
	if !strings.Contains(sb.String(), "done") {
		t.Fatalf("output:\n%s", sb.String())
	}
}

// The re-ask is bounded: a model that keeps leaking gets its text delivered.
func TestToolTextGivesUpAfterCap(t *testing.T) {
	leaked := "(tool_call) name=shell arguments={\"command\": \"echo hi\"}"
	srv, seen := cutProvider(t, []step{{leaked, false, "stop"}})
	var sb strings.Builder
	if err := Turno(&sb, config.Config{BaseURL: srv.URL, Model: "m", APIKey: "k"}, t.TempDir(), "hi"); err != nil {
		t.Fatal(err)
	}
	if n := len(*seen); n != 1+maxToolTextRetries {
		t.Fatalf("requests = %d; want %d", n, 1+maxToolTextRetries)
	}
}
