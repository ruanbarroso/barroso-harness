// Full-screen mode in the OpenCode layout: scrolling conversation in the
// middle, side panel (title, context, MCPs), fixed input box at the bottom and
// a footer with directory, context and shortcuts.
//
// How it works: the usual REPL runs in a goroutine, reading from a pipe and
// writing to a writer whose lines become conversation lines. The keyboard is
// read in raw mode by the main goroutine, which edits the input box and sends
// the line to the pipe on Enter. That way every command — including the ones
// that ask something, like /model and /provider — works without a rewrite.
//
// Only enabled when stdin and stdout are real terminals, and never on the iSH
// build (linux/386), which always uses scrolling mode. Pipes, -p and tests
// never come through here.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/pricing"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"golang.org/x/term"
)

var ioPipe = io.Pipe

// Terminal modes OpenCode enables: alt screen, bracketed paste, and mouse with
// press + drag + any-motion reporting in SGR encoding (plus focus reporting).
// Everything is undone on exit, so the shell comes back exactly as it was.
const (
	escAlt        = "\033[?1049h"
	escNormal     = "\033[?1049l"
	escMouseOn    = "\033[?1000h\033[?1002h\033[?1003h\033[?1006h"
	escMouseOff   = "\033[?1006l\033[?1003l\033[?1002l\033[?1000l"
	escFocusOn    = "\033[?1004h"
	escFocusOff   = "\033[?1004l"
	escPasteOn    = "\033[?2004h" // bracketed paste: \033[200~...\033[201~
	escPasteOff   = "\033[?2004l"
	pasteStart    = "\033[200~"
	pasteEnd      = "\033[201~"
	escSyncStart  = "\033[?2026h" // paints the whole frame at once (no flicker)
	escSyncEnd    = "\033[?2026l"
	escClear      = "\033[2J"
	escClearLine  = "\033[0m" + bgScreen + "\033[K" // clear in the screen colour, never the terminal default
	escHideCursor = "\033[?25l"
	escShowCursor = "\033[?25h"
	maxLines      = 5000
)

// Box backgrounds from the theme: the sidebar and the user message use
// raised.base (#141414); their hover uses the prompt box colour (#1e1e1e).
const (
	bgPanel     = bgRaised
	bgUser      = bgRaised
	bgUserHover = bgBox
)

// sidebarWidth is OpenCode's SESSION_SIDEBAR_WIDTH (42 columns, docked flush).
const sidebarWidth = panelWidth

// panelWidth is the sidebar's width: OpenCode's SESSION_SIDEBAR_WIDTH, 42
// columns, docked flush against the right edge (bh used 40 at >=110).
const panelWidth = 42

// panePad is the session pane's left/right padding (OpenCode: 2, halved when
// the pane is narrower than 44 columns).
const panePad = 2

// paneFloor and panelThreshold implement the only breakpoint in OpenCode: the
// sidebar shows when the terminal is WIDER than 120 columns.
const (
	panelThreshold = 120
	paneFloor      = 44
)

// wheelLines is OpenCode's scroll speed (CustomSpeedScroll(3)).
const wheelLines = 3

// escInterruptWindow is the double-press window OpenCode uses (5 s).
const escInterruptWindow = 5 * time.Second

// footerCopyHint is what OpenCode points at once the command palette exists.
// bh has no palette yet (spec §3), so the same slot offers /help instead.
const footerCopyHint = "ctrl+p"

// panelState is what the side panel, the input box and the footer show.
type panelState struct {
	title     string
	model     string
	provider  string
	effort    string
	cwd       string
	ctxTokens int
	// cachedTokens is how much of the last request's prompt came from the
	// provider's cache (0 = none reported).
	cachedTokens int
	window       int
	mcps         []mcpEntry
	branch       string
}

// mcpEntry is one sidebar MCP row: the server name and whether its connection
// is alive (the dot is green when live, red when the client broke).
type mcpEntry struct {
	name  string
	falha bool
}

type screen struct {
	out io.Writer

	mu      sync.Mutex
	lines   []string
	partial string // line still without \n (e.g. "select model > ")
	transit string // transient spinner line
	// scroll is how many lines the view sits ABOVE the bottom. 0 means
	// pinned (sticky bottom, OpenCode's stickyScroll): new output scrolls
	// with it; >0 means the user read back, and new output must NOT pull the
	// view down again.
	scroll int

	w, h  int
	state panelState
	// ed is the prompt buffer (editor.go); boxTop is the first wrapped row the
	// box shows once the text is taller than the box (it scrolls with the caret).
	ed     *editor
	boxTop int
	masked bool // the typed answer is a secret (API key)
	busy   bool // model turn in progress
	// toastHovered pauses the toast's timer while the pointer is on it.
	toastHovered bool
	// imageCount numbers the [Image N] tokens of the current prompt.
	imageCount int
	// sentImages holds the images of lines already sent and not yet taken by
	// the REPL, keyed by the trimmed line (see takeImages).
	sentImages map[string][][]string
	// lastTabTitle is the last title sent to the tab (resent only on change).
	lastTabTitle string
	spin         chan struct{}
	spinDone     chan struct{}

	// ── mouse ──
	// hits are the rectangles of the last painted frame, with their hover
	// style and click action; mouse events are dispatched through them.
	hits *hitList
	// sel is the in-app text selection (left-drag over the conversation). It
	// is copied on release/right-click/ctrl+c and suppresses click actions
	// while it holds text, as in OpenCode.
	sel   selRange
	selOn bool
	// selAnchor remembers the drag start so the first cells of a very short
	// drag still count as a selection anchor.
	selAnchor selPoint
	// promptCol is where the text starts inside the input box (1-based), saved
	// by the frame so a click moves the caret to the matching input column.
	promptCol int
	// items are the interactive transcript parts (transcript.go), keyed by a
	// stable id that a marker line in t.lines points at; hoverItem is the one
	// under the pointer; rowOwner maps each row of the last wrapped() to the
	// item it belongs to (0 for plain lines). pendingCmd is a slash command a
	// click asked for (a sub-agent row opens /tasks <id>).
	items    map[int]*item
	nextItem int
	// runningTools are the calls started and not finished yet, by call id.
	runningTools map[string]*item
	hoverItem    int
	rowOwner     []int
	pendingCmd   string
	// pendingTaskID is a sub-agent a click asked to inspect: the key loop opens
	// its conversation dialog.
	pendingTaskID string
	// mcpOpen is the sidebar's MCP collapse state (open by default).
	mcpOpen bool
	// wasHome is whether the last frame was the home screen.
	wasHome bool
	// placeholderIdx picks the home placeholder example, drawn once at random
	// (OpenCode's randomIndex on mount).
	placeholderIdx int
	// dlg is the open dialog (dialog.go); ac the prompt's autocomplete popup
	// (autocomplete.go); toast the top-right notification; src the REPL data
	// the dialogs read. files caches the "@" completion's file list.
	dlg       *dialog
	ac        *acState
	toast     *toast
	src       *dialogSource
	files     []string
	filesRoot string
	// queued are prompts waiting for the turn to end (ctrl+x Enter, or steer
	// messages the turn ended without delivering); queuePaused holds them after
	// an interrupt until Enter. replReady is set while the REPL waits for its
	// next line (Estado is drawn right before it reads), so a queued prompt is
	// never typed into a question like /model's.
	queued      []queuedPrompt
	queuePaused bool
	replReady   bool
	// sidebarHidden is OpenCode's <leader>b toggle.
	sidebarHidden bool
	// leaderAt is when ctrl+x was pressed; while the leader waits for its second
	// key the prompt bar turns border.base, as OpenCode shows it.
	leaderAt time.Time
	// panelOff is the sidebar's own scroll offset in lines (its content can grow
	// taller than the pane). It is independent of the transcript's scroll: the two
	// scroll separately, as OpenCode's do, and the offset is clamped per frame.
	panelOff int
	// interruptArmed is set by the first Esc while a turn runs; the second Esc
	// within escInterruptWindow actually interrupts. escAt is when the first
	// press happened.
	interruptArmed bool
	escAt          time.Time
	// spinnerSeq counts frames of the footer's busy spinner; busyStop stops the
	// ticker that advances it while a turn runs.
	spinnerSeqVal int
	busyStop      chan struct{}
	// custoUSD is the turn's money cost, delivered by runtime.RendererCusto
	// only when the active model has a declared price (prices.json). 0 = no
	// price, so the footer shows no cost and tokens stay the metric.
	custoUSD float64
}

// interruptLabel is the footer text while a turn runs: "esc interrupt", and
// "esc again to interrupt" after the first Esc (OpenCode's PromptInterruptStatus).
const (
	interruptLabelArmed = "again to interrupt"
	interruptLabel      = "interrupt"
)

// currentSpin paints the footer's knight-rider spinner frame: 8 cells of ■/⬝
// in the agent colour, at 40 ms per frame, as OpenCode's ui/spinner.ts does.
func currentSpin(seq int, _ bool) string {
	width := 8
	total := width + 9 + (width - 1) + 30 // forward + holdEnd + backward + holdStart
	f := seq % total
	var pos int
	switch {
	case f < width:
		pos = f
	case f < width+9:
		pos = width - 1
	case f < width+9+width-1:
		pos = width - 2 - (f - width - 9)
	default:
		pos = 0
	}
	var sb strings.Builder
	for i := 0; i < width; i++ {
		if i == pos {
			sb.WriteString(bluef("■"))
			continue
		}
		// The trail dims by distance from the head, floored at 30% alpha.
		d := pos - i
		if d < 0 {
			d = -d
		}
		if d < 6 {
			sb.WriteString(color("\033[38;2;"+mixHex(92, 156, 245, float64(d)/6.0)+"m", "⬝"))
			continue
		}
		sb.WriteString(grayf("⬝"))
	}
	return sb.String()
}

// mixHex blends c toward the background at ratio f, keeping the 24-bit escape.
func mixHex(r, g, b int, f float64) string {
	const bg = 10
	clamp := func(v int) int { return clampInt(int(v), 0, 255) }
	if f > 1 {
		f = 1
	}
	return itoa(clamp(r+int(float64(bg-r)*f))) + ";" +
		itoa(clamp(g+int(float64(bg-g)*f))) + ";" +
		itoa(clamp(b+int(float64(bg-b)*f)))
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var d []byte
	for v > 0 {
		d = append([]byte{byte('0' + v%10)}, d...)
		v /= 10
	}
	return string(d)
}

func newScreen(out io.Writer) *screen {
	fmt.Fprint(out, escAlt+escMouseOn+escFocusOn+escPasteOn+escClear)
	return &screen{out: out, hits: newHits(), mcpOpen: true,
		placeholderIdx: int(time.Now().UnixNano() % int64(len(homePlaceholders)))}
}

// takeImages returns (and forgets) the images sent with line: the REPL reads
// lines from a pipe, so the attachments travel beside it, keyed by the text.
func (t *screen) takeImages(line string) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := strings.TrimSpace(line)
	q := t.sentImages[key]
	if len(q) == 0 {
		return nil
	}
	if len(q) == 1 {
		delete(t.sentImages, key)
	} else {
		t.sentImages[key] = q[1:]
	}
	return q[0]
}

