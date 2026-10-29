// Package subagent implements the sibling-task runner: autonomous sub-agents
// that run in the background with ALL built-in tools EXCEPT delegation, and an
// in-memory task registry exposed to the parent agent through three tools
// (spawn/status/result).
//
// The "child does not delegate" invariant holds BY CONSTRUCTION, not by
// filtering: runtime.Session.Send builds each turn's tool list by calling
// tools.SpecsBuiltins() (session.go: `lista := tools.SpecsBuiltins()`), and
// this package's specs are handed ONLY to the parent's list by the caller's
// wiring. No code path injects <prefix>_spawn into a child session — the
// sub-agent is born without it —, and the task prompt is passed to the child
// AS IS, with no wrapper: whoever wants extra instructions writes them in the
// prompt.
//
// Persistence is OUT OF SCOPE: the registry lives only in memory and tasks die
// with the process. The sessions slice plugs in the store later; Tarefa
// already carries a deterministic ID and an explicit lifecycle, which is all a
// persisted registry needs.
//
// LIFETIME: Spawn derives the child session's context from the given ctx
// (context.WithCancel), so cancelling the parent cancels the task. The REPL
// creates a ctx PER TURN and cancels it at the end of the line — passing that
// ctx would bury the task as soon as the turn ended. For tasks that outlive
// the turn, the wiring must use a long-lived ctx (e.g.
// g.Vincular(context.Background()) once, and Spawn with a ctx at that level);
// Cancelar remains the per-task stop. See Vincular.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

const (
	// PrefixoPadrao is the launcher tool's name (and the status/result
	// prefix) when Config.Nome is empty: subagent / subagent_status /
	// subagent_result. Single lowercase tool name, opencode-style.
	PrefixoPadrao = "subagent"

	// TetoTranscript caps the number of tool-trace lines per task.
	TetoTranscript = 200
	// TetoConversa caps the number of activity entries kept per task.
	TetoConversa = 300
	// TetoTextoConversa bounds each captured activity body.
	TetoTextoConversa = 16 << 10
)

const (
	EventoPensamento = "thinking"
	EventoNota       = "note"
	EventoResposta   = "assistant"
	EventoToolCall   = "tool_call"
	EventoSistema    = "system"
)

// Evento is one visible part of a sub-agent's conversation. Tool calls and
// results are separate, so the UI can show a call while its tool is still
// running, then update it when the result arrives.
type Evento struct {
	ID         string
	Tipo       string
	Nome       string
	Args       string
	Texto      string
	Duracao    time.Duration
	Erro       bool
	Executando bool
}

// Task states.
const (
	Queued    = "queued"
	Running   = "running"
	Completed = "completed"
	Failed    = "failed"
	Cancelled = "cancelled"
)

// Config is the manager's configuration. Nome is the launcher tool's name
// exposed to the parent ("subagent"; status/result get it as a prefix:
// subagent_status/subagent_result). There
// is no concurrency or count cap: every task starts running on Spawn.
type Config struct {
	Nome string
}

// Tarefa is a sibling task: what was requested, what state it is in and what
// the sub-agent produced. Resultado is the child's FINAL text (what
// <prefix>_result returns); Erro is ONE failure line; the tool trace lives in
// transcript (capped at TetoTranscript lines).
//
// The lifecycle fields are written by the worker and read by anyone, always
// under Gerente.mu — the task has NO lock of its own, which is why it can be
// copied (snapshot, tests) without tripping vet.
type Tarefa struct {
	ID          string
	Prompt      string
	Estado      string
	Criada      time.Time
	Inicada     time.Time
	TerminadaEm time.Time
	Resultado   string
	Erro        string

	cancel     context.CancelFunc
	ctx        context.Context // child session; born together with the (ctx, cancel) pair
	transcript []string
	conversa   []Evento
}

// Transcricao returns a copy of the tool trace (observability/tests).
func (t *Tarefa) Transcricao() []string {
	return append([]string(nil), t.transcript...)
}

// Conversa returns a copy of the captured conversation activity.
func (t *Tarefa) Conversa() []Evento {
	return append([]Evento(nil), t.conversa...)
}

