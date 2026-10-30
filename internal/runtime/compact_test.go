package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func TestCompactHistoryKeepsCurrentRequestAndRound(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "old request"},
		{Role: "assistant", Conteudo: "old answer"},
		{Role: "user", Conteudo: "current request"},
		{Role: "tool_call", ToolID: "c1", ToolName: "context_compact", ToolArgs: `{"state":"x"}`},
		{Role: "tool_result", ToolID: "c1", ToolName: "context_compact", Conteudo: "ok"},
	}
	out := compactHistory(h, "goal: X")
	if len(out) != 4 {
		t.Fatalf("expected summary + 3 messages of the current request, got %d: %+v", len(out), out)
	}
	if out[0].Role != "user" || !strings.HasPrefix(out[0].Conteudo, summaryPrefix) || !strings.Contains(out[0].Conteudo, "goal: X") {
		t.Fatalf("first message should be the state: %+v", out[0])
	}
	if out[1].Conteudo != "current request" || out[2].Role != "tool_call" || out[3].Role != "tool_result" {
		t.Fatalf("current request and round should stay intact: %+v", out[1:])
	}
}

// With nothing before the current request there is nothing to compact.
func TestCompactHistoryWithoutEarlierLeavesUnchanged(t *testing.T) {
	h := []dealer.Mensagem{{Role: "user", Conteudo: "only"}}
	if out := compactHistory(h, "e"); len(out) != 1 || out[0].Conteudo != "only" {
		t.Fatalf("should not change: %+v", out)
	}
}

// Compacting again replaces the previous summary instead of stacking.
func TestCompactHistoryRecompactsWithoutStacking(t *testing.T) {
	h := compactHistory([]dealer.Mensagem{
		{Role: "user", Conteudo: "a"}, {Role: "assistant", Conteudo: "b"}, {Role: "user", Conteudo: "c"},
	}, "e1")
	h = append(h, dealer.Mensagem{Role: "assistant", Conteudo: "d"}, dealer.Mensagem{Role: "user", Conteudo: "f"})
	out := compactHistory(h, "e2")
	if len(out) != 2 || !strings.Contains(out[0].Conteudo, "e2") || strings.Contains(out[0].Conteudo, "e1") || out[1].Conteudo != "f" {
		t.Fatalf("wrong recompaction: %+v", out)
	}
}

// A summary written by an older version (Portuguese marker) is still
// recognized as a summary, not as a real user request.
func TestCompactHistoryRecognizesLegacyMarker(t *testing.T) {
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "[estado de trabalho — escrito pelo próprio agente ao compactar o histórico anterior]\nold"},
		{Role: "assistant", Conteudo: "d"},
		{Role: "user", Conteudo: "f"},
	}
	out := compactHistory(h, "new")
	if len(out) != 2 || !strings.Contains(out[0].Conteudo, "new") || out[1].Conteudo != "f" {
		t.Fatalf("legacy marker not recognized: %+v", out)
	}
}

// Messages the runtime injects with role "user" (the image note after a read,
// the loop notice) are not the user's request: compaction must not cut there,
// or the real current request is thrown away with the old history.
func TestCompactHistorySkipsSyntheticUserMessages(t *testing.T) {
	for _, synthetic := range []string{
		readImagesNote,
		"[loop] You have made exactly the same call(s) (read, same arguments) 5 rounds in a row and got the same results.",
	} {
		h := []dealer.Mensagem{
			{Role: "user", Conteudo: "old request"},
			{Role: "assistant", Conteudo: "old answer"},
			{Role: "user", Conteudo: "current request"},
			{Role: "tool_call", ToolID: "r1", ToolName: "read", ToolArgs: `{"path":"a.png"}`},
			{Role: "tool_result", ToolID: "r1", ToolName: "read", Conteudo: tools.ImageAttached + "a.png]\n"},
			{Role: "user", Conteudo: synthetic, Images: []string{"a.png"}},
			{Role: "tool_call", ToolID: "c1", ToolName: "context_compact", ToolArgs: `{"state":"x"}`},
			{Role: "tool_result", ToolID: "c1", ToolName: "context_compact", Conteudo: "ok"},
		}
		out := compactHistory(h, "goal: X")
		if len(out) != 7 || out[1].Conteudo != "current request" {
			t.Fatalf("%q: the current request must survive compaction, got %+v", synthetic[:10], out)
		}
		if len(out[4].Images) != 1 {
			t.Fatalf("the image note after the request must stay with its image: %+v", out[4])
		}
	}
}

