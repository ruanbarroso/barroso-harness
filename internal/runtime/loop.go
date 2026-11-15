package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// blockedNotice is what a blocked round's tool_result carries in place of its
// output. It does not repeat the steering notice: the model already got that
// as a user message right after the round. Here the point is to name the lever
// — this round was refused, act on what you have.
const blockedNotice = "[loop guard] This round was refused: its result would be identical to the previous ones. Use what you already have, try a different approach or arguments, or answer the user with what you found and what is blocking you."

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
		return g.note(loopVerdictStop, calls, results), loopVerdictStop
	case g.count >= block:
		return g.note(loopVerdictBlock, calls, results), loopVerdictBlock
	case g.count >= warn:
		return g.note(loopVerdictWarn, calls, results), loopVerdictWarn
	}
	return "", loopContinue
}

// note builds the steering notice appended as a user message right after the
// round's real results.
func (g *loopGuard) note(verdict int, calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	if verdict == loopContinue {
		return ""
	}
	toolList := strings.Join(names(calls), ", ")
	return fmt.Sprintf(loopNoticePrefix+"You have made the same call(s) (%s) %d rounds in a row and the tool results are identical "+
		"(same arguments or different arguments, same output). Repeating them will not change the outcome. "+
		"The results are real and came from the runtime — do not paste them from memory. %s"+
		"Use what you already have, try a different approach or arguments, "+
		"or answer the user with what you found and what is blocking you.",
		toolList, g.count, g.steering(calls, results))
}

// blockedResults is the content of a blocked round's tool_results: the round's
// REAL output with the refusal appended. The output is kept on purpose: it is
// often the one thing that would unblock the model. Conversation 129051 (Bruce,
// 2026-11-15): eight `listar_conversas` calls answered "há 2 contas; diga qual
// em `conta`", and replacing that text with a generic refusal hid exactly the
// instruction that fixes the call.
func (g *loopGuard) blockedResults(verdict int, calls []dealer.ToolChamada, results []dealer.Mensagem) []dealer.Mensagem {
	if verdict < loopVerdictBlock {
		return results
	}
	out := make([]dealer.Mensagem, len(results))
	copy(out, results)
	byID := make(map[string]string, len(results))
	for _, r := range results {
		byID[r.ToolID] = r.Conteudo
	}
	for i, c := range calls {
		if i >= len(out) {
			break
		}
		out[i].Conteudo = byID[c.ID] + "\n\n" + blockedNotice
		out[i].IsErro = true
	}
	return out
}

// steering names the field the tool's error says it needs, so the model can fix
// the call instead of repeating it. Conversation 129051, 2026-11-15: the
// `listar_conversas` error said "diga qual em `conta`" for eight rounds and the
// model never supplied it. Returns "" when the error does not name a field the
// call is missing — a guessed field would be worse than silence.
func (g *loopGuard) steering(calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	field := errorFieldName(calls, results)
	if field == "" {
		return ""
	}
	return fmt.Sprintf("The error above says what is missing: the argument %q is absent from this call. "+
		"Call the tool again with it filled in.", field)
}

// errorFieldName takes the first `campo` the round's error mentions and checks
// that the call really is missing it. Only the wpp connector phrases it this
// way ("diga qual em `conta`"), so nothing else is matched.
func errorFieldName(calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	byID := make(map[string]string, len(results))
	for _, r := range results {
		byID[r.ToolID] = r.Conteudo
	}
	for _, c := range calls {
		err := byID[c.ID]
		if !strings.HasPrefix(err, "error: ") {
			continue
		}
		field := fieldBackticked(err)
		if field == "" {
			continue
		}
		if args, ok := callArguments(c); ok {
			if _, present := args[field]; !present {
				return field
			}
		}
	}
	return ""
}

// callArguments unwraps a deferred tool_call into the arguments object the
// called tool received.
func callArguments(c dealer.ToolChamada) (map[string]any, bool) {
	raw := json.RawMessage(c.Args)
	var outer struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if c.Nome == toolCallName && json.Unmarshal(raw, &outer) == nil && len(outer.Arguments) > 0 {
		args := make(map[string]any, len(outer.Arguments))
		for k := range outer.Arguments {
			args[k] = nil
		}
		return args, true
	}
	var args map[string]any
	if json.Unmarshal(raw, &args) != nil || args == nil {
		return nil, false
	}
	return args, true
}

// fieldBackticked takes the first `campo` the tool's error mentions.
func fieldBackticked(text string) string {
	i := strings.Index(text, "`")
	if i < 0 {
		return ""
	}
	rest := text[i+1:]
	j := strings.Index(rest, "`")
	if j < 0 {
		return ""
	}
	field := rest[:j]
	if field == "" || len(field) > 64 || strings.ContainsAny(field, " \t\n\"'") {
		return ""
	}
	return field
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
