package runtime

import (
	"strings"
	"testing"
)

func TestHookContextClaudeCodeShape(t *testing.T) {
	out := &hookBuffer{max: promptHookStdout}
	out.Write([]byte(`{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"<memoria>x</memoria>"}}`))
	if got := parseHookOutput(out).Context; got != "<memoria>x</memoria>" {
		t.Fatalf("got %q", got)
	}
}

func TestHookContextPlainAndEmpty(t *testing.T) {
	out := &hookBuffer{max: promptHookStdout}
	out.Write([]byte("  texto puro \n"))
	if got := parseHookOutput(out).Context; got != "texto puro" {
		t.Fatalf("got %q", got)
	}
	if got := parseHookOutput(&hookBuffer{max: promptHookStdout}).Context; got != "" {
		t.Fatalf("empty output gave %q", got)
	}
}

// A context above the cap is cut without breaking a rune: the prefix has to be
// valid UTF-8, or the request carries a bad byte that renders as "?".
func TestHookContextCutKeepsRunesWhole(t *testing.T) {
	out := &hookBuffer{max: 5}
	out.Write([]byte("😀éx")) // the cap lands after the first byte of "é"
	if got := out.String(); got != "😀" {
		t.Fatalf("cut left a partial rune: %q", got)
	}
	if got := truncatePrefix("😀éx", 5); got != "😀" {
		t.Fatalf("truncatePrefix left a partial rune: %q", got)
	}
	if got := truncatePrefix(strings.Repeat("é", promptHookMax), promptHookMax+1); len(got) != promptHookMax {
		t.Fatalf("truncatePrefix cut a whole rune: len %d", len(got))
	}
}

// The cap is on the CONTEXT (10,000 chars), not on stdout (256 KiB, room for
// the JSON envelope around a full context).
func TestHookContextCappedAtLimit(t *testing.T) {
	out := &hookBuffer{max: promptHookStdout}
	out.Write([]byte(strings.Repeat("a", promptHookMax+50)))
	if got := len(parseHookOutput(out).Context); got != promptHookMax {
		t.Fatalf("context len = %d; want %d", got, promptHookMax)
	}
}

// A hook that blocks (decision or continue) blocks, and the reason rides
// along; extra context from a blocking hook is dropped.
func TestHookDecisionBlock(t *testing.T) {
	out := &hookBuffer{max: promptHookStdout}
	out.Write([]byte(`{"decision":"block","reason":"senha no prompt","hookSpecificOutput":{"additionalContext":"x"}}`))
	r := parseHookOutput(out)
	if !r.Blocked || r.Reason != "senha no prompt" || r.Context != "" {
		t.Fatalf("decision block = %+v", r)
	}
	out2 := &hookBuffer{max: promptHookStdout}
	out2.Write([]byte(`{"continue":false,"stopReason":"nao vale"}`))
	r2 := parseHookOutput(out2)
	if !r2.Blocked || r2.Reason != "nao vale" {
		t.Fatalf("continue false = %+v", r2)
	}
}
