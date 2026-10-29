package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// ── test infrastructure ────────────────────────────────────────────────────

// pipe is a synchronized writer: the test writes requests, the agent writes
// responses, and the test reads what has been written so far. Unlike io.Pipe —
// which would block the reader waiting on the writer and make the test depend
// on goroutine scheduling.
type pipe struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (p *pipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Write(b)
}

func (p *pipe) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// line is an already-parsed JSON-RPC message, enough to inspect responses and
// notifications without a type for each.
type line struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string          `json:"sessionUpdate"`
			ToolCallID    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Kind          string          `json:"kind"`
			Status        string          `json:"status"`
			RawInput      json.RawMessage `json:"rawInput"`
			Content       Content         `json:"content"`
			Meta          json.RawMessage `json:"_meta"`
		} `json:"update"`
	} `json:"params"`
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Content decodes the content field of an ACP update, which has NO single
// shape: agent_message_chunk uses a SINGLE block object ({type,text});
// tool_call_update uses an ARRAY of blocks. Accepting both is what lets us
// inspect both update kinds with the same decoder — and if the agent ever
// emits the wrong shape for one of them, the test breaks here instead of
// passing silently.
type Content struct {
	Blocks []block
}

type block struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func (c *Content) UnmarshalJSON(b []byte) error {
	c.Blocks = nil
	if len(trimSpaceJSON(b)) == 0 || string(trimSpaceJSON(b)) == "null" {
		return nil
	}
	if trimSpaceJSON(b)[0] == '[' {
		return json.Unmarshal(b, &c.Blocks)
	}
	var one block
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	c.Blocks = []block{one}
	return nil
}

func trimSpaceJSON(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r') {
		b = b[1:]
	}
	return b
}

// readLines returns the lines already written to the pipe, parsed.
func readLines(t *testing.T, p *pipe) []line {
	t.Helper()
	raw := p.String()
	if raw == "" {
		return nil
	}
	parts := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	out := make([]line, 0, len(parts))
	for _, s := range parts {
		if strings.TrimSpace(s) == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", s, err)
		}
		out = append(out, l)
	}
	return out
}

// waitLines waits until n lines have been written (or the deadline passes). A
// session/prompt turn runs on a goroutine, so the response is not synchronous
// with writing the request.
func waitLines(t *testing.T, p *pipe, n int) []line {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ls := readLines(t, p)
		if len(ls) >= n {
			return ls
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out: expected %d lines, got %d:\n%s", n, len(ls), p.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// ── fakes ──────────────────────────────────────────────────────────────────

// fakeTurn is a deterministic Turno: it emits fixed events in the runtime's
// order (tools, guardrail, tokens, text) and returns the configured error. It
// honours the ctx on purpose — if it ignored cancellation, the session/cancel
// test would pass by accident without ever exercising the real path.
type fakeTurn struct {
	text    string
	tools   []string      // tools that run successfully
	fails   []string      // tools that fail
	echo    string        // output echoed after each tool
	err     error         // error returned at the end
	delay   time.Duration // delay before responding (cancellation)
	compact bool          // emits Guardrail(true)
	tokens  bool          // emits Tokens (must not produce a notification)
	thought string        // provider-exposed reasoning, emitted through RendererRaciocinio
	nota    string        // intermediate text, emitted through RendererNota
	args    string        // when set, tools go through RendererAoVivo with these args

	started chan struct{}
	once    sync.Once

	mu      sync.Mutex
	ctxSeen bool
	prompt  string
	cwdSeen string
}

func newFakeTurn() *fakeTurn { return &fakeTurn{started: make(chan struct{})} }

func (f *fakeTurn) Send(ctx context.Context, r runtime.Renderer, cwd, prompt string) error {
	f.once.Do(func() { close(f.started) })
	f.mu.Lock()
	f.prompt = prompt
	f.cwdSeen = cwd
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			f.mark()
		}
	}
	if rr, ok := r.(runtime.RendererRaciocinio); ok && f.thought != "" {
		rr.Pensou(f.thought, 1500*time.Millisecond)
	}
	if rn, ok := r.(runtime.RendererNota); ok && f.nota != "" {
		rn.Nota(f.nota)
	}
	av, live := r.(runtime.RendererAoVivo)
	for i, name := range f.tools {
		if live && f.args != "" {
			id := fmt.Sprintf("%d/c%d", i, i)
			av.ToolComecou(id, name, f.args)
			av.ToolTerminou(id, name, f.args, f.echo, 7*time.Millisecond, false)
			continue
		}
		r.Tool(name, 7*time.Millisecond, false)
		if f.echo != "" {
			r.Eco(f.echo)
		}
	}
	for _, name := range f.fails {
		r.Tool(name, 3*time.Millisecond, true)
	}
	if f.compact {
		r.Guardrail(true)
	}
	if f.tokens {
		r.Tokens(10, 20, time.Second, false)
	}
	if f.text != "" {
		r.Resposta(f.text)
	}
	if f.err != nil {
		return f.err
	}
	return nil
}

