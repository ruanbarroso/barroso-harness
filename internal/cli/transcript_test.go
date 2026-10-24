// Transcript tests: the OpenCode rows bh's real events turn into, and the
// item identity that keeps clicks pointing at the right thing.
package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func clickItem(t *testing.T, sc *screen, kind string) *item {
	t.Helper()
	sc.frame()
	for _, r := range sc.hits.regions {
		if it := sc.items[r.itemID]; it != nil && it.kind == kind && r.action != nil {
			r.action()
			return it
		}
	}
	t.Fatalf("no clickable %s row", kind)
	return nil
}

// A shell call is a "$ command" block keeping the TAIL of its output, with
// "(N earlier lines)"; a click shows everything.
func TestShellBlockCollapse(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 60, panelState{model: "m"})
	var out []string
	for i := 1; i <= 30; i++ {
		out = append(out, fmt.Sprintf("out %d", i))
	}
	sc.toolResult("shell", `{"command":"make test"}`, "exit_code: 0\nstdout+stderr:\n"+strings.Join(out, "\n"), false)
	txt := sc.transcriptText()
	if !strings.Contains(txt, "$ make test") || !strings.Contains(txt, "out 30") || strings.Contains(txt, "out 5\n") {
		t.Fatalf("collapsed:\n%s", txt)
	}
	if !strings.Contains(txt, "earlier lines)") {
		t.Fatalf("missing the dropped-lines count:\n%s", txt)
	}
	clickItem(t, sc, "shell")
	if txt := sc.transcriptText(); !strings.Contains(txt, "out 1\n") {
		t.Fatalf("expanded:\n%s", txt)
	}
	// A failing command names the exit code; a short output is not clickable.
	sc.toolResult("shell", `{"command":"false"}`, "exit_code: 2\nstdout+stderr:\n", false)
	if !strings.Contains(sc.transcriptText(), "Command exited with code 2") {
		t.Fatal("exit code line")
	}
}

// Consecutive reads/searches collapse into "→ Explored: …", expandable; a lone
// one draws as itself.
func TestExplorationGroup(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.toolResult("read", `{"path":"a.go"}`, "package a", false)
	if txt := sc.transcriptText(); !strings.Contains(txt, "→ Read a.go") || strings.Contains(txt, "package a") {
		t.Fatalf("single read (no output shown):\n%s", txt)
	}
	sc.toolResult("grep", `{"pattern":"func"}`, "a.go:1:func x\nb.go:2:func y", false)
	sc.toolResult("read", `{"path":"b.go","offset":10,"limit":5}`, "x", false)
	txt := sc.transcriptText()
	if !strings.Contains(txt, "→ Explored: 2 reads, 1 search") {
		t.Fatalf("group:\n%s", txt)
	}
	clickItem(t, sc, "group")
	txt = sc.transcriptText()
	if !strings.Contains(txt, `✱ Grep "func" (2 matches)`) || !strings.Contains(txt, "→ Read b.go:10-14") {
		t.Fatalf("expanded group:\n%s", txt)
	}
	// A non-exploration tool ends the group.
	sc.toolResult("edit", `{"path":"a.go","oldString":"x","newString":"y"}`, "ok: 1 replacement(s) in a.go", false)
	sc.toolResult("read", `{"path":"c.go"}`, "c", false)
	if n := strings.Count(sc.transcriptText(), "Explored"); n != 1 {
		t.Fatalf("a new run starts a new group: %d", n)
	}
}

// Expanding one Thought must not change which item the other rows belong to
// (the old line-index keying broke exactly here).
func TestThoughtStableIdentity(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 60, panelState{model: "m"})
	sc.thought(time.Second, "**One**\n\nfirst body\nmore")
	sc.thought(2*time.Second, "**Two**\n\nsecond body")
	var first, second *item
	for _, it := range sc.items {
		if it.title == "One" {
			first = it
		}
		if it.title == "Two" {
			second = it
		}
	}
	sc.mu.Lock()
	sc.toggle(first)
	sc.mu.Unlock()
	sc.frame()
	// Click the row that now shows "Two": it must open the second Thought.
	rows := visualize(sc.frame(), 100, 60)
	for _, r := range sc.hits.regions {
		if strings.Contains(rowAt(rows, r.row), "Thought: Two") && r.action != nil {
			r.action()
		}
	}
	if !second.open || !first.open {
		t.Fatalf("first open=%v second open=%v", first.open, second.open)
	}
	if txt := sc.transcriptText(); !strings.Contains(txt, "first body") || !strings.Contains(txt, "second body") {
		t.Fatalf("bodies:\n%s", txt)
	}
}

// An edit draws the diff from its arguments; a write the file with numbers.
func TestEditAndWriteBlocks(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.toolResult("edit", `{"path":"x.go","oldString":"old line","newString":"new line"}`, "ok", false)
	sc.toolResult("write", `{"path":"y.txt","content":"l1\nl2"}`, "ok", false)
	txt := sc.transcriptText()
	for _, want := range []string{"← Edit x.go", "- old line", "+ new line", "# Wrote y.txt", "  1 l1", "  2 l2"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q:\n%s", want, txt)
		}
	}
}

