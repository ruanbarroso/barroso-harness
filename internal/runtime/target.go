package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/budget"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// ── The working target ────────────────────────────────────────────────────
//
// The measured problem (Bruce, Greg, Zoe; sessions.db, 2026-10-29): the
// window is a PHYSICAL limit, not a working set. With a 922k catalog window
// the guardrail only acts near 900k, and the model almost never calls
// context_compact on its own (10 calls in 15k rounds on Bruce, 4 in 4.7k on
// Greg, 0 in 3.5k on Zoe). 19 of 20 (Bruce), 35 of 54 (Greg) and 78 of 96
// (Zoe) of the turns that reached 300k+ tokens STARTED at 70%+ of that size:
// the weight accumulates between turns, not inside one. Requests that reach
// the gateway with 500k-1M tokens cost full price on every uncached token
// and the model's recall rots (Anthropic, "context rot").
//
// So the policy gets a WORKING TARGET (alvo, budget.AlvoTrabalho): the size at
// which the turn is asked to compact. Levers, in order of preference:
//
//  1. stub OLD tool results in one batch down to a lower floor — free,
//     deterministic, the recent rounds stay verbatim, and the stub carries the
//     arguments so the model can re-issue the exact call;
//  2. tell the model, once per turn, to compact at its next milestone;
//  3. over the target after the stubs: REQUIRE context_compact (twice);
//  4. the model did not comply: the harness writes the state mechanically.
//
// Hysteresis matters: one batch of stubs buys many rounds of a stable prefix
// (the provider's cache lives until the prefix changes), so the policy never
// touches the history at every cycle.

// contextNotePrefix marks the synthetic messages the context policy injects.
// It is a runtime notice, not the user's request: isSynthetic recognizes it
// and the screen does not draw it as the user speaking.
const contextNotePrefix = "[context] "

// compactKeepUnits is how many of the turn's newest tool rounds survive a
// context_compact verbatim (the compact round itself counts as one). They are
// the freshest evidence; everything older is the model's state.
const compactKeepUnits = 3

// maxDemands is how many times the model is REQUIRED to compact before the
// harness writes the state itself.
const maxDemands = 2

// avisoContext asks the model to compact at its next milestone.
const avisoContext = contextNotePrefix + "This conversation's context is at %d tokens, near the %d-token working target. " +
	"At your next natural milestone — a unit of work just finished (root cause found, change made and tested, exploration done), " +
	"or right before your final answer — call context_compact with your working state, before you start the next step. " +
	"Do not wait for the context to fill up, and do not call it halfway through a step. Whatever you leave out stays recoverable through session_search / session_retrieve.\n"

// exigeContext demands the working state before anything else happens.
const exigeContext = contextNotePrefix + "The context is over the working target (%d tokens against %d). " +
	"Your next action MUST be context_compact: write the working state that lets you continue without the earlier history " +
	"(goal, decisions, discovered facts with paths and values, what was already changed, the exact next step). " +
	"Do not run any other tool first.\n"

const factsHeader = "## Working facts kept by the harness"

// Bounds of the appendix: it is context too.
const (
	maxFactUsers = 8
	maxFactFiles = 20
	maxFactUser  = 240
	maxFactPath  = 240
)

// appendix holds the mechanical facts of a conversation: what the user asked
// (verbatim, clipped) and which files the agent read or changed. They are
// derived from the history, never from a model call, so they survive the
// compaction of the very history they describe — and accumulate across
// compactions, which a model-written state tends to forget.
type appendix struct {
	users, files []string
	seen         map[string]bool
}

func newAppendix() *appendix { return &appendix{seen: map[string]bool{}} }

func (a *appendix) addUser(s string) {
	if s = strings.TrimSpace(s); s != "" && !a.seen["u:"+s] {
		a.seen["u:"+s] = true
		a.users = append(a.users, s)
	}
}

func (a *appendix) addFile(s string) {
	if s = clip(strings.TrimSpace(s), maxFactPath); s != "" && !a.seen["f:"+s] {
		a.seen["f:"+s] = true
		a.files = append(a.files, s)
	}
}

// parseFacts reads back the facts block of a summary written by compactTurn,
// so a later compaction carries them forward. The model's own state precedes
// the block and is ignored: it is rewritten every time.
func (a *appendix) parseFacts(summary string) {
	i := strings.Index(summary, factsHeader)
	if i < 0 {
		return
	}
	for _, l := range strings.Split(summary[i:], "\n") {
		switch {
		case strings.HasPrefix(l, "- user asked: "):
			a.addUser(strings.TrimPrefix(l, "- user asked: "))
		case strings.HasPrefix(l, "- read "), strings.HasPrefix(l, "- changed "):
			a.addFile(strings.TrimPrefix(l, "- "))
		}
	}
}

