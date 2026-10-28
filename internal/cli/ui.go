package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/pricing"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"golang.org/x/term"
)

// REPL presentation layer, in the spirit of OpenCode: ANSI-256 palette,
// UTF-8 glyphs and a live status line; no TUI framework (one binary, no third
// parties). No text checked by a test has an escape in the middle — color
// wraps the whole line, and colorsOn is only enabled when both Out AND ErrOut
// are real terminals; pipes, redirections and test buffers (which are not
// *os.File) get plain text.
const (
	reset = "\033[0m"
	bold  = "\033[1m"
)

// Low-key ANSI-256 palette — except the title cyan and the green/red.
const (
	colorDim     = "\033[38;5;244m"
	colorCyan    = "\033[38;5;38m"
	colorGreen   = "\033[38;5;114m"
	colorRed     = "\033[38;5;203m"
	colorYellow  = "\033[38;5;221m"
	colorMagenta = "\033[38;5;176m"
	colorPurple  = "\033[38;5;99m"
)

// Glyphs: modern terminal fonts (Windows Terminal, conhost on 10/11, macOS,
// Linux) cover them all; with colors off they still work as visual cues.
const (
	glyphOk      = "✓"
	glyphErr     = "✗"
	glyphWarn    = "⚠"
	glyphTool    = "▶"
	glyphGuard   = "◆"
	glyphCurrent = "▸"
)

// paletteMagenta is exposed so the test can assert the palette was emitted
// (color() itself returns plain text with colorsOn=false, so the test turns
// colors on and looks for this raw constant).
const paletteMagenta = colorMagenta

// colorsOn decides whether the palette is emitted. Starts off; enableColors
// sets it at the start of each RunInteractive.
var colorsOn = false

func color(ansi, s string) string {
	if !colorsOn || ansi == "" {
		return s
	}
	return ansi + s + reset
}

func dimf(s string) string     { return color(colorDim, s) }
func cyanf(s string) string    { return color(colorCyan, s) }
func greenf(s string) string   { return color(colorGreen, s) }
func redf(s string) string     { return color(colorRed, s) }
func yellowf(s string) string  { return color(colorYellow, s) }
func magentaf(s string) string { return color(colorMagenta, s) }
func purplef(s string) string  { return color(colorPurple, s) }
func boldf(s string) string    { return color(bold, s) }

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

// enableColors turns the palette on only with Out and ErrOut on a terminal
// (x/term, already a dependency for the hidden key), honouring NO_COLOR and TERM=dumb.
func enableColors(io IO) {
	colorsOn = isTerminal(io.Out) && isTerminal(io.ErrOut) &&
		os.Getenv("NO_COLOR") == "" &&
		os.Getenv("TERM") != "dumb"
}

// banner is the opening strip — clean and stable, in the spirit of OpenCode.
// Volatile state lives in the statusbar; the brand is not repeated every turn.
func banner(out io.Writer) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, cyanf("╭─ ")+boldf(cyanf("Barroso Harness")))
	fmt.Fprintln(out, cyanf("╰─ ")+dimf("type /help for commands · plain text goes to the model"))
	fmt.Fprintln(out)
}

// statusbar prints a compact strip before each prompt. It replaces the raw
// Config.Resumo format and makes /model, /effort and /provider changes
// immediately visible without pushing an unnecessary block of text.
func statusbar(out io.Writer, cfg *config.Config) {
	provider := cfg.CurrentProvider
	if provider == "" {
		provider = "no-provider"
	}
	model := cfg.Model
	if model == "" {
		model = "select a model"
	}
	line := provider + " · " + model
	if cfg.Effort != "" {
		line += " · effort " + cfg.Effort
	} else {
		line += " · effort off"
	}
	if cfg.Janela > 0 {
		line += fmt.Sprintf(" · %dk", cfg.Janela/1000)
	}
	fmt.Fprintln(out, dimf("╭─ "+line))
}

// printPrompt is the input line: OpenCode's chevron, the model highlighted
// and a discreet effort. Ends with a space so the cursor sits right after it.
func printPrompt(out io.Writer, model, effort string) {
	mod := model
	if mod == "" {
		mod = "barroso"
	}
	fmt.Fprint(out, greenf("❯")+" "+boldf(mod))
	if effort != "" {
		fmt.Fprint(out, " "+yellowf("·"+effort))
	}
	fmt.Fprint(out, " ")
}

// farewell exits cleanly.
func farewell(out io.Writer) {
	if colorsOn {
		// Guarantee: the cursor is never left hidden after bh exits, even if
		// the spinner was interrupted midway.
		fmt.Fprint(out, showCursor)
	}
	// A rule above and below sets the goodbye apart from the session and from
	// the shell prompt that comes back right after it.
	rule := dimf(strings.Repeat("─", textWidth()))
	fmt.Fprintln(out, rule)
	fmt.Fprintln(out, dimf("bye"))
	fmt.Fprintln(out, rule)
}

// errorFmt prints an error in red, on one line, with no raw markup.
func errorFmt(out io.Writer, prefix string, err error) {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i] // an error is one line; the rest is protocol detail
	}
	indent := ""
	if colorsOn {
		indent = "  " // aligned with the turn lines in the TUI
	}
	fmt.Fprintf(out, "%s%s %s\n", indent, redf(glyphErr), redf(prefix+": "+msg))
}

