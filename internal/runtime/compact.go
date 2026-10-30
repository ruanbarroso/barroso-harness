package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Context policies (config.json, field context_policy).
//
//   - PoliticaAgente (default): the model has context_compact and decides WHEN to
//     compact, at a task milestone, by writing the working state. The budget
//     guardrail stays on as a safety net.
//   - PoliticaLimiar: no context_compact; only the guardrail compacts, on overflow.
//
// Both record every compaction in the log (event "compactacao" or
// "guardrail", both with the politica field), so they can be compared under
// the same harness against the real sessions in sessions.db.
//
// The constant values are what gets persisted in sessions.db and read back by
// other packages, so they keep their original form; config.json also accepts
// the English aliases "agent" and "threshold".
const (
	PoliticaAgente = "agente"
	PoliticaLimiar = "limiar"
)

// policyThresholdAlias is the English spelling of PoliticaLimiar accepted in
// config.json ("agent" needs no alias: anything not threshold is the default).
const policyThresholdAlias = "threshold"

// policy normalizes the config value; unknown values fall back to the default.
func policy(v string) string {
	v = strings.TrimSpace(v)
	if strings.EqualFold(v, PoliticaLimiar) || strings.EqualFold(v, policyThresholdAlias) {
		return PoliticaLimiar
	}
	return PoliticaAgente
}

// Event kinds of an agent compaction. eventCompaction is what is WRITTEN (kept
// in its original form because sessions.db readers outside this package match
// on it); eventCompactionEN is also accepted on read.
const (
	eventCompaction   = "compactacao"
	eventCompactionEN = "compaction"
	eventContextPrune = "context_prune"
)

const compactToolName = "context_compact"

// maxSummary caps the state written by the model: a summary larger than this
// is no longer a compaction.
const maxSummary = 16 << 10

// compactSpec is the agent's compaction action. Exec only validates; the turn
// loop swaps the history, after the round's results are in (the history
// cannot change in the middle of a tool round).
func compactSpec() tools.Spec {
	return tools.Spec{
		Name: compactToolName,
		Description: "Compacts this conversation: the earlier history AND the older tool rounds of the current turn are replaced by the " +
			"working state you write. Kept verbatim: the current request and your newest tool rounds. The harness appends by itself " +
			"the user's requests and the list of files read/changed, so do not repeat those. Call it at every milestone — root cause " +
			"found, change made and verified, exploration finished, a sub-goal closed — BEFORE starting the next step, and whenever a " +
			"[context] notice asks. Do not call it halfway through a step. The state is ALL you will remember of what it replaces, so " +
			"write it as a handoff to yourself, with these sections: " +
			"Goal (what the user wants, in their terms); " +
			"Constraints (the user's rules, preferences and corrections, verbatim when short); " +
			"Decisions (what was decided and WHY, including approaches rejected); " +
			"Facts (paths with line numbers, signatures, commands, ids, values, exact error messages — whatever you would otherwise re-read); " +
			"Done (what changed and how it was verified); " +
			"Open (failing tests, unresolved errors, things to check); " +
			"Next (the exact next action). " +
			"Dense and concrete beats long: no narration, no pleasantries. Anything you omit stays recoverable through session_search / session_retrieve.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{"type": "string", "description": "handoff state with these sections: Goal; Constraints (the user's rules, verbatim when short); Decisions and why; Facts (paths with line numbers, signatures, commands, ids, values, exact error messages); Done and how it was verified; Open issues; the exact Next action. Dense and concrete, no narration."},
			},
			"required": []string{"state"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			state, err := readCompactState(raw)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("compaction accepted: at the end of this round the earlier history and your older tool rounds are replaced by your state (%d characters); the current request and your newest rounds stay. Continue with the next step.", len(state)), nil
		},
	}
}

