package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/tools"
)

// liveRenderer records the RendererAoVivo events.
type liveRenderer struct {
	recRenderer
	mu   sync.Mutex
	live []string
}

func (r *liveRenderer) ToolComecou(id, name, _ string) { r.note("start:" + id + ":" + name) }
func (r *liveRenderer) ToolSaida(id, chunk string)     { r.note("out:" + id + ":" + chunk) }
func (r *liveRenderer) ToolTerminou(id, name, _, output string, _ time.Duration, _ bool) {
	r.note("end:" + id + ":" + name + ":" + output)
}

func (r *liveRenderer) note(e string) {
	r.mu.Lock()
	r.live = append(r.live, e)
	r.mu.Unlock()
}

// A live renderer gets start → output while running → end for the same id,
// and neither ToolInicio nor Tool/Eco.
func TestLiveRendererGetsStartOutputEnd(t *testing.T) {
	srv := twoCallProvider("talker")
	defer srv.Close()
	s := NewSession(chatTestConfig(srv.URL, "fake", "k"))
	s.Injetar(tools.Spec{Name: "talker", Schema: map[string]any{"type": "object"},
		Exec: func(ctx context.Context, _ string, _ json.RawMessage) (string, error) {
			// Built-in shell streams through the same sink.
			sink := tools.OutputSinkForTest(ctx)
			if sink == nil {
				t.Error("no live output sink in the tool's context")
				return "", nil
			}
			sink("hello\n")
			return "final", nil
		}})
	rr := &liveRenderer{}
	if err := s.Send(context.Background(), rr, t.TempDir(), "go"); err != nil {
		t.Fatal(err)
	}
	want := "start:0/c0:talker,out:0/c0:hello\n,end:0/c0:talker:final"
	if got := strings.Join(rr.live, ","); got != want {
		t.Fatalf("live events:\n got %q\nwant %q", got, want)
	}
	if len(rr.events) != 0 {
		t.Fatalf("the plain path must not run alongside the live one: %v", rr.events)
	}
}