func (t *screen) shutdown() {
	t.stopSpin()
	// The tab goes back to a neutral title: the shell (cmd/PowerShell) does not
	// rewrite it, and the session name would stick to the tab after bh exits.
	fmt.Fprint(t.out, escPasteOff+escMouseOff+escFocusOff+escShowCursor+escNormal+tabTitle(""))
}

// tabTitle is the OSC 0 sequence that sets the terminal tab/window name.
// Control characters are stripped so they cannot break the sequence.
func tabTitle(session string) string {
	name := "BH"
	if s := strings.TrimSpace(session); s != "" {
		clean := strings.Map(func(r rune) rune {
			if r < 32 || r == 127 {
				return -1
			}
			return r
		}, s)
		if r := []rune(clean); len(r) > 60 {
			clean = string(r[:60]) + "…"
		}
		name = "BH | " + clean
	}
	return "\033]0;" + name + "\a"
}

// rawTabTitle sets the tab to exactly name, without the "BH | " prefix (the
// home screen shows the harness name alone).
func rawTabTitle(name string) string {
	return "\033]0;" + name + "\a"
}

// ── API used by the REPL and by the turn renderer ─────────────────────────

// Adicionar appends lines to the conversation. While the view is pinned to the
// bottom (sticky) the new lines scroll in with it; once the user read back,
// the view stays exactly where it is — new output must not yank it down.
func (t *screen) Adicionar(text string) {
	t.appendLines(strings.Split(text, "\n")...)
}

// appendLines takes the lock and appends. Callers that already hold t.mu
// (because they read t.lines or t.state to build the lines first) must call
// appendLocked instead, or they deadlock on the same mutex.
func (t *screen) appendLines(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.appendLocked(lines...)
}

// wrappedRows is how many rows the whole transcript occupies once wrapped, which
// is the unit both the frame and the scroll offset count in.
func (t *screen) wrappedRows() int {
	return len(t.wrapped(t.conversationWidth() - 2))
}

// appendLocked adds the lines and keeps the stay-put offset correct. It is the
// single choke point for every writer: user messages, tool output, renderer
// output and expanded reasoning all come through here.
//
// Stay-put is expressed in WRAPPED rows, not logical lines: that is the unit the
// frame shows and the offset is measured in, so a logical line that wraps into
// three rows grows the offset by three regardless of who wrote it. While pinned
// (scroll == 0) the offset stays 0 and new output scrolls in with the view,
// exactly like OpenCode's sticky scroll.
//
// Caller holds t.mu.
func (t *screen) appendLocked(lines ...string) {
	if len(lines) == 0 {
		return
	}
	before := t.wrappedRows()
	for _, l := range lines {
		t.lines = append(t.lines, l)
	}
	t.trim()
	if t.scroll > 0 && len(t.lines) > 0 {
		// Grow the offset by the wrapped growth, so the rows on screen stay put
		// while content grows underneath them.
		t.scroll += max(0, t.wrappedRows()-before)
	}
	t.drawLocked()
}

func (t *screen) Limpar() {
	t.mu.Lock()
	t.lines, t.partial, t.scroll = nil, "", 0
	t.items, t.hoverItem, t.runningTools = nil, 0, nil
	t.drawLocked()
	t.mu.Unlock()
}

func (t *screen) Estado(e panelState) {
	t.mu.Lock()
	// The REPL does not track the cache share: keep the last turn's while the
	// context it belongs to is still the one shown.
	if e.cachedTokens == 0 && e.ctxTokens > 0 && e.ctxTokens == t.state.ctxTokens {
		e.cachedTokens = t.state.cachedTokens
	}
	t.state = e
	t.replReady = true
	t.drawLocked()
	t.mu.Unlock()
}

// Ocupado marks whether a turn is running. The armed interrupt state belongs to
// the turn: it is cleared when the turn ends, so a leftover "esc again to
// interrupt" cannot arm the next turn's first Esc.
func (t *screen) Ocupado(v bool) {
	t.mu.Lock()
	if !v {
		t.interruptArmed = false
		t.escAt = time.Time{}
		if t.busyStop != nil {
			close(t.busyStop)
			t.busyStop = nil
		}
		t.stopRunningTools()
	} else if t.busyStop == nil {
		t.busyStop = make(chan struct{})
		go t.animateSpin(t.busyStop)
	}
	t.busy = v
	t.drawLocked()
	t.mu.Unlock()
}

// spinInterval is OpenCode's knight-rider frame time (ui/spinner.ts).
const spinInterval = 40 * time.Millisecond

// animateSpin advances the footer spinner every spinInterval until stop is
// closed. Only the footer row is repainted: a full frame re-wraps the whole
// transcript, too much work for 25 frames a second.
func (t *screen) animateSpin(stop chan struct{}) {
	tick := time.NewTicker(spinInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
		}
		t.mu.Lock()
		t.spinnerSeqVal++
		if len(t.runningTools) > 0 && t.spinnerSeqVal%2 == 0 && t.dlg == nil {
			// A running call spins in the transcript and its live output
			// grows: the whole frame is repainted every other tick.
			t.drawLocked()
		} else {
			t.repaintFooterLocked()
		}
		t.mu.Unlock()
	}
}

// repaintFooterLocked repaints just the footer row. With a dialog open the
// footer sits under its darkened backdrop, so it is left to the next full
// frame. Caller holds t.mu.
func (t *screen) repaintFooterLocked() {
	if !t.busy || t.dlg != nil || t.w == 0 || t.isHome() {
		return
	}
	f := t.frameFor()
	width := f.boxWidth
	left, right := t.footerParts(width)
	gap := max(1, width-visWidth(left)-visWidth(right))
	row := left + strings.Repeat(" ", gap) + truncateVis(right, width-visWidth(left)-gap)
	// Padded to the pane, NOT cleared to the end of the line as the full frame
	// does: the sidebar shares this row (its cwd line), and erasing it 25
	// times a second made it blink.
	row += strings.Repeat(" ", max(0, width-visWidth(row)))
	var sb strings.Builder
	sb.WriteString(escSyncStart + escHideCursor)
	fmt.Fprintf(&sb, "[%d;1H%s%s[0m", f.footerRow, bgScreen, row)
	sb.WriteString(escSyncEnd)
	fmt.Fprint(t.out, sb.String())
	t.placeCursor()
}

func (t *screen) Ocultar(v bool) {
	t.mu.Lock()
	t.masked = v
	t.mu.Unlock()
}

func (t *screen) trim() {
	if n := len(t.lines); n > maxLines {
		t.lines = t.lines[n-maxLines:]
	}
}