// collector is a sub-agent's runtime.Renderer: it draws nothing, it only
// COLLECTS output into the Tarefa. Resposta accumulates the final text in
// Resultado; Tool records a trace line; Eco/Guardrail/Tokens are no-ops —
// whoever only wants the result needs neither echo nor a token footer.
//
// It holds the manager lock only to write into the task (never across the
// network call), and it is the ONLY writer of Resultado/transcript while the
// task runs.
type collector struct {
	g *Gerente
	t *Tarefa
}

// activity stores one child event and calls the host outside the manager lock.
// The callback may write to the ACP stream and must not run while g.mu is held.
func (c *collector) activity(event Evento, result bool) {
	c.g.mu.Lock()
	if result {
		c.t.appendResult(event.Texto)
	}
	c.t.appendConversa(event)
	fn, taskID := c.g.onActivity, c.t.ID
	c.g.mu.Unlock()
	if fn != nil {
		fn(taskID, event)
	}
}

func (c *collector) Resposta(text string) {
	if text == "" {
		return
	}
	c.activity(Evento{Tipo: EventoResposta, Texto: text}, true)
}

func (c *collector) Tool(name string, _ time.Duration, isErr bool) {
	line := "[tool " + name + "]"
	if isErr {
		line = "[tool " + name + " error]"
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	c.t.appendTranscript(line)
}

// Optional renderer callbacks retain the same milestones and tool payloads
// the parent's full-screen transcript displays.
func (c *collector) Pensando()    {}
func (c *collector) Contexto(int) {}
func (c *collector) Pensou(text string, dur time.Duration) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.activity(Evento{Tipo: EventoPensamento, Texto: text, Duracao: dur}, false)
}
func (c *collector) Nota(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	c.activity(Evento{Tipo: EventoNota, Texto: text}, false)
}
func (c *collector) ToolComecou(id, name, args string) {
	c.activity(Evento{ID: id, Tipo: EventoToolCall, Nome: name, Args: args, Executando: true}, false)
}
func (c *collector) ToolSaida(id, chunk string) {
	if chunk == "" {
		return
	}
	c.g.mu.Lock()
	defer c.g.mu.Unlock()
	for i := len(c.t.conversa) - 1; i >= 0; i-- {
		if c.t.conversa[i].ID == id && c.t.conversa[i].Tipo == EventoToolCall {
			c.t.conversa[i].Texto = limitConversationText(c.t.conversa[i].Texto + chunk)
			return
		}
	}
}
func (c *collector) ToolTerminou(id, name, args, output string, dur time.Duration, failed bool) {
	c.g.mu.Lock()
	line := "[tool " + name + "]"
	if failed {
		line = "[tool " + name + " error]"
	}
	c.t.appendTranscript(line)
	event := Evento{ID: id, Tipo: EventoToolCall, Nome: name, Args: args, Texto: limitConversationText(output), Duracao: dur, Erro: failed}
	for i := len(c.t.conversa) - 1; i >= 0; i-- {
		e := &c.t.conversa[i]
		if e.ID == id && e.Tipo == EventoToolCall {
			e.Nome, e.Args, e.Texto = name, args, limitConversationText(output)
			e.Duracao, e.Erro, e.Executando = dur, failed, false
			event = *e
			break
		}
	}
	fn, taskID := c.g.onActivity, c.t.ID
	c.g.mu.Unlock()
	if fn != nil && event.ID != "" {
		fn(taskID, event)
	}
}
func (c *collector) Guardrail(proactive bool) {
	text := "Context compacted"
	if proactive {
		text = "Context compacted proactively"
	}
	c.activity(Evento{Tipo: EventoSistema, Texto: text}, false)
}
func (c *collector) Cortada(limit int, gaveUp bool) {
	text := fmt.Sprintf("Response cut at the output limit (%d tokens)", limit)
	if gaveUp {
		text += "; stopped"
	} else {
		text += "; continuing"
	}
	c.activity(Evento{Tipo: EventoSistema, Texto: text}, false)
}

func (c *collector) Eco(string)                           {}
func (c *collector) Tokens(int, int, time.Duration, bool) {}

