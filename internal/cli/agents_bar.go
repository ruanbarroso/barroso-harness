// The agents bar: the sub-agents that are still working, listed under the
// footer like Claude Code's agent list. "main" is the conversation on screen;
// clicking a sub-agent row opens its conversation.
package cli

import (
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

const maxAgentRows = 5

type agentRow struct {
	id, label string
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
	n := len(t.liveAgents())
	if n == 0 {
		return 0
	}
	return min(n+1, maxAgentRows, max(0, t.h/4))
}

func (t *screen) agentRows(limit int) []agentRow {
	live := t.liveAgents()
	if len(live) == 0 || limit <= 0 {
		return nil
	}
	rows := []agentRow{{label: "main"}}
	for _, tk := range live {
		if len(rows) >= limit {
			break
		}
		rows = append(rows, agentRow{id: tk.ID, label: tk.ID + "  " + oneLine(tk.Prompt, 70)})
	}
	return rows[:min(len(rows), limit)]
}

func agentLine(r agentRow, hover bool) string {
	mark, fg := "○", colorText
	if r.id == "" {
		mark, fg = "●", fgAccent
	}
	if hover {
		fg = fgWarning
	}
	return "  " + color(fg, mark+" "+r.label)
}

func (t *screen) drawAgents(sb *strings.Builder, f frame) {
	for i, r := range t.agentRows(f.agentN) {
		row := f.agentRow + i
		plain, hover := agentLine(r, false), agentLine(r, true)
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", row, bgScreen, truncateVis(plain, f.boxWidth), escClearLine)
		region := &hitRegion{row: row, col: 1, width: f.boxWidth, height: 1, id: "agent:" + r.id, plain: plain, hover: hover}
		if r.id != "" {
			id := r.id
			region.action = func() { t.openSubagentDialog(id) }
		}
		t.hits.add(region)
	}
}