// readCompactState parses context_compact arguments. The legacy "estado" key is
// still accepted, for calls from models primed with the old schema.
func readCompactState(raw json.RawMessage) (string, error) {
	var c struct {
		State  string `json:"state"`
		Estado string `json:"estado"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", fmt.Errorf("invalid input: %w", err)
	}
	e := strings.TrimSpace(c.State)
	if e == "" {
		e = strings.TrimSpace(c.Estado)
	}
	if e == "" {
		return "", fmt.Errorf("empty state: write the working state that replaces the history")
	}
	if len(e) > maxSummary {
		return "", fmt.Errorf("state of %d bytes exceeds the limit of %d: summarize further", len(e), maxSummary)
	}
	return e, nil
}

// summaryPrefix marks the synthetic message; Resume and later compactions
// recognize it by this prefix.
const summaryPrefix = "[working state — written by the agent itself when compacting the earlier history]\n"

// legacySummaryPrefix is the marker of sessions recorded before the English
// translation; it is still recognized.
const legacySummaryPrefix = "[estado de trabalho"

// isSummary reports whether a message content is a compaction summary (new or
// legacy marker).
func isSummary(content string) bool {
	return strings.HasPrefix(content, summaryPrefix) || strings.HasPrefix(content, legacySummaryPrefix)
}

// readImagesNote is the content of the user message the runtime adds after a
// round whose read calls opened images (tool messages cannot carry images).
const readImagesNote = "[images opened with read]"

// loopNoticePrefix starts the steering notice of the loop guard.
const loopNoticePrefix = "[loop] "

// isSynthetic reports whether a user-role message was injected by the runtime
// (image note, loop notice, turn-reduction note) rather than written by the
// user. Steer messages are real user text and are not synthetic.
func isSynthetic(content string) bool {
	return content == readImagesNote ||
		strings.HasPrefix(content, loopNoticePrefix) ||
		strings.HasPrefix(content, contextNotePrefix) ||
		strings.HasPrefix(content, UpdatePrefix) ||
		strings.HasPrefix(content, turnDropNotePrefix)
}

// compactHistory replaces everything before the CURRENT request (the last real
// user message) with a message carrying the state. The current request and
// the ongoing round — with its tool_call/tool_result pairs — stay intact, so
// the request remains valid for the provider. Summaries and the messages the
// runtime injects with role "user" (isSynthetic) are not requests: cutting at
// them threw the user's actual request away with the old history.
//
// With nothing before the current request there is nothing to compact and h
// is returned unchanged.
func compactHistory(h []dealer.Mensagem, state string) []dealer.Mensagem {
	cut := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" && !isSummary(h[i].Conteudo) && !isSynthetic(h[i].Conteudo) {
			cut = i
			break
		}
	}
	if cut <= 0 {
		return h
	}
	out := make([]dealer.Mensagem, 0, len(h)-cut+1)
	out = append(out, dealer.Mensagem{Role: "user", Conteudo: summaryPrefix + state})
	return append(out, h[cut:]...)
}

// maxForcedState caps the mechanical state written by the guardrail.
const maxForcedState = 24 << 10

// turnDropNote prefixes the synthetic user message a turn reduction injects
// in place of the dropped rounds. A LATER reduction replaces the old note
// instead of stacking, and isSynthetic must recognize it so compaction cuts
// never take it for the user's request.
const turnDropNotePrefix = "[earlier tool rounds of this turn are dropped"

const turnDropNote = turnDropNotePrefix + " by the guardrail to keep the request in the context window; the working state above summarizes them, and session_search can recover any detail]\n\n"

// forcedState builds the state of a guardrail-forced compaction, used when
// pruning tool results cannot make the request fit: the latest summary (if
// any) plus the conversation's own words — user requests and assistant
// texts, newest kept first when over the cap. Tool calls and results are left
// out; session_search recovers them. Returns "" when there is nothing before
// the current request to compact.
func forcedState(h []dealer.Mensagem) string {
	cut := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" && !isSummary(h[i].Conteudo) && !isSynthetic(h[i].Conteudo) {
			cut = i
			break
		}
	}
	if cut <= 0 {
		return ""
	}
	prev := ""
	var lines []string
	for _, m := range h[:cut] {
		c := strings.TrimSpace(m.Conteudo)
		switch {
		case m.Role == "user" && isSummary(m.Conteudo):
			prev = strings.TrimSpace(strings.TrimPrefix(m.Conteudo, summaryPrefix))
			lines = nil // the summary already covers what came before it
		case m.Role == "user" && c != "":
			lines = append(lines, "- user: "+clip(c, 1500))
		case m.Role == "assistant" && c != "":
			lines = append(lines, "- assistant: "+clip(c, 1500))
		}
	}
	head := "The earlier history no longer fit the context window, so the guardrail replaced it with this " +
		"mechanical state (tool calls and results omitted; use session_search / session_retrieve to recover them).\n"
	if prev != "" {
		head += "\n## Previous working state\n" + clip(prev, maxForcedState/2) + "\n"
	}
	const section = "\n## Conversation since then (oldest omitted if too long)\n"
	room := maxForcedState - len(head) - len(section) - 1
	var kept []string
	for i := len(lines) - 1; i >= 0; i-- {
		if room-len(lines[i])-1 < 0 {
			break
		}
		room -= len(lines[i]) + 1
		kept = append([]string{lines[i]}, kept...)
	}
	if len(kept) > 0 {
		head += section + strings.Join(kept, "\n") + "\n"
	}
	return head
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func historyBytes(h []dealer.Mensagem) int {
	n := 0
	for _, m := range h {
		n += len(m.Conteudo) + len(m.HookContext) + len(m.ToolArgs)
		for _, item := range m.ResponsesOutput {
			n += len(item)
		}
	}
	return n
}

// reduceTurn replaces the turn's OLDER tool rounds with a mechanical note,
// dropping whole units oldest first until the freed bytes cover wantBytes (or
// only the newest unit remains). It is the guardrail's last mechanical lever:
// prune stubs results, forced compaction replaces the history before the
// request, but a LONG TURN can still hold the overflow inside its own rounds
// (conversation 104129, session bh-20261028013103-4583, 2026-10-28: 229 tool
// rounds kept ~612 KiB alive after the prompt; the forced compaction cut
// before the prompt and freed nothing, and prune had the tail results only).
//
// A unit is whole (assistant text + its tool calls + all their results), so
// the request stays valid for the provider and for a stateless Responses
// replay. The weight is counted the way historyBytes does. A previous
// reduction note is replaced, never stacked. Returns nil when there is
// nothing to drop (no units beyond the newest, or no real user request to
// anchor the turn).
func reduceTurn(h []dealer.Mensagem, wantBytes int) []dealer.Mensagem {
	if wantBytes <= 0 {
		return nil
	}
	cut := -1 // the turn's own request: the last real user message
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" && !isSummary(h[i].Conteudo) && !isSynthetic(h[i].Conteudo) {
			cut = i
			break
		}
	}
	units := turnUnits(h, cut+1)
	if len(units) <= 1 {
		return nil
	}
	freed := 0
	drop := 0
	for drop = 0; drop < len(units)-1; drop++ {
		freed += historyBytes(h[units[drop].start:units[drop].end])
		if freed >= wantBytes {
			drop++
			break
		}
	}
	if drop == 0 || freed < wantBytes {
		return nil
	}
	out := make([]dealer.Mensagem, 0, len(h)-(units[drop-1].end-units[0].start)+1)
	for _, m := range h[:units[0].start] {
		if strings.HasPrefix(m.Conteudo, turnDropNotePrefix) {
			continue // the fresh note below replaces an older one
		}
		out = append(out, m)
	}
	out = append(out, dealer.Mensagem{Role: "user", Conteudo: turnDropNote})
	return append(out, h[units[drop].start:]...)
}

// turnUnit is the span [start, end) of ONE tool round unit in the history:
// an assistant text and/or a batch of tool calls together with all their
// results. The span OPENS at the gap of user messages that precedes the
// unit's first message (steers, notes, updates): runtime-injected notes ride
// with the unit that follows them, so keeping a unit keeps instructions the
// user sent for it, and dropping one drops its own stale notices.
type turnUnit struct {
	start, end int
}

// turnUnits segments the history after `from` into whole tool-round units.
// It relies on the runtime being the only writer: a tool round is appended as
// [assistant text (optional), tool_call×N, tool_result×N] back to back, so a
// unit's END is its call/result pairing, never the next user message.
// Call-less results cannot exist: the runtime commits a batch and its
// results together (and Resume repairs orphan calls before the transcript
// is used).
func turnUnits(h []dealer.Mensagem, from int) []turnUnit {
	var units []turnUnit
	i := from
	for i < len(h) {
		start := i // the span opens at the pending gap of user messages
		for i < len(h) && h[i].Role == "user" {
			i++
		}
		if i >= len(h) {
			break
		}
		if h[i].Role == "assistant" {
			i++ // the round's intermediate text heads its batch
		}
		var calls int
		for i < len(h) && h[i].Role == "tool_call" {
			calls++
			i++
		}
		if calls > 0 {
			var results int
			for i < len(h) && h[i].Role == "tool_result" && results < calls {
				results++
				i++
			}
			// Repairs of Resume (closeOrphanToolCalls) may append extra
			// results for interrupted calls right after the pairing block;
			// they belong to this unit, so no call is ever half-dropped.
			for i < len(h) && h[i].Role == "tool_result" {
				i++
			}
		}
		if i > start {
			units = append(units, turnUnit{start: start, end: i})
		}
	}
	return units
}

// compactedState returns the state of the round's last SUCCESSFUL
// context_compact, or "" when there was none. A failed call (empty state, over the
// limit) compacts nothing: the model got the error and may try again.
func compactedState(calls []dealer.ToolChamada, results []dealer.Mensagem) string {
	state := ""
	for i, c := range calls {
		if c.Nome != compactToolName || i >= len(results) || results[i].IsErro {
			continue
		}
		if e, err := readCompactState(json.RawMessage(c.Args)); err == nil {
			state = e
		}
	}
	return state
}
