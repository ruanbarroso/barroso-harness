// Transcript items: the parts of the conversation that OpenCode draws as
// interactive rows — the user's message box, a Thought, a tool call, a shell or
// edit block, a group of exploration tools, a finished sub-agent.
//
// An item lives in its own map, keyed by a stable id; t.lines only holds a
// marker line pointing at it. Rows are rendered from the item on every frame,
// so expanding one never moves any other item's identity (the old Thought
// expansion was keyed by line index and broke as soon as lines shifted), and
// box rows follow the terminal width after a resize.
package cli

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// itemMark starts a t.lines entry that stands for an item; the id follows.
const itemMark = "\x00item:"

// Item colours from the theme (spec §0, §2).
const (
	fgThoughtDim = "\033[38;2;151;104;44m" // warning at 60% over background.base
	fgBlockBar   = "\033[38;2;10;10;10m"   // the block's ┃ in background.base: an invisible gap
	fgThoughtBar = "\033[38;2;30;30;30m"
	bgDiffAdd    = "\033[48;2;32;48;59m"
	bgDiffDel    = "\033[48;2;55;34;44m"
	fgDiffAdd    = "\033[38;2;184;219;135m"
	fgDiffDel    = "\033[38;2;226;106;117m"
	bgChipFile   = "\033[48;2;157;124;216m"
	fgChipFile   = "\033[38;2;20;20;20m"
)

// collapsedRows is how many rows a shell or edit block shows before a click
// expands it (OpenCode's collapseShellOutput keeps 10).
const collapsedRows = 10

// item is one interactive part of the transcript.
type item struct {
	id   int
	kind string // "user", "thought", "tool", "shell", "block", "group", "sub"
	open bool
	// pending marks a steer message the model has not received yet: its box
	// bar is border.base instead of the primary colour (queue.go).
	pending bool

	text   string   // user: the message; thought: the reasoning body
	images []string // user: attached image paths
	title  string   // thought: the "**Title**" of the reasoning
	dur    time.Duration

	name, args, out string // tool calls
	failed          bool
	// running marks a call that started and has no result yet: it draws with a
	// spinner, and a shell block shows live (its output so far). spin is the
	// spinner frame, copied from the screen when painting.
	running bool
	live    string
	spin    int
	members []*item // group: the exploration tools it gathers

	taskID, state string // sub: finished sub-agent
}

// addItem registers it and appends its marker after a blank spacer row
// (OpenCode gives every part a marginTop of 1). Caller holds t.mu.
func (t *screen) addItem(it *item) {
	if t.items == nil {
		t.items = map[int]*item{}
	}
	t.nextItem++
	it.id = t.nextItem
	t.items[it.id] = it
	var add []string
	if n := len(t.lines); n > 0 && t.lines[n-1] != "" {
		add = append(add, "")
	}
	t.appendLocked(append(add, itemMark+strconv.Itoa(it.id))...)
}

// lastItem returns the item the transcript ends with, if it ends with one.
func (t *screen) lastItem() *item {
	if n := len(t.lines); n > 0 && strings.HasPrefix(t.lines[n-1], itemMark) {
		return t.itemOf(t.lines[n-1])
	}
	return nil
}

func (t *screen) itemOf(line string) *item {
	id, err := strconv.Atoi(strings.TrimPrefix(line, itemMark))
	if err != nil {
		return nil
	}
	return t.items[id]
}

// mutate changes items in place keeping the stay-put offset, like appendLocked
// does for new lines. Caller holds t.mu.
func (t *screen) mutate(f func()) {
	before := t.wrappedRows()
	f()
	if t.scroll > 0 {
		t.scroll = max(0, t.scroll+t.wrappedRows()-before)
	}
	t.drawLocked()
}

// toggle opens or closes an expandable item (the click action).
func (t *screen) toggle(it *item) { t.mutate(func() { it.open = !it.open }) }

// clickable reports whether an item answers a click.
func (t *screen) clickable(it *item) bool {
	switch it.kind {
	case "user":
		return !it.pending
	case "thought", "sub":
		return true
	case "group":
		// A lone member draws as itself, so it clicks as itself.
		if len(it.members) == 1 {
			return t.clickable(it.members[0])
		}
		return true
	case "tool":
		return it.failed || len(t.genericFields(it)) > 0 || it.out != ""
	case "shell", "block":
		return len(t.renderItem(it, 200, false, true)) > collapsedRows+2
	}
	return false
}

