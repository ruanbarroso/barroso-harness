package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// First status after a launch is inside the window: short list plus an order
// to end the turn. After the window it is the full report. Never blocks.
func TestStatusRateLimitedAfterLaunch(t *testing.T) {
	_, cfg := newServer(t, "ok", 3*time.Second, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	now := time.Now()
	g.now = func() time.Time { return now }
	if _, err := g.Spawn(context.Background(), "slow"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Second)
	out := g.statusReport()
	if !strings.Contains(out, "t1 running") || !strings.Contains(out, "Do NOT call it again") || strings.Contains(out, " | ") {
		t.Fatalf("status inside the window should be short:\n%s", out)
	}

	now = now.Add(StatusIntervaloMinimo)
	full := g.statusReport()
	if !strings.Contains(full, " | ") || !strings.Contains(full, "END THE TURN") {
		t.Fatalf("status after the window should be full:\n%s", full)
	}
	// Right after a full report the window restarts.
	now = now.Add(time.Second)
	if again := g.statusReport(); !strings.Contains(again, "Do NOT call it again") {
		t.Fatalf("status right after a full report should be short:\n%s", again)
	}
	g.Cancelar("t1")
}

// With nothing running the status is never throttled.
func TestStatusNotThrottledWhenIdle(t *testing.T) {
	_, cfg := newServer(t, "done", 0, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	id, _ := g.Spawn(context.Background(), "quick")
	waitState(t, g, id, Completed)
	out := g.statusReport()
	if strings.Contains(out, "Do NOT call it again") || !strings.Contains(out, "finished: done") {
		t.Fatalf("unexpected status:\n%s", out)
	}
}

type logged struct {
	kind    string
	payload map[string]any
}

func toRegistros(ls []logged) []Registro {
	var out []Registro
	for _, l := range ls {
		b, _ := json.Marshal(l.payload)
		out = append(out, Registro{Kind: l.kind, Payload: string(b)})
	}
	return out
}

// A bh process that died with a sub-agent running: the log has the launch and
// no end. Restaurar turns it into failed/interrupted, delivered once.
func TestRestaurarInterruptedAndUndelivered(t *testing.T) {
	_, cfg := newServer(t, "x", 5*time.Second, false)
	g1 := Novo(Config{Nome: "task"})
	g1.Vincular(context.Background())
	g1.ConfigPai(cfg)
	var log []logged
	g1.Persistir(func(k string, p map[string]any) { log = append(log, logged{k, p}) })
	// Hand-written log: t1 running (died), t2 completed and never delivered,
	// t3 completed and delivered.
	log = append(log,
		logged{EventoTarefa, map[string]any{"id": "t1", "state": Running, "prompt": "p1"}},
		logged{EventoTarefa, map[string]any{"id": "t2", "state": Running, "prompt": "p2"}},
		logged{EventoTarefa, map[string]any{"id": "t2", "state": Completed, "result": "R2"}},
		logged{EventoTarefa, map[string]any{"id": "t3", "state": Running, "prompt": "p3"}},
		logged{EventoTarefa, map[string]any{"id": "t3", "state": Completed, "result": "R3"}},
		logged{EventoEntregue, map[string]any{"ids": "t3"}},
	)

	g2 := Novo(Config{Nome: "task"})
	if n := g2.Restaurar(toRegistros(log)); n != 2 {
		t.Fatalf("restored = %d, want 2", n)
	}
	if g2.Pendentes() != 2 {
		t.Fatalf("pending = %d", g2.Pendentes())
	}
	ts := g2.Terminadas()
	if len(ts) != 2 || ts[0].ID != "t1" || ts[0].Estado != Failed || !strings.Contains(ts[0].Erro, "interrupted") {
		t.Fatalf("t1 should be failed/interrupted: %+v", ts)
	}
	if ts[1].ID != "t2" || ts[1].Estado != Completed || ts[1].Resultado != "R2" {
		t.Fatalf("t2 should keep its result: %+v", ts[1])
	}
	msg := AvisoTerminadas(ts, g2.Ativas())
	if !strings.Contains(msg, "## t1 failed") || !strings.Contains(msg, "interrupted") || !strings.Contains(msg, "R2") {
		t.Fatalf("notice:\n%s", msg)
	}
	if len(g2.Terminadas()) != 0 || g2.Pendentes() != 0 {
		t.Fatal("must not deliver twice")
	}
	// New ids do not collide with restored ones.
	if g2.seq != 3 {
		t.Fatalf("seq = %d, want 3", g2.seq)
	}
}

// Lifecycle is recorded end to end: launch, end, delivery.
func TestLifecycleIsRecorded(t *testing.T) {
	_, cfg := newServer(t, "fine", 0, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	var ev []logged
	done := make(chan struct{}, 1)
	g.Persistir(func(k string, p map[string]any) { ev = append(ev, logged{k, p}) })
	g.AoTerminar(func() {
		select {
		case done <- struct{}{}:
		default:
		}
	})
	id, _ := g.Spawn(context.Background(), "do it")
	<-done
	g.Terminadas()
	g.mu.Lock()
	defer g.mu.Unlock()
	var kinds []string
	for _, e := range ev {
		kinds = append(kinds, e.kind+":"+toStr(e.payload["state"]))
	}
	want := []string{EventoTarefa + ":running", EventoTarefa + ":completed", EventoEntregue + ":"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v (id %s)", kinds, want, id)
	}
	if ev[0].payload["prompt"] != "do it" || ev[1].payload["result"] != "fine" {
		t.Fatalf("payloads: %+v", ev)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
