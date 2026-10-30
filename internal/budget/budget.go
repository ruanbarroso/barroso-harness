// Package budget computes the context-window guardrail and a calibratable
// chars/token estimator. Nothing here decides the semantic moment — that is
// runtime/agent policy; budget only says whether the next request physically fits.
package budget

import (
	"fmt"
	"math"
)

const (
	// ReservaSaida: room the model needs available to respond (the
	// max_tokens clamp varies per provider).
	ReservaSaida = 16 << 10
	// ReservaOperacional: estimated format overhead (tool schemas, message
	// structure, provider-invisible tokens).
	ReservaOperacional = 8 << 10

	// AlvoTrabalhoPadrao is the default working target of a turn, in tokens: the
	// point where compaction is ASKED of the model. It is deliberately far below
	// the model's window — measured on the fleet (Bruce, Greg, Zoe, 2026-10-29):
	// context rot degrades recall well before the window ends, and the requests
	// that reach the gateway with 500k-1M tokens buy a rotating prefix that costs
	// cache writes on every compaction.
	AlvoTrabalhoPadrao = 200_000
	// PercentualAlvoTrabalho is how much of the window the working target takes
	// when no manual target is set, capped by AlvoTrabalhoPadrao: a 1M-token
	// catalog window does not become a 1M-token working set, and a 200k window
	// keeps 20% free for the round in flight.
	PercentualAlvoTrabalho = 80
)

// AlvoTrabalho is the turn's working target, in tokens: the size at which the
// policy asks the model to compact rather than letting the turn grind up to the
// physical window. manual > 0 is honoured verbatim; otherwise it is the smaller
// of AlvoTrabalhoPadrao and PercentualAlvoTrabalho of the model's window. 0 (no
// known window and no manual target) means the policy only reacts to the
// provider's rejection — the guardrail stays the safety net.
func AlvoTrabalho(window, manual int) int {
	if manual < 0 {
		return 0 // explicitly off
	}
	if window <= 0 {
		// A known physical limit is preferable, but Zoe's config did not
		// publish one. Still keep a working target; provider rejection is the
		// physical safety net.
		if manual > 0 {
			return manual
		}
		return AlvoTrabalhoPadrao
	}
	physical := window - ReservaSaida - ReservaOperacional
	if physical <= 0 {
		return 0
	}
	ceiling := min(AlvoTrabalhoPadrao, window*PercentualAlvoTrabalho/100, physical)
	if manual > 0 {
		return min(manual, ceiling)
	}
	return ceiling
}

// Degraus of the working-target ladder, as fractions (in percent) of the
// target. Aviso: the model is told, once per cycle, to compact at its next
// milestone. Poda: old tool results are stubbed in one batch, down to
// DestinoPoda, so the cache prefix is rewritten once and then stays stable for
// many rounds. Exige: the model is REQUIRED to write the working state.
const (
	DegrauAviso = 65
	DegrauPoda  = 85
	DestinoPoda = 50
	DegrauExige = 100
)

// Percent returns pct percent of n.
func Percent(n, pct int) int { return n * pct / 100 }

// Window is the model's context window (tokens). 0 = unknown: with no
// published window the guardrail stays inactive and the policy only acts
// after a real provider error.
type Window struct {
	Tokens int
}

// Estimador estimates tokens ~ len/charsPerToken, calibratable per model from
// (bytes sent, reported prompt_tokens) pairs.
type Estimador struct {
	CharsPorToken float64
}

func (e Estimador) Tokens(text string) int {
	return int(float64(len(text)) / e.cpt())
}

// Bytes is the inverse of Tokens: how many bytes of text the estimator counts
// as n tokens. Rounded up, so freeing that much always covers n.
func (e Estimador) Bytes(n int) int {
	return int(math.Ceil(float64(n) * e.cpt()))
}

func (e Estimador) cpt() float64 {
	if e.CharsPorToken <= 0 {
		return 4 // conservative for Latin-script text
	}
	return e.CharsPorToken
}

// Calibrar takes a (bytes sent, actual tokens) pair and moves the factor
// toward the measured truth. Absurd samples are discarded.
func (e *Estimador) Calibrar(bytes int, actualTokens int) {
	if bytes <= 0 || actualTokens <= 0 {
		return
	}
	sample := float64(bytes) / float64(actualTokens)
	if sample < 1.5 || sample > 12 {
		return
	}
	if e.CharsPorToken <= 0 {
		e.CharsPorToken = sample
	} else {
		e.CharsPorToken = 0.7*e.CharsPorToken + 0.3*sample
	}
}

// Request is the forecast of the next physical request.
type Request struct {
	TokensEntrada int
	TokensSaida   int
	Janela        int // 0 = unknown
	Cabe          bool
}

func (r Request) Veredito() string {
	switch {
	case r.Janela == 0:
		return "guardrail inactive: model context window unknown"
	case r.Cabe:
		return fmt.Sprintf("fits: %d + %d reserved ≤ %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	default:
		return fmt.Sprintf("overflows: %d + %d reserved > %d", r.TokensEntrada, r.TokensSaida, r.Janela)
	}
}

// Avaliar decides (by estimate) whether the prompt fits in the window.
func Avaliar(prompt string, window int, est Estimador) Request {
	e := est.Tokens(prompt)
	if e < ReservaOperacional {
		e = ReservaOperacional
	}
	r := Request{TokensEntrada: e, TokensSaida: ReservaSaida, Janela: window}
	if window == 0 {
		r.Cabe = true
		return r
	}
	r.Cabe = e+r.TokensSaida <= window
	return r
}
