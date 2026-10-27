// Sub-agent inspector body: the conversation of one task as plain text lines,
// in the order it happened (prompt, thoughts, notes, tool calls with their
// output, final answer). The dialog shows it read-only, like OpenCode's
// subagent inspector, and rebuilds it while the task runs.
package cli

import (
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// maxToolLines bounds how much of one tool's output the inspector shows.
const maxToolLines = 12

func subagentConversationLines(task subagent.Tarefa, width int) []string {
	var out []string
	add := func(prefix, text string) {
		for _, l := range strings.Split(wrap(cleanOutput(text), max(20, width-len(prefix))), "\n") {
			out = append(out, prefix+l)
		}
	}
	out = append(out, "Prompt")
	add("  ", task.Prompt)
	events := task.Conversa()
	if len(events) == 0 && task.Estado == subagent.Running {
		out = append(out, "", "No subagent activity yet")
	}
	for _, e := range events {
		out = append(out, "")
		switch e.Tipo {
		case subagent.EventoPensamento:
			head := "Thought"
			if e.Duracao > 0 {
				head += " · " + e.Duracao.Round(100*time.Millisecond).String()
			}
			out = append(out, head)
			add("  ", e.Texto)
		case subagent.EventoNota, subagent.EventoResposta:
			add("", e.Texto)
		case subagent.EventoToolCall:
			_, label := toolLabel(e.Nome, e.Args, e.Texto, e.Erro)
			switch {
			case e.Executando:
				label = "⠋ " + label
			case e.Erro:
				label = "✗ " + label
			default:
				label = "→ " + label
			}
			if e.Duracao > 0 {
				label += " · " + e.Duracao.Round(10*time.Millisecond).String()
			}
			out = append(out, label)
			if body := strings.TrimRight(cleanOutput(e.Texto), "\n "); body != "" {
				lines := strings.Split(body, "\n")
				if len(lines) > maxToolLines {
					lines = append(lines[len(lines)-maxToolLines:], "")
					lines[len(lines)-1] = "… (showing the last " + itoa(maxToolLines) + " lines)"
				}
				for _, l := range lines {
					out = append(out, "    "+truncateVis(l, max(10, width-4)))
				}
			}
		case subagent.EventoSistema:
			out = append(out, "! "+e.Texto)
		}
	}
	if task.Erro != "" {
		out = append(out, "", "error: "+task.Erro)
	}
	return out
}
