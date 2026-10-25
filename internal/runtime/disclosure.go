package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Progressive tool disclosure. With a large catalog (MCP servers publish
// dozens of tools with big schemas) every request would carry all of them,
// and the provider's prompt cache is dropped whenever that list changes —
// measured: appending one tool zeroed the cache, because tools sit at the
// start of the prompt. So, past a size threshold, the function list sent to
// the provider holds only the core tools plus two fixed ones:
//
//   - tool_search finds deferred tools and returns their full schema as a
//     tool RESULT (in the append-only history, not in the tool list);
//   - tool_call runs a deferred tool by name with its arguments.
//
// The function list never changes during the session, whatever MCP servers
// come and go. The deferred tools are listed one line each in the system
// prompt (frozen at the first request); later ones are announced by
// [system update] and found by tool_search.

// Disclosure modes (config "tool_disclosure").
const (
	DisclosureAuto        = "auto"
	DisclosureFull        = "full"
	DisclosureProgressive = "progressive"
)

// progressiveThreshold: in auto mode, a catalog whose schemas serialize past
// this many bytes (~8k tokens) goes progressive.
const progressiveThreshold = 32 << 10

// catalogLineMax is the description length of a deferred tool's catalog line.
const catalogLineMax = 120

// managementTools change the harness itself; they are rarely needed, so they
// are deferred in progressive mode.
var managementTools = map[string]bool{
	"mcp_list": true, "mcp_add": true, "mcp_edit": true, "mcp_remove": true,
	"skill_create": true, "skill_edit": true, "skill_remove": true,
	"instructions_read": true, "instructions_write": true, "instructions_edit": true, "instructions_delete": true,
}

const (
	toolSearchName = "tool_search"
	toolCallName   = "tool_call"
)

// deferrable reports whether a tool goes behind tool_search: management
// tools and the tools of external MCP servers (injected, not sub-agents).
// The subagent family (launcher "subagent" + subagent_status/subagent_result)
// is always kept direct: results arrive as messages, deferring the launcher
// changes the delegation contract.
func deferrable(name string, injected map[string]bool) bool {
	if name == "subagent" || strings.HasPrefix(name, "subagent_") {
		return false
	}
	return managementTools[name] || injected[name]
}

// disclose splits the catalog. direct is what goes to the provider; exec is
// what runTool may run (direct + deferred, so a model that calls a deferred
// tool by its own name still works); catalog is the system prompt section
// ("" in full mode).
func (s *Session) disclose(list []tools.Spec) (direct, exec []tools.Spec, catalog string) {
	s.muExtras.Lock()
	injected := make(map[string]bool, len(s.injected))
	for _, sp := range s.injected {
		injected[strings.TrimSpace(sp.Name)] = true
	}
	s.muExtras.Unlock()

	mode := strings.ToLower(strings.TrimSpace(s.config.ToolDisclosure))
	switch mode {
	case DisclosureFull:
		return list, list, ""
	case DisclosureProgressive:
		s.progressive = true
	default:
		// Sticky: once progressive, the session stays so (switching back would
		// change the tool list and drop the cache again).
		if !s.progressive && jsonLen(toolSchemas(list)) > progressiveThreshold {
			s.progressive = true
		}
	}
	if !s.progressive {
		return list, list, ""
	}
	var deferred []tools.Spec
	for _, sp := range list {
		if deferrable(sp.Name, injected) {
			deferred = append(deferred, sp)
		} else {
			direct = append(direct, sp)
		}
	}
	direct = append(direct, toolSearchSpec(deferred), toolCallSpec(deferred))
	exec = append(append([]tools.Spec(nil), direct...), deferred...)
	if s.deferredCatalog == nil {
		c := deferredCatalog(deferred)
		s.deferredCatalog = &c
	}
	return direct, exec, *s.deferredCatalog
}

// deferredCatalog is one line per deferred tool: name — short description.
func deferredCatalog(deferred []tools.Spec) string {
	var sb strings.Builder
	for _, sp := range deferred {
		d := strings.Join(strings.Fields(sp.Description), " ")
		if r := []rune(d); len(r) > catalogLineMax {
			d = string(r[:catalogLineMax-1]) + "…"
		}
		fmt.Fprintf(&sb, "- %s — %s\n", sp.Name, d)
	}
	return sb.String()
}

// paramNames lists a schema's property names.
func paramNames(schema map[string]any) []string {
	props, _ := schema["properties"].(map[string]any)
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// searchTools scores the deferred tools against the query: each query word
// found in the name counts 3, in the description or a parameter name 1. An
// exact name wins outright.
func searchTools(deferred []tools.Spec, query string, limit int) []tools.Spec {
	q := strings.ToLower(strings.TrimSpace(query))
	for _, sp := range deferred {
		if strings.ToLower(sp.Name) == q {
			return []tools.Spec{sp}
		}
	}
	terms := strings.FieldsFunc(q, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	type hit struct {
		sp    tools.Spec
		score int
	}
	var hits []hit
	for _, sp := range deferred {
		name := strings.ToLower(sp.Name)
		text := strings.ToLower(sp.Description + " " + strings.Join(paramNames(sp.Schema), " "))
		score := 0
		for _, t := range terms {
			if strings.Contains(name, t) {
				score += 3
			}
			if strings.Contains(text, t) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{sp, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]tools.Spec, 0, limit)
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].sp)
	}
	return out
}

func toolSearchSpec(deferred []tools.Spec) tools.Spec {
	return tools.Spec{
		Name: toolSearchName,
		Description: "Finds tools that are not loaded directly (MCP server tools and harness management: mcp_*, skill_create/edit/remove, instructions_*). " +
			"Give keywords or an exact tool name; returns each match's name, description and full parameter schema. " +
			"Then run the tool with tool_call. The deferred tools are listed in the system prompt and in [system update] messages.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "keywords (e.g. \"browser click\") or an exact tool name"},
				"limit": map[string]any{"type": "integer", "description": "max results (default 5)"},
			},
			"required": []string{"query"},
		},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				Query string `json:"query"`
				Limit int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Limit <= 0 {
				c.Limit = 5
			}
			found := searchTools(deferred, c.Query, c.Limit)
			if len(found) == 0 {
				names := make([]string, 0, len(deferred))
				for _, sp := range deferred {
					names = append(names, sp.Name)
				}
				return fmt.Sprintf("no tool matches %q. Deferred tools: %s", c.Query, strings.Join(names, ", ")), nil
			}
			var sb strings.Builder
			for _, sp := range found {
				schema, _ := json.Marshal(sp.Schema)
				fmt.Fprintf(&sb, "## %s\n%s\nparameters: %s\n\n", sp.Name, sp.Description, schema)
			}
			sb.WriteString("Run one with tool_call {\"name\": ..., \"arguments\": {...}}.")
			return sb.String(), nil
		},
	}
}

