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

// The refusal is appended to the real output, never a replacement: the
// 129051 loop (2026-11-15) was an error that said which argument was missing,
// and the block used to swallow that text.
func TestBlockedRoundKeepsRealOutput(t *testing.T) {
	const real = "error: mcp wpp.listar_conversas: há 2 contas; diga qual em `conta`: A, B"
	calls := []dealer.ToolChamada{{ID: "c1", Nome: "listar_conversas", Args: "{}"}}
	var g loopGuard
	var verdict int
	var results []dealer.Mensagem
	for i := 0; i < loopBlock; i++ {
		_, results = loopRound(calls, real)
		_, verdict = g.observe(calls, results)
	}
	if verdict != loopVerdictBlock {
		t.Fatalf("verdict = %d, want block", verdict)
	}
	got := g.blockedResults(verdict, calls, results)
	if len(got) != 1 || !got[0].IsErro || got[0].ToolID != "c1" {
		t.Fatalf("blocked result lost its pairing or error flag: %+v", got)
	}
	if !strings.Contains(got[0].Conteudo, real) {
		t.Fatalf("blocked result dropped the real output: %q", got[0].Conteudo)
	}
	if !strings.Contains(got[0].Conteudo, "loop guard") {
		t.Fatalf("blocked result does not say who refused it: %q", got[0].Conteudo)
	}
	if results[0].Conteudo != real {
		t.Fatal("blockedResults mutated the caller's slice")
	}
}

func TestBlockedResultsBelowBlockAreUntouched(t *testing.T) {
	var g loopGuard
	calls := []dealer.ToolChamada{{ID: "c1", Nome: "read", Args: "{}"}}
	_, results := loopRound(calls, "x")
	got := g.blockedResults(loopVerdictWarn, calls, results)
	if got[0].Conteudo != "x" || got[0].IsErro {
		t.Fatalf("warn must not touch the result: %+v", got[0])
	}
}

// The notice names the argument the error asks for, when the call lacks it.
func TestNoticeNamesMissingArgumentFromError(t *testing.T) {
	const real = "error: mcp wpp.listar_conversas: há 2 contas; diga qual em `conta`: A, B"
	calls := []dealer.ToolChamada{{ID: "c1", Nome: "tool_call", Args: `{"name":"listar_conversas"}`}}
	var g loopGuard
	var n string
	for i := 0; i < loopWarn; i++ {
		_, results := loopRound(calls, real)
		n, _ = g.observe(calls, results)
	}
	if !strings.Contains(n, `"conta"`) {
		t.Fatalf("notice does not name the missing argument: %q", n)
	}
}

// A call that already carries the field named by the error gets no hint: the
// guard must not tell the model to add what it already sent.
func TestNoticeSkipsFieldTheCallAlreadyHas(t *testing.T) {
	const real = "error: mcp wpp.listar_conversas: há 2 contas; diga qual em `conta`: A, B"
	calls := []dealer.ToolChamada{{ID: "c1", Nome: "tool_call", Args: `{"name":"listar_conversas","arguments":{"conta":"x"}}`}}
	var g loopGuard
	var n string
	for i := 0; i < loopWarn; i++ {
		_, results := loopRound(calls, real)
		n, _ = g.observe(calls, results)
	}
	if strings.Contains(n, "is absent from this call") {
		t.Fatalf("hint for a field the call already has: %q", n)
	}
}

func TestErrorFieldNameIgnoresNonErrorsAndOddQuotes(t *testing.T) {
	calls := []dealer.ToolChamada{{ID: "c1", Nome: "shell", Args: `{}`}}
	for _, out := range []string{
		"all fine, see `foo` here", // not an error
		"error: use `two words` here",
		"error: nothing quoted",
		"error: open `unclosed",
	} {
		_, results := loopRound(calls, out)
		if f := errorFieldName(calls, results); f != "" {
			t.Fatalf("%q -> %q, want no field", out, f)
		}
	}
}
