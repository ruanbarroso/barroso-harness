package acp

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// recTurn records every prompt it runs and answers with a fixed text; when
// gate is set, the first turn blocks until it is closed.
type recTurn struct {
	mu      sync.Mutex
	prompts []string
	gate    chan struct{}
	first   sync.Once
	started chan struct{}
}

func (r *recTurn) Send(ctx context.Context, rend runtime.Renderer, _, prompt string) error {
	r.mu.Lock()
	r.prompts = append(r.prompts, prompt)
	n := len(r.prompts)
	r.mu.Unlock()
	if n == 1 && r.gate != nil {
		r.first.Do(func() { close(r.started) })
		select {
		case <-r.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	rend.Resposta("ok " + strings.SplitN(prompt, "\n", 2)[0])
	return nil
}

func (r *recTurn) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.prompts...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

const notice = "[subagents] Sub-agents finished:\n\n## t1 completed (2s)\nresultado A\n\nNo sub-agents running now.\n"

func onceFetch(text string) func() (string, bool) {
	var mu sync.Mutex
	done := false
	return func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if done {
			return "", false
		}
		done = true
		return text, true
	}
}

func TestInitializeAdvertisesSubagentNoticesOnlyWhenEnabled(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 1, map[string]any{"protocolVersion": 1})
	waitLines(t, h.out, 1)
	if strings.Contains(h.out.String(), SubagentNoticesCap) {
		t.Fatalf("capability advertised without being enabled: %s", h.out.String())
	}

	h2 := setup(t, newFakeTurn())
	h2.a.HabilitarAvisosSubagentes()
	h2.send(t, MetInitialize, 1, map[string]any{"protocolVersion": 1})
	waitLines(t, h2.out, 1)
	if !strings.Contains(h2.out.String(), `"_meta":{"`+SubagentNoticesCap+`":true}`) {
		t.Fatalf("capability missing: %s", h2.out.String())
	}
}

// The regression: in -acp the sub-agent results were lost. Now a notice runs
// as a turn of the parent, streamed to the client and closed by turnEnd.
func TestSubagentNoticeRunsParentTurnAndEndsWithTurnEnd(t *testing.T) {
	turn := &recTurn{}
	h := setup(t, turn)
	h.a.HabilitarAvisosSubagentes()
	h.send(t, MetSessionNovo, 1, map[string]any{"cwd": t.TempDir()})
	waitLines(t, h.out, 1)

	h.a.AvisarSubagentes("s-deadbeef", onceFetch(notice))
	waitFor(t, func() bool { return strings.Contains(h.out.String(), TurnEndMeta) })

	if got := turn.got(); len(got) != 1 || got[0] != notice {
		t.Fatalf("parent turn prompt = %q", got)
	}
	out := h.out.String()
	iUser := strings.Index(out, `"sessionUpdate":"user_message_chunk"`)
	iAns := strings.Index(out, "ok [subagents]")
	iEnd := strings.Index(out, TurnEndMeta)
	if iUser < 0 || iAns < 0 || iUser > iAns || iAns > iEnd {
		t.Fatalf("expected user_message_chunk, answer, turnEnd in order:\n%s", out)
	}
	if !strings.Contains(out, `"`+TurnEndMeta+`":{"state":"completed"}`) {
		t.Fatalf("turnEnd state: %s", out)
	}
	// A second wake-up with nothing new runs no turn.
	h.a.AvisarSubagentes("s-deadbeef", onceFetch(""))
	time.Sleep(50 * time.Millisecond)
	if n := len(turn.got()); n != 1 {
		t.Fatalf("empty notice ran a turn (%d)", n)
	}
}

// A notice that arrives during a client turn waits for it to end.
func TestSubagentNoticeWaitsForClientTurn(t *testing.T) {
	turn := &recTurn{gate: make(chan struct{}), started: make(chan struct{})}
	h := setup(t, turn)
	h.a.HabilitarAvisosSubagentes()
	h.send(t, MetSessionNovo, 1, map[string]any{"cwd": t.TempDir()})
	waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 2, map[string]any{"sessionId": "s-deadbeef", "prompt": []map[string]any{{"type": "text", "text": "pedido"}}})
	<-turn.started

	h.a.AvisarSubagentes("s-deadbeef", onceFetch(notice))
	time.Sleep(50 * time.Millisecond)
	if n := len(turn.got()); n != 1 {
		t.Fatalf("notice overlapped the client turn (%d turns)", n)
	}
	close(turn.gate)
	waitFor(t, func() bool { return strings.Contains(h.out.String(), TurnEndMeta) })
	got := turn.got()
	if len(got) != 2 || got[0] != "pedido" || got[1] != notice {
		t.Fatalf("turn order = %q", got)
	}
}

// A client prompt that arrives during a notice turn is queued, not refused.
func TestClientPromptDuringNoticeTurnIsQueued(t *testing.T) {
	turn := &recTurn{gate: make(chan struct{}), started: make(chan struct{})}
	h := setup(t, turn)
	h.a.HabilitarAvisosSubagentes()
	h.send(t, MetSessionNovo, 1, map[string]any{"cwd": t.TempDir()})
	waitLines(t, h.out, 1)
	h.a.AvisarSubagentes("s-deadbeef", onceFetch(notice))
	<-turn.started

	h.send(t, MetPrompt, 2, map[string]any{"sessionId": "s-deadbeef", "prompt": []map[string]any{{"type": "text", "text": "depois"}}})
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(h.out.String(), `"code":-32001`) {
		t.Fatalf("client prompt refused during notice turn: %s", h.out.String())
	}
	close(turn.gate)
	waitFor(t, func() bool { return len(turn.got()) == 2 && strings.Contains(h.out.String(), `"id":2,"result"`) })
	if got := turn.got(); got[0] != notice || got[1] != "depois" {
		t.Fatalf("turn order = %q", got)
	}
}
