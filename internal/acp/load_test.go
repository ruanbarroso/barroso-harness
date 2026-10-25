package acp

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
)

// turnWithHistory is a Turno that also exposes History — what session/load
// uses to replay the conversation to the client.
type turnWithHistory struct {
	*fakeTurn
	h []dealer.Mensagem
}

func (t *turnWithHistory) History() []dealer.Mensagem { return t.h }

var _ Turno = (*turnWithHistory)(nil)
var _ runtime.Renderer = (*acpRenderer)(nil)
var _ runtime.RendererAoVivo = (*acpRenderer)(nil)
var _ runtime.RendererNota = (*acpRenderer)(nil)

// setupWithLoader builds the agent with a Carregador BEFORE starting Handle
// (the field is read by the loop; writing it afterwards would be a race).
func setupWithLoader(t *testing.T, load Carregador) *harness {
	t.Helper()
	a := Novo(config.Config{})
	a.Fabrica = factory(newFakeTurn())
	a.Carregar = load
	a.RegistrarLog(func(string) {})
	out := &pipe{}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = a.Handle(ctx, pr, out) }()
	return &harness{a: a, out: out, input: pw, cancel: cancel}
}

func testHistory() []dealer.Mensagem {
	return []dealer.Mensagem{
		{Role: "user", Conteudo: "list the files"},
		{Role: "tool_call", ToolID: "c1", ToolName: "list", ToolArgs: `{"path":"src"}`},
		{Role: "tool_result", ToolID: "c1", ToolName: "list", Conteudo: "a.go\nb.go"},
		{Role: "assistant", Conteudo: "there are two files"},
	}
}

// Without a Carregador the agent cannot promise what it does not do.
func TestInitializeWithoutLoaderDoesNotAnnounceLoad(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetInitialize, 1, nil)
	ls := waitLines(t, h.out, 1)
	caps := ls[0].Result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != false {
		t.Fatalf("without a Carregador loadSession should be false: %v", caps)
	}
}

func TestInitializeWithLoaderAnnouncesLoad(t *testing.T) {
	h := setupWithLoader(t, func(config.Config, string) (Turno, error) { return newFakeTurn(), nil })
	h.send(t, MetInitialize, 1, nil)
	ls := waitLines(t, h.out, 1)
	caps := ls[0].Result["agentCapabilities"].(map[string]any)
	if caps["loadSession"] != true {
		t.Fatalf("with a Carregador loadSession should be true: %v", caps)
	}
}

func TestSessionLoadWithoutLoaderRefuses(t *testing.T) {
	h := setup(t, newFakeTurn())
	h.send(t, MetLoad, 1, map[string]any{"sessionId": "bh-x"})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodMetodo {
		t.Fatalf("expected a method error, got %+v", ls[0])
	}
}

// The happy path: replays the history in order and only then responds {}.
func TestSessionLoadReplaysHistoryAndResponds(t *testing.T) {
	var requested string
	h := setupWithLoader(t, func(_ config.Config, id string) (Turno, error) {
		requested = id
		return &turnWithHistory{fakeTurn: newFakeTurn(), h: testHistory()}, nil
	})
	h.send(t, MetLoad, 7, map[string]any{"sessionId": "bh-old", "cwd": `C:\tmp`})
	// user + tool_call + tool_call_update + agent_message + response
	ls := waitLines(t, h.out, 5)

	if requested != "bh-old" {
		t.Fatalf("Carregador received id %q", requested)
	}
	kinds := []string{}
	for _, l := range ls[:4] {
		if l.Method != "session/update" || l.Params.SessionID != "bh-old" {
			t.Fatalf("expected session/update for the loaded session, got %+v", l)
		}
		kinds = append(kinds, l.Params.Update.SessionUpdate)
	}
	want := []string{"user_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("replay order = %v, want %v", kinds, want)
		}
	}
	if ls[0].Params.Update.Content.Blocks[0].Text != "list the files" {
		t.Errorf("wrong user text: %+v", ls[0].Params.Update.Content)
	}
	if ls[1].Params.Update.Title != "list" {
		t.Errorf("tool_call without the tool name: %+v", ls[1].Params.Update)
	}
	if string(ls[1].Params.Update.RawInput) != `{"path":"src"}` {
		t.Errorf("replayed tool_call rawInput = %s", ls[1].Params.Update.RawInput)
	}
	if ls[2].Params.Update.ToolCallID != ls[1].Params.Update.ToolCallID {
		t.Errorf("update does not reference the tool_call: %q vs %q", ls[2].Params.Update.ToolCallID, ls[1].Params.Update.ToolCallID)
	}
	if ls[2].Params.Update.Status != "completed" {
		t.Errorf("result status = %q", ls[2].Params.Update.Status)
	}
	resp := ls[4]
	if resp.Error != nil || string(resp.ID) != "7" {
		t.Fatalf("unexpected final response: %+v", resp)
	}
}

