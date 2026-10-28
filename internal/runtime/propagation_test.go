package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// bodyLog keeps every request body the fake provider saw.
type bodyLog struct {
	mu      sync.Mutex
	bodies  []string
	toolRan bool
}

func (l *bodyLog) add(b string) {
	l.mu.Lock()
	l.bodies = append(l.bodies, b)
	l.mu.Unlock()
}

func (l *bodyLog) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.bodies) == 0 {
		return ""
	}
	return l.bodies[len(l.bodies)-1]
}

// ranTool reports whether a request already carried a tool result, i.e. the
// call of the previous round was executed.
func (l *bodyLog) ranTool() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.toolRan
}

func (l *bodyLog) markToolResult(v bool) {
	l.mu.Lock()
	if v {
		l.toolRan = true
	}
	l.mu.Unlock()
}

// streamTurn answers each request with one chunk of content, or with a
// slow_a tool call while that tool has not been executed yet (so the turn
// reaches a request boundary again, where an update lands).
func streamTurn(l *bodyLog) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		l.add(systemOf(body.Messages))
		l.markToolResult(hasToolResult(body.Messages))
		w.Header().Set("Content-Type", "text/event-stream")
		var chunks []string
		if !l.ranTool() {
			chunks = []string{`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"slow_a","arguments":"{}"}}]}}]}`}
		} else {
			chunks = []string{`{"choices":[{"delta":{"content":"ok"}}]}`}
		}
		chunks = append(chunks, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1}}`, `[DONE]`)
		for _, d := range chunks {
			_, _ = w.Write([]byte("data: " + d + "\n\n"))
		}
	}
}

// hasToolResult reports whether the transcript already holds a tool result.
func hasToolResult(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// systemOf returns the system message the runtime sent.
func systemOf(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) string {
	for _, m := range msgs {
		if m.Role == "system" {
			return m.Content
		}
	}
	return ""
}

// An open session must see a BARROSO.md change made DURING its turn (by
// another process, or by the user editing the file) at the next request
// boundary: the frozen system prompt is not rewritten, the change arrives
// append-only as a [system update] message.
func TestChangeReachesOpenSession(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	cwd := t.TempDir()
	log := &bodyLog{}
	srv := httptest.NewServer(streamTurn(log))
	defer srv.Close()
	cfg := chatTestConfig(srv.URL, "fake", "k")
	cfg.Effort = "none"

	s := NewSession(cfg)
	// A tool that writes BARROSO.md while it runs: the classic mid-flight change.
	s.Injetar(tools.Spec{
		Name:        "slow_a",
		Description: "writes BARROSO.md and returns",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, _ string, _ json.RawMessage) (string, error) {
			return "done", os.WriteFile(filepath.Join(cwd, "BARROSO.md"), []byte("be terse"), 0o644)
		},
	})
	if err := s.Send(context.Background(), nil, cwd, "go"); err != nil {
		t.Fatal(err)
	}

	var upd string
	for _, m := range s.History() {
		if strings.HasPrefix(m.Conteudo, UpdatePrefix) {
			upd = m.Conteudo
		}
	}
	if !strings.Contains(upd, "be terse") {
		t.Fatalf("the session must receive the mid-turn change as an update:\n%s", upd)
	}
	// Append-only: the update is a user-role message and the turn still ends
	// with the assistant's answer.
	h := s.History()
	if i, m := lastIndexOf(h, UpdatePrefix); i < 0 || m.Role != "user" {
		t.Fatalf("update must be a user-role message (got %#v)", m)
	}
	if h[len(h)-1].Role != "assistant" {
		t.Fatalf("the turn must end with the assistant answer, got %s", h[len(h)-1].Role)
	}
	// The frozen system prompt does NOT change: the last request still carries
	// the system of the FIRST request (no BARROSO.md inside it).
	if sys := log.last(); strings.Contains(sys, "## BARROSO.md (system instructions)") {
		t.Fatalf("the system prompt must stay frozen:\n%s", sys)
	}
}

// A session opened after the change gets it frozen in its system prompt
// instead of an update message.
func TestChangeAppliesToNewSession(t *testing.T) {
	t.Setenv("BARROSO_HARNESS_HOME", t.TempDir())
	cwd := t.TempDir()
	log := &bodyLog{}
	srv := httptest.NewServer(streamTurn(log))
	defer srv.Close()
	cfg := chatTestConfig(srv.URL, "fake", "k")
	cfg.Effort = "none"

	if err := os.WriteFile(filepath.Join(cwd, "BARROSO.md"), []byte("be terse"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg)
	if err := s.Send(context.Background(), nil, cwd, "go"); err != nil {
		t.Fatal(err)
	}
	for _, m := range s.History() {
		if strings.HasPrefix(m.Conteudo, UpdatePrefix) {
			t.Fatal("a session born after the change must not receive an update")
		}
	}
	if sys := log.last(); !strings.Contains(sys, "be terse") {
		t.Fatalf("the new session's system prompt must carry the new BARROSO.md:\n%s", sys)
	}
}

func lastIndexOf(h []dealer.Mensagem, prefix string) (int, dealer.Mensagem) {
	for i := len(h) - 1; i >= 0; i-- {
		if strings.HasPrefix(h[i].Conteudo, prefix) {
			return i, h[i]
		}
	}
	return -1, dealer.Mensagem{}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}
