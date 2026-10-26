package runtime

// Prompt hooks run before each persistent user prompt. Their output can add
// context or block the prompt, following the relevant part of the Claude Code
// UserPromptSubmit contract without promising transcript_path.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ruanbarroso/barroso-harness/internal/config"
)

const (
	promptHookTimeout = 10 * time.Second
	// promptHookMax caps the context one hook adds, and the total of all hooks.
	promptHookMax = 10_000
	// promptHookStdout caps what is read from stdout: room for the JSON
	// envelope around a full-size additionalContext.
	promptHookStdout = 256 << 10
	// promptHookWait bounds Wait after the kill: a grandchild that escaped the
	// group and holds a pipe cannot hang the turn.
	promptHookWait = 500 * time.Millisecond
)

// promptHookResult is what the hooks decided for one prompt.
type promptHookResult struct {
	Context string
	Blocked bool
	Reason  string
}

// promptHooks runs configured hooks concurrently, combining their results in
// configuration order. Only persistent sessions run them.
func (s *Session) promptHooks(ctx context.Context, cwd, prompt string) promptHookResult {
	if len(s.config.PromptHooks) == 0 || !s.Persistente() {
		return promptHookResult{}
	}
	in, err := json.Marshal(map[string]string{
		"hook_event_name": "UserPromptSubmit",
		"prompt":          prompt,
		"cwd":             cwd,
		"session_id":      s.sessionID,
		"permission_mode": "bypassPermissions",
	})
	if err != nil {
		return promptHookResult{}
	}
	results := make([]promptHookResult, len(s.config.PromptHooks))
	var wg sync.WaitGroup
	for i, h := range s.config.PromptHooks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runPromptHook(ctx, h, cwd, in)
		}()
	}
	wg.Wait()
	var combined promptHookResult
	var parts []string
	for _, result := range results {
		if result.Blocked && !combined.Blocked {
			combined.Blocked = true
			combined.Reason = result.Reason
		}
		if result.Context != "" {
			parts = append(parts, result.Context)
		}
	}
	combined.Context = truncatePrefix(strings.Join(parts, "\n\n"), promptHookMax)
	return combined
}

// runPromptHook runs one hook in its own process group: exit 0 reads JSON or
// plain text, exit 2 blocks, and any other failure or the timeout skips it.
// The timeout kills the whole group, so a grandchild holding stdout open
// cannot keep the turn waiting.
func runPromptHook(ctx context.Context, h config.PromptHook, cwd string, in []byte) promptHookResult {
	if strings.TrimSpace(h.Command) == "" {
		return promptHookResult{}
	}
	limit := promptHookTimeout
	if h.Timeout > 0 {
		limit = time.Duration(h.Timeout) * time.Second
	}
	hctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(hctx, h.Command, h.Args...)
	cmd.Dir = cwd
	cmd.Stdin = bytes.NewReader(in)
	out, stderr := &hookBuffer{max: promptHookStdout}, &hookBuffer{max: promptHookMax}
	cmd.Stdout, cmd.Stderr = out, stderr
	treeAttrs(cmd)
	tree := newProcTree()
	defer tree.close()
	cmd.WaitDelay = promptHookWait
	cmd.Cancel = func() error {
		tree.kill(cmd.Process)
		return nil
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return promptHookResult{} // command not found or not runnable: skipped
	}
	tree.add(cmd.Process)
	err := cmd.Wait()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.Is(err, exec.ErrWaitDelay) && hctx.Err() == nil:
		// The hook exited 0 but left a descendant holding the pipe: what it
		// wrote counts, and the leftover tree goes down with it.
		tree.kill(cmd.Process)
	case errors.As(err, &exit) && exit.ExitCode() == 2 && hctx.Err() == nil:
		// Exit 2 blocks, with stderr (or the JSON reason) as the reason.
		r := parseHookOutput(out)
		r.Context, r.Blocked = "", true
		if r.Reason == "" {
			r.Reason = strings.TrimSpace(stderr.String())
		}
		return r
	default:
		return promptHookResult{} // any other failure or the timeout: skipped
	}
	return parseHookOutput(out)
}

// parseHookOutput reads a hook's stdout: the Claude Code JSON shape when it
// parses (additionalContext, decision:"block", continue:false), plain text
// otherwise. A JSON cut at the read cap is dropped, not passed off as text.
func parseHookOutput(out *hookBuffer) promptHookResult {
	t := strings.TrimSpace(out.String())
	if t == "" {
		return promptHookResult{}
	}
	if !strings.HasPrefix(t, "{") {
		return promptHookResult{Context: truncatePrefix(t, promptHookMax)}
	}
	var j struct {
		Decision           string `json:"decision"`
		Continue           *bool  `json:"continue"`
		Reason             string `json:"reason"`
		StopReason         string `json:"stopReason"`
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(t), &j) != nil {
		if out.cut {
			return promptHookResult{}
		}
		return promptHookResult{Context: truncatePrefix(t, promptHookMax)}
	}
	r := promptHookResult{Context: truncatePrefix(strings.TrimSpace(j.HookSpecificOutput.AdditionalContext), promptHookMax)}
	if strings.EqualFold(j.Decision, "block") || (j.Continue != nil && !*j.Continue) {
		// A blocked prompt never reaches the model: its context is moot.
		r.Context, r.Blocked = "", true
		r.Reason = strings.TrimSpace(j.Reason)
		if r.Reason == "" {
			r.Reason = strings.TrimSpace(j.StopReason)
		}
	}
	return r
}

// hookBuffer keeps the first max bytes a hook writes and discards the rest
// (still reporting success, so the hook is not killed by a broken pipe).
type hookBuffer struct {
	b   []byte
	max int
	cut bool
}

func (h *hookBuffer) Write(p []byte) (int, error) {
	if room := h.max - len(h.b); room < len(p) {
		h.cut = true
		h.b = append(h.b, p[:max(room, 0)]...)
	} else {
		h.b = append(h.b, p...)
	}
	return len(p), nil
}

// String is the kept output, never ending inside a UTF-8 rune.
func (h *hookBuffer) String() string { return trimPartialRune(string(h.b)) }

// err is the error a blocked prompt returns to the caller.
func (r promptHookResult) err() error {
	if r.Reason == "" {
		return fmt.Errorf("prompt blocked by hook")
	}
	return fmt.Errorf("prompt blocked by hook: %s", r.Reason)
}
