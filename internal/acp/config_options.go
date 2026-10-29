// Session config options (ACP session-config-options): the model and the
// reasoning effort of a session, advertised on session/new and session/load as
// configOptions and changed with session/set_config_option. session/set_model
// (the older "models" shape) is served too, for clients that only know it.
//
// A change applies to THAT session only (runtime.Session.Configure) and is not
// written to config.json: the connector applies the agent's choice to every
// session it opens, and a global write would leak one agent's model into
// another's process.
package acp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

const (
	MetSetConfig = "session/set_config_option"
	MetSetModel  = "session/set_model"

	optModel  = "model"
	optEffort = "effort"
)

// effortLevels are the reasoning_effort values offered; "none" = omit the
// field (the same meaning config.Effort gives it). Same list as the REPL's
// /effort.
var effortLevels = []string{"none", "low", "medium", "high", "xhigh", "max"}

// configurable is what a Turno needs to expose model/effort. runtime.Session
// satisfies it; a fake turn that does not simply has no options to change.
type configurable interface {
	Config() config.Config
	Configure(config.Config)
}

// catalogTTL bounds how long the provider's /models answer is reused.
const catalogTTL = 10 * time.Minute

type catalogCache struct {
	mu     sync.Mutex
	key    string
	at     time.Time
	models []dealer.CatalogModel
}

