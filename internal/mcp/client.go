package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/jsonrpc"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// MCP protocol implemented by this client.
const (
	// protocolVersion is the latest protocol version this client announces.
	// Older servers reply success with THEIR version in the result; the client
	// accepts it (MCP is backward compatible) and does not fail because of it.
	protocolVersion = "2025-06-18"

	// clientName / clientVersion go in the initialize clientInfo.
	clientName    = "barroso-harness"
	clientVersion = "1.0"

	maxStdioLine       = 1 << 20           // 1 MiB per JSON line in the stdio scanner
	maxHTTPBody        = jsonrpc.MaxBody   // cap on the HTTP response body
	defaultCallTimeout = 120 * time.Second // default cap for each JSON-RPC call
	killGracePeriod    = 5 * time.Second   // wait after closing stdin before Kill
)

// ── JSON-RPC 2.0 ─────────────────────────────────────────────────────────

// The protocol core lives in internal/jsonrpc, shared with the hosted web
// providers; these names keep this package's code reading as before.
type (
	message  = jsonrpc.Message
	rpcError = jsonrpc.Error
	request  = jsonrpc.Request
)

var (
	newNotification = jsonrpc.NewNotification
	newRequest      = jsonrpc.NewRequest
	jsonBytes       = jsonrpc.Bytes
	decodeResult    = jsonrpc.DecodeResult
	sameID          = jsonrpc.SameID
	idKey           = jsonrpc.IDKey
	summarizeHTTP   = jsonrpc.Summarize
	oneLine         = jsonrpc.OneLine
)

// ── MCP params and results ───────────────────────────────────────────────

// paramsInitialize is what we send in initialize. Empty Capabilities = this
// client offers nothing beyond tools (sampling/roots/prompts are left for a
// future version, and the server handles their absence).
type paramsInitialize struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      clientInfo     `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// resultInitialize is what we accept back — tolerant of missing capabilities,
// partial serverInfo and long instructions.
type resultInitialize struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ServerInfo      struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
	Instructions string `json:"instructions,omitempty"`
}

// mcpTool is one tools/list entry.
type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type resultToolsList struct {
	Tools []mcpTool `json:"tools"`
}

// paramsToolsCall is what we send in tools/call. Arguments is an object: the
// caller's argsJSON is parsed into a map and forwarded as such.
type paramsToolsCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// resultToolsCall is the tools/call result: content is a list of typed
// blocks and isError flags a server-side failure.
type resultToolsCall struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError"`
}

// contentBlock is one content item. Non-"text" types show up in the summary
// by Type — they do not break the call.
type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ── Client ───────────────────────────────────────────────────────────────

// Cliente is ONE connection to ONE MCP server, over either protocol
// transport: stdio (subprocess, newline-delimited JSON-RPC on stdin/stdout)
// and streamable HTTP (JSON-RPC POST; response as plain JSON or SSE).
//
// Tools are exposed with the RAW NAME the server publishes — this package
// adds no prefix. Namespacing to avoid collisions between servers (e.g.
// "github__create_issue") is the integrator's job, and Fonte() returns the
// server name precisely for that.
//
// respRouter dispatches responses read from the transport to WHOEVER is
// waiting for that id. Without it, two concurrent calls would compete for
// one message at a time on a shared channel: each discards the response that
// does not match — sometimes exactly the one the other needed, which then
// times out with nobody delivering it.
type respRouter struct {
	mu      sync.Mutex
	waiting map[string]chan message
}

func newRespRouter() *respRouter {
	return &respRouter{waiting: map[string]chan message{}}
}

// register creates the delivery channel for an id and returns it.
func (r *respRouter) register(id string) chan message {
	ch := make(chan message, 1)
	r.mu.Lock()
	r.waiting[id] = ch
	r.mu.Unlock()
	return ch
}

