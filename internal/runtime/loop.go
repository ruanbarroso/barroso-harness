package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// The ladder. A round that keeps producing the SAME outcomes is warned at
// loopWarn, blocked at loopBlock and, if the model still insists, the turn
// stops at loopStop. Each step counts consecutive rounds; a different outcome
// resets the streak.
var (
	loopWarn  = 3
	loopBlock = 5
	loopStop  = 8
)

// Verdicts returned by observe.
const (
	loopContinue = iota
	loopVerdictWarn
	loopVerdictBlock
	loopVerdictStop
)

// loopGuard notices a model that keeps issuing calls that produce the same
// outcomes round after round — the "state blindness" measured in BH sessions
// (Bruce ran 154 shell calls in 18 minutes whose only difference was `tail -4`
// vs `tail -n 4`; identical output, so the old arguments-only signature never
// fired). It warns, then blocks the repeated calls, then stops the turn.
type loopGuard struct {
	lastOut string
	count   int
	// cyclesSinceCompact tightens the ladder right after a compaction, when
	// the model is most likely to re-read the same files.
	cyclesSinceCompact int
}

// blockedError is the content of the synthetic tool_result that replaces a
// blocked round's outputs. It must read as a real result (the transcript is
// what the model sees), and it has to say why the call was refused.
func blockedError(notice string) string {
	return "[loop guard] " + notice
}

// note returns the steering notice for the current streak, if any.
func (g *loopGuard) note(verdict int, names []string) string {
	if verdict == loopContinue {
		return ""
	}
	toolList := strings.Join(names, ", ")
	return fmt.Sprintf(loopNoticePrefix+"You have made the same call(s) (%s) %d rounds in a row and the tool results are identical "+
		"(same arguments or different arguments, same output). Repeating them will not change the outcome. "+
		"The results are real and came from the runtime — do not paste them from memory. "+
		"Use what you already have, try a different approach or arguments, "+
		"or answer the user with what you found and what is blocking you.", toolList, g.count)
}

// observe takes one round's calls and their results and returns (notice,
// verdict). results are aligned with calls by ToolID. A round with no calls is
// not a loop. The verdict ladder is tightened by one step while the guard is
// inside the post-compaction window.
func (g *loopGuard) observe(calls []dealer.ToolChamada, results []dealer.Mensagem) (string, int) {
	if len(calls) == 0 {
		return "", loopContinue
	}
	// Streak key: tool names plus output hashes, NOT arguments. Differently
	// spelled arguments that produce the same output are the same loop.
	out := outputs(calls, results)
	if out != g.lastOut {
		g.lastOut, g.count = out, 1
		return "", loopContinue
	}
	g.count++
	warn, block, stop := g.thresholds()
	switch {
	case g.count >= stop:
		return g.note(loopVerdictStop, names(calls)), loopVerdictStop
	case g.count >= block:
		return g.note(loopVerdictBlock, names(calls)), loopVerdictBlock
	case g.count >= warn:
		return g.note(loopVerdictWarn, names(calls)), loopVerdictWarn
	}
	return "", loopContinue
}

func (g *loopGuard) thresholds() (warn, block, stop int) {
	if g.cyclesSinceCompact > 0 {
		return loopWarn - 1, loopBlock - 1, loopStop - 1
	}
	return loopWarn, loopBlock, loopStop
}

// Compacted marks a compaction: the next cycles run on a tighter ladder.
func (g *loopGuard) Compacted() { g.cyclesSinceCompact = 2 }

// tick ages the post-compaction window one cycle.
func (g *loopGuard) tick() {
	if g.cyclesSinceCompact > 0 {
		g.cyclesSinceCompact--
	}
}

// Reset breaks the streak (a new user message, a steer, a compaction).
func (g *loopGuard) Reset() {
	g.lastOut, g.count = "", 0
}

// outputs hashes the results of the round, keyed by tool name so a call that
// moves position in the round does not look like a different outcome.
func outputs(calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	byID := make(map[string]string, len(results))
	for _, r := range results {
		byID[r.ToolID] = r.Conteudo
	}
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		h := sha256.Sum256([]byte(byID[c.ID]))
		parts = append(parts, c.Nome+"\x00"+hex.EncodeToString(h[:8]))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x01")
}

func names(calls []dealer.ToolChamada) []string {
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		names = append(names, c.Nome)
	}
	return names
}
