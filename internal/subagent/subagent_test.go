// Package subagent — end-to-end tests with a fake provider (SSE), in the same
// format as runtime_test.go: chunks with choices[].delta.content, a final chunk
// with finish_reason and usage, and "data: [DONE]".
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// ── fake provider ──────────────────────────────────────────────────────────

// reqView is what the fake needs to see of the request: the offered tools and
// the number of messages (to know whether it is the sub-agent's first round).
type reqView struct {
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
	Messages []struct {
		Role string `json:"role"`
	} `json:"messages"`
}

// fakeProvider serves /chat/completions from a script. It returns text for
// the round; while a tool request has no tool result yet, it asks for a real
// built-in tool (list) to exercise the loop.
type fakeProvider struct {
	mu          sync.Mutex
	calls       int
	views       []reqView
	text        string
	delay       time.Duration
	requestTool bool // if true, round 1 asks for list before answering
}

func (f *fakeProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var view reqView
	_ = json.NewDecoder(r.Body).Decode(&view)
	f.mu.Lock()
	f.calls++
	f.views = append(f.views, view)
	n := f.calls
	text, delay, requestTool := f.text, f.delay, f.requestTool
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	emit := func(p any) {
		b, _ := json.Marshal(p)
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}

	hasToolResult := false
	for _, m := range view.Messages {
		if m.Role == "tool" {
			hasToolResult = true
		}
	}

	// Round 1 asks for a built-in; round 2 (already with tool_result) answers text.
	if requestTool && n == 1 && !hasToolResult {
		emit(map[string]any{"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{
					"name":      "list",
					"arguments": `{"depth":1}`,
				},
			}}},
		}}})
	} else {
		if delay > 0 {
			time.Sleep(delay)
		}
		for _, p := range []string{text} {
			emit(map[string]any{"choices": []map[string]any{{
				"delta": map[string]any{"content": p},
			}}})
		}
	}
	emit(map[string]any{
		"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 42, "completion_tokens": 7},
	})
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
}

func (f *fakeProvider) Views() []reqView {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]reqView(nil), f.views...)
}

func newServer(t *testing.T, text string, delay time.Duration, requestTool bool) (*fakeProvider, config.Config) {
	t.Helper()
	f := &fakeProvider{text: text, delay: delay, requestTool: requestTool}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := config.Config{
		CurrentProvider: "fake",
		Providers: map[string]*config.Provider{
			"fake": {BaseURL: srv.URL, APIKey: "k"},
		},
		Model:     "fake-model",
		Janela:    100_000,
		MaxTokens: 512,
	}
	return f, cfg
}

// waitState polls until the task reaches a terminal state (or the deadline
// expires). Avoids fixed sleeps — the suite must be fast and stable.
func waitState(t *testing.T, g *Gerente, id string, accepted ...string) Tarefa {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cp, ok := g.snapshot(id)
		if !ok {
			t.Fatalf("task %s does not exist", id)
		}
		for _, a := range accepted {
			if cp.Estado == a {
				return cp
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	cp, _ := g.snapshot(id)
	t.Fatalf("task %s did not reach %v (current state: %s, error: %s)", id, accepted, cp.Estado, cp.Erro)
	return Tarefa{}
}

// runTool runs a catalog spec with the given JSON argument.
func runTool(t *testing.T, s tools.Spec, args string) (string, error) {
	t.Helper()
	cwd := t.TempDir()
	return s.Exec(context.Background(), cwd, []byte(args))
}

// ── Tests ──────────────────────────────────────────────────────────────────

// Test A: the sub-agent runs the full loop (tool + text), the result comes
// back through task_result and — central invariant — the provider NEVER sees a
// delegation tool (the spawn launcher, task_status/task_result) in the catalog sent.
func TestSpawnRunsAndResultReturnsWithoutDelegationTools(t *testing.T) {
	f, cfg := newServer(t, "task done", 0, true)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), statusSpec(g, "task"), resultSpec(g, "task"))

	byName := map[string]tools.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(specs))
	}
	for _, name := range []string{"task", "task_status", "task_result"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("tool %q missing from the catalog: %v", name, keys(byName))
		}
	}

	out, err := runTool(t, byName["task"], `{"prompt":"list the directory and answer"}`)
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	if !strings.Contains(out, "launched: t1") {
		t.Fatalf("unexpected spawn response: %q", out)
	}

	var id string
	for _, tk := range g.Tarefas() {
		id = tk.ID
	}
	if id != "t1" {
		t.Fatalf("deterministic id should be t1, got %q", id)
	}

	cp := waitState(t, g, id, Completed, Failed)
	if cp.Estado != Completed {
		t.Fatalf("task should complete, state %s error %q", cp.Estado, cp.Erro)
	}
	if cp.Resultado != "task done" {
		t.Fatalf("unexpected result: %q", cp.Resultado)
	}
	if len(cp.Transcricao()) != 1 || !strings.Contains(cp.Transcricao()[0], "list") {
		t.Fatalf("tool trace should contain list: %v", cp.Transcricao())
	}
	events := cp.Conversa()
	if len(events) < 2 || events[0].Tipo != EventoToolCall || events[len(events)-1].Tipo != EventoResposta {
		t.Fatalf("conversation should contain the tool call and the final answer: %+v", events)
	}
	call := events[0]
	if call.Nome != "list" || call.Erro || call.Texto == "" {
		t.Fatalf("the finished tool call should carry its result: %+v", call)
	}
	if call.Executando {
		t.Fatalf("a finished tool call must not stay marked as running: %+v", call)
	}
	if events[len(events)-1].Texto != "task done" {
		t.Fatalf("assistant event lost the final text: %+v", events[len(events)-1])
	}
	// Snapshots do not share the event slice with the running task.
	events[0].Nome = "changed"
	if g.Fotos()[0].Conversa()[0].Nome != "list" {
		t.Fatal("task conversation snapshot aliases internal storage")
	}

	// Result through the tool.
	res, err := runTool(t, byName["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result failed: %v", err)
	}
	if res != "task done" {
		t.Fatalf("task_result should return the final text, got %q", res)
	}

	// INVARIANT: no request to the provider carried a delegation tool.
	for i, v := range f.Views() {
		if len(v.Tools) == 0 {
			t.Fatalf("request %d offered no tools — the child should have the built-ins", i)
		}
		for _, tl := range v.Tools {
			name := tl.Function.Name
			if name == "task" || strings.HasPrefix(name, "task_") {
				t.Fatalf("request %d offered %q: a sub-agent must NOT have delegation tools", i, name)
			}
		}
		if !hasBuiltin(v, "list") {
			t.Fatalf("request %d should offer the runtime built-ins (list among them)", i)
		}
	}
}

