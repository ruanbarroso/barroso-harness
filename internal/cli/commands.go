// One command registry for /help, the command palette and the "/"
// autocomplete. Every entry is a slash command the REPL's own switch handles,
// so anything a dialog shows runs the same code as typing it; the three views
// share this list instead of keeping separate copies that drift apart.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// command is one entry of the registry.
type command struct {
	name  string // "/model"
	usage string // "/model [id]" as /help shows it
	cat   string // palette group: "Session", "Model", "Agents", "System"
	desc  string
	key   string // shortcut shown at the right of the palette row
	// dialog marks commands that, with no argument, open a full-screen dialog
	// instead of printing into the transcript.
	dialog bool
}

// commands is the registry, in /help order.
func commands() []command {
	return []command{
		{name: "/model", usage: "/model [id]", cat: "Model", desc: "list the catalog and switch model", key: "ctrl+x m", dialog: true},
		{name: "/effort", usage: "/effort [level|-]", cat: "Model", desc: "show or set the reasoning effort", key: "ctrl+t", dialog: true},
		{name: "/provider", usage: "/provider [zen]", cat: "Model", desc: "set a custom provider, or restore public OpenCode Zen and remove custom providers"},
		{name: "/new", usage: "/new", cat: "Session", desc: "start a new session (the current one stays in /resume) on the home screen", key: "ctrl+x n"},
		{name: "/resume", usage: "/resume", cat: "Session", desc: "list recent sessions and resume one", key: "ctrl+x l", dialog: true},
		{name: "/save", usage: "/save [title]", cat: "Session", desc: "title the current session and save the log"},
		{name: "/rename", usage: "/rename <name>", cat: "Session", desc: "rename the current session (tab and panel)", key: "ctrl+r", dialog: true},
		{name: "/context", usage: "/context", cat: "Session", desc: "active compaction policy and what it has done", dialog: true},
		{name: "/skills", usage: "/skills", cat: "Agents", desc: "list skills; /<name> [args] runs one", dialog: true},
		{name: "/tasks", usage: "/tasks [id|cancel id]", cat: "Agents", desc: "list sub-agents, show one, cancel one", dialog: true},
		{name: "/mcps", usage: "/mcps", cat: "Agents", desc: "MCP servers and their status", dialog: true},
		{name: "/update", usage: "/update", cat: "System", desc: "download and install the latest release", dialog: true},
		{name: "/help", usage: "/help", cat: "System", desc: "this help", dialog: true},
		{name: "/exit", usage: "/exit, /quit", cat: "System", desc: "exit", key: "ctrl+x q"},
	}
}

// printCommands writes the registry as /help's command list.
func printCommands(out io.Writer) {
	for _, c := range commands() {
		fmt.Fprintf(out, "  %-22s %s\n", c.usage, c.desc)
	}
}

// completionEntry is one row of the "/" autocomplete.
type completionEntry struct {
	text string // inserted text, e.g. "/model"
	desc string
}

// noArgCommand reports whether name is a registered command whose usage takes
// no argument ("/new", "/help"); skills always may take one.
func noArgCommand(name string) bool {
	for _, c := range commands() {
		if c.name == name {
			return !strings.ContainsAny(c.usage, "[<")
		}
	}
	return false
}

// slashCompletions lists the commands and skills whose name starts with the
// typed prefix ("/" alone lists everything), commands first.
func slashCompletions(cwd, prefix string) []completionEntry {
	var all []completionEntry
	for _, c := range commands() {
		all = append(all, completionEntry{text: c.name, desc: c.desc})
	}
	for _, s := range runtime.SkillsDisponiveis(cwd) {
		all = append(all, completionEntry{text: "/" + s.Nome, desc: oneLine(s.Descricao, 60)})
	}
	idx := fuzzyRank(len(all), strings.TrimPrefix(prefix, "/"), func(i int) string {
		return strings.TrimPrefix(all[i].text, "/")
	})
	out := make([]completionEntry, len(idx))
	for i, j := range idx {
		out[i] = all[j]
	}
	return out
}