// After load the session is addressable by session/prompt.
func TestSessionLoadLeavesSessionUsable(t *testing.T) {
	f := &fakeTurn{started: make(chan struct{}), text: "continuing"}
	h := setupWithLoader(t, func(config.Config, string) (Turno, error) {
		return &turnWithHistory{fakeTurn: f}, nil
	})
	h.send(t, MetLoad, 1, map[string]any{"sessionId": "bh-old"})
	waitLines(t, h.out, 1)
	h.send(t, MetPrompt, 2, map[string]any{
		"sessionId": "bh-old",
		"prompt":    []map[string]string{{"type": "text", "text": "and now?"}},
	})
	ls := waitLines(t, h.out, 3)
	if ls[1].Params.Update.SessionUpdate != "agent_message_chunk" || ls[2].Error != nil {
		t.Fatalf("prompt on the loaded session failed: %+v", ls)
	}
	if f.promptText() != "and now?" {
		t.Errorf("turn received %q", f.promptText())
	}
}

func TestSessionLoadNonexistentId(t *testing.T) {
	h := setupWithLoader(t, func(config.Config, string) (Turno, error) {
		return nil, errors.New("nonexistent session")
	})
	h.send(t, MetLoad, 1, map[string]any{"sessionId": "bh-none"})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("expected -32602, got %+v", ls[0])
	}
}

func TestSessionLoadWithoutSessionId(t *testing.T) {
	h := setupWithLoader(t, func(config.Config, string) (Turno, error) { return newFakeTurn(), nil })
	h.send(t, MetLoad, 1, map[string]any{})
	ls := waitLines(t, h.out, 1)
	if ls[0].Error == nil || ls[0].Error.Code != CodParams {
		t.Fatalf("expected -32602, got %+v", ls[0])
	}
}

// session/resume is the alias the connector tries FIRST (the bh reuses the
// recorded id, so resume and load are the same operation here).
func TestSessionResumeReplaysHistoryLikeLoad(t *testing.T) {
	h := setupWithLoader(t, func(_ config.Config, id string) (Turno, error) {
		if id != "bh-old" {
			return nil, errors.New("nonexistent session")
		}
		return &turnWithHistory{fakeTurn: newFakeTurn(), h: testHistory()}, nil
	})
	h.send(t, MetResume, 9, map[string]any{"sessionId": "bh-old"})
	// user + tool_call + tool_call_update + agent_message + response
	ls := waitLines(t, h.out, 5)
	kinds := []string{}
	for _, l := range ls[:4] {
		kinds = append(kinds, l.Params.Update.SessionUpdate)
	}
	want := []string{"user_message_chunk", "tool_call", "tool_call_update", "agent_message_chunk"}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("replay order = %v, want %v", kinds, want)
		}
	}
	resp := ls[4]
	if resp.Error != nil || string(resp.ID) != "9" {
		t.Fatalf("unexpected final response: %+v", resp)
	}
}
