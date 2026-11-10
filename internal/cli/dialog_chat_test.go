package cli

import (
	"strings"
	"testing"
)

func TestTrimURLKeepsPunctuationOutsideTheLink(t *testing.T) {
	cases := map[string][2]string{
		"https://a.com/x.":             {"https://a.com/x", "."},
		"https://a.com/x),":            {"https://a.com/x", "),"},
		"https://a.com/w_(b)":          {"https://a.com/w_(b)", ""},
		"https://a.com/path?q=1&r=2;":  {"https://a.com/path?q=1&r=2", ";"},
		"https://a.com/a_b":            {"https://a.com/a_b", ""},
	}
	for in, want := range cases {
		link, tail := trimURL(in)
		if link != want[0] || tail != want[1] {
			t.Fatalf("trimURL(%q) = (%q, %q), want (%q, %q)", in, link, tail, want[0], want[1])
		}
	}
}

func TestStyleLinksPaintsBlueUnderlinedAndRestoresColor(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	got := styleLinks("see https://example.com/a. ok", colorText)
	want := "see " + colorBlue + underlineOn + "https://example.com/a" + reset + colorText + ". ok"
	if got != want {
		t.Fatalf("styleLinks = %q, want %q", got, want)
	}
}

func TestStyleLinksLeavesTextWithoutColors(t *testing.T) {
	noColors(t)
	in := "see https://example.com/a"
	if got := styleLinks(in, ""); got != in {
		t.Fatalf("with colors off the text must not change: %q", got)
	}
}

func TestRenderMarkdownStylesLinksOfTheAssistant(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	out := renderMarkdown("open https://example.com/docs now", 80)
	if !strings.Contains(out, colorBlue+underlineOn+"https://example.com/docs"+reset) {
		t.Fatalf("assistant link should be blue and underlined: %q", out)
	}
}

func TestURLAtFindsTheLinkUnderTheColumn(t *testing.T) {
	line := "see https://example.com/a. and http://b.org"
	cases := []struct {
		col  int
		want string
	}{
		{0, ""},
		{3, ""},
		{4, "https://example.com/a"},
		{24, "https://example.com/a"},
		{25, ""},
		{31, "http://b.org"},
		{-1, ""},
	}
	for _, c := range cases {
		if got := urlAt(line, c.col); got != c.want {
			t.Fatalf("urlAt(col %d) = %q, want %q", c.col, got, c.want)
		}
	}
}

func TestOpenInBrowserRefusesNonHTTP(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "javascript:alert(1)", "calc.exe"} {
		if err := openInBrowser(u); err == nil {
			t.Fatalf("%q must not be opened", u)
		}
	}
}

func inspectorWithBody(t *testing.T, lines ...string) (*screen, *dialog) {
	t.Helper()
	noColors(t)
	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	d := &dialog{taskID: "t1", text: lines, size: dialogWide}
	d.geo = dialogGeo{left: 10, top: 3, width: 80, bottom: 20, textTop: 5, textRows: 5}
	sc.dlg = d
	return sc, d
}

func TestInspectorTypingGoesToTheMessageField(t *testing.T) {
	sc, d := inspectorWithBody(t, "body")
	for _, key := range []string{"j", "k", "o", "k"} {
		sc.dialogKey(keyEvent{text: key})
	}
	if string(d.input) != "jkok" {
		t.Fatalf("typed keys should reach the field, got %q", string(d.input))
	}
	sc.dialogKey(keyEvent{action: keyBackspace})
	if string(d.input) != "jko" {
		t.Fatalf("backspace should erase one rune, got %q", string(d.input))
	}
	sc.dialogKey(keyEvent{action: "ctrl+u"})
	if len(d.input) != 0 {
		t.Fatalf("ctrl+u should clear the field, got %q", string(d.input))
	}
}

