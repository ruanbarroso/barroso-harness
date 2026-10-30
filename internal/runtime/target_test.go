package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

func round(id, tool, args, result string) []dealer.Mensagem {
	return []dealer.Mensagem{
		{Role: "tool_call", ToolID: id, ToolName: tool, ToolArgs: args},
		{Role: "tool_result", ToolID: id, ToolName: tool, Conteudo: result},
	}
}

func TestDegrau(t *testing.T) {
	const alvo = 100_000
	for tokens, want := range map[int]string{0: "", 50_000: "", 64_000: "", 65_000: "aviso", 84_000: "aviso", 85_000: "poda", 99_000: "poda", 100_000: "exige", 400_000: "exige"} {
		if got := degrau(tokens, alvo); got != want {
			t.Errorf("degrau(%d) = %q; want %q", tokens, got, want)
		}
	}
	if degrau(500_000, 0) != "" {
		t.Error("no target, no ladder")
	}
}

// compactTurn drops the turn's OLDER rounds (the case compactHistory could not
// shrink), keeps the request and the newest rounds, and appends the harness's
// facts: what the user asked and which files were touched.
func TestCompactTurnDropsOlderRoundsAndKeepsFacts(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "first request: fix the parser"},
		{Role: "assistant", Conteudo: "done"},
		{Role: "user", Conteudo: "second request: add a test"},
	}
	for i := 0; i < 6; i++ {
		h = append(h, round(fmt.Sprintf("r%d", i), "read", fmt.Sprintf(`{"path":"file%d.go"}`, i), strings.Repeat("x", 3000))...)
	}
	h = append(h, round("e1", "edit", `{"path":"parse.go"}`, "ok")...)

	out, ok := compactTurn(h, "Goal: parser", 3)
	if !ok {
		t.Fatal("expected a compaction")
	}
	// summary, request, drop note, 3 kept units of 2 messages.
	if len(out) != 3+6 {
		t.Fatalf("len = %d: %+v", len(out), out)
	}
	sum := out[0].Conteudo
	for _, want := range []string{"Goal: parser", "user asked: first request: fix the parser", "read file0.go", "read file3.go", factsHeader} {
		if !strings.Contains(sum, want) {
			t.Errorf("summary missing %q:\n%s", want, sum)
		}
	}
	// The newest rounds stay; their files are not "facts" yet.
	if strings.Contains(sum, "file5.go") || strings.Contains(sum, "parse.go") {
		t.Errorf("kept rounds must not be summarized:\n%s", sum)
	}
	if out[1].Conteudo != "second request: add a test" || out[2].Conteudo != turnDropNote {
		t.Fatalf("request and drop note: %+v", out[1:3])
	}
	if out[len(out)-1].ToolName != "edit" || out[3].ToolID != "r4" {
		t.Fatalf("newest rounds must survive whole: %+v", out[3:])
	}
	if historyBytes(out) > historyBytes(h)/2 {
		t.Fatalf("expected a real reduction: %d -> %d", historyBytes(h), historyBytes(out))
	}
}

// Compacting twice must not forget what the first compaction kept.
func TestCompactTurnFactsAccumulateAcrossCompactions(t *testing.T) {
	h := []dealer.Mensagem{{Role: "user", Conteudo: "refactor the store"}}
	for i := 0; i < 5; i++ {
		h = append(h, round(fmt.Sprintf("a%d", i), "edit", fmt.Sprintf(`{"path":"early%d.go"}`, i), "ok")...)
	}
	first, _ := compactTurn(h, "state one", 2)
	for i := 0; i < 5; i++ {
		first = append(first, round(fmt.Sprintf("b%d", i), "read", fmt.Sprintf(`{"path":"late%d.go"}`, i), "ok")...)
	}
	second, ok := compactTurn(first, "state two", 2)
	if !ok {
		t.Fatal("expected a second compaction")
	}
	sum := second[0].Conteudo
	if strings.Contains(sum, "state one") {
		t.Errorf("the model's old state must be replaced, not stacked:\n%s", sum)
	}
	// The request itself stays verbatim as the message after the summary.
	if second[1].Conteudo != "refactor the store" {
		t.Errorf("the current request must stay verbatim: %+v", second[1])
	}
	for _, want := range []string{"state two", "changed early0.go", "read late0.go"} {
		if !strings.Contains(sum, want) {
			t.Errorf("second summary missing %q:\n%s", want, sum)
		}
	}
	if n := strings.Count(sum, factsHeader); n != 1 {
		t.Errorf("facts block must not stack, found %d", n)
	}
}

