package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// usageProvider asks for the given tools (none = a plain answer) in its first
// round, then answers text. Unlike twoCallProvider it reports usage, which is
// what makes the harness write the "round" event these tests read.
func usageProvider(toolNames ...string) *httptest.Server {
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
		if first && len(toolNames) > 0 {
			var calls []map[string]any
			for i, name := range toolNames {
				calls = append(calls, map[string]any{"index": i, "id": fmt.Sprintf("c%d", i), "type": "function",
					"function": map[string]any{"name": name, "arguments": `{}`}})
			}
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": calls}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "done"}}}})
		}
		emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}}})
		emit(map[string]any{"choices": []map[string]any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 1}})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// roundEvents returns the payloads of every "round" event of a session, in
// order.
func roundEvents(t *testing.T, s *Session, id string) []map[string]any {
	t.Helper()
	events, err := s.store.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, e := range events {
		if e.Kind != "round" {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			t.Fatalf("round payload: %v: %s", err, e.Payload)
		}
		out = append(out, p)
	}
	return out
}

// A panel shows the gap between two events as "thinking". The round event now
// splits that gap: duration_ms (provider), prep_ms (harness, before the
// request left) and tools_ms (the previous cycle's tools). A slow tool must
// land in tools_ms of the NEXT round, never in prep_ms or duration_ms.
func TestRoundEventSplitsToolTimeFromProviderAndPrep(t *testing.T) {
	srv := usageProvider("slow_a")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101110000-0001"
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	const toolTime = 700 * time.Millisecond
	s.Injetar(slowTool("slow_a", toolTime))

	if err := s.Send(context.Background(), nil, t.TempDir(), "run it"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	rounds := roundEvents(t, s, id)
	if len(rounds) != 2 {
		t.Fatalf("rounds = %d, expected 2 (tool call + answer): %+v", len(rounds), rounds)
	}
	for i, r := range rounds {
		for _, k := range []string{"duration_ms", "prep_ms", "tools_ms"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("round %d without %s: %+v", i, k, r)
			}
		}
	}
	// First cycle: no tool ran before it.
	if got := rounds[0]["tools_ms"].(float64); got != 0 {
		t.Errorf("round 0 tools_ms = %v, expected 0 (no previous tools)", got)
	}
	// Second cycle: carries the tool wall time.
	tools := time.Duration(rounds[1]["tools_ms"].(float64)) * time.Millisecond
	if tools < toolTime-50*time.Millisecond || tools > toolTime+2*time.Second {
		t.Errorf("round 1 tools_ms = %v, expected ~%v", tools, toolTime)
	}
	// The tool time must not leak into the provider or prep numbers.
	for i, r := range rounds {
		if d := time.Duration(r["duration_ms"].(float64)) * time.Millisecond; d >= toolTime {
			t.Errorf("round %d duration_ms = %v includes the tool time", i, d)
		}
		if p := time.Duration(r["prep_ms"].(float64)) * time.Millisecond; p >= toolTime {
			t.Errorf("round %d prep_ms = %v includes the tool time", i, p)
		}
	}
}

// Two parallel slow tools: the wall time of the cycle stays near ONE tool,
// the sum of each tool's own time is near TWO. That is what tells a panel
// "two things ran at once" apart from "one thing took that long".
func TestRoundEventToolsSumMsWithParallelCalls(t *testing.T) {
	srv := usageProvider("slow_a", "slow_b")
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101110000-0003"
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	const d = 400 * time.Millisecond
	s.Injetar(slowTool("slow_a", d))
	s.Injetar(slowTool("slow_b", d))

	if err := s.Send(context.Background(), nil, t.TempDir(), "run both"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	rounds := roundEvents(t, s, id)
	if len(rounds) != 2 {
		t.Fatalf("rounds = %d, expected 2", len(rounds))
	}
	r := rounds[1]
	wall := time.Duration(r["tools_ms"].(float64)) * time.Millisecond
	sum := time.Duration(r["tools_sum_ms"].(float64)) * time.Millisecond
	if wall < d-50*time.Millisecond || wall > d+2*time.Second {
		t.Errorf("tools_ms = %v, expected ~%v (one wall)", wall, d)
	}
	if sum < 2*d-80*time.Millisecond || sum > 2*d+2*time.Second {
		t.Errorf("tools_sum_ms = %v, expected ~%v (two calls)", sum, 2*d)
	}
	if sum <= wall {
		t.Errorf("tools_sum_ms (%v) should exceed tools_ms (%v) for parallel calls", sum, wall)
	}
}

// A turn without tools: prep_ms is small and non-negative (it counts from the
// start of the turn) and there is no previous tool time.
func TestRoundEventPrepMsIsNonNegative(t *testing.T) {
	srv := usageProvider()
	defer srv.Close()
	st := testStore(t)
	const id = "bh-20240101110000-0002"
	s := NewSessionWithStore(fakeConfig(t, srv.URL), st, id)
	if err := s.Send(context.Background(), nil, t.TempDir(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	rounds := roundEvents(t, s, id)
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, expected 1: %+v", len(rounds), rounds)
	}
	if p := rounds[0]["prep_ms"].(float64); p < 0 || p > 5000 {
		t.Errorf("prep_ms = %v, expected a small non-negative value", p)
	}
	if got := rounds[0]["tools_ms"].(float64); got != 0 {
		t.Errorf("tools_ms = %v, expected 0 on a turn without tools", got)
	}
}