// callArgs extracts tool_call's target and arguments; arguments may come as
// an object or as a JSON string.
func callArgs(raw json.RawMessage) (string, json.RawMessage, error) {
	var c struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return "", nil, fmt.Errorf("invalid input: %w", err)
	}
	args := c.Arguments
	var asString string
	if json.Unmarshal(args, &asString) == nil {
		args = json.RawMessage(asString)
	}
	if len(strings.TrimSpace(string(args))) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	return strings.TrimSpace(c.Name), args, nil
}

// missingRequired lists the schema's required fields absent from args.
func missingRequired(schema map[string]any, args json.RawMessage) []string {
	var got map[string]any
	if json.Unmarshal(args, &got) != nil {
		return nil
	}
	var req []string
	switch r := schema["required"].(type) {
	case []string:
		req = r
	case []any:
		for _, v := range r {
			if s, ok := v.(string); ok {
				req = append(req, s)
			}
		}
	}
	var out []string
	for _, k := range req {
		if _, ok := got[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

func toolCallSpec(deferred []tools.Spec) tools.Spec {
	return tools.Spec{
		Name: toolCallName,
		Description: "Runs a deferred tool (one found with tool_search or listed in the system prompt) by its exact name. " +
			"arguments is the object the tool's parameter schema describes. Independent tool_call calls run in parallel like any other tool.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"type": "string", "description": "exact tool name"},
				"arguments": map[string]any{"type": "object", "description": "the tool's arguments"},
			},
			"required": []string{"name"},
		},
		Exec: func(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
			name, args, err := callArgs(raw)
			if err != nil {
				return "", err
			}
			for _, sp := range deferred {
				if sp.Name != name {
					continue
				}
				if miss := missingRequired(sp.Schema, args); len(miss) > 0 {
					schema, _ := json.Marshal(sp.Schema)
					// Models that send only {"name": X} (step-5-preview, Bruce
					// 2026-10-25: four okf_create calls in a row without arguments)
					// need the shape spelled out, not just the schema.
					return "", fmt.Errorf("%s needs %s; call it again as a real tool call: tool_call {\"name\": %q, \"arguments\": {%s}} — the arguments object is required, never write the call as text; parameters: %s",
						name, strings.Join(miss, ", "), name, argsSkeleton(miss), schema)
				}
				return sp.Exec(ctx, cwd, args)
			}
			return "", fmt.Errorf("no deferred tool named %q; find it with tool_search", name)
		},
	}
}

// shownCall is what the screen and the parallelism rule see for a call:
// tool_call is unwrapped into the tool it runs.
func shownCall(name, args string) (string, string) {
	if name != toolCallName {
		return name, args
	}
	inner, a, err := callArgs(json.RawMessage(args))
	if err != nil || inner == "" {
		return name, args
	}
	return inner, string(a)
}

// argsSkeleton renders the missing fields as `"a": ..., "b": ...` for the
// error message.
func argsSkeleton(fields []string) string {
	parts := make([]string, len(fields))
	for i, f := range fields {
		parts[i] = fmt.Sprintf("%q: ...", f)
	}
	return strings.Join(parts, ", ")
}