// collect adds the facts of a span of the history.
func (a *appendix) collect(h []dealer.Mensagem) {
	for _, m := range h {
		switch m.Role {
		case "user":
			c := strings.TrimSpace(m.Conteudo)
			switch {
			case c == "":
			case isSummary(c):
				a.parseFacts(c)
			case isSynthetic(c):
			default:
				a.addUser(clip(strings.Join(strings.Fields(c), " "), maxFactUser))
			}
		case "tool_call":
			switch m.ToolName {
			case "read":
				for _, p := range toolPaths(m.ToolName, m.ToolArgs) {
					a.addFile("read " + p)
				}
			case "write", "edit", "edit_many", "copy":
				for _, p := range toolPaths(m.ToolName, m.ToolArgs) {
					a.addFile("changed " + p)
				}
			}
		}
	}
}

// texto renders the appendix, newest last, each part bounded.
func (a *appendix) texto() string {
	if len(a.users) == 0 && len(a.files) == 0 {
		return ""
	}
	users, files := a.users, a.files
	if len(users) > maxFactUsers {
		// The first request is usually the original goal: it always stays,
		// followed by the newest ones.
		users = append([]string{users[0]}, users[len(users)-(maxFactUsers-1):]...)
	}
	if len(files) > maxFactFiles {
		files = files[len(files)-maxFactFiles:]
	}
	var sb strings.Builder
	sb.WriteString(factsHeader + "\n")
	sb.WriteString("(the user's requests and the files touched, newest last; the full detail is in the log — session_search recovers it)\n")
	for _, u := range users {
		sb.WriteString("- user asked: " + u + "\n")
	}
	for _, f := range files {
		sb.WriteString("- " + f + "\n")
	}
	return sb.String()
}