func TestCompactTurnNothingToDo(t *testing.T) {
	h := []dealer.Mensagem{{Role: "user", Conteudo: "only"}}
	h = append(h, round("a", "read", `{"path":"a"}`, "x")...)
	if out, ok := compactTurn(h, "s", 3); ok || len(out) != len(h) {
		t.Fatalf("first request with few rounds: ok=%v out=%+v", ok, out)
	}
}

// The batch prune stubs old results (oldest first), names the call in the
// stub, spares the tail, and replays identically on Resume.
func TestPodarStubsOldResultsAndReplays(t *testing.T) {
	h := []dealer.Mensagem{{Role: "user", Conteudo: "go"}}
	for i := 0; i < 20; i++ {
		h = append(h, round(fmt.Sprintf("c%d", i), "read", fmt.Sprintf(`{"path":"f%d.go"}`, i), strings.Repeat("y", 5000))...)
	}
	orig := append([]dealer.Mensagem(nil), h...)
	out, freed, items := podar(h, 30_000)
	if freed < 30_000 || len(items) == 0 {
		t.Fatalf("freed=%d items=%d", freed, len(items))
	}
	if h[2].Conteudo != orig[2].Conteudo {
		t.Fatal("podar must not mutate its input")
	}
	if !strings.Contains(out[2].Conteudo, `read {"path":"f0.go"}`) || !strings.Contains(out[2].Conteudo, "call it again") {
		t.Fatalf("stub should name the call: %q", out[2].Conteudo)
	}
	for i := len(out) - tailProtected; i < len(out); i++ {
		if out[i].Conteudo != orig[i].Conteudo {
			t.Fatalf("tail message %d was touched", i)
		}
	}
	replayed := applyPrunedResults(append([]dealer.Mensagem(nil), orig...), items)
	for i := range out {
		if out[i].Conteudo != replayed[i].Conteudo {
			t.Fatalf("replay differs at %d", i)
		}
	}
	// A log that no longer lines up must not stub the wrong message.
	wrong := applyPrunedResults([]dealer.Mensagem{{Role: "user", Conteudo: "z"}, {Role: "tool_result", ToolID: "other", Conteudo: "keep"}}, []prunedResult{{Index: 1, ID: "c1", Stub: "S"}})
	if wrong[1].Conteudo != "keep" {
		t.Fatal("mismatched tool id must be ignored")
	}
}

// ── end to end ────────────────────────────────────────────────────────────

type ladderServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies []ladderBody
}

type ladderBody struct {
	ToolChoice any              `json:"tool_choice"`
	Messages   []map[string]any `json:"messages"`
}

// forced: the request pins ONE function (a tool_choice object). The harness
// must never send that: claude-*-5-5 rejects it with HTTP 400.
func (b ladderBody) forced() bool {
	m, ok := b.ToolChoice.(map[string]any)
	return ok && m["type"] == "function"
}

// summary: the harness-initiated summarization request (tool_choice "none").
func (b ladderBody) summary() bool { return b.ToolChoice == "none" }

