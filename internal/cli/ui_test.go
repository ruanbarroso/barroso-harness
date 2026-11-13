package cli

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// TestBasicTUITurnGlyphsAndPalette: with colors on, the TUI turn facade emits
// glyph and color together; without colors, turnRenderer returns the plain
// text runtime_test pins via Turno.
func TestBasicTUITurnGlyphsAndPalette(t *testing.T) {
	prevColors := colorsOn
	colorsOn = true
	defer func() { colorsOn = prevColors }()

	var sb strings.Builder
	tui := basicTUITurn{out: &sb}
	tui.Tool("shell", 120*time.Millisecond, false)
	tui.Eco("exit_code: 0")
	tui.Guardrail(false)
	tui.Tokens(1200, 45, 890*time.Millisecond, true)
	output := sb.String()

	for _, want := range []string{
		glyphTool, "shell", "120ms", paletteMagenta,
		glyphGuard, "tokens ↑1200 ↓45", "890ms", "compacted",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("TUI output missing %q:\n%q", want, output)
		}
	}
}

func TestPlainHasNoEscapes(t *testing.T) {
	// The plain renderer goes quiet when BH_FINAL_ONLY=1 is exported; this test
	// needs the default tool log.
	t.Setenv(runtime.EnvSoResposta, "")
	prevColors := colorsOn
	colorsOn = false
	defer func() { colorsOn = prevColors }()

	var sb strings.Builder
	turnRenderer(&sb).Tool("shell", 120*time.Millisecond, false)
	if !strings.Contains(sb.String(), "[tool shell ok in 120ms]") {
		t.Fatalf("plain mode lost the runtime_test literal:\n%q", sb.String())
	}
	if strings.Contains(sb.String(), "\033") {
		t.Fatalf("stray ANSI escape in plain mode:\n%q", sb.String())
	}
}

// TestListModelsComplete: the whole catalog comes out in one list, with no
// paging, and the window column stays aligned on the current model's line
// too (bold must not count towards the padding).
func TestListModelsComplete(t *testing.T) {
	prevColors := colorsOn
	defer func() { colorsOn = prevColors }()

	ls := make([]dealer.CatalogModel, 0, 120)
	for i := 0; i < 120; i++ {
		ls = append(ls, dealer.CatalogModel{ID: fmt.Sprintf("m-%03d", i), Input: []string{"text"}, ContextLength: 250000})
	}
	ls = append(ls, dealer.CatalogModel{ID: "z-long-model", ContextLength: 1048000})

	colorsOn = false
	var sb strings.Builder
	listModels(&sb, ls, "m-050")
	output := sb.String()
	for _, want := range []string{"121 available", "  1 m-000", " 51 m-050", "120 m-119", "121 z-long-model"} {
		if !strings.Contains(output, want) {
			t.Fatalf("list missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "page") || strings.Contains(output, "pg<") {
		t.Fatalf("there should be no paging:\n%s", output)
	}

	// With colors, the window column starts at the same visible position
	// on the current model's line and on a regular line.
	colorsOn = true
	sb.Reset()
	listModels(&sb, ls, "m-050")
	ansiRe := regexp.MustCompile("" + `\[[0-9;]*m`)
	col := func(id string) int {
		for _, l := range strings.Split(sb.String(), "\n") {
			l = ansiRe.ReplaceAllString(l, "")
			if strings.Contains(l, id+" ") {
				// Column in CHARACTERS, not bytes: the current model's ▸ is 3
				// bytes and takes one column, like a space.
				if k := strings.Index(l, "250k"); k >= 0 {
					return utf8.RuneCountInString(l[:k])
				}
				return -1
			}
		}
		return -1
	}
	if a, b := col("m-050"), col("m-051"); a < 0 || a != b {
		t.Fatalf("column misaligned on the current model: %d vs %d", a, b)
	}
}

// TestColorsOffNoEscape: colorsOn=false strips every escape and errorFmt
// keeps a single line (the protocol detail is dropped).
func TestColorsOffNoEscape(t *testing.T) {
	prevColors := colorsOn
	defer func() { colorsOn = prevColors }()
	colorsOn = false
	for _, f := range []func(string) string{greenf, redf, yellowf, cyanf, dimf, magentaf, purplef, boldf} {
		if strings.ContainsRune(f("x"), 0x1b) {
			t.Fatal("color emitted an escape with colorsOn=false")
		}
	}
	var sb strings.Builder
	errorFmt(&sb, "error saving", errors.New("disk full\nprotocol detail"))
	output := sb.String()
	if !strings.Contains(output, "disk full") || strings.ContainsRune(output, 0x1b) {
		t.Fatalf("errorFmt has an escape or lacks the message: %q", output)
	}
	if strings.Contains(output, "protocol detail") {
		t.Fatalf("errorFmt should drop the following lines: %q", output)
	}
}