// forget removes the channel from the map (caller gave up: timeout or dead ctx).
func (r *respRouter) forget(id string) {
	r.mu.Lock()
	delete(r.waiting, id)
	r.mu.Unlock()
}

// deliver forwards the response to the owner of the id. Orphan responses
// (nobody waiting anymore) are dropped without blocking the reader.
func (r *respRouter) deliver(id string, m message) {
	r.mu.Lock()
	ch, ok := r.waiting[id]
	r.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- m: // buffer 1: never blocks the reader
	default:
	}
}

// Cliente is safe for concurrent use: each call reserves its own id and the
// response is routed by id, so different goroutines can call tools at the
// same time (stdio uses a single writer + a single reader).
type Cliente struct {
	serv      Servidor
	transport string
	source    string

	// http
	httpT *jsonrpc.HTTP

	// stdio
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	outbox   chan []byte   // write queue (single writer)
	router   *respRouter   // response → whoever waits for that id
	done     chan struct{} // closed on shutdown: unblocks the waiters
	writeErr chan error    // first write error (not fatal by itself)

	mu      sync.Mutex
	readErr error

	initialized bool
	info        resultInitialize
	caps        map[string]any

	nextID    atomic.Int64 // atomic.Int64 is 8-byte aligned on 386 too (iSH)
	timeout   time.Duration
	closeOnce sync.Once

	// toolsChanged is set when the server sends tools/list_changed.
	toolsChanged atomic.Bool
}

// shutdown closes the done channel exactly ONCE — called both by Fecha (local
// decision) and by the reader when the subprocess stdout ends (server
// decision). Closing twice would panic, hence the Once.
func (c *Cliente) shutdown() {
	c.closeOnce.Do(func() { close(c.done) })
}

// Novo validates the configuration and returns a client NOT YET connected.
// Opening is a separate step (Abre) because the connection may fail and the
// caller decides what to do — including continuing without this server.
func Novo(s Servidor) (*Cliente, error) {
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	transport, err := normalizeTransport(s.Tipo)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	return &Cliente{
		serv:      s,
		transport: transport,
		source:    s.Nome,
		outbox:    make(chan []byte, 32),
		router:    newRespRouter(),
		done:      make(chan struct{}),
		writeErr:  make(chan error, 1),
		timeout:   s.timeoutDuration(),
	}, nil
}

// Fonte returns Servidor.Nome — the server name, for prefixing/namespacing.
func (c *Cliente) Fonte() string { return c.source }

// ToolsChanged reports (and clears) whether the server announced that its
// tool list changed since the last call.
func (c *Cliente) ToolsChanged() bool { return c.toolsChanged.Swap(false) }

// Falhou reports whether the connection broke after opening (the stdio
// reader saw the subprocess end or a read error). The UI shows such a server
// with the error dot instead of "Connected".
func (c *Cliente) Falhou() bool { return c.currentReadErr() != nil }

// Transporto returns the canonical transport form: "stdio" or "http".
func (c *Cliente) Transporto() string { return c.transport }

// InfoServidor returns what initialize negotiated (defensive copy). Before
// opening, it returns the zero value.
func (c *Cliente) InfoServidor() resultInitialize {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// Capacidades returns the capabilities map the server announced in
// initialize (defensive copy; nil before opening or if none were announced).
func (c *Cliente) Capacidades() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.caps) == 0 {
		return nil
	}
	out := make(map[string]any, len(c.caps))
	for k, v := range c.caps {
		out[k] = v
	}
	return out
}

// nextRequestID reserves a monotonic id (id-based correlation, required
// because the server may emit notifications between responses).
func (c *Cliente) nextRequestID() int64 { return c.nextID.Add(1) }

// Abre connects and performs the handshake (initialize +
// notifications/initialized). After Abre the client is ready for
// Ferramentas and Chamar.
func (c *Cliente) Abre(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	switch c.transport {
	case transportStdio:
		return c.openStdio(ctx)
	case transportHTTP:
		return c.openHTTP(ctx)
	default:
		return fmt.Errorf("mcp: server %q: transport %q not implemented", c.source, c.transport)
	}
}

