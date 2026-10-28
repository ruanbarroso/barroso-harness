package dealer

// Request deadlines and automatic retry. The only bound used to be the
// 15-minute http.Client timeout: a provider that accepted the connection and
// went quiet held the turn longer than the ACP connector waits (10 min), so
// the session/prompt never got an answer. Now silence has its own deadlines
// (one response deadline, from the send) and transient failures are retried with
// backoff before the error goes up.
//
// Retrying a round mid-stream is safe: the dealer only hands the Rodada over
// once it is complete, so nothing of a failed attempt reached the user.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Variables, not constants, so tests can shrink them.
var (
	// idleTimeout is the only response deadline: how long the provider may
	// stay silent, counted from the moment the request goes out (so it also
	// covers the wait for headers and the first line) and re-armed after every
	// line of the stream. There is deliberately NO separate first-byte
	// deadline: on a big context the first byte takes minutes (139 s measured
	// on 2026-10-27) and cutting it only restarted the same wait on a retry.
	// A
	// reasoning model can go quiet for minutes mid-thought: the survey found
	// 300 s (Codex, Gemini), 120 s (Crush, Goose) and 15 min (OpenCode, because
	// DeepSeek asked for it). Below ~2 min you cut working models.
	idleTimeout = 300 * time.Second
	// There is deliberately NO total deadline. The surveyed harnesses bound
	// silence, not duration; the 15-minute cap this replaces cut turns that
	// were legitimately still working. Neither bound counts a provider
	// keepalive: only "data:" lines feed the watchdog (readRound), so a
	// stream that pings forever without content still trips idleTimeout.
	maxAttempts = 4
	// Timeouts get their own small budget: a dead provider must not be able to
	// hold the turn open for the whole backoff ladder.
	maxTimeoutAttempts = 2
	backoffBase        = 2 * time.Second
	backoffMax         = 30 * time.Second
	retryAfterMx       = 60 * time.Second
)

var (
	errIdle = errors.New("no response from the provider")
)

// AvisoRetry is called before each new attempt: which attempt comes next, the
// total, how long it waits first and why the previous one failed.
type AvisoRetry func(attempt, max int, wait time.Duration, reason error)

// ComAviso returns a copy of the client that reports its retries to f.
func (c *Cliente) ComAviso(f AvisoRetry) *Cliente {
	c2 := *c
	c2.aviso = f
	return &c2
}

// httpError is a non-200 answer. retryAfter is the parsed Retry-After header
// (0 when absent or invalid).
type httpError struct {
	status     int
	summary    string
	retryAfter time.Duration
	// shouldRetry is the provider's x-should-retry header: +1 true, -1 false,
	// 0 absent. When present it wins over the status rules (Anthropic SDK,
	// OpenCode, pi and Crush all honor it).
	shouldRetry int
	// quota: a 429 whose body says the usage limit or credit is exhausted.
	// Repeating it only burns time (Codex makes it terminal).
	quota bool
}

func (e *httpError) Error() string {
	return fmt.Sprintf("provider HTTP %d: %s", e.status, e.summary)
}

// providerError is an error the provider reported INSIDE an HTTP 200 stream
// (Chat `{"error":{...}}` chunk, Responses `error` / `response.failed`
// events), or a content_filter stop with nothing to show. kind is the
// provider's error code or type ("context_length_exceeded",
// "rate_limit_exceeded", "server_error", ...); status is the numeric status
// some gateways embed (0 when absent). The Error() text always carries kind
// and message, so the runtime's context-overflow detection reads them.
type providerError struct {
	status  int
	kind    string
	summary string
	quota   bool
}

func (e *providerError) Error() string {
	if e.kind != "" {
		return fmt.Sprintf("provider error (%s): %s", e.kind, e.summary)
	}
	return "provider error: " + e.summary
}

// retryable: overload-style codes are worth another attempt; quota,
// invalid requests, context overflow and content filters are not.
func (e *providerError) retryable() bool {
	if e.quota {
		return false
	}
	if e.status != 0 {
		return retryableStatus(e.status)
	}
	k := strings.ToLower(e.kind)
	for _, bad := range []string{"context", "invalid", "content_filter", "quota", "auth", "permission", "not_found", "unsupported"} {
		if strings.Contains(k, bad) {
			return false
		}
	}
	for _, good := range []string{"server_error", "overloaded", "rate_limit", "unavailable", "timeout", "internal", "capacity", "too_many_requests", "try_again"} {
		if strings.Contains(k, good) {
			return true
		}
	}
	return false
}

// newProviderError builds the typed error from the parts a payload offers.
func newProviderError(status int, kind, message string) *providerError {
	kind = strings.TrimSpace(kind)
	msg := truncateLine(message, 300)
	if msg == "" {
		msg = "(no message)"
	}
	e := &providerError{status: status, kind: kind, summary: msg}
	e.quota = quotaBody.MatchString(kind + " " + message)
	return e
}

// rawString renders a JSON string or number (error codes come as either).
func rawString(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return t
}

