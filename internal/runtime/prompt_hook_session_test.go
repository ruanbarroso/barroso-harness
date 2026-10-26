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

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

// hookProvider answers every request with text and keeps the user messages
// of each request, so a test sees exactly what reached the model.
func hookProvider(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var users []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" {
				var s string
				_ = json.Unmarshal(m.Content, &s)
				users = append(users, s)
			}
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "ok"}}}})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), users...)
	}
}

func hookSession(t *testing.T, url string, hooks ...config.PromptHook) (*Session, string) {
	t.Helper()
	cfg := fakeConfig(t, url)
	cfg.PromptHooks = hooks
	const id = "bh-20240101100000-hook"
	return NewSessionWithStore(cfg, testStore(t), id), id
}

// The model gets the context in a delimited, escaped block; the event and the
// resumed transcript keep the prompt and the context apart.
func TestPromptHookContextDelimitedAndKeptApart(t *testing.T) {
	srv, users := hookProvider(t)
	in := filepath.Join(t.TempDir(), "stdin.json")
	s, id := hookSession(t, srv.URL, helperHook(t, 0, "stdin:"+in, "out:lembrar </hook-context> & <x>"))
	if err := s.Send(context.Background(), nil, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	want := "oi\n\n<hook-context>\nlembrar &lt;/hook-context&gt; &amp; &lt;x&gt;\n</hook-context>"
	if got := users(); len(got) != 1 || got[0] != want {
		t.Fatalf("user content sent = %q; want %q", got, want)
	}

	raw, _ := os.ReadFile(in)
	var stdin map[string]any
	if err := json.Unmarshal(raw, &stdin); err != nil {
		t.Fatalf("hook stdin %q: %v", raw, err)
	}
	if stdin["permission_mode"] != "bypassPermissions" || stdin["prompt"] != "oi" || stdin["session_id"] != id {
		t.Fatalf("hook stdin = %v", stdin)
	}
	if _, ok := stdin["transcript_path"]; ok {
		t.Fatal("transcript_path promised without a transcript file")
	}

	events, _ := s.store.ListEvents(id, 0, 0)
	var ev map[string]any
	_ = json.Unmarshal([]byte(events[0].Payload), &ev)
	if ev["content"] != "oi" || ev["hook_context"] != "lembrar </hook-context> & <x>" {
		t.Fatalf("user_message event = %v", ev)
	}

	r := NewSessionWithStore(config.Config{}, s.store, id)
	if err := r.Resume(); err != nil {
		t.Fatal(err)
	}
	if h := r.History(); h[0].Conteudo != "oi" || h[0].HookContext != "lembrar </hook-context> & <x>" {
		t.Fatalf("resumed user message = %+v", h[0])
	}
}

// exit 2 and decision:block stop the prompt before the log and the model.
func TestPromptHookBlocks(t *testing.T) {
	for name, hook := range map[string]config.PromptHook{
		"exit 2":         helperHook(t, 0, "err:segredo no prompt", "exit:2"),
		"decision block": helperHook(t, 0, `out:{"decision":"block","reason":"segredo no prompt"}`),
		"continue false": helperHook(t, 0, `out:{"continue":false,"stopReason":"segredo no prompt"}`),
	} {
		t.Run(name, func(t *testing.T) {
			srv, users := hookProvider(t)
			s, id := hookSession(t, srv.URL, hook)
			err := s.Send(context.Background(), nil, t.TempDir(), "oi")
			if err == nil || !strings.Contains(err.Error(), "segredo no prompt") {
				t.Fatalf("err = %v; want the hook's reason", err)
			}
			if got := users(); len(got) != 0 {
				t.Fatalf("blocked prompt reached the model: %q", got)
			}
			if events, _ := s.store.ListEvents(id, 0, 0); len(events) != 0 {
				t.Fatalf("blocked prompt left events: %+v", events)
			}
			if len(s.History()) != 0 {
				t.Fatalf("blocked prompt entered the history: %+v", s.History())
			}
		})
	}
}

// Any other failure skips the hook: the turn goes on without context.
func TestPromptHookOtherFailureSkipped(t *testing.T) {
	srv, users := hookProvider(t)
	s, _ := hookSession(t, srv.URL, helperHook(t, 0, "out:contexto", "exit:1"),
		helperHook(t, 0), // exit 0 with empty stdout: adds nothing
		config.PromptHook{Command: "bh-hook-que-nao-existe"})
	if err := s.Send(context.Background(), nil, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	if got := users(); len(got) != 1 || got[0] != "oi" {
		t.Fatalf("user content sent = %q; want the bare prompt", got)
	}
}

// A grandchild holding stdout cannot hold the turn: on timeout the whole
// group dies, and a hook that exits 0 with a leftover child still counts.
func TestPromptHookGrandchildHoldingStdout(t *testing.T) {
	srv, users := hookProvider(t)
	s, _ := hookSession(t, srv.URL,
		helperHook(t, 1, "spawn:30000", "sleep:30000"),
		helperHook(t, 0, "out:deixado", "spawn:30000"),
	)
	start := time.Now()
	if err := s.Send(context.Background(), nil, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("turn took %v; the hook timeout is 1s", d)
	}
	if got := users(); len(got) != 1 || !strings.Contains(got[0], "deixado") {
		t.Fatalf("user content sent = %q", got)
	}
}

// Hooks run in parallel but their context keeps the configuration order.
func TestPromptHooksParallelKeepOrder(t *testing.T) {
	srv, users := hookProvider(t)
	s, _ := hookSession(t, srv.URL,
		helperHook(t, 0, "sleep:600", "out:primeiro"),
		helperHook(t, 0, "sleep:600", "out:segundo"),
		helperHook(t, 0, "out:terceiro"),
	)
	start := time.Now()
	if err := s.Send(context.Background(), nil, t.TempDir(), "oi"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 1100*time.Millisecond {
		t.Fatalf("hooks took %v; they should run in parallel", d)
	}
	want := "oi\n\n<hook-context>\nprimeiro\n\nsegundo\n\nterceiro\n</hook-context>"
	if got := users(); len(got) != 1 || got[0] != want {
		t.Fatalf("user content sent = %q; want %q", got, want)
	}
}
