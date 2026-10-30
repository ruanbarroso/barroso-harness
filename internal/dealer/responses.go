// responses.go speaks the OpenAI Responses API (POST {base}/responses) as the
// preferred protocol. Chat Completions remains available when that route is
// absent. It reuses the transcript preparation of
// the chat path (parallel-call merge, duplicate-id repair, result re-pairing)
// but NOT its wire fingerprint: the Zen decoy tools and the system-side
// brake are a Chat-only contract and are never added here. The shared
// OpenCode client headers, TLS routing and retry/watchdog machinery are the
// same as on the chat path.
//
// The request is stateless: store=false and no previous_response_id, the
// whole history travels in `input` on every round.
package dealer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ── request ──────────────────────────────────────────────────────────────

type respPart struct {
	Type     string  `json:"type"`
	Text     *string `json:"text,omitempty"`
	ImageURL string  `json:"image_url,omitempty"`
}

type respMessage struct {
	Type    string     `json:"type"`
	Role    string     `json:"role"`
	Content []respPart `json:"content"`
}

type respFunctionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type respFunctionOutput struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type respTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters"`
	Strict      bool   `json:"strict"`
}

type respReasoning struct {
	Effort string `json:"effort"`
}

// respRequest keeps a fixed field order (the body is byte-deterministic).
type respRequest struct {
	Model             string         `json:"model"`
	Instructions      string         `json:"instructions"`
	Input             []any          `json:"input"`
	Tools             []respTool     `json:"tools,omitempty"`
	ToolChoice        any            `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Reasoning         *respReasoning `json:"reasoning,omitempty"`
	Include           []string       `json:"include,omitempty"`
	MaxOutputTokens   int            `json:"max_output_tokens"`
	Stream            bool           `json:"stream"`
	Store             bool           `json:"store"`
}

func textPtr(s string) *string { return &s }

// toolResponses flattens one tool schema ({"type":"function","function":{...}}
// or already flat) into the Responses function tool.
func toolResponses(t map[string]any) respTool {
	src := t
	if fn, ok := t["function"].(map[string]any); ok {
		src = fn
	}
	out := respTool{Type: "function", Parameters: src["parameters"]}
	out.Name, _ = src["name"].(string)
	out.Description, _ = src["description"].(string)
	if out.Parameters == nil {
		out.Parameters = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return out
}

// escolhaResponses returns the Responses tool_choice: auto normally, or the
// one named function forced for a context-compaction request. A missing name
// falls back to auto rather than emitting a reference to an absent tool.
func (c *Cliente) escolhaResponses(tools []respTool) any {
	if c.noToolCalls {
		return "none"
	}
	name := c.toolChoice
	if name == "" {
		return "auto"
	}
	for _, t := range tools {
		if t.Name == name {
			return map[string]any{"type": "function", "name": name}
		}
	}
	return "auto"
}

func (i *respItem) UnmarshalJSON(data []byte) error {
	type plain respItem
	var item plain
	if err := json.Unmarshal(data, &item); err != nil {
		return err
	}
	*i = respItem(item)
	i.Raw = append(i.Raw[:0], data...)
	return nil
}

func appendOutputItem(r *Rodada, item *respItem, seen map[string]bool) {
	if item == nil || len(item.Raw) == 0 {
		return
	}
	if item.ID != "" && seen[item.ID] {
		return
	}
	r.ResponsesOutput = append(r.ResponsesOutput, append(json.RawMessage(nil), item.Raw...))
	if item.ID != "" {
		seen[item.ID] = true
	}
}

func (c *Cliente) corpoResponses(system string, msgs []Mensagem, tools []map[string]any, inline bool) ([]byte, error) {
	list, err := converterHistorico(msgs, inline)
	if err != nil {
		return nil, err
	}
	list = fundirChamadasConsecutivas(list)
	desduplicarIDsDeChamada(list) // before re-pairing, see corpoChat
	list = reapararResultados(list)

	// A raw Responses output already carries the function_call items. Track
	// those IDs so the compatibility tool_call messages below do not duplicate
	// them when replaying the same transcript.
	replayedCalls := map[string]bool{}
	for _, m := range list {
		for _, raw := range m.ResponsesOutput {
			var item struct {
				Type   string `json:"type"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(raw, &item) == nil && item.Type == "function_call" && item.CallID != "" {
				replayedCalls[item.CallID] = true
			}
		}
	}
	input := make([]any, 0, len(list))
	for _, m := range list {
		switch m.Role {
		case "user":
			var parts []respPart
			switch v := m.Content.(type) {
			case []contentPart:
				for _, p := range v {
					if p.ImageURL != nil {
						parts = append(parts, respPart{Type: "input_image", ImageURL: p.ImageURL.URL})
						continue
					}
					parts = append(parts, respPart{Type: "input_text", Text: textPtr(p.Text)})
				}
			default:
				parts = []respPart{{Type: "input_text", Text: textPtr(textoMensagem(m.Content))}}
			}
			input = append(input, respMessage{Type: "message", Role: "user", Content: parts})
		case "assistant":
			if len(m.ResponsesOutput) > 0 {
				for _, item := range m.ResponsesOutput {
					if json.Valid(item) {
						input = append(input, json.RawMessage(item))
					}
				}
			} else {
				if txt := textoMensagem(m.Content); txt != "" {
					input = append(input, respMessage{Type: "message", Role: "assistant",
						Content: []respPart{{Type: "output_text", Text: textPtr(txt)}}})
				}
				for _, call := range m.ToolCalls {
					if replayedCalls[call.ID] {
						continue
					}
					input = append(input, respFunctionCall{Type: "function_call", CallID: call.ID,
						Name: call.Function.Name, Arguments: call.Function.Arguments})
				}
			}
		case "tool":
			input = append(input, respFunctionOutput{Type: "function_call_output",
				CallID: m.ToolCallID, Output: textoMensagem(m.Content)})
		}
	}

	req := respRequest{
		Model:           c.cfg.Model,
		Instructions:    comBlocoEnv(system),
		Input:           input,
		Include:         []string{"reasoning.encrypted_content"},
		MaxOutputTokens: c.MaxTokens(),
		Stream:          true,
		Store:           false,
	}
	for _, t := range tools {
		req.Tools = append(req.Tools, toolResponses(t))
	}
	if len(req.Tools) > 0 {
		req.ToolChoice = c.escolhaResponses(req.Tools)
		if c.cfg.DisableParallelToolCalls {
			f := false
			req.ParallelToolCalls = &f
		}
	}
	if effort := strings.TrimSpace(c.cfg.Effort); effort != "" && !strings.EqualFold(effort, "none") {
		req.Reasoning = &respReasoning{Effort: effort}
	}
	return json.Marshal(req)
}

