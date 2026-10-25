// ACP v1 (Agent Client Protocol) agent for Barroso Harness, spoken over stdio
// as newline-delimited JSON-RPC. The package is the server: a CLIENT (editor,
// Zed, our own CLI) launches the process and drives the turns.
//
// Implemented surface, and only this — each method exists because a real ACP
// client sends it, and the absence of the others is a decision, not an
// oversight:
//
//	initialize     → protocolVersion echoed (what the client sent, if >0)
//	session/new    → creates a session with cwd; id "s-<8hex>"
//	session/prompt → turn; session/update notifications during, response at the end
//	session/cancel → cancels that session's in-flight turn
//	session/load, session/resume → reopen a recorded session, replaying its history
//	session/set_config_option, session/set_model → model / reasoning effort
//	                 of the session (config_options.go)
//
// The rest of the protocol (session/list, authenticate, request_permission)
// was left out on purpose: the harness runs in YOLO — there is nothing to
// approve. Adding a new method is cheap; faking a capability the runtime
// lacks is not.
//
// Concurrency: the Handle loop reads sequentially, but session/prompt
// dispatches the turn on a goroutine — that is what lets session/cancel arrive
// mid-turn. Every write to the wire goes through writeMu, otherwise two JSON
// lines would interleave.
package acp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// Turno is the testable seam of the runtime: the only thing the agent needs
// from a session is to run a turn. runtime.Session satisfies the interface
// without an adapter (same signature), and tests inject a fake that emits
// deterministic events and honours the ctx.
type Turno interface {
	Send(ctx context.Context, r runtime.Renderer, cwd string, prompt string) error
}

// ACP methods served. Exported for anyone who wants to route before Handle.
const (
	MetInitialize  = "initialize"
	MetSessionNovo = "session/new"
	MetPrompt      = "session/prompt"
	MetCancel      = "session/cancel"
	MetLoad        = "session/load"
	MetResume      = "session/resume"
)

// Carregador reopens a recorded session by id. It returns an error when the
// session does not exist or cannot be read. Optional: without it the agent
// announces loadSession=false and refuses session/load.
type Carregador func(cfg config.Config, id string) (Turno, error)

// withHistory is what the agent needs from a Turno to replay the conversation
// to the client on session/load. runtime.Session satisfies it without an
// adapter.
type withHistory interface {
	History() []dealer.Mensagem
}

// session holds an ACP client's turn and the cancellation of the in-flight turn.
type session struct {
	id     string
	cwd    string
	turn   Turno
	cancel context.CancelFunc // nil while no turn is running
}

// Agente is the ACP server. It is born with the config captured in Novo
// (determinism: session/new does NOT re-read the disk, so tests do not depend
// on ~/.barroso-harness) and a turn factory — the production one wraps
// runtime.NewSession, the test one returns a fake.
type Agente struct {
	cfg     config.Config
	log     func(string) // optional; nil = silent
	Fabrica func(config.Config) (Turno, string)
	// Carregar reopens a recorded session (session/load). nil = loadSession false.
	Carregar Carregador

	mu       sync.Mutex
	sessions map[string]*session

	writeMu sync.Mutex   // serializes writes to the wire
	seq     atomic.Int64 // process-unique tool call ids; aligned on 386
	running sync.WaitGroup
	// compaction: the client advertised clientCapabilities.session.compaction
	// in initialize, so compaction_update may be sent (see Guardrail).
	compaction atomic.Bool

	// Catalogo fetches the provider's model list for configOptions; nil =
	// dealer.Modelos. A seam for tests.
	Catalogo func(ctx context.Context, baseURL, apiKey string) ([]dealer.CatalogModel, error)
	catalog  catalogCache
}

// Novo creates the agent over cfg, with the production factory.
func Novo(cfg config.Config) *Agente {
	a := &Agente{
		cfg:      cfg,
		sessions: map[string]*session{},
		Fabrica:  runtimeTurn,
	}
	return a
}

// NovoComFabrica creates the agent with another turn factory (tests, embedded
// harness, alternative engine).
func NovoComFabrica(cfg config.Config, factory func(config.Config) (Turno, string)) *Agente {
	a := Novo(cfg)
	if factory != nil {
		a.Fabrica = factory
	}
	return a
}

