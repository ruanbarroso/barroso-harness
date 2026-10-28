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
		Description: "Compacts this conversation's history: everything BEFORE the current request is replaced by the " +
			"working state you write. Use it at a natural milestone of the task (e.g. you found the cause and are " +
			"about to edit; you finished exploring) — do not wait for the context to fill up. The state must be " +
			"enough to carry on: goal, decisions, discovered facts (paths, signatures, values), what has " +
			"already been changed and the next step. The full log remains retrievable through session_search.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"state": map[string]any{"type": "string", "description": "structured working state that replaces the earlier history"},
			},
			"required": []string{"state"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			state, err := readCompactState(raw)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("compaction scheduled: the history before this request will be replaced by the state (%d characters) at the end of this round", len(state)), nil
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
// (image note, loop notice) rather than written by the user. Steer messages
// are real user text and are not synthetic.
func isSynthetic(content string) bool {
	return content == readImagesNote || strings.HasPrefix(content, loopNoticePrefix)
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

// forcedState builds the state of a guardrail-forced compaction, used when
// pruning tool results cannot make the request fit: the latest summary (if
// any) plus the conversation's own words — user requests and assistant
// texts, newest kept first when over the cap. Tool calls and results are left
// out; session_search recovers them. Returns "" when there is nothing before
// the current request to compact.
func forcedState(h []dealer.Mensagem) string {
	cut := -1
	for i := len(h) - 1; i >= 0; i-- {
		if h[i].Role == "user" && !isSummary(h[i].Conteudo) {
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