// itemAction is what a click on the item does.
func (t *screen) itemAction(it *item) func() {
	switch it.kind {
	case "user":
		return func() { t.openMessageDialog(it) }
	case "sub":
		return func() { t.pendingTaskID = it.taskID }
	case "group":
		if len(it.members) == 1 {
			return func() { t.toggle(it.members[0]) }
		}
	}
	return func() { t.toggle(it) }
}

// openMessageDialog is OpenCode's "Message Actions" (Revert and Fork have no
// bh counterpart and are left out).
func (t *screen) openMessageDialog(it *item) {
	t.openDialog(&dialog{title: "Message Actions", size: dialogSize,
		items: []dialogItem{{title: "Copy", desc: "message text to clipboard", value: "copy"}},
		choose: func(t *screen, _ dialogItem) string {
			fmt.Fprint(t.out, osc52(it.text))
			if clipboardWrite != nil {
				_ = clipboardWrite(it.text)
			}
			t.toastf("success", "Copied to clipboard")
			return ""
		}})
	t.drawLocked()
}

// ── rendering ───────────────────────────────────────────────────────────────

// renderItem draws an item at the pane width. hover gives the hover variant;
// full ignores the collapse (used to tell whether collapsing hides anything).
func (t *screen) renderItem(it *item, width int, hover, full bool) []string {
	it.spin = t.spinnerSeqVal
	switch it.kind {
	case "user":
		return renderUser(it, width, hover && !it.pending)
	case "thought":
		return renderThought(it, width, hover)
	case "shell":
		return renderShell(it, width, hover, full || it.open)
	case "block":
		return renderBlock(it, width, hover, full || it.open)
	case "group":
		return t.renderGroup(it, width, hover)
	case "sub":
		return []string{renderSub(it, width, hover)}
	}
	return t.renderTool(it, width, hover)
}

// renderUser is the message box: ┃ in the primary colour on raised.base, one
// padding row above and below, the text two columns in, attachments as chips.
func renderUser(it *item, width int, hover bool) []string {
	bg := bgUser
	if hover {
		bg = bgUserHover
	}
	edge := bluef(bar)
	if it.pending {
		edge = color(fgBorder, bar)
	}
	blank := withBg(edge, bg, width)
	rows := []string{blank}
	text := strings.TrimSpace(tokenRe.ReplaceAllString(it.text, ""))
	if text != "" {
		for _, l := range strings.Split(wrap(text, width-5), "\n") {
			rows = append(rows, withBg(edge+"  "+color(colorText, l), bg, width))
		}
	}
	if len(it.images) > 0 {
		if text != "" {
			rows = append(rows, blank)
		}
		var chips []string
		for _, p := range it.images {
			chips = append(chips, chip("file", path.Base(strings.ReplaceAll(p, "\\", "/"))))
		}
		rows = append(rows, withBg(edge+"  "+strings.Join(chips, " "), bg, width))
	}
	return append(rows, blank)
}

// chip is OpenCode's attachment chip: a bold " file " tag and the name.
func chip(tag, name string) string {
	if !colorsOn {
		return "[" + tag + ": " + name + "]"
	}
	return bold + bgChipFile + fgChipFile + " " + tag + " " + reset + bgBox + colorGray + " " + name + " " + reset
}

// renderThought is "+ Thought: title · 8.1s", and with the body open a ┃ column
// of the reasoning in the muted colour.
func renderThought(it *item, width int, hover bool) []string {
	sign, fg := "+", fgThoughtDim
	if it.open {
		sign = "-"
	}
	if hover {
		fg = fgWarning
		if !it.open {
			sign = "-"
		}
	}
	label := sign + " Thought"
	if it.title != "" {
		label += ": " + it.title
	}
	rows := []string{truncateVis(color(fg, label)+grayf(" · "+fmtDur(it.dur)), width-3)}
	if it.open && strings.TrimSpace(it.text) != "" {
		rows = append(rows, "")
		for _, l := range strings.Split(wrap(it.text, width-7), "\n") {
			rows = append(rows, color(fgThoughtBar, bar)+" "+grayf(l))
		}
	}
	return rows
}

