package subagent

import (
	"context"
	"strings"
	"testing"
	"time"
)

func newManager(t *testing.T, text string, delay time.Duration) (*fakeProvider, *Gerente) {
	t.Helper()
	f, cfg := newServer(t, text, delay, false)
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	return f, g
}

func TestMensagemResumesFinishedTaskInTheSameSession(t *testing.T) {
	f, g := newManager(t, "ok", 0)
	id, err := g.Spawn(nil, "first request")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	waitState(t, g, id, Completed)

	if err := g.Mensagem(id, "second request"); err != nil {
		t.Fatalf("message failed: %v", err)
	}
	cp := waitState(t, g, id, Completed, Failed)
	if cp.Estado != Completed {
		t.Fatalf("resumed task should complete, state %s error %q", cp.Estado, cp.Erro)
	}

	views := f.Views()
	if len(views) != 2 {
		t.Fatalf("expected 2 provider calls, got %d", len(views))
	}
	if len(views[1].Messages) <= len(views[0].Messages) {
		t.Fatalf("second call should carry the first turn's history: %d then %d messages",
			len(views[0].Messages), len(views[1].Messages))
	}

	var users []string
	for _, e := range cp.Conversa() {
		if e.Tipo == EventoUsuario {
			users = append(users, e.Texto)
		}
	}
	if len(users) != 1 || users[0] != "second request" {
		t.Fatalf("conversation should record the user message once: %v", users)
	}
}

func TestMensagemRejectsEmptyAndUnknown(t *testing.T) {
	_, g := newManager(t, "ok", 0)
	if err := g.Mensagem("t9", "hello"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("unknown task should be refused, got %v", err)
	}
	if err := g.Mensagem("t9", "   "); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("blank message should be refused, got %v", err)
	}
}

func TestMensagemToRunningTaskIsDeliveredWithoutLosingIt(t *testing.T) {
	f, g := newManager(t, "ok", 150*time.Millisecond)
	id, err := g.Spawn(nil, "slow request")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	waitState(t, g, id, Running)

	if err := g.Mensagem(id, "while you work"); err != nil {
		t.Fatalf("message to a running task failed: %v", err)
	}
	cp := waitState(t, g, id, Completed, Failed)
	if cp.Estado != Completed {
		t.Fatalf("task should complete, state %s error %q", cp.Estado, cp.Erro)
	}

	seen := false
	for _, v := range f.Views() {
		if len(v.Messages) >= 3 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("the provider never saw the steered message: %+v", f.Views())
	}
}

func TestMensagemRefusesTaskWithoutLiveSession(t *testing.T) {
	_, g := newManager(t, "ok", 0)
	restored := g.Restaurar([]Registro{{Kind: EventoTarefa, Payload: `{"id":"t1","state":"completed","prompt":"old"}`}})
	if restored != 1 {
		t.Fatalf("expected one restored task, got %d", restored)
	}
	if err := g.Mensagem("t1", "hello"); err == nil || !strings.Contains(err.Error(), "no live session") {
		t.Fatalf("restored task has no session and should refuse, got %v", err)
	}
}

// The parent must hear the resumed task's NEW result. Terminadas skips tasks
// already delivered, so Mensagem has to reopen the delivery slot.
func TestMensagemReopensDeliveryForTheSecondResult(t *testing.T) {
	_, g := newManager(t, "ok", 0)
	id, err := g.Spawn(nil, "first request")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	waitState(t, g, id, Completed)

	// First delivery: the parent already heard the first answer.
	if ts := g.Terminadas(); len(ts) != 1 || ts[0].ID != id {
		t.Fatalf("first delivery = %+v", ts)
	}
	if len(g.Terminadas()) != 0 || g.Pendentes() != 0 {
		t.Fatal("the same task must not be delivered twice")
	}

	if err := g.Mensagem(id, "second request"); err != nil {
		t.Fatalf("message failed: %v", err)
	}
	cp := waitState(t, g, id, Completed, Failed)
	if cp.Estado != Completed {
		t.Fatalf("resumed task should complete, state %s error %q", cp.Estado, cp.Erro)
	}

	ts := g.Terminadas()
	if len(ts) != 1 || ts[0].ID != id {
		t.Fatalf("the second result must be delivered: %+v", ts)
	}
	if ts[0].Resultado == "" {
		t.Fatalf("the second result must carry the new answer: %+v", ts[0])
	}
	if again := g.Terminadas(); len(again) != 0 {
		t.Fatal("the second result must be delivered once")
	}
}

// Cancelling a resumed task stops it and records it as cancelled.
func TestMensagemResumedTaskCanBeCancelled(t *testing.T) {
	_, g := newManager(t, "ok", 400*time.Millisecond)
	id, err := g.Spawn(nil, "first request")
	if err != nil {
		t.Fatalf("spawn failed: %v", err)
	}
	waitState(t, g, id, Completed)
	if err := g.Mensagem(id, "slow follow-up"); err != nil {
		t.Fatalf("message failed: %v", err)
	}
	waitState(t, g, id, Running)
	if !g.Cancelar(id) {
		t.Fatal("a resumed running task must be cancellable")
	}
	cp := waitState(t, g, id, Cancelled, Failed)
	if cp.Estado != Cancelled {
		t.Fatalf("state = %s, want cancelled", cp.Estado)
	}
}