func (b ladderBody) text() string {
	var sb strings.Builder
	for _, m := range b.Messages {
		if c, ok := m["content"].(string); ok {
			sb.WriteString(c)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

const ladderSummary = "Goal: finish the report. Next: send it."

// How the fake gateway answers the summarization request.
const (
	sumOK    = "ok"    // plain text with the state
	sumFail  = "fail"  // HTTP 400 (not retried)
	sumEmpty = "empty" // 200 with no text
	sumTool  = "tool"  // calls a tool although tool_choice is none
)

// newLadderServer answers the summarization request (tool_choice "none")
// according to mode; every other request gets plain text "done". A request
// that pins a function is answered with the BK's 400, as claude-*-5-5 does.
func newLadderServer(t *testing.T, mode string) *ladderServer {
	ls := &ladderServer{}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b ladderBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		ls.mu.Lock()
		ls.bodies = append(ls.bodies, b)
		ls.mu.Unlock()
		if b.forced() || (b.summary() && mode == sumFail) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"tool_choice_nao_suportado","message":"only auto or none"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) {
			j, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(j) + "\n\n"))
		}
		switch {
		case b.summary() && mode == sumTool:
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "x1", "type": "function",
				"function": map[string]any{"name": "context_compact", "arguments": `{"state":"ignored"}`},
			}}}}}})
		case b.summary() && mode == sumEmpty:
		case b.summary():
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": ladderSummary}}}})
		default:
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(ls.Close)
	return ls
}

// seedHeavy writes a conversation whose weight is plain text (nothing the
// batch prune can stub): ~24k tokens against a 20k target.
func seedHeavy(t *testing.T, s *Session) {
	for i := 0; i < 12; i++ {
		s.record("user_message", map[string]any{"role": "user", "content": fmt.Sprintf("request %d %s", i, strings.Repeat("q", 4000))})
		s.record("assistant", map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d %s", i, strings.Repeat("a", 4000))})
	}
}

// runHeavyTurn resumes a heavy conversation and sends one more request.
func runHeavyTurn(t *testing.T, srv *ladderServer, id, prompt string) (*Session, config.Config, *store.Store) {
	t.Helper()
	st := testStore(t)
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 20_000
	seedHeavy(t, NewSessionWithStore(cfg, st, id))
	s := NewSessionWithStore(cfg, st, id)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), prompt); err != nil {
		t.Fatalf("the turn must survive: %v", err)
	}
	return s, cfg, st
}

