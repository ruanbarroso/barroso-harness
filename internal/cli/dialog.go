// Dialogs over the screen, in OpenCode's geometry (ui/dialog.tsx,
// ui/dialog-select.tsx): the frame underneath is dimmed by RGBA(0,0,0,150), a
// panel is centered horizontally with its top at H/4 (60/88 columns, capped to
// the terminal), and holds a title with "esc", a search field and a grouped
// list driven by keyboard and mouse (move highlights, mouse-up selects, a click
// on the backdrop closes).
//
// Every dialog is fed by real bh data (catalog, sessions, skills, sub-agents,
// MCP clients, compaction policy) through dialogSource, and every choice ends
// in the same slash command typing it would run — nothing here is decorative.
package cli

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// Theme colours the overlays use (spec §0).
const (
	bgAccent   = "\033[48;2;250;178;131m" // background.action.primary.focused
	fgAccent   = "\033[38;2;250;178;131m" // text.formfield.selected
	fgHeader   = "\033[38;2;157;124;216m" // hue.accent[200], dialog group headers
	fgBorder   = "\033[38;2;72;72;72m"    // border.base
	fgInfo     = "\033[38;2;86;182;194m"
	fgSuccess  = "\033[38;2;127;216;143m"
	fgError    = "\033[38;2;224;108;117m"
	fgWarning  = "\033[38;2;245;167;66m"
	dialogSize = 60
	dialogWide = 88
)

// dialogItem is one row of a dialog list.
type dialogItem struct {
	title, desc, cat, footer string
	current                  bool   // OpenCode's "●" row (the active model, the open session…)
	value                    string // what choosing the row acts on
}

// dialogGeo is where the last frame painted the dialog, for mouse hit-testing.
type dialogGeo struct {
	left, top, width, bottom int
	titleRow, escCol         int
	rowItem                  map[int]int // screen row -> item index
}

// dialog is the open overlay. Exactly one of items/text/prompt is its body.
type dialog struct {
	title, placeholder string
	size               int
	items              []dialogItem
	text               []string // read-only body (help, context, sub-agent conversation)
	prompt             bool     // a single text field (rename)
	query              []rune
	sel, off           int
	loading            bool
	errText            string
	// taskID marks the live sub-agent inspector: its body is rebuilt on every
	// frame while the task runs (tab/shift+tab cycles to the next one), so the
	// dialog the key/mouse model opens stays a read-only text panel.
	taskID    string
	taskLines int
	// choose runs for the chosen row; submit for a prompt's text. Both return a
	// slash command for the REPL, or "" when the dialog handled it itself.
	choose func(t *screen, it dialogItem) string
	submit func(t *screen, text string) string

	// upd is the update dialog's state (only set for /update).
	upd *updateState

	geo      dialogGeo
	pressIdx int
	pressOut bool
	pressEsc bool
}

// ── data ────────────────────────────────────────────────────────────────────

// dialogSource is the REPL state the dialogs read. The REPL owns cfg, the
// session store and the sub-agent manager; the screen gets a snapshot of the
// config on every prompt (update) so dialogs never read it mid-change.
type dialogSource struct {
	mu      sync.Mutex
	cfg     config.Config
	sessID  string
	db      *store.Store
	gerente *subagent.Gerente
	cwd     string
	// steer delivers a message into the running turn (Session.AddSteer).
	steer func(runtime.Steer)
}

func (s *dialogSource) update(cfg config.Config, sessID string) {
	s.mu.Lock()
	s.cfg, s.sessID = cfg, sessID
	s.mu.Unlock()
}

func (s *dialogSource) snapshot() (config.Config, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.sessID
}

func (s *dialogSource) models() ([]dealer.CatalogModel, error) {
	cfg, _ := s.snapshot()
	base, key := "", ""
	if p := cfg.Ativo(); p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return fetchModels(ctx, base, key)
}

func (s *dialogSource) sessions() []sessionRow {
	if s.db == nil {
		return nil
	}
	return loadSessionRows(s.db, 100)
}

