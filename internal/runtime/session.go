// Package runtime implements a reusable conversation session. The transcript
// only commits a round after a valid response and, for tool_calls, after all
// results — errors/cancellations never leave orphaned calls.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/instructions"
	"github.com/ruanbarroso/barroso-harness/internal/pricing"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Session holds the transcript and reconfigurable state of a REPL.
type Session struct {
	config  config.Config
	dealer  *dealer.Cliente
	history []dealer.Mensagem
	est     budget.Estimador
	key     string // calibration per (normalized URL, model)

	// Persistence is OPTIONAL. A nil store means an in-memory-only session (the
	// NewSession path, used by -p mode and by tests). With a store, every
	// committed event of the turn goes into the log — and the session can be
	// resumed by Resume in a new process.
	store     *store.Store
	sessionID string

	// injected holds the specs of EXTERNAL subsystems (subagents, MCP), added
	// by the wiring through Injetar. They do not live in the static catalog
	// because those tools are owned by a process: see extras.go.
	//
	// muExtras only guards this field. The mutex is fine-grained on purpose —
	// the hot path (Send, building the turn's list) holds it only to copy the
	// slice, and never while talking to the provider.
	muExtras sync.Mutex
	injected []tools.Spec

	// inbox holds messages the user sent while a turn runs (steer). They are
	// delivered at the next safe boundary — after a tool round, before the next
	// request — in arrival order, without cancelling the turn. Messages still
	// in the inbox when the turn ends are the caller's (TakeInbox).
	muInbox sync.Mutex
	inbox   []Steer

	// day is the date the system prompt shows, fixed at the session's first
	// request: a prompt that changed at midnight would drop the provider's
	// prompt cache for the rest of a long session.
	day time.Time

	// baroMD is the BARROSO.md frozen into the system prompt at the first
	// request; later changes arrive as [system update] messages (updates.go).
	baroMD    *string
	resources *resourceState
	watchers  []Watcher

	// progressive and deferredCatalog: see disclosure.go.
	progressive     bool
	deferredCatalog *string

	// precos is the OPTIONAL price table (prices.json in the harness home).
	// Empty = no money cost is ever computed: tokens stay the metric and
	// nothing is invented. custoUSD/temPreco accumulate the turn's cost so the
	// renderer can show what the round actually cost.
	precos   pricing.Tabela
	muPreco  sync.Mutex
	custoUSD float64
	temPreco bool
}

// Steer is a message sent into a running turn.
type Steer struct {
	Text   string
	Images []string
}

// AddSteer queues a message for the running turn. Safe from any goroutine.
func (s *Session) AddSteer(m Steer) {
	s.muInbox.Lock()
	s.inbox = append(s.inbox, m)
	s.muInbox.Unlock()
}

// TakeInbox returns and clears the messages not delivered to a turn.
func (s *Session) TakeInbox() []Steer {
	s.muInbox.Lock()
	defer s.muInbox.Unlock()
	out := s.inbox
	s.inbox = nil
	return out
}

// NewSession creates an empty session over the given configuration.
func NewSession(cfg config.Config) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	return s
}

// NewSessionWithStore creates an ALREADY persistent session; the id only
// enters the log with the first event. It is the REPL path, which knows up
// front where the database is.
func NewSessionWithStore(cfg config.Config, st *store.Store, sessionID string) *Session {
	s := &Session{config: cfg}
	s.rebuild()
	s.Attach(st, sessionID)
	return s
}

// Attach binds the session to an existing store. It may be called after
// NewSession (when the database only opens later, e.g. a best-effort failure
// in the REPL); events of the next Send start being recorded.
func (s *Session) Attach(st *store.Store, sessionID string) {
	if st == nil || strings.TrimSpace(sessionID) == "" {
		return
	}
	s.store = st
	s.sessionID = sessionID
	// No indexing here: the session row is born with the first event
	// (StoreEvent) or with /rename. Opening and closing bh without talking
	// leaves no empty session in the database nor in the /resume list.
}

// ID returns the session id (empty when there is no persistence).
func (s *Session) ID() string { return s.sessionID }

// Persistente reports whether a log is recording (used by the CLI to show `sess`).
func (s *Session) Persistente() bool { return s.store != nil && s.sessionID != "" }

func (s *Session) Config() config.Config { return s.config }

// Configure updates provider/model/effort, clearing pending tool state (the
// full transcript only keeps committed turns; text stays compatible).
func (s *Session) Configure(cfg config.Config) {
	if s.config.Model != cfg.Model || s.config.CurrentProvider != cfg.CurrentProvider {
		// There are no pending tool calls thanks to Send's transaction; committed
		// textual history is valid across models and providers.
	}
	s.config = cfg
	s.rebuild()
}