func (f *fakeTurn) mark() {
	f.mu.Lock()
	f.ctxSeen = true
	f.mu.Unlock()
}

func (f *fakeTurn) sawCtx() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxSeen
}

func (f *fakeTurn) promptText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

func (f *fakeTurn) receivedCwd() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cwdSeen
}

// blockingTurn blocks until the ctx is cancelled and returns the cancellation
// error — the shape the real runtime delivers when the client sends
// session/cancel mid-turn.
type blockingTurn struct {
	started  chan struct{}
	released chan struct{}
}

func newBlockingTurn() *blockingTurn {
	return &blockingTurn{started: make(chan struct{}), released: make(chan struct{})}
}

func (t *blockingTurn) Send(ctx context.Context, _ runtime.Renderer, _, _ string) error {
	close(t.started)
	<-ctx.Done()
	close(t.released)
	return ctx.Err()
}

// factory returns a deterministic factory: always the same turn and always
// the same id, so tests do not depend on randomness.
func factory(f Turno) func(config.Config) (Turno, string) {
	return func(config.Config) (Turno, string) { return f, "s-deadbeef" }
}

// harness wires agent + output pipe + request writer, and runs Handle on a
// goroutine with the test's ctx.
type harness struct {
	a      *Agente
	out    *pipe
	input  *io.PipeWriter
	cancel context.CancelFunc
}

func setup(t *testing.T, fake Turno) *harness {
	t.Helper()
	a := Novo(config.Config{})
	a.Fabrica = factory(fake)
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Handle(ctx, pr, out) }()
	return &harness{a: a, out: out, input: pw, cancel: cancel}
}

// send writes a request to the input wire.
func (h *harness) send(t *testing.T, method string, id any, params any) {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.input.Write(append(b, '\n')); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("writing request %s: %v", method, err)
	}
}

// ── jsonrpc.go ─────────────────────────────────────────────────────────────

func TestWriteLineEmitsOneJSONLine(t *testing.T) {
	var b strings.Builder
	if err := EscreverLinha(&b, map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\n") {
		t.Fatalf("line without terminator: %q", b.String())
	}
	if strings.Count(b.String(), "\n") != 1 {
		t.Fatalf("expected exactly one line, got %q", b.String())
	}
}

func TestWriteLineFlushes(t *testing.T) {
	// The host may pass a *bufio.Writer; without a flush the response would die
	// in the buffer and the client would never see it.
	var raw strings.Builder
	bw := bufio.NewWriter(&raw)
	if err := EscreverLinha(bw, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if raw.String() == "" {
		t.Fatal("message did not reach the writer (missing flush)")
	}
}

func TestDecodeLineEmptyLineIsNotAMessage(t *testing.T) {
	if _, ok, err := DecodificarLinha([]byte("   \n")); ok || err != nil {
		t.Fatalf("empty line should be silent: ok=%v err=%v", ok, err)
	}
}

func TestHasIDDistinguishesRequestFromNotification(t *testing.T) {
	req := Request{ID: json.RawMessage("1")}
	if !req.TemID() {
		t.Fatal("numeric id should count as a request")
	}
	notif := Request{ID: json.RawMessage("null")}
	if notif.TemID() {
		t.Fatal("null id should count as a notification")
	}
	empty := Request{}
	if empty.TemID() {
		t.Fatal("missing id should count as a notification")
	}
}

func TestDecodeLineInvalidJSONReturnsError(t *testing.T) {
	if _, _, err := DecodificarLinha([]byte("{not json")); err == nil {
		t.Fatal("invalid JSON should return an error")
	}
}

// ── initialize ─────────────────────────────────────────────────────────────

func TestInitializeEchoesClientVersion(t *testing.T) {
	f := newFakeTurn()
	h := setup(t, f)
	h.send(t, MetInitialize, 1, map[string]any{"protocolVersion": 5})
	ls := waitLines(t, h.out, 1)
	r := ls[0]
	if r.Error != nil {
		t.Fatalf("initialize failed: %+v", r.Error)
	}
	if v, ok := r.Result["protocolVersion"].(float64); !ok || int(v) != 5 {
		t.Fatalf("protocolVersion should echo 5, got %v", r.Result["protocolVersion"])
	}
}

func TestInitializeWithoutParamsUsesVersionOne(t *testing.T) {
	// A client that sends initialize without params (or without
	// protocolVersion) needs a usable response, not an error: version 1 is the
	// protocol default.
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 7, map[string]any{})
	ls := waitLines(t, h.out, 1)
	if v, ok := ls[0].Result["protocolVersion"].(float64); !ok || int(v) != 1 {
		t.Fatalf("default protocolVersion should be 1, got %v", ls[0].Result["protocolVersion"])
	}
}

