// Chat with a sub-agent from its inspector: the text typed in the dialog goes
// to the task (Gerente.Mensagem), the body can be selected with the mouse
// (copied on release) and links in it open with ctrl+click.
package cli

import "strings"

const chatBodyPad = 3

// dialogTextRows is how many rows a text body shows.
func (t *screen) dialogTextRows(_ *dialog) int { return max(3, t.h/2-6) }

// chatKey handles a key in the sub-agent inspector. Printable keys are typed
// into the message field; the arrows and page keys still scroll the body.
func (t *screen) chatKey(d *dialog, k keyEvent, name string) string {
	switch name {
	case keyUp, "ctrl+p":
		d.off = max(0, d.off-1)
	case keyDown, "ctrl+n":
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
		t.sendChat(d)
	case keyBackspace:
		if n := len(d.input); n > 0 {
			d.input = d.input[:n-1]
		}
	case "ctrl+u", "ctrl+w":
		d.input = nil
	case "paste":
		d.input = append(d.input, []rune(pasteText(k.text))...)
	default:
		if k.action == "" && k.text != "" {
			d.input = append(d.input, []rune(k.text)...)
		}
	}
	return ""
}

// sendChat delivers the typed message to the inspected sub-agent.
func (t *screen) sendChat(d *dialog) {
	text := strings.TrimSpace(string(d.input))
	if text == "" || t.src == nil || t.src.gerente == nil {
		return
	}
	if err := t.src.gerente.Mensagem(d.taskID, text); err != nil {
		t.toastf("error", err.Error())
		return
	}
	d.input = nil
	d.tselOn = false
	d.off = 1 << 30
}

// textCell maps a screen cell to a line of the text body and a column inside
// it. ok is false when the cell is not on the body.
func (d *dialog) textCell(row, col int) (line, column int, ok bool) {
	g := d.geo
	if g.textRows == 0 || row < g.textTop || row >= g.textTop+g.textRows {
		return 0, 0, false
	}
	line = d.off + row - g.textTop
	column = col - (g.left + chatBodyPad)
	if line < 0 || line >= len(d.text) || column < 0 {
		return 0, 0, false
	}
	return line, column, true
}

// chatMouse handles the mouse over the inspector's body. handled is false when
// the event is not for the body and the generic dialog code should run.
func (t *screen) chatMouse(d *dialog, k keyEvent) (repaint, handled bool) {
	switch k.action {
	case "mousedown":
		d.tselOn, d.tselDrag = false, false
		line, col, ok := d.textCell(k.row, k.col)
		if !ok {
			return false, false
		}
		if k.ctrl {
			if link := urlAt(d.text[line], col); link != "" {
				t.openLink(link)
				return true, true
			}
		}
		d.tsel = selRange{from: selPoint{line, col}, to: selPoint{line, col}}
		d.tselDrag = true
		return true, true
	case "mousedrag":
		if !d.tselDrag {
			return false, false
		}
		g := d.geo
		row := clampInt(k.row, g.textTop, g.textTop+g.textRows-1)
		line := clampInt(d.off+row-g.textTop, 0, max(0, len(d.text)-1))
		col := max(0, k.col-(g.left+chatBodyPad))
		to := selPoint{line, col}
		if line > d.tsel.from.line || line == d.tsel.from.line && col >= d.tsel.from.col {
			to.col++
		}
		d.tsel.to = to
		d.tselOn = true
		return true, true
	case "mouseup":
		if !d.tselDrag {
			return false, false
		}
		d.tselDrag = false
		if d.tselOn {
			if text := extractSelection(d.text, d.tsel); text != "" {
				t.copyText(text)
			}
		}
		return true, true
	}
	return false, false
}

// dialogTextLine paints one line of a text body: the selected span inverted,
// links in blue and underlined.
func dialogTextLine(d *dialog, idx int, l string, inner int) string {
	plain := truncateVis(l, inner)
	if d.tselOn {
		n := d.tsel.normalize()
		if idx >= n.from.line && idx <= n.to.line {
			runes := []rune(plain)
			from, to := 0, len(runes)
			if idx == n.from.line {
				from = clampInt(n.from.col, 0, len(runes))
			}
			if idx == n.to.line {
				to = clampInt(n.to.col, from, len(runes))
			}
			if from < to {
				return color(colorText, string(runes[:from])) +
					bgSelection + fgSelected + string(runes[from:to]) + reset +
					color(colorText, string(runes[to:]))
			}
		}
	}
	return color(colorText, styleLinks(plain, colorText))
}

// chatField paints the message field of the inspector and returns the cursor
// column offset inside it.
func chatField(d *dialog, inner int) (string, int) {
	room := max(1, inner-2)
	if len(d.input) == 0 {
		return color(fgAccent, "› ") + grayf(truncateVis("Message this sub-agent · tab switches agent", room)), 2
	}
	shown := d.input
	if len(shown) > room {
		shown = shown[len(shown)-room:]
	}
	return color(fgAccent, "› ") + color(colorText, string(shown)), 2 + len(shown)
}
