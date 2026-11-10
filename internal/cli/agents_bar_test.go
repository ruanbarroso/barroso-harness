package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

func TestAgentsBarListsLiveSubAgentsAndOpensOnClick(t *testing.T) {
	noColors(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	cfg := config.Config{
		CurrentProvider: "fake",
		Providers: map[string]*config.Provider{
			"fake": {BaseURL: srv.URL, APIKey: "k", Protocol: config.ProtocoloChatCompletions},
		},
		Model:     "fake-model",
		Janela:    100_000,
		MaxTokens: 512,
	}
	g := subagent.Novo(subagent.Config{})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	id, err := g.Spawn(context.Background(), "investigate the parser")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		g.Cancelar(id)
		close(release)
		srv.Close()
	})

	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.Adicionar("hello")
	sc.src = &dialogSource{gerente: g}

	f := sc.frameFor()
	if f.agentN != 2 {
		t.Fatalf("the bar should reserve main plus one agent row, got %d", f.agentN)
	}
	if f.footerRow != 40-1-f.agentN {
		t.Fatalf("the footer should sit above the bar, got row %d", f.footerRow)
	}

	sc.drawLocked()
	region := sc.hits.at(f.agentRow+1, 5)
	if region == nil || region.action == nil {
		t.Fatalf("the agent row should answer a click: %+v", region)
	}
	region.action()
	if sc.dlg == nil || sc.dlg.taskID != id {
		t.Fatalf("clicking the row should open the conversation of %s: %+v", id, sc.dlg)
	}
}

func TestAgentsBarIsHiddenWithoutLiveSubAgents(t *testing.T) {
	noColors(t)
	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.src = &dialogSource{gerente: subagent.Novo(subagent.Config{})}
	if n := sc.frameFor().agentN; n != 0 {
		t.Fatalf("no live sub-agent should reserve no rows, got %d", n)
	}
}
