// JSON-RPC 2.0 framing and types over a newline-delimited byte transport.
// The agent speaks over stdio, so each message is ONE JSON line terminated by
// "\n": no Content-Length header, no extra framing. A bufio.Scanner capped at
// 8 MiB keeps a giant line (or a misbehaving client) from blowing up the
// process memory; exceeding the cap ends the scanner with ErrTooLong and the
// serve loop dies with an honest error, not a panic.
//
// Identifier rules: the id is kept as json.RawMessage, so a request with a
// numeric id gets a response with a numeric id and one with a string id gets a
// string id. A notification is whatever has no id (missing or null) — and a
// notification NEVER gets a response.
package acp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
)

// MaxLinha is the cap on a JSON-RPC message in bytes (8 MiB). Large session
// documents (transcripts, tool results) fit; an endless line does not.
const MaxLinha = 8 << 20

// JSON-RPC protocol version accepted on this transport.
const jsonRPCVersion = "2.0"

// JSON-RPC 2.0 error codes + the agent's custom range.
const (
	CodParse        = -32700 // reserved; whole-line framing, nearly inert
	CodInvalido     = -32600 // invalid request
	CodMetodo       = -32601 // method not found
	CodParams       = -32602 // invalid params
	CodInterno      = -32603 // internal error
	CodTurnoRodando = -32001 // agent: a turn is already in flight in this session
	CodErroAgente   = -32000 // agent: turn failure / cancellation
	CodTransitorio  = -32002 // agent: session exists but could not be read now (db lock); retry the same id
)

// Request is a message with an id: it requires a response.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// TemID tells a request from a notification. Missing or `null` id = notification.
func (r Request) TemID() bool {
	id := bytes.TrimSpace(r.ID)
	return len(id) > 0 && !bytes.Equal(id, []byte("null"))
}

// Notification is a message without an id (one-way, no response).
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// Response is the reply to a Request. Result and Error are mutually
// exclusive; a non-nil Error silences Result.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Erro           `json:"error,omitempty"`
}

// Erro is the JSON-RPC error object. Data is free-form and omitted when empty.
type Erro struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *Erro) Error() string {
	if e == nil {
		return "<nil json-rpc error>"
	}
	return e.Message
}

// NovaResposta wraps a success result, preserving the raw id.
func NovaResposta(id json.RawMessage, result any) Response {
	return Response{JSONRPC: jsonRPCVersion, ID: id, Result: result}
}

// NovoErro wraps a failure, preserving the raw id.
func NovoErro(id json.RawMessage, code int, msg string) Response {
	return Response{JSONRPC: jsonRPCVersion, ID: id, Error: &Erro{Code: code, Message: msg}}
}

// NovaNotificacao wraps a one-way notification.
func NovaNotificacao(method string, params any) Notification {
	return Notification{JSONRPC: jsonRPCVersion, Method: method, Params: params}
}

// writeJSON serializes v, appends the framing "\n" and writes EVERYTHING in a
// single Write call — two separate writes could interleave with another
// goroutine's line and produce broken JSON on the wire.
func writeJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// EscreverLinha is the testable boundary of writeJSON: it serializes and emits
// one JSON line ending in "\n", flushing if the writer is buffered.
func EscreverLinha(w io.Writer, v any) error {
	if err := writeJSON(w, v); err != nil {
		return err
	}
	return flush(w)
}

// flush is safe for writers without Flush (os.Stdout is unbuffered).
func flush(w io.Writer) error {
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// WriteError answers a request with a failure. Kept exported because main.go
// (and other protocol hosts) need to respond outside the agent loop.
func WriteError(w io.Writer, id json.RawMessage, code int, msg string) error {
	return EscreverLinha(w, NovoErro(id, code, msg))
}

// DecodificarLinha turns a line into a Request. An empty (whitespace-only)
// line returns ok=false with no error: blank is not a message.
func DecodificarLinha(line []byte) (Request, bool, error) {
	if len(bytes.TrimSpace(line)) == 0 {
		return Request{}, false, nil
	}
	var r Request
	if err := json.Unmarshal(line, &r); err != nil {
		return Request{}, false, err
	}
	return r, true, nil
}

// NovoScanner returns the connection's line scanner with the cap already applied.
func NovoScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLinha)
	return sc
}