func (s *Session) rebuild() {
	p := s.config.Ativo()
	base, key := "", ""
	if p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	id := dealer.NormalizarURL(base) + "\x00" + s.config.Model
	if id != s.key {
		s.est = budget.Estimador{}
		if s.config.BytesPorToken > 0 {
			s.est.CharsPorToken = float64(s.config.BytesPorToken)
		}
		s.key = id
	}
	// The configured protocol decides which dialect the request is written
	// in; chat_completions is the default for a provider that sets none.
	s.dealer = dealer.Novo(dealer.Config{
		BaseURL: base, APIKey: key, Model: s.config.Model,
		Protocol:                 s.config.ProtocoloAtivo(),
		MaxTokens:                s.config.MaxTokens,
		JanelaTokens:             s.config.Janela,
		Effort:                   s.config.Effort,
		DisableParallelToolCalls: s.config.ParallelToolCallsOff(),
		Idle:                     secs(s.config.IdleSecs),
	})
}

// History returns a copy for tests/safe observability; it does not expose the
// internal mutable slice.
func (s *Session) History() []dealer.Mensagem { return append([]dealer.Mensagem(nil), s.history...) }

// UltimoContexto returns the prompt tokens of the last request recorded in the
// transcript (0 if none): the context size of a resumed session, before any
// new turn.
func (s *Session) UltimoContexto() int {
	for i := len(s.history) - 1; i >= 0; i-- {
		if u := s.history[i].Usage; u != nil && u.PromptTokens > 0 {
			return u.PromptTokens
		}
	}
	return 0
}

// zerarCusto resets the turn's money cost. Each turn is accounted separately
// (like promptUsage/outputUsage), so the footer shows THIS turn's cost.
func (s *Session) zerarCusto() {
	s.muPreco.Lock()
	s.custoUSD = 0
	s.temPreco = false
	s.muPreco.Unlock()
}

// DefinirPrecos installs the optional price table. An empty table means bh
// never computes a money cost — tokens stay the only metric. Called by the
// wiring, which loads prices.json from the harness home.
func (s *Session) DefinirPrecos(t pricing.Tabela) {
	s.muPreco.Lock()
	s.precos = t
	s.muPreco.Unlock()
}

// addCost accumulates the cost of one round, when the active model has a
// declared price. Called from the turn loop with the round's usage.
func (s *Session) addCost(u *dealer.Usage) {
	if u == nil {
		return
	}
	s.muPreco.Lock()
	defer s.muPreco.Unlock()
	if s.precos.Empty() {
		return
	}
	priced, ok := s.precos.Price(s.config.Model)
	if !ok {
		return
	}
	// The cached part is priced separately when the provider reports it;
	// otherwise the whole prompt is billed at the full price.
	fresh := u.PromptTokens - u.CachedTokens
	if fresh < 0 {
		fresh = 0
	}
	c := priced.Calc(fresh, u.CachedTokens, u.OutputTokens)
	if !c.HasPrice {
		return
	}
	s.custoUSD += c.USD
	s.temPreco = true
}

// Custo returns the turn's accumulated cost in USD and whether any price
// applied. false means "no price declared", which is never a zero cost.
func (s *Session) Custo() (float64, bool) {
	s.muPreco.Lock()
	defer s.muPreco.Unlock()
	return s.custoUSD, s.temPreco
}

// ClearHistory clears the transcript. Used only when it makes sense to start a
// new conversation explicitly; it is not called when switching models.
//
// With persistence, the break also enters the log (session_reset): the log is
// append-only and cannot "undo" earlier events, but it must record that the
// conversation was cut — otherwise a Resume would rebuild a transcript the
// user thinks they discarded.
func (s *Session) ClearHistory() {
	s.history = nil
	s.record("session_reset", map[string]any{"role": "system", "content": "history cleared; new conversation"})
}

// StartFresh drops the transcript WITHOUT logging a session_reset. It is for
// /new, which attaches a new session id first: the previous session's log must
// stay exactly as it was so /resume brings it back whole.
func (s *Session) StartFresh() {
	s.history = nil
}

// registrarSaida stores native Responses output items with the assistant event
// so Resume can replay stateless reasoning/tool turns without exposing them in
// the visible transcript.
func registrarSaida(evento map[string]any, round *dealer.Rodada) {
	if round != nil && len(round.ResponsesOutput) > 0 {
		evento["responses_output"] = round.ResponsesOutput
	}
}

func mensagemResposta(round *dealer.Rodada) dealer.Mensagem {
	return dealer.Mensagem{
		Role:            "assistant",
		Conteudo:        round.Texto,
		Usage:           round.Usage,
		ResponsesOutput: round.ResponsesOutput,
	}
}

// Send runs a turn and keeps the transcript between calls. The user message
// and the rounds are appended to a private buffer and only committed on full
// success; YOLO tool execution already performed cannot be reverted, but the
// provider history is never left malformed.
func (s *Session) Send(ctx context.Context, r Renderer, cwd, prompt string) error {
	return s.SendWithImages(ctx, r, cwd, prompt, nil)
}