// Gerente registers and runs the sibling tasks. Thread-safe: the caller
// creates and queries tasks while the workers run in the background.
type Gerente struct {
	name string
	cwd  string

	mu      sync.Mutex
	tasks   map[string]*Tarefa
	order   []string
	seq     int
	running int

	childCfg config.Config   // config inherited by ALL tasks (provider/model/key)
	parent   context.Context // long-lived context; see Vincular

	// onFinish is called (outside the lock) whenever a task completes, fails
	// or is cancelled: that is how the parent learns without polling status.
	onFinish   func()
	onActivity func(taskID string, event Evento)
	delivered  map[string]bool // tasks already delivered by Terminadas

	// spawnTurn is the ctx of the turn that already launched sub-agents: a
	// turn launches only once (see spawnSpec).
	spawnTurn context.Context
}

// firstSpawnOfTurn reports whether this turn (identified by the tool's ctx)
// has not launched sub-agents yet, and marks it as having launched.
func (g *Gerente) firstSpawnOfTurn(ctx context.Context) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ctx != nil && g.spawnTurn == ctx {
		return false
	}
	g.spawnTurn = ctx
	return true
}

// Novo creates a task manager.
func Novo(cfg Config) *Gerente {
	name := strings.TrimSpace(cfg.Nome)
	if name == "" {
		name = PrefixoPadrao
	}
	cwd, _ := os.Getwd()
	return &Gerente{
		name:      name,
		cwd:       cwd,
		tasks:     map[string]*Tarefa{},
		delivered: map[string]bool{},
		parent:    context.Background(),
	}
}

// AoTerminar registers who is notified when a task finishes (completed,
// failed or cancelled). The notification does not carry the task: the
// receiver calls Terminadas, which delivers each finished task only once —
// several tasks finishing together become a single notice to the parent.
func (g *Gerente) AoTerminar(fn func()) {
	g.mu.Lock()
	g.onFinish = fn
	g.mu.Unlock()
}

// AoAtividade registers a host for each visible child event. Callbacks run
// outside the manager lock so the host can safely write to a protocol stream.
func (g *Gerente) AoAtividade(fn func(taskID string, event Evento)) {
	g.mu.Lock()
	g.onActivity = fn
	g.mu.Unlock()
}

// Terminadas returns copies of the tasks that finished and were not yet
// delivered to the parent, in creation order, and marks them as delivered.
func (g *Gerente) Terminadas() []Tarefa {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []Tarefa
	for _, id := range g.order {
		t := g.tasks[id]
		if t == nil || g.delivered[id] || t.TerminadaEm.IsZero() {
			continue
		}
		g.delivered[id] = true
		out = append(out, copyTask(t))
	}
	return out
}

// markDelivered: the parent already read the result (subagent_result), so the
// automatic notice for that task need not repeat the text.
func (g *Gerente) markDelivered(id string) {
	g.mu.Lock()
	if t := g.tasks[id]; t != nil && !t.TerminadaEm.IsZero() {
		g.delivered[id] = true
	}
	g.mu.Unlock()
}

// Ativas counts the tasks that have not finished yet.
func (g *Gerente) Ativas() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, t := range g.tasks {
		if t.TerminadaEm.IsZero() {
			n++
		}
	}
	return n
}

// notify calls onFinish outside the lock.
func (g *Gerente) notify() {
	g.mu.Lock()
	fn := g.onFinish
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// Prefixo returns this manager's tool prefix.
func (g *Gerente) Prefixo() string { return g.name }

// Cwd returns the directory tasks run in by default.
func (g *Gerente) Cwd() string { return g.cwd }

// DefinirCwd changes the working directory of future tasks.
func (g *Gerente) DefinirCwd(cwd string) { g.mu.Lock(); g.cwd = cwd; g.mu.Unlock() }

// ConfigPai stores the configuration every child task inherits (same
// provider/model/key as the parent). Called by the wiring — normally via
// Especificacoes, which already receives the parent config.
func (g *Gerente) ConfigPai(cfg config.Config) {
	g.mu.Lock()
	g.childCfg = cfg
	g.mu.Unlock()
}

// Vincular replaces the parent context used when Spawn gets a nil ctx. The
// REPL calls it once with context.Background() so sibling tasks outlive the
// turn that created them (the per-turn ctx is cancelled at the end of the line).
func (g *Gerente) Vincular(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}
	g.mu.Lock()
	g.parent = parent
	g.mu.Unlock()
}

