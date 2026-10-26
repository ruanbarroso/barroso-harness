// Package config holds the harness's persistent configuration. Source of
// truth on disk: ~/.barroso-harness/config.json, with the active provider in
// current_provider + providers (a map, so switching never erases credentials).
// Credentials live in THIS file, with restricted permissions — never in code,
// README or logs. BARROSO_HARNESS_HOME relocates the data directory, and
// BARROSO_BASE_URL / BARROSO_AUTH_TOKEN override the active provider for the
// process (like ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN in Claude Code)
// without ever being written to disk.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that override the active provider. The token goes
// out as "Authorization: Bearer <token>", like every other key.
const (
	EnvBaseURL   = "BARROSO_BASE_URL"
	EnvAuthToken = "BARROSO_AUTH_TOKEN"
)

// DefaultBaseURL is the in-house default gateway (OpenAI Chat Completions
// style, with /v1 at the end).
const DefaultBaseURL = "https://keys.barroso.tec.br/v1"

// First-install defaults: with no config.json and no BARROSO_* env, the
// harness starts on the OpenCode Zen gateway with its public key and
// big-pickle — the free model there that stays up (the others come and go).
const (
	ZenProvider = "opencode-zen"
	ZenBaseURL  = "https://opencode.ai/zen/v1"
	ZenAPIKey   = "public"
	ZenModel    = "big-pickle"
	ZenJanela   = 200000 // models.opencode.ai: limit.context of big-pickle
)

// Padrao is the ready-to-run config of a first install (see ZenProvider).
// big-pickle publishes no reasoning_options, so effort goes out omitted.
func Padrao() Config {
	return Config{
		CurrentProvider: ZenProvider,
		Providers:       map[string]*Provider{ZenProvider: {BaseURL: ZenBaseURL, APIKey: ZenAPIKey}},
		Model:           ZenModel,
		Effort:          "none",
		Janela:          ZenJanela,
		JanelaSource:    FonteCatalogo,
		BaseURL:         ZenBaseURL,
		APIKey:          ZenAPIKey,
		Home:            Dir(),
	}
}

// envSet reports whether BARROSO_BASE_URL or BARROSO_AUTH_TOKEN is set.
func envSet() bool {
	return strings.TrimSpace(os.Getenv(EnvBaseURL)) != "" || strings.TrimSpace(os.Getenv(EnvAuthToken)) != ""
}

// Possible sources of the context window:
//   - FonteCatalogo: value derived from the provider's /models (switching
//     models may update or clear it).
//   - FonteManual: value written by hand in config.json (never cleared
//     automatically).
const (
	FonteCatalogo = "catalog"
	FonteManual   = "manual"
)

// Provider is a Chat Completions endpoint: base URL + key.
type Provider struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	// DisableParallelToolCalls turns parallel tool calls off for this
	// provider only (see Config.DisableParallelToolCalls).
	DisableParallelToolCalls bool `json:"disable_parallel_tool_calls,omitempty"`
}

