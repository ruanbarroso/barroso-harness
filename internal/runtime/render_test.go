package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// The plain renderer is what `-p` uses (and what the REPL uses when its stdout
// is a pipe). These tests build it with the quiet mode set explicitly, so they
// do not depend on the environment: NovoRendererPuro reads EnvSoResposta once,
// at construction, and the package's TestMain keeps the variable unset.
//
// The tests below need no provider: they call the methods directly.

func TestRendererPuroModoPadraoTemLogDeFerramentas(t *testing.T) {
	var out strings.Builder
	r := NovoRendererPuro(&out)
	r.Tool("shell", time.Second, false)
	r.Eco("exit_code: 0")
	r.Guardrail(true)
	r.Cortada(8000, false)
	r.ProtocoloAlternativo(dealer.ProtocoloResponses, dealer.ProtocoloChat)
	r.Resposta("done")

	got := out.String()
	for _, want := range []string{
		"[tool shell ok",
		"exit_code: 0",
		guardrailProactive,
		"[output limit] response cut at 8000 tokens; continuing",
		"[protocol] responses endpoint unavailable; using chat",
		"done",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("default mode must keep the tool log and every notice, missing %q in %q", want, got)
		}
	}
}

func TestRendererPuroFinalOnlyImprimeSoAResposta(t *testing.T) {
	var out, diag strings.Builder
	q := &RendererPuro{out: &out, quiet: true, diag: &diag}
	q.Tool("shell", time.Second, false)
	q.Eco("exit_code: 0")
	q.Guardrail(true)
	q.Guardrail(false)
	q.Cortada(8000, false)
	q.Cortada(8000, true)
	q.ProtocoloAlternativo(dealer.ProtocoloResponses, dealer.ProtocoloChat)
	q.Resposta("done")

	if got := out.String(); got != "done\n" {
		t.Fatalf("%s=1 must print only the answer, got %q", EnvSoResposta, got)
	}
	for _, want := range []string{
		guardrailProactive,
		guardrailReactive,
		"[output limit] response cut at 8000 tokens; continuing",
		"[output limit] response cut at 8000 tokens; giving up",
		"[protocol] responses endpoint unavailable; using chat",
	} {
		if !strings.Contains(diag.String(), want) {
			t.Fatalf("quiet mode must move %q to the diagnostics stream, got %q", want, diag.String())
		}
	}
}

// The run notices go to stderr by default: an operator who runs `bh -p` sees
// them on screen while stdout stays a clean answer. notice() reads os.Stderr at
// call time, so the test swaps it for a pipe.
func TestRendererPuroFinalOnlyMandaAvisosParaOStderr(t *testing.T) {
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = wr
	t.Cleanup(func() { os.Stderr = old })

	var out strings.Builder
	q := &RendererPuro{out: &out, quiet: true} // diag nil: the default, os.Stderr
	q.Guardrail(true)
	q.Cortada(8000, false)
	q.ProtocoloAlternativo(dealer.ProtocoloResponses, dealer.ProtocoloChat)
	os.Stderr = old
	_ = wr.Close()

	b, _ := io.ReadAll(rd)
	_ = rd.Close()
	for _, want := range []string{guardrailProactive, "[output limit]", "[protocol]"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("notice %q missing from stderr, got %q", want, b)
		}
	}
	if out.Len() != 0 {
		t.Fatalf("notices must not touch the answer stream, got %q", out.String())
	}
}

// The quiet mode is decided when the renderer is built, not per method call:
// setting the variable after construction changes nothing.
func TestFinalOnlyELidoUmaVezNoConstrutor(t *testing.T) {
	t.Setenv(EnvSoResposta, "")
	var sb strings.Builder
	r := NovoRendererPuro(&sb)
	t.Setenv(EnvSoResposta, "1")
	r.Tool("shell", time.Second, false)
	if !strings.Contains(sb.String(), "[tool shell ok") {
		t.Fatalf("renderer built without the variable must keep the tool log even if it is set later, got %q", sb.String())
	}

	sb.Reset()
	q := NovoRendererPuro(&sb) // built AFTER the variable was set
	q.Tool("shell", time.Second, false)
	if sb.Len() != 0 {
		t.Fatalf("renderer built with the variable must be quiet, got %q", sb.String())
	}
}