func TestInitializeVersionZeroFallsBackToDefault(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 8, map[string]any{"protocolVersion": 0})
	ls := waitLines(t, h.out, 1)
	if v, _ := ls[0].Result["protocolVersion"].(float64); int(v) != 1 {
		t.Fatalf("protocolVersion 0 should fall back to default 1, got %v", ls[0].Result["protocolVersion"])
	}
}

func TestInitializeReportsCapabilitiesAndAuthMethods(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, "abc", map[string]any{"protocolVersion": 1})
	ls := waitLines(t, h.out, 1)
	res := ls[0].Result
	if _, ok := res["agentCapabilities"]; !ok {
		t.Fatalf("response without agentCapabilities: %v", res)
	}
	if _, ok := res["authMethods"]; !ok {
		t.Fatalf("response without authMethods: %v", res)
	}
}

func TestInitializePreservesNumericAndStringID(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 42, nil)
	h.send(t, MetInitialize, "id-text", nil)
	ls := waitLines(t, h.out, 2)
	if string(ls[0].ID) != "42" {
		t.Fatalf("numeric id should come back as 42, got %s", ls[0].ID)
	}
	if string(ls[1].ID) != `"id-text"` {
		t.Fatalf("string id should come back as \"id-text\", got %s", ls[1].ID)
	}
}

// ── session/new ────────────────────────────────────────────────────────────

func TestSessionNewReturnsSessionId(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 1, nil)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": `C:\tmp\project`})
	ls := waitLines(t, h.out, 2)
	id, _ := ls[1].Result["sessionId"].(string)
	if id != "s-deadbeef" {
		t.Fatalf("unexpected sessionId: %q", id)
	}
}

func TestSessionNewCreatesDistinctIdsPerSession(t *testing.T) {
	f := newFakeTurn()
	n := 0
	var mu sync.Mutex
	a := Novo(config.Config{})
	a.Fabrica = func(config.Config) (Turno, string) {
		mu.Lock()
		defer mu.Unlock()
		n++
		switch n {
		case 1:
			return f, "s-first"
		case 2:
			return f, "s-second"
		default:
			return f, "s-other"
		}
	}
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	defer pw.Close()
	go func() { _ = a.Handle(context.Background(), pr, out) }()

	for i, id := range []int{10, 11} {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": MetSessionNovo, "params": map[string]any{"cwd": "."}})
		_, _ = pw.Write(append(b, '\n'))
		_ = i
	}
	ls := waitLines(t, out, 2)
	if ls[0].Result["sessionId"] == ls[1].Result["sessionId"] {
		t.Fatalf("two sessions shared the same id: %v", ls[0].Result["sessionId"])
	}
}

func TestSessionNewEmptyCwdUsesProcessDirectory(t *testing.T) {
	// An empty cwd does not invalidate the session: the harness needs a
	// directory for the tools, and the process one is the best guess available.
	f := &fakeTurn{started: make(chan struct{}), text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 3, map[string]any{})
	ls := waitLines(t, h.out, 1)
	id, _ := ls[0].Result["sessionId"].(string)
	if id == "" {
		t.Fatal("session/new without cwd should create a session normally")
	}
	h.send(t, MetPrompt, 4, map[string]any{"sessionId": id, "prompt": []map[string]string{{"type": "text", "text": "hi"}}})
	waitLines(t, h.out, 2)
	if f.receivedCwd() == "" {
		t.Fatal("cwd was empty in the turn")
	}
}

// ── session/prompt ─────────────────────────────────────────────────────────