func (s *dialogSource) tasks() []subagent.Tarefa {
	if s == nil || s.gerente == nil {
		return nil
	}
	return s.gerente.Fotos()
}

func (s *dialogSource) context() []string {
	cfg, _ := s.snapshot()
	var b strings.Builder
	cmdContext(&b, &cfg, s.db)
	return strings.Split(strings.TrimRight(stripANSI(b.String()), "\n"), "\n")
}

// bindSource hands the screen the REPL's data. Called once by the REPL.
func (t *screen) bindSource(src *dialogSource) {
	t.mu.Lock()
	t.src = src
	t.mu.Unlock()
}

// ── opening ─────────────────────────────────────────────────────────────────

func (t *screen) openDialog(d *dialog) {
	d.pressIdx = -1
	t.dlg = d
	t.ac = nil
}

func (t *screen) closeDialog() { t.dlg = nil }

// dialogCommand reports whether a submitted line is a bare dialog command
// ("/model" alone opens the model dialog; "/model gpt-x" goes to the REPL).
func dialogCommand(line string) (string, bool) {
	for _, c := range commands() {
		if c.dialog && c.name == line {
			return c.name, true
		}
	}
	return "", false
}

// runCommand opens the dialog of a dialog command, or returns the command for
// the REPL to run.
func (t *screen) runCommand(name string) string {
	switch name {
	case "/model":
		t.openModelDialog()
	case "/effort":
		t.openEffortDialog()
	case "/resume":
		t.openSessionsDialog()
	case "/rename":
		t.openRenameDialog()
	case "/context":
		t.openContextDialog()
	case "/skills":
		t.openSkillsDialog()
	case "/tasks":
		t.openTasksDialog()
	case "/mcps":
		t.openMCPDialog()
	case "/help":
		t.openHelpDialog()
	case "/update":
		t.openUpdateDialog()
	default:
		return name
	}
	return ""
}

// openPalette is Ctrl+P: every command of the registry, grouped, searchable.
func (t *screen) openPalette() {
	var items []dialogItem
	for _, c := range commands() {
		items = append(items, dialogItem{title: c.name, desc: c.desc, cat: c.cat, footer: c.key, value: c.name})
	}
	t.openDialog(&dialog{title: "Commands", placeholder: "Search", size: dialogSize, items: items,
		choose: func(t *screen, it dialogItem) string { return t.runCommand(it.value) }})
}

// async fills d off the key loop (network, disk) and repaints when it lands,
// unless the user closed or replaced the dialog meanwhile.
func (t *screen) async(d *dialog, load func() ([]dialogItem, error)) {
	d.loading = true
	go func() {
		items, err := load()
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.dlg != d {
			return
		}
		d.loading = false
		if err != nil {
			d.errText = oneLineErr(err)
		}
		d.items = items
		for i, it := range items {
			if it.current {
				d.sel = i
			}
		}
		t.drawLocked()
	}()
}

func (t *screen) openModelDialog() {
	cur, prov := t.state.model, t.state.provider
	d := &dialog{title: "Select model", placeholder: "Search models", size: dialogSize,
		choose: func(t *screen, it dialogItem) string {
			if it.value == cur {
				return ""
			}
			return "/model " + it.value
		}}
	t.openDialog(d)
	src := t.src
	if src == nil {
		d.errText = "no catalog in this session"
		return
	}
	t.async(d, func() ([]dialogItem, error) {
		ls, err := src.models()
		var items []dialogItem
		for _, m := range ls {
			desc := ""
			if m.ContextLength > 0 {
				desc = fmt.Sprintf("%dk", m.ContextLength/1000)
			}
			items = append(items, dialogItem{title: m.ID, desc: desc, cat: prov, current: m.ID == cur, value: m.ID})
		}
		return items, err
	})
}

func (t *screen) openEffortDialog() {
	cur := t.state.effort
	items := []dialogItem{{title: "None", desc: "no reasoning_effort", current: cur == "" || cur == "none", value: "-"}}
	for _, e := range Esforcos {
		items = append(items, dialogItem{title: e, current: e == cur, value: e})
	}
	d := &dialog{title: "Select variant", placeholder: "Search", size: dialogSize, items: items,
		choose: func(t *screen, it dialogItem) string {
			if it.current {
				return ""
			}
			return "/effort " + it.value
		}}
	for i, it := range items {
		if it.current {
			d.sel = i
		}
	}
	t.openDialog(d)
}