// erroDePayload recognizes an error carried by an SSE payload that arrived
// under HTTP 200: `{"error":{...}}` (or a string error), or the Responses
// top-level `{"type":"error","code":...,"message":...}` event. nil = not an
// error payload.
func erroDePayload(payload string) error {
	if !strings.Contains(payload, `"error"`) {
		return nil
	}
	var env struct {
		Type    string          `json:"type"`
		Error   json.RawMessage `json:"error"`
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Status  int             `json:"status"`
	}
	if json.Unmarshal([]byte(payload), &env) != nil {
		return nil
	}
	if e := strings.TrimSpace(string(env.Error)); e != "" && e != "null" {
		return erroDeObjeto(env.Error, env.Status)
	}
	if env.Type == "error" {
		return newProviderError(env.Status, rawString(env.Code), env.Message)
	}
	return nil
}

// erroDeObjeto turns a Responses/Chat `error` value (object or string) into
// the typed error.
func erroDeObjeto(raw json.RawMessage, status int) *providerError {
	var o struct {
		Message string          `json:"message"`
		Code    json.RawMessage `json:"code"`
		Type    string          `json:"type"`
		Status  int             `json:"status"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return newProviderError(status, "", rawString(raw))
	}
	kind := rawString(o.Code)
	if n, err := strconv.Atoi(kind); err == nil && status == 0 {
		status, kind = n, ""
	}
	if kind == "" {
		kind = o.Type
	}
	if o.Status != 0 {
		status = o.Status
	}
	msg := o.Message
	if msg == "" {
		msg = string(raw)
	}
	return newProviderError(status, kind, msg)
}

// stallError is the response (idle) deadline firing.
type stallError struct {
	kind  error
	after time.Duration
}

func (e *stallError) Error() string {
	return fmt.Sprintf("%v for %s", e.kind, e.after.Round(time.Second))
}

func (e *stallError) Unwrap() error { return e.kind }

// retryableStatus: overload, rate limit and gateway/proxy failures. 500 IS
// retried: every harness surveyed (OpenCode, Codex, Gemini, Claude Code's SDK,
// Crush, Goose) retries ≥500. A deterministic context-overflow answer still
// does not come back as 500 on this path — it arrives as a 400/200 stream
// error, which contextOverflow already handles in the runtime.
func retryableStatus(code int) bool {
	switch code {
	case 408, 409, 425, 429:
		return true
	case 500, 502, 503, 504, 520, 521, 522, 523, 524, 529:
		return true
	}
	return false
}

// transient decides whether err is worth another attempt.
func transient(err error) bool {
	// These answers describe a missing/unsupported route, not a transient
	// provider failure. Let Responses fallback run immediately.
	if responsesEndpointUnavailable(err) {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		switch {
		case he.shouldRetry > 0:
			return true
		case he.shouldRetry < 0, he.quota:
			return false
		}
		return retryableStatus(he.status)
	}
	var pe *providerError
	if errors.As(err, &pe) {
		return pe.retryable()
	}
	var se *stallError
	if errors.As(err, &se) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNABORTED) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "connection reset") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "unexpected eof") || strings.Contains(s, "http2: ")
}

// retryWait: Retry-After when the provider sent one (a floor, capped), else
// exponential backoff from backoffBase with ±25% jitter. Jitter keeps a fleet
// of sessions from asking again in the same millisecond after an outage.
func retryWait(err error, attempt int) time.Duration {
	var he *httpError
	if errors.As(err, &he) && he.retryAfter > 0 {
		return min(he.retryAfter, retryAfterMx)
	}
	d := min(backoffBase<<(attempt-1), backoffMax)
	return d*3/4 + time.Duration(rand.Int63n(int64(d/4)))
}

// newHTTPError builds the error of a non-200 answer from its status, headers
// and (already read, capped) body.
// responsesEndpointUnavailable classifies only errors that say the Responses
// route/method is absent. It deliberately excludes generic request validation,
// auth, model, quota, transport and generation failures.
func responsesEndpointUnavailable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		if he.status == http.StatusMethodNotAllowed || he.status == http.StatusNotImplemented {
			return true
		}
		if he.status == http.StatusBadRequest {
			msg := strings.ToLower(he.summary)
			for _, marker := range []string{"endpoint_not_found", "unsupported_endpoint", "unsupported endpoint", "route_not_found", "unsupported_route", "unsupported route", "unknown endpoint", "responses endpoint is not supported"} {
				if strings.Contains(msg, marker) {
					return true
				}
			}
			return false
		}
		if he.status != http.StatusNotFound {
			return false
		}
		msg := strings.ToLower(he.summary)
		for _, marker := range []string{"model_not_found", "model_not_supported", "model_not_available", "unknown_model", "unsupported_model", "invalid_model", "model not found", "unknown model", "no such model", "invalid model", "unsupported model", "model does not exist", "model doesn't exist"} {
			if strings.Contains(msg, marker) {
				return false
			}
		}
		// A 404 for the requested Responses URL means the route is absent. If
		// the provider names another resource (notably a model), do not guess.
		return true
	}
	var pe *providerError
	if errors.As(err, &pe) {
		kind := strings.ToLower(pe.kind)
		msg := strings.ToLower(pe.summary)
		if strings.Contains(kind, "model_not_found") || strings.Contains(msg, "model not found") {
			return false
		}
		for _, marker := range []string{"endpoint_not_found", "unsupported_endpoint", "unsupported endpoint", "route_not_found", "unsupported_route", "unsupported route"} {
			if strings.Contains(kind, marker) {
				return true
			}
		}
		if strings.Contains(kind, "not_found") &&
			(strings.Contains(msg, "/responses") || strings.Contains(msg, "responses endpoint") || strings.Contains(msg, "responses route")) {
			return true
		}
	}
	return false
}

func newHTTPError(resp *http.Response, body []byte) *httpError {
	e := &httpError{
		status:     resp.StatusCode,
		summary:    resumoComCodigo(body),
		retryAfter: parseRetryAfterMs(resp.Header.Get("Retry-After-Ms")),
	}
	if e.retryAfter == 0 {
		e.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	switch strings.ToLower(strings.TrimSpace(resp.Header.Get("X-Should-Retry"))) {
	case "true":
		e.shouldRetry = 1
	case "false":
		e.shouldRetry = -1
	}
	if resp.StatusCode == 429 && quotaBody.Match(body) {
		e.quota = true
	}
	return e
}

// resumoComCodigo is errorSummary plus the error code (or type) of an
// OpenAI-style JSON body, e.g. "... (context_length_exceeded)": the code is
// what tells overflow, quota and invalid requests apart.
func resumoComCodigo(body []byte) string {
	s := errorSummary(body, 300)
	var env struct {
		Error struct {
			Code json.RawMessage `json:"code"`
			Type string          `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return s
	}
	for _, tag := range []string{rawString(env.Error.Code), env.Error.Type} {
		if tag != "" && !strings.Contains(s, tag) {
			s += " (" + tag + ")"
			break
		}
	}
	return s
}