// okFmt prints a confirmation in green.
func okFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", greenf(glyphOk), msg)
}

// warnFmt prints a warning in yellow.
func warnFmt(out io.Writer, msg string) {
	fmt.Fprintf(out, "%s %s\n", yellowf(glyphWarn), msg)
}

// listModels shows the WHOLE catalog, alphabetically, numbered (the same
// number the selection accepts), with the window column aligned and the
// current model marked with ▸. No paging: the terminal scrolls, and the full
// list is what lets you spot a model by eye.
func listModels(out io.Writer, ls []dealer.CatalogModel, current string) {
	if len(ls) == 0 {
		fmt.Fprintln(out, yellowf("empty catalog"))
		return
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, cyanf(fmt.Sprintf("Models  %d available", len(ls))))
	fmt.Fprintln(out, dimf("────────────────────────────────────────────────────────────"))
	width := 0
	for _, m := range ls {
		if n := utf8.RuneCountInString(m.ID); n > width {
			width = n
		}
	}
	if width > 44 {
		width = 44
	}
	for i, m := range ls {
		mark := "  "
		// Padding is computed on the RAW id: applying the width after
		// coloring would count the ANSI escape bytes and misalign the column
		// exactly on the current model's line.
		pad := strings.Repeat(" ", max(0, width-utf8.RuneCountInString(m.ID)))
		name := m.ID
		if m.ID == current {
			mark = greenf("▸ ")
			name = boldf(m.ID)
		}
		extra := ""
		if m.ContextLength > 0 {
			extra = dimf(fmt.Sprintf("  %dk", m.ContextLength/1000))
		}
		fmt.Fprintf(out, "%s%s %s%s%s\n", mark, dimf(fmt.Sprintf("%3d", i+1)), name, pad, extra)
	}
	hint := "type the number or id"
	if current != "" {
		hint += " · empty Enter keeps " + current
	}
	fmt.Fprintln(out, dimf(hint))
}

// basicTUITurn is the TUI dress of a turn: ▶ glyph with the tool name in
// magenta, dim duration, echo indented by 2, ◆ guardrail and a token footer.
type basicTUITurn struct {
	out io.Writer
	// custoUSD is the turn's money cost (only set when the model has a declared price).
	custoUSD float64
}

func (t basicTUITurn) Resposta(text string) { fmt.Fprintln(t.out, text) }

func (t basicTUITurn) ProtocoloAlternativo(from, to string) {
	fmt.Fprintf(t.out, "%s %s\n", yellowf(glyphWarn), dimf(fmt.Sprintf("%s endpoint unavailable; using %s", from, to)))
}

func (t basicTUITurn) Tool(name string, dur time.Duration, failed bool) {
	state, colorize := "ok", greenf
	if failed {
		state, colorize = "error", redf
	}
	fmt.Fprintf(t.out, "%s %s %s %s\n",
		cyanf(glyphTool), magentaf(name),
		dimf(dur.Round(time.Millisecond).String()), colorize(state))
}

func (t basicTUITurn) Eco(output string) {
	if output == "" {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		fmt.Fprintln(t.out, dimf("  "+line))
	}
}

func (t basicTUITurn) Guardrail(proactive bool) {
	msg := "provider rejected the request size; reactive compaction"
	if proactive {
		msg = "proactive compaction (predicted overflow)"
	}
	fmt.Fprintf(t.out, "%s %s\n", purplef(glyphGuard), dimf(msg))
}

func (t basicTUITurn) Tokens(prompt, output int, dur time.Duration, compacted bool) {
	if prompt <= 0 && output <= 0 && t.custoUSD <= 0 {
		return
	}
	line := fmt.Sprintf("tokens ↑%d ↓%d · %s", prompt, output, dur.Round(time.Millisecond))
	if compacted {
		line += " · compacted"
	}
	// Only when the model has a DECLARED price (prices.json): never a
	// guessed number, and tokens stay the metric otherwise.
	if texto := pricing.Formato(t.custoUSD); texto != "" {
		line += " · " + texto
	}
	fmt.Fprintln(t.out, dimf(line))
}

// Custo receives the turn's money cost — only when the active model has a
// declared price. No call = nothing to show.
func (t *basicTUITurn) Custo(usd float64) { t.custoUSD = usd }

// turnRenderer picks the facade: glyphs+palette when the session turned
// colors on (real terminal), plain text otherwise — including test buffers,
// which then see the same literal line runtime_test pins in plain mode.
func turnRenderer(out io.Writer) runtime.Renderer {
	if colorsOn {
		return &basicTUITurn{out: out}
	}
	return runtime.RendererDe(out)
}

// maxTextWidth caps the answer's line length: on a wide window a paragraph
// running edge to edge is hard to read.
const maxTextWidth = 100

// textWidth is the width the answer wraps at: the terminal, up to maxTextWidth.
func textWidth() int { return min(terminalWidth(), maxTextWidth) }

// terminalWidth returns stdout's columns; 100 when unknown.
func terminalWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 20 {
		return w
	}
	return 100
}
