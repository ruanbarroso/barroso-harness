package runtime

import (
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// Size limits for tool results. The built-in tools already cap their own
// output at 64 KiB; MCP tools did not, and one of them (wpp baixar_arquivo,
// conversation 129062, 2026-11-15) returned a 680 KB image as base64 text:
// ~900 KB, 240k tokens, in a 200k window. The result was the newest message,
// which prune and reduceTurn never touch, so the turn died with "request does
// not fit". Every result is now capped as it enters the history, whatever
// tool produced it.
const (
	maxToolResult  = 96 << 10 // cap of one tool result, markers included (built-ins stop at 64 KiB + a marker)
	lastResortKeep = 8 << 10  // what shrinkResults leaves of an oversized result
)

// base64Run matches a long unbroken run of base64 characters: binary data
// that a tool returned as text. Prose, code and JSON keys never reach 1 KiB
// without a space, quote or punctuation.
var base64Run = regexp.MustCompile(`[A-Za-z0-9+/_-]{1000,}={0,2}`) // Go's regexp caps repeat counts at 1000

// capToolResult bounds a tool result to maxToolResult bytes. Runs of base64
// are replaced by a one-line note first (the bytes are useless as text and
// the surrounding JSON stays readable); what still exceeds the cap keeps its
// start and its end. Results within the cap come back untouched, so the
// function is idempotent.
func capToolResult(s string) string {
	if len(s) <= maxToolResult {
		return s
	}
	orig := len(s)
	s = base64Run.ReplaceAllStringFunc(s, func(run string) string {
		return fmt.Sprintf("[base64 payload of %d bytes omitted: it is binary data, not readable as text; have the tool save it to a file instead of returning it]", len(run))
	})
	return cutMiddle(s, maxToolResult, orig)
}

// cutMiddle keeps the start and the end of s within limit bytes (marker
// included), never splitting a UTF-8 rune. orig is the size of the result
// before any cut, for the marker.
func cutMiddle(s string, limit, orig int) string {
	if len(s) <= limit {
		return s
	}
	marker := fmt.Sprintf("\n\n[... tool result of %d bytes cut to fit the context window; the middle was omitted. Ask the tool for a smaller slice if you need it ...]\n\n", orig)
	room := limit - len(marker)
	if room < 2 {
		return trimPartialRune(s[:max(limit, 0)])
	}
	head := room * 3 / 4
	tail := room - head
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return trimPartialRune(s[:head]) + marker + s[start:]
}

// shrinkResults is the guardrail's last lever when one fresh result alone
// overflows the window and nothing older can be pruned or dropped (prune
// spares the newest result, reduceTurn keeps the newest unit). It cuts the
// newest oversized tool results, newest first, down to lastResortKeep until
// wantBytes are freed. Returns nil when that cannot free enough. In-memory
// only: Resume applies capToolResult to what the log holds.
func shrinkResults(h []dealer.Mensagem, wantBytes int) []dealer.Mensagem {
	if wantBytes <= 0 {
		return nil
	}
	out := append([]dealer.Mensagem(nil), h...)
	freed := 0
	for i := len(out) - 1; i >= 0 && freed < wantBytes; i-- {
		if out[i].Role != "tool_result" || len(out[i].Conteudo) <= lastResortKeep {
			continue
		}
		before := len(out[i].Conteudo)
		out[i].Conteudo = cutMiddle(out[i].Conteudo, lastResortKeep, before)
		freed += before - len(out[i].Conteudo)
	}
	if freed < wantBytes {
		return nil
	}
	return out
}