// RegistrarLog turns on the diagnostic log (nil turns it off).
func (a *Agente) RegistrarLog(f func(string)) { a.log = f }

// ComLog is the fluent form of RegistrarLog.
func (a *Agente) ComLog(f func(string)) *Agente { a.RegistrarLog(f); return a }

func (a *Agente) logf(format string, args ...any) {
	if a.log == nil {
		return
	}
	a.log(fmt.Sprintf(format, args...))
}

// runtimeTurn is the production factory: one runtime.Session per ACP session,
// with a fresh id. The (Turno, id) pair lets tests pick the id too.
func runtimeTurn(cfg config.Config) (Turno, string) {
	return runtime.NewSession(cfg), newSessionID()
}

// newSessionID returns "s-" + 8 hex digits (32 bits of randomness).
func newSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Kernel randomness is reliable on the target platforms; if it still
		// fails, the clock keeps ids unique without panicking.
		binary.LittleEndian.PutUint32(b[:], uint32(time.Now().UnixNano()))
	}
	return "s-" + hex.EncodeToString(b[:])
}

// Handle serves the protocol until the reader ends, the ctx is cancelled or a
// write fails. Client EOF is a CLEAN shutdown: it returns nil.
//
// A malformed line does not kill the loop (it is logged and ignored) — a
// client that sent garbage on one line should not bring the whole agent down.
// Only a line above the 8 MiB cap (the scanner's ErrTooLong) ends it.
func (a *Agente) Handle(ctx context.Context, r io.Reader, w io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	sc := NovoScanner(r)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, ok, err := DecodificarLinha(sc.Bytes())
		if err != nil {
			// Invalid JSON: we do not know the id, so there is no one to answer.
			a.logf("acp: malformed line ignored: %v", err)
			continue
		}
		if !ok {
			continue
		}
		if err := a.dispatch(ctx, w, req); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	return ctx.Err()
}

// dispatch handles one request. A client notification (no id) is ignored —
// the agent defines no inbound notifications in this scope.
func (a *Agente) dispatch(ctx context.Context, w io.Writer, req Request) error {
	if !req.TemID() {
		a.logf("acp: client notification ignored: %s", req.Method)
		return nil
	}
	switch req.Method {
	case MetInitialize:
		return a.initialize(w, req)
	case MetSessionNovo:
		return a.newSession(w, req)
	case MetPrompt:
		return a.prompt(ctx, w, req)
	case MetCancel:
		return a.cancel(w, req)
	case MetLoad, MetResume:
		// session/resume is an alias of session/load here: both reopen a
		// recorded session and replay it. A real resume-only agent (new id per
		// resume) would differ, but the bh reuses the recorded id, so the
		// client can use either name and get the same session back.
		return a.loadSession(w, req)
	case MetSetConfig:
		return a.setConfigOption(w, req)
	case MetSetModel:
		return a.setModel(w, req)
	default:
		a.logf("acp: unknown method: %s", req.Method)
		return WriteError(w, req.ID, CodMetodo, "method not found: "+req.Method)
	}
}

// ── initialize ─────────────────────────────────────────────────────────────

