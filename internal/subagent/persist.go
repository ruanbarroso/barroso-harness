package subagent

// Durable task lifecycle. The registry lives in memory, so a bh process that
// dies (kill, OOM, connector restart) used to take its running sub-agents with
// it and the parent never heard about them. The manager now records every
// lifecycle change in the session log (through the Persistir hook, so this
// package does not import the store), and Restaurar rebuilds from those
// records when the session is reopened: tasks that were still running become
// failed ("interrupted") and tasks whose result was never delivered are
// delivered, both through the usual Terminadas/AvisoTerminadas path.

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// EventoTarefa is the log kind of a task lifecycle record.
	EventoTarefa = "subagent_task"
	// EventoEntregue is the log kind recording that results reached the parent.
	EventoEntregue = "subagent_delivered"
	// MsgInterrompida is the error of a task whose process died.
	MsgInterrompida = "interrupted: the bh process ended before this sub-agent finished; it produced no result"
)

// Registro is one raw log event handed to Restaurar.
type Registro struct {
	Kind    string
	Payload string
}

// Persistir registers where lifecycle records go (nil = nowhere).
func (g *Gerente) Persistir(fn func(kind string, payload map[string]any)) {
	g.mu.Lock()
	g.persist = fn
	g.mu.Unlock()
}

// persistTask records the task's current state. The caller holds g.mu.
func (g *Gerente) persistTask(t *Tarefa, withPrompt bool) {
	if g.persist == nil {
		return
	}
	p := map[string]any{"id": t.ID, "state": t.Estado}
	if t.Erro != "" {
		p["error"] = t.Erro
	}
	if t.Resultado != "" {
		p["result"] = truncate(t.Resultado, TetoResultado, t.ID)
	}
	if withPrompt {
		p["prompt"] = limitConversationText(t.Prompt)
	}
	g.persist(EventoTarefa, p)
}

// persistDelivered records that these tasks reached the parent. The caller
// holds g.mu.
func (g *Gerente) persistDelivered(ids []string) {
	if g.persist == nil || len(ids) == 0 {
		return
	}
	g.persist(EventoEntregue, map[string]any{"ids": strings.Join(ids, ",")})
}

// fin finishes a task and records it. The caller holds g.mu.
func (g *Gerente) fin(t *Tarefa, state, errText string) {
	t.finish(state, errText)
	g.persistTask(t, false)
}

// Pendentes counts finished tasks the parent has not been told about yet.
func (g *Gerente) Pendentes() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, id := range g.order {
		if t := g.tasks[id]; t != nil && !t.TerminadaEm.IsZero() && !g.delivered[id] {
			n++
		}
	}
	return n
}

// Restaurar rebuilds the undelivered tasks from the session log, in order.
// Delivered tasks are only used to keep new ids from colliding with old ones.
// It returns how many tasks now wait for delivery.
func (g *Gerente) Restaurar(regs []Registro) int {
	type rec struct {
		id, prompt, state, errText, result string
		delivered                          bool
	}
	recs := map[string]*rec{}
	var order []string
	maxSeq := 0
	for _, r := range regs {
		var p struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Error  string `json:"error"`
			Result string `json:"result"`
			Prompt string `json:"prompt"`
			IDs    string `json:"ids"`
		}
		if json.Unmarshal([]byte(r.Payload), &p) != nil {
			continue
		}
		switch r.Kind {
		case EventoTarefa:
			if p.ID == "" {
				continue
			}
			c := recs[p.ID]
			if c == nil {
				c = &rec{id: p.ID}
				recs[p.ID] = c
				order = append(order, p.ID)
				var n int
				if _, err := fmt.Sscanf(p.ID, "t%d", &n); err == nil && n > maxSeq {
					maxSeq = n
				}
			}
			c.state, c.errText, c.result = p.State, p.Error, p.Result
			if p.Prompt != "" {
				c.prompt = p.Prompt
			}
		case EventoEntregue:
			for _, id := range strings.Split(p.IDs, ",") {
				if c := recs[strings.TrimSpace(id)]; c != nil {
					c.delivered = true
				}
			}
		}
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if maxSeq > g.seq {
		g.seq = maxSeq
	}
	now := g.clock()
	n := 0
	for _, id := range order {
		c := recs[id]
		if c.delivered || g.tasks[id] != nil {
			continue
		}
		t := &Tarefa{ID: id, Prompt: c.prompt, Criada: now, Resultado: c.result}
		switch c.state {
		case Completed, Failed, Cancelled:
			t.Estado, t.Erro = c.state, c.errText
		default:
			t.Estado, t.Erro = Failed, MsgInterrompida
		}
		t.TerminadaEm = now
		g.tasks[id] = t
		g.order = append(g.order, id)
		g.persistTask(t, false)
		n++
	}
	return n
}

// clock is time.Now, replaceable in tests.
func (g *Gerente) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}