// SendWithImages is Send with image files attached to the user message (the
// full screen's pasted [Image N]). The paths go into the transcript and the
// log; the dealer inlines the bytes on each request, so the model must
// support vision.
func (s *Session) SendWithImages(ctx context.Context, r Renderer, cwd, prompt string, images []string) error {
	if r == nil {
		r = RendererDe(&strings.Builder{})
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt")
	}
	if !s.config.Pronta() {
		return fmt.Errorf("incomplete configuration (%s); run `bh` with no arguments to set it up", strings.Join(s.config.Faltas(), ", "))
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
	}
	start := time.Now()
	if s.baroMD == nil {
		b, err := instructions.Resolve(cwd)
		if err != nil {
			return err
		}
		s.baroMD = &b
	}
	baroMD := *s.baroMD
	// Prompt hooks (memory recall) run before anything is recorded: a blocked
	// prompt leaves no user_message and never reaches the model. Their
	// context travels in HookContext, apart from what the user typed: screens,
	// titles and ACP replay read Conteudo, only the request joins the two.
	if len(s.config.PromptHooks) > 0 && s.Persistente() {
		if rd, ok := r.(RendererDetalhado); ok {
			rd.Pensando()
		}
	}
	hooks := s.promptHooks(ctx, cwd, prompt)
	if hooks.Blocked {
		return hooks.err()
	}
	hookCtx := hooks.Context
	working := append([]dealer.Mensagem(nil), s.history...)
	working = append(working, dealer.Mensagem{Role: "user", Conteudo: prompt, Images: images, HookContext: hookCtx})
	// The turn is never thrown away. Whatever way Send exits from here on —
	// final answer, provider error after the retries, timeout, cancellation,
	// guardrail — the history keeps the user prompt and every round that
	// completed. working only ever grows by whole units (a valid answer, or a
	// tool_call batch together with all its results), so it is always a
	// well-formed transcript. Dropping it on failure (the old behavior) made
	// the next message reach the model without the request it was meant to
	// continue (conversation 92120, 2026-10-23: a 503 burst erased a turn
	// with 15 tool rounds, and "prossiga" got an answer to the turn before).
	// A failed attempt itself never enters working: retries re-send the same
	// list and only a valid response replaces anything.
	defer func() { s.history = working }()
	floor := 0
	// Consecutive cuts at the output limit, and the limit of the retry when the
	// cut came back empty (the model spent everything thinking).
	extraLimit := 0
	lastEmptyCutLimit := 0
	// Empty final answers: the round that ends the turn may come back with no
	// text and no tool calls (GLM free spends the whole budget on reasoning
	// and stops, measured 2026-10-22). Regen (re-request the SAME transcript,
	// nothing appended) up to the cap, then fail honestly. Measured in the
	// opencode runner: a stream dead BEFORE output re-runs the step
	// transparently (Retry); only interrupted/length flows get a synthetic
	// continuation note, and no harness appends a fake user message.
	regens := 0
	// Rounds re-asked because the call came written as text (see tooltext.go).
	toolTextRetries := 0
	reactiveDone := false
	compacted := false
	forcedCompaction := false
	// Whole-turn totals: each tool cycle spends request and response, and the
	// UI footer shows the total, not the last cycle's. Same for the money cost:
	// it restarts on every turn.
	promptUsage, outputUsage := 0, 0
	s.zerarCusto()

	// The user prompt enters the log BEFORE the loop: if the turn fails midway,
	// the log still records what was asked.
	userEvent := map[string]any{"role": "user", "content": prompt}
	if len(images) > 0 {
		userEvent["images"] = images
	}
	if hookCtx != "" {
		userEvent["hook_context"] = hookCtx
	}
	s.record("user_message", userEvent)

	// No cycle limit: the turn runs until the model answers with text or the
	// user interrupts it (Esc / Ctrl+C cancels ctx).
	var loop loopGuard
	for cycle := 0; ; cycle++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Catalog rebuilt on EVERY cycle: mcp_add, mcp_remove and skill_create
		// change the tools mid-turn, and the next round must already see them.
		// Built only once, a freshly connected tool stayed invisible until the
		// next prompt — the model saw the server in mcp_list and could not call
		// its tool.
		// Request boundary: changes to skills, BARROSO.md or MCP made since the
		// last request (here or in another process) join the history.
		if upd := s.pendingUpdate(cwd); upd != "" {
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: upd})
			s.record("user_message", map[string]any{"role": "user", "content": upd, "update": true})
			if ru, ok := r.(RendererUpdate); ok {
				ru.Atualizado(upd)
			}
		}
		direct, list, deferred := s.disclose(s.toolCatalog(cwd))
		schemas := toolSchemas(direct)
		if s.day.IsZero() {
			s.day = time.Now()
		}
		system := systemPrompt(cwd, baroMD, hasTool(list, "subagent"), s.day, deferred)

		// Measured without the image bytes: see dealer.CorpoEstimativa.
		body, err := s.dealer.CorpoEstimativa(system, working, schemas)
		if err != nil {
			return err
		}
		ev := budget.Avaliar(string(body), s.config.Janela, s.est)
		if !ev.Cabe {
			pruned := prune(working, &floor, s.est.Bytes(ev.TokensEntrada+ev.TokensSaida-s.config.Janela))
			if pruned == nil && !forcedCompaction {
				// Pruning only stubs tool results; a history whose weight is
				// in tool_call arguments, or one rehydrated in full by Resume
				// (the in-memory stubs never reach the log), cannot be saved
				// by it (conversation 100135, 2026-10-25: 324k real tokens in
				// a 200k window after a process restart). Last resort: replace
				// everything before the current request with a mechanical
				// state, persisted so Resume re-applies it.
				if state := forcedState(working); state != "" {
					forcedCompaction = true
					before := historyBytes(working)
					working = compactHistory(working, state)
					floor = 0
					compacted = true
					r.Guardrail(true)
					s.record(eventCompaction, map[string]any{
						"politica":     policy(s.config.PoliticaContexto),
						"tipo":         "forcada",
						"ciclo":        cycle,
						"bytes_antes":  before,
						"bytes_depois": historyBytes(working),
						"estado_bytes": len(state),
						"estado":       state,
					})
					continue
				}
			}
			if pruned == nil {
				return fmt.Errorf("guardrail: request does not fit (%s) and pruning did not free enough", ev.Veredito())
			}
			r.Guardrail(true)
			// Persisted kind/keys/values stay in their original (Portuguese)
			// form: sessions.db readers (cli context stats) match on them.
			s.record("guardrail", map[string]any{
				"tipo":       "proativa",
				"politica":   policy(s.config.PoliticaContexto),
				"veredito":   ev.Veredito(),
				"janela":     s.config.Janela,
				"estimativa": ev.TokensEntrada + ev.TokensSaida,
			})
			compacted = true
			working = pruned
			continue
		}

		if rd, ok := r.(RendererDetalhado); ok {
			rd.Pensando()
		}
		cli := s.dealer
		// The conversation identity rides along: the canonical ses_ id
		// mints once and stays stable for the whole session (one-shot
		// traffic outside a store keeps a per-call fresh id).
		cli = cli.ComConversa(s.sessionID)
		limit := cli.MaxTokens()
		if extraLimit > 0 {
			limit = extraLimit
		}
		if s.config.Janela > 0 {
			// The limit cannot exceed what is left in the window (as OpenCode and
			// pi do), with a floor so the model can still answer.
			limit = max(1024, min(limit, s.config.Janela-ev.TokensEntrada))
		}
		if limit != cli.MaxTokens() {
			cli = cli.ComTeto(limit)
		}
		if rr, ok := r.(RendererRetry); ok {
			cli = cli.ComAviso(rr.Retentando)
		}
		round, err := cli.RodarContext(ctx, system, working, schemas)
		if err != nil {
			if contextOverflow(err) && s.config.Janela > 0 && !reactiveDone {
				reactiveDone = true
				r.Guardrail(false)
				s.record("guardrail", map[string]any{
					"tipo":     "reativa",
					"politica": policy(s.config.PoliticaContexto),
					"motivo":   oneLine(err.Error()),
				})
				pruned := prune(working, &floor, s.est.Bytes(budget.ReservaOperacional))
				if pruned != nil {
					compacted = true
					working = pruned
					continue
				}
			}
			return err
		}
		if rr, ok := r.(RendererRaciocinio); ok && strings.TrimSpace(round.Raciocinio) != "" {
			rr.Pensou(round.Raciocinio, round.DurRaciocinio)
		}
		if round.Usage != nil {
			if rd, ok := r.(RendererDetalhado); ok {
				rd.Contexto(round.Usage.PromptTokens)
			}
			if rc, ok := r.(RendererCache); ok {
				rc.Cache(round.Usage.CachedTokens, round.Usage.PromptTokens)
			}
			// One metrics event per request: what bh bench and the context
			// stats read to measure cache hit rate and latency.
			s.record("round", map[string]any{
				"cycle":         cycle,
				"prompt_tokens": round.Usage.PromptTokens,
				"cached_tokens": round.Usage.CachedTokens,
				"output_tokens": round.Usage.OutputTokens,
				"ttft_ms":       round.TTFT.Milliseconds(),
				"duration_ms":   round.Dur.Milliseconds(),
				"tools_bytes":   jsonLen(schemas),
				"system_bytes":  len(system),
				"tool_calls":    len(round.Tools),
			})
			s.est.Calibrar(len(body), round.Usage.PromptTokens)
			if round.Usage.PromptTokens > 0 {
				promptUsage += round.Usage.PromptTokens
			}
			outputUsage += round.Usage.OutputTokens
			s.addCost(round.Usage)
			if rc, ok := r.(RendererCusto); ok {
				if usd, tem := s.Custo(); tem {
					rc.Custo(usd)
				}
			}
		}
		if round.Cortada {
			// A response cut at the output limit is never a final answer: no
			// harness studied (OpenCode, Codex, Claude Code, Gemini CLI, dsh, pi,
			// Crush, Aider) silently ends the turn in that case.
			// No give-up count: the turn keeps continuing until the model
			// finishes or the user interrupts.
			rc, _ := r.(RendererCorte)
			if rc != nil {
				rc.Cortada(limit, false)
			}
			if strings.TrimSpace(round.Texto) == "" && len(round.Tools) == 0 {
				// Only thought: retry with twice the limit. If the context-window
				// clamp prevents an increase, another request would be identical.
				if limit <= lastEmptyCutLimit {
					return fmt.Errorf("provider exhausted the output limit (%d tokens) without visible text, and the limit cannot increase within the configured context window; lower reasoning effort or adjust the context/output limits", limit)
				}
				lastEmptyCutLimit = limit
				extraLimit = limit * 2
				continue
			}
			// Half-written text (or tool call): the text stays, the tools do not
			// run (their arguments may be truncated) and the model is told to
			// continue in smaller pieces.
			if round.Texto != "" || len(round.ResponsesOutput) > 0 {
				if round.Texto != "" {
					r.Resposta(round.Texto)
				}
				working = append(working, mensagemResposta(round))
				evento := map[string]any{"role": "assistant", "content": round.Texto}
				registrarSaida(evento, round)
				s.record("assistant", evento)
			}
			notice := cutNotice
			if len(round.Tools) > 0 {
				notice += " " + cutNoticeTools
			}
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: notice})
			s.record("user_message", map[string]any{"role": "user", "content": notice})
			continue
		}
		extraLimit = 0
		if len(round.Tools) > 0 && len(list) > 0 && looksLikeToolText(round.Texto) {
			// Some gateways return a malformed tool-call envelope in content
			// alongside valid structured calls. It must not be shown as an
			// intermediate assistant message, but the actual tool calls remain
			// authoritative and still execute below.
			s.record("tool_texto", map[string]any{
				"cycle":     cycle,
				"fim":       round.Fim,
				"bytes":     len(round.Texto),
				"com_tools": true,
				"tentativa": toolTextRetries,
			})
			round.Texto = ""
		}
		if len(round.Tools) == 0 {
			if strings.TrimSpace(round.Texto) == "" {
				// An empty final round is never an answer: the provider
				// ended the round without visible content and without tool
				// calls (truncated reasoning, empty content). Regen
				// transparently — re-request the SAME transcript (nothing is
				// appended; the sticky prefix cache survives), never deliver
				// an empty answer. A cut stream (no finish reason) and a
				// plain stop both regen; a length finish is handled above.
				if regens < maxRegensVazia {
					regens++
					gastados := 0
					if round.Usage != nil {
						gastados = round.Usage.OutputTokens
					}
					s.record("regem_vazia", map[string]any{
						"cycle":         cycle,
						"fim":           round.Fim,
						"output_tokens": gastados,
						"tentativa":     regens,
					})
					continue
				}
				return fmt.Errorf("the provider returned an empty answer after %d attempts (finish_reason %q); no text and no tool calls — try lower reasoning effort or repeat the request", regens+1, round.Fim)
			}
			if len(list) > 0 && toolTextRetries < maxToolTextRetries && looksLikeToolText(round.Texto) {
				// A call written as text is never an answer: keep the text in the
				// transcript (the model sees what it did), tell it nothing ran and
				// ask for the real call. Not shown as the answer.
				toolTextRetries++
				s.record("tool_texto", map[string]any{
					"cycle":     cycle,
					"fim":       round.Fim,
					"bytes":     len(round.Texto),
					"tentativa": toolTextRetries,
				})
				working = append(working,
					mensagemResposta(round),
					dealer.Mensagem{Role: "user", Conteudo: toolTextNotice})
				evento := map[string]any{"role": "assistant", "content": round.Texto}
				registrarSaida(evento, round)
				s.record("assistant", evento)
				s.record("user_message", map[string]any{"role": "user", "content": toolTextNotice})
				continue
			}
			r.Resposta(round.Texto)
			r.Tokens(promptUsage, outputUsage, time.Since(start), compacted)
			working = append(working, mensagemResposta(round))
			s.history = working
			// Committed text enters the log AFTER the transcript commit: a failed
			// round leaves no trace of a response that never existed.
			assistant := map[string]any{"role": "assistant", "content": round.Texto}
			registrarSaida(assistant, round)
			if round.Usage != nil {
				assistant["usage"] = map[string]any{
					"prompt_tokens":    round.Usage.PromptTokens,
					"output_tokens":    round.Usage.OutputTokens,
					"cached_tokens":    round.Usage.CachedTokens,
					"reasoning_tokens": round.Usage.ReasoningTokens,
				}
			}
			s.record("assistant", assistant)
			return nil
		}

		// Build the assistant tool-call + results in a separate transaction; only
		// add them to working when every execution has finished.
		turn := make([]dealer.Mensagem, 0, len(round.Tools)+1)
		if round.Texto != "" || len(round.ResponsesOutput) > 0 {
			// Text that comes with tool calls is an intermediate message ("let me
			// check X"): renderers that show progress get it before the tools run.
			if rn, ok := r.(RendererNota); ok && strings.TrimSpace(round.Texto) != "" {
				rn.Nota(round.Texto)
			}
			turn = append(turn, mensagemResposta(round))
			// The text enters the log with its calls: without it, session/load and
			// /resume rebuild a history of silent tool rounds, and the model copies
			// that pattern and stops speaking between tools (Bruce, 2026-10-23).
			evento := map[string]any{"role": "assistant", "content": round.Texto}
			registrarSaida(evento, round)
			s.record("assistant", evento)
		}
		results := make([]dealer.Mensagem, 0, len(round.Tools))
		for _, call := range round.Tools {
			turn = append(turn, dealer.Mensagem{Role: "tool_call", ToolID: call.ID, ToolName: call.Nome, ToolArgs: call.Args})
			// The call enters the log here, before executing: if execution hangs
			// or blows up, the log still shows what was requested and with which
			// arguments.
			s.record("tool_call", map[string]any{
				"role":      "tool_call",
				"tool_id":   call.ID,
				"tool_name": call.Nome,
				"tool_args": call.Args,
			})
		}
		for _, out := range s.runRound(ctx, r, list, cwd, round.Tools) {
			results = append(results, dealer.Mensagem{Role: "tool_result", Conteudo: out.output, ToolID: out.call.ID, ToolName: out.call.Nome, IsErro: out.isErr})
		}
		working = append(working, turn...)
		working = append(working, results...)
		// Images opened with read reach the model as images: the tool result
		// only names the file, and a user message right after the round's
		// results carries it (tool messages cannot hold images).
		if imgs := readImages(results); len(imgs) > 0 {
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: readImagesNote, Images: imgs})
			s.record("user_message", map[string]any{"role": "user", "content": readImagesNote, "images": imgs})
		}
		if notice := loop.observe(round.Tools); notice != "" {
			// A nudge, not a stop: the turn keeps going, but the model is told it
			// is repeating itself (Gemini CLI stops at 5 identical calls in a row;
			// BH prefers to keep working and steer).
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: notice})
			s.record("user_message", map[string]any{"role": "user", "content": notice})
		}
		if state := compactedState(round.Tools, results); state != "" {
			before := historyBytes(working)
			working = compactHistory(working, state)
			floor = 0 // the history changed shape; pruning restarts from the beginning
			compacted = true
			r.Guardrail(true)
			// Kind and keys kept as originally persisted (see persist.go).
			s.record(eventCompaction, map[string]any{
				"politica":     PoliticaAgente,
				"ciclo":        cycle,
				"bytes_antes":  before,
				"bytes_depois": historyBytes(working),
				"estado_bytes": len(state),
				"estado":       state,
			})
		}
		// Safe boundary: the round's calls and results are complete, the next
		// request has not gone out. Steer messages join the conversation here.
		for _, m := range s.TakeInbox() {
			working = append(working, dealer.Mensagem{Role: "user", Conteudo: m.Text, Images: m.Images})
			ev := map[string]any{"role": "user", "content": m.Text, "steer": true}
			if len(m.Images) > 0 {
				ev["images"] = m.Images
			}
			s.record("user_message", ev)
			if rs, ok := r.(RendererSteer); ok {
				rs.Entregue(m.Text)
			}
		}
	}
}