// sessionDay is OpenCode's session-list group: "Today", else the date as
// JavaScript's toDateString prints it ("Mon Oct 13 2026").
func sessionDay(ts string, now time.Time) string {
	at, err := time.Parse(store.TsFormato, ts)
	if err != nil {
		return "Older"
	}
	at = at.Local()
	if y, m, d := at.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "Today"
	}
	return at.Format("Mon Jan 02 2006")
}

func (t *screen) openSessionsDialog() {
	d := &dialog{title: "Sessions", placeholder: "Search sessions", size: dialogWide,
		choose: func(t *screen, it dialogItem) string {
			if it.current {
				return ""
			}
			return "/resume " + it.value
		}}
	t.openDialog(d)
	src := t.src
	if src == nil || src.db == nil {
		d.errText = "sessions are not persisted in this run"
		return
	}
	t.async(d, func() ([]dialogItem, error) {
		_, cur := src.snapshot()
		now := time.Now()
		var items []dialogItem
		for _, r := range src.sessions() {
			title := strings.TrimSpace(r.Title)
			if title == "" {
				title = oneLine(r.Previa, 80)
			}
			if title == "" {
				title = "(untitled)"
			}
			items = append(items, dialogItem{title: title, cat: sessionDay(r.UpdatedAt, now),
				footer: formatAge(r.UpdatedAt), current: r.ID == cur, value: r.ID})
		}
		return items, nil
	})
}

func (t *screen) openRenameDialog() {
	d := &dialog{title: "Rename session", placeholder: "Session title", size: dialogSize, prompt: true,
		query: []rune(t.state.title),
		submit: func(t *screen, text string) string {
			name := strings.Join(strings.Fields(text), " ")
			if name == "" || name == t.state.title {
				return ""
			}
			return "/rename " + name
		}}
	t.openDialog(d)
}

func (t *screen) openContextDialog() {
	d := &dialog{title: "Context", size: dialogWide}
	if t.src != nil {
		d.text = t.src.context()
	} else {
		d.text = []string{"no session in this run"}
	}
	t.openDialog(d)
}

// openSkillsDialog lists the skills; choosing one puts "/name " in the prompt,
// ready for its arguments, which is how a skill runs.
func (t *screen) openSkillsDialog() {
	cwd := ""
	if t.src != nil {
		cwd = t.src.cwd
	}
	var items []dialogItem
	for _, s := range runtime.SkillsDisponiveis(cwd) {
		items = append(items, dialogItem{title: "/" + s.Nome, desc: oneLine(s.Descricao, 70), footer: s.Fontes, value: s.Nome})
	}
	t.openDialog(&dialog{title: "Skills", placeholder: "Search skills", size: dialogSize, items: items,
		choose: func(t *screen, it dialogItem) string {
			t.ed, t.boxTop = newEditor(), 0
			t.editor().Insert("/" + it.value + " ")
			return ""
		}})
}

func (t *screen) openTasksDialog() {
	var items []dialogItem
	if t.src != nil {
		for _, tk := range t.src.tasks() {
			tk := tk
			items = append(items, dialogItem{title: tk.ID + "  " + oneLine(tk.Prompt, 60), desc: tk.Estado,
				footer: taskDuration(&tk), current: tk.TerminadaEm.IsZero(), value: tk.ID})
		}
	}
	t.openDialog(&dialog{title: "Sub-agents", placeholder: "Search", size: dialogWide, items: items,
		choose: func(t *screen, it dialogItem) string {
			t.openSubagentDialog(it.value)
			return ""
		}})
}

// openSubagentDialog opens the live conversation of one sub-agent (OpenCode's
// subagent inspector): the body is rebuilt on every frame while the task runs.
func (t *screen) openSubagentDialog(id string) {
	d := &dialog{title: "Subagent " + id, size: dialogWide, taskID: id}
	t.updateSubagentDialog(d)
	t.openDialog(d)
}

