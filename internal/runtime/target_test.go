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

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
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

func TestValidCompactCall(t *testing.T) {
	ok := dealer.ToolChamada{ID: "1", Nome: compactToolName, Args: `{"state":"x"}`}
	if !validCompactCall([]dealer.ToolChamada{{Nome: "read", Args: `{}`}, ok}) {
		t.Error("a real compact call must be accepted")
	}
	for _, bad := range [][]dealer.ToolChamada{nil, {{Nome: "read", Args: `{}`}}, {{Nome: compactToolName, Args: `{"state":"  "}`}}, {{Nome: compactToolName, Args: `not json`}}} {
		if validCompactCall(bad) {
			t.Errorf("must be rejected: %+v", bad)
		}
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

func (b ladderBody) forced() bool {
	m, ok := b.ToolChoice.(map[string]any)
	return ok && m["type"] == "function"
}

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

// newLadderServer answers a forced request with a context_compact call when
// obey is true; every other request gets plain text.
func newLadderServer(t *testing.T, obey bool) *ladderServer {
	ls := &ladderServer{}
	ls.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b ladderBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		ls.mu.Lock()
		ls.bodies = append(ls.bodies, b)
		ls.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) {
			j, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(j) + "\n\n"))
		}
		if b.forced() && obey {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "cmp1", "type": "function",
				"function": map[string]any{"name": "context_compact", "arguments": `{"state":"Goal: finish the report. Next: send it."}`},
			}}}}}})
		} else {
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

func TestLadderRequiresCompactionOnTheWire(t *testing.T) {
	srv := newLadderServer(t, true)
	st := testStore(t)
	const id = "bh-20240101140000-lad1"
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 20_000
	seedHeavy(t, NewSessionWithStore(cfg, st, id))

	s := NewSessionWithStore(cfg, st, id)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "current request: wrap up"); err != nil {
		t.Fatal(err)
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 2 {
		t.Fatalf("requests = %d; want the forced one plus the compacted one", len(srv.bodies))
	}
	first, second := srv.bodies[0], srv.bodies[1]
	if !first.forced() {
		t.Fatalf("the first request must force context_compact: %v", first.ToolChoice)
	}
	if !strings.Contains(first.text(), "The context is over the working target") {
		t.Fatal("the demand text must travel with the forced request")
	}
	if second.forced() || strings.Contains(second.text(), "The context is over the working target") {
		t.Fatal("the demand must not outlive its request")
	}
	if len(second.Messages) > 6 {
		t.Fatalf("compacted request still has %d messages", len(second.Messages))
	}
	if !strings.Contains(second.text(), "Goal: finish the report") || !strings.Contains(second.text(), "user asked: request 0") {
		var roles []string
		for _, m := range second.Messages {
			c, _ := m["content"].(string)
			roles = append(roles, fmt.Sprintf("%v:%.80q", m["role"], c[max(len(c)-80, 0):]))
		}
		t.Fatalf("state and facts must reach the next request: %d messages\n%s", len(second.Messages), strings.Join(roles, "\n"))
	}
	if !strings.Contains(second.text(), "current request: wrap up") {
		t.Fatal("the current request must survive")
	}
	for _, m := range s.History() {
		if strings.HasPrefix(m.Conteudo, contextNotePrefix) {
			t.Fatalf("a [context] notice leaked into the transcript: %q", m.Conteudo)
		}
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
		if e.Kind == eventCompaction && strings.Contains(e.Payload, `"mantem":`) {
			found = true
		}
	}
	if !found {
		t.Fatal("the compaction event must record `mantem` for a deterministic replay")
	}
}

// A gateway that ignores tool_choice must neither end the turn on text nor
// leave the context inflated: after maxDemands the harness compacts itself.
func TestLadderFallsBackWhenTheGatewayIgnoresToolChoice(t *testing.T) {
	srv := newLadderServer(t, false)
	st := testStore(t)
	const id = "bh-20240101140000-lad2"
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 20_000
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
	forced := 0
	for _, b := range srv.bodies {
		if b.forced() {
			forced++
		}
	}
	if forced != maxDemands {
		t.Fatalf("forced requests = %d; want %d", forced, maxDemands)
	}
	last := srv.bodies[len(srv.bodies)-1]
	if last.forced() || len(last.Messages) > 6 {
		t.Fatalf("the final request must be the compacted, unforced one: forced=%v messages=%d", last.forced(), len(last.Messages))
	}
	h := s.History()
	if !isSummary(h[0].Conteudo) || h[len(h)-1].Conteudo != "done" {
		t.Fatalf("history: %+v", h)
	}
}

// claude-*-5-5 behind the BK answers a FORCED tool_choice with HTTP 400
// tool_choice_nao_suportado (Zoe, conversation 105570). The turn must survive:
// one rejected attempt, then the mechanical state, and never forced again.
func TestLadderSurvivesGatewayRejectingForcedToolChoice(t *testing.T) {
	var mu sync.Mutex
	var bodies []ladderBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b ladderBody
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		if b.forced() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","code":"tool_choice_nao_suportado","message":"\"claude-sonnet-5-5\" só aceita \"tool_choice\" ausente, \"auto\" ou \"none\": o raciocínio adaptativo não permite forçar ferramenta."}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, p := range []any{
			map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}},
			map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}},
		} {
			j, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(j) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	st := testStore(t)
	const id = "bh-20240101140000-lad3"
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 20_000
	seedHeavy(t, NewSessionWithStore(cfg, st, id))
	s := NewSessionWithStore(cfg, st, id)
	if err := s.Resume(); err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "current request"); err != nil {
		t.Fatalf("a rejected forced tool_choice must not kill the turn: %v", err)
	}
	forced := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, b := range bodies {
			if b.forced() {
				n++
			}
		}
		return n
	}
	if got := forced(); got != 1 {
		t.Fatalf("forced requests = %d; want exactly the one the gateway rejected", got)
	}
	mu.Lock()
	last := bodies[len(bodies)-1]
	mu.Unlock()
	if last.forced() || len(last.Messages) > 6 {
		t.Fatalf("the final request must be compacted and unforced: forced=%v messages=%d", last.forced(), len(last.Messages))
	}
	h := s.History()
	if !isSummary(h[0].Conteudo) || h[len(h)-1].Conteudo != "done" {
		t.Fatalf("history: %+v", h)
	}
	if !s.forcedChoiceRejected() {
		t.Fatal("the rejection must be remembered for this provider/model")
	}

	// Remembered: a second heavy turn goes straight to the mechanical state.
	seedHeavy(t, s)
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "again"); err != nil {
		t.Fatal(err)
	}
	if got := forced(); got != 1 {
		t.Fatalf("forced requests after the second turn = %d; want still 1", got)
	}
}

// Below the aviso step nothing happens: no extra request, no note, no prune.
func TestLadderQuietBelowTheTarget(t *testing.T) {
	srv := newLadderServer(t, true)
	cfg := fakeConfig(t, srv.URL)
	cfg.AlvoContexto = 150_000
	s := NewSession(cfg)
	if err := s.Send(context.Background(), RendererDe(&strings.Builder{}), t.TempDir(), "hello"); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.bodies) != 1 || srv.bodies[0].forced() || strings.Contains(srv.bodies[0].text(), "working target") {
		t.Fatalf("a small conversation must not be disturbed: %+v", srv.bodies)
	}
}

// Near the target the model gets a one-request advisory that never enters the
// transcript (it would inflate every later prompt by itself).
func TestLadderAdvisoryIsEphemeral(t *testing.T) {
	srv := newLadderServer(t, true)
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