// A long turn whose older rounds hold the weight: reduceTurn drops whole
// units OLDEST first until the freed bytes cover the deficit, and keeps the
// newest unit with its riding notes intact.
func TestReduceTurnDropsOldUnitsKeepsNewest(t *testing.T) {
	u := func(id, result string) []dealer.Mensagem {
		return []dealer.Mensagem{
			{Role: "assistant", Conteudo: "checking " + id},
			{Role: "tool_call", ToolID: id, ToolName: "grep", ToolArgs: `{"q":"x"}`},
			{Role: "tool_result", ToolID: id, ToolName: "grep", Conteudo: result},
		}
	}
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: summaryPrefix + "state so far"},
		{Role: "user", Conteudo: "build the parser"},
	}
	h = append(h, u("u1", strings.Repeat("a", 1600))...)
	h = append(h, u("u2", strings.Repeat("b", 1600))...)
	h = append(h, u("u3", strings.Repeat("c", 1600))...)

	got := reduceTurn(h, historyBytes(h[2:5]))
	if got == nil {
		t.Fatal("should drop at least one unit")
	}
	if !isSummary(got[0].Conteudo) || got[1].Conteudo != "build the parser" {
		t.Fatalf("state and request must stay: %+v", got[:2])
	}
	if !strings.HasPrefix(got[2].Conteudo, turnDropNotePrefix) {
		t.Fatalf("the note must replace the dropped units: %+v", got[2])
	}
	all := ""
	for _, m := range got[3:] {
		all += m.Conteudo
	}
	if strings.Contains(all, strings.Repeat("a", 1600)) || strings.Contains(all, "checking u1") {
		t.Fatalf("the OLDEST unit must be gone: %+v", got[3:])
	}
	if !strings.Contains(all, strings.Repeat("c", 1600)) {
		t.Fatalf("the NEWEST unit must stay: %+v", got[3:])
	}
}

// Exactly the deficit: the oldest units go, the middle and newest stay.
func TestReduceTurnStopsAtTheDeficit(t *testing.T) {
	u := func(id string) []dealer.Mensagem {
		return []dealer.Mensagem{
			{Role: "tool_call", ToolID: id, ToolName: "grep", ToolArgs: `{"q":"x"}`},
			{Role: "tool_result", ToolID: id, ToolName: "grep", Conteudo: strings.Repeat(id, 10)},
		}
	}
	h := []dealer.Mensagem{{Role: "user", Conteudo: "request"}}
	h = append(h, u("ua")...)
	h = append(h, u("ub")...)
	h = append(h, u("uc")...)
	h = append(h, u("ud")...)

	got := reduceTurn(h, historyBytes(h[1:5]))
	if got == nil {
		t.Fatal("should cover the deficit")
	}
	if last := got[len(got)-1]; last.ToolID != "ud" {
		t.Fatalf("newest unit must survive: %+v", got[len(got)-3:])
	}
	all := ""
	for _, m := range got {
		all += m.Conteudo
	}
	if strings.Contains(all, strings.Repeat("ua", 10)) {
		t.Fatal("ua should be dropped")
	}
	if !strings.Contains(all, strings.Repeat("uc", 10)) {
		t.Fatalf("uc was covered but not needed — keep it: %+v", got)
	}
}

