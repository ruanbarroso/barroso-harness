// Session persistence: the bridge between the append-only log (internal/store)
// and the in-memory transcript ([]dealer.Mensagem). The log stores what
// happened; memory is derived from it — never the other way around. Send
// writes; Resume rebuilds.
package runtime

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// Log limits. The 16 KiB limit on tool results keeps the file short without
// losing content: above it the result goes to `blobs` and the event carries
// only the preview + the reference.
const (
	blobThreshold = 16 << 10 // 16 KiB: above this, tool_result becomes a blob
	previewBytes  = 512      // how much of the result stays in the preview
)

// record writes an event to the log. Silent when there is no persistence: the
// in-memory session keeps working without a database (-p mode and tests).
//
// A write failure does NOT abort the turn. Reason: the log is observability,
// not authorization — losing a line because the disk filled up must not take
// down a running conversation. The cost is an incomplete, recoverable history.
func (s *Session) record(kind string, payload map[string]any) {
	if !s.Persistente() || payload == nil {
		return
	}
	_, _ = s.store.StoreEvent(s.sessionID, kind, payload)
}

// recordResult writes a tool_result, moving to a blob whatever exceeds
// blobThreshold. It returns the written payload (useful for tests and
// inspection).
func (s *Session) recordResult(callID, name, output string, isErr bool) map[string]any {
	// Session without a database (subagent, -p without store): nothing to
	// write. Without this early return, a result above blobThreshold called
	// PutBlob on a nil store and crashed the whole process.
	if !s.Persistente() {
		return nil
	}
	payload := map[string]any{
		"role":      "tool_result",
		"tool_id":   callID,
		"tool_name": name,
		"is_erro":   isErr,
	}
	if len(output) > blobThreshold {
		if ref, err := s.store.PutBlob([]byte(output)); err == nil {
			for k, v := range ref {
				payload[k] = v
			}
			payload["preview"] = truncatePrefix(output, previewBytes)
			payload["truncated"] = true
		} else {
			// Without a blob (write failure), the preview is what gets recorded.
			payload["content"] = truncatePrefix(output, previewBytes)
			payload["truncated"] = true
		}
	} else {
		payload["content"] = output
	}
	s.record("tool_result", payload)
	return payload
}

// truncatePrefix returns the first n bytes without splitting a UTF-8 rune: a
// preview with dangling bytes would show up as "?" when read and in the log.
func truncatePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return trimPartialRune(s[:n])
}

// trimPartialRune drops an incomplete rune at the end of s. Checking only for
// a trailing continuation byte (10xxxxxx) kept a lone leading byte: a cut
// right after the first byte of "é".
func trimPartialRune(s string) string {
	for i := len(s) - 1; i >= 0 && i >= len(s)-utf8.UTFMax; i-- {
		if utf8.RuneStart(s[i]) {
			if !utf8.FullRuneInString(s[i:]) {
				return s[:i]
			}
			break
		}
	}
	return s
}

// ── reconstruction ────────────────────────────────────────────────────────

// Resume loads the session events and rebuilds s.history in the same
// []dealer.Mensagem format the Send loop already uses. Tool results that went
// to a blob are rehydrated here — the transcript comes back complete.
//
// Without events, history stays empty (new session or wrong id): not an
// error, just a conversation starting.
func (s *Session) Resume() error {
	if !s.Persistente() {
		return nil
	}
	events, err := s.store.ListEvents(s.sessionID, 0, 0)
	if err != nil {
		return err
	}
	s.history = eventsToMessages(s.store, events)
	s.closeOrphanToolCalls()
	return nil
}