func TestPromptHappyPathEmitsChunkThenEmptyResponse(t *testing.T) {
	f := &fakeTurn{started: make(chan struct{}), text: "turn ok"}
	h := setup(t, f)

	h.send(t, MetInitialize, 1, map[string]any{"protocolVersion": 1})
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": `C:\tmp`})
	ls := waitLines(t, h.out, 2)
	id := ls[1].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt": []map[string]string{
			{"type": "text", "text": "good morning"},
			{"type": "text", "text": " how are you"},
		},
	})
	ls = waitLines(t, h.out, 4)

	// 1 agent_message_chunk notification, then the {} response — in that order.
	not := ls[2]
	if not.Method != "session/update" {
		t.Fatalf("expected a session/update notification, got %q", not.Method)
	}
	if not.Params.Update.SessionUpdate != "agent_message_chunk" {
		t.Fatalf("unexpected sessionUpdate: %q", not.Params.Update.SessionUpdate)
	}
	if n := len(not.Params.Update.Content.Blocks); n != 1 || not.Params.Update.Content.Blocks[0].Text != "turn ok" {
		t.Fatalf("unexpected chunk content: %+v", not.Params.Update.Content)
	}
	if not.Params.Update.Content.Blocks[0].Type != "text" {
		t.Fatalf("chunk block should be type=text: %q", not.Params.Update.Content.Blocks[0].Type)
	}
	if not.Params.SessionID != id {
		t.Fatalf("notification should reference session %q, got %q", id, not.Params.SessionID)
	}

	resp := ls[3]
	if resp.Error != nil {
		t.Fatalf("prompt should succeed: %+v", resp.Error)
	}
	if len(resp.Result) != 0 {
		t.Fatalf("prompt result should be an empty {}, got %v", resp.Result)
	}
	if string(resp.ID) != "3" {
		t.Fatalf("response should reference id 3, got %s", resp.ID)
	}

	// The request sent to the runtime is the concatenation of the text blocks.
	select {
	case <-f.started:
	case <-time.After(time.Second):
		t.Fatal("turn never ran")
	}
}

func TestPromptConcatenatesTextBlocks(t *testing.T) {
	f := &fakeTurn{started: make(chan struct{}), text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt": []map[string]string{
			{"type": "text", "text": "one"},
			{"type": "text", "text": " two"},
		},
	})
	waitLines(t, h.out, 3)
	if got := f.promptText(); got != "one two" {
		t.Fatalf("concatenated prompt should be \"one two\", got %q", got)
	}
}

func TestPromptIgnoresNonTextBlocks(t *testing.T) {
	// Image/audio/resource do not go into the request: the harness is
	// text-only in this scope. Ignoring is better than inventing a transcript.
	f := &fakeTurn{started: make(chan struct{}), text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt": []map[string]string{
			{"type": "image", "text": "ignored"},
			{"type": "text", "text": "kept"},
		},
	})
	waitLines(t, h.out, 3)
	if got := f.promptText(); got != "kept" {
		t.Fatalf("only the text block should go in, got %q", got)
	}
}

func TestPromptUnknownSessionReturnsAgentError(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetPrompt, 9, map[string]any{
		"sessionId": "s-missing",
		"prompt":    []map[string]string{{"type": "text", "text": "hi"}},
	})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodErroAgente {
		t.Fatalf("unknown session should give -32000, got %+v", ls[0])
	}
	if !strings.Contains(ls[0].Error.Message, "s-missing") {
		t.Fatalf("message should reference the session: %q", ls[0].Error.Message)
	}
}

func TestPromptEmptyArrayIsInvalidParams(t *testing.T) {
	f := newFakeTurn()
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{"sessionId": id, "prompt": []map[string]string{}})
	ls = waitLines(t, h.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodParams {
		t.Fatalf("empty prompt should give -32602, got %+v", ls[1])
	}
	select {
	case <-f.started:
		t.Fatal("turn should not even start with an empty prompt")
	default:
	}
}

func TestPromptWithoutAnyTextBlockIsInvalidParams(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "image", "text": "x"}},
	})
	ls = waitLines(t, h.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodParams {
		t.Fatalf("prompt without a text block should give -32602, got %+v", ls[1])
	}
}

func TestPromptWithoutSessionIdIsInvalidParams(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetPrompt, 3, map[string]any{
		"prompt": []map[string]string{{"type": "text", "text": "hi"}},
	})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("missing sessionId should give -32602, got %+v", ls[0])
	}
}

func TestPromptMalformedParamsIsInvalidParams(t *testing.T) {
	h := setup(t, newFakeTurn())
	// params that is not an object: unmarshalling into the typed struct fails.
	b := []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":[1,2,3]}` + "\n")
	if _, err := h.input.Write(b); err != nil {
		t.Fatal(err)
	}
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("malformed params should give -32602, got %+v", ls[0])
	}
}

func TestPromptWithTurnAlreadyRunningReturns32001(t *testing.T) {
	// One in-flight turn per session: the ACP protocol does not queue. The
	// second prompt gets an explicit error instead of waiting indefinitely.
	f := newBlockingTurn()
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "first"}},
	})
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn did not start")
	}

	h.send(t, MetPrompt, 4, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "second"}},
	})
	ls = waitLines(t, h.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodTurnoRodando {
		t.Fatalf("concurrent prompt should give -32001, got %+v", ls[1])
	}
	if string(ls[1].ID) != "4" {
		t.Fatalf("the error should reference the rejected prompt's id, got %s", ls[1].ID)
	}
}