// updateSubagentDialog refreshes the inspector from the manager's snapshot and
// keeps the view pinned to the bottom while the user has not scrolled away.
func (t *screen) updateSubagentDialog(d *dialog) {
	if d == nil || d.taskID == "" {
		return
	}
	for _, task := range t.src.tasks() {
		if task.ID != d.taskID {
			continue
		}
		status := task.Estado
		if !task.Inicada.IsZero() {
			status += " · " + taskDuration(&task)
		}
		d.title = "Subagent " + task.ID + " · " + status
		width := max(20, min(d.size, max(20, t.w-2))-8)
		maxRows := max(3, t.h/2-6)
		atBottom := d.off >= max(0, d.taskLines-maxRows)
		d.text = subagentConversationLines(task, width)
		if atBottom {
			d.off = max(0, len(d.text)-maxRows)
		}
		d.taskLines = len(d.text)
		return
	}
	d.title = "Subagent " + d.taskID
	d.text = []string{"Sub-agent not found"}
	d.taskLines = len(d.text)
}

// cycleSubagentDialog moves the inspector to the next/previous task (tab).
func (t *screen) cycleSubagentDialog(d *dialog, direction int) {
	tasks := t.src.tasks()
	if d == nil || d.taskID == "" || len(tasks) < 2 {
		return
	}
	index := 0
	for i := range tasks {
		if tasks[i].ID == d.taskID {
			index = i
			break
		}
	}
	d.taskID = tasks[(index+direction+len(tasks))%len(tasks)].ID
	d.off = 0
	t.updateSubagentDialog(d)
}

// openMCPDialog shows the MCP servers and their real status. Read-only: the
// connections belong to the startup wiring, so there is no toggle to offer.
func (t *screen) openMCPDialog() {
	var items []dialogItem
	for _, m := range t.state.mcps {
		foot := "Connected ✓"
		if m.falha {
			foot = "Failed !"
		}
		items = append(items, dialogItem{title: m.name, footer: foot, value: m.name})
	}
	t.openDialog(&dialog{title: "MCP servers", placeholder: "Search", size: dialogSize, items: items,
		choose: func(*screen, dialogItem) string { return "" }})
}

func (t *screen) openHelpDialog() {
	text := []string{"Press ctrl+p to see all available actions and commands in any context.", ""}
	for _, c := range commands() {
		text = append(text, fmt.Sprintf("%-22s %s", c.usage, c.desc))
	}
	t.openDialog(&dialog{title: "Help", size: dialogSize, text: text})
}

// ── list model ──────────────────────────────────────────────────────────────

// visible returns the items the dialog shows: all of them in their own order
// without a query, ranked by the fuzzy match with one.
func (d *dialog) visible() []dialogItem {
	q := strings.TrimSpace(string(d.query))
	if q == "" || d.prompt {
		return d.items
	}
	idx := fuzzyRank(len(d.items), q, func(i int) string {
		return d.items[i].title + " " + d.items[i].cat + " " + d.items[i].desc
	})
	out := make([]dialogItem, len(idx))
	for i, j := range idx {
		out[i] = d.items[j]
	}
	return out
}

// dlgRow is one painted line of the list: a group header, a blank before it,
// or an item.
type dlgRow struct {
	header string
	item   int // -1 for headers and blanks
}

// rows lays the visible items out with OpenCode's group headers (only when no
// query is typed: a search shows the ranked results flat).
func (d *dialog) rows(items []dialogItem) []dlgRow {
	grouped := strings.TrimSpace(string(d.query)) == ""
	var out []dlgRow
	last := ""
	for i, it := range items {
		if grouped && it.cat != "" && it.cat != last {
			if len(out) > 0 {
				out = append(out, dlgRow{item: -1})
			}
			out = append(out, dlgRow{header: it.cat, item: -1})
			last = it.cat
		}
		out = append(out, dlgRow{item: i})
	}
	return out
}