// Test B: no cap — N prompts in one call launch N sub-agents, all running at
// the same time, and the parent is notified when they finish.
func TestSpawnManyWithoutCapAndNotifies(t *testing.T) {
	_, cfg := newServer(t, "ok", 400*time.Millisecond, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	notices := make(chan struct{}, 16)
	g.AoTerminar(func() { notices <- struct{}{} })
	specs := append(Especificacoes(g, cfg), statusSpec(g, "task"), resultSpec(g, "task"))
	var spawn tools.Spec
	for _, s := range specs {
		if s.Name == "task" {
			spawn = s
		}
	}
	out, err := runTool(t, spawn, `{"prompts":["a","b","c","d","e"]}`)
	if err != nil || !strings.Contains(out, "5 sub-agent(s) launched: t1, t2, t3, t4, t5") {
		t.Fatalf("spawn = %q %v", out, err)
	}
	running := 0
	for _, tk := range g.Fotos() {
		if tk.Estado == Running {
			running++
		}
	}
	if running != 5 {
		t.Fatalf("all should run together, running=%d", running)
	}
	if g.Ativas() != 5 {
		t.Fatalf("active = %d", g.Ativas())
	}
	delivered := 0
	deadline := time.After(5 * time.Second)
	for delivered < 5 {
		select {
		case <-notices:
			delivered += len(g.Terminadas())
		case <-deadline:
			t.Fatalf("only %d delivered", delivered)
		}
	}
	if len(g.Terminadas()) != 0 {
		t.Fatal("Terminadas must not repeat an already delivered task")
	}
	msg := AvisoTerminadas(g.Fotos(), 0)
	if !strings.HasPrefix(msg, "[subagents]") || !strings.Contains(msg, "## t5 completed") || !strings.Contains(msg, "ok") {
		t.Fatalf("notice:\n%s", msg)
	}
}

// Test C: cancelling a running task takes it to cancelled and status shows it.
func TestCancelRunningTask(t *testing.T) {
	_, cfg := newServer(t, "never going to answer", 2*time.Second, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), statusSpec(g, "task"), resultSpec(g, "task"))
	byName := map[string]tools.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	id, err := g.Spawn(context.Background(), "takes a long time")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}

	// Wait for the child session to actually start (the provider got a call).
	deadline := time.Now().Add(2 * time.Second)
	for {
		cp, _ := g.snapshot(id)
		if cp.Estado == Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task did not start (state %s)", cp.Estado)
		}
		time.Sleep(2 * time.Millisecond)
	}

	if !g.Cancelar(id) {
		t.Fatal("Cancelar should return true for a running task")
	}

	cp := waitState(t, g, id, Cancelled, Completed)
	if cp.Estado != Cancelled {
		t.Fatalf("state should be cancelled, got %s", cp.Estado)
	}

	// Only after the terminal state is there nothing left to cancel: calling
	// again returns false. (Before that the worker has not written the state.)
	if g.Cancelar(id) {
		t.Fatal("Cancelar of an already cancelled task should return false")
	}

	out, err := runTool(t, byName["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result failed: %v", err)
	}
	if !strings.HasPrefix(out, "task "+id+" cancelled") {
		t.Fatalf("result of a cancelled task should be a state line: %q", out)
	}

	status, _ := runTool(t, byName["task_status"], `{}`)
	if !strings.Contains(status, Cancelled) {
		t.Fatalf("status should show cancelled:\n%s", status)
	}
}