// toolPaths extracts the file paths of a tool call's arguments: one "path"
// (read, write, edit), from/to (copy) or the list of edit_many.
func toolPaths(name, args string) []string {
	if strings.TrimSpace(args) == "" {
		return nil
	}
	var c struct {
		Path  string `json:"path"`
		From  string `json:"from"`
		To    string `json:"to"`
		Edits []struct {
			Path string `json:"path"`
		} `json:"edits"`
	}
	if json.Unmarshal([]byte(args), &c) != nil {
		return nil
	}
	switch name {
	case "read", "write", "edit":
		if c.Path != "" {
			return []string{c.Path}
		}
	case "copy":
		if c.To != "" {
			return []string{c.To}
		}
	case "edit_many":
		set := map[string]bool{}
		for _, e := range c.Edits {
			if e.Path != "" {
				set[e.Path] = true
			}
		}
		out := make([]string, 0, len(set))
		for p := range set {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	return nil
}

// ── compactTurn ───────────────────────────────────────────────────────────

// realRequest reports whether a message is the user's own request (not a
// summary and not a runtime notice).
func realRequest(m dealer.Mensagem) bool {
	return m.Role == "user" && !isSummary(m.Conteudo) && !isSynthetic(m.Conteudo)
}

// compactTurn applies the model's context_compact. Unlike compactHistory —
// which only replaces what comes BEFORE the current request, and so freed
// almost nothing when the weight sat in the turn's own rounds (7 of the 10
// calls on Bruce shrank the history by under 12%) — it also drops the turn's
// OLDER tool rounds. What survives, in order:
//
//   - the state the model wrote, plus the harness's mechanical facts (the
//     user's requests and the files touched, accumulated across compactions);
//   - the current request, verbatim;
//   - the newest `keep` tool rounds of the turn, whole (call + results), so
//     the request stays valid for every provider dialect and the model keeps
//     its freshest evidence.
//
// It is deterministic: Resume replays it from the logged state and `keep`.
// The second result is false when there was nothing to compact.
func compactTurn(h []dealer.Mensagem, state string, keep int) ([]dealer.Mensagem, bool) {
	cut := -1
	for i := len(h) - 1; i >= 0; i-- {
		if realRequest(h[i]) {
			cut = i
			break
		}
	}
	if cut < 0 {
		return h, false
	}
	keep = max(keep, 1)
	units := turnUnits(h, cut+1)
	drop := max(len(units)-keep, 0)
	if cut == 0 && drop == 0 {
		return h, false
	}

	// Facts come from everything that is about to leave the request.
	a := newAppendix()
	a.collect(h[:cut])
	tailStart := cut + 1
	if drop > 0 {
		a.collect(h[units[0].start:units[drop-1].end])
		tailStart = units[drop].start
	}
	summary := dealer.Mensagem{Role: "user", Conteudo: summaryPrefix + joinState(state, a.texto())}

	out := make([]dealer.Mensagem, 0, 3+len(h)-tailStart)
	out = append(out, summary, h[cut])
	if drop > 0 {
		out = append(out, dealer.Mensagem{Role: "user", Conteudo: turnDropNote})
	}
	return append(out, h[tailStart:]...), true
}

func joinState(state, facts string) string {
	state = strings.TrimSpace(state)
	switch {
	case facts == "":
		return state
	case state == "":
		return facts
	}
	return state + "\n\n" + facts
}

// ── the ladder ────────────────────────────────────────────────────────────

// degrau says which step of the ladder a request of `tokens` deserves.
func degrau(tokens, alvo int) string {
	if alvo <= 0 || tokens <= 0 {
		return ""
	}
	switch {
	case tokens >= budget.Percent(alvo, budget.DegrauExige):
		return "exige"
	case tokens >= budget.Percent(alvo, budget.DegrauPoda):
		return "poda"
	case tokens >= budget.Percent(alvo, budget.DegrauAviso):
		return "aviso"
	}
	return ""
}

// tailProtected is how many of the newest messages podar never touches: the
// current request and the rounds in flight.
const tailProtected = 16

// podar stubs old tool results, oldest first, until at least wantBytes are
// freed, sparing the newest tailProtected messages. It is the batch lever: it
// is only called when the target is crossed, and aims BELOW it, so the
// provider's cached prefix is rewritten once and then stays stable.
type prunedResult struct {
	Index int    `json:"index"`
	ID    string `json:"tool_id,omitempty"`
	Stub  string `json:"stub"`
}

func podar(h []dealer.Mensagem, wantBytes int) ([]dealer.Mensagem, int, []prunedResult) {
	if wantBytes <= 0 {
		return h, 0, nil
	}
	out := append([]dealer.Mensagem(nil), h...)
	limit := max(len(out)-tailProtected, 0)
	freed := 0
	var items []prunedResult
	for i := 0; i < limit && freed < wantBytes; i++ {
		if stubResult(out, i, &freed) {
			items = append(items, prunedResult{Index: i, ID: out[i].ToolID, Stub: out[i].Conteudo})
		}
	}
	return out, freed, items
}

// applyPrunedResults replays the exact batch at the event position. The index
// is stable because earlier transcript transformations are themselves replayed
// in event order; ToolID/Role additionally prevent stubbing the wrong message if
// an old or malformed log no longer lines up.
func applyPrunedResults(h []dealer.Mensagem, items []prunedResult) []dealer.Mensagem {
	for _, item := range items {
		if item.Index < 0 || item.Index >= len(h) {
			continue
		}
		m := &h[item.Index]
		if m.Role == "tool_result" && (item.ID == "" || m.ToolID == item.ID) && item.Stub != "" {
			m.Conteudo = item.Stub
		}
	}
	return h
}

// validCompactCall reports whether a forced round really asked for the
// compaction: a context_compact call with a non-empty state. A gateway that
// ignores tool_choice may answer with text or another tool; that response must
// never be accepted as the compaction.
func validCompactCall(calls []dealer.ToolChamada) bool {
	for _, c := range calls {
		if c.Nome != compactToolName {
			continue
		}
		if state, err := readCompactState(json.RawMessage(c.Args)); err == nil && strings.TrimSpace(state) != "" {
			return true
		}
	}
	return false
}

// argsDaChamada finds the arguments of the call that produced a result, so the
// stub can say exactly what to re-issue.
func argsDaChamada(h []dealer.Mensagem, toolID string) string {
	if toolID == "" {
		return ""
	}
	for _, m := range h {
		if m.Role == "tool_call" && m.ToolID == toolID {
			a := strings.TrimSpace(m.ToolArgs)
			if a == "" {
				return ""
			}
			return " " + clip(a, 300)
		}
	}
	return ""
}

// stubText is the text that replaces a pruned result.
func stubText(h []dealer.Mensagem, i int) string {
	return fmt.Sprintf("(earlier result of %s%s compacted by the context policy; call it again if you still need the content)",
		h[i].ToolName, argsDaChamada(h, h[i].ToolID))
}