// move shifts the selection; wrap applies to the arrow keys (OpenCode wraps
// up/down, page keys stop at the ends).
func (d *dialog) move(delta int, wrap bool) {
	n := len(d.visible())
	if n == 0 {
		return
	}
	s := d.sel + delta
	switch {
	case wrap:
		s = ((s % n) + n) % n
	default:
		s = clampInt(s, 0, n-1)
	}
	d.sel = s
}

// ── keys and mouse ──────────────────────────────────────────────────────────

// dialogKey handles a key while a dialog is open; returns the command to run.
func (t *screen) dialogKey(k keyEvent) string {
	d := t.dlg
	name := keyName(k)
	switch name {
	case keyEsc:
		if d.upd != nil && d.upd.cancel != nil {
			d.upd.cancel()
		}
		t.closeDialog()
		return ""
	case "ctrl+c":
		if len(d.query) > 0 && !d.prompt {
			d.query, d.sel = nil, 0
			return ""
		}
		t.closeDialog()
		return ""
	}
	if d.upd != nil {
		return t.updateKey(name)
	}
	if d.text != nil {
		switch name {
		case keyUp, "ctrl+p", "k":
			d.off = max(0, d.off-1)
		case keyDown, "ctrl+n", "j":
			d.off++
		case "pgup":
			d.off = max(0, d.off-10)
		case "pgdn":
			d.off += 10
		case "tab":
			t.cycleSubagentDialog(d, 1)
		case "shift+tab":
			t.cycleSubagentDialog(d, -1)
		case keyEnter:
			t.closeDialog()
		}
		return ""
	}
	switch name {
	case keyUp, "ctrl+p":
		d.move(-1, true)
	case keyDown, "ctrl+n":
		d.move(1, true)
	case "pgup":
		d.move(-10, false)
	case "pgdn":
		d.move(10, false)
	case "home":
		d.sel = 0
	case "end":
		d.sel = max(0, len(d.visible())-1)
	case keyEnter:
		return t.dialogChoose()
	case keyBackspace:
		if n := len(d.query); n > 0 {
			d.query, d.sel = d.query[:n-1], 0
		}
	case "ctrl+u", "ctrl+w":
		d.query, d.sel = nil, 0
	case "paste":
		d.query = append(d.query, []rune(pasteText(k.text))...)
		d.sel = 0
	default:
		if k.action == "" && k.text != "" {
			d.query = append(d.query, []rune(k.text)...)
			if !d.prompt {
				d.sel = 0
			}
		}
	}
	return ""
}

// dialogChoose runs the selected row (or submits the prompt) and closes the
// dialog, unless the choice opened another one (the palette opening the model
// dialog, for instance).
func (t *screen) dialogChoose() string {
	d := t.dlg
	var out string
	switch {
	case d.prompt:
		out = d.submit(t, string(d.query))
	default:
		items := d.visible()
		if d.sel < 0 || d.sel >= len(items) || d.choose == nil {
			return ""
		}
		out = d.choose(t, items[d.sel])
	}
	if t.dlg == d {
		t.closeDialog()
	}
	return out
}

// dialogMouse handles a mouse event while a dialog is open. It returns the
// command to run and whether the frame needs repainting: pointer motion only
// repaints when it moves the highlight.
func (t *screen) dialogMouse(k keyEvent) (string, bool) {
	d := t.dlg
	g := d.geo
	inside := k.row >= g.top && k.row < g.bottom && k.col >= g.left && k.col < g.left+g.width
	idx, onItem := g.rowItem[k.row]
	onItem = onItem && inside
	onEsc := k.row == g.titleRow && k.col >= g.escCol && k.col < g.escCol+3
	if d.upd != nil && inside && !onEsc {
		if cmd, repaint := t.updateMouse(k); repaint || cmd != "" {
			return cmd, repaint
		}
	}
	switch k.action {
	case "mousemove", "mousedrag":
		if onItem && idx != d.sel {
			d.sel = idx
			return "", true
		}
		return "", false
	case "mousedown":
		d.pressOut, d.pressEsc, d.pressIdx = !inside, onEsc, -1
		if onItem {
			d.pressIdx, d.sel = idx, idx
		}
		return "", true
	case "mouseup":
		pressOut, pressEsc, pressIdx := d.pressOut, d.pressEsc, d.pressIdx
		d.pressOut, d.pressEsc, d.pressIdx = false, false, -1
		switch {
		case pressOut && !inside, pressEsc && onEsc:
			t.closeDialog()
		case onItem && idx == pressIdx:
			d.sel = idx
			return t.dialogChoose(), true
		}
		return "", true
	case "rodacima":
		if d.text != nil {
			d.off = max(0, d.off-wheelLines)
		} else {
			d.move(-wheelLines, false)
		}
		return "", true
	case "rodabaixo":
		if d.text != nil {
			d.off += wheelLines
		} else {
			d.move(wheelLines, false)
		}
		return "", true
	}
	return "", false
}

