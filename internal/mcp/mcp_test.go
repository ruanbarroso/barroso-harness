package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── fake HTTP MCP server ─────────────────────────────────────────────────

// fakeMCP serves the three methods the client uses: initialize, tools/list
// and tools/call. It replies in the format chosen by the test (plain JSON or SSE).
type fakeMCP struct {
	mu   sync.Mutex
	seen []string // methods received, in order

	initResp  any
	toolsResp any
	callResp  any

	status int // != 0 forces this status on every response (401 test)

	sse           bool // frame responses as SSE events
	batch         bool // reply to tools/call as a JSON-RPC batch
	noContentType bool // generic Content-Type on SSE (forces sniffing)

	deleted int // DELETE (session close) requests received
}

// write emits a response in the format requested by the test.
func (f *fakeMCP) write(w http.ResponseWriter, r *fakeMCP, result any, id any) {
	msg := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	if f.sse {
		b, _ := json.Marshal(msg)
		if !f.noContentType {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, ":ping\n\nevent: message\ndata: %s\n\n", b)
		return
	}
	if f.batch {
		b, _ := json.Marshal([]map[string]any{msg})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
		return
	}
	b, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	status := f.status
	f.mu.Unlock()

	// 401: authentication refused before any processing.
	if status != 0 {
		http.Error(w, "unauthorized", status)
		return
	}

	if req.Method != http.MethodPost {
		// DELETE (session close) is the only other method the client sends.
		if req.Method == http.MethodDelete {
			f.mu.Lock()
			f.deleted++
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "method not supported", http.StatusMethodNotAllowed)
		return
	}

	var m message
	if err := json.NewDecoder(req.Body).Decode(&m); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	f.mu.Lock()
	f.seen = append(f.seen, m.Method)
	f.mu.Unlock()

	// Notifications have no id: nothing to reply, just a 200/202.
	if !m.HasID() {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch m.Method {
	case "initialize":
		// Streamable HTTP hands out a session id in initialize; without one the
	// client has nothing to close.
	if m.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", "sess-1")
	}
	f.write(w, f, f.initResp, idForResponse(m.ID))
	case "tools/list":
		if f.toolsResp == nil {
			f.toolsResp = map[string]any{"tools": []any{}}
		}
		f.write(w, f, f.toolsResp, idForResponse(m.ID))
	case "tools/call":
		if f.callResp == nil {
			f.callResp = map[string]any{"content": []any{}}
		}
		f.write(w, f, f.callResp, idForResponse(m.ID))
	default:
		http.Error(w, "unknown method: "+m.Method, http.StatusNotFound)
	}
}

// idForResponse returns the request's raw id for the response — JSON-RPC
// requires the response to carry the request id, not an invented one.
func idForResponse(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("0")
	}
	return raw
}

// seenMethods returns the list of received methods (copy).
func (f *fakeMCP) seenMethods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	copy(out, f.seen)
	return out
}

// deletedCount returns how many session-close DELETEs the server received.
func (f *fakeMCP) deletedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted
}

// newFake starts an httptest.Server and returns it plus its URL.
func newFake(t *testing.T, f *fakeMCP) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv, srv.URL
}

// initializeResult is what a real MCP server returns on initialize.
func initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": true},
		},
		"serverInfo": map[string]any{"name": "fake", "version": "0.1"},
	}
}

// ── HTTP tests ───────────────────────────────────────────────────────────