// ── stdio ────────────────────────────────────────────────────────────────

func (c *Cliente) openStdio(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.serv.Command, c.serv.Args...)
	cmd.Dir = serverWorkDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("mcp: server %q: stdin: %w", c.source, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("mcp: server %q: stdout: %w", c.source, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("mcp: server %q: stderr: %w", c.source, err)
	}
	// Environment = the process's own + the server's. On Windows the format is
	// already the expected one ("K=V" blocks separated by NUL); nothing to convert.
	cmd.Env = append(os.Environ(), envSlice(c.serv.Env)...)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("mcp: server %q: start: %w", c.source, err)
	}
	c.cmd = cmd
	c.stdin = stdin

	// stderr is drained and discarded: MCP servers use the error stream as a
	// debug log and, if nobody read it, the pipe would fill up and block the
	// subprocess. We never print it — the noise may contain secrets.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	// One reader (newline-delimited JSON) and one writer: no interleaving.
	go c.readLoopStdio(stdout)
	go c.writeLoop()

	if err := c.handshake(ctx); err != nil {
		_ = c.Fecha() // do not leave a zombie subprocess half-connected
		return err
	}
	return nil
}

// readLoopStdio reads one JSON message per line. Buffer up to 1 MiB — MCP
// stdio is newline-delimited JSON-RPC (LSP's Content-Length framing is NOT
// part of this protocol).
//
// Notifications between responses are dropped HERE: the client correlates by
// id, and notifications have no id — keeping them would only fill the channel
// and confuse the waiter. (The client does not and need not subscribe to MCP
// notifications.)
func (c *Cliente) readLoopStdio(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxStdioLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			continue // non-JSON noise: ignored, does not kill the session
		}
		if !m.HasID() {
			// notifications/tools/list_changed: the server's tools changed; the
			// owner refetches them at its next check (ToolsChanged).
			if m.Method == "notifications/tools/list_changed" {
				c.toolsChanged.Store(true)
			}
			continue // other notifications: no id to correlate
		}
		// Route by id: the message goes straight to its owner, without a
		// shared channel (other concurrent calls never pick it up).
		c.router.deliver(idKey(m.ID), m)
	}
	// stdout EOF (or a line over the buffer): the server is gone. Record the
	// failure and signal the end to pending waiters.
	c.setReadErr(sc.Err())
	c.shutdown()
}

