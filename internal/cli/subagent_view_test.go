package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

func TestSubagentConversationLines(t *testing.T) {
	noColors(t)
	task := subagent.Tarefa{ID: "t1", Prompt: "investigate the parser", Estado: subagent.Running, Inicada: time.Now()}
	if got := strings.Join(subagentConversationLines(task, 80), "\n"); !strings.Contains(got, "No subagent activity yet") {
		t.Fatalf("a running task without activity should say so:\n%s", got)
	}
}

// The dialog opens on a task from the manager, shows its prompt and error,
// and tab cycles between tasks.
func TestSubagentDialogOpensAndCycles(t *testing.T) {
	noColors(t)
	g, id := managerWithTask(t)
	other, err := g.Spawn(t.Context(), "second task")
	if err != nil {
		t.Fatal(err)
	}
	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.src = &dialogSource{gerente: g}
	sc.openSubagentDialog(id)
	if sc.dlg == nil || sc.dlg.taskID != id {
		t.Fatalf("dialog = %+v", sc.dlg)
	}
	body := strings.Join(sc.dlg.text, "\n")
	if !strings.Contains(body, "investigate the date parser bug") {
		t.Fatalf("body missing the prompt:\n%s", body)
	}
	sc.dialogKey(keyEvent{action: "tab"})
	if sc.dlg.taskID != other {
		t.Fatalf("tab should move to %s, got %s", other, sc.dlg.taskID)
	}
	sc.dialogKey(keyEvent{action: "shift+tab"})
	if sc.dlg.taskID != id {
		t.Fatalf("shift+tab should come back to %s, got %s", id, sc.dlg.taskID)
	}
	sc.frame() // painting must not panic and must keep the dialog open
	if sc.dlg == nil {
		t.Fatal("frame closed the dialog")
	}
}