func (a *Agente) initialize(w io.Writer, req Request) error {
	// Missing params are tolerable: the client may send {} or nothing. An
	// unrecognizable protocolVersion is not honoured — the default 1 applies.
	var p struct {
		ProtocolVersion    any            `json:"protocolVersion"`
		ClientCapabilities map[string]any `json:"clientCapabilities"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	v := 1
	if f, ok := p.ProtocolVersion.(float64); ok && int(f) > 0 {
		v = int(f)
	}
	// Omitted or null means unsupported; {} advertises the whole contract.
	sess, _ := p.ClientCapabilities["session"].(map[string]any)
	a.compaction.Store(sess != nil && sess["compaction"] != nil)
	return a.respond(w, NovaResposta(req.ID, map[string]any{
		"protocolVersion": v,
		"agentCapabilities": map[string]any{
			"loadSession": a.Carregar != nil,
		},
		"authMethods": []any{},
	}))
}

// ── session/new ────────────────────────────────────────────────────────────

func (a *Agente) newSession(w io.Writer, req Request) error {
	var p struct {
		CWD string `json:"cwd"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	cwd := p.CWD
	if cwd == "" {
		// An empty cwd is not an invalid session: the client may not know where
		// it is. Assume the process directory — worst case, the turn fails
		// because of the tools and the client fixes it with another session.
		if d, err := os.Getwd(); err == nil {
			cwd = d
		} else {
			cwd = "."
		}
	}
	turn, id := a.Fabrica(a.cfg)
	a.mu.Lock()
	// An id collision is unlikely (32 bits), but a repeated id would overwrite
	// a live session; spinning until a free one is found is cheap.
	for _, exists := a.sessions[id]; exists; {
		id = newSessionID()
		_, exists = a.sessions[id]
	}
	s := &session{id: id, cwd: cwd, turn: turn}
	a.sessions[id] = s
	a.mu.Unlock()
	a.logf("acp: session %s created in %s", id, cwd)
	res := a.sessionState(s)
	res["sessionId"] = id
	return a.respond(w, NovaResposta(req.ID, res))
}

// ── session/load ───────────────────────────────────────────────────────────

// loadSession reopens a recorded session. Per the protocol, the agent REPLAYS
// the conversation to the client as session/update notifications —
// user_message_chunk, agent_message_chunk and tool_call — and only then
// responds. That way the editor redraws the history without keeping state of
// its own.
//
// A session already live in this process is reused (the log is not reopened
// over an in-flight turn); a session unknown on disk is a -32602 error with
// the id.
func (a *Agente) loadSession(w io.Writer, req Request) error {
	if a.Carregar == nil {
		return WriteError(w, req.ID, CodMetodo, "session/load not supported (loadSession=false)")
	}
	var p struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" {
		return WriteError(w, req.ID, CodParams, "invalid params: sessionId is required")
	}
	cwd := p.CWD
	if cwd == "" {
		if d, err := os.Getwd(); err == nil {
			cwd = d
		} else {
			cwd = "."
		}
	}

	a.mu.Lock()
	live, ok := a.sessions[p.SessionID]
	a.mu.Unlock()
	var turn Turno
	if ok {
		if live.cancel != nil {
			return WriteError(w, req.ID, CodTurnoRodando, "turn in progress in this session")
		}
		turn = live.turn
	} else {
		t, err := a.Carregar(a.cfg, p.SessionID)
		if err != nil {
			return WriteError(w, req.ID, CodParams, "session not loaded: "+p.SessionID+": "+errorMessage(err))
		}
		turn = t
		a.mu.Lock()
		a.sessions[p.SessionID] = &session{id: p.SessionID, cwd: cwd, turn: turn}
		a.mu.Unlock()
	}

	if h, ok := turn.(withHistory); ok {
		a.replay(w, p.SessionID, h.History())
	}
	a.logf("acp: session %s loaded in %s", p.SessionID, cwd)
	loaded, _ := a.lookupSession(p.SessionID)
	return a.respond(w, NovaResposta(req.ID, a.sessionState(loaded)))
}

// replay emits the transcript as notifications, in order. A tool_call and its
// result become a single tool_call with the final status and the output as
// content — the same format the client received live.
func (a *Agente) replay(w io.Writer, sessionID string, h []dealer.Mensagem) {
	r := &acpRenderer{a: a, w: w, session: sessionID}
	ids := map[string]string{} // ToolID from the log → emitted toolCallId
	for _, m := range h {
		switch m.Role {
		case "user":
			r.emit("user_message_chunk", map[string]any{
				"content": map[string]any{"type": "text", "text": m.Conteudo},
			})
		case "assistant":
			if m.Conteudo != "" {
				r.Resposta(m.Conteudo)
			}
		case "tool_call":
			id := a.nextID()
			ids[m.ToolID] = id
			upd := map[string]any{
				"toolCallId": id,
				"title":      m.ToolName,
				"kind":       "other",
				"status":     "pending",
			}
			if m.ToolArgs != "" {
				upd["rawInput"] = rawInput(m.ToolArgs)
			}
			r.emit("tool_call", upd)
		case "tool_result":
			id := ids[m.ToolID]
			if id == "" {
				continue
			}
			status := "completed"
			if m.IsErro {
				status = "failed"
			}
			upd := map[string]any{"toolCallId": id, "status": status}
			if echo := shortEcho(m.Conteudo); echo != "" {
				upd["content"] = []map[string]any{{"type": "text", "text": echo}}
			}
			r.emit("tool_call_update", upd)
		}
	}
}

