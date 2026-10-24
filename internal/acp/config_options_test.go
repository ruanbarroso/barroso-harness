package acp

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// cfgTurn is a fake turn that exposes Config/Configure like runtime.Session.
type cfgTurn struct {
	mu  sync.Mutex
	cfg config.Config
}

func (c *cfgTurn) Send(context.Context, runtime.Renderer, string, string) error { return nil }
func (c *cfgTurn) Config() config.Config                                        { c.mu.Lock(); defer c.mu.Unlock(); return c.cfg }
func (c *cfgTurn) Configure(n config.Config)                                    { c.mu.Lock(); c.cfg = n; c.mu.Unlock() }

func setupCfg(t *testing.T, turn *cfgTurn) *harness {
	t.Helper()
	a := Novo(config.Config{})
	a.Fabrica = factory(turn)
	a.RegistrarLog(func(string) {})
	a.Catalogo = func(context.Context, string, string) ([]dealer.CatalogModel, error) {
		return []dealer.CatalogModel{{ID: "a-model", ContextLength: 100000}, {ID: "b-model", ContextLength: 200000}}, nil
	}
	out := &pipe{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Handle(ctx, pr, out) }()
	return &harness{a: a, out: out, input: pw, cancel: cancel}
}

func optionsOf(t *testing.T, res map[string]any) map[string]map[string]any {
	t.Helper()
	b, _ := json.Marshal(res["configOptions"])
	var opts []map[string]any
	if err := json.Unmarshal(b, &opts); err != nil || len(opts) != 2 {
		t.Fatalf("configOptions inválido: %s", b)
	}
	m := map[string]map[string]any{}
	for _, o := range opts {
		m[o["id"].(string)] = o
	}
	return m
}

func TestSessionNewAdvertisesModelAndEffort(t *testing.T) {
	turn := &cfgTurn{cfg: config.Config{Model: "a-model", Effort: "medium", BaseURL: "https://x/v1"}}
	h := setupCfg(t, turn)
	h.send(t, MetSessionNovo, 1, map[string]any{"cwd": t.TempDir()})
	ls := waitLines(t, h.out, 1)
	opts := optionsOf(t, ls[0].Result)
	if opts["model"]["currentValue"] != "a-model" || len(opts["model"]["options"].([]any)) != 2 {
		t.Fatalf("model option: %v", opts["model"])
	}
	if opts["effort"]["currentValue"] != "medium" || opts["effort"]["category"] != "thought_level" {
		t.Fatalf("effort option: %v", opts["effort"])
	}
	if ls[0].Result["models"] == nil {
		t.Fatal("faltou o formato models (currentModelId/availableModels)")
	}
}

func TestSetConfigOptionChangesSession(t *testing.T) {
	turn := &cfgTurn{cfg: config.Config{Model: "a-model", Effort: "medium", BaseURL: "https://x/v1"}}
	h := setupCfg(t, turn)
	h.send(t, MetSessionNovo, 1, map[string]any{"cwd": t.TempDir()})
	waitLines(t, h.out, 1)
	h.send(t, MetSetConfig, 2, map[string]any{"sessionId": "s-deadbeef", "configId": "model", "value": "b-model"})
	h.send(t, MetSetConfig, 3, map[string]any{"sessionId": "s-deadbeef", "configId": "effort", "value": "high"})
	h.send(t, MetSetConfig, 4, map[string]any{"sessionId": "s-deadbeef", "configId": "model", "value": "nope"})
	h.send(t, MetSetModel, 5, map[string]any{"sessionId": "s-deadbeef", "modelId": "a-model"})
	ls := waitLines(t, h.out, 5)
	if ls[1].Error != nil || optionsOf(t, ls[1].Result)["model"]["currentValue"] != "b-model" {
		t.Fatalf("set model: %+v", ls[1])
	}
	if ls[2].Error != nil || optionsOf(t, ls[2].Result)["effort"]["currentValue"] != "high" {
		t.Fatalf("set effort: %+v", ls[2])
	}
	if ls[3].Error == nil {
		t.Fatal("modelo fora do catálogo deveria falhar")
	}
	if ls[4].Error != nil {
		t.Fatalf("set_model: %+v", ls[4].Error)
	}
	c := turn.Config()
	if c.Model != "a-model" || c.Effort != "high" {
		t.Fatalf("config final: model=%q effort=%q", c.Model, c.Effort)
	}
}