// Pensar animates "⠋ <text> 3s" as the last conversation line until stopSpin.
func (t *screen) Pensar(text string) {
	t.stopSpin()
	t.mu.Lock()
	t.spin, t.spinDone = make(chan struct{}), make(chan struct{})
	stop, end := t.spin, t.spinDone
	t.mu.Unlock()
	go func() {
		defer close(end)
		tick := time.NewTicker(90 * time.Millisecond)
		defer tick.Stop()
		for i := 0; ; i++ {
			t.mu.Lock()
			// OpenCode's in-transcript spinner: glyph + space + label in the
			// warning colour, with NO elapsed seconds (they only appear at the
			// end, on the "+ Thought · 8.1s" line).
			t.transit = "  " + bluef(spinFrames[i%len(spinFrames)]) + " " +
				color(colorText, text)
			t.drawLocked()
			t.mu.Unlock()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
}

func (t *screen) stopSpin() {
	t.mu.Lock()
	stop, end := t.spin, t.spinDone
	t.spin, t.spinDone = nil, nil
	t.mu.Unlock()
	if stop != nil {
		close(stop)
		<-end
	}
	t.mu.Lock()
	t.transit = ""
	t.mu.Unlock()
}

// screenWriter is the REPL's io.Writer: each finished line becomes a
// conversation line; the rest stays as the partial line (a prompt awaiting input).
type screenWriter struct{ t *screen }

// Write appends the finished lines of the chunk to the conversation. The
// stay-put accounting is shared with every other writer (appendLocked), so the
// view does not behave differently depending on which path produced the output.
func (e screenWriter) Write(p []byte) (int, error) {
	t := e.t
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := strings.Split(t.partial+string(p), "\n")
	// Everything except the trailing fragment is a finished line; it goes
	// through the same accounting as every other writer.
	done := make([]string, 0, len(parts)-1)
	for _, l := range parts[:len(parts)-1] {
		if i := strings.LastIndexByte(l, '\r'); i >= 0 {
			l = l[i+1:]
		}
		done = append(done, l)
	}
	t.partial = parts[len(parts)-1]
	// The partial line is part of the wrapped view (a prompt awaiting input), so
	// the accounting is taken before it changes and after the lines land.
	t.appendLocked(done...)
	return len(p), nil
}

// ── drawing ───────────────────────────────────────────────────────────────

// screenSize returns the terminal columns and rows (100x30 when unknown).
// A variable so the preview and the tests can pin a size.
var screenSize = func() (int, int) {
	if a, b, err := term.GetSize(int(os.Stdout.Fd())); err == nil && a >= 40 && b >= 12 {
		return a, b
	}
	return 100, 30
}

// measure reads the terminal size; returns true when it changed.
func (t *screen) measure() bool {
	w, h := screenSize()
	changed := w != t.w || h != t.h
	t.w, t.h = w, h
	return changed
}

// panelVisible implements OpenCode's only breakpoint: wide = width > 120.
func (t *screen) panelVisible() bool { return t.w > panelThreshold && !t.sidebarHidden }

// panePadCols is the pane's left/right padding, halved below the floor width.
func (t *screen) panePadCols() int {
	if t.w < paneFloor {
		return 1
	}
	return panePad
}

// conversationWidth is the pane's content width (pane width minus padding).
func (t *screen) conversationWidth() int {
	pad := t.panePadCols()
	pane := t.w
	if t.panelVisible() {
		pane = t.w - panelWidth
	}
	return max(8, pane-2*pad)
}

// boxTop..boxBottom are the rows of the input box, whose height depends on how
// many lines the typed text wrapped into. Layout (OpenCode §1.3), for a
// terminal of H rows, input of n lines (grow = n-1):
//
//	1..H-7-grow      transcript
//	H-6-grow         status row ("Jump to latest ↓", right-aligned, or blank)
//	H-5-grow         ┃ + prompt padding row (paddingTop 1)
//	H-4-grow..H-3     ┃ + text lines
//	H-2              ┃ + metadata row (paddingTop 1)
//	H-1              bottom cap (╹ then ▀ × width)
//	H-1 → footer     footer row (OpenCode puts it on H-1, blade of the box)
//	H                blank (pane paddingBottom 1)
//
// frame describes one such layout so both drawing and hit-testing agree.
type frame struct {
	w, h       int
	convRow    int // first transcript row
	convHeight int // number of transcript rows
	statusRow  int
	dockRow    int // first row of the queued dock (0 when there is none)
	boxTop     int // the padding row above the text
	boxTextRow int // first text row
	boxTextN   int // number of text lines
	boxMetaRow int // the model/provider/effort row
	boxGapRow  int // the blank metadata padding row (paddingTop 1)
	boxCapRow  int
	footerRow  int
	barCol     int // 1-based column of the ┃ of the box
	textCol    int // 1-based column where the box's text starts
	boxWidth   int // width of the ┃..end of the painted box
	paneCols   int // pane content width
}

// frameFor computes the layout for the current terminal size and input height.
// The transcript grows downward from row 2 (OpenCode's first message row has
// its own marginTop, so row 1 is blank but scrolls with the content).
//
// The prompt owns four fixed rows below the typing area: a blank metadata
// padding row (paddingTop 1), the metadata row, the ╹/▀ cap, and then the
// footer. Below the footer the last row of the screen stays blank (OpenCode's
// pane paddingBottom 1), so the footer lands on H−1.
func (t *screen) frameFor() frame {
	// The home screen centers the prompt: every caller that maps a mouse cell
	// or the caret onto the box must see the same geometry it was painted with.
	if t.isHome() {
		f, _ := t.homeFrame()
		return f
	}
	f := frame{w: t.w, h: t.h}
	boxN := len(t.boxLines(max(6, t.h/3))) // OpenCode: maxHeight = max(6, floor(H/3))
	// Rows are counted from the bottom so the block always ends on H−1 (footer)
	// with row H blank, no matter how tall the typing area is.
	f.footerRow = t.h - 1
	f.boxCapRow = f.footerRow - 1
	f.boxMetaRow = f.boxCapRow - 1
	f.boxGapRow = f.boxMetaRow - 1
	f.boxTextRow = f.boxGapRow - boxN
	f.boxTop = f.boxTextRow - 1
	f.statusRow = f.boxTop - 1
	if len(t.queued) > 0 {
		// OpenCode's queued dock sits between the status row and the prompt.
		f.dockRow = f.boxTop - dockRows
		f.statusRow = f.dockRow - 1
	}
	f.convRow = 2
	f.convHeight = max(3, f.statusRow-f.convRow)
	f.boxTextN = boxN
	f.paneCols = t.conversationWidth()
	// The ┃ sits at the pane's left padding + 1 (1-based column), the text two
	// columns after it; the painted box spans the rest of the pane.
	f.barCol = t.panePadCols() + 1
	f.textCol = f.barCol + 1 + 2
	f.boxWidth = t.boxWidthCols()
	return f
}

// boxWidthCols is the last column (1-based) the prompt box paints: the pane's
// right edge minus its right padding.
func (t *screen) boxWidthCols() int {
	if t.panelVisible() {
		return t.w - panelWidth - t.panePadCols()
	}
	return t.w - t.panePadCols()
}

// fmtTokens formats a count like JavaScript's toLocaleString in en-US:
// 12345 -> "12,345" (OpenCode's sidebar and footer).
func fmtTokens(n int) string {
	return strings.ReplaceAll(thousands(n), ".", ",")
}

// boxLines are the box's visible text rows, capped at maxBox (the window
// follows the caret, see boxView).
func (t *screen) boxLines(maxBox int) []string { return t.boxView(maxBox).lines }

// ensure initializes the fields a zero-value screen (the tests build one
// directly, without newScreen) needs.
func (t *screen) ensure() {
	if t.hits == nil {
		t.hits = newHits()
	}
}

// drawLocked paints the whole frame and rebuilds the hit regions. Called with
// t.mu held.
func (t *screen) drawLocked() {
	t.ensure()
	t.hits.clear()
	fullClear := t.measure()
	if home := t.isHome(); home != t.wasHome {
		// Home and conversation paint different rows: the switch repaints all.
		t.wasHome, fullClear = home, true
	}
	f := t.frameFor()
	if t.isHome() {
		t.drawHomeLocked(fullClear)
		return
	}

	var sb strings.Builder
	sb.WriteString(escSyncStart + escHideCursor)
	if fullClear {
		sb.WriteString(escClear)
	}
	line := func(r int, s string) {
		fmt.Fprintf(&sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}

	// No title line: the session name goes to the terminal tab (OSC 0) and to
	// the panel. bh has one session per terminal, so OpenCode's tab bar makes
	// no sense here.
	tabName := t.state.title
	if strings.TrimSpace(tabName) == "" {
		tabName = "New session"
	}
	if tabName != t.lastTabTitle {
		t.lastTabTitle = tabName
		sb.WriteString(tabTitle(tabName))
	}

	// conversation: every wrapped line, the visible window taken from the end
	// only while the view is pinned; once the user scrolled up, new content
	// must NOT yank the view down (OpenCode's sticky scroll).
	all := t.wrapped(f.paneCols - 2)
	height := f.convHeight
	if t.scroll > len(all)-height {
		t.scroll = max(0, len(all)-height)
	}
	end := len(all) - t.scroll
	vis := all[max(0, end-height):end]
	t.paintTranscript(&sb, f, vis, height)

	// status row: "Jump to latest ↓" while the view sits above the bottom.
	//
	// The hint is right-aligned inside the PANE, not the whole terminal: padding
	// to the full width would put it under the sidebar, which is painted after
	// this row and would cover it in the wide layout.
	//
	// The hit region spans the whole pane row (col 1, width boxWidth) and its
	// plain/hover strings carry their own right-alignment padding. That is what
	// makes a partial (hover) repaint correct: the region is repainted at its own
	// geometry with its own text, so the text lands on exactly the columns the
	// full frame had drawn. A region that stored an unpadded text and a separate
	// column would be repainted flush-left, and one that stored a row padded to
	// the full terminal width would be truncated by its own narrower geometry.
	if t.scroll > 0 {
		pane := f.boxWidth
		gap := max(1, pane-visWidth(grayf("Jump to latest ↓")))
		hint := "Jump to latest ↓"
		row := padding(gap) + grayf(hint)
		t.hits.add(&hitRegion{
			row: f.statusRow, col: 1, width: pane, height: 1,
			plain:  row,
			hover:  padding(gap) + color(colorText, hint),
			action: func() { t.toBottom() },
		})
		line(f.statusRow, row)
	} else {
		line(f.statusRow, "")
	}

	t.drawBox(&sb, f)
	t.drawDock(&sb, f)
	t.drawFooter(&sb, f)
	if t.panelVisible() {
		t.drawPanel(&sb, f)
	}

	t.drawAC(&sb, f)
	t.drawToast(&sb)

	// A dialog dims everything painted so far (OpenCode's backdrop) and draws
	// its panel on top; the cursor then belongs to its search field.
	if t.dlg != nil {
		dimmed := dimFrame(sb.String())
		sb.Reset()
		sb.WriteString(dimmed)
		row, col := t.drawDialog(&sb)
		if row > 0 {
			fmt.Fprintf(&sb, "\033[%d;%dH%s", row, col, escShowCursor)
		}
		sb.WriteString(escSyncEnd)
		fmt.Fprint(t.out, sb.String())
		return
	}

	// cursor: in the box (also during the turn, to type the next message right
	// away), or at the end of the command prompt line
	if t.partial == "" {
		row, col := t.caretScreen(f)
		t.promptCol = col
		fmt.Fprintf(&sb, "\033[%d;%dH", row, col)
		sb.WriteString(escShowCursor)
	} else if len(vis) > 0 {
		last := vis[len(vis)-1]
		fmt.Fprintf(&sb, "\033[%d;%dH", f.convRow+len(vis)-1, 1+visWidth(conversationMargin(last)+last))
		sb.WriteString(escShowCursor)
	}
	sb.WriteString(escSyncEnd)
	fmt.Fprint(t.out, sb.String())
}

// wrapped returns the whole transcript wrapped to width (each entry already
// colored), the way the frame shows it.
func (t *screen) wrapped(width int) []string {
	var all []string
	owner := t.rowOwner[:0]
	for _, l := range t.lines {
		if strings.HasPrefix(l, itemMark) {
			it := t.itemOf(l)
			if it == nil {
				continue
			}
			hover := it.id == t.hoverItem && t.clickable(it)
			if it.kind == "user" || it.kind == "shell" || it.kind == "block" {
				// Boxes span the pane from the bar's column; they are sized here
				// and never re-wrapped.
				for _, r := range t.renderItem(it, t.conversationWidth(), hover, false) {
					all, owner = append(all, r), append(owner, it.id)
				}
				continue
			}
			for _, r := range t.renderItem(it, width, hover, false) {
				all, owner = append(all, "  "+r), append(owner, it.id)
			}
			continue
		}
		for _, r := range wrapVis(l, width) {
			all, owner = append(all, r), append(owner, 0)
		}
	}
	t.rowOwner = owner
	if t.partial != "" {
		all = append(all, wrapVis("  "+t.partial+t.visibleInput(), width)...)
	}
	if t.transit != "" {
		// OpenCode gives every transcript row marginTop 1: the spinner line
		// ("Thinking", a running tool) gets the same blank line above it.
		if n := len(all); n > 0 && strings.TrimSpace(stripANSI(all[n-1])) != "" {
			all = append(all, "")
		}
		all = append(all, t.transit)
	}
	return all
}

// paintTranscript paints the visible conversation rows, applying the in-app
// selection over them. Each row is registered as a selection source, and the
// rows that carry a collapsible Thought line are registered with their click
// action and hover style, as OpenCode's message-parts do.
func (t *screen) paintTranscript(sb *strings.Builder, f frame, vis []string, height int) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	// Total wrapped lines before the window, so a mouse row maps back to the
	// index in t.lines (which line of the transcript is on screen).
	base := len(t.wrapped(f.paneCols-2)) - t.scroll - len(vis)
	// Row 1 is the pane's top margin; the messages start on row 2.
	for i := 0; i < height; i++ {
		row := f.convRow + i
		s := ""
		if i < len(vis) {
			visRow := vis[i]
			if t.selOn {
				visRow = t.applySelection(visRow, base+i)
			}
			s = conversationMargin(visRow) + visRow
		}
		line(row, s)
		// A drag starts on any conversation cell; a row of a clickable item also
		// answers a click and shows the item's hover style (a full repaint: an
		// item spans several rows).
		region := &hitRegion{row: row, col: 1, width: f.paneCols, height: 1, id: "conv"}
		if idx := base + i; idx >= 0 && idx < len(t.rowOwner) && i < len(vis) {
			if it := t.items[t.rowOwner[idx]]; it != nil && t.clickable(it) {
				region.id = "item:" + strconv.Itoa(it.id)
				region.itemID = it.id
				region.plain, region.hover = s, s
				region.action = t.itemAction(it)
			}
		}
		t.hits.add(region)
	}
}

// applySelection paints the selected segment of one wrapped line.
func (t *screen) applySelection(l string, idx int) string {
	plain := stripANSI(l)
	runes := []rune(plain)
	n := t.sel.normalize()
	// A line takes part in the selection when it lies between the endpoints
	// (inclusive); the endpoints then clamp the painted span.
	if idx < n.from.line || idx > n.to.line {
		return l
	}
	from, to := 0, len(runes)
	if idx == n.from.line {
		from = clampInt(n.from.col, 0, len(runes))
	}
	if idx == n.to.line {
		to = clampInt(n.to.col, from, len(runes))
	}
	if from >= to {
		return l
	}
	return color(colorText, string(runes[:from])) +
		bgSelection + fgSelected + string(runes[from:to]) + reset +
		color(colorText, string(runes[to:]))
}

// drawBox paints the input box: the ┃ gutter, the padded text rows, the
// metadata row, and the bottom cap. No bottom padding row inside the box.
func (t *screen) drawBox(sb *strings.Builder, f frame) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	// While the ctrl+x leader waits for its second key the bar turns
	// border.base (#484848), as OpenCode shows a pending leader.
	barFg := bluef
	if !t.leaderAt.IsZero() && time.Since(t.leaderAt) < leaderTimeout {
		barFg = func(s string) string { return color("[38;2;72;72;72m", s) }
	}
	pad := strings.Repeat(" ", f.barCol-1)
	// Every box row spans the same columns (the bar to the box's right edge),
	// with the box background behind the bar, the gap and the text, so no row
	// leaves a hole in the screen colour on either side.
	inner := f.boxWidth - f.barCol + 1
	row := func(content string) string {
		return pad + withBg(barFg(bar)+"  "+content, bgBox, inner)
	}
	// padding row above the text (paddingTop 1)
	line(f.boxTop, row(""))
	maxBox := max(6, t.h/3)
	for i, l := range t.boxLines(maxBox) {
		painted := color(colorText, l)
		if !t.masked {
			painted = highlightTokens(l)
		}
		// OpenCode shows a placeholder only on the home screen, while the box is
		// empty; the caret sits on its first cell, so the text starts there.
		if i == 0 && l == "" && t.isHome() {
			painted = grayf(truncateVis(homePlaceholder(t.placeholderIdx), f.boxWidth-f.textCol-1))
		}
		line(f.boxTextRow+i, row(painted))
	}
	// metadata row: a blank padding row first (paddingTop 1), then the model,
	// provider and effort (the effort in bold warning), as OpenCode does.
	line(f.boxGapRow, row(""))
	info := color(colorText, t.state.model) + " " + grayf(t.state.provider)
	if t.state.effort != "" {
		info += grayf(" · ") + boldf(orangef(t.state.effort))
	}
	line(f.boxMetaRow, row(info))
	// Bottom cap, OpenCode's prompt edge: ╹ in the bar colour, then ▀ drawn in
	// the box colour over the screen background, so the upper half-block closes
	// the box instead of painting a light bar under it.
	edge := "[38;2;30;30;30m" + strings.Repeat(barEdge, max(0, inner-1)) + reset
	line(f.boxCapRow, pad+barFg(barCap)+edge)
	// the whole box is clickable: a click inside it must not reach the
	// conversation underneath.
	t.hits.add(&hitRegion{row: f.boxTop, col: 1, width: f.boxWidth,
		height: f.boxCapRow - f.boxTop + 1, id: "box"})
}

// drawFooter paints the footer on the row right below the cap; the last row
// stays blank (OpenCode's pane paddingBottom 1).
func (t *screen) drawFooter(sb *strings.Builder, f frame) {
	line := func(r int, s string) {
		fmt.Fprintf(sb, "\033[%d;1H%s%s%s", r, bgScreen, s, escClearLine)
	}
	// The footer is aligned to the prompt box: it starts at the pane's left
	// padding and never crosses into the sidebar.
	width := f.boxWidth
	left, right := t.footerParts(width)
	gap := max(1, width-visWidth(left)-visWidth(right))
	line(f.footerRow, left+strings.Repeat(" ", gap)+truncateVis(right, width-visWidth(left)-gap))
	line(f.h, "")
}

// footerParts builds the left (location or interrupt state) and right (usage
// and shortcut hint) half of the footer, following OpenCode's fit rules: the
// shortcuts are dropped before the usage, and the right side disappears
// entirely while an interrupt is armed on a narrow screen.
func (t *screen) footerParts(w int) (string, string) {
	var left string
	if t.busy {
		label := color(colorText, "esc") + " " + grayf(interruptLabel)
		if t.interruptArmed {
			label = orangef("esc " + interruptLabelArmed)
		}
		left = "   " + currentSpin(t.spinnerSeqVal, t.interruptArmed) + " " + label
	} else if t.state.cwd != "" {
		left = "  " + grayf(t.state.cwd)
	}
	usage := ""
	if t.state.ctxTokens > 0 {
		usage = fmtContext(t.state.ctxTokens, t.state.window)
	}
	// Running sub-agents, like OpenCode's "N subagents" footer entry.
	live := 0
	for _, tk := range t.src.tasks() {
		if tk.TerminadaEm.IsZero() {
			live++
		}
	}
	if live > 0 {
		label := fmt.Sprintf("%d subagent", live)
		if live > 1 {
			label += "s"
		}
		left += "  " + grayf("ctrl+x a ") + color(colorText, label)
	}
	// OpenCode's fit rule: available = (W-8) - min(28, (W-8)/2).
	available := max(0, w-8-min(28, (w-8)/2))
	hint := boldf(color(colorText, footerCopyHint)) + " " + grayf("commands")
	fits := func(parts ...string) bool {
		return visWidth(strings.Join(parts, " · ")) <= available
	}
	var right []string
	switch {
	case usage != "" && hint != "" && fits(usage, hint):
		right = []string{usage, hint}
	case usage != "" && fits(usage):
		right = []string{usage}
	case hint != "" && fits(hint):
		right = []string{hint}
	}
	// Interrupt armed on a narrow screen hides the right side entirely.
	if t.busy && t.interruptArmed && w < 80 {
		right = nil
	}
	return left, strings.Join(right, grayf(" · "))
}

// visibleInput is the prompt text as shown on a command prompt's line: masked
// with • while a secret is typed.
func (t *screen) visibleInput() string {
	text := t.editor().Text()
	if t.masked {
		return strings.Repeat("•", utf8.RuneCountInString(text))
	}
	return text
}

// drawPanel paints the sidebar with its own independent scroll offset: the
// transcript and the sidebar scroll separately, as OpenCode's does.
func (t *screen) drawPanel(sb *strings.Builder, f frame) {
	col := t.w - panelWidth + 1
	panel, n := t.panelLines()
	avail := f.h - 2 // top/bottom padding of 1
	// panelOff is clamped on every frame, so it can never leave a blank stretch
	// at the bottom when the content shrinks (MCPs disconnecting, a session being
	// renamed to a shorter title).
	maxOff := max(0, n-avail)
	off := clampInt(t.panelOff, 0, maxOff)
	t.panelOff = off
	visible := panel[min(off, len(panel)):]
	// The MCP heading, when it is one of the visible lines and there are more
	// than two servers: a mouse-down collapses the section.
	mcpVisible := -1
	for i, l := range visible {
		if strings.Contains(stripANSI(l), "MCP") {
			mcpVisible = i
			break
		}
	}
	// The pinned footer (working directory + branch) always stays visible, so a
	// scrolled sidebar never hides which repository is open.
	pinned := panel[len(panel)-1]
	lastAvail := avail - 1
	for i := 0; i < avail; i++ {
		row := 2 + i
		s := ""
		switch {
		case i == lastAvail:
			s = pinned
		case i < len(visible)-1:
			s = visible[i]
		}
		fmt.Fprintf(sb, "\033[%d;%dH%s%s", row, col, reset, withBg(s, bgPanel, panelWidth-2))
		if i == mcpVisible && len(t.state.mcps) > 2 {
			// The heading carries its own plain/hover text: a region without them
			// is treated as unclickable by the mouse state machine (press() only
			// arms regions that can paint a hover style) and hover never repaints
			// it. The hover variant paints the muted "(N active, M errors)" count
			// in text.base, the way OpenCode's sidebar heading reacts to a pointer.
			hover := t.mcpHeading(false)
			t.hits.add(&hitRegion{
				row: row, col: col, width: panelWidth - 2, height: 1,
				id:     "mcpheader",
				plain:  visible[i],
				hover:  hover,
				action: func() { t.mcpOpen = !t.mcpOpen; t.drawLocked() },
			})
			continue // the region is registered; nothing else to do on this row
		}
		// An MCP row opens the MCP dialog, as OpenCode's sidebar rows do.
		if i < len(visible)-1 && i != lastAvail {
			plain := stripANSI(visible[i])
			for _, m := range t.state.mcps {
				if strings.HasPrefix(strings.TrimSpace(plain), "• "+truncateVis(m.name, panelWidth-18)) {
					t.hits.add(&hitRegion{
						row: row, col: col, width: panelWidth - 2, height: 1, id: "mcprow",
						plain:  withBg(s, bgPanel, panelWidth-2),
						hover:  withBg(s, bgBox, panelWidth-2),
						action: func() { t.openMCPDialog(); t.drawLocked() },
					})
					break
				}
			}
		}
	}
}

// panelLines renders the sidebar: title, Context (hidden with no usage) and MCP
// (hidden with no servers, collapsed past two rows when closed).
func (t *screen) panelLines() ([]string, int) {
	e := t.state
	title := e.title
	if title == "" {
		title = "New session"
	}
	var out []string
	out = append(out, "") // paddingTop 1
	for _, l := range strings.Split(wrap(title, panelWidth-6), "\n") {
		out = append(out, "  "+boldf(color(colorText, l)))
	}
	// Context: hidden entirely while there is no usage (OpenCode's
	// <Show when={state()}>), not a placeholder row.
	if e.ctxTokens > 0 {
		out = append(out, "", "  "+boldf(color(colorText, "Context")))
		out = append(out, "  "+grayf(fmtTokens(e.ctxTokens)+" tokens"))
		if e.window > 0 {
			pct := 100 * e.ctxTokens / e.window
			txt := fmt.Sprintf("%d%% used", pct)
			if pct == 0 {
				txt = "<1% used"
			}
			out = append(out, "  "+grayf(txt))
		}
		if e.cachedTokens > 0 {
			out = append(out, "  "+grayf(fmt.Sprintf("%d%% cached", 100*e.cachedTokens/e.ctxTokens)))
		}
	}
	// MCP: hidden when there are no servers; the header opens/collapses past
	// two servers, exactly like OpenCode's sidebar/mcp.tsx.
	if len(e.mcps) > 0 {
		out = append(out, "", "  "+t.mcpHeading(true))
		if t.mcpOpen || len(e.mcps) <= 2 {
			for _, m := range e.mcps {
				name := truncateVis(m.name, panelWidth-18)
				dot := greenf("•")
				status := grayf("Connected")
				if m.falha {
					dot, status = redf("•"), redf("Error")
				}
				out = append(out, "  "+dot+" "+boldf(color(colorText, name))+"  "+status)
			}
		}
	}
	out = append(out, "")
	// footer: the working directory and its git branch.
	loc := e.cwd
	if e.branch != "" {
		loc += ":" + e.branch
	}
	out = append(out, "  "+grayf(truncateVis(loc, panelWidth-6)))
	return out, len(out)
}

// mcpHeading renders the sidebar's MCP heading: "▼ MCP" open, and
// "▶ MCP" + the "(N active, M errors)" count when collapsed (OpenCode counts the
// live and the broken servers so the hidden section still reports them).
// muted decides the colour of that count: muted in the plain row, text.base in
// the hover variant — that difference is what the pointer has to show.
func (t *screen) mcpHeading(muted bool) string {
	// OpenCode's sidebar/mcp.tsx only shows the arrow (and only answers the
	// click) with more than two servers; with one or two the heading is plain.
	if len(t.state.mcps) <= 2 {
		return boldf(color(colorText, "MCP"))
	}
	arrow := "▼"
	head := boldf(color(colorText, arrow+" MCP"))
	if t.mcpOpen {
		return head
	}
	arrow = "▶"
	head = boldf(color(colorText, arrow+" MCP"))
	live, bad := 0, 0
	for _, m := range t.state.mcps {
		if m.falha {
			bad++
			continue
		}
		live++
	}
	count := fmt.Sprintf("(%d active", live)
	if bad > 0 {
		count += fmt.Sprintf(", %d error", bad)
		if bad > 1 {
			count += "s"
		}
	}
	count += ")"
	paint := color(colorText, count)
	if muted {
		paint = grayf(count)
	}
	return head + " " + paint
}

// padding pads a footer/status row's left side to width columns.
func padding(width int) string { return strings.Repeat(" ", max(1, width)) }

// conversationMargin: boxes (user messages) start at the input box column;
// the rest of the conversation starts at the column of the TEXT inside them,
// like in OpenCode.
func conversationMargin(l string) string {
	if colorsOn && (strings.HasPrefix(l, bgRaised) || strings.HasPrefix(l, bgBox)) {
		return "  "
	}
	return "   "
}

// ── scroll: sticky bottom that new output must not break ───────────────────

// t.scroll is how many lines the view sits ABOVE the bottom: 0 is the pinned
// (sticky) position, maxScroll() the very top. The helpers below work with the
// offset FROM THE TOP, which is what the keys mean (Home = 0, End = max).

// maxScroll is the furthest the view can sit above the bottom.
func (t *screen) maxScroll() int {
	return max(0, len(t.wrapped(t.conversationWidth()-2))-t.frameFor().convHeight)
}

// sticky reports whether the view is pinned to the bottom. It is DERIVED from
// scroll == 0 rather than kept as a separate field: two pieces of state holding
// the same fact drift apart (a raw screen literal, a code path that set one and
// not the other), and every such drift turns into a view that either stops
// following new output or yanks it down while the user reads back.
func (t *screen) sticky() bool { return t.scroll == 0 }

// offsetFromTop is the view's position measured from the top of the transcript.
func (t *screen) offsetFromTop() int { return t.maxScroll() - t.scroll }

// setOffset pins the view at n lines from the top. Reaching the bottom re-sticks
// the view exactly like OpenCode's sticky scroll (and, now that sticky is derived
// from the offset, no separate flag can fall out of step with it).
func (t *screen) setOffset(n int) {
	t.scroll = clampInt(t.maxScroll()-n, 0, t.maxScroll())
	t.drawLocked()
}

// scrollUp/scrollDown move by n lines: "up" means towards the older content
// (the top), which is a LARGER t.scroll and a smaller offset from the top.
// Moving up un-sticks the view — the status row then shows "Jump to latest ↓"
// and new output no longer pulls it down — and reaching the bottom re-sticks it.
func (t *screen) scrollUp(n int)   { t.setOffset(t.offsetFromTop() - n) }
func (t *screen) scrollDown(n int) { t.setOffset(t.offsetFromTop() + n) }

// scrollPage moves by a fraction of the visible height: half for PgUp/PgDn
// (OpenCode's page keys), a quarter for its "half page".
func (t *screen) scrollPage(fraction int, up bool) {
	step := t.frameFor().convHeight / fraction
	if up {
		t.scrollUp(step)
		return
	}
	t.scrollDown(step)
}

// toBottom re-pins the view to the bottom, like OpenCode's toBottom().
func (t *screen) toBottom() {
	t.scroll = 0
	t.drawLocked()
}

// ── mouse dispatch ─────────────────────────────────────────────────────────

// mouse is the screen's left-button state (idle / selecting / pressed).
var mouse = mouseState{}

// wheelScroll routes a wheel event by the column the pointer is over: the
// sidebar scrolls its own list, everything else scrolls the transcript, at
// OpenCode's 3 lines per tick.
func (t *screen) wheelScroll(row, col, dir int) {
	if t.panelVisible() && col > t.w-panelWidth {
		t.panelScroll(dir)
		return
	}
	// dir is +1 for a wheel-up event: the view moves UP (older content).
	if dir > 0 {
		t.scrollUp(wheelLines)
		return
	}
	t.scrollDown(wheelLines)
}

// panelScroll moves the sidebar's own offset by OpenCode's 3 lines per tick,
// upward for dir > 0. The offset is clamped per frame (drawPanel), so scrolling
// past either end simply stops there.
//
// It does NOT take t.mu: wheel events reach it from the key loop's edit(), which
// already holds the lock. Go mutexes are not reentrant, so locking here would
// deadlock the whole screen on the first wheel tick over the sidebar.
func (t *screen) panelScroll(dir int) {
	if dir > 0 {
		t.panelOff -= wheelLines
	} else {
		t.panelOff += wheelLines
	}
	t.drawLocked()
}

// panelMaxOff is the furthest the sidebar can scroll: its content height minus
// the rows it has (both paddings of 1 taken from the pane height).
func (t *screen) panelMaxOff() int {
	_, n := t.panelLines()
	return max(0, n-(t.h-2))
}

// mouseHover repaints the regions whose hover state changed. Motion inside one
// region costs nothing: only crossing a border changes a state, and then both
// the region that gained the highlight and the one that lost it are repainted
// (otherwise the latter stays painted in the hover style).
func (t *screen) mouseHover(row, col int) {
	// A held press owns the pointer: the region pressed keeps the highlight and
	// hover motion over it must not repaint the conversation underneath.
	if mouse.active() {
		return
	}
	changed := t.hits.hoverRegion(row, col)
	id := 0
	if at := t.hits.at(row, col); at != nil {
		id = at.itemID
	}
	if id != t.hoverItem {
		t.hoverItem = id
		t.drawLocked()
		return
	}
	t.repaintHoverRegions(changed)
}

// repaintHoverRegions writes just the given regions of the last frame instead
// of the whole frame: a hover must not cost a full repaint. The caret is put
// back under the cursor afterwards — a partial repaint moved the terminal cursor,
// and the frame had it on the input box's caret.
func (t *screen) repaintHoverRegions(regions []*hitRegion) {
	var own []*hitRegion
	for _, r := range regions {
		if r.itemID == 0 { // item rows are painted by the full frame
			own = append(own, r)
		}
	}
	if len(own) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString(escHideCursor)
	for _, r := range own {
		r.paint(&sb)
	}
	sb.WriteString(escShowCursor)
	fmt.Fprint(t.out, sb.String())
	t.placeCursor()
}

// recomputePromptCol records the column of the caret for the given frame, the
// same way drawLocked does, so a partial repaint can put the cursor back.
func (t *screen) recomputePromptCol(f frame) {
	_, t.promptCol = t.caretScreen(f)
}

// placeCursor puts the terminal cursor back on the caret of the input box (or
// at the end of a command prompt awaiting an answer), as drawLocked does.
func (t *screen) placeCursor() {
	f := t.frameFor()
	switch {
	case t.partial == "":
		row, col := t.caretScreen(f)
		t.promptCol = col
		fmt.Fprint(t.out, fmt.Sprintf("\033[%d;%dH", row, col)+escShowCursor)
	case f.convHeight > 0:
		// A command prompt line is the last transcript row.
		all := t.wrapped(f.paneCols - 2)
		height := f.convHeight
		if t.scroll > len(all)-height {
			t.scroll = max(0, len(all)-height)
		}
		vis := all[max(0, len(all)-t.scroll-height) : len(all)-t.scroll]
		if len(vis) > 0 {
			last := vis[len(vis)-1]
			fmt.Fprint(t.out, fmt.Sprintf("\033[%d;%dH", f.convRow+len(vis)-1,
				1+visWidth(conversationMargin(last)+last))+escShowCursor)
		}
	}
}

// mouseDown handles a left press: it starts a selection over the conversation
// or arms the click of the region underneath. Any hover highlight is dropped
// first: while a button is held the pressed region owns the pointer.
func (t *screen) mouseDown(row, col int) {
	t.repaintHoverRegions(t.hits.unhover())
	mouse.press(t.hits, row, col)
}

// mouseUp closes the interaction: a release that matches the pressed region
// dispatches its action; the highlight of that region is cleared either way, so
// a press that never became a click leaves nothing painted.
func (t *screen) mouseUp(row, col int) {
	r, kept := mouse.release(t.hits, row, col)
	// A press that started a selection releases it: the selection is kept and
	// copied, and the click is ignored (OpenCode ignores clicks while a
	// selection holds text).
	if r == nil && kept {
		if t.selOn {
			t.copySelection()
		}
		return
	}
	// Whatever region was pressed is repainted plain: a highlight must not
	// survive a click, nor a press that was cancelled.
	if r != nil {
		t.repaintHoverRegions([]*hitRegion{r})
	}
	if kept && r != nil && r.action != nil {
		r.action()
	}
}

// mouseDrag extends the selection (or cancels the click). The drag is measured
// in transcript coordinates: the row maps to the wrapped line (the frame's
// first conversation row), the column subtracts the pane's left padding.
func (t *screen) mouseDrag(row, col int) {
	p := mouse.drag(row, col, t.frameFor().convRow, t.panePadCols())
	if p == nil {
		// The press was dropped (a drag off the pressed region): whatever region
		// the terminal still shows as highlighted goes back to plain.
		t.repaintHoverRegions(t.hits.unhover())
		return
	}
	// mouse.drag gives the row on SCREEN; the selection is kept in transcript
	// lines (what applySelection and selectionText index), so it adds the
	// first visible line. Without it only a transcript shorter than the screen
	// could be selected.
	//
	// The column is measured the same way: paintTranscript prints each row
	// after its own margin (conversationMargin: 3 columns, 2 for a boxed row),
	// so screen column c is rune c-1-margin of the wrapped line.
	p.line += t.viewTop()
	all := t.wrapped(t.frameFor().paneCols - 2)
	margin := 3
	if p.line >= 0 && p.line < len(all) {
		margin = len(conversationMargin(all[p.line]))
	}
	p.col = max(0, col-1-margin)
	if !t.selOn {
		t.selOn = true
		t.sel = selRange{from: *p}
	}
	// The selection end is exclusive; the cell under the pointer is part of
	// it when dragging forward, so the last character dragged over is copied.
	to := *p
	if to.line > t.sel.from.line || to.line == t.sel.from.line && to.col >= t.sel.from.col {
		to.col++
	}
	t.sel.to = to
	t.drawLocked()
}

// viewTop is the index, in the wrapped transcript, of the first line on
// screen (paintTranscript's base).
func (t *screen) viewTop() int {
	f := t.frameFor()
	n := len(t.wrapped(f.paneCols - 2))
	end := n - clampInt(t.scroll, 0, max(0, n-f.convHeight))
	return max(0, end-f.convHeight)
}

// rightClick copies the current selection, as OpenCode does on Windows (where
// copy-on-select is "manual" by default).
func (t *screen) rightClick() {
	if t.selOn {
		t.copySelection()
	}
}

// selectionText is the plain text of the current in-app selection.
func (t *screen) selectionText() string {
	return extractSelection(t.wrapped(t.conversationWidth()-2), t.sel)
}

// hasSelection reports whether the in-app selection holds any text.
func (t *screen) hasSelection() bool {
	return t.selOn && t.selectionText() != ""
}

// clearSelection drops the in-app selection, as OpenCode's Esc does.
func (t *screen) clearSelection() {
	if !t.selOn {
		return
	}
	t.selOn = false
	t.drawLocked()
}

// copySelection copies the selection to the terminal clipboard (OSC 52) and to
// the OS clipboard, and shows the "Copied to clipboard" notice.
func (t *screen) copySelection() {
	text := t.selectionText()
	if text == "" {
		return
	}
	fmt.Fprint(t.out, osc52(text))
	if clipboardWrite == nil || clipboardWrite(text) != nil {
		t.toastf("success", "Copied to clipboard")
	}
	t.drawLocked()
}

type keyEvent struct {
	text   string
	action string
	// mouse is set when the event came from an SGR mouse sequence.
	mouse bool
	col   int
	row   int
}

// mouseEvent maps a decoded SGR event onto a keyEvent action, carrying the
// cell it happened on for the dispatcher.
func mouseEvent(e mouseSeq) keyEvent {
	k := keyEvent{mouse: true, col: e.Col, row: e.Row}
	switch {
	case e.Wheel == 64:
		k.action = "rodacima"
	case e.Wheel == 65:
		k.action = "rodabaixo"
	case e.Drag && e.Button == 0:
		k.action = "mousedrag"
	case e.Drag:
		k.action = "mousemove"
	case e.Motion:
		k.action = "mousemove"
	case e.Release && e.Button == 0:
		k.action = "mouseup"
	case e.Release && e.Button == 2:
		k.action = "rightclick"
	case e.Release:
		k.action = "mouserelease"
	default:
		k.action = "mousedown"
	}
	return k
}

// pasteResult is the outcome of reading the clipboard for image n.
type pasteResult struct {
	n          int
	path, text string
	err        error
}

// applyPaste puts a clipboard read into the input: an image becomes the
// [Image N] placeholder, text goes in as typed, a failure becomes a footer
// notice. Called with t.mu held.
func (t *screen) applyPaste(p pasteResult) {
	switch {
	case p.err != nil:
		if errors.Is(p.err, errNoClipboard) {
			t.toastf("warning", "Nothing to paste")
		} else {
			t.toastf("error", "Paste failed: "+oneLineErr(p.err))
		}
	case p.path != "":
		if p.n <= t.imageCount {
			p.n = t.imageCount + 1 // the input was sent/cleared meanwhile
		}
		t.imageCount = p.n
		t.editor().AddToken(token{kind: tokenImage, text: imageLabel(p.n), path: p.path})
	default:
		t.pasteInput(p.text)
	}
}

func oneLineErr(err error) string {
	s := strings.Join(strings.Fields(err.Error()), " ")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60]) + "…"
	}
	return s
}