// Config is the persisted harness configuration.
type Config struct {
	CurrentProvider string               `json:"current_provider"`
	Providers       map[string]*Provider `json:"providers,omitempty"`
	Model           string               `json:"model"`
	Effort          string               `json:"effort,omitempty"`                // "none" = the dealer omits the field; absent = DefaultEffort
	Janela          int                  `json:"context_window"`                  // window in tokens; 0 = unknown (guardrail off)
	JanelaSource    string               `json:"context_window_source,omitempty"` // FonteCatalogo | FonteManual | ""
	MaxTokens       int                  `json:"max_tokens"`                      // reserved for the provider; 0 = dealer default
	BytesPorToken   int                  `json:"bytes_per_token"`                 // manual calibration; 0 = default estimator (4)
	// Provider watchdog, in seconds. Absent = dealer default (150 s to the
	// first byte, 300 s of stream silence); 0 = disabled. There is no total
	// deadline by design — only silence is bounded.
	FirstByteSecs *int   `json:"provider_first_byte_secs,omitempty"`
	IdleSecs      *int   `json:"provider_idle_secs,omitempty"`
	BaseURL       string `json:"-"` // resolved from the active provider
	APIKey        string `json:"-"` // resolved from the active provider; never log
	Home          string `json:"-"`

	// envOverride is the provider built from BARROSO_BASE_URL /
	// BARROSO_AUTH_TOKEN. It wins over providers/current_provider and lives
	// only in memory: Save never sees it, so the env never leaks to disk.
	envOverride *Provider

	// SearchURL is an optional SearXNG endpoint with the JSON API enabled.
	// When set, websearch tries it FIRST; empty does not disable anything —
	// search goes to the hosted providers and then DuckDuckGo lite.
	SearchURL string `json:"search_url,omitempty"`

	// SearchProviders orders the anonymous hosted MCP providers used by
	// websearch and webfetch ("exa", "parallel"). Absent = both, in that
	// order; ["off"] (or any list without a known name) disables them. Only
	// names: the endpoints are fixed in package web, never configurable.
	SearchProviders []string `json:"search_providers,omitempty"`

	// MCPServidores lists MCP servers (stdio or streamable HTTP). The tools
	// they publish enter the turn's catalog.
	MCPServidores []MCPServidor `json:"mcp_servers,omitempty"`

	// PoliticaContexto chooses who decides to compact: "agente" (default; the
	// model calls context_compact at a task milestone, with the guardrail as a
	// safety net) or "limiar" (threshold: only the guardrail, when the window
	// overflows). These values are part of the on-disk format.
	PoliticaContexto string `json:"context_policy,omitempty"`

	// ToolDisclosure: "auto" (default; progressive past ~8k tokens of tool
	// schemas), "full" (every tool in every request) or "progressive" (MCP
	// and management tools behind tool_search/tool_call).
	ToolDisclosure string `json:"tool_disclosure,omitempty"`

	// DisableParallelToolCalls asks OpenAI-compatible providers to emit only
	// one tool call per response. Useful when a gateway's parallel-call
	// encoding is broken; absent keeps the protocol default. Prefer the
	// per-provider field: this one applies to every provider.
	DisableParallelToolCalls bool `json:"disable_parallel_tool_calls,omitempty"`

	// PromptHooks run (in parallel) before every user prompt of a persistent
	// session. The contract is compatible with Claude Code's UserPromptSubmit,
	// minus the transcript fields (the history lives in SQLite, there is no
	// transcript_path): JSON on stdin ({"hook_event_name", "prompt", "cwd",
	// "session_id", "permission_mode": "bypassPermissions"}). On exit 0,
	// stdout is hookSpecificOutput.additionalContext or plain text, sent to
	// the model beside the prompt (up to 10,000 chars); decision:"block" or
	// continue:false blocks the prompt, as does exit 2 (stderr is the
	// reason). Any other failure or the timeout skips the hook.
	PromptHooks []PromptHook `json:"prompt_hooks,omitempty"`
}

// PromptHook is one command of PromptHooks.
type PromptHook struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Timeout int      `json:"timeout_segundos,omitempty"` // seconds; 0 = 10
}

// MCPServidor mirrors mcp.Servidor without importing the package — config is
// the root of the dependency tree and must not point to a package that speaks
// a protocol. Converting to mcp.Servidor is one line in the wiring.
type MCPServidor struct {
	Nome    string            `json:"name"`
	Tipo    string            `json:"type"` // "stdio" | "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout int               `json:"timeout_segundos,omitempty"` // seconds; 0 = default
}

// BuscaAtiva reports whether a SearXNG endpoint (search_url) is configured.
// Search works without one (hosted providers, then DuckDuckGo); this only
// tells whoever needs to KNOW — wiring, UI, tests — whether SearXNG comes
// first, instead of comparing strings.
func (c Config) BuscaAtiva() bool { return strings.TrimSpace(c.SearchURL) != "" }

// validServers returns only the MCP servers with a recognizable name and
// type. FINE-grained validation belongs to the mcp package (it knows the
// protocol); here we only drop what would never open, so the wiring does not
// try.
func (c Config) validServers() []MCPServidor {
	var out []MCPServidor
	seen := map[string]bool{}
	for _, s := range c.MCPServidores {
		name := strings.TrimSpace(s.Nome)
		if name == "" || seen[name] {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(s.Tipo))
		kind = strings.NewReplacer("_", "-", " ", "-").Replace(kind)
		switch {
		case kind == "stdio":
			if strings.TrimSpace(s.Command) == "" {
				continue
			}
		case kind == "http", kind == "streamable-http", kind == "http-streamable",
			kind == "http-streaming", kind == "streamablehttp":
			if strings.TrimSpace(s.URL) == "" {
				continue
			}
		default:
			continue
		}
		seen[name] = true
		out = append(out, s)
	}
	return out
}