// eventsToMessages translates the log into the transcript. Only content
// events enter: user_message, assistant, tool_call, tool_result. The others
// (guardrail, session_reset, metadata) are observability, not transcript.
func eventsToMessages(st *store.Store, events []store.Event) []dealer.Mensagem {
	var out []dealer.Mensagem
	for _, e := range events {
		switch e.Kind {
		case "user_message", "assistant", "tool_call", "tool_result":
		case eventCompaction, eventCompactionEN:
			// Re-apply the agent compaction where it happened: without this,
			// /resume and session/load would bring back the conversation the
			// model had already summarized. Both the original "estado" key and
			// the English "state" key are accepted.
			var c struct {
				Estado string `json:"estado"`
				State  string `json:"state"`
			}
			if json.Unmarshal([]byte(e.Payload), &c) == nil {
				state := c.Estado
				if strings.TrimSpace(state) == "" {
					state = c.State
				}
				if strings.TrimSpace(state) != "" {
					out = compactHistory(out, state)
				}
			}
			continue
		default:
			continue
		}
		var p struct {
			Role            string            `json:"role"`
			Content         string            `json:"content"`
			ToolID          string            `json:"tool_id"`
			ToolName        string            `json:"tool_name"`
			ToolArgs        string            `json:"tool_args"`
			IsErro          bool              `json:"is_erro"`
			Blob            string            `json:"blob"`
			Preview         string            `json:"preview"`
			Images          []string          `json:"images"`
			HookCtx         string            `json:"hook_context"`
			ResponsesOutput []json.RawMessage `json:"responses_output"`
			Usage           *struct {
				PromptTokens    int `json:"prompt_tokens"`
				OutputTokens    int `json:"output_tokens"`
				CachedTokens    int `json:"cached_tokens"`
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			continue // an unreadable event must not corrupt the transcript
		}
		m := dealer.Mensagem{
			Role:     p.Role,
			Conteudo: p.Content,
			ToolID:   p.ToolID,
			ToolName: p.ToolName,
			ToolArgs: p.ToolArgs,
			IsErro:   p.IsErro,
			Images:   p.Images,
			// The hook context stays apart: Conteudo is what the user typed,
			// and only the request to the model joins the two.
			HookContext:     p.HookCtx,
			ResponsesOutput: p.ResponsesOutput,
		}
		if p.Usage != nil {
			m.Usage = &dealer.Usage{
				PromptTokens:    p.Usage.PromptTokens,
				OutputTokens:    p.Usage.OutputTokens,
				CachedTokens:    p.Usage.CachedTokens,
				ReasoningTokens: p.Usage.ReasoningTokens,
			}
		}
		// Rehydration: large content stayed in the blob, the event holds the reference.
		if p.Blob != "" {
			m.Conteudo = rehydrate(st, p.Blob, p.Preview)
		}
		out = append(out, m)
	}
	return out
}

// rehydrate returns the full content of an archived tool_result. The preview
// is NOT added: it is the start of the blob itself, and concatenating them
// would repeat the first bytes. Without a blob (database missing the data),
// the preview is what is left — and better than nothing.
func rehydrate(st *store.Store, hash, preview string) string {
	if st == nil || hash == "" {
		return preview
	}
	data, err := st.GetBlob(hash)
	if err != nil {
		return preview
	}
	return string(data)
}

// oneLine collapses a text into a single line: the log is one line per event,
// and provider messages often contain newlines.
func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const limit = 240
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

// closeOrphanToolCalls repairs a rebuilt transcript whose log ended
// mid-round: a tool_call was recorded before its result and the process died
// (kill, crash, connector restart) before recordResult wrote it. The resumed
// transcript is then malformed: for Claude the WHOLE request is rejected —
// "tool_use ids were found without tool_result blocks immediately after" —
// and, with no frame of error on the way back, the client only sees silence,
// so every turn of the resumed session times out instead of failing honestly
// (Bruce, 2026-10-24: one cancel interrupted a grep/glob round in flight and
// every later turn died in the 150 s watchdog ×2, in series).
//
// The orphan gets a synthetic result, in call order, recorded into the log
// with "cancelled": true so later resumes do not repeat the repair. Results
// without a call are left alone: the runtime never writes them (Send commits
// both ends together) and the log is the observability layer, not something
// the transcript rewrites silently.
func (s *Session) closeOrphanToolCalls() {
	hasResult := map[string]bool{}
	callMsg := map[string]dealer.Mensagem{}
	var orphans []dealer.Mensagem
	for _, m := range s.history {
		switch m.Role {
		case "tool_call":
			callMsg[m.ToolID] = m
			orphans = append(orphans, m)
		case "tool_result":
			hasResult[m.ToolID] = true
		}
	}
	if len(callMsg) == len(hasResult) {
		// Common shape: every call has exactly one result (results also repeat
		// when a blob rehydrates — can't prune calls by count alone).
		balanced := true
		for _, m := range orphans {
			if !hasResult[m.ToolID] {
				balanced = false
				break
			}
		}
		if balanced {
			return
		}
	}
	for _, call := range orphans {
		if hasResult[call.ToolID] {
			continue
		}
		const note = "[turn interrupted] This tool call was cancelled before any result was recorded; its execution state is unknown. " +
			"Check the side effects on disk and re-run the tool if the task depends on it."
		s.record("tool_result", map[string]any{
			"role":      "tool_result",
			"tool_id":   call.ToolID,
			"tool_name": call.ToolName,
			"content":   note,
			"is_erro":   true,
			"cancelled": true,
		})
		s.history = append(s.history, dealer.Mensagem{
			Role: "tool_result", Conteudo: note,
			ToolID: call.ToolID, ToolName: call.ToolName, IsErro: true,
		})
	}
}

// ── search and retrieval ──────────────────────────────────────────────────

// Search looks through the session events (substring, case-insensitive) and
// returns excerpts with context — it is what recovers content the guardrail
// compacted or that left the model window.
func (s *Session) Search(query string, limit int) []store.Hit {
	if !s.Persistente() || strings.TrimSpace(query) == "" {
		return nil
	}
	hits, err := s.store.Buscar(s.sessionID, query, limit)
	if err != nil {
		return nil
	}
	return hits
}

// Retrieve returns ONE event with the full payload (blob rehydrated), so the
// model can recover the whole output of an archived tool.
func (s *Session) Retrieve(seq int) (store.Event, error) {
	if !s.Persistente() {
		return store.Event{}, store.ErrSessaoInexistente
	}
	e, err := s.store.GetEvent(s.sessionID, seq)
	if err != nil {
		return store.Event{}, err
	}
	full, err := s.store.Rehydrate(e.Payload)
	if err != nil {
		return *e, nil
	}
	e.Payload = full
	return *e, nil
}