// writeLoop is the ONLY stdin writer: it takes the already-serialized body
// and appends a newline (message delimiter). The c.outbox channel is NEVER
// closed: closing it would panic ("send on closed channel") in any concurrent
// caller that was sending. The stop signal is c.done, which Fecha closes —
// then the writer exits.
func (c *Cliente) writeLoop() {
	stdin := c.stdin
	if stdin == nil {
		return
	}
	for {
		select {
		case body := <-c.outbox:
			if len(body) == 0 {
				continue
			}
			if _, err := stdin.Write(append(body, '\n')); err != nil {
				// Broken pipe: signal whoever waits and stop writing.
				// stdin stays as is — Fecha is the one that closes it.
				c.signalWriteErr(err)
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *Cliente) signalWriteErr(err error) {
	select {
	case c.writeErr <- err:
	default:
	}
}

// setReadErr stores the reader's first error/EOF.
func (c *Cliente) setReadErr(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.mu.Unlock()
}

func (c *Cliente) currentReadErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// serverWorkDir: the MCP subprocess runs in the agent's directory, not the
// harness's — stdio servers usually resolve paths relative to the project.
func serverWorkDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

// ── http ─────────────────────────────────────────────────────────────────

func (c *Cliente) openHTTP(_ context.Context) error {
	c.httpT = &jsonrpc.HTTP{
		URL:       strings.TrimSpace(c.serv.URL),
		Headers:   c.serv.Headers,
		UserAgent: dealer.UserAgent(),
		Client:    &http.Client{Timeout: c.timeout},
		Timeout:   c.timeout,
		MaxBody:   maxHTTPBody,
	}
	return c.handshake(context.Background())
}

// ── handshake ────────────────────────────────────────────────────────────

// handshake performs initialize (with id correlation, ignoring
// notifications) and notifications/initialized. Idempotent: subsequent calls
// are no-ops.
func (c *Cliente) handshake(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	already := c.initialized
	c.mu.Unlock()
	if already {
		return nil
	}

	req := newRequest(c.nextRequestID(), "initialize", paramsInitialize{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo:      clientInfo{Name: clientName, Version: clientVersion},
	})
	res, err := c.send(ctx, req)
	if err != nil {
		return fmt.Errorf("mcp: server %q: initialize: %w", c.source, err)
	}
	var initRes resultInitialize
	if err := decodeResult(res.Result, &initRes); err != nil {
		return fmt.Errorf("mcp: server %q: initialize: %w", c.source, err)
	}
	c.mu.Lock()
	c.info = initRes
	c.caps = initRes.Capabilities
	c.initialized = true
	c.mu.Unlock()

	if err := c.sendRaw(ctx, newNotification("notifications/initialized", nil)); err != nil {
		return fmt.Errorf("mcp: server %q: notifications/initialized: %w", c.source, err)
	}
	return nil
}

// ── request → response ───────────────────────────────────────────────────

// send fires the request and returns the response with the matching id.
func (c *Cliente) send(ctx context.Context, r request) (message, error) {
	if len(r.Body) == 0 {
		return message{}, fmt.Errorf("mcp: request %s failed to serialize", r.Method)
	}
	if c.transport == transportHTTP {
		return c.requestHTTP(ctx, r)
	}
	return c.requestStdio(ctx, r)
}

// requestStdio puts the body on the single writer's queue and waits for the
// response.
func (c *Cliente) requestStdio(ctx context.Context, r request) (message, error) {
	wait := c.deadline(ctx)
	select {
	case c.outbox <- r.Body:
	case <-time.After(wait):
		return message{}, fmt.Errorf("mcp: server %q: write queue full while sending %s", c.source, r.Method)
	case <-ctx.Done():
		return message{}, ctx.Err()
	case <-c.done:
		return message{}, fmt.Errorf("mcp: server %q: connection closed while sending %s", c.source, r.Method)
	}

	rawID := jsonBytes(r.ID)
	key := idKey(rawID)
	mine := c.router.register(key)
	defer c.router.forget(key)

	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case m := <-mine:
			if m.Error != nil {
				return message{}, m.Error
			}
			return m, nil
		case err := <-c.writeErr:
			if err != nil {
				return message{}, fmt.Errorf("mcp: server %q: write: %w", c.source, err)
			}
		case <-timer.C:
			return message{}, fmt.Errorf("mcp: server %q: timed out after %s waiting for response to %s", c.source, wait, r.Method)
		case <-ctx.Done():
			return message{}, ctx.Err()
		case <-c.done:
			return message{}, c.connectionError(r.Method)
		}
	}
}

// connectionError explains the end of the connection: plain EOF, IO error or
// local shutdown via Fecha.
func (c *Cliente) connectionError(method string) error {
	select {
	case <-c.done:
	default:
		return fmt.Errorf("mcp: server %q: client closed before %s", c.source, method)
	}
	if err := c.currentReadErr(); err != nil {
		return fmt.Errorf("mcp: server %q: read: %w", c.source, err)
	}
	return fmt.Errorf("mcp: server %q: server closed the connection during %s", c.source, method)
}

// requestHTTP performs the POST. A 404 on a call carrying a session id means
// the server dropped the session: the client initializes again ONCE and
// repeats the call (never for initialize itself, which would loop).
func (c *Cliente) requestHTTP(ctx context.Context, r request) (message, error) {
	m, err := c.httpT.Call(ctx, r)
	if errors.Is(err, jsonrpc.ErrSessionExpired) && r.Method != "initialize" {
		c.mu.Lock()
		c.initialized = false
		c.mu.Unlock()
		if herr := c.handshake(ctx); herr != nil {
			return message{}, herr
		}
		m, err = c.httpT.Call(ctx, r)
	}
	if err != nil {
		var rpc *rpcError
		if errors.As(err, &rpc) {
			return message{}, rpc
		}
		return message{}, fmt.Errorf("mcp: server %q: %w", c.source, err)
	}
	return m, nil
}

// sendRaw sends a notification (no response to wait for). On stdio it goes
// to the writer; on HTTP it is the POST, where an empty 202/200 is valid.
func (c *Cliente) sendRaw(ctx context.Context, m message) error {
	if c.transport == transportHTTP {
		if err := c.httpT.Notify(ctx, m); err != nil {
			return fmt.Errorf("mcp: server %q: %w", c.source, err)
		}
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	wait := c.deadline(ctx)
	select {
	case c.outbox <- b:
		return nil
	case <-time.After(wait):
		return fmt.Errorf("write queue full")
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("mcp: server %q: connection closed while notifying %s", c.source, m.Method)
	}
}

// deadline reconciles the server timeout with the ctx one (the smaller wins).
func (c *Cliente) deadline(ctx context.Context) time.Duration {
	d := c.timeout
	if dl, ok := ctx.Deadline(); ok {
		if rest := time.Until(dl); rest < d {
			d = rest
		}
	}
	if d <= 0 {
		d = defaultCallTimeout
	}
	return d
}

// ── high-level MCP calls ─────────────────────────────────────────────────

// Ferramentas performs the handshake (if not done yet) and tools/list; each
// server tool becomes a tools.Spec whose Exec calls Chamar with the raw name.
// A server with no tools returns an empty list — not an error.
func (c *Cliente) Ferramentas(ctx context.Context) ([]tools.Spec, error) {
	if err := c.handshake(ctx); err != nil {
		return nil, err
	}
	res, err := c.send(ctx, newRequest(c.nextRequestID(), "tools/list", nil))
	if err != nil {
		return nil, fmt.Errorf("mcp: server %q: tools/list: %w", c.source, err)
	}
	var list resultToolsList
	if err := decodeResult(res.Result, &list); err != nil {
		return nil, fmt.Errorf("mcp: server %q: tools/list: %w", c.source, err)
	}
	specs := make([]tools.Spec, 0, len(list.Tools))
	for _, t := range list.Tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		specs = append(specs, c.specFor(t, name))
	}
	return specs, nil
}

// specFor converts an MCP tool into a harness Spec. Schema is the server's
// inputSchema; if it did not publish one (or it is empty), it falls back to
// an unconstrained object — no provider accepts missing parameters.
func (c *Cliente) specFor(t mcpTool, name string) tools.Spec {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = freeObjectSchema()
	}
	// The server name leads the description: it groups the tools in the
	// deferred catalog and lets tool_search match "playwright", "github"...
	desc := oneLine(t.Description)
	if desc == "" {
		desc = "MCP tool " + name
	}
	desc = "[" + c.source + "] " + desc
	serv := c.source
	return tools.Spec{
		Name:        name,
		Description: desc,
		Schema:      schema,
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			args := string(bytes.TrimSpace(raw))
			if args == "" || args == "null" {
				args = "{}"
			}
			out, err := c.Chamar(ctx, name, args)
			if err != nil {
				// Server error becomes text + Go error: the provider sees the
				// failure and can react, and the runtime does not treat it as a
				// session crash.
				if out == "" {
					return "", fmt.Errorf("mcp %s.%s: %w", serv, name, err)
				}
				return out, fmt.Errorf("mcp %s.%s: %w", serv, name, err)
			}
			return out, nil
		},
	}
}

func freeObjectSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// Chamar runs tools/call. argsJSON is the arguments JSON; empty or "null"
// become {}. A server isError becomes a Go error carrying its message.
func (c *Cliente) Chamar(ctx context.Context, name, argsJSON string) (string, error) {
	if err := c.handshake(ctx); err != nil {
		return "", err
	}
	args := map[string]any{}
	clean := strings.TrimSpace(argsJSON)
	if clean != "" && clean != "null" {
		if err := json.Unmarshal([]byte(clean), &args); err != nil {
			return "", fmt.Errorf("mcp: invalid arguments for %q: %w", name, err)
		}
	}
	res, err := c.send(ctx, newRequest(c.nextRequestID(), "tools/call", paramsToolsCall{Name: name, Arguments: args}))
	if err != nil {
		return "", fmt.Errorf("mcp: server %q: tools/call %s: %w", c.source, name, err)
	}
	var out resultToolsCall
	if err := decodeResult(res.Result, &out); err != nil {
		return "", fmt.Errorf("mcp: server %q: tools/call %s: %w", c.source, name, err)
	}
	text := joinContent(out.Content)
	if out.IsError {
		if text == "" {
			text = "(server reported an error without content)"
		}
		return text, fmt.Errorf("MCP server %q reported an error in %s: %s", c.source, name, oneLine(text))
	}
	return text, nil
}