func TestPromptWithTurnErrorReturns32000(t *testing.T) {
	f := &fakeTurn{started: make(chan struct{}), err: errors.New("provider 502")}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "hi"}},
	})
	ls = waitLines(t, h.out, 2)
	if ls[1].Error == nil || ls[1].Error.Code != CodErroAgente {
		t.Fatalf("turn error should give -32000, got %+v", ls[1])
	}
	if !strings.Contains(ls[1].Error.Message, "502") {
		t.Fatalf("error message should carry the turn failure: %q", ls[1].Error.Message)
	}
}

// ── keepalive ───────────────────────────────────────────────────────────────

func TestKeepaliveSpeaksWhileTurnRuns(t *testing.T) {
	// A long turn must not leave the wire silent: the connector gives up on
	// 10 minutes of silence, and session/prompt never got an answer in the
	// 2026-10-22 incident. Progress, not content, keeps it alive.
	old := keepaliveInterval
	keepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	f := &fakeTurn{started: make(chan struct{}), delay: 150 * time.Millisecond, text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "slow"}},
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		lines := readLines(t, h.out)
		kept := false
		for _, l := range lines {
			if l.Params.Update.SessionUpdate == "agent_thought_chunk" {
				t.Fatalf("keepalive must not be reasoning text:\n%s", h.out.String())
			}
			if l.Params.Update.SessionUpdate == "session_info_update" {
				kept = true
			}
		}
		if kept {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no keepalive while the turn ran:\n%s", h.out.String())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestKeepaliveStopsWhenTurnEnds(t *testing.T) {
	// Without a stop, the ticker would keep emitting notifications after the
	// response arrived — the client would see a turn that never ends.
	old := keepaliveInterval
	keepaliveInterval = 2 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	f := &fakeTurn{started: make(chan struct{}), text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "quick"}},
	})
	// session/new response + message chunk + prompt response = 3 lines; wait for
	// the turn to be over before checking that the ticker went quiet.
	waitLines(t, h.out, 3)

	before := len(readLines(t, h.out))
	time.Sleep(60 * time.Millisecond) // several ticker periods
	after := readLines(t, h.out)
	if len(after) != before {
		t.Fatalf("keepalive kept writing after the response:\n%s", h.out.String())
	}
	for _, l := range after {
		if l.Params.Update.SessionUpdate == "session_info_update" {
			t.Fatalf("keepalive survived the end of the turn:\n%s", h.out.String())
		}
	}
}

func TestCancelStopsKeepalive(t *testing.T) {
	// Cancelling a stalled turn must also silence the keepalive: otherwise the
	// wire keeps talking about a turn that no longer exists.
	old := keepaliveInterval
	keepaliveInterval = 5 * time.Millisecond
	t.Cleanup(func() { keepaliveInterval = old })

	f := newBlockingTurn()
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "hang"}},
	})
	<-f.started
	h.send(t, MetCancel, 4, map[string]any{"sessionId": id})
	<-f.released

	time.Sleep(80 * time.Millisecond)
	kept := false
	for _, l := range readLines(t, h.out) {
		if l.Params.Update.SessionUpdate == "session_info_update" {
			kept = true
		}
	}
	if kept {
		t.Fatalf("keepalive kept writing after cancellation:\n%s", h.out.String())
	}
}

// ── session/cancel ─────────────────────────────────────────────────────────

func TestCancelInterruptsInFlightTurn(t *testing.T) {
	f := newBlockingTurn()
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "slow"}},
	})
	select {
	case <-f.started:
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not start")
	}

	h.send(t, MetCancel, 4, map[string]any{"sessionId": id})

	// The in-flight prompt returns a cancellation error, and the cancel
	// response also arrives — the two lines may come in either order.
	ls = waitLines(t, h.out, 3)

	var promptResp, cancelResp *line
	for i := range ls {
		if string(ls[i].ID) == "3" {
			promptResp = &ls[i]
		}
		if string(ls[i].ID) == "4" {
			cancelResp = &ls[i]
		}
	}
	if promptResp == nil || promptResp.Error == nil || promptResp.Error.Code != CodErroAgente {
		t.Fatalf("cancelled prompt should give error -32000, got %+v", promptResp)
	}
	if !strings.Contains(strings.ToLower(promptResp.Error.Message), "cancel") {
		t.Fatalf("message should mention cancellation: %q", promptResp.Error.Message)
	}
	if cancelResp == nil || cancelResp.Error != nil {
		t.Fatalf("session/cancel should respond {} successfully, got %+v", cancelResp)
	}

	select {
	case <-f.released:
	case <-time.After(2 * time.Second):
		t.Fatal("the turn should have observed the cancellation")
	}
}