func toolSchemas(list []tools.Spec) []map[string]any {
	schemas := make([]map[string]any, 0, len(list))
	for _, s := range list {
		schemas = append(schemas, map[string]any{
			"type":     "function",
			"function": map[string]any{"name": s.Name, "description": s.Description, "parameters": s.Schema},
		})
	}
	return schemas
}

// readImages returns the image paths that read results in the round asked to
// attach (tools.ImageAttached).
func readImages(results []dealer.Mensagem) []string {
	var paths []string
	for _, m := range results {
		if m.IsErro || m.ToolName != "read" || !strings.HasPrefix(m.Conteudo, tools.ImageAttached) {
			continue
		}
		rest := strings.TrimPrefix(m.Conteudo, tools.ImageAttached)
		if end := strings.Index(rest, "]\n"); end > 0 {
			paths = append(paths, rest[:end])
		}
	}
	return paths
}

// toolOutcome is one executed call of a round.
type toolOutcome struct {
	call   dealer.ToolChamada
	output string
	isErr  bool
	dur    time.Duration
}

// sequentialTools mutate the catalog or the session itself; a round that
// contains any of them runs one call at a time, in order.
var sequentialTools = map[string]bool{
	"mcp_add":             true,
	"mcp_remove":          true,
	"mcp_edit":            true,
	"skill_edit":          true,
	"instructions_write":  true,
	"instructions_edit":   true,
	"instructions_delete": true,
	"skill_create":        true,
	"skill_remove":        true,
	"context_compact":     true,
	"context_write":       true,
	"context_append":      true,
	"subagent":            true,
}