// shellResult parses shell's result: "exit_code: N\nstdout+stderr:\n…".
func shellResult(out string) (code int, body string, ok bool) {
	head, rest, found := strings.Cut(out, "\n")
	if !found || !strings.HasPrefix(head, "exit_code: ") {
		return 0, out, false
	}
	code, err := strconv.Atoi(strings.TrimPrefix(head, "exit_code: "))
	if err != nil {
		return 0, out, false
	}
	if strings.HasPrefix(rest, "stdout+stderr:\n") {
		rest = strings.TrimPrefix(rest, "stdout+stderr:\n")
	} else if rest == "stdout+stderr:" {
		rest = ""
	}
	return code, strings.TrimRight(rest, "\n"), true
}

// blockRows wraps content rows in OpenCode's block: an invisible ┃ gap, the
// raised background (#1e1e1e on hover), one padding row above and below.
func blockRows(content []string, width int, hover bool) []string {
	bg := bgRaised
	if hover {
		bg = bgBox
	}
	edge := color(fgBlockBar, bar) + "  "
	rows := []string{withBg(edge, bg, width)}
	for _, c := range content {
		rows = append(rows, withBg(edge+c, bg, width))
	}
	return append(rows, withBg(edge, bg, width))
}

// renderShell is the shell block: "$ command", the output muted and collapsed
// to its last lines, and the failure reason in red.
func renderShell(it *item, width int, hover, full bool) []string {
	var a struct {
		Command string `json:"command"`
		Workdir string `json:"workdir"`
	}
	_ = json.Unmarshal([]byte(it.args), &a)
	inner := max(10, width-5)
	prompt := "$ "
	if it.running {
		// OpenCode drops the "$ " while the command runs: the spinner is there.
		prompt = spinGlyph(it.spin) + " "
	}
	cmd := strings.Split(wrap(prompt+commandLine(a.Command, a.Workdir), inner), "\n")
	if !full && len(cmd) > 2 {
		cmd = append(cmd[:1], truncateVis(cmd[1], inner-1)+"…")
	}
	var content []string
	for _, l := range cmd {
		content = append(content, color(colorText, l))
	}
	if it.running {
		if body := strings.TrimRight(cleanOutput(it.live), "\n"); body != "" {
			var out []string
			for _, l := range strings.Split(body, "\n") {
				out = append(out, strings.Split(wrapPlain(l, inner), "\n")...)
			}
			if !full {
				out = tailCollapse(out, max(1, collapsedRows-len(cmd)))
			}
			for _, l := range out {
				content = append(content, grayf(l))
			}
		}
		return blockRows(content, width, hover)
	}
	code, body, parsed := shellResult(it.out)
	errLine := ""
	switch {
	case !parsed && it.failed && strings.Contains(it.out, "context canceled"):
		errLine, body = "Command cancelled", ""
	case !parsed && it.failed:
		errLine, body = strings.TrimPrefix(strings.TrimSpace(it.out), "error: "), ""
	case parsed && code != 0:
		errLine = "Command exited with code " + strconv.Itoa(code)
	}
	if body != "" {
		var out []string
		for _, l := range strings.Split(body, "\n") {
			out = append(out, strings.Split(wrapPlain(l, inner), "\n")...)
		}
		if !full {
			out = tailCollapse(out, max(1, collapsedRows-len(cmd)))
		}
		for _, l := range out {
			content = append(content, grayf(l))
		}
	}
	if errLine != "" {
		content = append(content, color(fgError, truncateVis(errLine, inner)))
	}
	return blockRows(content, width, hover)
}

// tailCollapse keeps the last lines of lines within budget rows, the first row
// counting what was dropped (OpenCode's "(N earlier lines)").
func tailCollapse(lines []string, budget int) []string {
	if len(lines) <= budget {
		return lines
	}
	kept := lines[len(lines)-(budget-1):]
	return append([]string{"(" + strconv.Itoa(len(lines)-len(kept)) + " earlier lines)"}, kept...)
}

// wrapPlain breaks a line of output at exactly n runes (output is not prose:
// breaking at spaces would reshuffle columns).
func wrapPlain(s string, n int) string {
	r := []rune(s)
	var out []string
	for len(r) > n {
		out = append(out, string(r[:n]))
		r = r[n:]
	}
	return strings.Join(append(out, string(r)), "\n")
}

