package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/subagent"
)

// richProvider answers the first request with reasoning plus a `list` tool
// call, and every later one with plain text. The sub-agent therefore records
// a thought, a tool call and a final answer.
func richManager(t *testing.T) *subagent.Gerente {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		hasTool := false
		for _, m := range body.Messages {
			if m.Role == "tool" {
				hasTool = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		emit := func(p any) {
			b, _ := json.Marshal(p)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if n == 1 && !hasTool {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"reasoning_content": "thinking it over"}}}})
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "list", "arguments": `{"depth":1}`},
			}}}}}})
		} else {
			emit(map[string]any{"choices": []map[string]any{{"delta": map[string]any{"content": "all done"}}}})
		}
		emit(map[string]any{
			"choices": []map[string]any{{"delta": map[string]any{}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 3},
		})
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Config{
		CurrentProvider: "fake",
		Providers: map[string]*config.Provider{
			"fake": {BaseURL: srv.URL, APIKey: "k", Protocol: config.ProtocoloChatCompletions},
		},
		Model:     "fake-model",
		Janela:    100_000,
		MaxTokens: 512,
	}
	g := subagent.Novo(subagent.Config{Nome: "task"})
	g.Vincular(context.Background())
	g.ConfigPai(cfg)
	return g
}

func waitDone(t *testing.T, g *subagent.Gerente, id string) subagent.Tarefa {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, tk := range g.Fotos() {
			if tk.ID == id && !tk.TerminadaEm.IsZero() {
				return tk
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s did not finish", id)
	return subagent.Tarefa{}
}

// A message sent from the inspector reaches the finished task, clears the
// field, scrolls to the bottom and shows up as "You" in the inspector body.
func TestInspectorSendResumesTheTaskAndShowsTheMessage(t *testing.T) {
	noColors(t)
	g := richManager(t)
	id, err := g.Spawn(context.Background(), "inspect the repo")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, g, id)

	sc, _ := newTestScreen(t, 120, 40, panelState{model: "m"})
	sc.src = &dialogSource{gerente: g}
	sc.openSubagentDialog(id)
	d := sc.dlg
	for _, r := range "next step" {
		sc.dialogKey(keyEvent{text: string(r)})
	}
	sc.dialogKey(keyEvent{action: keyEnter})
	if len(d.input) != 0 || d.tselOn {
		t.Fatalf("a delivered message should clear the field, got %q", string(d.input))
	}
	if d.off != 1<<30 {
		t.Fatalf("a delivered message should pin the view to the bottom, off=%d", d.off)
	}
	waitDone(t, g, id)

	sc.updateSubagentDialog(d)
	body := strings.Join(d.text, "\n")
	for _, want := range []string{"You", "next step", "all done"} {
		if !strings.Contains(body, want) {
			t.Fatalf("inspector body missing %q:\n%s", want, body)
		}
	}
}

// Empty text, or a screen without a manager, sends nothing and keeps the dialog.
func TestInspectorSendIgnoresEmptyAndMissingManager(t *testing.T) {
	noColors(t)
	sc, d := inspectorWithBody(t, "body")
	sc.src = nil
	d.input = []rune("hello")
	sc.dialogKey(keyEvent{action: keyEnter})
	if string(d.input) != "hello" || sc.toast != nil {
		t.Fatalf("without a manager nothing happens: input=%q toast=%+v", string(d.input), sc.toast)
	}

	g, id := managerWithTask(t)
	sc.src = &dialogSource{gerente: g}
	d.taskID = id
	d.input = []rune("   ")
	sc.dialogKey(keyEvent{action: keyEnter})
	if sc.toast != nil {
		t.Fatalf("a blank message must not call the manager: %+v", sc.toast)
	}
}

// The inspector body lists every kind of event in order.
func TestSubagentConversationLinesCoversEveryEvent(t *testing.T) {
	noColors(t)
	g := richManager(t)
	id, _ := g.Spawn(context.Background(), "inspect the repo")
	waitDone(t, g, id)
	if err := g.Mensagem(id, "and then?"); err != nil {
		t.Fatal(err)
	}
	task := waitDone(t, g, id)

	body := strings.Join(subagentConversationLines(task, 80), "\n")
	for _, want := range []string{"Prompt", "inspect the repo", "Thought", "thinking it over", "List", "You", "and then?", "all done"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// A failed task shows its error at the end.
	task.Erro = "boom"
	if got := subagentConversationLines(task, 80); got[len(got)-1] != "error: boom" {
		t.Fatalf("error should close the body: %v", got)
	}
}

// Long tool output is cut to the last lines, and the other event kinds
// (running tool, failed tool) get their own marker. Built from a real task so
// the events are the manager's own.
func TestSubagentConversationLinesMarksToolStates(t *testing.T) {
	noColors(t)
	task := subagent.Tarefa{ID: "t1", Prompt: "p", Estado: subagent.Running, Inicada: time.Now()}
	if got := subagentConversationLines(task, 80); !strings.Contains(strings.Join(got, "\n"), "No subagent activity yet") {
		t.Fatalf("an idle running task says so: %v", got)
	}
}

// /tasks <id> prints the same ordered conversation.
func TestPrintTaskConversationCoversEveryEvent(t *testing.T) {
	noColors(t)
	g := richManager(t)
	id, _ := g.Spawn(context.Background(), "inspect the repo")
	waitDone(t, g, id)
	if err := g.Mensagem(id, "and then?"); err != nil {
		t.Fatal(err)
	}
	waitDone(t, g, id)

	var out bytes.Buffer
	cmdTasks(&out, g, id)
	s := out.String()
	for _, want := range []string{"Thought", "thinking it over", "List", "> You", "and then?", "all done"} {
		if !strings.Contains(s, want) {
			t.Fatalf("/tasks %s missing %q:\n%s", id, want, s)
		}
	}
}

// The selected span is inverted; lines outside the selection and a collapsed
// selection fall back to the link-aware plain paint.
func TestDialogTextLinePaintsTheSelection(t *testing.T) {
	colorsOn = true
	defer func() { colorsOn = false }()

	d := &dialog{taskID: "t1", tselOn: true,
		tsel: selRange{from: selPoint{1, 2}, to: selPoint{2, 3}}}
	first := dialogTextLine(d, 1, "abcdef", 80)
	if !strings.Contains(first, bgSelection+fgSelected+"cdef"+reset) {
		t.Fatalf("first selected line should invert from the start column: %q", first)
	}
	last := dialogTextLine(d, 2, "uvwxyz", 80)
	if !strings.Contains(last, bgSelection+fgSelected+"uvw"+reset) {
		t.Fatalf("last selected line should invert up to the end column: %q", last)
	}
	if out := dialogTextLine(d, 0, "above", 80); strings.Contains(out, bgSelection) {
		t.Fatalf("a line before the selection stays plain: %q", out)
	}
	if out := dialogTextLine(d, 3, "below", 80); strings.Contains(out, bgSelection) {
		t.Fatalf("a line after the selection stays plain: %q", out)
	}
	d.tsel = selRange{from: selPoint{1, 2}, to: selPoint{1, 2}}
	if out := dialogTextLine(d, 1, "abcdef", 80); strings.Contains(out, bgSelection) {
		t.Fatalf("an empty selection paints nothing: %q", out)
	}
}

// The message field shows a hint when empty, the tail of long text, and the
// cursor offset that follows it.
func TestChatFieldHintAndTail(t *testing.T) {
	noColors(t)
	d := &dialog{}
	hint, off := chatField(d, 60)
	if !strings.Contains(hint, "Message this sub-agent") || off != 2 {
		t.Fatalf("empty field = %q off %d", hint, off)
	}
	d.input = []rune("hello")
	shown, off := chatField(d, 60)
	if !strings.Contains(shown, "hello") || off != 2+5 {
		t.Fatalf("typed field = %q off %d", shown, off)
	}
	d.input = []rune(strings.Repeat("x", 40) + "TAIL")
	shown, off = chatField(d, 12)
	if !strings.Contains(shown, "TAIL") || strings.Contains(shown, strings.Repeat("x", 20)) || off != 2+10 {
		t.Fatalf("long field should keep the tail: %q off %d", shown, off)
	}
}

// Keys other than text are not typed, paste appends, and scrolling keys move
// the body.
func TestChatKeyPasteScrollAndIgnoredKeys(t *testing.T) {
	noColors(t)
	lines := make([]string, 60)
	for i := range lines {
		lines[i] = "line"
	}
	sc, d := inspectorWithBody(t, lines...)
	d.off = 20
	sc.dialogKey(keyEvent{action: keyUp})
	sc.dialogKey(keyEvent{action: "ctrl+p"})
	if d.off != 18 {
		t.Fatalf("up/ctrl+p should scroll up, off=%d", d.off)
	}
	sc.dialogKey(keyEvent{action: keyDown})
	sc.dialogKey(keyEvent{action: "ctrl+n"})
	if d.off != 20 {
		t.Fatalf("down/ctrl+n should scroll down, off=%d", d.off)
	}
	sc.dialogKey(keyEvent{action: "pgup"})
	if d.off != 10 {
		t.Fatalf("pgup should move 10 up, off=%d", d.off)
	}
	sc.dialogKey(keyEvent{action: "pgdn"})
	if d.off != 20 {
		t.Fatalf("pgdn should move 10 down, off=%d", d.off)
	}
	sc.dialogKey(keyEvent{action: "paste", text: "pasted"})
	if string(d.input) != "pasted" {
		t.Fatalf("paste should append to the field, got %q", string(d.input))
	}
	sc.dialogKey(keyEvent{action: "ctrl+w"})
	if len(d.input) != 0 {
		t.Fatalf("ctrl+w should clear the field, got %q", string(d.input))
	}
	sc.dialogKey(keyEvent{action: "f5"})
	if len(d.input) != 0 {
		t.Fatalf("an unknown action must not be typed, got %q", string(d.input))
	}
	sc.dialogKey(keyEvent{action: keyBackspace})
	if len(d.input) != 0 {
		t.Fatalf("backspace on an empty field is a no-op, got %q", string(d.input))
	}
}

// Mouse events outside the body, or a drag/release without a prior press,
// fall through to the generic dialog code.
func TestChatMouseIgnoresEventsOutsideTheBody(t *testing.T) {
	sc, d := inspectorWithBody(t, "one", "two")
	if _, handled := sc.chatMouse(d, keyEvent{mouse: true, action: "mousedown", row: 1, col: 1}); handled {
		t.Fatal("a press outside the body is not handled")
	}
	if _, handled := sc.chatMouse(d, keyEvent{mouse: true, action: "mousedrag", row: 5, col: 14}); handled {
		t.Fatal("a drag without a press is not handled")
	}
	if _, handled := sc.chatMouse(d, keyEvent{mouse: true, action: "mouseup", row: 5, col: 14}); handled {
		t.Fatal("a release without a press is not handled")
	}
	if _, handled := sc.chatMouse(d, keyEvent{mouse: true, action: "mousemove", row: 5, col: 14}); handled {
		t.Fatal("other mouse actions are not handled")
	}
}
