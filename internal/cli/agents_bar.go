// The agents bar: what is working in the background, listed under the footer
// like Claude Code's background list. "main" is the conversation on screen;
// clicking a sub-agent row opens its conversation, clicking a shell row opens
// its live output.
package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

const maxAgentRows = 8

const (
	rowMain  = ""
	rowAgent = "agent"
	rowShell = "shell"
)

type agentRow struct {
	kind, id, label, age string
}

func (t *screen) liveAgents() []subagent.Tarefa {
	if t.src == nil {
		return nil
	}
	var live []subagent.Tarefa
	for _, tk := range t.src.tasks() {
		if tk.TerminadaEm.IsZero() {
			live = append(live, tk)
		}
	}
	return live
}

func (t *screen) agentRowCount() int {
	n := len(t.liveAgents()) + len(t.src.shells())
	if n == 0 {
		return 0
	}
	return min(n+1, maxAgentRows, max(0, t.h/4))
}

func (t *screen) agentRows(limit int) []agentRow {
	live := t.liveAgents()
	shells := t.src.shells()
	if len(live)+len(shells) == 0 || limit <= 0 {
		return nil
	}
	rows := []agentRow{{kind: rowMain, label: "main"}}
	for _, tk := range live {
		rows = append(rows, agentRow{kind: rowAgent, id: tk.ID, label: tk.ID + "  " + oneLine(tk.Prompt, 70), age: taskDuration(&tk)})
	}
	for _, j := range shells {
		rows = append(rows, agentRow{kind: rowShell, id: j.ID, label: j.ID + "  " + oneLine(j.Command, 70),
			age: time.Since(j.Start).Round(time.Second).String()})
	}
	return rows[:min(len(rows), limit)]
}

func agentLine(r agentRow, hover bool, width int) string {
	mark, fg := "○", colorText
	if r.kind == rowMain {
		mark, fg = "●", fgAccent
	}
	if hover {
		fg = fgWarning
	}
	left := "  " + color(fg, mark+" "+r.label)
	if r.age == "" {
		return left
	}
	age := grayf(r.age)
	gap := max(1, width-visWidth(left)-visWidth(age)-1)
	return left + strings.Repeat(" ", gap) + age
}

func (t *screen) drawAgents(sb *strings.Builder, f frame) {
	for i, r := range t.agentRows(f.agentN) {
		row := f.agentRow + i
		plain, hover := agentLine(r, false, f.boxWidth), agentLine(r, true, f.boxWidth)
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", row, bgScreen, truncateVis(plain, f.boxWidth), escClearLine)
		region := &hitRegion{row: row, col: 1, width: f.boxWidth, height: 1, id: "agent:" + r.kind + r.id, plain: plain, hover: hover}
		id := r.id
		switch r.kind {
		case rowAgent:
			region.action = func() { t.openSubagentDialog(id) }
		case rowShell:
			region.action = func() { t.openShellDialog(id) }
		}
		t.hits.add(region)
	}
}