// readPaste reads a bracketed paste's content up to "\033[201~". ok is false
// when the input ends before the closing sequence.
func readPaste(r *bufio.Reader) (string, bool) {
	var buf []byte
	for {
		x, err := r.ReadByte()
		if err != nil {
			return "", false
		}
		buf = append(buf, x)
		if x == '~' && len(buf) >= len(pasteEnd) && string(buf[len(buf)-len(pasteEnd):]) == pasteEnd {
			return string(buf[:len(buf)-len(pasteEnd)]), true
		}
	}
}

// ── main entry ────────────────────────────────────────────────────────────

// useScreen reports whether the REPL opens in full-screen mode.
func useScreen(io IO) bool {
	if simpleOnly || os.Getenv("TERM") == "dumb" {
		return false
	}
	in, okIn := io.In.(*os.File)
	out, okOut := io.Out.(*os.File)
	return okIn && okOut && term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}

// runScreen runs the normal REPL behind the full screen. Returns -1 when raw
// mode is unavailable (the caller falls back to scrolling mode).
func runScreen(io IO) int {
	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return -1
	}
	defer term.Restore(fd, old)

	t := newScreen(io.Out)
	defer t.shutdown()

	pr, pw := ioPipe()
	esc := screenWriter{t: t}
	injected := make(chan string, 16)
	sub := IO{In: pr, Out: esc, ErrOut: esc, screen: t, cancelCh: make(chan struct{}, 1),
		inject: func(s string) {
			select {
			case injected <- s:
			default: // queue full: a notice is already pending, and one is enough
			}
		}}
	sub.Reader() // the shared bufio is created here, before IO is copied

	// The API key cannot be read with term.ReadPassword (stdin is in raw mode,
	// owned by the screen): it comes through the box, masked with •.
	prevPromptKey := PromptKey
	PromptKey = func(prompt string) (string, error) {
		t.Ocultar(true)
		defer t.Ocultar(false)
		fmt.Fprint(esc, prompt)
		l, err := sub.Reader().ReadString('\n')
		return strings.TrimSpace(l), err
	}
	defer func() { PromptKey = prevPromptKey }()

	exitCode := make(chan int, 1)
	go func() {
		exitCode <- RunInteractive(sub)
		_ = pr.Close()
	}()

	keys := make(chan keyEvent, 64)
	go readKeys(bufio.NewReader(os.Stdin), keys)
	tick := time.NewTicker(400 * time.Millisecond) // resize and notices
	defer tick.Stop()

	var (
		history []historyEntry
		histPos = -1
		lead    leader
		// noticePending: a sub-agent finished while a prompt was waiting.
		noticePending bool
	)
	// The pipe is synchronous: writing directly would block keys while the REPL
	// is still finishing an interrupted turn. The queue keeps send order.
	output := make(chan string, 16)
	go func() {
		for s := range output {
			_, _ = pw.Write([]byte(s + "\n"))
		}
	}()
	defer close(output)
	send := func(s string) {
		t.mu.Lock()
		if t.partial != "" {
			// the answer to a command prompt is recorded in the conversation
			vis := s
			if t.masked {
				vis = strings.Repeat("•", utf8.RuneCountInString(s))
			}
			t.lines = append(t.lines, t.partial+vis)
			t.partial = ""
		}
		t.ed, t.boxTop = newEditor(), 0
		t.replReady = false
		t.drawLocked()
		t.mu.Unlock()
		output <- s
	}
	edit := func(f func()) {
		t.mu.Lock()
		f()
		t.refreshAC()
		t.drawLocked()
		t.mu.Unlock()
	}
	// The clipboard is read off the key loop (PowerShell takes a second to
	// start); the result comes back through pasted.
	pasted := make(chan pasteResult, 4)
	pasting := false

	for {
		select {
		case c := <-exitCode:
			_ = pw.Close()
			return c
		case p := <-pasted:
			pasting = false
			edit(func() { t.applyPaste(p) })
		case s := <-injected:
			// With a command prompt awaiting an answer (e.g. /model), the synthetic
			// line would be read as the answer: hold it until the prompt closes.
			t.mu.Lock()
			waiting := t.partial != ""
			t.mu.Unlock()
			if waiting {
				noticePending = true
				continue
			}
			output <- s
		case <-tick.C:
			t.mu.Lock()
			idle := t.partial == ""
			t.mu.Unlock()
			if noticePending && idle {
				noticePending = false
				output <- subagentsLine
			}
			t.mu.Lock()
			q, ok := t.nextQueued()
			if ok {
				t.queueSent(q.sent, q.images)
				t.replReady = false
				t.drawLocked()
			}
			t.mu.Unlock()
			if ok {
				output <- encodeLine(q.sent)
			}
			edit(func() {
				if t.toast != nil && time.Since(t.toast.at) > toastLife && !t.toastHovered {
					t.toast = nil
				}
				// OpenCode's interrupt window: the armed "esc again to interrupt"
				// also expires by itself, whether or not the user presses again.
				// Otherwise a first Esc would arm the turn indefinitely, and the
				// next Esc minutes later would cancel it unasked.
				if t.busy && t.interruptArmed && time.Since(t.escAt) >= escInterruptWindow {
					t.interruptArmed = false
					t.escAt = time.Time{}
				}
				// Reassert the tab name: child processes run through cmd.exe
				// (npx.cmd for the Playwright MCP, shell with cmd /C) overwrite
				// the console title, and bh cannot prevent it. Clearing forces a
				// resend on the next frame.
				t.lastTabTitle = ""
			})
		case k, ok := <-keys:
			if !ok {
				_ = pw.Close()
				return <-exitCode
			}
			t.mu.Lock()
			busy, awaitingReply := t.busy, t.partial != ""
			t.mu.Unlock()
			name := keyName(k)
			// command sends a slash command without touching the draft in the box.
			// Only between turns: the REPL reads its next line after the turn ends,
			// and a command typed now would silently wait until then.
			command := func(c string) {
				if c == "" {
					return
				}
				if busy || awaitingReply {
					edit(func() { t.toastf("warning", "Wait for the current turn to finish") })
					return
				}
				output <- c
			}
			cancelTurn := func() {
				select {
				case sub.cancelCh <- struct{}{}:
				default:
				}
			}

			// An open dialog owns the keyboard and the mouse.
			t.mu.Lock()
			if t.dlg != nil {
				var cmd string
				repaint := true
				if k.mouse {
					cmd, repaint = t.dialogMouse(k)
				} else if k.action != "pasteimage" {
					cmd = t.dialogKey(k)
				}
				if repaint {
					t.refreshAC()
					t.drawLocked()
				}
				t.mu.Unlock()
				command(cmd)
				continue
			}
			// The toast: a click dismisses it, hovering keeps it on screen.
			if k.mouse && (k.action == "mousedown" || k.action == "mousemove") && t.toast != nil {
				over := t.toastHit(k.row, k.col)
				t.toastHovered = over
				if over && k.action == "mousedown" {
					t.toast = nil
					t.drawLocked()
					t.mu.Unlock()
					continue
				}
			}
			// The autocomplete popup takes its keys and its clicks.
			used := false
			if k.mouse {
				used = t.acMouse(k)
			} else if !lead.armed {
				used = t.acKey(name)
			}
			if used {
				t.refreshAC()
				t.drawLocked()
				t.mu.Unlock()
				continue
			}
			t.mu.Unlock()

			// OpenCode's leader (ctrl+x): the next key within 2 s is a leader chord.
			if !k.mouse && lead.take(time.Now()) {
				edit(func() { t.leaderAt = time.Time{} })
				switch name {
				case "n":
					command("/new")
				case "l":
					edit(func() { t.openSessionsDialog() })
				case "m":
					edit(func() { t.openModelDialog() })
				case "s":
					edit(func() { t.openMCPDialog() })
				case "b":
					edit(func() { t.sidebarHidden = !t.sidebarHidden })
				case "q":
					cancelTurn()
					output <- "/exit"
				case keyEnter:
					// OpenCode's queue: sent after the current turn, not into it.
					edit(func() {
						if awaitingReply || strings.TrimSpace(t.editor().Text()) == "" {
							return
						}
						entry, sent, imgs := t.takeInput()
						history, histPos = append(history, entry), -1
						t.enqueue(queuedPrompt{entry: entry, sent: sent, images: imgs})
					})
				}
				continue
			}
			if name == "ctrl+x" && !awaitingReply {
				lead.arm(time.Now())
				edit(func() { t.leaderAt = time.Now() })
				continue
			}

			switch name {
			case "":
				continue
			case keyEnter:
				t.mu.Lock()
				if awaitingReply {
					l := t.editor().Text()
					t.mu.Unlock()
					send(l) // an empty Enter is a valid answer (e.g. keeps the model)
					continue
				}
				if strings.TrimSpace(t.editor().Text()) == "" {
					// Empty Enter: while a turn runs it promotes the first queued
					// prompt to a steer; after an interrupt it resumes the queue.
					switch {
					case busy && len(t.queued) > 0 && t.steerTo(t.queued[0].sent, t.queued[0].images):
						t.queued = t.queued[1:]
						t.drawLocked()
					case t.queuePaused:
						t.queuePaused = false
						t.drawLocked()
					}
					t.mu.Unlock()
					continue
				}
				if c, ok := dialogCommand(strings.TrimSpace(t.editor().Text())); ok && !busy {
					t.takeInput()
					t.runCommand(c)
					t.refreshAC()
					t.drawLocked()
					t.mu.Unlock()
					continue
				}
				entry, sent, imgs := t.takeInput()
				if busy {
					// OpenCode's steer: the message joins the running turn at the
					// next tool-round boundary instead of cancelling it.
					history, histPos = append(history, entry), -1
					if !t.steerTo(sent, imgs) {
						t.enqueue(queuedPrompt{entry: entry, sent: sent, images: imgs})
					}
					t.drawLocked()
					t.mu.Unlock()
					continue
				}
				if len(t.queued) > 0 {
					// Prompts already waiting go first: keep the order.
					t.enqueue(queuedPrompt{entry: entry, sent: sent, images: imgs})
					t.queuePaused = false
					history, histPos = append(history, entry), -1
					t.drawLocked()
					t.mu.Unlock()
					continue
				}
				t.queueSent(sent, imgs)
				t.mu.Unlock()
				history = append(history, entry)
				histPos = -1
				send(encodeLine(sent))
			case keyNewline:
				edit(func() {
					if !awaitingReply && !t.masked {
						t.editor().Insert("\n")
					}
				})
			case "paste":
				edit(func() { t.pasteInput(k.text) })
			case "pasteimage":
				if pasting {
					continue
				}
				pasting = true
				t.mu.Lock()
				n := t.imageCount + 1
				t.mu.Unlock()
				go func() {
					path, text, err := pasteClipboard(n)
					pasted <- pasteResult{n: n, path: path, text: text, err: err}
				}()
			case keyUp:
				// Up moves the caret up a line; only at the very start of the buffer
				// does it walk the history, and only while the box is empty or still
				// shows the entry Up brought back (OpenCode's rule).
				edit(func() {
					e := t.editor()
					if !e.caretAtStart() {
						if e.row > 0 {
							e.Up()
						} else {
							e.BufferStart()
						}
						return
					}
					if awaitingReply || len(history) == 0 || histPos == 0 {
						return
					}
					if !e.empty() && (histPos < 0 || e.Text() != history[histPos].text) {
						return
					}
					if histPos < 0 {
						histPos = len(history) - 1
					} else {
						histPos--
					}
					t.recall(history[histPos], true)
				})
			case keyDown:
				edit(func() {
					e := t.editor()
					if !e.caretAtEnd() {
						if e.row < len(e.lines)-1 {
							e.Down()
						} else {
							e.BufferEnd()
						}
						return
					}
					if histPos < 0 || e.Text() != history[histPos].text {
						return
					}
					histPos++
					if histPos >= len(history) {
						histPos = -1
						t.ed = newEditor()
						return
					}
					t.recall(history[histPos], false)
				})
			case "pgup", "pgdn", "rodacima", "rodabaixo", "mousedrag", "mousemove",
				"mousedown", "mouseup", "mouserelease", "rightclick":
				// A selection suppresses hover while it holds text, as OpenCode
				// does: the drag is being made or read. Scrolling is never
				// suppressed (a kept selection used to freeze the wheel until
				// Esc), and a new press drops the old selection and starts over.
				t.mu.Lock()
				selecting := t.hasSelection()
				t.mu.Unlock()
				if selecting && (name == "mousemove" || name == "mousedrag") && mouse.mode != mouseSelecting {
					continue
				}
				edit(func() {
					if name == "mousedown" && t.selOn {
						t.selOn = false
					}
					switch name {
					case "rodacima":
						t.wheelScroll(k.row, k.col, 1)
					case "rodabaixo":
						t.wheelScroll(k.row, k.col, -1)
					case "mousemove":
						t.mouseHover(k.row, k.col)
					case "mousedown":
						// A click in the box's text places the caret there.
						if t.clickBox(k.row, k.col) {
							return
						}
						t.mouseDown(k.row, k.col)
					case "mousedrag":
						t.mouseDrag(k.row, k.col)
					case "mouseup":
						t.mouseUp(k.row, k.col)
					case "mouserelease":
						t.repaintHoverRegions(t.hits.unhover())
					case "rightclick":
						t.rightClick()
					case "pgup": // half the visible height (OpenCode's page keys)
						t.scrollPage(2, true)
					case "pgdn":
						t.scrollPage(2, false)
					}
				})
				// A click that asked for a command (a sub-agent row opens
				// /tasks <id>) hands it to the REPL.
				t.mu.Lock()
				pc := t.pendingCmd
				t.pendingCmd = ""
				t.mu.Unlock()
				command(pc)
			case "ctrl+alt+b", "ctrl+alt+f", "ctrl+alt+u", "ctrl+alt+d", "ctrl+alt+y", "ctrl+alt+e":
				edit(func() {
					switch name {
					case "ctrl+alt+b":
						t.scrollPage(2, true)
					case "ctrl+alt+f":
						t.scrollPage(2, false)
					case "ctrl+alt+u": // OpenCode's "half page" is a quarter viewport
						t.scrollPage(4, true)
					case "ctrl+alt+d":
						t.scrollPage(4, false)
					case "ctrl+alt+y":
						t.scrollUp(1)
					case "ctrl+alt+e":
						t.scrollDown(1)
					}
				})
			case "ctrl+g", "alt+home":
				edit(func() { t.setOffset(0) })
			case "ctrl+alt+g", "alt+end":
				edit(func() { t.toBottom() })
			case "home", "end":
				// With text in the box Home/End move its caret; with an empty box
				// they jump to the first / last message.
				edit(func() {
					e := t.editor()
					switch {
					case !e.empty() && name == "home":
						e.LineStart()
					case !e.empty():
						e.LineEnd()
					case name == "home":
						t.setOffset(0)
					default:
						t.toBottom()
					}
				})
			case "ctrl+l":
				edit(func() { t.w = 0 }) // forces a clear and repaint
			case "ctrl+p":
				if !awaitingReply {
					edit(func() { t.openPalette() })
				}
			case "ctrl+r":
				if !awaitingReply {
					edit(func() { t.openRenameDialog() })
				}
			case "ctrl+t":
				// OpenCode's variant cycle: bh's variants are the effort levels.
				t.mu.Lock()
				cur := t.state.effort
				t.mu.Unlock()
				command("/effort " + nextEffort(cur))
			case keyEsc:
				// A selection cancels itself on Esc before any other action,
				// as OpenCode does.
				t.mu.Lock()
				selecting := t.hasSelection()
				t.mu.Unlock()
				if selecting {
					edit(func() { t.clearSelection() })
					continue
				}
				if !busy {
					continue
				}
				// Esc arms the interrupt; a second press within the window
				// actually interrupts, exactly as OpenCode does.
				t.mu.Lock()
				armed := t.interruptArmed && time.Since(t.escAt) < escInterruptWindow
				t.mu.Unlock()
				if armed {
					edit(func() {
						t.interruptArmed = false
						if len(t.queued) > 0 {
							t.queuePaused = true
						}
					})
					cancelTurn()
					continue
				}
				edit(func() { t.interruptArmed, t.escAt = true, time.Now() })
			case "ctrl+c":
				// OpenCode: copy a selection; otherwise clear a non-empty prompt;
				// on an empty prompt, exit.
				t.mu.Lock()
				selecting := t.hasSelection()
				blank := t.editor().empty()
				t.mu.Unlock()
				switch {
				case selecting:
					edit(func() { t.copySelection() })
				case !blank:
					edit(func() { t.ed, t.imageCount, t.boxTop = newEditor(), 0, 0 })
				case awaitingReply:
					send("") // backs out of the question (an empty answer)
				default:
					if busy {
						cancelTurn()
					}
					output <- "/exit"
				}
			case "ctrl+d":
				// Delete forward; on an empty prompt, exit (OpenCode's app.exit).
				t.mu.Lock()
				blank := t.editor().empty()
				t.mu.Unlock()
				if !blank {
					edit(func() { t.editor().DeleteForward() })
					continue
				}
				if busy {
					cancelTurn()
				}
				output <- "/exit"
			default:
				edit(func() {
					if t.editKey(name) {
						return
					}
					if k.action == "" && k.text != "" {
						t.editor().Insert(k.text)
					}
				})
			}
		}
	}
}

