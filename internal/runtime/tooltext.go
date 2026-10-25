package runtime

import (
	"regexp"
	"strings"
)

// A tool call written out as text instead of called (Bruce, session
// bh-20261024223743-5c5f, 2026-10-25): the gateway served step-5-preview for
// claude-opus-5-5 and two turns ended with finish=stop, zero tool calls and
// the whole `(tool_call) name=okf_create arguments={...}` in the content. The
// screen showed it as the answer and nothing ran. The same shape hit Greg on
// 2026-10-23 ("tool_call name=context_read argument{}").
//
// Detection needs BOTH a call marker and a structural ending (closing brace or
// closing tag), so an answer that merely quotes a call mid-prose and then
// keeps talking is not caught.
var (
	toolTextMarker = regexp.MustCompile(`(?i)(\(tool_call\)\s*name\s*=|\btool_call\s+name\s*=|<tool_call>|<function\s*=|<invoke\s+name\s*=|<\|tool_call)`)
	toolTextEnd    = regexp.MustCompile("(\\}|</tool_call>|</function>|</invoke>|</parameter>|<\\|tool_call_end\\|>|\\])\\s*(```)?\\s*$")
)

// looksLikeToolText reports whether a final round's text is a tool call that
// leaked into the content.
func looksLikeToolText(text string) bool {
	t := strings.TrimSpace(text)
	if t == "" || !toolTextEnd.MatchString(t) {
		return false
	}
	return toolTextMarker.MatchString(t)
}

const (
	// toolTextNotice is appended (as a user message) after a leaked call. The
	// "[tool call as text]" prefix lets the screen and the session title skip
	// it, like "[output limit]".
	toolTextNotice = "[tool call as text] Your last response wrote a tool call out as plain text instead of calling the tool, so NOTHING was executed. Call it now as a real tool call (for deferred tools: tool_call with {\"name\": ..., \"arguments\": {...}} — the arguments object is required). Do not write the call as text and do not apologize."
	// maxToolTextRetries bounds the re-asks in one turn; after that the text is
	// delivered as the answer, as before.
	maxToolTextRetries = 2
)
