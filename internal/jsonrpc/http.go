package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// MaxBody is the default cap on an HTTP response body.
	MaxBody = 32 << 20
	// DefaultTimeout caps each call when neither the transport nor the ctx
	// says otherwise.
	DefaultTimeout = 120 * time.Second
	// sessionHeader is the streamable-HTTP session header: the server hands
	// it out in the initialize response and expects it back on every call.
	sessionHeader = "Mcp-Session-Id"
	// closeTimeout caps the session-ending DELETE: it is best-effort, so a
	// hung server must not hold the process exit.
	closeTimeout = 5 * time.Second
)

// ErrSessionExpired: the server answered 404 to a call that carried a
// session id — by the streamable-HTTP spec, the session is gone and the
// client must initialize again. The transport has already forgotten the id;
// the caller (who knows the MCP handshake) redoes it once.
var ErrSessionExpired = errors.New("MCP session expired")

// HTTP is a streamable-HTTP JSON-RPC transport: one POST per message, the
// response as plain JSON, batch or SSE. Safe for concurrent use.
type HTTP struct {
	URL       string
	Headers   map[string]string // extra headers (MCP server config); nil = anonymous
	UserAgent string
	Client    *http.Client // nil = a client without a global timeout (the ctx bounds each call)
	Timeout   time.Duration
	MaxBody   int64 // 0 = MaxBody

	mu      sync.Mutex
	session string
}

// SessionID returns the session id the server handed out ("" = none).
func (h *HTTP) SessionID() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.session
}

// ResetSession forgets the session id (before a new initialize).
func (h *HTTP) ResetSession() {
	h.mu.Lock()
	h.session = ""
	h.mu.Unlock()
}

// Deadline reconciles the transport timeout with the ctx one (the smaller wins).
func Deadline(ctx context.Context, timeout time.Duration) time.Duration {
	d := timeout
	if d <= 0 {
		d = DefaultTimeout
	}
	if dl, ok := ctx.Deadline(); ok {
		if rest := time.Until(dl); rest < d {
			d = rest
		}
	}
	if d <= 0 {
		d = time.Millisecond // ctx already expired: fail fast, never "no timeout"
	}
	return d
}

// Call POSTs the request and returns the response with the matching id. A
// JSON-RPC error comes back as *Error, unwrapped.
func (h *HTTP) Call(ctx context.Context, r Request) (Message, error) {
	if len(r.Body) == 0 {
		return Message{}, fmt.Errorf("request %s failed to serialize", r.Method)
	}
	m, err := h.post(ctx, r.Method, r.Body, Bytes(r.ID))
	if err != nil {
		return Message{}, err
	}
	if m.Error != nil {
		return Message{}, m.Error
	}
	return m, nil
}

// Notify POSTs a notification. Only TRANSPORT failures matter (network, HTTP
// status, timeout): the server may reply 202/200 with an empty body, with a
// JSON-RPC response or with an error one — all three are valid.
func (h *HTTP) Notify(ctx context.Context, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	_, err = h.post(ctx, m.Method, b, nil)
	if err != nil && !errors.Is(err, ErrEmptyResponse) && !isParseError(err) {
		return err
	}
	return nil
}

// parseError marks a body that came back but could not be read as the
// expected response — irrelevant for a notification.
type parseError struct{ err error }

func (e parseError) Error() string { return e.err.Error() }
func (e parseError) Unwrap() error { return e.err }

func isParseError(err error) bool {
	var p parseError
	return errors.As(err, &p)
}

// Close ends the server-side session. The streamable-HTTP spec makes DELETE
// optional — a server that keeps sessions after the client goes away must
// expire them on its own — so the failure is returned and ignored by the
// caller. It is what keeps a long-lived gateway (an MCP bridge hosted by the
// app's API) from accumulating one session per harness process.
func (h *HTTP) Close(ctx context.Context) error {
	sent := h.SessionID()
	if sent == "" {
		return nil // nothing to end: never send a sessionless DELETE
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, closeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodDelete, h.URL, nil)
	if err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if h.UserAgent != "" {
		req.Header.Set("User-Agent", h.UserAgent)
	}
	req.Header.Set(sessionHeader, sent)
	for k, v := range h.Headers {
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	client := h.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("close: HTTP %d", resp.StatusCode)
	}

	h.mu.Lock()
	if h.session == sent {
		h.session = ""
	}
	h.mu.Unlock()
	return nil
}

// post performs the HTTP round trip. id nil = notification (the body is
// drained and not interpreted).
func (h *HTTP) post(ctx context.Context, method string, body []byte, id []byte) (Message, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	hctx, cancel := context.WithTimeout(ctx, Deadline(ctx, h.Timeout))
	defer cancel()

	req, err := http.NewRequestWithContext(hctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		return Message{}, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.UserAgent != "" {
		// Some WAFs block the default "Go-http-client/1.1" before even looking
		// at credentials — and MCP gateways often sit behind one.
		req.Header.Set("User-Agent", h.UserAgent)
	}
	// The MCP streamable HTTP spec REQUIRES this Accept: the server picks
	// between plain JSON and SSE based on what the client declares it accepts.
	req.Header.Set("Accept", "application/json, text/event-stream")
	sent := h.SessionID()
	if sent != "" {
		req.Header.Set(sessionHeader, sent)
	}
	for k, v := range h.Headers {
		if k == "" {
			continue
		}
		req.Header.Set(k, v)
	}

	client := h.Client
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()

	if sid := strings.TrimSpace(resp.Header.Get(sessionHeader)); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}

	limit := h.MaxBody
	if limit <= 0 {
		limit = MaxBody
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		if resp.StatusCode == http.StatusNotFound && sent != "" {
			h.mu.Lock()
			if h.session == sent {
				h.session = ""
			}
			h.mu.Unlock()
			return Message{}, fmt.Errorf("%w (HTTP 404: %s)", ErrSessionExpired, Summarize(b, 300))
		}
		return Message{}, fmt.Errorf("HTTP %d: %s", resp.StatusCode, Summarize(b, 300))
	}
	if id == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, limit))
		return Message{}, nil
	}

	// SSE is read line by line and returns at the matching id: a stream left
	// open after the answer does not hold the call until the timeout.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		m, err := readSSE(resp.Body, id, limit)
		if err != nil {
			if hctx.Err() != nil {
				return Message{}, fmt.Errorf("%s: %w", method, hctx.Err())
			}
			return Message{}, parseError{fmt.Errorf("%s: %w", method, err)}
		}
		return m, nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Message{}, fmt.Errorf("reading response: %w", err)
	}
	if int64(len(b)) > limit {
		return Message{}, fmt.Errorf("%s: response exceeded %d bytes", method, limit)
	}
	m, err := ResponseFrom(b, resp.Header.Get("Content-Type"), id)
	if err != nil {
		if errors.Is(err, ErrEmptyResponse) {
			return Message{}, fmt.Errorf("%s: %w", method, err)
		}
		return Message{}, parseError{fmt.Errorf("%s: %w", method, err)}
	}
	return m, nil
}