func TestCancelWithoutInFlightTurnSucceeds(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetCancel, 3, map[string]any{"sessionId": ls[0].Result["sessionId"]})
	ls = waitLines(t, h.out, 2)
	if ls[1].Error != nil {
		t.Fatalf("cancel without a turn should be a successful no-op, got %+v", ls[1].Error)
	}
}

func TestCancelUnknownSessionReturnsError(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetCancel, 3, map[string]any{"sessionId": "s-ghost"})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodErroAgente {
		t.Fatalf("cancel on a nonexistent session should give -32000, got %+v", ls[0])
	}
}

func TestCancelWithoutSessionIdIsInvalidParams(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetCancel, 3, map[string]any{})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("cancel without sessionId should give -32602, got %+v", ls[0])
	}
}

func TestCancelAfterTurnAllowsNewPrompt(t *testing.T) {
	// Cancellation clears the session state: without that, every following
	// prompt in the same session would get -32001 forever.
	f := newBlockingTurn()
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"].(string)

	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "hang"}},
	})
	<-f.started
	h.send(t, MetCancel, 4, map[string]any{"sessionId": id})
	<-f.released

	// swap the turn for one that responds normally
	h.a.mu.Lock()
	h.a.sessions[id].turn = &fakeTurn{started: make(chan struct{}), text: "after"}
	h.a.mu.Unlock()

	h.send(t, MetPrompt, 5, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "again"}},
	})
	ls = waitLines(t, h.out, 5)
	var resp *line
	for i := range ls {
		if string(ls[i].ID) == "5" {
			resp = &ls[i]
		}
	}
	if resp == nil || resp.Error != nil {
		t.Fatalf("prompt after cancellation should work, got %+v", resp)
	}
}

// ── protocol errors ────────────────────────────────────────────────────────

func TestUnknownMethodReturns32601(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, "session/list", 5, nil)
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodMetodo {
		t.Fatalf("unknown method should give -32601, got %+v", ls[0])
	}
	if !strings.Contains(ls[0].Error.Message, "session/list") {
		t.Fatalf("message should reference the method: %q", ls[0].Error.Message)
	}
}

func TestMalformedLineDoesNotKillLoop(t *testing.T) {
	// A client that sent garbage on one line cannot bring the agent down: the
	// line is ignored and the next request is served normally.
	h := setup(t, newFakeTurn())
	if _, err := h.input.Write([]byte("{this is not json\n")); err != nil {
		t.Fatal(err)
	}
	h.send(t, MetInitialize, 6, nil)
	ls := waitLines(t, h.out, 1)
	if ls[0].Error != nil {
		t.Fatalf("initialize after a malformed line should work: %+v", ls[0].Error)
	}
	if v, _ := ls[0].Result["protocolVersion"].(float64); int(v) != 1 {
		t.Fatalf("unexpected protocolVersion: %v", ls[0].Result["protocolVersion"])
	}
}

func TestClientNotificationGetsNoResponse(t *testing.T) {
	// A notification (no id) is one-way: responding would violate JSON-RPC 2.0.
	h := setup(t, newFakeTurn())
	b := []byte(`{"jsonrpc":"2.0","method":"initialized","params":{}}` + "\n")
	if _, err := h.input.Write(b); err != nil {
		t.Fatal(err)
	}
	h.send(t, MetInitialize, 12, nil)
	ls := waitLines(t, h.out, 1)
	if len(ls) != 1 {
		t.Fatalf("client notification should not produce a line, got %d:\n%s", len(ls), h.out.String())
	}
	if string(ls[0].ID) != "12" {
		t.Fatalf("the only line should be the initialize response, got %s", ls[0].ID)
	}
}

func TestHandleEOFReturnsNil(t *testing.T) {
	a := Novo(config.Config{})
	a.Fabrica = factory(newFakeTurn())
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	if err := a.Handle(context.Background(), strings.NewReader(""), out); err != nil {
		t.Fatalf("EOF should be a clean shutdown, got %v", err)
	}
}

func TestHandleLineAboveCapReturnsError(t *testing.T) {
	// A line larger than the scanner cap ends the transport: pretending to
	// have read it would corrupt the protocol. The error must surface, not be
	// swallowed.
	a := Novo(config.Config{})
	a.Fabrica = factory(newFakeTurn())
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	huge := strings.Repeat("x", MaxLinha+1024)
	if err := a.Handle(context.Background(), strings.NewReader(huge+"\n"), out); err == nil {
		t.Fatal("line above the cap should return an error")
	}
}

