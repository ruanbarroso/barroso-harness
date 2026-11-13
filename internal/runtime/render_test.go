package runtime

import (
	"strings"
	"testing"
	"time"
)

func TestRendererPuroFinalOnly(t *testing.T) {
	var sb strings.Builder
	r := NovoRendererPuro(&sb)
	r.Tool("shell", time.Second, false)
	r.Eco("exit_code: 0")
	r.Resposta("done")
	if got := sb.String(); !strings.Contains(got, "[tool shell ok") || !strings.Contains(got, "exit_code: 0") {
		t.Fatalf("default mode must keep the tool log, got %q", got)
	}

	t.Setenv(EnvSoResposta, "1")
	sb.Reset()
	q := NovoRendererPuro(&sb)
	q.Tool("shell", time.Second, false)
	q.Eco("exit_code: 0")
	q.Guardrail(true)
	q.Resposta("done")
	if got := sb.String(); got != "done\n" {
		t.Fatalf("%s=1 must print only the answer, got %q", EnvSoResposta, got)
	}
}