// Test D: task_result with an unknown id returns a ONE-line error; cancelling
// an unknown id returns false; and an empty prompt creates no task.
func TestResultUnknownIdAndInputErrors(t *testing.T) {
	_, cfg := newServer(t, "ok", 0, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), statusSpec(g, "task"), resultSpec(g, "task"))
	byName := map[string]tools.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}

	_, err := runTool(t, byName["task_result"], `{"id":"t99"}`)
	if err == nil {
		t.Fatal("task_result of an unknown id should fail")
	}
	if strings.Contains(err.Error(), "\n") {
		t.Fatalf("error should be ONE line: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "t99") {
		t.Fatalf("error should mention the id: %q", err.Error())
	}

	if g.Cancelar("t99") {
		t.Fatal("Cancelar of an unknown id should return false")
	}

	if _, err := g.Spawn(context.Background(), "   "); err == nil {
		t.Fatal("spawn with an empty prompt should fail")
	}

	// spawn without arguments also fails (prompt is required).
	if _, err := runTool(t, byName["task"], `{}`); err == nil {
		t.Fatal("spawn without a prompt should fail")
	}

	// Status without tasks is an informative line, not an error.
	out, err := runTool(t, byName["task_status"], `{}`)
	if err != nil {
		t.Fatalf("status without tasks should not fail: %v", err)
	}
	if !strings.Contains(out, "no tasks") {
		t.Fatalf("unexpected empty status: %q", out)
	}
}

// Test E: without a ready config the task is born failed with a one-line
// error (the manager neither panics nor stalls the queue).
func TestSpawnWithoutConfigFailsCleanly(t *testing.T) {
	g := Novo(Config{})
	g.Vincular(context.Background())
	id, err := g.Spawn(context.Background(), "something")
	if err != nil {
		t.Fatalf("spawn should not fail because of config, only fail the task: %v", err)
	}
	cp := waitState(t, g, id, Failed)
	if cp.Erro == "" || strings.Contains(cp.Erro, "\n") {
		t.Fatalf("error should be set and one line: %q", cp.Erro)
	}
}

// Test F: the result comes back truncated at 16 KiB when the sub-agent writes a lot.
func TestResultTruncatedAtCap(t *testing.T) {
	big := strings.Repeat("x", TetoResultado+5000)
	_, cfg := newServer(t, big, 0, false)
	g := Novo(Config{})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	specs := append(Especificacoes(g, cfg), statusSpec(g, "task"), resultSpec(g, "task"))
	byName := map[string]tools.Spec{}
	for _, s := range specs {
		byName[s.Name] = s
	}
	id, err := g.Spawn(context.Background(), "write a lot")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	waitState(t, g, id, Completed, Failed)
	out, err := runTool(t, byName["task_result"], fmt.Sprintf(`{"id":%q}`, id))
	if err != nil {
		t.Fatalf("result failed: %v", err)
	}
	if len(out) <= TetoResultado || !strings.Contains(out, "truncated at") {
		t.Fatalf("result should come back truncated at the cap (got %d bytes)", len(out))
	}
}

// ── helpers ────────────────────────────────────────────────────────────────

func keys(m map[string]tools.Spec) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func hasBuiltin(v reqView, name string) bool {
	for _, tl := range v.Tools {
		if tl.Function.Name == name {
			return true
		}
	}
	return false
}

// The model only gets the launcher ("task" here, "subagent" by default):
// without status/result it has no way to keep
// polling in a loop, and results arrive through the automatic notice.
func TestModelOnlyGetsSpawn(t *testing.T) {
	specs := Especificacoes(Novo(Config{Nome: "task"}), config.Config{})
	if len(specs) != 1 || specs[0].Name != "task" {
		t.Fatalf("model tools = %v", specs)
	}
}

// One launch per turn: the second call in the same turn is rejected; in a
// new turn (another ctx) it works again.
func TestOneSpawnPerTurn(t *testing.T) {
	_, cfg := newServer(t, "ok", 0, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	spawn := Especificacoes(g, cfg)[0]
	turn1, end1 := context.WithCancel(context.Background())
	defer end1()
	if _, err := spawn.Exec(turn1, t.TempDir(), []byte(`{"prompts":["a","b"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := spawn.Exec(turn1, t.TempDir(), []byte(`{"prompt":"dummy"}`)); err == nil || !strings.Contains(err.Error(), "END THE TURN") {
		t.Fatalf("second spawn in the turn should be rejected: %v", err)
	}
	turn2, end2 := context.WithCancel(context.Background())
	defer end2()
	if _, err := spawn.Exec(turn2, t.TempDir(), []byte(`{"prompt":"c"}`)); err != nil {
		t.Fatalf("a new turn should launch: %v", err)
	}
	if n := len(g.Fotos()); n != 3 {
		t.Fatalf("tasks = %d", n)
	}
}