// Handshake + tools/list with TWO tools, one with a full schema.
func TestHTTPHandshakeAndList(t *testing.T) {
	f := &fakeMCP{
		initResp: initializeResult(),
		toolsResp: map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Returns its input",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"msg": map[string]any{"type": "string"},
						},
						"required": []string{"msg"},
					},
				},
				map[string]any{
					"name":        "sum",
					"description": "Adds two numbers",
					// no inputSchema: the client must make up a free object
				},
			},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	if got := c.Fonte(); got != "fake" {
		t.Errorf("Fonte() = %q, want %q", got, "fake")
	}
	if got := c.Transporto(); got != "http" {
		t.Errorf("Transporto() = %q, want %q", got, "http")
	}

	specs, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("Ferramentas returned %d specs, want 2", len(specs))
	}
	if specs[0].Name != "echo" || specs[1].Name != "sum" {
		t.Fatalf("names = %q, %q; want echo, sum", specs[0].Name, specs[1].Name)
	}
	if specs[0].Description != "[fake] Returns its input" { // the server name leads the description
		t.Errorf("description = %q", specs[0].Description)
	}

	// The first tool's schema must be the server's, untouched.
	props, ok := specs[0].Schema["properties"].(map[string]any)
	if !ok || len(props) != 1 {
		t.Fatalf("Schema[properties] = %#v", specs[0].Schema["properties"])
	}
	if _, ok := props["msg"]; !ok {
		t.Errorf("Schema missing the msg property: %#v", props)
	}
	if specs[0].Schema["type"] != "object" {
		t.Errorf("Schema[type] = %v", specs[0].Schema["type"])
	}

	// The second one, without a schema, gets the free object.
	if specs[1].Schema["type"] != "object" {
		t.Errorf("default Schema[type] = %v, want object", specs[1].Schema["type"])
	}
	if _, ok := specs[1].Schema["properties"].(map[string]any); !ok {
		t.Errorf("default Schema without properties: %#v", specs[1].Schema)
	}

	// initialize + notifications/initialized + tools/list, in that order.
	seen := f.seenMethods()
	wanted := []string{"initialize", "notifications/initialized", "tools/list"}
	if len(seen) < len(wanted) {
		t.Fatalf("seen methods = %v", seen)
	}
	for i, w := range wanted {
		if seen[i] != w {
			t.Fatalf("method %d = %q, want %q (seen: %v)", i, seen[i], w, seen)
		}
	}

	// The handshake stored what the server negotiated.
	info := c.InfoServidor()
	if info.ServerInfo.Name != "fake" || info.ProtocolVersion != "2025-06-18" {
		t.Errorf("InfoServidor = %+v", info)
	}
	if caps := c.Capacidades(); len(caps) == 0 {
		t.Errorf("empty Capabilities; want the server's map")
	}
}

// tools/call echoing the arguments, via Spec (the path the runtime uses).
func TestHTTPCallEchoesArguments(t *testing.T) {
	f := &fakeMCP{
		initResp: initializeResult(),
		toolsResp: map[string]any{
			"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Returns its input",
					"inputSchema": map[string]any{"type": "object"},
				},
			},
		},
		callResp: map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "first"},
				map[string]any{"type": "text", "text": "second"},
			},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	// Multiple text blocks are concatenated.
	out, err := c.Chamar(context.Background(), "echo", `{"msg":"hi"}`)
	if err != nil {
		t.Fatalf("Chamar: %v", err)
	}
	if out != "first\nsecond" {
		t.Errorf("Chamar = %q, want %q", out, "first\nsecond")
	}

	// Empty and "null" arguments both become {} without blowing up.
	if _, err := c.Chamar(context.Background(), "echo", ""); err != nil {
		t.Errorf("Chamar with empty args: %v", err)
	}
	if _, err := c.Chamar(context.Background(), "echo", "null"); err != nil {
		t.Errorf("Chamar with null args: %v", err)
	}

	// Non-JSON arguments must yield a ONE-line error, not a panic.
	if _, err := c.Chamar(context.Background(), "echo", "{"); err == nil {
		t.Errorf("Chamar with broken JSON should fail")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("multi-line error: %q", err.Error())
	}
}

