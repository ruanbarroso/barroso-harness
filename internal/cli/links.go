// Links in the full screen. The mouse is captured by the app, so the terminal
// never sees a ctrl+click: bh draws every http(s) URL in blue with an
// underline and opens it itself when it is ctrl+clicked.
package cli

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	goruntime "runtime"
	"strings"
)

const underlineOn = "\033[4m"

var reURL = regexp.MustCompile("https?://[^\\s<>\"'`\\x1b]+")

// trimURL splits trailing punctuation off a matched URL: the full stop that
// ends a sentence, or a closing bracket that has no opener inside the URL.
func trimURL(u string) (link, tail string) {
	for len(u) > 0 {
		last := u[len(u)-1]
		switch {
		case strings.IndexByte(".,;:!?", last) >= 0:
		case last == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
		case last == ']' && strings.Count(u, "[") < strings.Count(u, "]"):
		case last == '}' && strings.Count(u, "{") < strings.Count(u, "}"):
		default:
			return u, tail
		}
		tail = u[len(u)-1:] + tail
		u = u[:len(u)-1]
	}
	return u, tail
}

// styleLinks paints every URL of s blue and underlined. restore is written
// after each link, to bring back the colour the surrounding text had.
func styleLinks(s, restore string) string {
	if !colorsOn || !strings.Contains(s, "http") {
		return s
	}
	return reURL.ReplaceAllStringFunc(s, func(match string) string {
		link, tail := trimURL(match)
		if link == "" {
			return match
		}
		return colorBlue + underlineOn + link + reset + restore + tail
	})
}

// urlAt returns the URL that covers the visible column col of a plain line,
// or "" when the cell is not on a link.
func urlAt(line string, col int) string {
	if col < 0 {
		return ""
	}
	for _, loc := range reURL.FindAllStringIndex(line, -1) {
		link, _ := trimURL(line[loc[0]:loc[1]])
		start := len([]rune(line[:loc[0]]))
		if col >= start && col < start+len([]rune(link)) {
			return link
		}
	}
	return ""
}

// openURL opens a link in the default browser. A variable so tests never
// launch one.
var openURL = openInBrowser

func openInBrowser(u string) error {
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		return errors.New("only http and https links are opened")
	}
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}

// openLink opens u and reports the outcome in a toast.
func (t *screen) openLink(u string) {
	if err := openURL(u); err != nil {
		t.toastf("error", fmt.Sprintf("Could not open the link: %v", err))
		return
	}
	t.toastf("info", "Opening "+u)
}

// linkAt returns the URL under a conversation cell, or "".
func (t *screen) linkAt(row, col int) string {
	f := t.frameFor()
	if row < f.convRow {
		return ""
	}
	all := t.wrapped(f.paneCols - 2)
	idx := t.viewTop() + row - f.convRow
	if idx < 0 || idx >= len(all) {
		return ""
	}
	margin := len(conversationMargin(all[idx]))
	return urlAt(stripANSI(all[idx]), col-1-margin)
}

// copyText puts text on the terminal clipboard (OSC 52) and on the OS
// clipboard, and shows the "Copied to clipboard" notice.
func (t *screen) copyText(text string) {
	fmt.Fprint(t.out, osc52(text))
	if clipboardWrite == nil || clipboardWrite(text) != nil {
		t.toastf("success", "Copied to clipboard")
	}
}