// Dir is the harness data root (~/.barroso-harness by default).
func Dir() string {
	if v := os.Getenv("BARROSO_HARNESS_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home, _ = os.Getwd()
	}
	return filepath.Join(home, ".barroso-harness")
}

// Provider returns the provider with the given name (nil if it does not exist).
func (c *Config) Provider(name string) *Provider {
	if c.Providers == nil || name == "" {
		return nil
	}
	return c.Providers[name]
}

// Ativo returns the current provider (nil if none is selected). The in-memory
// BaseURL/APIKey fields keep compatibility with older callers; they are never
// serialized and the disk keeps using providers/current_provider.
func (c *Config) Ativo() *Provider {
	if c.envOverride != nil {
		return c.envOverride
	}
	if p := c.Provider(c.CurrentProvider); p != nil {
		return p
	}
	if c.BaseURL != "" || c.APIKey != "" {
		return &Provider{BaseURL: c.BaseURL, APIKey: c.APIKey}
	}
	return nil
}

// ParallelToolCallsOff reports whether the active provider must get one tool
// call per response: the global setting or the active provider's own.
func (c *Config) ParallelToolCallsOff() bool {
	if c.DisableParallelToolCalls {
		return true
	}
	p := c.Ativo()
	return p != nil && p.DisableParallelToolCalls
}

// SelecionarProvider switches the active provider, recomputing the window
// when it came from the catalog (a manual window is never overwritten).
func (c *Config) SelecionarProvider(name string, window int) {
	c.CurrentProvider = name
	if c.JanelaSource == FonteCatalogo {
		c.Janela = window
		if c.Janela <= 0 {
			c.JanelaSource = ""
		}
	}
}

// Faltas lists what is missing for the config to be ready (used in
// actionable messages; never contains the key).
func (c Config) Faltas() []string {
	var missing []string
	p := c.Ativo()
	if p == nil {
		missing = append(missing, "provider (base_url + api_key)")
	} else {
		if p.BaseURL == "" {
			missing = append(missing, "base_url")
		}
		if p.APIKey == "" {
			missing = append(missing, "api_key")
		}
	}
	if c.Model == "" {
		missing = append(missing, "model")
	}
	return missing
}

// Pronta reports whether the harness has a provider (URL + key) and a model,
// enough to run a turn without interaction.
func (c Config) Pronta() bool { return len(c.Faltas()) == 0 }

// Resumo is the status line shown in the REPL — WITHOUT the key.
func (c Config) Resumo() string {
	base := ""
	if p := c.Ativo(); p != nil {
		base = p.BaseURL
	}
	line := fmt.Sprintf("provider %q @ %s", c.CurrentProvider, base)
	if c.envOverride != nil {
		line = fmt.Sprintf("provider (env %s/%s) @ %s", EnvBaseURL, EnvAuthToken, base)
	}
	if c.Effort != "" {
		line += " · effort " + c.Effort
	}
	line += fmt.Sprintf(" · model %s", c.Model)
	if c.Janela > 0 {
		line += fmt.Sprintf(" · window %d (%s)", c.Janela, windowSource(c.JanelaSource))
	}
	return line
}

func windowSource(s string) string {
	switch s {
	case FonteCatalogo:
		return FonteCatalogo
	case FonteManual:
		return FonteManual
	default:
		return "unknown"
	}
}

// DefaultEffort is the reasoning_effort of a config that does not set one:
// medium measured fastest in bh's effort bench without extra rounds.
const DefaultEffort = "medium"

