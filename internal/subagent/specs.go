// Specs (tools) that the subagent package exposes to the PARENT agent — and
// only to it. See the package doc: runtime.Session.Send builds each turn's tool
// list with tools.SpecsBuiltins(), so a child session NEVER receives these
// specs; there is no wiring to do and no filter to apply.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// TetoResultado caps the text returned by <prefix>_result: 16 KiB. A
// sub-agent may produce a lot; the tool truncates with a marker and the parent
// asks for the rest some other way (or refines the request).
const TetoResultado = 16 << 10

// Especificacoes returns the model's delegation tools: the launcher
// <prefix> {prompts[]}, which launches the sub-agents and returns immediately,
// and <prefix>_status, a non-blocking snapshot so the parent can answer "how
// are they doing?". There is no <prefix>_result: each sub-agent's result
// arrives on its own (AvisoTerminadas). With a result tool at hand the model
// kept polling in a loop, so status is rate-limited (see statusSpec) and tells
// the model to end its turn instead. resultSpec remains for the lifecycle tests.
//
// parentCfg is the parent's configuration, inherited by the tasks (same
// provider, model and key). Spawn does not get a long-lived ctx: the tool
// caller passes the turn ctx and, for tasks that must outlive it, the wiring
// calls Vincular (see subagent.go) with a long-lived context, and then a nil
// ctx inherits that one.
func Especificacoes(g *Gerente, parentCfg config.Config) []tools.Spec {
	if g == nil {
		return nil
	}
	g.ConfigPai(parentCfg)
	p := g.Prefixo()
	return []tools.Spec{spawnSpec(g, p), statusSpec(g, p)}
}

// ── spawn: the manager's launcher tool; its name IS the prefix ────────────
// (opencode convention: the tool exposed to the parent is a single lowercase
// name, "subagent" by default; status/result get the prefix as a family).

func spawnSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix,
		Description: "Call it ONCE per turn, with all requests in prompts[]. Launches one or SEVERAL sub-agents at once, in the BACKGROUND, and returns their ids immediately. " +
			"Each sub-agent runs autonomously with the built-in tools (terminal, files), but CANNOT " +
			"create other sub-agents. There is no limit on count or concurrency: they all start together. " +
			"You cannot wait for them: when each sub-agent finishes, fails or dies, its result arrives " +
			"on its own as a new message, in a new turn. A second call in the same turn is REJECTED with an error. " +
			"Never launch tasks to pass the time or to wait. After launching, only do work that does not " +
			"depend on the results, or END THE TURN telling the user what you launched and that you are waiting. " +
			"" + prefix + "_status only reports progress when the user asks; it is not a way to wait.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompts": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "one complete, self-contained instruction per sub-agent; N items = N sub-agents in parallel",
				},
			},
			"required": []string{"prompts"},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Prompt  string   `json:"prompt"`
				Prompts []string `json:"prompts"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			var requests []string
			if strings.TrimSpace(c.Prompt) != "" {
				requests = append(requests, c.Prompt)
			}
			for _, p := range c.Prompts {
				if strings.TrimSpace(p) != "" {
					requests = append(requests, p)
				}
			}
			if len(requests) == 0 {
				return "", fmt.Errorf("provide prompts (or prompt)")
			}
			// One launch per turn. With no tool to wait, the model once
			// launched "dummy" sub-agents just to pass the time; a second call
			// in the same turn is rejected and told to end the turn.
			if !g.firstSpawnOfTurn(ctx) {
				return "", fmt.Errorf("%s", "sub-agents were already launched this turn; put them all in a single "+
					prefix+" call with prompts[]. Do not launch tasks to wait: END THE TURN now "+
					"telling the user — the results arrive on their own in a new turn")
			}
			var ids []string
			for _, p := range requests {
				// nil ctx: the task lives in the manager's long-lived context,
				// not the turn's — the turn may end and the sub-agent keeps going.
				id, err := g.Spawn(nil, p)
				if err != nil {
					return "", err
				}
				ids = append(ids, id)
			}
			return fmt.Sprintf("%d sub-agent(s) launched: %s. Each result arrives on its own as a new message "+
				"when it finishes. Do not spawn again this turn: only do work that does not depend on the "+
				"results, or end the turn now telling the user what you launched.",
				len(ids), strings.Join(ids, ", ")), nil
		},
	}
}

// ── <prefix>_status ────────────────────────────────────────────────────────

// StatusIntervaloMinimo is the shortest time between two FULL status reports,
// counted from the last launch or the last full report. Inside the window the
// tool answers with one line per task and an order to end the turn, so a model
// that polls gets nothing new to chew on and no reason to loop. Never blocks.
const StatusIntervaloMinimo = 60 * time.Second

func statusSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix + "_status",
		Description: "Non-blocking snapshot of the sub-agents: id, state (running/completed/failed/cancelled), age " +
			"and what each one is doing now. Use it ONLY when the user asks how they are going. It is NOT a way to " +
			"wait: results, failures and crashes arrive on their own as a new message. Calling it again within " +
			"60 s returns only a short state list.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			return g.statusReport(), nil
		},
	}
}

// statusReport builds the status text and applies the anti-polling guard.
func (g *Gerente) statusReport() string {
	fotos := g.Fotos()
	if len(fotos) == 0 {
		return "no sub-agents yet"
	}
	now := g.clock()
	active := 0
	var newest time.Time
	for i := range fotos {
		if fotos[i].TerminadaEm.IsZero() {
			active++
		}
		if fotos[i].Criada.After(newest) {
			newest = fotos[i].Criada
		}
	}
	g.mu.Lock()
	ref := g.lastStatus
	if newest.After(ref) {
		ref = newest
	}
	short := active > 0 && now.Sub(ref) < StatusIntervaloMinimo
	if !short {
		g.lastStatus = now
	}
	g.mu.Unlock()

	var sb strings.Builder
	for i := range fotos {
		t := &fotos[i]
		fmt.Fprintf(&sb, "%s %s %s", t.ID, t.Estado, ageText(now, t))
		if !short {
			fmt.Fprintf(&sb, " | %s", lastActivity(t))
		}
		sb.WriteString("\n")
	}
	switch {
	case active == 0:
		sb.WriteString("\nNone is running. Results that were not delivered yet arrive as a message.\n")
	case short:
		sb.WriteString("\nStatus was just checked. Do NOT call it again: END THE TURN now. The result of each " +
			"sub-agent (success, error or crash) arrives on its own as a new message.\n")
	default:
		fmt.Fprintf(&sb, "\n%d still running. Tell the user and END THE TURN: each result (success, error or crash) "+
			"arrives on its own as a new message. Do not poll.\n", active)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// lastActivity is a one-line description of what a task did last.
func lastActivity(t *Tarefa) string {
	if t.TerminadaEm.IsZero() {
		ev := t.conversa
		for i := len(ev) - 1; i >= 0; i-- {
			e := ev[i]
			switch e.Tipo {
			case EventoToolCall:
				state := "running"
				if !e.Executando {
					state = "done"
					if e.Erro {
						state = "failed"
					}
				}
				return fmt.Sprintf("tool %s (%s); %d tool calls so far", e.Nome, state, len(t.transcript))
			case EventoResposta, EventoNota, EventoPensamento:
				return e.Tipo + ": " + excerpt(e.Texto, 160)
			}
		}
		return "starting"
	}
	switch t.Estado {
	case Completed:
		if strings.TrimSpace(t.Resultado) == "" {
			return "finished with no final text"
		}
		return "finished: " + excerpt(t.Resultado, 160)
	default:
		return "error: " + orEmpty(t.Erro)
	}
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// ageText summarizes the relevant time: for finished tasks, from creation to
// the end; for pending ones, from creation until now.
func ageText(now time.Time, t *Tarefa) string {
	ref := t.TerminadaEm
	if ref.IsZero() {
		ref = now
	}
	d := ref.Sub(t.Criada).Round(time.Second)
	if d < 0 {
		d = 0
	}
	return d.String()
}

// ── <prefix>_result ────────────────────────────────────────────────────────

func resultSpec(g *Gerente, prefix string) tools.Spec {
	return tools.Spec{
		Name: prefix + "_result",
		Description: "Returns the FINAL result of a sibling task. completed: the sub-agent's text " +
			"(truncated at 16 KiB); failed: one line with the error; queued/running/cancelled: one line with the state.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "task id, as returned by " + prefix + "_status"},
			},
			"required": []string{"id"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(raw, &c)
			id := strings.TrimSpace(c.ID)
			if id == "" {
				return "", fmt.Errorf("id is required")
			}
			cp, ok := g.snapshot(id)
			if !ok {
				return "", fmt.Errorf("task %s not found", id)
			}
			g.markDelivered(id)
			switch cp.Estado {
			case Completed:
				if strings.TrimSpace(cp.Resultado) == "" {
					return fmt.Sprintf("task %s completed with no final text", id), nil
				}
				return truncate(cp.Resultado, TetoResultado, id), nil
			case Failed:
				return fmt.Sprintf("task %s failed: %s", id, orEmpty(cp.Erro)), nil
			case Cancelled:
				return fmt.Sprintf("task %s cancelled%s", id, suffix(cp.Erro)), nil
			case Queued:
				return fmt.Sprintf("task %s starting", id), nil
			case Running:
				return fmt.Sprintf("task %s running", id), nil
			default:
				return fmt.Sprintf("task %s in state %q", id, cp.Estado), nil
			}
		},
	}
}

// truncate applies the cap with a marker at the end.
func truncate(s string, limit int, id string) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n\n[result of task %s truncated at %d bytes; ask for a more specific excerpt]", id, limit)
}

func orEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no detail)"
	}
	return s
}

func suffix(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	return ": " + s
}

// AvisoTerminadas builds the message the parent receives when sub-agents
// finish: one block per task with the result (or the error) and, at the end,
// how many are still running. The "[subagents]" prefix lets the screen and
// the session title recognize the message as a notice, not user speech.
func AvisoTerminadas(ts []Tarefa, active int) string {
	var sb strings.Builder
	sb.WriteString("[subagents] Sub-agents finished:\n")
	for i := range ts {
		t := &ts[i]
		dur := t.TerminadaEm.Sub(t.Criada).Round(time.Second)
		fmt.Fprintf(&sb, "\n## %s %s (%s)\n", t.ID, t.Estado, dur)
		switch t.Estado {
		case Completed:
			if strings.TrimSpace(t.Resultado) == "" {
				sb.WriteString("(no final text)\n")
			} else {
				sb.WriteString(truncate(t.Resultado, TetoResultado, t.ID) + "\n")
			}
		default:
			sb.WriteString("error: " + orEmpty(t.Erro) + "\n")
		}
	}
	if active > 0 {
		fmt.Fprintf(&sb, "\n%d sub-agent(s) still running; their results will arrive the same way.\n", active)
	} else {
		sb.WriteString("\nNo sub-agents running now.\n")
	}
	return sb.String()
}