// nextEffort is the effort level ctrl+t moves to: the next of Esforcos, then
// back to off ("-"), as OpenCode cycles a model's variants.
func nextEffort(cur string) string {
	for i, e := range Esforcos {
		if e == cur {
			if i == len(Esforcos)-1 {
				return "-"
			}
			return Esforcos[i+1]
		}
	}
	return Esforcos[0]
}

// ── colored text: visible width, truncation, wrapping, background ─────────

// escapeEnd returns the index right after the ANSI sequence starting at i.
func escapeEnd(s string, i int) int {
	j := i + 1
	if j < len(s) && s[j] == '[' {
		j++
	}
	for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
		j++
	}
	return min(j+1, len(s))
}

// visWidth counts visible columns (ANSI escapes do not count).
func visWidth(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			i = escapeEnd(s, i)
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// truncateVis cuts to n visible columns, keeping the escapes.
func truncateVis(s string, n int) string {
	if visWidth(s) <= n {
		return s
	}
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := escapeEnd(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis >= n-1 {
			sb.WriteString("…" + reset)
			return sb.String()
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return sb.String()
}

// wrapVis splits a colored line into chunks of n visible columns.
func wrapVis(s string, n int) []string {
	n = max(10, n)
	if visWidth(s) <= n {
		return []string{s}
	}
	var out []string
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == '\033' {
			j := escapeEnd(s, i)
			sb.WriteString(s[i:j])
			i = j
			continue
		}
		if vis == n {
			out = append(out, sb.String())
			sb.Reset()
			sb.WriteString("  ")
			vis = 2
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+size])
		vis++
		i += size
	}
	return append(out, sb.String())
}

// wrapInput wraps the typed text into lines of up to n columns, preferring
// the last space on the line (a word that is too long breaks in the middle).
// The box is one column wider than n: the cursor fits after a full line.
func wrapInput(s string, n int) []string {
	n = max(10, n)
	var out []string
	r := []rune(s)
	for len(r) > n {
		cut := n
		for i := n; i > 0; i-- {
			if r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		if cut < len(r) && r[cut] == ' ' {
			cut++
		}
		r = r[cut:]
	}
	return append(out, string(r))
}

// withBg paints s with background bg in exactly n columns. Each reset
// inside s reapplies the background so text color cannot punch a hole in the box.
func withBg(s, bg string, n int) string {
	if !colorsOn {
		bg = ""
	}
	s = strings.ReplaceAll(truncateVis(s, n), reset, reset+bg)
	return bg + s + strings.Repeat(" ", max(0, n-visWidth(s))) + reset
}

func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ".")
}

