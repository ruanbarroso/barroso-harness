// Keyboard selection in the agents bar: with the prompt box empty, left and
// right move the focus between main, the sub-agents and the shells, Enter opens
// the focused row and Esc (or typing) drops the focus.
package cli

// barKey handles left/right for the bar. It reports whether the key was used;
// any other key, or a non-empty box, drops the focus and falls through to the
// prompt editor.
func (t *screen) barKey(name string) bool {
	if name != "left" && name != "right" {
		t.barFocus = false
		return false
	}
	if t.partial != "" || !t.editor().empty() {
		t.barFocus = false
		return false
	}
	rows := t.agentRows(t.frameFor().agentN)
	if len(rows) == 0 {
		t.barFocus = false
		return false
	}
	if !t.barFocus || t.barSel >= len(rows) {
		t.barFocus = true
		t.barSel = min(1, len(rows)-1)
		if name == "left" {
			t.barSel = len(rows) - 1
		}
		return true
	}
	step := 1
	if name == "left" {
		step = -1
	}
	t.barSel = (t.barSel + step + len(rows)) % len(rows)
	return true
}

// barOpen opens the focused row on Enter. It reports whether Enter was used.
func (t *screen) barOpen() bool {
	if !t.barFocus {
		return false
	}
	t.barFocus = false
	rows := t.agentRows(t.frameFor().agentN)
	if len(rows) == 0 {
		return false
	}
	if t.barSel < 0 || t.barSel >= len(rows) {
		return true
	}
	switch r := rows[t.barSel]; r.kind {
	case rowAgent:
		t.openSubagentDialog(r.id)
	case rowShell:
		t.openShellDialog(r.id)
	}
	return true
}