// joinContent concatenates the result blocks. Text blocks are separated by
// newlines; blocks of other types become a minimal bracketed marker (images
// and embedded resources have no text for the provider to read).
//
// Text content is kept RAW. A server may reply "echo: " with the space being
// meaningful (formatted value), and trimming it would erase tool output.
func joinContent(blocks []contentBlock) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		switch strings.ToLower(strings.TrimSpace(b.Type)) {
		case "", "text":
			parts = append(parts, b.Text) // raw: no trim
		case "image":
			parts = append(parts, fmt.Sprintf("[image %s omitted]", orDefault(b.MimeType, "image/*")))
		case "resource":
			target := strings.TrimSpace(b.URI)
			if target == "" {
				target = orDefault(b.Name, "resource")
			}
			parts = append(parts, fmt.Sprintf("[resource %s]", target))
		default:
			parts = append(parts, fmt.Sprintf("[%s block omitted]", orDefault(b.Type, "unknown")))
		}
	}
	return strings.Join(parts, "\n")
}

func orDefault(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return v
}

// ── closing ──────────────────────────────────────────────────────────────

// Fecha closes the connection. On stdio: closes stdin (a well-behaved server
// exits on EOF), waits up to 5 s and only then sends Kill. On HTTP: ends the
// server-side session (best-effort DELETE), so a gateway does not keep a
// session per process. Idempotent.
//
// Order matters: the done channel is signalled first, so pending senders give
// up instead of blocking; the outbox channel is never closed, so the single
// writer never writes to a closed pipe.
func (c *Cliente) Fecha() error {
	c.closeOnce.Do(func() {
		close(c.done) // whoever waits for a response wakes up here

		// HTTP first: the session lives on the server and outlives the process,
		// so it is the half that MUST be ended explicitly. Best-effort — a
		// gateway that ignores DELETE expires sessions on its own.
		if c.httpT != nil {
			_ = c.httpT.Close(context.Background())
		}

		if c.stdin != nil {
			_ = c.stdin.Close() // EOF: the server exits on its own
		}
		// c.outbox is NOT closed: it has several concurrent senders, and closing
		// a channel with an active sender panics in Go. The writer notices the
		// end via c.done; the channel is left to the GC.

		if c.cmd == nil || c.cmd.Process == nil {
			return
		}
		waited := make(chan error, 1)
		go func() { waited <- c.cmd.Wait() }()
		select {
		case <-waited:
			return // exited on EOF — normal path
		case <-time.After(killGracePeriod):
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			<-waited
		}
	})
	return nil
}