// Tarefas returns a snapshot of the tasks in creation order (the internal map
// never escapes).
func (g *Gerente) Tarefas() []*Tarefa {
	g.mu.Lock()
	out := make([]*Tarefa, 0, len(g.order))
	for _, id := range g.order {
		if t := g.tasks[id]; t != nil {
			out = append(out, t)
		}
	}
	g.mu.Unlock()
	return out
}

// Fotos returns COPIES of all tasks, in creation order, read under the same
// lock the workers use to write. This is what the UI should use: the Tarefas
// pointers are live, and reading Estado/Resultado from them while a worker
// writes is a data race.
func (g *Gerente) Fotos() []Tarefa {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Tarefa, 0, len(g.order))
	for _, id := range g.order {
		t := g.tasks[id]
		if t == nil {
			continue
		}
		out = append(out, copyTask(t))
	}
	return out
}

// snapshot returns a consistent copy of a task (the whole read happens under
// the same lock the workers use to write).
func (g *Gerente) snapshot(id string) (Tarefa, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.tasks[id]
	if !ok || t == nil {
		return Tarefa{}, false
	}
	return copyTask(t), true
}

func copyTask(t *Tarefa) Tarefa {
	cp := *t
	cp.transcript = append([]string(nil), t.transcript...)
	cp.conversa = append([]Evento(nil), t.conversa...)
	cp.cancel, cp.ctx = nil, nil
	return cp
}

// finish closes a task's lifecycle (called under the manager lock). An empty
// errText gets a default text per state, so that <prefix>_result never
// returns a state without explanation.
func (t *Tarefa) finish(state, errText string) {
	t.Estado = state
	if errText == "" {
		switch state {
		case Cancelled:
			errText = "cancelled"
		}
	}
	if errText != "" {
		t.Erro = errText
	}
	t.TerminadaEm = time.Now()
	t.cancel = nil
	t.ctx = nil
}

// markStarted records that the task left the queue and started running.
func (t *Tarefa) markStarted() {
	t.Estado = Running
	t.Inicada = time.Now()
}

// appendResult adds the model's text to the final result.
func (t *Tarefa) appendResult(text string) {
	if t.Resultado != "" {
		t.Resultado += "\n"
	}
	t.Resultado += text
}

// appendTranscript records an executed tool, honouring the line cap.
func (t *Tarefa) appendTranscript(line string) {
	if len(t.transcript) >= TetoTranscript {
		return
	}
	t.transcript = append(t.transcript, line)
}

func (t *Tarefa) appendConversa(event Evento) {
	if len(t.conversa) >= TetoConversa {
		return
	}
	event.Nome = limitConversationText(event.Nome)
	event.Args = limitConversationText(event.Args)
	event.Texto = limitConversationText(event.Texto)
	t.conversa = append(t.conversa, event)
}

func limitConversationText(text string) string {
	if len(text) <= TetoTextoConversa {
		return text
	}
	text = text[:TetoTextoConversa]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text + "… (truncated)"
}