// renderBlock is a write/append block (the file with line numbers) or an edit
// block (the diff of oldString → newString), both from the call's arguments.
func renderBlock(it *item, width int, hover, full bool) []string {
	var a struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		OldString string `json:"oldString"`
		NewString string `json:"newString"`
	}
	_ = json.Unmarshal([]byte(it.args), &a)
	inner := max(10, width-5)
	var head string
	var body []string
	switch it.name {
	case "edit":
		head = "← Edit " + a.Path
		del := func(l string) string { return color(fgDiffDel, "-") + " " + l }
		add := func(l string) string { return color(fgDiffAdd, "+") + " " + l }
		for _, l := range strings.Split(strings.TrimRight(a.OldString, "\n"), "\n") {
			body = append(body, withBg(del(truncateVis(l, inner-2)), bgDiffDel, inner))
		}
		for _, l := range strings.Split(strings.TrimRight(a.NewString, "\n"), "\n") {
			body = append(body, withBg(add(truncateVis(l, inner-2)), bgDiffAdd, inner))
		}
	default:
		head = "# Wrote " + a.Path
		if it.name == "append" {
			head = "# Appended " + a.Path
		}
		lines := strings.Split(strings.TrimRight(a.Content, "\n"), "\n")
		nw := max(3, len(strconv.Itoa(len(lines))))
		for i, l := range lines {
			body = append(body, grayf(fmt.Sprintf("%*d ", nw, i+1))+color(colorText, truncateVis(l, inner-nw-1)))
		}
	}
	if !full && len(body) > collapsedRows {
		more := len(body) - (collapsedRows - 1)
		body = append(body[:collapsedRows-1], grayf("("+strconv.Itoa(more)+" more lines)"))
	}
	if it.running {
		head = spinGlyph(it.spin) + " " + strings.TrimLeft(head, "←# ")
	}
	content := append([]string{grayf(truncateVis(head, inner))}, body...)
	if it.failed {
		content = append(content, color(fgError, truncateVis(strings.TrimPrefix(strings.TrimSpace(it.out), "error: "), inner)))
	}
	return blockRows(content, width, hover)
}

// toolLabel is the inline row's text: icon and label per tool (spec §2.5).
func toolLabel(name, args, out string, failed bool) (icon, label string) {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		URL     string `json:"url"`
		Query   string `json:"query"`
		Name    string `json:"name"`
		Offset  int    `json:"offset"`
		Limit   int    `json:"limit"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	in := func(p string) string {
		if p == "" || p == "." {
			return ""
		}
		return " in " + p
	}
	switch name {
	case "read":
		return "→", "Read " + pathRange(a.Path, a.Offset, a.Limit)
	case "glob":
		return "✱", fmt.Sprintf("Glob %q%s%s", a.Pattern, in(a.Path), matchCount(out, failed))
	case "grep":
		return "✱", fmt.Sprintf("Grep %q%s%s", a.Pattern, in(a.Path), matchCount(out, failed))
	case "webfetch":
		return "%", "WebFetch " + a.URL
	case "websearch":
		return "◈", fmt.Sprintf("Web Search %q", a.Query)
	case "skill":
		return "→", fmt.Sprintf("Skill %q", a.Name)
	case "list":
		return "→", "List " + a.Path
	case "subagent":
		return "│", "Subagent — " + oneLine(spawnSummary(args), 80)
	}
	if failed {
		return "✗", name
	}
	return "✓", name
}

// pathRange is "path" or "path:from-to" for a ranged read.
func pathRange(path string, start, limit int) string {
	switch {
	case start > 0 && limit > 0:
		return fmt.Sprintf("%s:%d-%d", path, start, start+limit-1)
	case start > 0:
		return fmt.Sprintf("%s:%d", path, start)
	}
	return path
}

var reTruncNote = regexp.MustCompile(`^\[output truncated`)

// matchCount is " (N matches)" from a glob/grep result: its result lines.
func matchCount(out string, failed bool) string {
	if failed || out == "" || out == "no matches" || strings.HasPrefix(out, "no files match") {
		if failed {
			return ""
		}
		return " (0 matches)"
	}
	n := 0
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(l) != "" && !reTruncNote.MatchString(l) {
			n++
		}
	}
	unit := "matches"
	if n == 1 {
		unit = "match"
	}
	return fmt.Sprintf(" (%d %s)", n, unit)
}

// spawnSummary is the subagent launcher's argument in a few words.
func spawnSummary(args string) string {
	var a struct {
		Prompt  string   `json:"prompt"`
		Prompts []string `json:"prompts"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	if len(a.Prompts) > 1 {
		return strconv.Itoa(len(a.Prompts)) + " sub-agents"
	}
	if len(a.Prompts) == 1 {
		return a.Prompts[0]
	}
	return a.Prompt
}

// knownInline are the tools with their own label; any other shows its
// arguments as "k=v" and expands into them (OpenCode's generic tool).
var knownInline = map[string]bool{"read": true, "glob": true, "grep": true, "webfetch": true,
	"websearch": true, "skill": true, "list": true, "subagent": true}

// genericFields are the "key: value" lines of a generic tool's arguments.
func (t *screen) genericFields(it *item) []string {
	if knownInline[it.name] {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(it.args), &m) != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		out = append(out, k+": "+oneLine(fmt.Sprint(m[k]), 200))
	}
	return out
}