// shortCwd replaces the home directory with ~ in the footer path.
func shortCwd(cwd string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			if rel == "." {
				return "~"
			}
			return "~" + string(filepath.Separator) + rel
		}
	}
	return cwd
}

// gitBranch returns the current git branch of dir (for OpenCode's
// "~/path:branch" location label), reading .git/HEAD of the nearest repository
// without running git. Empty when dir is not in a repository or HEAD is
// detached.
func gitBranch(dir string) string {
	for d := dir; d != ""; {
		gitPath := filepath.Join(d, ".git")
		if st, err := os.Stat(gitPath); err == nil {
			if !st.IsDir() {
				// worktree/submodule: ".git" is a file "gitdir: <path>"
				b, err := os.ReadFile(gitPath)
				if err != nil {
					return ""
				}
				g := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
				if !filepath.IsAbs(g) {
					g = filepath.Join(d, g)
				}
				gitPath = g
			}
			head, err := os.ReadFile(filepath.Join(gitPath, "HEAD"))
			if err != nil {
				return ""
			}
			ref := strings.TrimSpace(string(head))
			if !strings.HasPrefix(ref, "ref: refs/heads/") {
				return ""
			}
			return strings.TrimPrefix(ref, "ref: refs/heads/")
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return ""
}

// isWorkingState reports whether a user message is the compaction working
// state. "[estado de trabalho" is the legacy marker still found in old logs.
func isWorkingState(s string) bool {
	return strings.HasPrefix(s, "[working state") || strings.HasPrefix(s, "[estado de trabalho")
}

// sessionTitle is the user's first message, as OpenCode names the session.
func sessionTitle(h []dealer.Mensagem) string {
	for _, m := range h {
		if m.Role != "user" || isWorkingState(m.Conteudo) ||
			strings.HasPrefix(m.Conteudo, "[subagents]") || strings.HasPrefix(m.Conteudo, "[output limit]") ||
			strings.HasPrefix(m.Conteudo, "[tool call as text]") ||
			strings.HasPrefix(m.Conteudo, runtime.UpdatePrefix) {
			continue
		}
		t := strings.Join(strings.Fields(m.Conteudo), " ")
		if r := []rune(t); len(r) > 60 {
			t = string(r[:60]) + "…"
		}
		return t
	}
	return ""
}

// ── turn renderer in full-screen mode ─────────────────────────────────────

type screenTurn struct {
	t         *screen
	model     string
	ctxTokens int
	// ended is set once the turn's footer is drawn (Tokens); a turn that
	// stops without it was interrupted or failed (see the REPL).
	ended bool
}

func (r *screenTurn) Pensando() { r.t.Pensar("Thinking") }

// ToolComecou, ToolSaida and ToolTerminou are the live path (runtime's
// RendererAoVivo): the call's row appears when it starts, with its input, a
// shell block fills with output as the command prints it, and the result
// completes the same row.
func (r *screenTurn) ToolComecou(id, name, args string) {
	r.t.stopSpin()
	r.t.toolStarted(id, name, args)
}

func (r *screenTurn) ToolSaida(id, chunk string) { r.t.toolOutput(id, chunk) }

func (r *screenTurn) ToolTerminou(id, name, args, output string, _ time.Duration, failed bool) {
	r.t.toolFinished(id, name, args, output, failed)
}

func (r *screenTurn) ToolInicio(name, args string) {
	icon, label := toolLabel(name, args, "", false)
	r.t.Pensar(icon + " " + label)
}

// Contexto arrives on every request of the turn: the panel and footer update
// immediately, without waiting for the turn to end (a tool-heavy turn takes minutes).
func (r *screenTurn) Contexto(n int) {
	r.ctxTokens = n
	r.t.mu.Lock()
	r.t.state.ctxTokens = n
	r.t.drawLocked()
	r.t.mu.Unlock()
}

// Atualizado draws a [system update] (skills, BARROSO.md or MCP changed
// mid-session) as event rows, not as user speech.
func (r *screenTurn) Atualizado(text string) { r.t.updateLines(text) }

// updateLines draws one "↻" row per update line: only its first line, the
// rest (a whole new BARROSO.md) is for the model.
func (t *screen) updateLines(text string) {
	for _, part := range strings.Split(text, runtime.UpdatePrefix) {
		line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(part), "\n", 2)[0])
		if line == "" {
			continue
		}
		t.Adicionar("  " + grayf("↻ "+line))
	}
}