// The note never stacks: a second reduction replaces it (it drops together
// with the span it rides in), and compactHistory never takes it for the
// user's request.
func TestReduceTurnReplacesNoteAndCutsSkipIt(t *testing.T) {
	u := func(id string) []dealer.Mensagem {
		return []dealer.Mensagem{
			{Role: "tool_call", ToolID: id, ToolName: "grep", ToolArgs: `{"q":"x"}`},
			{Role: "tool_result", ToolID: id, ToolName: "grep", Conteudo: strings.Repeat(id, 100)},
		}
	}
	h := []dealer.Mensagem{
		{Role: "user", Conteudo: "old stuff"},
		{Role: "user", Conteudo: "the request"},
	}
	h = append(h, u("ua")...)
	h = append(h, u("ub")...)
	first := reduceTurn(h, historyBytes(h[2:3]))
	if first == nil {
		t.Fatal("reduction expected")
	}
	// The next reduction frees the span the old note rides in (note + the
	// rounds dropped since), so exactly one note remains.
	again := reduceTurn(append(first, u("uc")...), historyBytes(first[3:5]))
	if again == nil {
		t.Fatal("second reduction expected")
	}
	notes := 0
	for _, m := range again {
		if strings.HasPrefix(m.Conteudo, turnDropNotePrefix) {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("the note must be replaced, not stacked (%d notes): %+v", notes, again)
	}
	// The fresh note and what follows it must not fool the compaction cut.
	out := compactHistory(again, "fresh state")
	if len(out) < 3 || !strings.Contains(out[0].Conteudo, "fresh state") || out[1].Conteudo != "the request" {
		t.Fatalf("the cut should land on the real request: %+v", out)
	}
	for _, m := range out {
		if strings.Contains(m.Conteudo, strings.Repeat("ua", 10)) || strings.Contains(m.Conteudo, strings.Repeat("ub", 10)) {
			t.Fatalf("dropped rounds must not survive a later compaction: %+v", out)
		}
	}
}

// Not enough to free even dropping all but the newest unit: honest nil — and
// the newest/singles unit never drops.
func TestReduceTurnGivesUpWhenDeficitTooBig(t *testing.T) {
	u := func(id string) []dealer.Mensagem {
		return []dealer.Mensagem{
			{Role: "tool_call", ToolID: id, ToolName: "grep", ToolArgs: `{"q":"x"}`},
			{Role: "tool_result", ToolID: id, ToolName: "grep", Conteudo: strings.Repeat(id, 50)},
		}
	}
	h := []dealer.Mensagem{{Role: "user", Conteudo: "request"}}
	h = append(h, u("ua")...)
	h = append(h, u("ub")...)
	if got := reduceTurn(h, historyBytes(h)+1<<10); got != nil {
		t.Fatalf("nil expected when nothing can free enough: %+v", got)
	}
	single := append([]dealer.Mensagem{{Role: "user", Conteudo: "req"}}, u("u")...)
	if got := reduceTurn(single, 10); got != nil {
		t.Fatalf("a single unit must never drop: %+v", got)
	}
}

// Steer messages and image notes ride with the unit that follows them: a
// kept unit keeps the instructions and images that arrived for it.
func TestReduceTurnKeepsGapWithTheFollowingUnit(t *testing.T) {
	u := func(id, result string) []dealer.Mensagem {
		return []dealer.Mensagem{
			{Role: "tool_call", ToolID: id, ToolName: "read", ToolArgs: `{"path":"x"}`},
			{Role: "tool_result", ToolID: id, ToolName: "read", Conteudo: result},
		}
	}
	h := []dealer.Mensagem{{Role: "user", Conteudo: "request"}}
	h = append(h, u("u1", strings.Repeat("a", 900))...)
	h = append(h, u("u2", tools.ImageAttached+"x.png\n")...)
	h = append(h, dealer.Mensagem{Role: "user", Conteudo: readImagesNote, Images: []string{"x.png"}})
	h = append(h, u("u3", strings.Repeat("c", 900))...)

	got := reduceTurn(h, historyBytes(h[1:3]))
	if got == nil {
		t.Fatal("reduction expected")
	}
	for _, m := range got {
		if m.Conteudo == readImagesNote {
			return // the note rides with the kept, recent work
		}
	}
	t.Fatalf("the image note must survive with the unit that follows it: %+v", got)
}

func TestCompactedStateIgnoresFailedCall(t *testing.T) {
	calls := []dealer.ToolChamada{{ID: "1", Nome: compactToolName, Args: `{"state":"good"}`}}
	if e := compactedState(calls, []dealer.Mensagem{{IsErro: true}}); e != "" {
		t.Fatalf("a failed call does not compact, got %q", e)
	}
	if e := compactedState(calls, []dealer.Mensagem{{}}); e != "good" {
		t.Fatalf("state = %q", e)
	}
	// The legacy "estado" argument is still accepted.
	legacy := []dealer.ToolChamada{{ID: "1", Nome: compactToolName, Args: `{"estado":"old"}`}}
	if e := compactedState(legacy, []dealer.Mensagem{{}}); e != "old" {
		t.Fatalf("legacy state = %q", e)
	}
}

func TestCompactSpecValidates(t *testing.T) {
	sp := compactSpec()
	if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"state":"  "}`)); err == nil {
		t.Fatal("empty state should fail")
	}
	if _, err := sp.Exec(context.Background(), "", json.RawMessage(`{"state":"`+strings.Repeat("x", maxSummary+1)+`"}`)); err == nil {
		t.Fatal("state above the limit should fail")
	}
}

// The policy decides whether the model sees context_compact.
func TestPolicyControlsCatalog(t *testing.T) {
	has := func(p string) bool {
		s := NewSession(config.Config{PoliticaContexto: p})
		for _, sp := range s.toolCatalog(t.TempDir()) {
			if sp.Name == compactToolName {
				return true
			}
		}
		return false
	}
	if !has("") || !has("agente") || !has("agent") {
		t.Error("default/agent policy should expose context_compact")
	}
	if has("limiar") || has("LIMIAR") || has("threshold") || has("Threshold") {
		t.Error("threshold policy should not expose context_compact")
	}
}

// End to end: the model calls context_compact in the middle of the second
// request; the following request already goes with the compacted history,
// and the session ends with the state in place of the old conversation.
func TestSendAppliesAgentCompaction(t *testing.T) {
	var (
		mu    sync.Mutex
		calls int
		last  []map[string]any
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		calls++
		n := calls
		last = req.Messages
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) {
			b, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		if n == 2 { // second request, first round: compact
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "cmp1", "type": "function",
				"function": map[string]any{"name": "context_compact", "arguments": `{"state":"goal: parser bug; cause in parse.go:42"}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cfg := chatTestConfig(srv.URL, "fake", "k")
	s := NewSession(cfg)
	cwd := t.TempDir()
	if err := s.Send(context.Background(), nil, cwd, "explore the parser"); err != nil {
		t.Fatalf("request 1: %v", err)
	}
	if err := s.Send(context.Background(), nil, cwd, "now fix it"); err != nil {
		t.Fatalf("request 2: %v", err)
	}

	// The 3rd request (after the context_compact result) is already compacted:
	// system, state, current request, tool_call, tool_result.
	mu.Lock()
	msgs := last
	mu.Unlock()
	if len(msgs) != 5 {
		t.Fatalf("post-compaction request should have 5 messages, got %d: %v", len(msgs), msgs)
	}
	if c, _ := msgs[1]["content"].(string); !strings.Contains(c, "cause in parse.go:42") {
		t.Fatalf("state missing from the request: %v", msgs[1])
	}
	// The old request no longer travels as a message of its own; only the
	// harness's facts block (inside the state) remembers what was asked.
	for _, m := range msgs[2:] {
		if c, _ := m["content"].(string); strings.Contains(c, "explore the parser") {
			t.Fatalf("old request should have left the request: %v", msgs)
		}
	}
	if c, _ := msgs[1]["content"].(string); !strings.Contains(c, "user asked: explore the parser") {
		t.Fatalf("the facts block should keep the earlier request: %v", msgs[1])
	}
	h := s.History()
	if !strings.HasPrefix(h[0].Conteudo, summaryPrefix) || h[len(h)-1].Conteudo != "done" {
		t.Fatalf("unexpected final history: %+v", h)
	}
}