// runRound executes the round's tool calls and returns their outcomes in the
// ORIGINAL order. Independent calls run concurrently; the renderer and the log
// (recordResult) are only touched from this goroutine, in call order: every
// ToolInicio is announced up front, and each call's recordResult/Tool/Eco is
// emitted as soon as it and all earlier calls are done. Cancellation reaches
// the tools through ctx; waiting for every goroutine keeps the transcript free
// of calls without results.
func (s *Session) runRound(ctx context.Context, r Renderer, list []tools.Spec, cwd string, calls []dealer.ToolChamada) []toolOutcome {
	outcomes := make([]toolOutcome, len(calls))
	av, live := r.(RendererAoVivo)
	liveID := func(i int) string { return strconv.Itoa(i) + "/" + calls[i].ID }
	run := func(i int) {
		call := calls[i]
		start := time.Now()
		cctx := ctx
		if live {
			id := liveID(i)
			cctx = tools.WithOutput(ctx, func(chunk string) { av.ToolSaida(id, chunk) })
		}
		output, execErr := runTool(cctx, list, cwd, call)
		o := toolOutcome{call: call, output: output, dur: time.Since(start)}
		if execErr != nil {
			o.isErr = true
			o.output = "error: " + execErr.Error()
		}
		outcomes[i] = o
	}
	report := func(i int) {
		o := outcomes[i]
		// Outputs above blobThreshold become blob + preview.
		s.recordResult(o.call.ID, o.call.Nome, o.output, o.isErr)
		if live {
			name, args := shownCall(o.call.Nome, o.call.Args)
			av.ToolTerminou(liveID(i), name, args, o.output, o.dur, o.isErr)
			return
		}
		if rr, ok := r.(RendererResultado); ok {
			name, args := shownCall(o.call.Nome, o.call.Args)
			rr.ToolResultado(name, args, o.output, o.dur, o.isErr)
			return
		}
		name, _ := shownCall(o.call.Nome, o.call.Args)
		r.Tool(name, o.dur, o.isErr)
		r.Eco(echoExcerpt(o.output))
	}
	rd, detailed := r.(RendererDetalhado)
	announce := func(i int) {
		name, args := shownCall(calls[i].Nome, calls[i].Args)
		switch {
		case live:
			av.ToolComecou(liveID(i), name, args)
		case detailed:
			rd.ToolInicio(name, args)
		}
	}

	parallel := len(calls) > 1
	for _, c := range calls {
		if name, _ := shownCall(c.Nome, c.Args); sequentialTools[name] {
			parallel = false
			break
		}
	}
	if !parallel {
		for i := range calls {
			announce(i)
			run(i)
			report(i)
		}
		return outcomes
	}

	done := make([]chan struct{}, len(calls))
	var wg sync.WaitGroup
	for i := range calls {
		announce(i)
		done[i] = make(chan struct{})
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer close(done[i])
			run(i)
		}(i)
	}
	for i := range calls {
		<-done[i]
		report(i)
	}
	wg.Wait()
	return outcomes
}