func TestInspectorSendKeepsTheTextWhenTheTaskRefuses(t *testing.T) {
	g, id := managerWithTask(t)
	sc, d := inspectorWithBody(t, "body")
	sc.src = &dialogSource{gerente: g}
	d.taskID = id

	for _, key := range []string{"h", "i"} {
		sc.dialogKey(keyEvent{text: key})
	}
	sc.dialogKey(keyEvent{action: keyEnter})

	if sc.toast == nil || sc.toast.variant != "error" {
		t.Fatalf("a refused message should show an error toast, got %+v", sc.toast)
	}
	if string(d.input) != "hi" {
		t.Fatalf("the text must stay in the field when the send fails, got %q", string(d.input))
	}
	if sc.dlg != d {
		t.Fatal("the inspector must stay open")
	}
}

func TestInspectorCtrlClickOpensTheLink(t *testing.T) {
	sc, _ := inspectorWithBody(t, "see https://example.com/a now")
	var opened []string
	before := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openURL = before })

	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", ctrl: true, row: 5, col: 10 + chatBodyPad + 4})
	if len(opened) != 1 || opened[0] != "https://example.com/a" {
		t.Fatalf("ctrl+click on the link should open it, opened %v", opened)
	}

	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", ctrl: true, row: 5, col: 10 + chatBodyPad})
	if len(opened) != 1 {
		t.Fatalf("ctrl+click away from the link must not open anything, opened %v", opened)
	}
}

func TestInspectorPlainClickOnTheLinkDoesNotOpenIt(t *testing.T) {
	sc, _ := inspectorWithBody(t, "see https://example.com/a now")
	before := openURL
	openURL = func(u string) error { t.Fatalf("plain click opened %s", u); return nil }
	t.Cleanup(func() { openURL = before })

	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: 5, col: 10 + chatBodyPad + 4})
}

func TestInspectorDragSelectsAndCopiesOnRelease(t *testing.T) {
	sc, d := inspectorWithBody(t, "see https://example.com/a now", "second line")
	var copied []string
	before := clipboardWrite
	clipboardWrite = func(s string) error { copied = append(copied, s); return nil }
	t.Cleanup(func() { clipboardWrite = before })

	left := 10 + chatBodyPad
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: 5, col: left})
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedrag", row: 5, col: left + 2})
	if len(copied) != 0 {
		t.Fatalf("nothing should be copied before the button is released: %v", copied)
	}
	sc.dialogMouse(keyEvent{mouse: true, action: "mouseup", row: 5, col: left + 2})

	if len(copied) != 1 || copied[0] != "see" {
		t.Fatalf("release should copy the dragged text, copied %q", copied)
	}
	if !d.tselOn {
		t.Fatal("the selection should stay painted after the copy")
	}
}

func TestInspectorDragAcrossLinesCopiesBoth(t *testing.T) {
	sc, _ := inspectorWithBody(t, "first line", "second line")
	var copied []string
	before := clipboardWrite
	clipboardWrite = func(s string) error { copied = append(copied, s); return nil }
	t.Cleanup(func() { clipboardWrite = before })

	left := 10 + chatBodyPad
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: 5, col: left + 6})
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedrag", row: 6, col: left + 5})
	sc.dialogMouse(keyEvent{mouse: true, action: "mouseup", row: 6, col: left + 5})

	if len(copied) != 1 || copied[0] != "line\nsecond" {
		t.Fatalf("a drag across lines should copy both, copied %q", copied)
	}
}

func TestInspectorClickWithoutDragCopiesNothing(t *testing.T) {
	sc, _ := inspectorWithBody(t, "first line")
	var copied []string
	before := clipboardWrite
	clipboardWrite = func(s string) error { copied = append(copied, s); return nil }
	t.Cleanup(func() { clipboardWrite = before })

	left := 10 + chatBodyPad
	sc.dialogMouse(keyEvent{mouse: true, action: "mousedown", row: 5, col: left + 2})
	sc.dialogMouse(keyEvent{mouse: true, action: "mouseup", row: 5, col: left + 2})

	if len(copied) != 0 {
		t.Fatalf("a click is not a selection, copied %q", copied)
	}
}

func TestDialogTextLinePaintsLinksBlueUnderlined(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	d := &dialog{taskID: "t1"}
	got := dialogTextLine(d, 0, "go to https://example.com/a", 80)
	if !strings.Contains(got, colorBlue+underlineOn+"https://example.com/a"+reset) {
		t.Fatalf("link in the inspector should be blue and underlined: %q", got)
	}
}