// A server isError becomes a Go error carrying ITS message.
func TestHTTPCallMapsIsError(t *testing.T) {
	f := &fakeMCP{
		initResp: initializeResult(),
		toolsResp: map[string]any{
			"tools": []any{
				map[string]any{"name": "breaks", "inputSchema": map[string]any{"type": "object"}},
			},
		},
		callResp: map[string]any{
			"isError": true,
			"content": []any{
				map[string]any{"type": "text", "text": "catastrophic server failure"},
			},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	out, err := c.Chamar(context.Background(), "breaks", "{}")
	if err == nil {
		t.Fatalf("Chamar should return an error when isError=true")
	}
	if !strings.Contains(err.Error(), "catastrophic server failure") {
		t.Errorf("error = %q; want the server's message", err.Error())
	}
	if !strings.Contains(out, "catastrophic server failure") {
		t.Errorf("output = %q; want the server's text along with it", out)
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("multi-line error: %q", err.Error())
	}
}

// 401: handshake fails with a useful status and message.
func TestHTTP401OnHandshake(t *testing.T) {
	f := &fakeMCP{status: http.StatusUnauthorized}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err == nil {
		t.Fatalf("Abre should fail with 401")
	} else {
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error = %q; want status 401", err.Error())
		}
		if !strings.Contains(err.Error(), "unauthorized") {
			t.Errorf("error = %q; want the server's body", err.Error())
		}
		if strings.Contains(err.Error(), "\n") {
			t.Errorf("multi-line error: %q", err.Error())
		}
	}
	c.Fecha() // idempotent, even without an actual connection
}

// SSE response: the client extracts the JSON-RPC from the data: lines.
func TestHTTPSSEResponse(t *testing.T) {
	f := &fakeMCP{
		sse:      true,
		initResp: initializeResult(),
		toolsResp: map[string]any{
			"tools": []any{
				map[string]any{"name": "sse", "inputSchema": map[string]any{"type": "object"}},
			},
		},
		callResp: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "response via stream"}},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx := context.Background()
	if err := c.Abre(ctx); err != nil {
		t.Fatalf("Abre (SSE): %v", err)
	}
	defer c.Fecha()

	specs, err := c.Ferramentas(ctx)
	if err != nil {
		t.Fatalf("Ferramentas (SSE): %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "sse" {
		t.Fatalf("Ferramentas (SSE) = %+v", specs)
	}
	out, err := c.Chamar(ctx, "sse", `{"x":1}`)
	if err != nil {
		t.Fatalf("Chamar (SSE): %v", err)
	}
	if out != "response via stream" {
		t.Errorf("Chamar (SSE) = %q", out)
	}
}

// SSE with a generic Content-Type: the client detects it from the body.
func TestHTTPSSEResponseWithoutContentType(t *testing.T) {
	f := &fakeMCP{
		sse:           true,
		noContentType: true,
		initResp:      initializeResult(),
		toolsResp: map[string]any{
			"tools": []any{map[string]any{"name": "sniff", "inputSchema": map[string]any{"type": "object"}}},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre (sniffed SSE): %v", err)
	}
	defer c.Fecha()
	if _, err := c.Ferramentas(context.Background()); err != nil {
		t.Fatalf("Ferramentas (sniffed SSE): %v", err)
	}
}

// JSON-RPC batch response: the response with the matching id is found in the array.
func TestHTTPBatchResponse(t *testing.T) {
	f := &fakeMCP{
		batch:    true,
		initResp: initializeResult(),
		callResp: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "came in a batch"}},
		},
	}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre (batch): %v", err)
	}
	defer c.Fecha()
	out, err := c.Chamar(context.Background(), "anything", "{}")
	if err != nil {
		t.Fatalf("Chamar (batch): %v", err)
	}
	if out != "came in a batch" {
		t.Errorf("Chamar (batch) = %q", out)
	}
}

// The HTTP connection must announce Accept as the MCP spec requires.
func TestHTTPRequiredAcceptHeader(t *testing.T) {
	var accept, ua, contentType string
	f := &fakeMCP{initResp: initializeResult()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		accept = req.Header.Get("Accept")
		ua = req.Header.Get("User-Agent")
		contentType = req.Header.Get("Content-Type")
		f.ServeHTTP(w, req)
	}))
	defer srv.Close()

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: srv.URL, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()

	if !strings.Contains(accept, "application/json") || !strings.Contains(accept, "text/event-stream") {
		t.Errorf("Accept = %q; the MCP spec requires both", accept)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type = %q", contentType)
	}
	if !strings.Contains(ua, "opencode/") {
		t.Errorf("User-Agent = %q; want the official client's", ua)
	}
}

// Headers configured on the Servidor reach the request.
func TestHTTPServerHeaders(t *testing.T) {
	var seenAuth string
	f := &fakeMCP{initResp: initializeResult()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		seenAuth = req.Header.Get("Authorization")
		f.ServeHTTP(w, req)
	}))
	defer srv.Close()

	c, err := Novo(Servidor{
		Nome:    "fake",
		Tipo:    "http",
		URL:     srv.URL,
		Timeout: 5,
		Headers: map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	defer c.Fecha()
	if seenAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", seenAuth)
	}
}