func TestEachResponseIsOneLineWithNewline(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 1, nil)
	h.send(t, MetInitialize, 2, nil)
	waitLines(t, h.out, 2)
	s := h.out.String()
	if !strings.HasSuffix(s, "\n") {
		t.Fatalf("output should end in a newline: %q", s)
	}
	// Each line must be complete JSON: no newline in the middle of objects. If
	// the write is ever split into two Write calls, two responses would mix on
	// the same line and it would show up here.
	for _, l := range strings.Split(strings.TrimSuffix(s, "\n"), "\n") {
		var probe map[string]any
		if err := json.Unmarshal([]byte(l), &probe); err != nil {
			t.Fatalf("line is not complete JSON (newline in the middle?): %q: %v", l, err)
		}
	}
	if n := strings.Count(s, "\n"); n != 2 {
		t.Fatalf("two requests should produce exactly 2 lines, got %d: %q", n, s)
	}
}

// ── renderer translation ───────────────────────────────────────────────────

func TestPromptTranslatesToolAndEchoToNotifications(t *testing.T) {
	f := &fakeTurn{
		started: make(chan struct{}),
		tools:   []string{"bash"},
		echo:    "tool output",
		text:    "done",
		nota:    "checking the file",
		args:    `{"command":"pwd"}`,
	}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "run"}},
	})
	// note + tool_call + tool_call_update + agent_message_chunk + response
	ls = waitLines(t, h.out, 6)

	if ls[1].Params.Update.SessionUpdate != "agent_message_chunk" || ls[1].Params.Update.Content.Blocks[0].Text != "checking the file" {
		t.Fatalf("intermediate message not translated: %+v", ls[1])
	}
	tool := ls[2]
	if tool.Method != "session/update" || tool.Params.Update.SessionUpdate != "tool_call" {
		t.Fatalf("expected tool_call, got %+v", tool)
	}
	if tool.Params.Update.Title != "bash" {
		t.Fatalf("title should be the tool name: %q", tool.Params.Update.Title)
	}
	if tool.Params.Update.Kind != "other" {
		t.Fatalf("kind should be \"other\": %q", tool.Params.Update.Kind)
	}
	if tool.Params.Update.Status != "in_progress" {
		t.Fatalf("status should be in_progress: %q", tool.Params.Update.Status)
	}
	if tool.Params.Update.ToolCallID == "" {
		t.Fatal("tool_call without toolCallId")
	}
	var args map[string]any
	if err := json.Unmarshal(tool.Params.Update.RawInput, &args); err != nil || args["command"] != "pwd" {
		t.Fatalf("tool rawInput = %s, err %v", tool.Params.Update.RawInput, err)
	}

	ended := ls[3]
	if ended.Params.Update.SessionUpdate != "tool_call_update" || ended.Params.Update.Status != "completed" {
		t.Fatalf("tool did not complete: %+v", ended)
	}
	if ended.Params.Update.ToolCallID != tool.Params.Update.ToolCallID {
		t.Fatalf("completion references %q, want %q", ended.Params.Update.ToolCallID, tool.Params.Update.ToolCallID)
	}
	if n := len(ended.Params.Update.Content.Blocks); n != 1 || ended.Params.Update.Content.Blocks[0].Text != "tool output" {
		t.Fatalf("unexpected tool output: %+v", ended.Params.Update.Content)
	}
}

func TestRenderReasoningIsPersistableThoughtBeforeIntermediateTextAndTools(t *testing.T) {
	f := &fakeTurn{
		started: make(chan struct{}),
		thought: "**Plan**\n\nInspect the file before editing.",
		nota:    "I will inspect the file now.",
		tools:   []string{"read"},
		args:    `{"filePath":"main.go"}`,
		text:    "Done.",
	}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	id := ls[0].Result["sessionId"]
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": id,
		"prompt":    []map[string]string{{"type": "text", "text": "inspect and edit"}},
	})
	ls = waitLines(t, h.out, 7)

	wantKinds := []string{"agent_thought_chunk", "agent_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk"}
	for i, want := range wantKinds {
		got := ls[i+1].Params.Update.SessionUpdate
		if got != want {
			t.Fatalf("notification %d kind = %q, want %q; output:\n%s", i, got, want, h.out.String())
		}
	}
	if got := ls[1].Params.Update.Content.Blocks[0].Text; got != f.thought {
		t.Fatalf("thought text = %q, want %q", got, f.thought)
	}
	if got := ls[2].Params.Update.Content.Blocks[0].Text; got != f.nota {
		t.Fatalf("intermediate message = %q, want %q", got, f.nota)
	}
	if ls[6].Error != nil || string(ls[6].ID) != "3" {
		t.Fatalf("prompt response after thought/tool/answer = %+v", ls[6])
	}
}

