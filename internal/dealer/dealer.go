// Package dealer supports the OpenAI Responses API as the runtime's default,
// with Chat Completions as a route-unavailable fallback, and exposes the
// /models catalog. An omitted Protocol prefers Responses and falls back to
// Chat only when the endpoint is unavailable. The runtime builds the transcript;
// the dealer translates both ways.
package dealer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// The User-Agent is the official client's (zen.go, UserAgent()): the
// measured contract of the opencode client, applied to every request and
// every gateway the harness talks to.

// Request protocols. An empty preference defaults to Responses; Chat
// Completions is used only when the Responses route is unavailable.
const (
	ProtocoloChat      = "chat_completions"
	ProtocoloResponses = "responses"
)

// sseMaxLinha caps one SSE line. A Responses terminal event echoes the whole
// response object (instructions included), so the old 4 MiB was too small.
const sseMaxLinha = 64 << 20

// normalizarProtocolo maps what a config/person wrote to a canonical
// protocol. ok=false means unknown (never guessed or silently replaced).
func normalizarProtocolo(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.NewReplacer("-", "_", " ", "_").Replace(v)
	switch v {
	case "":
		return ProtocoloResponses, true
	case "chat", "chat_completions", "completions":
		return ProtocoloChat, true
	case "responses":
		return ProtocoloResponses, true
	}
	return v, false
}

// Protocolo is the canonical request protocol of the client (an unknown
// value is returned lowercased and fails when the body is built).
func (c *Cliente) Protocolo() string {
	p, _ := normalizarProtocolo(c.cfg.Protocol)
	return p
}

// Config is the provider configuration. The key comes from the local config
// persisted by the CLI.
type Config struct {
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKey       string `json:"-"`
	MaxTokens    int    `json:"max_tokens"`     // output cap; 0 = per-effort default (TetoPadrao)
	JanelaTokens int    `json:"context_window"` // 0 = unknown
	Effort       string `json:"effort"`         // empty = omit reasoning_effort from the request
	// Protocol selects chat_completions (forced) or responses (preferred,
	// with Chat fallback when the Responses endpoint is unavailable). Empty
	// uses the Responses preference.
	Protocol string `json:"protocol,omitempty"`
	// DisableParallelToolCalls sends parallel_tool_calls=false: one tool call
	// per response, for gateways that drop arguments in parallel batches.
	DisableParallelToolCalls bool `json:"disable_parallel_tool_calls,omitempty"`
	// Response deadline override: nil = package default, ≤0 = disabled.
	Idle *time.Duration `json:"-"`
}

