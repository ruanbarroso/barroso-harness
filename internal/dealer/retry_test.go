package dealer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const okStream = "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// shrink makes the deadlines and backoff test-sized.
func shrink(t *testing.T) {
	t.Helper()
	idle, base, bmax := idleTimeout, backoffBase, backoffMax
	idleTimeout = 200 * time.Millisecond
	backoffBase, backoffMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { idleTimeout, backoffBase, backoffMax = idle, base, bmax })
}

// The 22/10 case: 503 "No server is available" once, then the provider is
// back. The round must succeed and the retry must be reported.
func TestRetry503ThenOK(t *testing.T) {
	shrink(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(503)
			w.Write([]byte("<html><body><h1>503 Service Unavailable</h1>No server is available to handle this request.</body></html>"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	var avisos []int
	c := chatClient(Config{BaseURL: srv.URL, Model: "m"}).ComAviso(func(a, max int, _ time.Duration, err error) {
		avisos = append(avisos, a)
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("reason %v", err)
		}
	})
	r, err := c.Rodar("sys", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "ok" || n.Load() != 2 || len(avisos) != 1 || avisos[0] != 2 {
		t.Fatalf("texto=%q requests=%d avisos=%v", r.Texto, n.Load(), avisos)
	}
}

// A provider that accepts and never answers must fail within the response
// deadline, retried, and the final error says why.
func TestSilentProviderGivesUp(t *testing.T) {
	shrink(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		io.Copy(io.Discard, r.Body) // only then does net/http watch for the client hanging up
		<-r.Context().Done()
	}))
	defer srv.Close()
	start := time.Now()
	_, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
	if err == nil || !errors.Is(err, errIdle) || !strings.Contains(err.Error(), "gave up after 2 attempts") {
		t.Fatalf("err %v", err)
	}
	if n.Load() != int32(maxTimeoutAttempts) {
		t.Fatalf("requests %d", n.Load())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

// A stream that sends one chunk and goes quiet trips the idle deadline; the
// next attempt completes.
func TestIdleTimeoutMidStreamRetries(t *testing.T) {
	shrink(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"meio\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	var reason error
	c := chatClient(Config{BaseURL: srv.URL, Model: "m"}).ComAviso(func(_, _ int, _ time.Duration, err error) { reason = err })
	r, err := c.Rodar("sys", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Texto != "ok" || !errors.Is(reason, errIdle) {
		t.Fatalf("texto=%q reason=%v", r.Texto, reason)
	}
}

// Steady chunks keep the stream alive past idleTimeout in total.
func TestSlowButSteadyStreamIsNotCut(t *testing.T) {
	shrink(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 5; i++ {
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	r, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
	if err != nil || r.Texto != "xxxxx" {
		t.Fatalf("texto=%q err=%v", r.Texto, err)
	}
}

// 400/401/404 are not retried.
func TestNonTransientNotRetried(t *testing.T) {
	shrink(t)
	for _, code := range []int{400, 401, 404} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n.Add(1)
			w.WriteHeader(code)
		}))
		_, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
		srv.Close()
		if err == nil || n.Load() != 1 || strings.Contains(err.Error(), "gave up") {
			t.Fatalf("%d: requests=%d err=%v", code, n.Load(), err)
		}
	}
}

func TestRetryAfterHonoredAndCapped(t *testing.T) {
	if d := retryWait(&httpError{status: 503, retryAfter: 7 * time.Second}, 1); d != 7*time.Second {
		t.Fatalf("%v", d)
	}
	if d := retryWait(&httpError{status: 429, retryAfter: time.Hour}, 1); d != retryAfterMx {
		t.Fatalf("%v", d)
	}
	if parseRetryAfter("12") != 12*time.Second || parseRetryAfter("abc") != 0 {
		t.Fatal("parse")
	}
	if parseRetryAfterMs("1500") != 1500*time.Millisecond || parseRetryAfterMs("-1") != 0 {
		t.Fatal("parse ms")
	}
}

// Backoff without Retry-After carries jitter within [75%, 100%] of the step.
func TestBackoffJitter(t *testing.T) {
	for i := 0; i < 200; i++ {
		d := retryWait(errors.New("x"), 2)
		if d < backoffBase*2*3/4 || d > backoffBase*2 {
			t.Fatalf("%v out of range", d)
		}
	}
}

// statusServer answers with code (and headers/body) count times, then OK.
func statusServer(t *testing.T, code, count int, hdr map[string]string, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if int(n.Add(1)) <= count {
			for k, v := range hdr {
				w.Header().Set(k, v)
			}
			w.WriteHeader(code)
			w.Write([]byte(body))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(okStream))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestRetry500(t *testing.T) {
	shrink(t)
	srv, n := statusServer(t, 500, 1, nil, "boom")
	if _, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil); err != nil || n.Load() != 2 {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
}

func TestQuota429IsTerminal(t *testing.T) {
	shrink(t)
	srv, n := statusServer(t, 429, 9, nil, `{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`)
	_, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
	if err == nil || n.Load() != 1 {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
}

func TestShouldRetryHeaderWins(t *testing.T) {
	shrink(t)
	srv, n := statusServer(t, 503, 9, map[string]string{"X-Should-Retry": "false"}, "")
	if _, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil); err == nil || n.Load() != 1 {
		t.Fatalf("false: requests=%d err=%v", n.Load(), err)
	}
	srv, n = statusServer(t, 400, 1, map[string]string{"X-Should-Retry": "true"}, "")
	if _, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil); err != nil || n.Load() != 2 {
		t.Fatalf("true: requests=%d err=%v", n.Load(), err)
	}
}

// A stream that only sends ": ping" comments is stalled: pings do not count.
func TestPingsDoNotFeedTheWatchdog(t *testing.T) {
	shrink(t)
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if n.Add(1) == 1 {
			for {
				if _, err := w.Write([]byte(": ping\n\n")); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
		w.Write([]byte(okStream))
	}))
	defer srv.Close()
	r, err := chatClient(Config{BaseURL: srv.URL, Model: "m"}).Rodar("sys", nil, nil)
	if err != nil || r.Texto != "ok" || n.Load() != 2 {
		t.Fatalf("requests=%d err=%v", n.Load(), err)
	}
}

// Cancelling the caller's ctx during the backoff ends at once with ctx.Err.
func TestCancelDuringBackoff(t *testing.T) {
	shrink(t)
	backoffBase, backoffMax = time.Hour, time.Hour
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	c := chatClient(Config{BaseURL: srv.URL, Model: "m"}).ComAviso(func(int, int, time.Duration, error) { cancel() })
	_, err := c.RodarContext(ctx, "sys", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}