// ── response stream ──────────────────────────────────────────────────────

type respItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        json.RawMessage `json:"arguments"`
	EncryptedContent string          `json:"encrypted_content"`
	Raw              json.RawMessage `json:"-"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Text string `json:"text"`
	} `json:"summary"`
}

type respObject struct {
	Status string     `json:"status"`
	Output []respItem `json:"output"`
	Usage  *struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"input_tokens_details"`
		OutputTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Error             json.RawMessage `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

type respEvent struct {
	Type        string          `json:"type"`
	Delta       json.RawMessage `json:"delta"`
	ItemID      string          `json:"item_id"`
	OutputIndex *int            `json:"output_index"`
	Item        *respItem       `json:"item"`
	Response    *respObject     `json:"response"`
	Text        string          `json:"text"`
	Refusal     string          `json:"refusal"`
	Arguments   json.RawMessage `json:"arguments"`
}

// respCall is a function call being assembled. It is only handed over once
// finalized: output_item.done, arguments.done with a known name, or listed
// by the terminal response object.
type respCall struct {
	itemID, callID, name, args string
	outIdx                     int
	seq                        int
	done                       bool
}

func deltaTexto(raw json.RawMessage) string {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return ""
}

func usageResponses(o *respObject) *Usage {
	if o == nil || o.Usage == nil {
		return nil
	}
	u := &Usage{PromptTokens: o.Usage.InputTokens, OutputTokens: o.Usage.OutputTokens}
	if d := o.Usage.InputTokensDetails; d != nil {
		u.CachedTokens = d.CachedTokens
	}
	if d := o.Usage.OutputTokensDetails; d != nil {
		u.ReasoningTokens = d.ReasoningTokens
	}
	return u
}

// readResponses consumes the SSE stream of a Responses request and builds the
// Rodada. Same contract as readRound: start is the send time, kick feeds the
// idle watchdog. A terminal event (response.completed / incomplete / failed /
// error) is required; without one the stream was cut (io.ErrUnexpectedEOF).
func readResponses(resp *http.Response, ctx context.Context, start time.Time, kick func()) (*Rodada, error) {
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, newHTTPError(resp, b)
	}
	r := &Rodada{}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 1<<20), sseMaxLinha)

	var (
		calls      []*respCall
		byItem     = map[string]*respCall{}
		byIdx      = map[int]*respCall{}
		textSeen   = map[string]bool{}
		textAlias  = map[string]string{}
		outputSeen = map[string]bool{}
		terminal   string // completed | incomplete
		reason     string // incomplete_details.reason
		lastText   string // item id/index of the last appended message text
	)
	lookup := func(itemID string, idx *int) *respCall {
		var c *respCall
		if itemID != "" {
			c = byItem[itemID]
		}
		if c == nil && idx != nil {
			c = byIdx[*idx]
			if c != nil && c.itemID != "" && itemID != "" && c.itemID != itemID {
				c = nil
			}
		}
		return c
	}
	ensure := func(itemID string, idx *int) *respCall {
		if c := lookup(itemID, idx); c != nil {
			if c.itemID == "" && itemID != "" {
				c.itemID = itemID
				byItem[itemID] = c
			}
			return c
		}
		c := &respCall{itemID: itemID, outIdx: -1, seq: len(calls)}
		if idx != nil {
			c.outIdx = *idx
			if byIdx[*idx] == nil {
				byIdx[*idx] = c
			}
		}
		if itemID != "" {
			byItem[itemID] = c
		}
		calls = append(calls, c)
		return c
	}
	appendText := func(key, text string) {
		if text == "" {
			return
		}
		if lastText != "" && key != "" && key != lastText {
			r.Texto += "\n\n"
		}
		r.Texto += text
		if key != "" {
			lastText = key
		}
	}
	textKey := func(itemID string, idx *int) string {
		if idx != nil {
			key := fmt.Sprintf("#%d", *idx)
			if itemID != "" {
				textAlias[itemID] = key
				if textSeen[itemID] {
					textSeen[key] = true
				}
				if lastText == itemID {
					lastText = key
				}
			}
			return key
		}
		if key := textAlias[itemID]; key != "" {
			return key
		}
		return itemID
	}
	absorb := func(c *respCall, it *respItem, final bool) {
		if it.CallID != "" {
			c.callID = it.CallID
		}
		if it.Name != "" {
			c.name = it.Name
		}
		frag, _ := argFragment(it.Arguments)
		if final {
			if frag != "" || c.args == "" {
				c.args = frag
			}
			c.done = true
		} else if c.args == "" {
			c.args = frag
		}
	}

loop:
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			if strings.HasPrefix(line, "{") {
				if err := erroDePayload(line); err != nil {
					return nil, err
				}
			}
			continue // event: lines, blank separators and ": ping" are not progress
		}
		if kick != nil {
			kick()
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break // without a terminal event this is still a cut stream
		}
		if r.TTFT == 0 {
			r.TTFT = elapsed(start)
		}
		if err := erroDePayload(payload); err != nil {
			return nil, err
		}
		var ev respEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			return nil, fmt.Errorf("invalid chunk: %w", err)
		}
		switch ev.Type {
		case "response.output_text.delta", "response.refusal.delta":
			if d := deltaTexto(ev.Delta); d != "" {
				k := textKey(ev.ItemID, ev.OutputIndex)
				appendText(k, d)
				textSeen[k] = true
			}
		case "response.output_text.done", "response.refusal.done":
			k := textKey(ev.ItemID, ev.OutputIndex)
			text := ev.Text
			if ev.Type == "response.refusal.done" && text == "" {
				text = ev.Refusal
			}
			if text != "" && !textSeen[k] {
				appendText(k, text)
				textSeen[k] = true
			}
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			if d := deltaTexto(ev.Delta); d != "" {
				r.Raciocinio += d
				r.DurRaciocinio = elapsed(start)
			}
		case "response.reasoning_summary_part.added":
			if r.Raciocinio != "" && !strings.HasSuffix(r.Raciocinio, "\n\n") {
				r.Raciocinio += "\n\n"
			}
		case "response.output_item.added", "response.output_item.done":
			if ev.Item == nil {
				break
			}
			final := ev.Type == "response.output_item.done"
			if final {
				appendOutputItem(r, ev.Item, outputSeen)
			}
			switch ev.Item.Type {
			case "function_call":
				absorb(ensure(ev.Item.ID, ev.OutputIndex), ev.Item, final)
			case "reasoning":
				if final {
					if r.Raciocinio == "" {
						var parts []string
						for _, part := range ev.Item.Summary {
							if part.Text != "" {
								parts = append(parts, part.Text)
							}
						}
						r.Raciocinio = strings.Join(parts, "\n\n")
					}
				}
			case "message":
				k := textKey(ev.Item.ID, ev.OutputIndex)
				if final && !textSeen[k] {
					for _, p := range ev.Item.Content {
						text := p.Text
						if p.Type == "refusal" && text == "" {
							text = p.Refusal
						}
						if (p.Type == "output_text" || p.Type == "refusal") && text != "" {
							appendText(k, text)
							textSeen[k] = true
						}
					}
				}
			}
		case "response.function_call_arguments.delta":
			c := ensure(ev.ItemID, ev.OutputIndex)
			if d := deltaTexto(ev.Delta); d != "" && !c.done {
				c.args += d
			}
		case "response.function_call_arguments.done":
			c := ensure(ev.ItemID, ev.OutputIndex)
			frag, _ := argFragment(ev.Arguments)
			if frag != "" {
				c.args = frag
			}
			if c.name != "" {
				c.done = true
			}
		case "response.completed", "response.incomplete":
			terminal = strings.TrimPrefix(ev.Type, "response.")
			r.Usage = usageResponses(ev.Response)
			if ev.Response != nil {
				if d := ev.Response.IncompleteDetails; d != nil {
					reason = d.Reason
				}
				// The terminal object lists the final output items: it
				// finalizes calls a gateway never sent output_item.done for
				// and carries text/reasoning a gateway never streamed.
				if len(ev.Response.Output) > 0 {
					// The terminal object is authoritative and is already ordered.
					// Replace streamed copies with its complete item list, including
					// message phase, function calls, and encrypted reasoning.
					r.ResponsesOutput = nil
					outputSeen = map[string]bool{}
					for i := range ev.Response.Output {
						appendOutputItem(r, &ev.Response.Output[i], outputSeen)
					}
				}
				for i := range ev.Response.Output {
					it := &ev.Response.Output[i]
					idx := i
					switch it.Type {
					case "function_call":
						c := lookup(it.ID, nil)
						if c == nil && it.CallID != "" {
							for _, o := range calls {
								if o.callID == it.CallID {
									c = o
									break
								}
							}
						}
						if c == nil {
							c = ensure(it.ID, &idx)
						}
						absorb(c, it, true)
					case "message":
						k := textKey(it.ID, &idx)
						if !textSeen[k] {
							for _, p := range it.Content {
								text := p.Text
								if p.Type == "refusal" && text == "" {
									text = p.Refusal
								}
								if p.Type == "output_text" || p.Type == "refusal" {
									appendText(k, text)
								}
							}
							textSeen[k] = true
						}
					case "reasoning":
						if r.Raciocinio == "" {
							var parts []string
							for _, s := range it.Summary {
								if s.Text != "" {
									parts = append(parts, s.Text)
								}
							}
							r.Raciocinio = strings.Join(parts, "\n\n")
						}
					}
				}
			}
			break loop
		case "response.failed":
			if ev.Response != nil {
				e := strings.TrimSpace(string(ev.Response.Error))
				if e != "" && e != "null" {
					return nil, erroDeObjeto(ev.Response.Error, 0)
				}
			}
			return nil, newProviderError(0, "response_failed", "the response failed without detail")
		case "response.cancelled", "response.canceled":
			return nil, newProviderError(0, "response_cancelled", "the provider cancelled the response")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("stream: %w", err)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if terminal == "" {
		return nil, fmt.Errorf("stream: ended without a terminal event: %w", io.ErrUnexpectedEOF)
	}

	// Hand over only finalized calls, in output order.
	sort.SliceStable(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if a.outIdx >= 0 && b.outIdx >= 0 && a.outIdx != b.outIdx {
			return a.outIdx < b.outIdx
		}
		return a.seq < b.seq
	})
	for _, c := range calls {
		if !c.done || c.name == "" {
			continue
		}
		id := c.callID
		if id == "" {
			id = c.itemID
		}
		r.Tools = append(r.Tools, ToolChamada{ID: id, Nome: c.name, Args: c.args})
	}

	switch terminal {
	case "completed":
		r.Fim = "stop"
		if len(r.Tools) > 0 {
			r.Fim = "tool_calls"
		}
	case "incomplete":
		// max_output_tokens, content_filter or an unknown reason: the output
		// may be partial. Never a final answer.
		switch reason {
		case "max_output_tokens":
			r.Fim = "length"
		case "":
			r.Fim = "incomplete"
		default:
			r.Fim = reason
		}
		if reason != "max_output_tokens" {
			if reason == "" {
				reason = "incomplete"
			}
			if len(r.Tools) == 0 {
				return nil, &providerError{kind: reason, summary: "the provider returned an incomplete response: " + reason}
			}
			// Only max_output_tokens should trigger another continuation.
			r.Cortada = false
		} else {
			r.Cortada = true
		}
	}
	r.FimPorTexto = len(r.Tools) == 0
	r.Dur = elapsed(start)
	return r, nil
}