// shortEcho truncates replayed output: the client is redrawing history, it
// does not need 60 KiB of grep from three turns ago.
func shortEcho(s string) string {
	const limit = 2 << 10
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// ── session/prompt ─────────────────────────────────────────────────────────

func (a *Agente) prompt(ctx context.Context, w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
		Prompt    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" || len(p.Prompt) == 0 {
		return WriteError(w, req.ID, CodParams, "invalid params: sessionId and prompt (at least one block) are required")
	}
	s, ok := a.lookupSession(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "unknown session: "+p.SessionID)
	}

	// Prompt text = concatenation of the type=text blocks. Blocks of another
	// type (image, audio, resource) do not go into the request: the harness is
	// text-only in this scope, and failing would be worse than ignoring — the
	// client sees the remaining text.
	promptText := ""
	for _, b := range p.Prompt {
		if b.Type != "" && b.Type != "text" {
			continue
		}
		promptText += b.Text
	}
	if promptText == "" {
		return WriteError(w, req.ID, CodParams, "invalid params: prompt has no text block")
	}

	a.mu.Lock()
	if s.cancel != nil {
		a.mu.Unlock()
		return WriteError(w, req.ID, CodTurnoRodando, "turn already in progress in this session")
	}
	// The turn ctx dies on session/cancel OR on the Handle ctx, whichever
	// comes first.
	tctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	a.mu.Unlock()

	rend := &acpRenderer{a: a, w: w, session: s.id}
	a.running.Add(1)
	// Keepalive: the turn is long (a provider round can take minutes) and the
	// connector gives up on 10 minutes of silence. A periodic notification —
	// progress, not content — keeps the wire speaking while the turn runs.
	stop := a.keepalive(rend, tctx)
	go func() {
		defer a.running.Done()
		err := s.turn.Send(tctx, rend, s.cwd, promptText)
		stop()
		a.mu.Lock()
		s.cancel = nil
		a.mu.Unlock()
		cancel()
		if err != nil {
			a.respond(w, NovoErro(req.ID, CodErroAgente, errorMessage(err)))
			a.logf("acp: turn of session %s failed: %v", s.id, err)
			return
		}
		a.respond(w, NovaResposta(req.ID, map[string]any{}))
		a.logf("acp: turn of session %s completed", s.id)
	}()
	return nil
}

// keepalive emits a periodic notification while a turn runs. A long turn (a
// provider round can take minutes) would otherwise leave the wire silent, and
// the connector gives up on 10 minutes of silence — the session/prompt never
// got an answer even though the turn was still alive. The returned stop() ends
// the ticker.
func (a *Agente) keepalive(rend *acpRenderer, ctx context.Context) func() {
	t := time.NewTicker(keepaliveInterval)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		for {
			select {
			case <-t.C:
				rend.Pensando()
			case <-done:
				t.Stop()
				return
			case <-ctx.Done():
				t.Stop()
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// errorMessage swaps Go's terse "context canceled" for a sentence the ACP
// client understands: it was the client who cancelled.
func errorMessage(err error) string {
	if errors.Is(err, context.Canceled) {
		return "turn cancelled by the client (session/cancel)"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "turn expired (deadline exceeded)"
	}
	return err.Error()
}

// ── session/cancel ─────────────────────────────────────────────────────────

func (a *Agente) cancel(w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" {
		return WriteError(w, req.ID, CodParams, "invalid params: sessionId is required")
	}
	s, ok := a.lookupSession(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "unknown session: "+p.SessionID)
	}
	a.mu.Lock()
	c := s.cancel
	a.mu.Unlock()
	if c != nil {
		c()
		a.logf("acp: cancellation requested for session %s", s.id)
	} else {
		a.logf("acp: cancellation with no in-flight turn in session %s", s.id)
	}
	return a.respond(w, NovaResposta(req.ID, map[string]any{}))
}

// lookupSession finds an existing session.
func (a *Agente) lookupSession(id string) (*session, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	return s, ok
}

// respond writes a response, serialized with all other writes.
func (a *Agente) respond(w io.Writer, r Response) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return EscreverLinha(w, r)
}

// notify writes a one-way notification (session/update).
func (a *Agente) notify(w io.Writer, n Notification) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return EscreverLinha(w, n)
}

