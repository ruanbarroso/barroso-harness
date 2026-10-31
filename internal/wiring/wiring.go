// Package wiring builds the session's extended catalog: what is NOT built-in
// and needs a process-level resource — the sub-agent manager and MCP servers.
//
// It exists so that the three entry points (REPL, -p mode and the ACP agent)
// build the SAME tool list from ONE recipe. Without this package, every caller
// would repeat the sequence "connect MCP, create manager, inject specs,
// remember to close" — and the step one forgets is the tool the model never sees.
//
// Why not in runtime: runtime is imported by internal/subagent (the child
// agent is born from a runtime.Session). Putting subagent/MCP inside it would
// be an import cycle. The runtime handles the independent half — live context,
// web and skills, see internal/runtime/extras.go — and this package handles
// the externally-owned half, delivering the result through the same entry
// point: Session.Injetar.
package wiring

import (
	"context"
	"fmt"
	"strings"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/mcp"
	"github.com/ruanbarroso/barroso-harness/internal/pricing"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// Conjunto is what the session gained: the already-injected specs and the
// resources that must be CLOSED at the end of the process. A Conjunto with no
// open resource (no MCP server; or the Conjunto{} of a nil session) is valid —
// Fechar is a no-op. The sub-agent manager is ALWAYS created: every main
// session has subagent/subagent_status/subagent_result; the CHILD session does not
// get them because the runtime builds it from tools.SpecsBuiltins() (an
// invariant by construction, not by configuration).
type Conjunto struct {
	manager    *subagent.Gerente // always present when Montar went through a real session
	mcpManager *mcpManager       // MCP servers (connected or not) and their tools
	jobs       *tools.JobSet     // background shell jobs of the session
}

// Jobs returns the set that reports background shell jobs ending (nil in an
// empty Conjunto).
func (c *Conjunto) Jobs() *tools.JobSet {
	if c == nil {
		return nil
	}
	return c.jobs
}

// Gerente returns the sibling-task manager (nil only in an empty Conjunto —
// nil session). Useful for the UI to show counts and for tests.
func (c *Conjunto) Gerente() *subagent.Gerente {
	if c == nil {
		return nil
	}
	return c.manager
}

// MCP returns the connected MCP clients, in the order of the config's servers.
func (c *Conjunto) MCP() []*mcp.Cliente {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	return c.mcpManager.clients()
}

// Montar connects the MCP servers, starts the sub-agent manager and injects
// everything into the session. Returns the Conjunto to close later and the
// WARNINGS — one line per failed server, for the caller to show without
// interrupting.
//
// Tolerance rule: failure of ONE MCP server invalidates neither the others nor
// the turn. An external tool is a convenience; bringing down the session
// because a subprocess did not start would trade a small problem for a big one.
//
// ctx must be LONG-LIVED (not a turn's): it is inherited by the sub-agents,
// which must outlive the turn that created them, and it is the MCP handshake
// deadline. The REPL passes a ctx that is only cancelled at process exit.
func Montar(ctx context.Context, cfg config.Config, sess *runtime.Session, cwd string) (*Conjunto, []string) {
	if sess == nil {
		return &Conjunto{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var warnings []string

	// ── MCP ────────────────────────────────────────────────────────────────
	// The manager connects the config's servers (a failure becomes a warning)
	// and publishes mcp_list/mcp_add/mcp_remove: the agent manages its own
	// servers without hand-editing config.json.
	mm := newMCPManager(ctx, sess)
	seen := map[string]bool{}
	for _, s := range cfg.MCPServidores {
		name := strings.TrimSpace(s.Nome)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		e := mm.connect(s)
		mm.register(e)
		if e.err != "" {
			warnings = append(warnings, fmt.Sprintf("MCP server %q: %s", name, e.err))
		}
	}
	sess.Injetar(mm.specs()...)
	mm.reconcile() // baseline: config.json as it is now
	sess.Observar(mm.reconcile)

	// ── sub-agents ─────────────────────────────────────────────────────────
	// ALWAYS on, no config flag: delegating is part of the main session's base
	// catalog. The child does not inherit delegation by construction — the
	// runtime.Session.Send builds its list from tools.SpecsBuiltins(), which
	// has no subagent_* — so there is no recursion risk to manage.
	manager := subagent.Novo(subagent.Config{Nome: subagentName()})
	manager.DefinirCwd(cwd)
	// Bind to the long-lived ctx BEFORE the first Spawn: tasks derive their
	// child context from here, and the turn ctx dies at the end of the line.
	manager.Vincular(ctx)
	sess.Injetar(subagent.Especificacoes(manager, cfg)...)

	// ── prices (optional) ─────────────────────────────────────────────────
	// prices.json in the harness home, hand-declared. A missing or invalid
	// file is NOT an error: bh simply never shows a money cost and tokens
	// stay the metric. Nothing here can block a session.
	if precos, err := pricing.Load(cfg.Home); err == nil && !precos.Empty() {
		sess.DefinirPrecos(precos)
	}

	// ── background shell jobs ──────────────────────────────────────────────
	// The shell tool reports a background job's end to this set (found in the
	// tool ctx); the host turns it into a notice turn.
	js := tools.NewJobSet()
	sess.DefinirJobs(js)

	return &Conjunto{manager: manager, mcpManager: mm, jobs: js}, warnings
}

// Fechar shuts down the MCP servers. Sub-agents have no shutdown: the registry
// is in memory and tasks die with the process (task persistence is outside
// this slice). Returns nil when there is no open client.
func (c *Conjunto) Fechar() error {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	return c.mcpManager.close()
}

// ── config → mcp conversion ───────────────────────────────────────────────

// serversFrom translates the config into the mcp package type, filtering out
// what certainly would not open. Fine-grained validation belongs to mcp (which
// knows the protocol); here we only avoid trying to open a server without a
// unique name, without command/url or with an unrecognizable type — each of
// those would become a pointless warning line.
//
// The translation is field by field, not a cast: config cannot import mcp (it
// is the root of the dependency tree), so the two types are independent by
// design and this function is the single boundary between them.
//
// The type rule is the SAME as config.servidoresValidos (which the REPL uses
// for listing): "streamable-http" and the name variants map to "http", the
// only HTTP transport the client implements.
func serversFrom(cfgs []config.MCPServidor) []mcp.Servidor {
	var out []mcp.Servidor
	seen := map[string]bool{}
	for _, s := range cfgs {
		if !acceptable(s) || seen[strings.TrimSpace(s.Nome)] {
			continue
		}
		seen[strings.TrimSpace(s.Nome)] = true
		out = append(out, mcp.Servidor{
			Nome:    strings.TrimSpace(s.Nome),
			Tipo:    mcpType(s.Tipo),
			Command: strings.TrimSpace(s.Command),
			Args:    s.Args,
			Env:     s.Env,
			URL:     strings.TrimSpace(s.URL),
			Headers: s.Headers,
			Timeout: s.Timeout,
		})
	}
	return out
}

// acceptable is the minimal filter: a name present and the field the
// transport requires. An unknown type is rejected here instead of becoming a
// per-server error.
func acceptable(s config.MCPServidor) bool {
	if strings.TrimSpace(s.Nome) == "" {
		return false
	}
	switch mcpType(s.Tipo) {
	case "stdio":
		return strings.TrimSpace(s.Command) != ""
	case "http":
		return strings.TrimSpace(s.URL) != ""
	default:
		return false
	}
}

// mcpType normalizes the transport name to the form the mcp package expects
// ("stdio" | "http"); returns "" when it is neither.
func mcpType(t string) string {
	n := strings.ToLower(strings.TrimSpace(t))
	n = strings.NewReplacer("_", "-", " ", "-").Replace(n)
	switch n {
	case "stdio":
		return "stdio"
	case "http", "streamable-http", "http-streamable", "http-streaming", "streamablehttp":
		return "http"
	default:
		return ""
	}
}

// subagentName returns the subagent family name (the launcher tool's name
// "subagent", also the prefix of subagent_status/subagent_result). Fixed, not
// derived from the model: a configurable
// prefix only matters to someone running two managers in the SAME catalog, and
// there the risk of ambiguous tool names outweighs the gain.
func subagentName() string { return subagent.PrefixoPadrao }

// oneLine reduces an error to ONE line: a server warning is a REPL line, not
// a block.
func oneLine(err error) string {
	if err == nil {
		return "(no detail)"
	}
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const limit = 160
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}