// Load reads config.json and overlays BARROSO_BASE_URL / BARROSO_AUTH_TOKEN.
// A missing file is NOT an error: without BARROSO_* env it returns Padrao()
// (OpenCode Zen, ready to run); with env it returns an empty config overlaid
// by it (the interactive path asks only the model).
func Load() (Config, error) {
	c := Config{Home: Dir()}
	path := filepath.Join(c.Home, "config.json")
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !envSet() {
			return Padrao(), nil
		}
		c.applyEnv()
		return c, nil
	case err != nil:
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("invalid config %s: %w", path, err)
	}
	migrateLegacyShape(b, &c)
	// No "effort" key at all: the default level. "none" is the explicit
	// "send no reasoning_effort" (what /effort - stores).
	var keys map[string]json.RawMessage
	if json.Unmarshal(b, &keys) == nil {
		if _, ok := keys["effort"]; !ok {
			c.Effort = DefaultEffort
		}
	}
	c.Home = Dir()
	if p := c.Provider(c.CurrentProvider); p != nil {
		c.BaseURL, c.APIKey = p.BaseURL, p.APIKey
	}
	c.applyEnv()
	return c, nil
}

// FromEnv reports whether the active provider comes from BARROSO_BASE_URL /
// BARROSO_AUTH_TOKEN instead of config.json.
func (c Config) FromEnv() bool { return c.envOverride != nil }

// applyEnv overlays BARROSO_BASE_URL / BARROSO_AUTH_TOKEN on the active
// provider. Either one alone is enough: a missing URL falls back to the
// configured provider (or DefaultBaseURL), a missing token to its key.
func (c *Config) applyEnv() {
	base := strings.TrimSpace(os.Getenv(EnvBaseURL))
	token := strings.TrimSpace(os.Getenv(EnvAuthToken))
	if base == "" && token == "" {
		return
	}
	p := Provider{BaseURL: DefaultBaseURL}
	if cur := c.Ativo(); cur != nil {
		p = *cur
		if p.BaseURL == "" {
			p.BaseURL = DefaultBaseURL
		}
	}
	if base != "" {
		p.BaseURL = strings.TrimSuffix(base, "/")
	}
	if token != "" {
		p.APIKey = token
	}
	c.envOverride = &p
	c.BaseURL, c.APIKey = p.BaseURL, p.APIKey
}

// migrateLegacyShape converts the slice-0 config ({base_url, model?} at the
// root, key from env) to the canonical shape with a named provider.
func migrateLegacyShape(b []byte, c *Config) {
	if c.CurrentProvider != "" || len(c.Providers) != 0 {
		return // already in the new shape
	}
	var legacy struct {
		BaseURL string `json:"base_url"`
		Model   string `json:"model"`
	}
	if err := json.Unmarshal(b, &legacy); err != nil || legacy.BaseURL == "" {
		return
	}
	name := "keys.barroso"
	if strings.TrimSuffix(legacy.BaseURL, "/") != DefaultBaseURL {
		name = "default"
	}
	c.Providers = map[string]*Provider{
		name: {BaseURL: strings.TrimSuffix(legacy.BaseURL, "/"), APIKey: ""},
	}
	c.CurrentProvider = name
	if c.Model == "" {
		c.Model = legacy.Model
	}
	if c.JanelaSource == "" && c.Janela > 0 {
		c.JanelaSource = FonteManual
	}
}

// Save persists the config atomically: a temp file in the same directory
// (0600 permissions), renamed over the target. The directory is created 0700.
func (c Config) Save() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	c.Home = dir
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	final := filepath.Join(dir, "config.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	_ = os.Chmod(tmp, 0o600) // best-effort; Windows ignores POSIX bits
	if err := atomicReplace(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Init creates the data layout NON-destructively: a 0700 directory, a sample
// global BARROSO.md and a config.json — Padrao() without BARROSO_* env, an
// empty skeleton (completed by `bh` onboarding) otherwise.
// Nothing that already exists is overwritten.
func Init() error {
	dir := Dir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	barPath := filepath.Join(dir, "BARROSO.md")
	if _, err := os.Stat(barPath); errors.Is(err, os.ErrNotExist) {
		sample := "# Global BARROSO.md\n\nInstructions that apply to ALL harness sessions, in any directory.\n" +
			"A per-project BARROSO.md (in the project folder or its parents) comes before this one.\n"
		if err := os.WriteFile(barPath, []byte(sample), 0o644); err != nil {
			return err
		}
	}
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) {
		if !envSet() {
			return Padrao().Save()
		}
		return Config{Home: dir}.Save()
	}
	return nil
}
