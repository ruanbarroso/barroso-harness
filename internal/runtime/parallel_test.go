package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// recRenderer records the order of renderer calls and flags concurrent use.
type recRenderer struct {
	mu     sync.Mutex
	busy   bool
	clash  bool
	events []string
}

func (r *recRenderer) enter() {
	r.mu.Lock()
	if r.busy {
		r.clash = true
	}
	r.busy = true
	r.mu.Unlock()
	time.Sleep(time.Millisecond)
	r.mu.Lock()
	r.busy = false
	r.mu.Unlock()
}

func (r *recRenderer) add(e string) {
	r.enter()
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recRenderer) Resposta(string)                           {}
func (r *recRenderer) Tool(name string, _ time.Duration, _ bool) { r.add("tool:" + name) }
func (r *recRenderer) Eco(out string)                            { r.add("eco:" + out) }
func (r *recRenderer) Guardrail(bool)                            {}
func (r *recRenderer) Tokens(int, int, time.Duration, bool)      {}
func (r *recRenderer) Pensando()                                 {}
func (r *recRenderer) ToolInicio(name, _ string)                 { r.add("start:" + name) }
func (r *recRenderer) Contexto(int)                              {}

// twoCallProvider asks for the given tools in one round, then answers text.
func twoCallProvider(names ...string) *httptest.Server {
	var mu sync.Mutex
	n := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = r.Body.Read(make([]byte, 1))
		mu.Lock()
		n++
		first := n == 1
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) { b, _ := json.Marshal(p); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		if first {
			var calls []map[string]any
			for i, name := range names {
				calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("c%d", i), "type": "function",
					"function": map[string]any{"name": name, "arguments": `{}`}})
			}
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": calls}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func slowTool(name string, d time.Duration) tools.Spec {
	return tools.Spec{Name: name, Schema: map[string]any{"type": "object"},
		Exec: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "out-" + name, nil
		}}
}

func TestParallelToolCallsRunConcurrentlyInOrder(t *testing.T) {
	srv := twoCallProvider("slow_a", "slow_b")
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	// slow_a takes longer, so it finishes after slow_b: the report must still
	// follow call order.
	s.Injetar(slowTool("slow_a", 600*time.Millisecond), slowTool("slow_b", 400*time.Millisecond))
	rr := &recRenderer{}
	start := time.Now()
	if err := s.Send(context.Background(), rr, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el >= 950*time.Millisecond {
		t.Fatalf("tools ran sequentially: %s", el)
	}
	if rr.clash {
		t.Fatal("renderer was called concurrently")
	}
	want := []string{"start:slow_a", "start:slow_b", "tool:slow_a", "eco:out-slow_a", "tool:slow_b", "eco:out-slow_b"}
	if strings.Join(rr.events, ",") != strings.Join(want, ",") {
		t.Fatalf("renderer order:\n got %v\nwant %v", rr.events, want)
	}
	h := s.History()
	var order []string
	for _, m := range h {
		if m.Role == "tool_call" || m.Role == "tool_result" {
			order = append(order, m.Role+":"+m.ToolName+":"+m.Conteudo)
		}
	}
	wantH := "tool_call:slow_a:,tool_call:slow_b:,tool_result:slow_a:out-slow_a,tool_result:slow_b:out-slow_b"
	if strings.Join(order, ",") != wantH {
		t.Fatalf("transcript order: %v", order)
	}
}

type liveCompletionRenderer struct {
	recRenderer
	completed chan string
}

func (r *liveCompletionRenderer) ToolComecou(string, string, string) {}
func (r *liveCompletionRenderer) ToolSaida(string, string)           {}
func (r *liveCompletionRenderer) ToolTerminou(_, name, _, _ string, _ time.Duration, _ bool) {
	r.completed <- name
}

func TestParallelLiveToolsCompleteAsTheyFinish(t *testing.T) {
	srv := twoCallProvider("slow_a", "fast_b")
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	releaseSlow := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseSlow) }) }
	defer release()
	s.Injetar(
		tools.Spec{Name: "slow_a", Schema: map[string]any{"type": "object"}, Exec: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			select {
			case <-releaseSlow:
				return "out-slow_a", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}},
		tools.Spec{Name: "fast_b", Schema: map[string]any{"type": "object"}, Exec: func(context.Context, string, json.RawMessage) (string, error) {
			return "out-fast_b", nil
		}},
	)
	rr := &liveCompletionRenderer{completed: make(chan string, 2)}
	finished := make(chan error, 1)
	go func() { finished <- s.Send(context.Background(), rr, t.TempDir(), "go") }()

	select {
	case name := <-rr.completed:
		if name != "fast_b" {
			t.Fatalf("first live completion = %q, want fast_b", name)
		}
	case <-time.After(time.Second):
		t.Fatal("fast tool did not report completion while the earlier call was still blocked")
	}
	release()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not finish after releasing the slow tool")
	}
	if name := <-rr.completed; name != "slow_a" {
		t.Fatalf("second live completion = %q, want slow_a", name)
	}

	var results []string
	for _, m := range s.History() {
		if m.Role == "tool_result" {
			results = append(results, m.ToolName+":"+m.Conteudo)
		}
	}
	if got, want := strings.Join(results, ","), "slow_a:out-slow_a,fast_b:out-fast_b"; got != want {
		t.Fatalf("tool results should remain in call order: got %q, want %q", got, want)
	}
}

func TestMutatingToolForcesSequentialRound(t *testing.T) {
	srv := twoCallProvider("slow_a", "context_write")
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	s.Injetar(slowTool("slow_a", 50*time.Millisecond))
	rr := &recRenderer{}
	if err := s.Send(context.Background(), rr, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	// Sequential: start/tool of the first call come before the second start.
	if len(rr.events) < 4 || rr.events[0] != "start:slow_a" || rr.events[1] != "tool:slow_a" {
		t.Fatalf("expected sequential execution, got %v", rr.events)
	}
}

func TestSystemPromptFacts(t *testing.T) {
	p := systemPrompt(`C:\work`, "project rule", true, time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC), "")
	for _, want := range []string{
		runtime.GOOS + "/" + runtime.GOARCH,
		"ISSUE THEM ALL IN THE SAME RESPONSE",
		"## Progress updates",
		"Before a lengthy phase of tool work, send one concise, concrete sentence",
		"ordinary assistant message text (`content`/commentary), not as a `thinking`-channel update",
		"This applies only to progress updates and does not restrict your independent reasoning",
		"visible assistant text of the same response as the tool call",
		"Do not give a full upfront plan",
		"continue the work immediately",
		`C:\work`,
		"2026-10-19",
		"subagent",
		"## BARROSO.md (system instructions)\nproject rule",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("system prompt lacks %q:\n%s", want, p)
		}
	}
	if runtime.GOOS == "windows" && !strings.Contains(p, "cmd.exe") {
		t.Fatalf("windows prompt must name cmd.exe:\n%s", p)
	}
	if strings.Contains(systemPrompt("x", "", false, time.Now(), ""), "subagent") {
		t.Fatal("subagent line must only appear when the tool exists")
	}
}