// ── painting ────────────────────────────────────────────────────────────────

var reTrueColor = regexp.MustCompile(`\x1b\[([34])8;2;(\d+);(\d+);(\d+)m`)

// dimFrame darkens every 24-bit colour of a painted frame the way OpenCode's
// RGBA(0,0,0,150) backdrop composites over it: each channel × (1 − 150/255).
func dimFrame(s string) string {
	return reTrueColor.ReplaceAllStringFunc(s, func(m string) string {
		p := reTrueColor.FindStringSubmatch(m)
		ch := func(v string) string {
			n, _ := strconv.Atoi(v)
			return strconv.Itoa(n * 105 / 255)
		}
		return "\x1b[" + p[1] + "8;2;" + ch(p[2]) + ";" + ch(p[3]) + ";" + ch(p[4]) + "m"
	})
}

// drawDialog paints the open dialog and records its geometry. Returns the cell
// for the cursor (the search field), or row 0 for none.
func (t *screen) drawDialog(sb *strings.Builder) (int, int) {
	d := t.dlg
	if d.taskID != "" {
		t.updateSubagentDialog(d)
	}
	width := min(d.size, t.w-2)
	left := (t.w-width)/2 + 1
	top := t.h/4 + 1
	if d.upd != nil {
		// OpenCode's update dialog calls dialog.setCentered(true): it sits in
		// the vertical middle (about 7 rows tall), not at a quarter height.
		top = max(1, (t.h-7)/2+1)
	}
	inner := width - 6
	row := top
	put := func(s string) {
		fmt.Fprintf(sb, "\033[%d;%dH%s", row, left, withBg(s, bgScreen, width))
		row++
	}
	d.geo = dialogGeo{left: left, top: top, width: width, rowItem: map[int]int{}}

	put("")
	title := boldf(color(colorText, d.title))
	d.geo.titleRow, d.geo.escCol = row, left+width-6
	put("   " + title + strings.Repeat(" ", max(1, inner-visWidth(title)-3)) + grayf("esc"))
	put("")

	curRow, curCol := 0, 0
	if d.upd != nil {
		t.drawUpdateBody(put, &row, left, width, sb)
		put("")
		d.geo.bottom = row
		return 0, 0
	}
	if d.text == nil {
		field := grayf(d.placeholder)
		if len(d.query) > 0 {
			field = color(colorText, truncateVis(string(d.query), inner))
		}
		curRow, curCol = row, left+3+min(len(d.query), inner)
		put("   " + field)
		put("")
	}

	maxRows := max(3, t.h/2-6)
	switch {
	case d.text != nil:
		d.off = clampInt(d.off, 0, max(0, len(d.text)-maxRows))
		for _, l := range d.text[d.off:min(len(d.text), d.off+maxRows)] {
			put("   " + color(colorText, truncateVis(l, inner)))
		}
	case d.prompt:
	case d.loading:
		put("   " + grayf("Loading…"))
	case d.errText != "" && len(d.items) == 0:
		put("   " + color(fgError, truncateVis(d.errText, inner)))
	default:
		items := d.visible()
		if len(items) == 0 {
			msg := "No items available"
			if len(d.query) > 0 {
				msg = "No results found"
			}
			put("   " + grayf(msg))
			break
		}
		d.sel = clampInt(d.sel, 0, len(items)-1)
		rows := d.rows(items)
		selRow := 0
		for i, r := range rows {
			if r.item == d.sel {
				selRow = i
			}
		}
		if selRow < d.off {
			d.off = selRow
		}
		if selRow >= d.off+maxRows {
			d.off = selRow - maxRows + 1
		}
		d.off = clampInt(d.off, 0, max(0, len(rows)-maxRows))
		for _, r := range rows[d.off:min(len(rows), d.off+maxRows)] {
			switch {
			case r.item < 0 && r.header == "":
				put("")
			case r.item < 0:
				put("   " + boldf(color(fgHeader, r.header)))
			default:
				d.geo.rowItem[row] = r.item
				t.dialogRow(sb, row, left, width, items[r.item], r.item == d.sel)
				row++
			}
		}
	}
	put("")
	d.geo.bottom = row
	return curRow, curCol
}

