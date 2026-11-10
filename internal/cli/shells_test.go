package cli

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

func longCommand() string {
	if runtime.GOOS == "windows" {
		return "echo shell-started & ping -n 30 127.0.0.1 >nul"
	}
	return "echo shell-started; sleep 30"
}

func startBackgroundShell(t *testing.T) (*tools.JobSet, string) {
	t.Helper()
	js := tools.NewJobSet()
	dir := t.TempDir()
	t.Cleanup(tools.KillJobs)
	for _, spec := range tools.SpecsBuiltins() {
		if spec.Name != "shell" {
			continue
		}
		args, _ := json.Marshal(map[string]any{"command": longCommand(), "background": true})
		out, err := spec.Exec(tools.WithJobSet(context.Background(), js), dir, args)
		if err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if jobs := js.Visao(); len(jobs) == 1 {
				if o, _ := js.Saida(jobs[0].ID); strings.Contains(o, "shell-started") {
					return js, jobs[0].ID
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("background shell did not start: %s", out)
	}
	t.Fatal("shell tool not found")
	return nil, ""
}

func TestAgentsBarListsBackgroundShellsAndOpensTheirOutput(t *testing.T) {
	noColors(t)
	js, id := startBackgroundShell(t)

	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.Adicionar("hello")
	sc.src = &dialogSource{jobs: js}

	f := sc.frameFor()
	if f.agentN != 2 {
		t.Fatalf("the bar should reserve main plus one shell row, got %d", f.agentN)
	}
	if left, _ := sc.footerParts(100); !strings.Contains(left, "1 shell") {
		t.Fatalf("the footer should count the shell: %q", left)
	}

	sc.drawLocked()
	region := sc.hits.at(f.agentRow+1, 5)
	if region == nil || region.action == nil {
		t.Fatalf("the shell row should answer a click: %+v", region)
	}
	region.action()
	if sc.dlg == nil || sc.dlg.jobID != id {
		t.Fatalf("clicking the row should open the output of %s: %+v", id, sc.dlg)
	}
	if body := strings.Join(sc.dlg.text, "\n"); !strings.Contains(body, "shell-started") {
		t.Fatalf("the window should show the output:\n%s", body)
	}
}

func TestShellWindowStopsTheJobWithX(t *testing.T) {
	noColors(t)
	js, id := startBackgroundShell(t)

	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.Adicionar("hello")
	sc.src = &dialogSource{jobs: js}
	sc.openShellDialog(id)

	sc.dialogKey(keyEvent{text: "x"})

	if n := len(js.Visao()); n != 0 {
		t.Fatalf("x should stop the job, %d still listed", n)
	}
	if n := sc.frameFor().agentN; n != 0 {
		t.Fatalf("with nothing running the bar should reserve no rows, got %d", n)
	}
}