// nextID yields process-unique tool call ids (clients index by them).
func (a *Agente) nextID() string {
	return fmt.Sprintf("tool-%d", a.seq.Add(1))
}

// Esperar waits for in-flight turns. It serves a host that wants to shut down
// cleanly: Sirve calls it before closing the descriptors, so no response is
// left half-written. Turns stuck on network I/O only exit with a cancelled ctx.
func (a *Agente) Esperar() { a.running.Wait() }

// ── acpRenderer ────────────────────────────────────────────────────────────

// acpRenderer translates the runtime's Renderer into ACP notifications.
// Translation rule, chosen so the client loses no information without
// inventing events the protocol does not have:
//
//	Resposta     → agent_message_chunk (the model's text; runtime delivers it whole)
//	Nota         → agent_message_chunk (text of a round that also calls tools)
//	ToolComecou  → tool_call in_progress, kind "other", arguments as rawInput
//	ToolTerminou → tool_call_update with the final status and an output excerpt
//	Tool / Eco   → fallback when the runtime does not use the live path
//	Guardrail    → compaction_update, or the opencode/compaction marker on
//	               session_info_update when the client lacks the capability
//	Tokens       → nothing: the usage footer is a local UI matter, not protocol
type acpRenderer struct {
	a       *Agente
	w       io.Writer
	session string

	mu   sync.Mutex
	last string // toolCallId of the last tool_call, for Eco to attach to
	// ids maps the runtime's live id ("3/c1") to the toolCallId given to the
	// client, so ToolSaida/ToolTerminou hit the call that was announced.
	ids map[string]string
}

func (r *acpRenderer) Resposta(text string) {
	// The chunk content is a single-block OBJECT ({type,text}), not two loose
	// fields on the update: that is the format the ACP client validates.
	r.emit("agent_message_chunk", map[string]any{
		"content": map[string]any{"type": "text", "text": text},
	})
}

func (r *acpRenderer) Tool(name string, dur time.Duration, failed bool) {
	status := "completed"
	if failed {
		status = "failed"
	}
	id := r.a.nextID()
	r.mu.Lock()
	r.last = id
	r.mu.Unlock()
	r.emit("tool_call", map[string]any{
		"toolCallId": id,
		"title":      name,
		"kind":       "other",
		"status":     status,
	})
}

// Nota is the text of a round that also called tools ("let me check X"). It
// goes out as agent_message_chunk: a client that sees a tool_call after text
// knows that text was an intermediate message, not the answer.
func (r *acpRenderer) Nota(text string) { r.Resposta(text) }

// ToolComecou announces the call BEFORE it runs, with its arguments as
// rawInput, so the client can show "grep: pattern in file" instead of "grep".
func (r *acpRenderer) ToolComecou(id, name, args string) {
	toolCallID := r.a.nextID()
	r.mu.Lock()
	if r.ids == nil {
		r.ids = make(map[string]string)
	}
	r.ids[id] = toolCallID
	r.last = toolCallID
	r.mu.Unlock()
	r.emit("tool_call", map[string]any{
		"toolCallId": toolCallID,
		"title":      name,
		"kind":       "other",
		"status":     "in_progress",
		"rawInput":   rawInput(args),
	})
}

// rawInput turns the call's JSON arguments into ACP rawInput. Arguments that
// are not valid JSON (a truncated call) go as the raw string: still readable.
func rawInput(args string) any {
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return args
	}
	return v
}

// ToolSaida is silent: ACP content in a tool_call_update REPLACES the previous
// content, so streaming chunks would show only the last one and every chunk
// would be one more event for the client. The result arrives in ToolTerminou.
func (r *acpRenderer) ToolSaida(string, string) {}

// ToolTerminou closes the call announced by ToolComecou with the final status
// and an excerpt of the output.
func (r *acpRenderer) ToolTerminou(id, _, _, output string, _ time.Duration, failed bool) {
	r.mu.Lock()
	toolCallID := r.ids[id]
	delete(r.ids, id)
	r.mu.Unlock()
	if toolCallID == "" {
		return
	}
	status := "completed"
	if failed {
		status = "failed"
	}
	upd := map[string]any{"toolCallId": toolCallID, "status": status}
	if echo := shortEcho(output); echo != "" {
		upd["content"] = []map[string]any{{"type": "text", "text": echo}}
	}
	r.emit("tool_call_update", upd)
}