// Dead server URL: ONE-line error, no panic.
func TestHTTPServerUnreachable(t *testing.T) {
	c, err := Novo(Servidor{Nome: "dead", Tipo: "http", URL: "http://127.0.0.1:1/mcp", Timeout: 2})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := c.Abre(ctx); err == nil {
		t.Fatalf("Abre should fail with an unreachable server")
	} else if strings.Contains(err.Error(), "\n") {
		t.Errorf("multi-line error: %q", err.Error())
	}
}

// ── config validation ────────────────────────────────────────────────────

func TestServerValidation(t *testing.T) {
	cases := []struct {
		name       string
		serv       Servidor
		shouldFail bool // true = Novo must return an error
	}{
		{"valid stdio", Servidor{Nome: "s", Tipo: "stdio", Command: "go"}, false},
		{"valid http", Servidor{Nome: "h", Tipo: "http", URL: "http://x"}, false},
		{"uppercase http", Servidor{Nome: "h", Tipo: "HTTP", URL: "http://x"}, false},
		{"streamable-http", Servidor{Nome: "h", Tipo: "streamable-http", URL: "http://x"}, false},
		{"no name", Servidor{Tipo: "http", URL: "http://x"}, true},
		{"unknown type", Servidor{Nome: "x", Tipo: "websocket", URL: "http://x"}, true},
		{"stdio without command", Servidor{Nome: "x", Tipo: "stdio"}, true},
		{"http without url", Servidor{Nome: "x", Tipo: "http"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Novo(c.serv)
			if !c.shouldFail && err != nil {
				t.Errorf("Novo(%+v) failed: %v", c.serv, err)
			}
			if c.shouldFail && err == nil {
				t.Errorf("Novo(%+v) should fail", c.serv)
			}
		})
	}
}

// envSlice sorts and formats K=V pairs.
func TestEnvSlice(t *testing.T) {
	got := envSlice(map[string]string{"B": "2", "A": "1"})
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Errorf("envSlice = %v; want [A=1 B=2] sorted", got)
	}
	if envSlice(nil) != nil {
		t.Errorf("envSlice(nil) should be nil")
	}
}

// Per-server timeout: default when not configured.
func TestServerTimeout(t *testing.T) {
	if got := (Servidor{Timeout: 30}).timeoutDuration(); got != 30*time.Second {
		t.Errorf("timeoutDuration = %v", got)
	}
	if got := (Servidor{}).timeoutDuration(); got != defaultCallTimeout {
		t.Errorf("default timeoutDuration = %v; want %v", got, defaultCallTimeout)
	}
	if got := (Servidor{Timeout: -5}).timeoutDuration(); got != defaultCallTimeout {
		t.Errorf("negative timeoutDuration = %v; want the default", got)
	}
}

// ── id correlation ───────────────────────────────────────────────────────

func TestIDCorrelation(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"1", "1", true},
		{"1", "2", false},
		{`"1"`, "1", true}, // number vs string with the same content
		{`"abc"`, `"abc"`, true},
		{"", "", true},
		{"1", "", false},
		{"null", "null", true},
	}
	for _, c := range cases {
		got := sameID(json.RawMessage(c.a), json.RawMessage(c.b))
		if got != c.want {
			t.Errorf("sameID(%s, %s) = %v; want %v", c.a, c.b, got, c.want)
		}
	}
}

// ── connector ────────────────────────────────────────────────────────────

func TestConectarTodosPerServerError(t *testing.T) {
	fOk := &fakeMCP{initResp: initializeResult(), toolsResp: map[string]any{"tools": []any{}}}
	_, urlOk := newFake(t, fOk)

	ctx := context.Background()
	clients, errs := ConectarTodos(ctx, []Servidor{
		{Nome: "good", Tipo: "http", URL: urlOk, Timeout: 5},
		{Nome: "bad-no-url", Tipo: "http", Timeout: 5},
		{Nome: "bad-dead", Tipo: "http", URL: "http://127.0.0.1:1/mcp", Timeout: 1},
	})

	// The good server connects; the bad ones come back as error items.
	if len(clients) != 1 {
		t.Fatalf("clients = %d; want 1 (only the good one)", len(clients))
	}
	if clients[0].Fonte() != "good" {
		t.Errorf("client = %q; want good", clients[0].Fonte())
	}
	if len(errs) != 2 {
		t.Fatalf("errors = %d; want 2 (%+v)", len(errs), errs)
	}
	for _, e := range errs {
		if e.Nome == "" {
			t.Errorf("ErroServidor without name: %+v", e)
		}
		if e.Err == nil {
			t.Errorf("ErroServidor without error: %+v", e)
		}
		msg := e.Error()
		if strings.Contains(msg, "\n") {
			t.Errorf("multi-line error: %q", msg)
		}
		if !strings.Contains(msg, e.Nome) {
			t.Errorf("error %q does not mention the server name", msg)
		}
	}

	// Stable order: errors come in the order the servers were given.
	if errs[0].Nome != "bad-no-url" || errs[1].Nome != "bad-dead" {
		t.Errorf("error order = %q, %q; want bad-no-url, bad-dead",
			errs[0].Nome, errs[1].Nome)
	}

	// ErroServidor unwraps for errors.Is/As to the original error.
	target := errs[0]
	var as ErroServidor
	if !errorsAs(target, &as) {
		t.Errorf("ErroServidor does not honour Unwrap")
	}
}

