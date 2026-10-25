package runtime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// loopRepeats is how many consecutive rounds with the very same tool calls
// (names and arguments) count as a loop. Gemini CLI uses 5.
var loopRepeats = 5

// loopGuard notices a model that keeps issuing the same calls round after
// round — the "state blindness" measured in BH sessions — and produces a
// steering notice. It never stops the turn.
type loopGuard struct {
	last  string
	count int
}

// observe takes one round's calls and returns a notice when they repeat the
// previous rounds loopRepeats times (then again every loopRepeats).
func (g *loopGuard) observe(calls []dealer.ToolChamada) string {
	sig := signature(calls)
	if sig == "" || sig != g.last {
		g.last, g.count = sig, 1
		return ""
	}
	g.count++
	if g.count%loopRepeats != 0 {
		return ""
	}
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Nome)
	}
	return fmt.Sprintf(loopNoticePrefix+"You have made exactly the same call(s) (%s, same arguments) %d rounds in a row and got the same results. "+
		"Repeating it will not change the outcome. Use what you already have, try a different approach or arguments, "+
		"or answer the user with what you found and what is blocking you.", strings.Join(names, ", "), g.count)
}

// signature identifies a round's calls regardless of their order.
func signature(calls []dealer.ToolChamada) string {
	if len(calls) == 0 {
		return ""
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Nome+"\x00"+strings.TrimSpace(c.Args))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}