// models returns the provider's chat catalog, cached per base URL. A failure
// is not fatal: the options still list the current model, so the client can
// see (if not change) what the session uses.
func (a *Agente) models(cfg config.Config) []dealer.CatalogModel {
	base, key := "", ""
	if p := cfg.Ativo(); p != nil {
		base, key = p.BaseURL, p.APIKey
	}
	if base == "" {
		return nil
	}
	c := &a.catalog
	c.mu.Lock()
	defer c.mu.Unlock()
	ck := dealer.NormalizarURL(base) + "\x00" + key
	if c.key == ck && time.Since(c.at) < catalogTTL && len(c.models) > 0 {
		return c.models
	}
	fetch := a.Catalogo
	if fetch == nil {
		fetch = dealer.Modelos
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ls, err := fetch(ctx, base, key)
	if err != nil {
		a.logf("acp: model catalog unavailable: %v", err)
		if c.key == ck {
			return c.models // stale is better than nothing for this provider
		}
		return nil
	}
	c.key, c.at, c.models = ck, time.Now(), ls
	return ls
}

// sessionConfig is the config the session runs with: its own when the turn
// exposes it, otherwise the agent's.
func (a *Agente) sessionConfig(s *session) (config.Config, configurable) {
	if c, ok := s.turn.(configurable); ok {
		return c.Config(), c
	}
	return a.cfg, nil
}

func currentEffort(cfg config.Config) string {
	e := strings.TrimSpace(cfg.Effort)
	if e == "" {
		return "none"
	}
	return e
}

// configOptions builds the ACP configOptions array for a session config.
func (a *Agente) configOptions(cfg config.Config) []map[string]any {
	var models []map[string]any
	seen := map[string]bool{}
	for _, m := range a.models(cfg) {
		seen[m.ID] = true
		models = append(models, map[string]any{"value": m.ID, "name": m.ID})
	}
	if cfg.Model != "" && !seen[cfg.Model] {
		models = append([]map[string]any{{"value": cfg.Model, "name": cfg.Model}}, models...)
	}
	effort := currentEffort(cfg)
	var efforts []map[string]any
	known := false
	for _, e := range effortLevels {
		known = known || e == effort
		efforts = append(efforts, map[string]any{"value": e, "name": e})
	}
	if !known {
		efforts = append(efforts, map[string]any{"value": effort, "name": effort})
	}
	return []map[string]any{
		{"id": optModel, "name": "Model", "category": "model", "type": "select", "currentValue": cfg.Model, "options": models},
		{"id": optEffort, "name": "Reasoning effort", "category": "thought_level", "type": "select", "currentValue": effort, "options": efforts},
	}
}

// modelsState is the older ACP "models" shape (currentModelId/availableModels).
func (a *Agente) modelsState(cfg config.Config) map[string]any {
	var avail []map[string]any
	for _, o := range a.configOptions(cfg)[0]["options"].([]map[string]any) {
		avail = append(avail, map[string]any{"modelId": o["value"], "name": o["name"]})
	}
	return map[string]any{"currentModelId": cfg.Model, "availableModels": avail}
}

// syncCatalogWindow fills a missing (or stale catalog-derived) context window from
// the active provider's /models response. Explicit/manual windows are never overwritten.
func (a *Agente) syncCatalogWindow(s *session) {
	cfg, c := a.sessionConfig(s)
	if c == nil || (cfg.Janela > 0 && cfg.JanelaSource != config.FonteCatalogo) {
		return
	}
	window := 0
	for _, model := range a.models(cfg) {
		if model.ID == cfg.Model {
			window = model.ContextLength
			break
		}
	}
	if cfg.Janela == window && (window == 0 || cfg.JanelaSource == config.FonteCatalogo) {
		return
	}
	cfg.Janela = window
	cfg.JanelaSource = ""
	if window > 0 {
		cfg.JanelaSource = config.FonteCatalogo
	}
	c.Configure(cfg)
	a.logf("acp: session %s: context window = %d (catalog)", s.id, window)
}

// sessionState is what session/new and session/load add to their result.
func (a *Agente) sessionState(s *session) map[string]any {
	a.syncCatalogWindow(s)
	cfg, _ := a.sessionConfig(s)
	return map[string]any{
		"configOptions": a.configOptions(cfg),
		"models":        a.modelsState(cfg),
	}
}

// applyOption changes one option on the session; it returns an error message
// for the client ("" = ok).
func (a *Agente) applyOption(s *session, id, value string) string {
	a.mu.Lock()
	running := s.cancel != nil
	a.mu.Unlock()
	if running {
		return "turn in progress in this session; change the option after it ends"
	}
	cfg, c := a.sessionConfig(s)
	if c == nil {
		return "this session does not accept configuration changes"
	}
	value = strings.TrimSpace(value)
	switch id {
	case optModel:
		if value == "" {
			return "invalid value: empty model"
		}
		ls := a.models(cfg)
		window := 0
		found := value == cfg.Model
		for _, m := range ls {
			if m.ID == value {
				found, window = true, m.ContextLength
				break
			}
		}
		if !found && len(ls) > 0 {
			return "model not in the provider's catalog: " + value
		}
		if value != cfg.Model {
			cfg.Model = value
			// Same rule as the REPL's /model: a manual window stays, a
			// catalog-derived one follows the new model.
			if !(cfg.JanelaSource == config.FonteManual && cfg.Janela > 0) {
				if window > 0 {
					cfg.Janela, cfg.JanelaSource = window, config.FonteCatalogo
				} else {
					cfg.Janela, cfg.JanelaSource = 0, ""
				}
			}
		}
	case optEffort:
		switch strings.ToLower(value) {
		case "", "-", "off", "clear", "none":
			value = "none"
		}
		cfg.Effort = value
	default:
		return "unknown config option: " + id
	}
	c.Configure(cfg)
	a.logf("acp: session %s: %s = %s", s.id, id, value)
	return ""
}

func (a *Agente) setConfigOption(w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     any    `json:"value"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" || p.ConfigID == "" {
		return WriteError(w, req.ID, CodParams, "invalid params: sessionId and configId are required")
	}
	s, ok := a.lookupSession(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "unknown session: "+p.SessionID)
	}
	value, _ := p.Value.(string)
	if msg := a.applyOption(s, p.ConfigID, value); msg != "" {
		return WriteError(w, req.ID, CodParams, msg)
	}
	cfg, _ := a.sessionConfig(s)
	return a.respond(w, NovaResposta(req.ID, map[string]any{"configOptions": a.configOptions(cfg)}))
}

func (a *Agente) setModel(w io.Writer, req Request) error {
	var p struct {
		SessionID string `json:"sessionId"`
		ModelID   string `json:"modelId"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return WriteError(w, req.ID, CodParams, "invalid params: "+err.Error())
		}
	}
	if p.SessionID == "" || p.ModelID == "" {
		return WriteError(w, req.ID, CodParams, "invalid params: sessionId and modelId are required")
	}
	s, ok := a.lookupSession(p.SessionID)
	if !ok {
		return WriteError(w, req.ID, CodErroAgente, "unknown session: "+p.SessionID)
	}
	if msg := a.applyOption(s, optModel, p.ModelID); msg != "" {
		return WriteError(w, req.ID, CodParams, msg)
	}
	return a.respond(w, NovaResposta(req.ID, map[string]any{}))
}
