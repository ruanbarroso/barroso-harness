package runtime

// Turn output layer. The runtime does not decide COLOR or glyph: it describes
// what happened and the writer picks the styling. Two implementations:
//
//   - RendererPuro: raw text, not a single escape character. It is what `-p`
//     uses (the site and panel recipes pipe the output and must not get ANSI)
//     and what tests use — the "[tool <name> ok" prefix stays LITERAL, because
//     runtime_test.go looks for it as a substring.
//   - the REPL TUI (package cli): 256-color palette, UTF-8 glyphs and a token
//     footer.

import (
	"fmt"
	"io"
	"time"
)

// Renderer is the output of a turn: model text, tool echo, guardrail notices
// and the token footer.
type Renderer interface {
	// Resposta writes the model's reply with the trailing newline.
	Resposta(text string)
	// Tool draws the header of an executed tool (name, duration, failure).
	Tool(name string, dur time.Duration, failed bool)
	// Eco echoes an excerpt of the tool result (the history already got it all).
	Eco(output string)
	// Guardrail reports a compaction: proactive (predicted overflow) or
	// reactive (the provider rejected it for context size).
	Guardrail(proactive bool)
	// Tokens is the turn footer: size of the request sent, tokens generated,
	// duration and whether any compaction ran. `compacted` alone already tells
	// a normal turn apart from one that went through the guardrail.
	Tokens(prompt, output int, dur time.Duration, compacted bool)
}

// RendererDetalhado is OPTIONAL: implementers also receive the start of each
// tool (with its arguments, before executing) and the context size of the
// last request. The TUI uses it to show "running <tool>" and the context %;
// ACP, subagents and plain mode do not need it and stick to Renderer.
type RendererDetalhado interface {
	// Pensando: the request to the model is about to go out (the TUI animates
	// the wait).
	Pensando()
	ToolInicio(name, args string)
	Contexto(promptTokens int)
}

// RendererUpdate is OPTIONAL: it receives each [system update] message the
// runtime appends (skills, BARROSO.md or MCP changed mid-session).
type RendererUpdate interface {
	Atualizado(text string)
}

// RendererCache is OPTIONAL: it receives, per request, how many of the prompt
// tokens the provider served from its prompt cache (the TUI shows "cache N%").
type RendererCache interface {
	Cache(cachedTokens, promptTokens int)
}

// RendererRetry is OPTIONAL: it is told before the dealer repeats a request
// that failed transiently (503, timeout, reset), so the client can show
// "retrying" instead of silence.
type RendererRetry interface {
	Retentando(attempt, max int, wait time.Duration, reason error)
}

// RendererProtocolFallback is OPTIONAL: it receives a notice when a provider
// lacks Responses and the dealer uses Chat Completions instead.
type RendererProtocolFallback interface {
	ProtocoloAlternativo(from, to string)
}

// RendererRaciocinio is OPTIONAL: it receives the round's reasoning when the
// provider exposes it, before tools and the answer (the TUI shows "Thought").
type RendererRaciocinio interface {
	Pensou(text string, dur time.Duration)
}

// RendererResultado is OPTIONAL: it receives each tool's call and its WHOLE
// result in place of Tool+Eco, which only carry the name and the first lines.
// The full screen draws per-tool rows from it (a shell block keeps the tail of
// the output, an edit block shows the diff from the arguments).
type RendererResultado interface {
	ToolResultado(name, args, output string, dur time.Duration, failed bool)
}

// RendererAoVivo is OPTIONAL and takes precedence over ToolInicio and
// ToolResultado: each call is announced when it starts, its live output
// (shell) arrives while it runs, and the result closes the same call. Calls in
// a parallel round can finish in any order, so ToolSaida and ToolTerminou may
// overlap and are invoked from the tool's goroutine. Implementations must be
// concurrency-safe. id tells concurrent calls of a round apart.
type RendererAoVivo interface {
	ToolComecou(id, name, args string)
	ToolSaida(id, chunk string)
	ToolTerminou(id, name, args, output string, dur time.Duration, failed bool)
}

// RendererNota is OPTIONAL: it receives the text the model wrote in a round
// that also calls tools, before those tools run. It is not the answer —
// Resposta still gets only the final round — so collectors that keep the
// answer (subagents) are not polluted by "let me check X".
type RendererNota interface {
	Nota(text string)
}

// RendererSteer is OPTIONAL: it is told when a message sent during the turn
// (Session.AddSteer) was delivered to the model.
type RendererSteer interface {
	Entregue(text string)
}

// RendererCorte is OPTIONAL: it reports that the response was cut at the
// output limit. gaveUp=false: the turn will continue on its own; true: it
// stopped.
type RendererCorte interface {
	Cortada(limit int, gaveUp bool)
}

// RendererCusto is OPTIONAL: it receives the turn's money cost in USD at the
// end of each request, but ONLY when the active model has a declared price
// (prices.json in the harness home). No price = no call, so the UI never shows
// an invented number: tokens remain the metric.
type RendererCusto interface {
	Custo(usd float64)
}

// RendererPuro is the unadorned implementation.
type RendererPuro struct{ out io.Writer }

// NovoRendererPuro creates the raw-text renderer over out.
func NovoRendererPuro(out io.Writer) *RendererPuro { return &RendererPuro{out: out} }

// RendererDe adapts an io.Writer to the interface — compatibility with older
// callers and with the `-p` path, which only knows how to write bytes.
func RendererDe(out io.Writer) Renderer { return NovoRendererPuro(out) }

func (p *RendererPuro) Resposta(text string) { fmt.Fprintln(p.out, text) }

func (p *RendererPuro) ProtocoloAlternativo(from, to string) {
	fmt.Fprintf(p.out, "[protocol] %s endpoint unavailable; using %s\n", from, to)
}

func (p *RendererPuro) Tool(name string, dur time.Duration, failed bool) {
	status := "ok"
	if failed {
		status = "error"
	}
	fmt.Fprintf(p.out, "[tool %s %s in %s]\n", name, status, dur.Round(time.Millisecond))
}

func (p *RendererPuro) Eco(output string) {
	if output == "" {
		return
	}
	fmt.Fprintln(p.out, output)
}

func (p *RendererPuro) Cortada(limit int, gaveUp bool) {
	if gaveUp {
		fmt.Fprintf(p.out, "[output limit] response cut at %d tokens; giving up\n", limit)
		return
	}
	fmt.Fprintf(p.out, "[output limit] response cut at %d tokens; continuing\n", limit)
}

func (p *RendererPuro) Guardrail(proactive bool) {
	if proactive {
		fmt.Fprintln(p.out, guardrailProactive)
		return
	}
	fmt.Fprintln(p.out, guardrailReactive)
}

// Tokens is deliberately silent in plain mode: `-p` feeds recipes through a
// pipe and a status footer would pollute each one. The per-turn counter
// belongs to the interactive UI, where someone is watching the screen.
func (p *RendererPuro) Tokens(int, int, time.Duration, bool) {}

// Guardrail texts. They live here because plain mode prints them literally.
const (
	guardrailProactive = "[guardrail] proactive compaction (predicted overflow)"
	guardrailReactive  = "[guardrail] provider rejected the request size; reactive compaction"
)
