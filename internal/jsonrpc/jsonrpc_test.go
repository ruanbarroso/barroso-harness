package jsonrpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResponseFromShapes(t *testing.T) {
	id := Bytes(int64(7))
	for name, c := range map[string]struct{ body, ct string }{
		"json":        {`{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`, "application/json"},
		"string id":   {`{"jsonrpc":"2.0","id":"7","result":{"ok":true}}`, "application/json"},
		"batch":       {`[{"jsonrpc":"2.0","id":3,"result":{}},{"jsonrpc":"2.0","id":7,"result":{"ok":true}}]`, ""},
		"sse":         {": ping\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"n\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n\n", "text/event-stream"},
		"sniffed sse": {": ping\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{\"ok\":true}}\n", ""},
	} {
		t.Run(name, func(t *testing.T) {
			m, err := ResponseFrom([]byte(c.body), c.ct, id)
			if err != nil || !strings.Contains(string(m.Result), "ok") {
				t.Errorf("ResponseFrom = %v, %v", m, err)
			}
		})
	}
	if _, err := ResponseFrom(nil, "application/json", id); !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("empty body err = %v", err)
	}
	if _, err := ResponseFrom([]byte(`{"jsonrpc":"2.0","id":8,"result":{}}`), "", id); err == nil {
		t.Error("wrong id accepted")
	}
}

// An SSE stream left open after the answer must not hold the call.
func TestHTTPOpenSSEReturnsAtMatchingID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	h := &HTTP{URL: srv.URL, Timeout: 5 * time.Second}
	start := time.Now()
	if _, err := h.Call(context.Background(), NewRequest(1, "x", nil)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("open stream held the call for %s", time.Since(start))
	}
}

func TestHTTPSessionCaptureAndExpiry(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Mcp-Session-Id"))
		if len(seen) == 1 {
			w.Header().Set("Mcp-Session-Id", "abc")
		}
		if len(seen) == 3 {
			http.Error(w, "gone", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	h := &HTTP{URL: srv.URL}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := h.Call(ctx, NewRequest(1, "x", nil)); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := h.Call(ctx, NewRequest(1, "x", nil))
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("404 with session err = %v", err)
	}
	if h.SessionID() != "" {
		t.Error("expired session id kept")
	}
	if seen[0] != "" || seen[1] != "abc" || seen[2] != "abc" {
		t.Errorf("session headers = %q", seen)
	}
}

func TestHTTPErrorsAreOneLineAndAnonymous(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("credential header sent by an anonymous transport")
		}
		http.Error(w, "<html>\n<b>boom</b>\n</html>", http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := (&HTTP{URL: srv.URL}).Call(context.Background(), NewRequest(1, "x", nil))
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "\n") {
		t.Errorf("err = %v", err)
	}
	if errors.Is(err, ErrSessionExpired) {
		t.Error("500 without session reported as expiry")
	}
}

func TestHTTPBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"` + strings.Repeat("x", 5000) + `"}`))
	}))
	defer srv.Close()
	_, err := (&HTTP{URL: srv.URL, MaxBody: 1000}).Call(context.Background(), NewRequest(1, "x", nil))
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("err = %v", err)
	}
}

func TestNotifyToleratesEmptyAndOddBodies(t *testing.T) {
	for _, body := range []string{"", "not json", `{"jsonrpc":"2.0","id":1,"error":{"code":1,"message":"x"}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(body))
		}))
		if err := (&HTTP{URL: srv.URL}).Notify(context.Background(), NewNotification("n", nil)); err != nil {
			t.Errorf("Notify(%q) = %v", body, err)
		}
		srv.Close()
	}
}

// Close ends the server session with a DELETE carrying the session id and the
// configured headers, then forgets the id; without a session it sends nothing.
func TestHTTPCloseDeletesSession(t *testing.T) {
	var methods, sessions, auths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		sessions = append(sessions, r.Header.Get("Mcp-Session-Id"))
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()

	h := &HTTP{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer t"}}
	if err := h.Close(context.Background()); err != nil || len(methods) != 0 {
		t.Fatalf("sessionless Close = %v, requests %v", err, methods)
	}
	if _, err := h.Call(context.Background(), NewRequest(1, "x", nil)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := h.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(methods) != 2 || methods[1] != http.MethodDelete || sessions[1] != "s1" || auths[1] != "Bearer t" {
		t.Errorf("methods=%v sessions=%v auths=%v", methods, sessions, auths)
	}
	if h.SessionID() != "" {
		t.Error("session id kept after Close")
	}
}

// A server that refuses DELETE (405 is allowed by the spec) reports an error
// the caller ignores; it never hangs.
func TestHTTPCloseToleratesRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Mcp-Session-Id", "s1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer srv.Close()
	h := &HTTP{URL: srv.URL}
	if _, err := h.Call(context.Background(), NewRequest(1, "x", nil)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if err := h.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "405") {
		t.Errorf("Close err = %v", err)
	}
}