// quotaBody recognizes the "you are out of quota" flavors of 429, as opposed
// to a momentary rate limit — including the free-tier budget of the opencode
// zen lane (its 429 reads FreeUsageLimitError until the UTC reset).
var quotaBody = regexp.MustCompile(`(?i)insufficient_quota|usage_limit_reached|quota[_ ]exceeded|credit[_ ]balance|billing|exceeded your current quota|free_usage_limit|free usage limit|freeusagelimiterror`)

// parseRetryAfterMs reads retry-after-ms (OpenAI, Anthropic): milliseconds.
func parseRetryAfterMs(v string) time.Duration {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n * float64(time.Millisecond))
}

// parseRetryAfter accepts delta-seconds or an HTTP date.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, time.Until(t))
	}
	return 0
}

// budgetFor returns the attempt ceiling for this error kind: timeouts get
// maxTimeoutAttempts, everything else maxAttempts.
func budgetFor(err error) int {
	var se *stallError
	if errors.As(err, &se) {
		return maxTimeoutAttempts
	}
	return maxAttempts
}

// comRetry runs once up to maxAttempts times. It stops early on success, on a
// non-transient error, or when ctx (the caller's) ends. The final error says
// how many attempts ran and how long it all took.
func (c *Cliente) comRetry(ctx context.Context, once func(context.Context) (*Rodada, error)) (*Rodada, error) {
	start := time.Now()
	var err error
	for attempt := 1; ; attempt++ {
		var r *Rodada
		r, err = once(ctx)
		if err == nil {
			return r, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		ceiling := budgetFor(err)
		if !transient(err) || attempt >= ceiling {
			break
		}
		wait := retryWait(err, attempt)
		if c.aviso != nil {
			c.aviso(attempt+1, ceiling, wait, err)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
	if transient(err) {
		return nil, fmt.Errorf("provider: gave up after %d attempts in %s: %w",
			budgetFor(err), time.Since(start).Round(time.Second), err)
	}
	return nil, err
}

// watchdog cancels the request when the provider stays silent for idle,
// counted from the request being sent. Kick after each line.
type watchdog struct {
	cancel context.CancelCauseFunc
	timer  *time.Timer
	idle   time.Duration
}

// pick is the configured override, or def when none was set.
func pick(override *time.Duration, def time.Duration) time.Duration {
	if override != nil {
		return *override
	}
	return def
}

// newWatchdog arms the response timer right away; a bound ≤0 is disabled.
func newWatchdog(ctx context.Context, idle time.Duration) (context.Context, *watchdog) {
	wctx, cancel := context.WithCancelCause(ctx)
	w := &watchdog{cancel: cancel, idle: idle}
	w.arm()
	return wctx, w
}

func (w *watchdog) kick() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.arm()
}

func (w *watchdog) arm() {
	if w.idle <= 0 {
		w.timer = nil
		return
	}
	w.timer = time.AfterFunc(w.idle, func() {
		w.cancel(&stallError{kind: errIdle, after: w.idle})
	})
}

func (w *watchdog) stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
	w.cancel(nil)
}

// cause turns the opaque "context canceled" of a fired watchdog back into the
// stallError; any other error passes through.
func cause(wctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var se *stallError
	if c := context.Cause(wctx); errors.As(c, &se) {
		return se
	}
	return err
}
