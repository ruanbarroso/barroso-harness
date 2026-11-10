package tools

import (
	"strings"
	"testing"
	"time"
)

func TestJobSetVisaoListsRunningBackgroundJobs(t *testing.T) {
	shrinkJobs(t)
	ctx, js, _ := watch(t)
	runTerm(t, ctx, map[string]any{"command": sleepCmd(30), "background": true})

	view := js.Visao()
	if len(view) != 1 || view[0].Command != sleepCmd(30) || view[0].ID == "" {
		t.Fatalf("running background job should be listed: %+v", view)
	}
	if other := NewJobSet().Visao(); len(other) != 0 {
		t.Fatalf("a different set must not see the job: %+v", other)
	}
	js.Parar(view[0].ID)
}

func TestJobSetVisaoIgnoresForegroundAndFinishedJobs(t *testing.T) {
	shrinkJobs(t)
	ctx, js, n := watch(t)
	runTerm(t, ctx, map[string]any{"command": "echo hi"})
	runTerm(t, ctx, map[string]any{"command": "echo done", "background": true})
	waitCount(t, n, 1)

	if view := js.Visao(); len(view) != 0 {
		t.Fatalf("foreground and finished jobs must not be listed: %+v", view)
	}
}

func TestJobSetSaidaReadsOutputWithoutConsumingIt(t *testing.T) {
	shrinkJobs(t)
	ctx, js, n := watch(t)
	runTerm(t, ctx, map[string]any{"command": "echo visible-output", "background": true})
	waitCount(t, n, 1)
	id := ""
	for _, f := range js.Terminadas() {
		id = f.ID
	}

	out, ok := js.Saida(id)
	if !ok || !strings.Contains(out, "visible-output") {
		t.Fatalf("output = %q ok=%v", out, ok)
	}
	if _, ok := js.Saida("job-unknown"); ok {
		t.Fatal("unknown job should not be found")
	}
	if _, ok := NewJobSet().Saida(id); ok {
		t.Fatal("a different set must not read the output")
	}
}

func TestJobSetPararStopsTheJob(t *testing.T) {
	shrinkJobs(t)
	ctx, js, _ := watch(t)
	runTerm(t, ctx, map[string]any{"command": sleepCmd(30), "background": true})
	view := js.Visao()
	if len(view) != 1 {
		t.Fatalf("expected one running job: %+v", view)
	}

	if !js.Parar(view[0].ID) {
		t.Fatal("Parar should find the job")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(js.Visao()) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(js.Visao()) != 0 {
		t.Fatal("stopped job should leave the list")
	}
	if js.Parar("job-unknown") {
		t.Fatal("unknown job should not be stoppable")
	}
}
