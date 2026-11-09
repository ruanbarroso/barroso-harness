package cli

import (
	"strings"
	"testing"
)

// linkAt is the hit-test behind ctrl+click in the main conversation: a cell on
// a link returns it, anything else returns "".
func TestLinkAtFindsTheConversationLinkUnderTheCell(t *testing.T) {
	noColors(t)
	sc, _ := newTestScreen(t, 140, 30, panelState{model: "m"})
	sc.Adicionar("see https://example.com/a now")

	const url = "https://example.com/a"
	f := sc.frameFor()
	// Locate the line on screen the same way linkAt does, from the wrapped
	// transcript, so the test does not depend on how the pane pads or aligns.
	all := sc.wrapped(f.paneCols - 2)
	idx, start := -1, -1
	for i, l := range all {
		plain := stripANSI(l)
		if at := strings.Index(plain, url); at >= 0 {
			idx, start = i, len([]rune(plain[:at]))
			break
		}
	}
	if idx < 0 {
		t.Fatalf("the URL is not in the wrapped transcript: %q", all)
	}
	row := f.convRow + idx - sc.viewTop()
	left := 1 + len(conversationMargin(all[idx])) + start // screen column of the first URL rune
	n := len([]rune(url))
	cases := []struct {
		col  int
		want string
	}{
		{left - 1, ""},      // the space before the URL
		{left, url},         // first rune
		{left + n - 1, url}, // last rune
		{left + n, ""},      // just past it
	}
	for _, c := range cases {
		if got := sc.linkAt(row, c.col); got != c.want {
			t.Fatalf("linkAt(row %d, col %d) = %q, want %q", row, c.col, got, c.want)
		}
	}
	// Above the transcript, and on a row with no text, there is no link.
	if got := sc.linkAt(f.convRow-1, left); got != "" {
		t.Fatalf("linkAt above the transcript = %q, want \"\"", got)
	}
	if got := sc.linkAt(f.convRow+len(all)+5, left); got != "" {
		t.Fatalf("linkAt below the last line = %q, want \"\"", got)
	}
}

// openLink reports the outcome in a toast: success opens, failure explains.
func TestOpenLinkReportsTheOutcome(t *testing.T) {
	noColors(t)
	sc, _ := newTestScreen(t, 140, 30, panelState{model: "m"})

	before := openURL
	t.Cleanup(func() { openURL = before })

	openURL = func(u string) error { return nil }
	sc.openLink("https://example.com/a")
	if sc.toast == nil || sc.toast.variant != "info" || !strings.Contains(sc.toast.text, "https://example.com/a") {
		t.Fatalf("a successful open should toast the link, got %+v", sc.toast)
	}

	openURL = func(u string) error { return errFake("no browser") }
	sc.openLink("https://example.com/a")
	if sc.toast == nil || sc.toast.variant != "error" || !strings.Contains(sc.toast.text, "no browser") {
		t.Fatalf("a failed open should toast the error, got %+v", sc.toast)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// spawnSummary is the launcher's argument in a few words.
func TestSpawnSummary(t *testing.T) {
	cases := map[string]string{
		`{"prompt":"one thing"}`:                "one thing",
		`{"prompts":["a"]}`:                     "a",
		`{"prompts":["a","b","c"]}`:             "3 sub-agents",
		`{}`:                                    "",
	}
	for in, want := range cases {
		if got := spawnSummary(in); got != want {
			t.Fatalf("spawnSummary(%s) = %q, want %q", in, got, want)
		}
	}
}
