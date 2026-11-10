package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// manualTask registers a task by hand, with a live (never used) session, so the
// edge states of Mensagem/Cancelar can be reached deterministically.
func manualTask(g *Gerente, id, state string) *Tarefa {
	t := &Tarefa{ID: id, Prompt: "p", Estado: state, Criada: time.Now(), sess: runtime.NewSession(config.Config{})}
	if state == Completed || state == Failed || state == Cancelled {
		t.TerminadaEm = time.Now()
	}
	g.mu.Lock()
	g.tasks[id] = t
	g.order = append(g.order, id)
	g.mu.Unlock()
	return t
}

func TestMensagemRefusesQueuedTask(t *testing.T) {
	g := Novo(Config{Nome: "task"})
	manualTask(g, "t1", Queued)
	if err := g.Mensagem("t1", "hello"); err == nil || !strings.Contains(err.Error(), "not started") {
		t.Fatalf("a queued task should refuse, got %v", err)
	}
}

func TestMensagemRefusesWhenTheSessionIsClosing(t *testing.T) {
	g := Novo(Config{Nome: "task"})
	ctx, cancel := context.WithCancel(context.Background())
	g.Vincular(ctx)
	manualTask(g, "t1", Completed)
	cancel()
	if err := g.Mensagem("t1", "hello"); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("a closing session should refuse to resume, got %v", err)
	}
	if cp, _ := g.snapshot("t1"); cp.Estado != Completed {
		t.Fatalf("a refused resume must not change the task, state %s", cp.Estado)
	}
}

// A running task receives the message as a steer and records it exactly once,
// and the activity host hears about it.
func TestMensagemToRunningTaskNotifiesTheHostOnce(t *testing.T) {
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	var got []Evento
	g.AoAtividade(func(_ string, e Evento) { got = append(got, e) })
	manualTask(g, "t1", Running)

	if err := g.Mensagem("t1", "  steer me  "); err != nil {
		t.Fatalf("message failed: %v", err)
	}
	if len(got) != 1 || got[0].Tipo != EventoUsuario || got[0].Texto != "steer me" {
		t.Fatalf("host should receive one trimmed user event, got %+v", got)
	}
	cp, _ := g.snapshot("t1")
	if conv := cp.Conversa(); len(conv) != 1 || conv[0].Tipo != EventoUsuario {
		t.Fatalf("conversation = %+v", conv)
	}
}

// A panic in the worker becomes a task failure, releases the slot and still
// notifies the parent.
func TestWorkerExitTurnsAPanicIntoAFailureAndNotifies(t *testing.T) {
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	notified := make(chan struct{}, 1)
	g.AoTerminar(func() { notified <- struct{}{} })
	tk := manualTask(g, "t1", Running)
	g.mu.Lock()
	g.running = 1
	g.mu.Unlock()

	g.workerExit(tk, "boom")

	cp, _ := g.snapshot("t1")
	if cp.Estado != Failed || !strings.Contains(cp.Erro, "panic: boom") {
		t.Fatalf("state %s error %q", cp.Estado, cp.Erro)
	}
	if g.Ativas() != 0 {
		t.Fatalf("a failed task must not stay active: %d", g.Ativas())
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("the parent was not notified")
	}
	g.mu.Lock()
	running := g.running
	g.mu.Unlock()
	if running != 0 {
		t.Fatalf("running slot not released: %d", running)
	}

	// A second exit without a panic never takes the counter below zero.
	g.workerExit(tk, nil)
	g.mu.Lock()
	running = g.running
	g.mu.Unlock()
	if running != 0 {
		t.Fatalf("running went negative: %d", running)
	}
}

func TestCancelarCoversEveryState(t *testing.T) {
	g := Novo(Config{Nome: "task"})
	g.Vincular(context.Background())
	notified := make(chan struct{}, 4)
	g.AoTerminar(func() { notified <- struct{}{} })

	if g.Cancelar("nope") {
		t.Fatal("unknown task cannot be cancelled")
	}
	manualTask(g, "t1", Completed)
	if g.Cancelar("t1") {
		t.Fatal("a finished task has nothing to cancel")
	}
	manualTask(g, "t2", Running) // no cancel func: refuses
	if g.Cancelar("t2") {
		t.Fatal("a running task without cancel func must refuse")
	}
	manualTask(g, "t3", Queued)
	if !g.Cancelar("t3") {
		t.Fatal("a queued task should cancel at once")
	}
	if cp, _ := g.snapshot("t3"); cp.Estado != Cancelled || cp.Erro != "cancelled before starting" {
		t.Fatalf("t3 = %s %q", cp.Estado, cp.Erro)
	}
	select {
	case <-notified:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling a queued task must notify the parent")
	}
}

func TestOneLineCollapsesAndTruncates(t *testing.T) {
	if oneLine(nil) != "" {
		t.Fatal("nil error is empty")
	}
	if got := oneLine(errors.New("a\n  b\t c")); got != "a b c" {
		t.Fatalf("oneLine = %q", got)
	}
	got := oneLine(errors.New(strings.Repeat("x", 500)))
	if !strings.HasSuffix(got, "…") || len(got) > 310 {
		t.Fatalf("long error not truncated: len %d", len(got))
	}
}
