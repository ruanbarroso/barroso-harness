// Package runtime composes a turn: BARROSO.md instructions → history →
// guardrail → model loop → YOLO tools → proactive/reactive compaction.
// The transcript is append-only within the process. Session (session.go)
// runs the loop; Turno is the single-prompt wrapper used by -p.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/dealer"
	"github.com/ruanbarroso/barroso-harness/internal/store"
)

// Turno runs a single prompt (-p mode): text streamed to the output, tools in
// YOLO mode, a window guardrail before each call and bounded reactive
// recovery. It reuses Session with no prior history: stateless per call.
func Turno(out io.Writer, cfg config.Config, cwd, prompt string) error {
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt: use -p \"...\"")
	}
	if !cfg.Pronta() {
		return fmt.Errorf(
			"incomplete configuration (%s) — run `bh` with no arguments to set up provider/model interactively, or edit %s",
			strings.Join(cfg.Faltas(), ", "), filePath(cfg))
	}
	if err := NewSession(cfg).Send(context.Background(), RendererDe(out), cwd, prompt); err != nil {
		return err
	}
	return nil
}

// TurnoPersistente is the -p mode Turno WITH logging. The caller hands over a
// ready store (main.go opens it best-effort); sessionID is the already
// generated id, so the caller can report it. Without a store it degrades to
// Turno: the turn runs the same.
func TurnoPersistente(out io.Writer, cfg config.Config, cwd, prompt string, st *store.Store, sessionID string) error {
	if st == nil || strings.TrimSpace(sessionID) == "" {
		return Turno(out, cfg, cwd, prompt)
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("empty prompt: use -p \"...\"")
	}
	if !cfg.Pronta() {
		return fmt.Errorf(
			"incomplete configuration (%s) — run `bh` with no arguments to set up provider/model interactively, or edit %s",
			strings.Join(cfg.Faltas(), ", "), filePath(cfg))
	}
	return NewSessionWithStore(cfg, st, sessionID).Send(context.Background(), RendererDe(out), cwd, prompt)
}

// NovoSessaoID generates a session id in the same format as the REPL
// (bh-<YYYYMMDDHHMMSS>-<4hex>), so -p mode writes to the SAME log.
func NovoSessaoID() string {
	return fmt.Sprintf("bh-%s-%s", time.Now().Format("20060102150405"), hex4Rand())
}

func hex4Rand() string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Without entropy, the clock becomes the fallback source of randomness.
		return fmt.Sprintf("%04x", time.Now().UnixNano()&0xffff)
	}
	return hex.EncodeToString(b[:])
}

// filePath returns the config.json path (not printed in tests).
func filePath(cfg config.Config) string {
	if cfg.Home != "" {
		return cfg.Home + "\\config.json"
	}
	return "~/.barroso-harness/config.json"
}

// echoExcerpt returns the excerpt to echo on the stream: up to 2 KiB of a tool
// output (the model history already received all of it).
func echoExcerpt(output string) string {
	s := strings.TrimRight(output, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 12 {
		lines = append(lines[:12], "…")
	}
	echo := strings.Join(lines, "\n")
	if len(echo) > 2<<10 {
		echo = echo[:2<<10] + "…"
	}
	return echo
}

// prune replaces large, old tool results with a short stub. Pruning is
// monotonic within the turn — it keeps the request prefix stable. Results in
// the last tail messages are kept while older ones can cover the deficit;
// when they cannot (a short conversation whose few results are huge), the
// tail is pruned too, oldest first, sparing only the latest result.
func prune(h []dealer.Mensagem, floor *int, deficit int) []dealer.Mensagem {
	if deficit <= 0 {
		return nil
	}
	const tail = 24
	pruned := append([]dealer.Mensagem(nil), h...)
	limit := max(len(pruned)-tail, 0)
	freed := 0
	// next is where the following prune resumes: only past what was actually
	// visited. Jumping straight to limit after an early stop left the
	// unvisited results unprunable for the rest of the turn.
	next := max(limit, *floor)
	for i := *floor; i < limit; i++ {
		if !stubResult(pruned, i, &freed) {
			continue
		}
		if freed >= deficit {
			next = i + 1
			break
		}
	}
	if freed < deficit {
		last := len(pruned) - 1
		for last >= 0 && pruned[last].Role != "tool_result" {
			last--
		}
		for i := max(limit, *floor); i < last && freed < deficit; i++ {
			stubResult(pruned, i, &freed)
		}
	}
	if freed < deficit {
		return nil
	}
	*floor = next
	return pruned
}

// stubResult replaces h[i] with the guardrail stub when it is a tool result
// of at least 2 KiB, adding the bytes saved to freed.
func stubResult(h []dealer.Mensagem, i int, freed *int) bool {
	if h[i].Role != "tool_result" || len(h[i].Conteudo) < 2048 {
		return false
	}
	before := len(h[i].Conteudo)
	h[i].Conteudo = stubText(h, i)
	*freed += before - len(h[i].Conteudo)
	return true
}

// contextOverflow reports whether a provider error means the request
// exceeded the context window.
func contextOverflow(err error) bool {
	s := strings.ToLower(err.Error())
	return (strings.Contains(s, "context") || strings.Contains(s, "maximum")) &&
		(strings.Contains(s, "length") || strings.Contains(s, "tokens"))
}