// Usage is the provider's token usage.
type Usage struct {
	PromptTokens int `json:"prompt_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CachedTokens is the part of PromptTokens the provider served from its
	// prompt cache (prompt_tokens_details.cached_tokens); 0 when it reports
	// none.
	CachedTokens int `json:"cached_tokens,omitempty"`
	// ReasoningTokens is the output-token subset spent on reasoning.
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// Mensagem is the internal protocol (serializable, byte-deterministic — it
// holds no maps). Roles: user, assistant, tool_call, tool_result.
type Mensagem struct {
	Role string `json:"role"`
	// ResponsesOutput retains exact output items needed for stateless replay.
	ResponsesOutput []json.RawMessage `json:"responses_output,omitempty"`
	Conteudo        string            `json:"conteudo,omitempty"`
	ToolID          string            `json:"tool_id,omitempty"`
	ToolName        string            `json:"tool_name,omitempty"`
	ToolArgs        string            `json:"tool_args,omitempty"`
	IsErro          bool              `json:"is_erro,omitempty"`
	Usage           *Usage            `json:"usage,omitempty"`
	// Images are image file paths attached to a user message (pasted into the
	// full screen as [Image N]). They are read and inlined as base64 data URLs
	// on every request, so the model needs vision support.
	Images []string `json:"images,omitempty"`
	// HookContext is what prompt hooks added to a user message. Conteudo stays
	// what the user typed (screens, titles and ACP replay read it); only the
	// request to the model joins the two (see TextoModelo).
	HookContext string `json:"hook_context,omitempty"`
}

// TextoModelo is the text of a message as the model receives it: Conteudo,
// plus the hook context in a <hook-context> block. The context is escaped so
// it cannot close the block and pass as something the user typed.
func (m Mensagem) TextoModelo() string {
	if m.HookContext == "" {
		return m.Conteudo
	}
	esc := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(m.HookContext)
	return m.Conteudo + "\n\n<hook-context>\n" + esc + "\n</hook-context>"
}

// ToolChamada is a tool call emitted by the model in a round.
type ToolChamada struct {
	ID   string `json:"id"`
	Nome string `json:"nome"`
	Args string `json:"args"` // raw input JSON
}

// Rodada is the result of one call to the model.
type Rodada struct {
	Texto string
	Tools []ToolChamada
	Usage *Usage
	// ResponsesOutput preserves provider output items for store:false replay.
	ResponsesOutput []json.RawMessage
	FimPorTexto     bool
	// Cortada: the provider stopped at the output cap (finish_reason
	// "length"). Text and tool calls may be incomplete.
	Cortada bool
	// Fim is the raw finish_reason of the round ("stop", "length",
	// "tool_calls", ...). Empty when the stream ended without one (cut
	// stream) — the runtime uses it to tell an honest empty answer from a
	// stream that died mid-think.
	Fim string
	// ProtocolFallback reports that this round succeeded after the unavailable
	// Responses endpoint caused a fallback to Chat Completions.
	ProtocolFallback bool
	// Raciocinio is the reasoning the provider exposed (reasoning or
	// reasoning_content in the delta) and DurRaciocinio the time from the
	// start of the request to its last chunk. Display only; never sent back
	// to the model.
	Raciocinio    string
	DurRaciocinio time.Duration
	// TTFT is the time from the request to the first streamed chunk, and Dur
	// the time to the end of the stream.
	TTFT time.Duration
	Dur  time.Duration
}

// Cliente is instantiated per turn.
type Cliente struct {
	cfg           Config
	maxOut        int            // output cap for this request (ComTeto); 0 = MaxTokens()
	aviso         AvisoRetry     // reported before each retry (ComAviso); nil = silent
	protocolState *protocolState // shared by request-scoped copies; caches a missing Responses route
	// conversa is the conversation identity the canonical opencode session
	// id (ses_) mints for and stays stable against. Empty = one-shot
	// traffic: a fresh id per request.
	conversa string
}

type protocolState struct {
	chatFallback atomic.Bool
}

// Novo returns a client for cfg. Copies made for one request share the
// endpoint-capability result, so an unavailable Responses route is learned
// once and subsequent rounds go straight to Chat Completions.
func Novo(cfg Config) *Cliente { return &Cliente{cfg: cfg, protocolState: &protocolState{}} }

// ComConversa returns a copy of the client that carries the conversation
// identity the canonical session headers build on.
func (c *Cliente) ComConversa(conversa string) *Cliente {
	c2 := *c
	c2.conversa = conversa
	return &c2
}

// MaxTokens is the output cap requested from the provider: the config's
// max_tokens, or TetoPadrao for the effort.
func (c *Cliente) MaxTokens() int {
	if c.maxOut > 0 {
		return c.maxOut
	}
	if c.cfg.MaxTokens > 0 {
		return c.cfg.MaxTokens
	}
	return TetoPadrao(c.cfg.Effort)
}

// ComTeto returns a copy of the client with a different output cap for its
// subsequent requests only (retry with a larger cap, trimming to the window).
func (c *Cliente) ComTeto(n int) *Cliente {
	c2 := *c
	c2.maxOut = n
	return &c2
}

// TetoPadrao: 32k, as OpenCode and Claude Code do for models with no known
// limit, plus the effort's reasoning budget — a high-effort model used to
// spend the old 4096 entirely on thinking and answer nothing.
func TetoPadrao(effort string) int {
	return max(32<<10, OrcamentoRaciocinio(effort)+(16<<10))
}

// OrcamentoRaciocinio is how much reasoning consumes per effort (the same
// table as the barroso-keys clamp).
func OrcamentoRaciocinio(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "max", "xhigh":
		return 32 << 10
	case "high":
		return 16 << 10
	case "medium":
		return 8 << 10
	case "low", "minimal":
		return 4 << 10
	}
	return 0
}

type oaiCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMsg struct {
	Role       string    `json:"role"`
	Content    any       `json:"content,omitempty"` // *string, or []contentPart for user images
	ToolCallID string    `json:"tool_call_id,omitempty"`
	ToolCalls  []oaiCall `json:"tool_calls,omitempty"`
	// ResponsesOutput is never sent on Chat; the Responses body replays it.
	ResponsesOutput []json.RawMessage `json:"-"`
}

func strNil(s string) *string { return &s }

// Corpo serializes the full chat/completions request. Public so the runtime
// can measure bytes (the bytes/tokens pair feeds calibration).
func (c *Cliente) Corpo(system string, msgs []Mensagem, tools []map[string]any) ([]byte, error) {
	return c.corpo(system, msgs, tools, true)
}

// CorpoEstimativa is Corpo for the context budget: each attached image is
// replaced by a fixed-size stand-in instead of its base64 bytes. A screenshot
// is hundreds of KB of base64, which the chars-per-token estimate would read
// as a full context window, while providers bill an image at roughly a
// thousand tokens.
func (c *Cliente) CorpoEstimativa(system string, msgs []Mensagem, tools []map[string]any) ([]byte, error) {
	return c.corpo(system, msgs, tools, false)
}

// imageEstimateStandIn stands for one image in CorpoEstimativa (~1k tokens
// at 4 chars/token).
var imageEstimateStandIn = strings.Repeat("A", 4<<10)

// corpo dispatches on the configured protocol or the cached endpoint capability.
// Unknown protocol values fail; fallback is handled only after a route error.
func (c *Cliente) corpo(system string, msgs []Mensagem, tools []map[string]any, inline bool) ([]byte, error) {
	p, ok := normalizarProtocolo(c.cfg.Protocol)
	if !ok {
		return nil, fmt.Errorf("unknown protocol %q (use %s or %s)", strings.TrimSpace(c.cfg.Protocol), ProtocoloChat, ProtocoloResponses)
	}
	if p == ProtocoloResponses && c.protocolState != nil && c.protocolState.chatFallback.Load() {
		p = ProtocoloChat
	}
	return c.corpoProtocolo(p, system, msgs, tools, inline)
}

func (c *Cliente) corpoProtocolo(protocol, system string, msgs []Mensagem, tools []map[string]any, inline bool) ([]byte, error) {
	if protocol == ProtocoloResponses {
		return c.corpoResponses(system, msgs, tools, inline)
	}
	if protocol == ProtocoloChat {
		return c.corpoChat(system, msgs, tools, inline)
	}
	return nil, fmt.Errorf("unknown protocol %q (use %s or %s)", protocol, ProtocoloChat, ProtocoloResponses)
}

// converterHistorico turns the internal transcript into Chat-compatible
// messages (no system message, no repair): both protocols build on it.
func converterHistorico(msgs []Mensagem, inline bool) ([]oaiMsg, error) {
	var list []oaiMsg
	for _, m := range msgs {
		switch m.Role {
		case "user":
			if len(m.Images) > 0 {
				list = append(list, oaiMsg{Role: "user", Content: userParts(m.TextoModelo(), m.Images, inline)})
				continue
			}
			list = append(list, oaiMsg{Role: "user", Content: strNil(m.TextoModelo())})
		case "assistant", "tool_call":
			a := oaiMsg{Role: "assistant", Content: strNil(m.Conteudo), ResponsesOutput: m.ResponsesOutput}
			if m.ToolID != "" {
				var call oaiCall
				call.ID = m.ToolID
				call.Type = "function"
				call.Function.Name = m.ToolName
				call.Function.Arguments = m.ToolArgs
				a.ToolCalls = append(a.ToolCalls, call)
			}
			list = append(list, a)
		case "tool_result":
			list = append(list, oaiMsg{Role: "tool", Content: strNil(m.Conteudo), ToolCallID: m.ToolID})
		default:
			return nil, fmt.Errorf("unknown role: %q", m.Role)
		}
	}
	return list, nil
}

func (c *Cliente) corpoChat(system string, msgs []Mensagem, tools []map[string]any, inline bool) ([]byte, error) {
	hist, err := converterHistorico(msgs, inline)
	if err != nil {
		return nil, err
	}
	list := append([]oaiMsg{{Role: "system", Content: strNil(comBlocoEnv(system))}}, hist...)
	// Anthropic wiring: multi-tool rounds formed as adjacent assistant
	// messages (one call each) reject on the Claude degraus — the translated
	// body groups every consecutive `tool` result into ONE user message,
	// whose owner becomes the LAST assistant only, so the earlier calls look
	// like results without calls (400 "unexpected tool_use_id found in
	// tool_result blocks", measured on virtual claude-opus-5-5 at
	// 2026-10-25 02:23Z). One assistant announcing the whole round keeps the
	// pairs adjacency-valid on every dialect.
	list = fundirChamadasConsecutivas(list)
	// Duplicated tool-call ids across the history are rejected upstream with
	// 400 — the first occurrence of each id keeps it, later ones are renamed
	// with their paired results. This runs BEFORE re-pairing: pairing by a
	// still-duplicated id would attach both results to the last owner.
	desduplicarIDsDeChamada(list)
	// Same contract for positions: each `tool` message must sit right after
	// the assistant that announced its id. Interrupted turns (healed orphan
	// calls, results recorded after later turns) can leave results stranded
	// behind unrelated messages; they are re-paired next to their owner, and
	// results whose call appears nowhere are dropped (nothing upstream can
	// pair them, and the harness never writes such a result).
	list = reapararResultados(list)
	wireTools, notas := aplicarFingerprint(tools)
	if len(notas) > 0 {
		// Appended decoys need their brake where the gate does not validate:
		// the system slot, as a first message of its own (the official
		// system follows intact, the shape the official client sends).
		list = append([]oaiMsg{{Role: "system", Content: notaFingerprint(notas)}}, list...)
	}
	body := map[string]any{
		"model":          c.cfg.Model,
		"messages":       list,
		"stream":         true,
		"max_tokens":     c.MaxTokens(),
		"stream_options": map[string]any{"include_usage": true},
	}
	if effort := strings.TrimSpace(c.cfg.Effort); effort != "" && !strings.EqualFold(effort, "none") {
		// Omit an empty field or `none` (the provider rejects both).
		body["reasoning_effort"] = effort
	}
	body["tools"] = wireTools
	body["tool_choice"] = "auto"
	if c.cfg.DisableParallelToolCalls && len(wireTools) > 0 {
		// The field is only meaningful (and only accepted) alongside tools.
		body["parallel_tool_calls"] = false
	}
	return json.Marshal(body)
}

// textoDeResultado extrae o texto de um Content do oaiMsg (assistente leva
// *string; usuário pode trazer []contentPart).
func textoMensagem(v any) string {
	switch t := v.(type) {
	case *string:
		if t != nil {
			return *t
		}
	case string:
		return t
	}
	return ""
}

// fundirChamadasConsecutivas collapses consecutive assistant messages that
// each announce tool_calls into a single assistant carrying all of them. The
// harness records every parallel tool of a round as its own tool_call message,
// so a two-tool round built as [assistant A][assistant B][tool A][tool B] used
// to translate on the Anthropic dialect to two assistants followed by one user
// message with both results — and only the last call came with its result.
func fundirChamadasConsecutivas(list []oaiMsg) []oaiMsg {
	out := make([]oaiMsg, 0, len(list))
	for _, m := range list {
		ultimo := len(out) - 1
		if m.Role == "assistant" && len(m.ToolCalls) > 0 && ultimo >= 0 &&
			out[ultimo].Role == "assistant" && len(out[ultimo].ToolCalls) > 0 {
			if textoMensagem(out[ultimo].Content) == "" {
				out[ultimo].Content = m.Content
			}
			out[ultimo].ToolCalls = append(out[ultimo].ToolCalls, m.ToolCalls...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// reapararResultados re-positions every tool result immediately after the
// assistant that announced its tool_call_id, preserving each owner's results
// in encounter order (results of the merged round travel as a group, which is
// exactly what the translation stacks into one user message). A result whose
// call appears nowhere in the body is dropped: unreachable by any dialect.
func reapararResultados(list []oaiMsg) []oaiMsg {
	dono := make(map[string]int, len(list))
	for i, m := range list {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			dono[c.ID] = i
		}
	}
	esperando := make(map[int][]oaiMsg, len(list))
	for _, m := range list {
		if m.Role != "tool" {
			continue
		}
		j, ok := dono[m.ToolCallID]
		if !ok {
			continue // no call announced it: unmated result
		}
		esperando[j] = append(esperando[j], m)
	}
	var out []oaiMsg
	for i, m := range list {
		if m.Role == "tool" {
			continue // re-attached next to its owner (or dropped)
		}
		out = append(out, m)
		if m.Role == "assistant" {
			out = append(out, esperando[i]...)
		}
	}
	return out
}

// contentPart is one element of a multimodal user content (OpenAI Chat
// Completions): {"type":"text","text":...} or
// {"type":"image_url","image_url":{"url":"data:..."}}.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// imagePlaceholder matches the [Image N] placeholders the full screen inserts.
var imagePlaceholder = regexp.MustCompile(`\[Image (\d+)\]`)

// userParts builds the content parts of a user message with images: the text
// (plus one "[Image N: path]" line per image, so the model knows the file)
// followed by each image as a base64 data URL. An image whose file can no
// longer be read is dropped with a note in the text instead of failing the
// request — a resumed session may point at a deleted attachment.
func userParts(text string, images []string, inline bool) []contentPart {
	// Labels follow the placeholders in the text when there is one per image;
	// otherwise the images are numbered in order.
	var labels []string
	seen := map[string]bool{}
	for _, m := range imagePlaceholder.FindAllStringSubmatch(text, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			labels = append(labels, m[1])
		}
	}
	if len(labels) != len(images) {
		labels = nil
		for i := range images {
			labels = append(labels, strconv.Itoa(i+1))
		}
	}
	var notes []string
	var imgs []contentPart
	for i, path := range images {
		if !inline {
			notes = append(notes, fmt.Sprintf("[Image %s: %s]", labels[i], path))
			imgs = append(imgs, contentPart{Type: "image_url", ImageURL: &imageURL{URL: "data:image/png;base64," + imageEstimateStandIn}})
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			notes = append(notes, fmt.Sprintf("[Image %s: %s (file missing, not attached)]", labels[i], path))
			continue
		}
		notes = append(notes, fmt.Sprintf("[Image %s: %s]", labels[i], path))
		url := "data:" + imageMime(path) + ";base64," + base64.StdEncoding.EncodeToString(data)
		imgs = append(imgs, contentPart{Type: "image_url", ImageURL: &imageURL{URL: url}})
	}
	full := text
	if len(notes) > 0 {
		full = strings.TrimRight(text, "\n") + "\n\n" + strings.Join(notes, "\n")
	}
	return append([]contentPart{{Type: "text", Text: full}}, imgs...)
}

// imageMime guesses the image type from the extension (PNG by default: the
// clipboard attachments are always saved as PNG).
func imageMime(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "image/png"
}

// elapsed rounds strictly positive so telemetry and TTFT remain observable on
// Windows clocks whose resolution can make an immediate event measure 0 ns.
func elapsed(start time.Time) time.Duration {
	d := time.Since(start)
	if d <= 0 {
		return time.Nanosecond
	}
	return d
}

type sseChunk struct {
	Choices []struct {
		Delta struct {
			Content          *string `json:"content"`
			Reasoning        *string `json:"reasoning"`
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
					// Raw, not string: some upstreams send the arguments as a
					// JSON object instead of the encoded string the OpenAI
					// contract asks for (see argFragment).
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// RodarContext is Rodar with real cancellation: the request and the stream
// read abort when ctx expires or is canceled (the REPL uses Ctrl+C). Silence
// is bounded by idleTimeout and transient failures are
// retried (retry.go).
func (c *Cliente) RodarContext(ctx context.Context, system string, msgs []Mensagem, tools []map[string]any) (*Rodada, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	protocol, ok := normalizarProtocolo(c.cfg.Protocol)
	if !ok {
		return nil, fmt.Errorf("unknown protocol %q (use %s or %s)", strings.TrimSpace(c.cfg.Protocol), ProtocoloChat, ProtocoloResponses)
	}
	if protocol == ProtocoloResponses && c.protocolState != nil && c.protocolState.chatFallback.Load() {
		protocol = ProtocoloChat
	}
	body, err := c.corpoProtocolo(protocol, system, msgs, tools, true)
	if err != nil {
		return nil, err
	}
	run := func(protocol string, body []byte) (*Rodada, error) {
		return c.comRetry(ctx, func(ctx context.Context) (*Rodada, error) {
			return c.tentativa(ctx, body, protocol)
		})
	}
	round, err := run(protocol, body)
	if err == nil || protocol != ProtocoloResponses || !responsesEndpointUnavailable(err) {
		return round, err
	}

	// This only follows a terminal route/method-unavailable response, never an
	// auth, model, payload, rate-limit, timeout or generation error. No stream
	// or tool call was accepted, so retrying the same turn in Chat is safe.
	if c.protocolState != nil {
		c.protocolState.chatFallback.Store(true)
	}
	chatBody, bodyErr := c.corpoProtocolo(ProtocoloChat, system, msgs, tools, true)
	if bodyErr != nil {
		return nil, bodyErr
	}
	round, err = run(ProtocoloChat, chatBody)
	if err != nil {
		return nil, fmt.Errorf("Responses endpoint unavailable; Chat Completions fallback failed: %w", err)
	}
	round.ProtocolFallback = true
	return round, nil
}

// Rodar runs one round: streams, accumulates text, assembles tool calls and
// captures usage. system and tools go in untouched — the dealer never edits
// history.
func (c *Cliente) Rodar(system string, msgs []Mensagem, tools []map[string]any) (*Rodada, error) {
	return c.RodarContext(context.Background(), system, msgs, tools)
}

// tentativa is one attempt: one POST and the read of its stream, under the
// watchdog.
func (c *Cliente) tentativa(ctx context.Context, body []byte, proto string) (*Rodada, error) {
	wctx, wd := newWatchdog(ctx, pick(c.cfg.Idle, idleTimeout))
	defer wd.stop()
	if proto != ProtocoloChat && proto != ProtocoloResponses {
		return nil, fmt.Errorf("unknown protocol %q (use %s or %s)", proto, ProtocoloChat, ProtocoloResponses)
	}
	endpoint := NormalizarURL(c.cfg.BaseURL) + "/chat/completions"
	if proto == ProtocoloResponses {
		endpoint = NormalizarURL(c.cfg.BaseURL) + "/responses"
	}
	req, err := http.NewRequestWithContext(wctx, "POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The opencode client contract applies to every gateway: live UA,
	// canonical ids and session headers stable per conversation.
	aplicarCabecalhosOpencode(req, sessaoEstavel(c.conversa))
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	req.Header.Set("Accept", "text/event-stream")
	// No http.Client.Timeout: the watchdog bounds silence, and a total cap
	// would cut a model that is still streaming (see retry.go).
	sent := time.Now()
	resp, err := clienteHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("provider: %w", cause(wctx, err))
	}
	defer resp.Body.Close()
	read := readRound
	if proto == ProtocoloResponses {
		read = readResponses
	}
	r, err := read(resp, wctx, sent, wd.kick)
	return r, cause(wctx, err)
}

// readRound consumes the already-open SSE stream and builds the Rodada. start
// is when the request went out: TTFT and Dur are measured from it. kick (may
// be nil) is called after every line read, to feed the idle watchdog.
func readRound(resp *http.Response, ctx context.Context, start time.Time, kick func()) (*Rodada, error) {
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, newHTTPError(resp, b)
	}
	r := &Rodada{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), sseMaxLinha)
	terminal := false // [DONE] or a finish_reason arrived
	pending := map[int]*oaiCall{}
	// byID maps a call id to its slot; slotOf maps an upstream index to the
	// slot it was resolved to (an index can be remapped when a new id arrives
	// on an index already taken). cur is the slot of the last delta, where an
	// indexless, idless continuation belongs.
	byID := map[string]int{}
	slotOf := map[int]int{}
	cur := -1
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if strings.HasPrefix(line, "{") {
				// A bare JSON body served as 200 (no SSE framing).
				if err := erroDePayload(line); err != nil {
					return nil, err
				}
			}
			continue // blank separators and ": ping" comments are not progress
		}
		if kick != nil {
			kick()
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			terminal = true
			break
		}
		if r.TTFT == 0 {
			r.TTFT = elapsed(start)
		}
		// An error chunk under HTTP 200 must surface, not decode into an
		// empty chunk and end as an empty answer.
		if err := erroDePayload(payload); err != nil {
			return nil, err
		}
		var ch sseChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			return nil, fmt.Errorf("invalid chunk: %w", err)
		}
		if len(ch.Choices) > 0 {
			d := &ch.Choices[0].Delta
			for pos, tc := range d.ToolCalls {
				i := resolveSlot(pending, byID, slotOf, cur, pos, tc.ID, tc.Index)
				cur = i
				if pending[i] == nil {
					pending[i] = &oaiCall{ID: tc.ID, Type: "function"}
				}
				if tc.ID != "" {
					pending[i].ID = tc.ID
				}
				if tc.Function.Name != "" {
					pending[i].Function.Name += tc.Function.Name
				}
				if frag, whole := argFragment(tc.Function.Arguments); whole {
					// A whole JSON object is the complete arguments: a repeat
					// replaces the previous one instead of gluing two documents.
					pending[i].Function.Arguments = frag
				} else {
					pending[i].Function.Arguments += frag
				}
			}
			if d.Content != nil {
				r.Texto += *d.Content
			}
			// Different providers use one field or the other (OpenRouter sends
			// both with the same text): the first non-empty one wins.
			if p := d.Reasoning; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = elapsed(start)
			} else if p := d.ReasoningContent; p != nil && *p != "" {
				r.Raciocinio += *p
				r.DurRaciocinio = elapsed(start)
			}
			if f := ch.Choices[0].FinishReason; f != nil && *f != "" {
				r.Fim = *f
				terminal = true
				if *f == "length" || *f == "content_filter" {
					r.Cortada = true
				}
			}
		}
		if ch.Usage != nil && r.Usage == nil {
			r.Usage = &Usage{PromptTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
			if d := ch.Usage.PromptTokensDetails; d != nil {
				r.Usage.CachedTokens = d.CachedTokens
			}
			if d := ch.Usage.CompletionTokensDetails; d != nil {
				r.Usage.ReasoningTokens = d.ReasoningTokens
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !terminal {
		// Neither [DONE] nor a finish_reason: the connection was cut.
		return nil, fmt.Errorf("stream: ended without a terminal event: %w", io.ErrUnexpectedEOF)
	}
	// close tool calls in index order
	idx := make([]int, 0, len(pending))
	for i := range pending {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		call := pending[i]
		r.Tools = append(r.Tools, ToolChamada{ID: call.ID, Nome: call.Function.Name, Args: call.Function.Arguments})
	}
	if r.Fim == "content_filter" && r.Texto == "" && len(r.Tools) == 0 {
		// Nothing to show: looks like an empty answer, and the runtime would
		// regenerate the very same filtered request. Fail explicitly.
		return nil, &providerError{kind: "content_filter", summary: "the provider stopped the answer with finish_reason content_filter"}
	}
	r.FimPorTexto = len(r.Tools) == 0
	r.Dur = elapsed(start)
	return r, nil
}

// argFragment turns one streamed arguments delta into text. The contract is a
// JSON-encoded string fragment (whole=false, to be concatenated); an upstream
// that sends the whole object instead gets it verbatim with whole=true, and
// null/absent adds nothing.
func argFragment(raw json.RawMessage) (frag string, whole bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return "", false
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, false
		}
		return "", false
	}
	return t, true
}

// resolveSlot picks the pending slot a tool-call delta belongs to:
//   - with an id: the slot already known for that id; else the specified
//     index if it is free; else the next free slot;
//   - index only: the slot that index was resolved to (slotOf), else the
//     index itself if free, else the next free slot;
//   - neither: the current call (cur), and the position in the delta only
//     while no call has started yet.
func resolveSlot(pending map[int]*oaiCall, byID map[string]int, slotOf map[int]int, cur, pos int, id string, index *int) int {
	if id != "" {
		if known, ok := byID[id]; ok {
			if index != nil {
				slotOf[*index] = known
			}
			return known
		}
		i := nextFree(pending)
		if index != nil && pending[*index] == nil {
			i = *index
		}
		byID[id] = i
		if index != nil {
			slotOf[*index] = i
		}
		return i
	}
	if index != nil {
		if s, ok := slotOf[*index]; ok {
			return s
		}
		i := *index
		if pending[i] != nil {
			i = nextFree(pending)
		}
		slotOf[*index] = i
		return i
	}
	if cur >= 0 {
		return cur
	}
	return pos
}

// nextFree is the slot after the highest index in use.
func nextFree(pending map[int]*oaiCall) int {
	n := 0
	for i := range pending {
		if i >= n {
			n = i + 1
		}
	}
	return n
}

// ── /models catalog ──────────────────────────────────────────────────────

// CatalogModel is a /models item, tolerant of unknown fields. ContextLength
// and Input may be missing (the provider does not publish them) — then the
// zero values apply and input filtering stays conservative (keeps it).
type CatalogModel struct {
	ID            string   `json:"id"`
	OwnedBy       string   `json:"owned_by,omitempty"`
	Created       int64    `json:"created,omitempty"`
	ContextLength int      `json:"context_length,omitempty"`
	Type          string   `json:"type,omitempty"`
	Input         []string `json:"input,omitempty"` // ["text"] | ["text","image"] | ["audio"]...
}

// ModelsResponse is the OpenAI /models envelope. It may carry extra metadata
// at the same level — ignored.
type ModelsResponse struct {
	Data    []CatalogModel `json:"data"`
	Object  string         `json:"object,omitempty"`
	RawJSON []byte         `json:"-"`
}

// NormalizarURL tolerates a base with or without a trailing slash and avoids
// duplicated suffixes (/v1/v1, /chat/completions/chat/completions, /responses).
func NormalizarURL(base string) string {
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	for _, suffix := range []string{"/chat/completions", "/completions", "/responses", "/models"} {
		if strings.HasSuffix(b, suffix) {
			b = strings.TrimSuffix(b, suffix)
		}
	}
	return b
}

const (
	catalogTimeout = 15 * time.Second
	catalogBodyCap = 1 << 20 // 1 MiB: a catalog is never huge; cuts off abuse
)

// errorSummary turns the body of an error response into ONE useful line.
// No provider returns plain HTML from an API — when it happens (WAF block
// page, edge 403 on the catalog), dumping the raw body into the terminal
// buries the real error under markup. So: if it is not JSON, extract the
// <title> or the first visible text and truncate.
func errorSummary(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty body)"
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		// JSON: try the OpenAI format {"error":{"message":...}} before truncating.
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(s), &env) == nil && len(env.Error) > 0 {
			var msg struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(env.Error, &msg) == nil && msg.Message != "" {
				return truncateLine(msg.Message, max)
			}
			return truncateLine(string(env.Error), max)
		}
		return truncateLine(s, max)
	}
	// HTML: <title> is usually the most informative ("Attention Required!",
	// "Sorry, you have been blocked").
	if i := strings.Index(strings.ToLower(s), "<title>"); i >= 0 {
		if j := strings.Index(s[i:], "</title>"); j > 0 {
			title := strings.TrimSpace(stripTags(s[i+7 : i+j]))
			if title != "" {
				return truncateLine(title, max)
			}
		}
	}
	// fallback: first chunk of visible text, without markup.
	if txt := stripTags(s); txt != "" {
		return truncateLine(txt, max)
	}
	return truncateLine(s, max)
}

// stripTags removes markup and collapses whitespace: what is left is the text
// the user reads.
func stripTags(s string) string {
	var sb strings.Builder
	inside := false
	for _, r := range s {
		switch {
		case r == '<':
			inside = true
		case r == '>':
			inside = false
			sb.WriteRune(' ')
		case !inside:
			sb.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func truncateLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ") // a single line, no breaks
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// Catalogo does GET {baseURL}/models with Bearer auth and returns the raw
// envelope.
func Catalogo(ctx context.Context, baseURL, apiKey string) (*ModelsResponse, error) {
	endpoint := NormalizarURL(baseURL) + "/models"
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// Same contract as the inference route: the catalog is a public vitrine,
	// but the identity travels with it (the day the /models answers per key,
	// this is already correct).
	aplicarCabecalhosOpencode(req, "")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	httpc := &http.Client{Timeout: catalogTimeout, Transport: roteadorTLS{}}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("catalog %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("catalog HTTP %d: %s", resp.StatusCode, errorSummary(b, 300))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, catalogBodyCap+1))
	if err != nil {
		return nil, fmt.Errorf("catalog: %w", err)
	}
	if len(b) > catalogBodyCap {
		return nil, fmt.Errorf("catalog: response exceeded %d bytes", catalogBodyCap)
	}
	var out ModelsResponse
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("catalog: invalid JSON: %w", err)
	}
	out.RawJSON = b
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("empty catalog at %s", endpoint)
	}
	return &out, nil
}

// Modelos returns the processed catalog: deduplicated, sorted alphabetically,
// only items that are at least plausibly chat models. Filtering is LENIENT:
// a provider may present {"input": ["text"]} as a list, a single string or
// publish nothing; with no signal, the model is kept. Explicit non-chat
// markers (embedding/tts/whisper/image/music) are dropped when the input does
// not confirm text.
func Modelos(ctx context.Context, baseURL, apiKey string) ([]CatalogModel, error) {
	resp, err := Catalogo(ctx, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []CatalogModel
	for _, m := range resp.Data {
		if m.ID == "" || seen[m.ID] {
			continue
		}
		if !plausibleChat(m) {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func plausibleChat(m CatalogModel) bool {
	if len(m.Input) > 0 {
		hasText := false
		for _, in := range m.Input {
			if strings.EqualFold(strings.TrimSpace(in), "text") {
				hasText = true
				break
			}
		}
		if !hasText {
			return false // published input WITHOUT text: audio/image/embedding
		}
		return true
	}
	// No input metadata: conservative — keep it, unless explicitly marked.
	marker := strings.ToLower(m.Type) + " " + strings.ToLower(m.ID)
	for _, bad := range []string{"embedding", "tts", "whisper", "transcri", "dall-e", "image-gen", "stabili", "stable-", "music", "moderation", "rerank"} {
		if strings.Contains(marker, bad) {
			return false
		}
	}
	return true
}

// ContextoDe returns the published window for model ID (0 = unknown).
func ContextoDe(list []CatalogModel, id string) int {
	for _, m := range list {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}