// Resuming a compacted session brings back the state, not the old
// conversation. Both the stored kind ("compactacao"/"estado") and the English
// one ("compaction"/"state") are re-applied.
func TestResumeReappliesCompaction(t *testing.T) {
	for _, tc := range []struct {
		kind, key string
	}{
		{"compactacao", "estado"},
		{"compaction", "state"},
	} {
		st := testStore(t)
		s := NewSessionWithStore(config.Config{}, st, "bh-cmp")
		s.record("user_message", map[string]any{"role": "user", "content": "old request"})
		s.record("assistant", map[string]any{"role": "assistant", "content": "old answer"})
		s.record("user_message", map[string]any{"role": "user", "content": "current request"})
		s.record(tc.kind, map[string]any{"politica": PoliticaAgente, tc.key: "goal: Y"})
		s.record("assistant", map[string]any{"role": "assistant", "content": "ok"})

		r := NewSessionWithStore(config.Config{}, st, "bh-cmp")
		if err := r.Resume(); err != nil {
			t.Fatal(err)
		}
		h := r.History()
		if len(h) != 3 || !strings.Contains(h[0].Conteudo, "goal: Y") || h[1].Conteudo != "current request" || h[2].Conteudo != "ok" {
			t.Fatalf("%s: resume should re-apply the compaction: %+v", tc.kind, h)
		}
	}
}

// A tool injected MID-turn (mcp_add, skill_create) must already appear in the
// next cycle. With the catalog built once per turn, the new tool only counted
// on the next request — proven against the real model on 2026-10-18.
func TestToolInjectedInTurnAppearsNextCycle(t *testing.T) {
	var (
		mu   sync.Mutex
		seen [][]string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var names []string
		for _, tl := range req.Tools {
			names = append(names, tl.Function.Name)
		}
		mu.Lock()
		seen = append(seen, names)
		n := len(seen)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) { b, _ := json.Marshal(p); _, _ = w.Write([]byte("data: " + string(b) + "\n\n")) }
		if n == 1 {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "connect_extra", "arguments": `{}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "ok"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	s.Injetar(tools.Spec{
		Name: "connect_extra", Schema: map[string]any{"type": "object"},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			s.Injetar(tools.Spec{Name: "new_tool", Schema: map[string]any{"type": "object"},
				Exec: func(context.Context, string, json.RawMessage) (string, error) { return "", nil }})
			return "connected", nil
		},
	})
	if err := s.Send(context.Background(), nil, t.TempDir(), "connect"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seen))
	}
	has := func(l []string) bool {
		for _, n := range l {
			if n == "new_tool" {
				return true
			}
		}
		return false
	}
	if has(seen[0]) || !has(seen[1]) {
		t.Fatalf("new_tool should show up in the 2nd cycle: %v", seen)
	}
}
