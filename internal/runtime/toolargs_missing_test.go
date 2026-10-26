package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// A call whose required arguments were lost on the way gets an error that
// says so, and the tool never runs.
func TestRunToolMissingArgsExplained(t *testing.T) {
	ran := false
	list := []tools.Spec{{
		Name:   "shell",
		Schema: map[string]any{"type": "object", "required": []string{"command"}},
		Exec: func(context.Context, string, json.RawMessage) (string, error) {
			ran = true
			return "ok", nil
		},
	}}
	for _, args := range []string{"", "  ", "null", "{}"} {
		_, err := runTool(context.Background(), list, t.TempDir(), dealer.ToolChamada{Nome: "shell", Args: args})
		if err == nil || !strings.Contains(err.Error(), "needs command") || !strings.Contains(err.Error(), "call shell again, alone") {
			t.Fatalf("args %q: err = %v", args, err)
		}
	}
	if ran {
		t.Fatal("the tool ran without its required arguments")
	}
	out, err := runTool(context.Background(), list, t.TempDir(), dealer.ToolChamada{Nome: "shell", Args: `{"command":"echo"}`})
	if err != nil || out != "ok" || !ran {
		t.Fatalf("complete call: out=%q err=%v ran=%v", out, err, ran)
	}
}

// A tool without required fields still runs with empty arguments.
func TestRunToolEmptyArgsWithoutRequired(t *testing.T) {
	var got string
	list := []tools.Spec{{
		Name:   "list",
		Schema: map[string]any{"type": "object"},
		Exec: func(_ context.Context, _ string, raw json.RawMessage) (string, error) {
			got = string(raw)
			return "ok", nil
		},
	}}
	if _, err := runTool(context.Background(), list, t.TempDir(), dealer.ToolChamada{Nome: "list"}); err != nil || got != "{}" {
		t.Fatalf("err=%v raw=%q", err, got)
	}
}

// Parallel calls can be turned off globally or for the active provider only.
func TestParallelToolCallsOffPerProvider(t *testing.T) {
	c := config.Config{CurrentProvider: "gw", Providers: map[string]*config.Provider{
		"gw":  {BaseURL: "https://gw/v1", DisableParallelToolCalls: true},
		"bom": {BaseURL: "https://bom/v1"},
	}}
	if !c.ParallelToolCallsOff() {
		t.Fatal("active provider gw asks for no parallel calls")
	}
	c.CurrentProvider = "bom"
	if c.ParallelToolCallsOff() {
		t.Fatal("provider bom keeps parallel calls")
	}
	c.DisableParallelToolCalls = true
	if !c.ParallelToolCallsOff() {
		t.Fatal("the global setting applies to every provider")
	}
}