// A failed inline tool is red and its error shows on click.
func TestFailedToolExpandsError(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.toolResult("webfetch", `{"url":"https://x"}`, "error: 404 not found", true)
	if strings.Contains(sc.transcriptText(), "404") {
		t.Fatal("the error is behind a click")
	}
	clickItem(t, sc, "group")
	if !strings.Contains(sc.transcriptText(), "404 not found") {
		t.Fatalf("expanded:\n%s", sc.transcriptText())
	}
}

// Finished sub-agents get one row each; a click asks for /tasks <id>.
func TestSubagentRows(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.AvisoSubagentes("[subagents] Sub-agents finished:\n\n## t1 completed (11s)\nok\n\n## t2 failed (3s)\nerror: x\n")
	txt := sc.transcriptText()
	if !strings.Contains(txt, "↳ Subagent finished · t1 · 11s") || !strings.Contains(txt, "! Subagent failed · t2") {
		t.Fatalf("rows:\n%s", txt)
	}
	clickItem(t, sc, "sub")
	if sc.pendingCmd != "/tasks t1" {
		t.Fatalf("pending = %q", sc.pendingCmd)
	}
}

// Compaction is a centered rule; an interrupted turn says so on its footer and a
// failed one gets an Error line.
func TestCompactionAndTurnEnd(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.compactionRule()
	sc.TurnoFim("kimi", 3*time.Second, true, errors.New("context canceled"))
	sc.TurnoFim("kimi", time.Second, false, errors.New("provider 500"))
	txt := sc.transcriptText()
	for _, want := range []string{"── Compaction ──", "kimi · 3.0s · interrupted", "Error: provider 500"} {
		if !strings.Contains(txt, want) {
			t.Errorf("missing %q:\n%s", want, txt)
		}
	}
	if strings.Contains(txt, "Error: context canceled") {
		t.Error("an interrupt is not an error")
	}
}

// The user box shows images as "file" chips, not the [Image N] text.
func TestUserAttachmentChips(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	// The chip shows the base name whichever OS wrote the path.
	for _, p := range []string{`C:\tmp\shot.png`, "/tmp/shot.png", "shot.png"} {
		sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
		sc.UsuarioCom("look [Image 1]", []string{p})
		txt := sc.transcriptText()
		if !strings.Contains(txt, "look") || strings.Contains(txt, "[Image 1]") || !strings.Contains(txt, " file  shot.png") {
			t.Fatalf("%s: box:\n%s", p, txt)
		}
	}
}

// A command's Windows output (CRLF) is painted inside the shell block: a stray
// \r would send the cursor back to column 1 and the block background would
// paint over the line (the date of a Get-Date call disappeared that way).
func TestShellBlockCRLFOutput(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()
	sc, sb := newTestScreen(t, 120, 30, panelState{model: "m"})
	sc.Usuario("que horas são")
	sc.toolResult("shell", `{"command":"Get-Date"}`,
		"exit_code: 0\r\nstdout+stderr:\r\n2026-10-20 02:01:34\r\n", false)
	sb.Reset()
	sc.mu.Lock()
	sc.w = 0
	sc.drawLocked()
	sc.mu.Unlock()
	if strings.Contains(sb.String(), "\r") {
		t.Fatal("a carriage return reached the frame")
	}
	if all := strings.Join(visualize(sb.String(), 120, 30), "\n"); !strings.Contains(all, "2026-10-20 02:01:34") {
		t.Fatalf("output line missing:\n%s", all)
	}
}

// A started shell call shows its command (no "$ ") with a spinner and its
// live output; the result completes the SAME block as "$ command".
func TestRunningShellBlockBecomesResult(t *testing.T) {
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.toolStarted("0/c0", "shell", `{"command":"make test"}`)
	txt := sc.transcriptText()
	if !strings.Contains(txt, "make test") || strings.Contains(txt, "$ make test") {
		t.Fatalf("running block:\n%s", txt)
	}
	sc.toolOutput("0/c0", "building\r\n")
	if !strings.Contains(sc.transcriptText(), "building") {
		t.Fatalf("live output missing:\n%s", sc.transcriptText())
	}
	sc.toolFinished("0/c0", "shell", `{"command":"make test"}`, "exit_code: 0\nstdout+stderr:\nok all", false)
	txt = sc.transcriptText()
	if !strings.Contains(txt, "$ make test") || !strings.Contains(txt, "ok all") || strings.Contains(txt, "building") {
		t.Fatalf("finished block:\n%s", txt)
	}
	if n := strings.Count(txt, "make test"); n != 1 {
		t.Fatalf("the result must complete the running block, not add another (%d):\n%s", n, txt)
	}
}

// An inline tool started shows its label with a spinner instead of ✓, and a
// turn that ends without the result stops spinning.
func TestRunningInlineToolStopsWithTurn(t *testing.T) {
	sc, _ := newTestScreen(t, 100, 40, panelState{model: "m"})
	sc.Ocupado(true)
	sc.toolStarted("0/c0", "mystery", `{"x":1}`)
	if strings.Contains(sc.transcriptText(), "✓ mystery") {
		t.Fatalf("a running call must not show ✓:\n%s", sc.transcriptText())
	}
	sc.Ocupado(false)
	sc.mu.Lock()
	n := len(sc.runningTools)
	sc.mu.Unlock()
	if n != 0 {
		t.Fatal("running calls must stop when the turn ends")
	}
}