func TestRenderFailedToolBecomesStatusFailed(t *testing.T) {
	f := &fakeTurn{started: make(chan struct{}), fails: []string{"bash"}, text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "run"}},
	})
	ls = waitLines(t, h.out, 3)
	if ls[1].Params.Update.Status != "failed" {
		t.Fatalf("a failed tool should become status failed, got %q", ls[1].Params.Update.Status)
	}
}

// guardrailTurn runs one turn that compacts and answers "ok", after an
// initialize with the given client capabilities (nil = none), and returns the
// raw update line of the compaction plus every update emitted.
func guardrailTurn(t *testing.T, caps map[string]any) (string, []line) {
	t.Helper()
	f := &fakeTurn{started: make(chan struct{}), compact: true, text: "ok"}
	h := setup(t, f)
	init := map[string]any{"protocolVersion": 1}
	if caps != nil {
		init["clientCapabilities"] = caps
	}
	h.send(t, MetInitialize, 1, init)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 2)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[1].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "run"}},
	})
	ls = waitLines(t, h.out, 5)
	raw := strings.Split(strings.TrimSuffix(h.out.String(), "\n"), "\n")
	return raw[2], ls[2:]
}

// The compaction notice is session state, not the model talking: it must never
// arrive as agent_message_chunk, which the client draws as an assistant bubble
// (one per prune, "[guardrail] proactive compaction…" a dozen times in 5 min).
func TestRenderGuardrailIsNotAMessage(t *testing.T) {
	for _, caps := range []map[string]any{nil, {"session": map[string]any{"compaction": map[string]any{}}}} {
		_, ups := guardrailTurn(t, caps)
		for _, u := range ups {
			for _, b := range u.Params.Update.Content.Blocks {
				if strings.Contains(b.Text, "guardrail") || strings.Contains(b.Text, "compaction") {
					t.Fatalf("compaction leaked into %s: %q", u.Params.Update.SessionUpdate, b.Text)
				}
			}
		}
		if ups[0].Params.Update.SessionUpdate == "agent_message_chunk" || ups[0].Params.Update.SessionUpdate == "agent_thought_chunk" {
			t.Fatalf("compaction must not be a chunk, got %s", ups[0].Params.Update.SessionUpdate)
		}
	}
}

// With the client's session.compaction capability, the spec's dedicated update.
func TestRenderGuardrailCompactionUpdateWhenAdvertised(t *testing.T) {
	raw, ups := guardrailTurn(t, map[string]any{"session": map[string]any{"compaction": map[string]any{}}})
	if ups[0].Params.Update.SessionUpdate != "compaction_update" {
		t.Fatalf("expected compaction_update, got %s", raw)
	}
	if ups[0].Params.Update.Status != "completed" || !strings.Contains(raw, `"compactionId":"cmp-`) {
		t.Fatalf("compaction_update needs an id and a terminal status: %s", raw)
	}
}

// Without the capability the spec forbids compaction_update; the OpenCode
// marker goes out instead, and a client that does not know it ignores it.
func TestRenderGuardrailMarkerWithoutCapability(t *testing.T) {
	raw, ups := guardrailTurn(t, nil)
	if ups[0].Params.Update.SessionUpdate != "session_info_update" {
		t.Fatalf("expected session_info_update, got %s", raw)
	}
	if !strings.Contains(raw, `"opencode/compaction"`) || !strings.Contains(raw, `"reason":"auto"`) || !strings.Contains(raw, `"status":"completed"`) {
		t.Fatalf("marker incomplete: %s", raw)
	}
}

func TestRenderTokensEmitsNothing(t *testing.T) {
	// Token usage is a local UI footer: there is no ACP event for it, and
	// inventing one would pollute the protocol.
	f := &fakeTurn{started: make(chan struct{}), tokens: true, text: "ok"}
	h := setup(t, f)
	h.send(t, MetSessionNovo, 2, map[string]any{"cwd": "."})
	ls := waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 3, map[string]any{
		"sessionId": ls[0].Result["sessionId"],
		"prompt":    []map[string]string{{"type": "text", "text": "run"}},
	})
	// response chunk + {} response = 2 lines. No tokens in between.
	ls = waitLines(t, h.out, 3)
	if len(ls) != 3 {
		t.Fatalf("Tokens should emit nothing; got %d lines:\n%s", len(ls), h.out.String())
	}
}