func TestConectarTodosEmpty(t *testing.T) {
	clients, errs := ConectarTodos(context.Background(), nil)
	if clients != nil || errs != nil {
		t.Errorf("ConectarTodos(nil) = %v, %v; want nil, nil", clients, errs)
	}
}

func TestFechaTodos(t *testing.T) {
	f := &fakeMCP{initResp: initializeResult()}
	_, url := newFake(t, f)
	c, err := Novo(Servidor{Nome: "x", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	// FechaTodos is idempotent on the client (Fecha already is).
	if err := FechaTodos([]*Cliente{c, nil}); err != nil {
		t.Errorf("FechaTodos: %v", err)
	}
	if err := FechaTodos([]*Cliente{c}); err != nil {
		t.Errorf("repeated FechaTodos: %v", err)
	}
}

// errorsAs checks that ErroServidor unwraps to the original error (Unwrap).
func errorsAs(err error, target *ErroServidor) bool {
	var e ErroServidor
	if !asErroServidor(err, &e) {
		return false
	}
	*target = e
	return true
}

// asErroServidor is errors.As without importing errors in this test file
// (keeps the import set lean).
func asErroServidor(err error, target *ErroServidor) bool {
	for err != nil {
		if e, ok := err.(ErroServidor); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Ensure the response body does not leak into the error: only what is needed.
func TestSummarizeHTTP(t *testing.T) {
	if got := summarizeHTTP([]byte("  line\n\nother  "), 100); got != "line other" {
		t.Errorf("summarizeHTTP = %q; want a single line", got)
	}
	if got := summarizeHTTP(nil, 100); got != "(empty body)" {
		t.Errorf("summarizeHTTP(empty) = %q", got)
	}
	long := bytes.Repeat([]byte("x"), 500)
	if got := summarizeHTTP(long, 10); len([]rune(got)) > 11 {
		t.Errorf("summarizeHTTP did not truncate: %d runes", len([]rune(got)))
	}
}

// Fecha must end the server-side HTTP session (one DELETE), so a gateway does
// not keep a session per harness process.
func TestFechaClosesHTTPSession(t *testing.T) {
	f := &fakeMCP{initResp: initializeResult()}
	_, url := newFake(t, f)

	c, err := Novo(Servidor{Nome: "fake", Tipo: "http", URL: url, Timeout: 5})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if err := c.Abre(context.Background()); err != nil {
		t.Fatalf("Abre: %v", err)
	}
	if err := c.Fecha(); err != nil {
		t.Fatalf("Fecha: %v", err)
	}
	if f.deletedCount() != 1 {
		t.Errorf("DELETE count = %d; want 1", f.deletedCount())
	}
	// Idempotent: a second Fecha must not send a second DELETE.
	if err := c.Fecha(); err != nil {
		t.Fatalf("Fecha 2: %v", err)
	}
	if f.deletedCount() != 1 {
		t.Errorf("DELETE count after second Fecha = %d", f.deletedCount())
	}
}

// A stdio server must not get a DELETE (there is no session to end).
func TestFechaStdioDoesNotDelete(t *testing.T) {
	c, err := Novo(Servidor{Nome: "fake", Tipo: "stdio", Command: "cmd"})
	if err != nil {
		t.Fatalf("Novo: %v", err)
	}
	if c.httpT != nil {
		t.Error("stdio client carries an HTTP transport")
	}
}