// renderTool is an inline tool row: muted, text.base on hover, red when it
// failed; open, it shows the error (failed) or the arguments and output
// (generic tools).
func (t *screen) renderTool(it *item, width int, hover bool) []string {
	icon, label := toolLabel(it.name, it.args, it.out, it.failed)
	if it.running {
		icon = spinGlyph(t.spinnerSeqVal)
	}
	fields := t.genericFields(it)
	if !knownInline[it.name] && len(fields) > 0 {
		var kv []string
		for _, f := range fields {
			k, v, _ := strings.Cut(f, ": ")
			kv = append(kv, k+"="+oneLine(v, 30))
		}
		label += " " + strings.Join(kv, ", ")
	}
	fg := colorGray
	switch {
	case it.failed:
		fg = fgError
	case hover:
		fg = colorText
	}
	rows := []string{color(fg, truncateVis(icon+" "+label, width-3))}
	if !it.open {
		return rows
	}
	if it.failed {
		for _, l := range strings.Split(wrap(strings.TrimPrefix(strings.TrimSpace(it.out), "error: "), width-7), "\n") {
			rows = append(rows, "  "+color(fgError, l))
		}
		return rows
	}
	for _, f := range fields {
		rows = append(rows, "  "+grayf(truncateVis(f, width-7)))
	}
	if out := strings.TrimSpace(it.out); out != "" {
		lines := strings.Split(out, "\n")
		if len(lines) > collapsedRows {
			lines = append(lines[:collapsedRows], "…")
		}
		rows = append(rows, "  "+grayf(truncateVis("output: "+lines[0], width-7)))
		for _, l := range lines[1:] {
			rows = append(rows, "          "+grayf(truncateVis(l, width-13)))
		}
	}
	return rows
}

// exploreKind maps the exploration tools to OpenCode's group counters.
var exploreKind = map[string]string{"read": "read", "list": "read", "glob": "search",
	"grep": "search", "websearch": "search", "webfetch": "fetch"}

// renderGroup is a run of exploration tools: one member draws as itself, more
// collapse into "→ Explored: 2 reads, 1 search" that a click expands.
func (t *screen) renderGroup(it *item, width int, hover bool) []string {
	if len(it.members) == 1 {
		return t.renderTool(it.members[0], width, hover)
	}
	counts := map[string]int{}
	for _, m := range it.members {
		counts[exploreKind[m.name]]++
	}
	var parts []string
	for _, k := range []string{"read", "search", "fetch"} {
		if n := counts[k]; n > 0 {
			unit := k
			if n > 1 {
				unit = map[string]string{"read": "reads", "search": "searches", "fetch": "fetches"}[k]
			}
			parts = append(parts, strconv.Itoa(n)+" "+unit)
		}
	}
	fg := colorGray
	if hover {
		fg = colorText
	}
	icon := "→"
	for _, m := range it.members {
		if m.running {
			icon = spinGlyph(t.spinnerSeqVal)
			break
		}
	}
	rows := []string{color(fg, truncateVis(icon+" Explored: "+strings.Join(parts, ", "), width-3))}
	if it.open {
		for _, m := range it.members {
			for _, r := range t.renderTool(m, width-2, false) {
				rows = append(rows, "  "+r)
			}
		}
	}
	return rows
}