// Over the target after the stubs, the HARNESS runs one summarization call
// (tool_choice "none" — what Claude Code, Codex and OpenCode do) and the next
// request carries its answer. No request ever pins a function.
func TestLadderSummarizesWithToolChoiceNone(t *testing.T) {
	srv := newLadderServer(t, sumOK)
	const id = "bh-20240101140000-lad1"
	s, cfg, st := runHeavyTurn(t, srv, id, "current request: wrap up")

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 2 {
		t.Fatalf("requests = %d; want the summary plus the compacted one", len(srv.bodies))
	}
	for _, b := range srv.bodies {
		if b.forced() {
			t.Fatalf("a request pinned a function: %v", b.ToolChoice)
		}
	}
	sum, second := srv.bodies[0], srv.bodies[1]
	if !sum.summary() {
		t.Fatalf("the first request must be the summarization (tool_choice none): %v", sum.ToolChoice)
	}
	if !strings.Contains(sum.text(), "the harness is compacting it now") {
		t.Fatal("the summarization instruction must travel with its request")
	}
	if second.summary() || strings.Contains(second.text(), "the harness is compacting it now") {
		t.Fatal("the instruction must not outlive its request")
	}
	if len(second.Messages) > 6 {
		t.Fatalf("compacted request still has %d messages", len(second.Messages))
	}
	if !strings.Contains(second.text(), ladderSummary) || !strings.Contains(second.text(), "user asked: request 0") {
		t.Fatalf("state and facts must reach the next request:\n%s", second.text())
	}
	if !strings.Contains(second.text(), "current request: wrap up") {
		t.Fatal("the current request must survive")
	}
	for _, m := range s.History() {
		if strings.HasPrefix(m.Conteudo, contextNotePrefix) {
			t.Fatalf("a [context] notice leaked into the transcript: %q", m.Conteudo)
		}
	}
	h := s.History()
	if !isSummary(h[0].Conteudo) || h[len(h)-1].Conteudo != "done" {
		t.Fatalf("history: %+v", h)
	}

	// The compaction is replayed by the next process, as it ran.
	again := NewSessionWithStore(cfg, st, id)
	if err := again.Resume(); err != nil {
		t.Fatal(err)
	}
	if got := historyBytes(again.History()); got > 20_000 {
		t.Fatalf("resume should start small, history has %d bytes", got)
	}
	found := false
	for _, e := range mustEvents(t, st, id) {
		if e.Kind == eventCompaction && strings.Contains(e.Payload, `"mantem":`) && strings.Contains(e.Payload, `"origem":"resumo"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the compaction event must record `mantem` and origem=resumo for a deterministic replay")
	}
}

// A summarization that fails (gateway error), comes back empty or calls a
// tool anyway must not end the turn nor leave the context inflated: the
// harness writes the state mechanically. Nothing is ever pinned.
func TestLadderFallsBackToMechanicalStateWhenSummaryFails(t *testing.T) {
	for _, mode := range []string{sumFail, sumEmpty, sumTool} {
		t.Run(mode, func(t *testing.T) {
			srv := newLadderServer(t, mode)
			s, _, st := runHeavyTurn(t, srv, "bh-20240101140000-lad2"+mode, "current request")
			srv.mu.Lock()
			defer srv.mu.Unlock()
			summaries := 0
			for _, b := range srv.bodies {
				if b.forced() {
					t.Fatalf("a request pinned a function: %v", b.ToolChoice)
				}
				if b.summary() {
					summaries++
				}
			}
			if summaries != 1 {
				t.Fatalf("summarization calls = %d; want exactly 1 (no retry loop)", summaries)
			}
			last := srv.bodies[len(srv.bodies)-1]
			if last.summary() || len(last.Messages) > 6 {
				t.Fatalf("the final request must be the compacted one: summary=%v messages=%d", last.summary(), len(last.Messages))
			}
			h := s.History()
			if !isSummary(h[0].Conteudo) || h[len(h)-1].Conteudo != "done" {
				t.Fatalf("history: %+v", h)
			}
			if strings.Contains(h[0].Conteudo, ladderSummary) {
				t.Fatal("the mechanical state must not contain a model summary")
			}
			found := false
			for _, e := range mustEvents(t, st, "bh-20240101140000-lad2"+mode) {
				if e.Kind == eventCompaction && strings.Contains(e.Payload, `"origem":"mecanico"`) {
					found = true
				}
			}
			if !found {
				t.Fatal("the compaction event must record origem=mecanico")
			}
		})
	}
}

// Below the aviso step nothing happens: no extra request, no note, no prune.
func TestLadderQuietBelowTheTarget(t *testing.T) {
	srv := newLadderServer(t, sumOK)
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 150_000
	s := NewSession(cfg)
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hello"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 1 || srv.bodies[0].forced() || srv.bodies[0].summary() || strings.Contains(srv.bodies[0].text(), "working target") {
		t.Fatalf("a small conversation must not be disturbed: %+v", srv.bodies)
	}
}

// Near the target the model gets a one-request advisory that never enters the
// transcript (it would inflate every later prompt by itself).
func TestLadderAdvisoryIsEphemeral(t *testing.T) {
	srv := newLadderServer(t, sumOK)
	st := testStore(t)
	const id = "bh-20240101140000-lad3"
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 40_000 // the seeded ~28k tokens (with prompt) land in the aviso step
	seedHeavy(t, NewSessionWithStore(cfg, st, id))

	s := NewSessionWithStore(cfg, st, id)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "current request"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 1 {
		t.Fatalf("requests = %d", len(srv.bodies))
	}
	if b := srv.bodies[0]; b.forced() || !strings.Contains(b.text(), "near the 40000-token working target") {
		t.Fatalf("the advisory must ride on the request without forcing it:\n%.300s", b.text())
	}
	for _, m := range s.History() {
		if strings.HasPrefix(m.Conteudo, contextNotePrefix) {
			t.Fatalf("the advisory leaked into the transcript: %q", m.Conteudo)
		}
	}
}