// Cache arrives with Contexto: the share of the prompt served from the
// provider's cache, shown under the context usage.
func (r *screenTurn) Cache(cached, prompt int) {
	r.t.mu.Lock()
	r.t.state.cachedTokens = min(cached, prompt)
	r.t.drawLocked()
	r.t.mu.Unlock()
}

// Tool and Eco are the plain Renderer path; the full screen gets ToolResultado
// (the whole result and the arguments) instead, so these only stop the spinner.
func (r *screenTurn) Tool(string, time.Duration, bool) { r.t.stopSpin() }
func (r *screenTurn) Eco(string)                       {}

// ToolResultado draws the finished call the way OpenCode does for its kind.
func (r *screenTurn) ToolResultado(name, args, output string, _ time.Duration, failed bool) {
	r.t.stopSpin()
	r.t.toolResult(name, args, output, failed)
}

// Pensou closes "Thinking" with a collapsed "+ Thought: <title> · 8.1s" that a
// click expands.
func (r *screenTurn) Pensou(text string, dur time.Duration) {
	r.t.stopSpin()
	r.t.thought(dur, text)
}

// reasoningSummary mirrors OpenCode's context/thinking.ts: a leading
// "**Title**" paragraph becomes the title and the rest is the body.
func reasoningSummary(text string) (title, body string) {
	content := strings.TrimSpace(text)
	if !strings.HasPrefix(content, "**") {
		return "", content
	}
	end := strings.Index(content, "**")
	if end < 0 {
		return "", content
	}
	rest := content[end+2:]
	// The title ends at the closing "**", and the body starts after the blank
	// line that follows it (or at the end).
	stop := strings.Index(rest, "**")
	if stop < 0 {
		return "", content
	}
	title = strings.TrimSpace(rest[:stop])
	body = strings.TrimSpace(strings.TrimPrefix(rest[stop+2:], "\n"))
	return title, body
}