// renderSub is a finished sub-agent: "↳ Subagent finished · t1 · 11s" in the
// info colour, "! Subagent failed · t2" in red; a click shows /tasks <id>.
func renderSub(it *item, width int, hover bool) string {
	text := "↳ Subagent finished · " + it.taskID
	fg := fgInfo
	if it.state != "completed" {
		text, fg = "! Subagent "+it.state+" · "+it.taskID, fgError
	}
	if it.dur > 0 {
		text += " · " + it.dur.String()
	}
	if hover {
		return boldf(color(fg, truncateVis(text, width-3)))
	}
	return color(fg, truncateVis(text, width-3))
}

// ── producing items ─────────────────────────────────────────────────────────

// UsuarioCom draws a sent message with its attached images.
func (t *screen) UsuarioCom(text string, images []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addItem(&item{kind: "user", text: text, images: images})
}

// Usuario draws a sent message (the box, like OpenCode's).
func (t *screen) Usuario(text string) { t.UsuarioCom(text, nil) }

// thought appends a collapsed Thought; reasoningSummary turns a leading
// "**Title**" into "Thought: Title".
func (t *screen) thought(dur time.Duration, reasoning string) {
	title, body := reasoningSummary(reasoning)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.addItem(&item{kind: "thought", title: title, text: strings.TrimRight(body, "\n"), dur: dur})
}

// toolResult draws a finished tool call the way OpenCode does for its kind.
func (t *screen) toolResult(name, args, out string, failed bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.placeTool(&item{kind: "tool", name: name, args: args, out: cleanOutput(out), failed: failed})
}

// toolStarted draws a call that is about to run, with its input (the command,
// the path) next to a spinner, as OpenCode does; toolFinished later fills the
// SAME item with the result instead of adding another row.
func (t *screen) toolStarted(id, name, args string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	it := &item{kind: "tool", name: name, args: args, running: true}
	if t.runningTools == nil {
		t.runningTools = map[string]*item{}
	}
	t.runningTools[id] = it
	t.placeTool(it)
}

// liveOutputCap bounds what a running shell block keeps of its output: only
// the last rows are shown, and the full result replaces it at the end.
const liveOutputCap = 64 << 10

// toolOutput appends live output to a running call. It does not repaint: the
// spinner ticker does, a few times a second, however fast the output comes.
func (t *screen) toolOutput(id, chunk string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	it := t.runningTools[id]
	if it == nil {
		return
	}
	grow := func() {
		it.live += chunk
		if len(it.live) > liveOutputCap {
			it.live = it.live[len(it.live)-liveOutputCap:]
		}
	}
	if t.scroll == 0 {
		grow()
		return
	}
	// Read back: keep the rows on screen where they are while the block grows.
	before := t.wrappedRows()
	grow()
	t.scroll = max(0, t.scroll+t.wrappedRows()-before)
}

// toolFinished completes the running item with the call's result.
func (t *screen) toolFinished(id, name, args, out string, failed bool) {
	t.mu.Lock()
	it := t.runningTools[id]
	if it == nil {
		t.mu.Unlock()
		t.toolResult(name, args, out, failed)
		return
	}
	defer t.mu.Unlock()
	delete(t.runningTools, id)
	t.mutate(func() {
		it.running, it.live = false, ""
		it.name, it.args, it.out, it.failed = name, args, cleanOutput(out), failed
	})
}

// stopRunningTools ends the spinner of any call still marked running when the
// turn is over (a result that never came must not spin forever). Caller holds
// t.mu.
func (t *screen) stopRunningTools() {
	for id, it := range t.runningTools {
		it.running = false
		delete(t.runningTools, id)
	}
}

// placeTool adds a call's item where its kind goes: a shell or edit block of
// its own, or a member of the current exploration group. Caller holds t.mu.
func (t *screen) placeTool(it *item) {
	name := it.name
	switch name {
	case "shell":
		it.kind = "shell"
	case "edit", "write", "append":
		it.kind = "block"
	}
	if _, explore := exploreKind[name]; explore {
		if last := t.lastItem(); last != nil && last.kind == "group" {
			t.mutate(func() { last.members = append(last.members, it) })
			return
		}
		t.addItem(&item{kind: "group", members: []*item{it}})
		return
	}
	t.addItem(it)
}

