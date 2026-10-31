package cli

import (
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// A background-job notice is not user speech: it never names the session.
func TestSessionTitleIgnoresShellNotice(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "[shell] Background jobs finished:\n\n## job-1 exit 0 (1s)\n"},
		{Role: "user", Conteudo: "fix the parser"},
	}
	if got := sessionTitle(h); got != "fix the parser" {
		t.Fatalf("title = %q", got)
	}
}