// End to end: a whole turn (tool call, streaming, answer) through Turno with the
// variable set prints only the answer. Without it, the log stays.
func TestTurnoInteiroFinalOnlySaiSoAResposta(t *testing.T) {
	srv := fakeProvider()
	defer srv.Close()

	var normal strings.Builder
	if err := Turno(&normal, chatTestConfig(srv.URL, "fake", "k"), t.TempDir(), "show the loop runs"); err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	if !strings.Contains(normal.String(), "[tool shell ok") || !strings.Contains(normal.String(), "loop-tuple") {
		t.Fatalf("default turn must keep the tool log, got %q", normal.String())
	}

	t.Setenv(EnvSoResposta, "1")
	var quiet strings.Builder
	if err := Turno(&quiet, chatTestConfig(srv.URL, "fake", "k"), t.TempDir(), "show the loop runs"); err != nil {
		t.Fatalf("final-only turn failed: %v", err)
	}
	if got := quiet.String(); got != "result received and closing.\n" {
		t.Fatalf("final-only turn must print only the answer, got %q", got)
	}
}

// Only the exact value "1" turns the mode on.
func TestFinalOnlySoComValorExato1(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes", " 1", "1 ", "2"} {
		t.Run("valor="+v, func(t *testing.T) {
			t.Setenv(EnvSoResposta, v)
			var sb strings.Builder
			NovoRendererPuro(&sb).Tool("shell", time.Second, false)
			if !strings.Contains(sb.String(), "[tool shell ok") {
				t.Fatalf("%q must NOT turn the mode on, got %q", v, sb.String())
			}
		})
	}
	t.Setenv(EnvSoResposta, "1")
	var sb strings.Builder
	NovoRendererPuro(&sb).Tool("shell", time.Second, false)
	if sb.Len() != 0 {
		t.Fatalf(`"1" must turn the mode on, got %q`, sb.String())
	}
}

// A response cut at the output limit raises the notice in the middle of the
// turn. In final-only mode the notice must not reach the answer stream, and the
// answer must still be complete.
func TestCorteNoModoFinalOnlyNaoPoluiARespostaNoTurnoInteiro(t *testing.T) {
	srv, _ := cutProvider(t, []step{{"part one ", true, "length"}, {"part two", false, "stop"}})
	t.Setenv(EnvSoResposta, "1")

	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = wr
	t.Cleanup(func() { os.Stderr = old })

	var out strings.Builder
	runErr := Turno(&out, chatTestConfig(srv.URL, "m", "k"), t.TempDir(), "hi")
	os.Stderr = old
	_ = wr.Close()
	diag, _ := io.ReadAll(rd)
	_ = rd.Close()
	if runErr != nil {
		t.Fatal(runErr)
	}

	if got := out.String(); strings.Contains(got, "[output limit]") || strings.Contains(got, "[tool ") {
		t.Fatalf("notices leaked into the answer stream: %q", got)
	}
	if got := out.String(); !strings.Contains(got, "part one") || !strings.Contains(got, "part two") {
		t.Fatalf("answer incomplete: %q", got)
	}
	if !strings.Contains(string(diag), "[output limit] response cut") {
		t.Fatalf("cut notice must reach stderr, got %q", diag)
	}
}

// A provider without the Responses route: the dealer retries on Chat
// Completions and the session reports the fallback. In final-only mode the
// notice goes to stderr and stdout carries only the answer.
func TestProtocolFallbackNoModoFinalOnlyNaoPoluiARespostaNoTurnoInteiro(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/responses") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	t.Setenv(EnvSoResposta, "1")

	cfg := config.Config{
		CurrentProvider: "test",
		Providers: map[string]*config.Provider{
			"test": {BaseURL: srv.URL + "/v1", APIKey: "k", Protocol: config.ProtocoloResponses},
		},
		Model: "fake", Janela: 100_000,
	}

	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = wr
	t.Cleanup(func() { os.Stderr = old })

	var out strings.Builder
	runErr := NewSession(cfg).Send(context.Background(), NovoRendererPuro(&out), t.TempDir(), "hi")
	os.Stderr = old
	_ = wr.Close()
	diag, _ := io.ReadAll(rd)
	_ = rd.Close()
	if runErr != nil {
		t.Fatalf("send: %v", runErr)
	}

	if got := out.String(); got != "fallback ok\n" {
		t.Fatalf("final-only fallback must print only the answer, got %q", got)
	}
	if !strings.Contains(string(diag), "[protocol] responses endpoint unavailable; using chat") {
		t.Fatalf("fallback notice must reach stderr, got %q", diag)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 2 || !strings.HasSuffix(paths[0], "/responses") || !strings.HasSuffix(paths[1], "/chat/completions") {
		t.Fatalf("paths = %v; want /responses then /chat/completions", paths)
	}
}