var reSubDone = regexp.MustCompile(`^## (\S+) (\S+) \(([^)]*)\)`)

// reJobDone matches a background job's row in a "[shell]" notice.
var reJobDone = regexp.MustCompile(`^## (job-\d+) exit (-?\d+) \(([^)]*)\)`)

// AvisoSubagentes draws one row per finished sub-agent of the notice.
func (t *screen) AvisoSubagentes(text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if m := reJobDone.FindStringSubmatch(l); m != nil {
			t.appendLocked("", grayf("background job "+m[1]+" finished (exit "+m[2]+", "+m[3]+")"))
			n++
			continue
		}
		m := reSubDone.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		d, _ := time.ParseDuration(m[3])
		t.addItem(&item{kind: "sub", taskID: m[1], state: m[2], dur: d})
		n++
	}
	if n == 0 {
		t.appendLocked("", grayf("sub-agents finished"))
	}
}

// compactionRule is OpenCode's compaction row: a full-width rule with the label
// centered.
func (t *screen) compactionRule() {
	t.mu.Lock()
	defer t.mu.Unlock()
	w := max(20, t.conversationWidth()-3)
	label := " Compaction "
	left := (w - len(label)) / 2
	rule := grayf(strings.Repeat("─", left) + label + strings.Repeat("─", w-left-len(label)))
	var add []string
	if n := len(t.lines); n > 0 && t.lines[n-1] != "" {
		add = append(add, "")
	}
	t.appendLocked(append(add, rule)...)
}

// turnFooter is the line after a turn: model in the agent colour, then the
// duration, the throughput and how the turn ended, muted.
func (t *screen) turnFooter(model string, dur time.Duration, extra ...string) {
	parts := []string{bluef(model), grayf(fmtDur(dur))}
	for _, e := range extra {
		parts = append(parts, grayf(e))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	add := []string{}
	if n := len(t.lines); n > 0 && t.lines[n-1] != "" {
		add = append(add, "")
	}
	t.appendLocked(append(add, strings.Join(parts, grayf(" · ")))...)
}

// TurnoFim closes a turn that did not end with Tokens: an interrupted turn gets
// " · interrupted" on its footer, a failed one an "Error:" line above it.
func (t *screen) TurnoFim(model string, dur time.Duration, interrupted bool, err error) {
	if interrupted {
		t.turnFooter(model, dur, "interrupted")
		return
	}
	t.mu.Lock()
	t.appendLocked("", color(fgError, "Error: "+oneLineErr(err)))
	t.mu.Unlock()
	t.turnFooter(model, dur)
}

// transcriptText is the transcript as plain text, the way the frame shows it.
func (t *screen) transcriptText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for _, r := range t.wrapped(t.conversationWidth() - 2) {
		out = append(out, strings.TrimRight(stripANSI(r), " "))
	}
	return strings.Join(out, "\n")
}

// commandLine is the shell block's command: "cd <wd> && command" when the call
// ran somewhere else, as OpenCode prints it.
func commandLine(cmd, workdir string) string {
	if workdir == "" || workdir == "." {
		return cmd
	}
	return "cd " + workdir + " && " + cmd
}

// peekImages returns the images queued for line without taking them: the user
// box shows them as chips, and the REPL still takes them for the model.
func (t *screen) peekImages(line string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	q := t.sentImages[strings.TrimSpace(line)]
	if len(q) == 0 {
		return nil
	}
	return q[0]
}

// cleanOutput makes tool output safe to paint inside a box: Windows CRLF and
// lone carriage returns become plain newlines (a \r moves the cursor back to
// column 1 and the box's background then paints over the line), tabs become
// spaces, and other control characters (ESC included: a program's own colour
// codes would break the box) are dropped.
func cleanOutput(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\t", "    ")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r >= 32 && r != 127 {
			return r
		}
		return -1
	}, s)
}

// spinGlyph is the braille spinner frame of a running transcript row. seq is
// the footer spinner's 40 ms counter; the braille one turns every other frame.
func spinGlyph(seq int) string { return spinFrames[(seq/2)%len(spinFrames)] }
