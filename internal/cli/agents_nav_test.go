package cli

import "testing"

func screenWithShell(t *testing.T) (*screen, string) {
	t.Helper()
	noColors(t)
	js, id := startBackgroundShell(t)
	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.Adicionar("hello")
	sc.src = &dialogSource{jobs: js}
	return sc, id
}

func TestBarArrowsMoveFocusAndEnterOpensTheShell(t *testing.T) {
	sc, id := screenWithShell(t)

	if !sc.barKey("right") || !sc.barFocus || sc.barSel != 1 {
		t.Fatalf("right should focus the first row after main: focus=%v sel=%d", sc.barFocus, sc.barSel)
	}
	if !sc.barKey("right") || sc.barSel != 0 {
		t.Fatalf("right at the end should wrap to main, sel=%d", sc.barSel)
	}
	if !sc.barKey("left") || sc.barSel != 1 {
		t.Fatalf("left from main should wrap to the shell, sel=%d", sc.barSel)
	}
	if !sc.barOpen() {
		t.Fatal("enter should be used by the focused row")
	}
	if sc.dlg == nil || sc.dlg.jobID != id {
		t.Fatalf("enter should open the output of %s: %+v", id, sc.dlg)
	}
	if sc.barFocus {
		t.Fatal("opening should drop the focus")
	}
}

func TestBarArrowsLeaveTheCaretAloneWhenTheBoxHasText(t *testing.T) {
	sc, _ := screenWithShell(t)
	sc.editor().Insert("abc")

	if sc.barKey("left") || sc.barFocus {
		t.Fatal("left must move the caret, not the bar, when the box has text")
	}
}

func TestBarFocusDropsWhenTyping(t *testing.T) {
	sc, _ := screenWithShell(t)
	sc.barKey("right")

	if sc.barKey("a") || sc.barFocus {
		t.Fatal("typing should drop the focus and reach the editor")
	}
	if sc.barOpen() {
		t.Fatal("enter without focus belongs to the prompt")
	}
}

func TestBarEnterOnMainOnlyDropsTheFocus(t *testing.T) {
	sc, _ := screenWithShell(t)
	sc.barKey("right")
	sc.barKey("right")

	if !sc.barOpen() || sc.dlg != nil {
		t.Fatalf("enter on main should not open anything: %+v", sc.dlg)
	}
}