func hasTool(list []tools.Spec, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

// systemPrompt builds the turn's system message: identity, real environment
// facts, tool policy and working style, then BARROSO.md.
func systemPrompt(cwd, baroMD string, canSpawn bool, now time.Time, deferred string) string {
	// Order matters for the provider's prompt cache, which matches the request
	// prefix byte for byte: the text that never changes goes first, BARROSO.md
	// next, and the environment (cwd, date) last. The date is the session's
	// start date (see Session.day), not the clock of each request, so the
	// prefix does not change when midnight passes mid-session.
	var sb strings.Builder
	sb.WriteString("You are the Barroso Harness agent, a coding agent running locally on the user's machine. " +
		"Your tools execute immediately, without asking for approval (YOLO mode): act decisively, but think before destructive actions. " +
		"You help with software engineering tasks: reading and changing code, running builds and tests, using git, investigating problems.\n")

	sb.WriteString("\n## Tool policy\n" +
		"- Find things with grep (file contents) and glob (file names). Do not search through shell (findstr, git grep, dir /s, find).\n" +
		"- Read files with read; read whole files when reasonably sized rather than many small windows.\n" +
		"- Change existing files with edit (exact string replacement of only the changed region). Use write only for new files or full rewrites.\n" +
		"- When the SAME change touches SEVERAL files (a rename, a signature change, a repeated import), use edit_many: one call with every edit. " +
		"It validates all of them against the current content and writes NOTHING if any edit fails. Use edit for a single file.\n" +
		"- Use shell for builds, tests, git, package managers and running programs.\n" +
		"- PARALLEL CALLS: when several tool calls are independent (reading several files, several searches, a build plus a git status), " +
		"ISSUE THEM ALL IN THE SAME RESPONSE: they run in parallel. Only chain calls across responses when one needs the result of another.\n" +
		"- If a call fails, read the error and adapt. Never repeat a failing command unchanged.\n" +
		"- Avoid exploratory loops: decide what you need, gather it in as few rounds as possible, then act.\n")
	if canSpawn {
		sb.WriteString("- subagent is available: use it for independent work that can proceed in parallel, at most once per turn, and end your turn right after spawning.\n")
	}

	sb.WriteString("\n## Working style\n" +
		"- Be direct. Do the task end to end instead of describing what you would do; ask the user only when truly blocked or when the request is ambiguous in a way that matters.\n" +
		"- Follow the conventions of the existing code (style, libraries, structure) and keep changes focused on the request.\n" +
		"- After changing code, verify it: build and run the relevant tests, and fix what you broke.\n" +
		"- Use tools to act and TEXT to talk to the user. When the task is done, reply with a short plain-text answer: what changed and what the user must know. No long recaps.\n" +
		"- Messages starting with [system update] come from the harness, not the user: they announce changes to instructions, skills or tools during the session. The latest update wins over anything earlier, including this prompt's BARROSO.md section.\n")

	if deferred != "" {
		sb.WriteString("\n## Deferred tools\n" +
			"These tools are not in your function list. Get a tool's full parameter schema with tool_search, then run it with tool_call " +
			"(skip the search when the line below is enough to fill its arguments). Tools added later are announced by [system update].\n" + deferred)
	}

	if baroMD != "" {
		sb.WriteString("\n## BARROSO.md (system instructions)\n" + baroMD + "\n")
	}

	sb.WriteString("\n## Environment\n")
	osName := goruntime.GOOS
	switch osName {
	case "windows":
		osName = "Windows"
	case "darwin":
		osName = "macOS"
	case "linux":
		osName = "Linux"
	}
	fmt.Fprintf(&sb, "- OS: %s (%s/%s)\n", osName, goruntime.GOOS, goruntime.GOARCH)
	fmt.Fprintf(&sb, "- Shell used by shell: %s\n", tools.ShellName())
	if goruntime.GOOS == "windows" {
		sb.WriteString("- This is Windows: do not use bash/POSIX syntax or tools (grep, sed, ls, cat, rm -rf) in shell. " +
			"File tools accept both backslashes and forward slashes in paths.\n")
	}
	sb.WriteString(tools.ProtectedNote())
	fmt.Fprintf(&sb, "- Working directory (cwd): %s\n", cwd)
	fmt.Fprintf(&sb, "- Session start date: %s\n", now.Format("2006-01-02"))
	return sb.String()
}

func runTool(ctx context.Context, list []tools.Spec, cwd string, call dealer.ToolChamada) (string, error) {
	for _, s := range list {
		if s.Name == call.Nome {
			args := json.RawMessage(call.Args)
			if t := strings.TrimSpace(call.Args); t == "" || t == "null" {
				args = json.RawMessage("{}")
			}
			if miss := missingRequired(s.Schema, args); len(miss) > 0 {
				return "", missingArgsErr(call.Nome, miss, call.Args)
			}
			return s.Exec(ctx, cwd, args)
		}
	}
	return "", fmt.Errorf("unknown tool: %q (check the exact name)", call.Nome)
}

// missingArgsErr explains a call that arrived without its required
// arguments. Some gateways drop the arguments of parallel calls, so the model
// is told what reached the harness and how to retry, instead of a tool
// error ("empty command") that reads as its own mistake.
func missingArgsErr(name string, miss []string, raw string) error {
	got := strings.TrimSpace(raw)
	if got == "" {
		got = "(empty)"
	} else if len(got) > 200 {
		got = got[:200] + "…"
	}
	return fmt.Errorf("%s needs %s, but the call arrived with arguments %s. If you did send them, they were lost on the way (some gateways drop the arguments of parallel calls): call %s again, alone in its response",
		name, strings.Join(miss, ", "), got, name)
}

// Synthetic continuation messages after a cut. The "[output limit]" prefix
// lets the screen recognize them and not draw them as user speech.
const (
	cutNotice      = "[output limit] Your last response hit the output token limit and was cut off. Continue exactly where you stopped, without repeating what you already wrote, and break the remaining work into smaller pieces."
	cutNoticeTools = "Tool calls in the cut response were NOT executed because their arguments may be incomplete; call them again, with smaller arguments if needed."

	// maxRegensVazia bounds the transparent regen of an empty final round
	// (thinking model burnt the budget and answered nothing, measured GLM
	// free via the zen lane): the same transcript is re-requested up to this
	// many times before the turn fails honestly.
	maxRegensVazia = 2
)

// jsonLen is the serialized size of v (0 when it does not serialize).
func jsonLen(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// secs turns an optional seconds setting into the dealer's override.
func secs(v *int) *time.Duration {
	if v == nil {
		return nil
	}
	d := time.Duration(*v) * time.Second
	return &d
}