func (r *screenTurn) Cortada(ceiling int, gaveUp bool) {
	r.t.stopSpin()
	msg := fmt.Sprintf("response cut at the output limit (%s tokens); continuing", thousands(ceiling))
	if gaveUp {
		msg = fmt.Sprintf("response cut at the output limit (%s tokens) again; stopped", thousands(ceiling))
	}
	r.t.Adicionar("  " + yellowf("⚠") + " " + grayf(msg))
}

func (r *screenTurn) Guardrail(proactive bool) {
	r.t.stopSpin()
	_ = proactive
	r.t.compactionRule()
}

func (r *screenTurn) Resposta(text string) {
	r.t.stopSpin()
	r.t.mu.Lock()
	avail := r.t.conversationWidth() - 4
	r.t.mu.Unlock()
	r.t.Adicionar("")
	r.t.Adicionar(renderMarkdown(text, avail))
}

func (r *screenTurn) Tokens(_, output int, dur time.Duration, _ bool) {
	r.t.stopSpin()
	r.ended = true
	var extra []string
	if output > 0 && dur > 0 {
		extra = append(extra, fmt.Sprintf("%.1f tok/s", float64(output)/dur.Seconds()))
	}
	// Money cost, only with a declared price (see RendererCusto): no
	// prices.json, no number — tokens stay the metric.
	if texto := pricing.Formato(r.t.custoUSD); texto != "" {
		extra = append(extra, texto)
	}
	r.t.turnFooter(r.model, dur, extra...)
}

// Custo implements runtime.RendererCusto: the turn's accumulated cost, called
// only when the active model has a price in prices.json.
func (r *screenTurn) Custo(usd float64) {
	r.t.mu.Lock()
	r.t.custoUSD = usd
	r.t.mu.Unlock()
}

// currentTitle is the session name: the stored one (/rename, /save) or,
// without it, the user's first message, as OpenCode names the session.
func currentTitle(db *store.Store, sess *runtime.Session) string {
	if db != nil && sess.ID() != "" {
		if se, err := db.GetSession(sess.ID()); err == nil && strings.TrimSpace(se.Title) != "" {
			return strings.TrimSpace(se.Title)
		}
	}
	// The first message comes from the log, not the in-memory history: after
	// a compaction the history starts at the working state, and the title
	// became the next message (e.g. "...").
	if db != nil && sess.ID() != "" {
		if p := strings.Join(strings.Fields(db.PrimeiraFala(sess.ID())), " "); p != "" {
			if r := []rune(p); len(r) > 60 {
				p = string(r[:60]) + "…"
			}
			return p
		}
	}
	return sessionTitle(sess.History())
}

// replayHistory redraws a resumed session's conversation the way a live turn
// draws it: user boxes, per-tool rows built from each call's arguments and its
// result, compaction rules and the answers. Calls come before their results in
// the log; each one is drawn when its result arrives, as live.
func replayHistory(t *screen, h []dealer.Mensagem) {
	calls := map[string]dealer.Mensagem{}
	r := &screenTurn{t: t}
	for _, m := range h {
		switch m.Role {
		case "user":
			switch {
			case isWorkingState(m.Conteudo):
				t.compactionRule()
			case strings.HasPrefix(m.Conteudo, "[subagents]"):
				t.AvisoSubagentes(m.Conteudo)
			case strings.HasPrefix(m.Conteudo, "[output limit]"):
				t.Adicionar("  " + yellowf("⚠") + " " + grayf("response cut at the output limit; continued"))
			case strings.HasPrefix(m.Conteudo, "[tool call as text]"):
				t.Adicionar("  " + yellowf("⚠") + " " + grayf("tool call came as text; asked again"))
			case strings.HasPrefix(m.Conteudo, runtime.UpdatePrefix):
				t.updateLines(m.Conteudo)
			default:
				t.UsuarioCom(strings.TrimSpace(m.Conteudo), m.Images)
			}
		case "tool_call":
			calls[m.ToolID] = m
		case "tool_result":
			t.toolResult(m.ToolName, calls[m.ToolID].ToolArgs, m.Conteudo, m.IsErro)
		case "assistant":
			if strings.TrimSpace(m.Conteudo) != "" {
				r.Resposta(m.Conteudo)
			}
		}
	}
}