// dialogRow paints one list item: "● " when current, the title, the muted
// description, and the footer at the right; the active row inverted in the
// accent colour, as OpenCode's dialog-select does.
func (t *screen) dialogRow(sb *strings.Builder, row, left, width int, it dialogItem, active bool) {
	inner := width - 2
	mark := "  "
	if it.current {
		mark = "● "
	}
	title := truncateVis(it.title, 61)
	foot := it.footer
	descRoom := inner - 2 - len([]rune(mark)) - visWidth(title) - visWidth(foot) - 2
	desc := ""
	if it.desc != "" && descRoom > 4 {
		desc = " " + truncateVis(it.desc, descRoom-1)
	}
	gap := max(1, inner-2-len([]rune(mark))-visWidth(title)-visWidth(desc)-visWidth(foot))
	var line string
	if active {
		line = bgAccent + fgSelected + bold + " " + mark + title + desc + strings.Repeat(" ", gap) + foot + " "
		fmt.Fprintf(sb, "\033[%d;%dH%s %s%s%s %s", row, left, bgScreen, withBg(line, bgAccent, inner), reset, bgScreen, reset)
		return
	}
	tc := colorText
	if it.current {
		tc = fgAccent
	}
	line = " " + color(fgAccent, mark) + color(tc, title) + grayf(desc) + strings.Repeat(" ", gap) + grayf(foot) + " "
	fmt.Fprintf(sb, "\033[%d;%dH%s", row, left, withBg(" "+line, bgScreen, width))
}

// ── toasts ──────────────────────────────────────────────────────────────────

// toast is OpenCode's top-right notification: 5 s, hover keeps it, a click
// dismisses it.
type toast struct {
	text, variant string
	at            time.Time
	geo           [4]int // row, col, width, height
}

const toastLife = 5 * time.Second

func (t *screen) toastf(variant, text string) {
	t.toast = &toast{text: text, variant: variant, at: time.Now()}
}

// toastHit reports whether a cell is on the toast.
func (t *screen) toastHit(row, col int) bool {
	if t.toast == nil {
		return false
	}
	g := t.toast.geo
	return row >= g[0] && row < g[0]+g[3] && col >= g[1] && col < g[1]+g[2]
}

func (t *screen) drawToast(sb *strings.Builder) {
	ts := t.toast
	if ts == nil {
		return
	}
	fg := map[string]string{"success": fgSuccess, "error": fgError, "warning": fgWarning}[ts.variant]
	if fg == "" {
		fg = fgInfo
	}
	maxW := min(60, t.w-6)
	lines := strings.Split(wrap(ts.text, max(10, maxW-6)), "\n")
	width := 0
	for _, l := range lines {
		width = max(width, visWidth(l))
	}
	width = min(maxW, width+6)
	col := t.w - 2 - width + 1
	rows := append(append([]string{""}, lines...), "")
	edge := withBg(color(fg, bar), bgBox, 1)
	for i, l := range rows {
		fmt.Fprintf(sb, "\033[%d;%dH%s%s%s", 2+i, col, edge, withBg("  "+color(colorText, l), bgBox, width-2), edge)
	}
	ts.geo = [4]int{2, col, width, len(rows)}
}