// Spawn creates a task and hands it to the runner. The prompt goes to the
// child AS IS. The given ctx (or the bound one, if nil) produces the child
// session's context: cancelling it cancels the task. There is no cap: the task
// starts running right away and Spawn never blocks. Returns the deterministic
// id (t1, t2, …).
func (g *Gerente) Spawn(ctx context.Context, request string) (string, error) {
	if strings.TrimSpace(request) == "" {
		return "", fmt.Errorf("empty prompt")
	}
	g.mu.Lock()
	if ctx == nil {
		ctx = g.parent
	}
	cfg := g.childCfg
	cwd := g.cwd
	g.seq++
	t := &Tarefa{ID: fmt.Sprintf("t%d", g.seq), Prompt: request, Estado: Queued, Criada: time.Now()}
	g.tasks[t.ID] = t
	g.order = append(g.order, t.ID)
	id := t.ID
	g.mu.Unlock()

	if ctx.Err() != nil {
		// Parent already dead: the task is born cancelled instead of
		// pretending to run.
		g.mu.Lock()
		t.finish(Cancelled, "parent context was already done")
		g.mu.Unlock()
		g.notify()
		return id, nil
	}
	if !cfg.Pronta() {
		g.mu.Lock()
		t.finish(Failed, "incomplete config: "+strings.Join(cfg.Faltas(), ", "))
		g.mu.Unlock()
		g.notify()
		return id, nil
	}

	// The child session's (ctx, cancel) pair is born RIGHT HERE, derived from
	// the parent ctx — not inside the worker. Creating it under the SAME lock
	// that marks running closes the window in which a Cancelar would see the
	// task running with a nil cancel and refuse to cancel.
	// No concurrency cap and no queue: every task starts running now.
	g.mu.Lock()
	childCtx, cancel := context.WithCancel(ctx)
	t.ctx = childCtx
	t.cancel = cancel
	cfg, cwd = g.childCfg, g.cwd
	t.markStarted()
	g.running++
	g.mu.Unlock()
	go g.run(ctx, t, cfg, cwd)
	return id, nil
}

// run executes the child session: the same built-in tools as the parent,
// without the delegation ones (see the package doc), an empty transcript and
// output collected by the collector. When it finishes (or fails, or is
// cancelled) it notifies the parent (AoTerminar).
//
// The child session's (ctx, cancel) pair was already created in Spawn; here it
// is used and released.
func (g *Gerente) run(ctx context.Context, t *Tarefa, cfg config.Config, cwd string) {
	defer func() {
		// A panic inside the sub-agent runs in its own goroutine and, without
		// recover, used to bring down the whole bh (main session included).
		// It becomes a task failure.
		if r := recover(); r != nil {
			g.mu.Lock()
			t.finish(Failed, fmt.Sprintf("panic: %v", r))
			g.mu.Unlock()
		}
		g.mu.Lock()
		if g.running > 0 {
			g.running--
		}
		g.mu.Unlock()
		g.notify()
	}()

	g.mu.Lock()
	childCtx, cancel := t.ctx, t.cancel
	g.mu.Unlock()
	if childCtx == nil {
		// No child session (unexpected state): fail instead of panicking;
		// the defer above notifies the parent.
		g.mu.Lock()
		t.finish(Failed, "child session not initialized")
		g.mu.Unlock()
		return
	}
	defer cancel()

	// OWN TRANSCRIPT, no inheritance: NewSession already starts with an empty
	// history; ClearHistory is just an explicit reinforcement of the invariant
	// (the sub-agent does not see the parent's history, nor the parent the
	// child's).
	sess := runtime.NewSession(cfg)
	sess.ClearHistory()

	// Child session = tools.SpecsBuiltins() ONLY (session.go builds the list
	// internally). No delegation spec reaches here: see the package doc.
	err := sess.Send(childCtx, &collector{g: g, t: t}, cwd, t.Prompt)

	g.mu.Lock()
	switch {
	case err == nil:
		t.finish(Completed, "")
	case childCtx.Err() != nil || ctx.Err() != nil || errors.Is(err, context.Canceled):
		t.finish(Cancelled, "")
	default:
		t.finish(Failed, oneLine(err))
	}
	g.mu.Unlock()
}

// Cancelar stops a task. Queued becomes cancelled immediately; running gets
// its session's cancel() called and the worker marks it cancelled. Returns
// true only when there was something to cancel.
func (g *Gerente) Cancelar(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tasks[id]
	if t == nil {
		return false
	}
	switch t.Estado {
	case Queued:
		t.finish(Cancelled, "cancelled before starting")
		go g.notify()
		return true
	case Running:
		if t.cancel != nil {
			t.cancel() // the worker writes cancelled while unwinding the cancel
			return true
		}
		return false
	default:
		return false
	}
}

// oneLine collapses an error into ONE short line (the Erro field is meant for
// quick reading and goes straight to the <prefix>_result tool).
func oneLine(err error) string {
	if err == nil {
		return ""
	}
	s := strings.Join(strings.Fields(err.Error()), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
