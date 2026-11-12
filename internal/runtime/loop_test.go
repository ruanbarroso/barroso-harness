package runtime

import (
	"strings"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/dealer"
)

// round builds calls + results with the given outputs, in call order.
func loopRound(calls []dealer.ToolChamada, outs ...string) ([]dealer.ToolChamada, []dealer.Mensagem) {
	results := make([]dealer.Mensagem, len(calls))
	for i, c := range calls {
		out := ""
		if i < len(outs) {
			out = outs[i]
		}
		results[i] = dealer.Mensagem{Role: "tool_result", Conteudo: out, ToolID: c.ID}
	}
	return calls, results
}

func TestLoopGuardWarnsOnThirdIdenticalRound(t *testing.T) {
	var g loopGuard
	call := []dealer.ToolChamada{{Nome: "context_read", Args: "{}"}}
	for i := 1; i <= 2; i++ {
		calls, results := loopRound(call, "same")
		if n, v := g.observe(calls, results); n != "" || v != loopContinue {
			t.Fatalf("round %d fired early: %q %d", i, n, v)
		}
	}
	calls, results := loopRound(call, "same")
	n, v := g.observe(calls, results)
	if v != loopVerdictWarn || !strings.Contains(n, "[loop]") || !strings.Contains(n, "context_read") {
		t.Fatalf("no warn on 3rd identical: %q %d", n, v)
	}
}

// The bug that motivated the rewrite: identical results with DIFFERENT
// arguments (`tail -4` vs `tail -n 4`) never registered with the old
// arguments-only signature.
func TestLoopGuardFiresOnDifferentArgsSameOutput(t *testing.T) {
	var g loopGuard
	for i := 0; i < 3; i++ {
		calls, results := loopRound([]dealer.ToolChamada{{Nome: "shell", Args: `{"tail":` + string(rune('0'+i)) + `}`}}, "identical output")
		_, v := g.observe(calls, results)
		if i < 2 && v != loopContinue {
			t.Fatalf("round %d warned early", i)
		}
		if i == 2 && v != loopVerdictWarn {
			t.Fatalf("round %d ignored identical output (verdict %d)", i, v)
		}
	}
}

func TestLoopGuardResetsOnDifferentOutput(t *testing.T) {
	var g loopGuard
	call := []dealer.ToolChamada{{Nome: "read", Args: "{}"}}
	for i := 0; i < 2; i++ {
		calls, results := loopRound(call, "A")
		g.observe(calls, results)
	}
	calls, results := loopRound(call, "B")
	if n, v := g.observe(calls, results); n != "" || v != loopContinue {
		t.Fatalf("different output did not reset: %q %d", n, v)
	}
}

func TestLoopGuardLadderBlocksAndStops(t *testing.T) {
	var g loopGuard
	call := []dealer.ToolChamada{{Nome: "shell", Args: "{}"}}
	sawBlock, sawStop := false, false
	for i := 0; i < 10; i++ {
		calls, results := loopRound(call, "same")
		_, v := g.observe(calls, results)
		switch v {
		case loopVerdictBlock:
			sawBlock = true
		case loopVerdictStop:
			sawStop = true
		}
	}
	if !sawBlock || !sawStop {
		t.Fatalf("ladder incomplete: block=%v stop=%v", sawBlock, sawStop)
	}
}

// A new user message breaks the streak.
func TestLoopGuardResetBreaksStreak(t *testing.T) {
	var g loopGuard
	call := []dealer.ToolChamada{{Nome: "shell", Args: "{}"}}
	calls, results := loopRound(call, "same")
	g.observe(calls, results)
	g.Reset()
	calls, results = loopRound(call, "same")
	if _, v := g.observe(calls, results); v != loopContinue {
		t.Fatal("Reset did not break the streak")
	}
}

func TestLoopGuardTighterAfterCompaction(t *testing.T) {
	var g loopGuard
	g.Compacted()
	call := []dealer.ToolChamada{{Nome: "shell", Args: "{}"}}
	// warn at 2 instead of 3
	calls, results := loopRound(call, "same")
	if _, v := g.observe(calls, results); v != loopContinue {
		t.Fatalf("first round fired: %d", v)
	}
	calls, results = loopRound(call, "same")
	if _, v := g.observe(calls, results); v != loopVerdictWarn {
		t.Fatalf("tighter ladder did not warn on 2nd: %d", v)
	}
}

func TestBlockedErrorMentionsRepetition(t *testing.T) {
	if !strings.Contains(blockedError("[loop] hi"), "loop guard") {
		t.Fatal("blocked error must say who refused the call")
	}
}