func (r *acpRenderer) Eco(output string) {
	if output == "" {
		return
	}
	r.mu.Lock()
	id := r.last
	r.mu.Unlock()
	if id == "" {
		return // stray echo, no tool call to attach to
	}
	r.emit("tool_call_update", map[string]any{
		"toolCallId": id,
		"content":    []map[string]any{{"type": "text", "text": output}},
	})
}

// Guardrail reports a compaction as session STATE, never as conversation
// content. As an agent_message_chunk it reached the client as the model's own
// words: one "[guardrail] …" bubble per prune, a dozen in five minutes on a long
// turn, and the chat read as an agent that was doing nothing (2026-10-22).
//
// ACP has a dedicated update for this, gated by the client's capability:
//   - client advertised session.compaction → compaction_update with a terminal
//     status (the guardrail prune is instant, and "one completed update is
//     enough" per the spec);
//   - otherwise the spec forbids compaction_update, and this follows OpenCode's
//     fallback: session_info_update carrying _meta["opencode/compaction"]. A
//     client that does not know the marker ignores it, which is the point.
func (r *acpRenderer) Guardrail(proactive bool) {
	id := fmt.Sprintf("cmp-%d", r.a.seq.Add(1))
	if r.a.compaction.Load() {
		r.emit("compaction_update", map[string]any{
			"compactionId": id,
			"status":       "completed",
		})
		return
	}
	reason := "auto"
	if !proactive {
		// The provider rejected the request size; still automatic, but the
		// detail is worth keeping for whoever reads the marker.
		reason = "overflow"
	}
	r.emit("session_info_update", map[string]any{
		"_meta": map[string]any{
			"opencode/compaction": map[string]any{
				"status":    "completed",
				"messageId": id,
				"reason":    reason,
			},
		},
	})
}

// Retentando tells the client the dealer is repeating a failed request. It is
// session STATE, never conversation content: as an agent_thought_chunk it
// landed in the client's "Raciocínio" panel, glued to the keepalives, and read
// as the model leaking garbage as reasoning (2026-10-23, conversation 93825).
// It goes out as session_info_update with a _meta marker (same fallback as the
// guardrail): a client that does not know the marker ignores it.
func (r *acpRenderer) Retentando(attempt, max int, wait time.Duration, reason error) {
	text := fmt.Sprintf("[retry] provider failed (%v); attempt %d/%d in %s", reason, attempt, max, wait.Round(time.Second))
	r.emit("session_info_update", map[string]any{
		"_meta": map[string]any{
			"barroso/retry": map[string]any{
				"attempt": attempt,
				"max":     max,
				"waitMs":  wait.Milliseconds(),
				"reason":  fmt.Sprint(reason),
			},
		},
	})
	r.a.logf("acp: session %s: %s", r.session, text)
}

// KeepaliveInterval is how often a running turn speaks on the wire. A variable,
// not a constant, so tests can shrink it.
var keepaliveInterval = 20 * time.Second

// Pensando is the keepalive notification: it only exists so the wire is not
// silent. It must carry NO text — as an agent_thought_chunk every tick
// appended "[keepalive] turn still running" to the client's reasoning panel
// (2026-10-23). A session_info_update with a _meta marker keeps the connection
// alive and is ignored by clients that do not know the marker.
func (r *acpRenderer) Pensando() {
	r.emit("session_info_update", map[string]any{
		"_meta": map[string]any{
			"barroso/keepalive": map[string]any{"status": "running"},
		},
	})
}

// Tokens is deliberately silent: see the type comment.
func (r *acpRenderer) Tokens(int, int, time.Duration, bool) {}

// emit sends a session/update. A write failure is only logged: the Handle loop
// owns the wire, and a tool cannot abort the turn because the client closed
// its read side — the turn is over, the response will fail on its own.
func (r *acpRenderer) emit(sessionUpdate string, update map[string]any) {
	update["sessionUpdate"] = sessionUpdate
	n := NovaNotificacao("session/update", map[string]any{
		"sessionId": r.session,
		"update":    update,
	})
	// Missing fields must be omitted, not null: the ACP client validates the
	// update format.
	if err := r.a.notify(r.w, n); err != nil {
		r.a.logf("acp: failed to emit %s: %v", sessionUpdate, err)
	}
}
