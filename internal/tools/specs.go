// Specs of the slice 0 built-in tools. YOLO mode: no permission gate;
// technical limits (timeout, output cap) are operational, not authorization.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	outputCap  = 64 << 10                               // output cap per tool call
	prunedTail = "(content compacted by the guardrail)" // reserved for runtime
)

// SpecsBuiltins returns the full catalog. Order is stable across platforms.
func SpecsBuiltins() []Spec {
	return []Spec{
		specTerminalRun(),
		specTerminalOutput(),
		specTerminalKill(),
		specFileRead(),
		specFileWrite(),
		specFileAppend(),
		specFileCopy(),
		specListDir(),
		specGrep(),
		specGlob(),
		specEdit(),
		specEditMany(),
		specCtxRead(),
	}
}

// ── helpers ──────────────────────────────────────────────────────────────

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

func intOr(m map[string]any, k string, fb int) int {
	if f, ok := m[k].(float64); ok {
		return int(f)
	}
	return fb
}

// resolvePath resolves relative to cwd; in YOLO there is no sandbox — the
// local process privileges apply.
func resolvePath(cwd, p string) string {
	if p == "" {
		return cwd
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(cwd, filepath.Clean(p))
}

// truncate applies the cap with a marker at the end.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + fmt.Sprintf("\n\n[output truncated at %d bytes; read with smaller start/limit for the rest]", limit)
}

// truncateMiddle keeps the start and the END of a long command output: build
// and test failures are reported last, and cutting the tail hid them.
func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	head := limit / 4
	tail := limit - head
	return s[:head] + fmt.Sprintf("\n\n[... %d bytes omitted from the middle; rerun with a filter (e.g. only failing tests) to see them ...]\n\n", len(s)-limit) + s[len(s)-tail:]
}

// ── shell ─────────────────────────────────────────────────────────

func specTerminalRun() Spec {
	return Spec{
		Name: "shell",
		Description: "Execute a shell command and return its output. " +
			shellNote() +
			" Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands " +
			"when possible (use grep to search contents, glob to find files, read to read and edit to change); " +
			"use this tool for builds, tests, git, package managers and running programs. " +
			"Yolo mode: no approval prompt; the local process privileges apply. " +
			"Returns merged stdout+stderr with exit_code. Rely on automatic truncation unless filtering the output " +
			"is more useful (huge output is saved to a file whose path is given). " +
			"Commands accept an optional timeout; a foreground command still running after ~60 s is moved to the " +
			"background and you get its jobID and output so far (wait/read with shell_output, stop with shell_kill). " +
			"A foreground command that prints nothing for 5 minutes is treated as stuck and killed. " +
			"For servers/watchers pass background:true (they are never killed for silence). " +
			"You are TOLD when a background job ends: a new turn starts with its exit code and output, so do not poll or block on it. " +
			"To wait for something (e.g. a reply from someone) start a background timer (`" + sleepHint() + "`), tell the user what you wait for and until when, and end your turn; " +
			"you are woken when the timer ends. Keep a retry limit. Never use a sub-agent just to wait.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "command to run"},
				"workdir":    map[string]any{"type": "string", "description": "working directory (default: the agent's cwd)"},
				"timeout":    map[string]any{"type": "integer", "description": "optional timeout in milliseconds; the command is killed when it passes it (default none; 0 = none)"},
				"background": map[string]any{"type": "boolean", "description": "start it as a background job and return immediately (servers, watchers, long jobs)"},
			},
			"required": []string{"command"},
		},
		Exec: func(ctx context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Command    string `json:"command"`
				Workdir    string `json:"workdir"`
				Timeout    int    `json:"timeout"`
				Background bool   `json:"background"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			c.Command = strings.TrimSpace(c.Command)
			if c.Command == "" {
				return "", fmt.Errorf("empty command")
			}
			if pid, hit := killsProtected(c.Command, ProtectedPIDs()); hit {
				return fmt.Sprintf("refused: PID %d is this agent or one of its ancestors (the shell/terminal/connector hosting this session); killing it ends the session. Exclude it and run again.", pid), nil
			}
			dir := cwd
			if c.Workdir != "" {
				dir = resolvePath(cwd, c.Workdir)
			}
			sink := outputSink(ctx)
			if c.Background {
				sink = nil // the turn moves on; nobody is watching live
			}
			j, err := startJob(c.Command, dir, sink, jobSetFrom(ctx))
			if err != nil {
				return fmt.Sprintf("exit_code: -1\nstart failed: %v", err), nil
			}
			if c.Background {
				j.background = true
				register(j)
				j.markBackground()
				return j.status("started in the background"), nil
			}
			// An explicit timeout is the model asking for a hard bound: honor it
			// and do not auto-background.
			return runForeground(ctx, j, time.Duration(c.Timeout)*time.Millisecond, c.Timeout <= 0), nil
		},
	}
}

func specTerminalOutput() Spec {
	return Spec{
		Name: "shell_output",
		Description: "Waits for a background shell job and returns its new output, or its final exit_code when it ended. " +
			"Without jobID lists the jobs. Waits up to waitSecs (default 60, max 600) — prefer one long wait over many short polls. " +
			"You do not need it to learn that a background job ended: you are notified with a new turn.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"jobID":    map[string]any{"type": "string"},
				"waitSecs": map[string]any{"type": "integer", "description": "how long to wait for it to finish (default 60, max 600; 0 = just peek)"},
			},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				JobID string `json:"jobID"`
				Wait  *int   `json:"waitSecs"`
			}
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &c); err != nil {
					return "", fmt.Errorf("invalid input: %w", err)
				}
			}
			if c.JobID == "" {
				return listJobs(), nil
			}
			j, ok := lookupJob(c.JobID)
			if !ok {
				return "", fmt.Errorf("unknown job %q (it may have been reported finished already); call shell_output without jobID to list", c.JobID)
			}
			wait := defaultOutputWait
			if c.Wait != nil {
				wait = min(time.Duration(max(0, *c.Wait))*time.Second, maxOutputWait)
			}
			return waitJob(ctx, j, wait), nil
		},
	}
}

func specTerminalKill() Spec {
	return Spec{
		Name:        "shell_kill",
		Description: "Stops a background shell job and its child processes, returning its final output.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"jobID": map[string]any{"type": "string"}},
			"required":   []string{"jobID"},
		},
		Exec: func(ctx context.Context, _ string, raw json.RawMessage) (string, error) {
			var c struct {
				JobID string `json:"jobID"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			j, ok := lookupJob(c.JobID)
			if !ok {
				return "", fmt.Errorf("unknown job %q", c.JobID)
			}
			if !j.finished() {
				j.kill("killed by shell_kill")
			}
			return waitJob(ctx, j, 0), nil
		},
	}
}

// shellNote describes the shell the shell tool uses on this platform.
func shellNote() string {
	if runtime.GOOS == "windows" {
		return "On this machine (Windows) the command runs via `cmd /C`: use cmd.exe syntax (dir, type, set, &&, double quotes), " +
			"or `powershell -NoProfile -Command \"...\"` for PowerShell; POSIX tools like grep/sed/ls may not exist."
	}
	return "The command runs via `bash -c`."
}

// sleepHint is an example of a timer command for this platform.
func sleepHint() string {
	if runtime.GOOS == "windows" {
		return "powershell -NoProfile -Command Start-Sleep 600"
	}
	return "sleep 600"
}

// ShellName is the shell the shell tool uses on this platform, for the system prompt.
func ShellName() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe (via `cmd /C`; PowerShell available as `powershell -NoProfile -Command ...`)"
	}
	return "bash (via `bash -c`)"
}

// ── read ────────────────────────────────────────────────────────────

func specFileRead() Spec {
	return Spec{
		Name: "read",
		Description: "Read the contents of a file. Each text line is prefixed by its 1-based line number as " +
			"`N: text`; the prefix is for reference and is not part of the file content. " +
			"Prefer one larger read over many small slices: read the WHOLE file when it is reasonably sized " +
			"(a few thousand lines); for very large files use offset (first line, 1-based) and limit, " +
			"e.g. around a line found with grep. The result states the delivered range and the total line count. " +
			"Images (PNG, JPEG, GIF, WebP) are attached to the conversation so you can see them; other binary files are refused.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer", "description": "first line to read (1-based)"},
				"limit":  map[string]any{"type": "integer", "description": "maximum number of lines in this read"},
			},
			"required": []string{"path"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path   string `json:"path"`
				Offset int    `json:"offset"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			p := resolvePath(cwd, c.Path)
			b, err := os.ReadFile(p)
			if err != nil {
				return "", err
			}
			// Binary content dumped as text lines is noise to the model and, JSON
			// escaped, several times its size in the request: a 282 KB PNG read
			// twice filled a 250k-token window (session bh-20261023110329-0a62).
			if mime := imageType(b); mime != "" {
				if len(b) > maxImageBytes {
					return "", fmt.Errorf("%s is a %s image of %d bytes, above the %d-byte limit for attaching images", c.Path, mime, len(b), maxImageBytes)
				}
				return ImageAttached + p + "]\n" + fmt.Sprintf("%s image, %d bytes. It is attached to the conversation right after this result, so you can see it.", mime, len(b)), nil
			}
			if isBinary(b) {
				return "", fmt.Errorf("%s is a binary file (%d bytes, %s); read only shows text — inspect it with shell (file, strings, xxd) if you need to", c.Path, len(b), http.DetectContentType(b))
			}
			lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
			total := len(lines)
			first := 0
			if c.Offset > 1 {
				first = c.Offset - 1
			}
			end := total
			if c.Limit > 0 && first+c.Limit < end {
				end = first + c.Limit
			}
			if first >= total {
				return fmt.Sprintf("file has %d lines (offset=%d)", total, c.Offset), nil
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "lines %d–%d of %d\n", first+1, end, total)
			for i := first; i < end; i++ {
				line := lines[i]
				if len(line) > 512 {
					line = line[:512] + "…"
				}
				fmt.Fprintf(&sb, "%d: %s\n", i+1, line)
			}
			return truncate(sb.String(), outputCap), nil
		},
	}
}

// ImageAttached starts the result of a read on an image file; the runtime
// attaches the path that follows (up to "]") as an image to the conversation.
const ImageAttached = "[image attached: "

// maxImageBytes caps an image attached by read (providers reject large data URLs).
const maxImageBytes = 10 << 20

// imageType returns the MIME type of an image the model can see, or "".
func imageType(b []byte) string {
	switch t := http.DetectContentType(b); t {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return t
	}
	return ""
}

// isBinary reports content that is not text: a NUL byte or invalid UTF-8 in
// the first 8 KiB.
func isBinary(b []byte) bool {
	sample := b[:min(len(b), 8<<10)]
	if bytes.IndexByte(sample, 0) >= 0 {
		return true
	}
	// The sample may end in the middle of a multi-byte rune: drop that tail.
	for cut := 0; cut < utf8.UTFMax-1 && len(sample) < len(b) && !utf8.Valid(sample); cut++ {
		sample = sample[:len(sample)-1]
	}
	return !utf8.Valid(sample)
}

// ── append ─────────────────────────────────────────────

func specFileWrite() Spec {
	return Spec{
		Name:        "write",
		Description: "Writes a file to the local filesystem, overwriting if one exists. Missing parent directories are created automatically. Use this tool to create NEW files or overwrite existing ones; for partial changes, use the edit tool instead.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			p := resolvePath(cwd, c.Path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(c.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("ok: %s (%d bytes)", c.Path, len(c.Content)), nil
		},
	}
}

func specFileAppend() Spec {
	return Spec{
		Name:        "append",
		Description: "Appends content to the end of a file, creating it if missing. For partial changes in the middle of a file use edit; to create or overwrite a whole file use write.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.Path == "" {
				return "", fmt.Errorf("path is required")
			}
			p := resolvePath(cwd, c.Path)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", err
			}
			defer f.Close()
			if _, err := f.WriteString(c.Content); err != nil {
				return "", err
			}
			return fmt.Sprintf("ok: appended to %s", c.Path), nil
		},
	}
}

// ── copy ────────────────────────────────────────────────────────────

func specFileCopy() Spec {
	return Spec{
		Name:        "copy",
		Description: "Copies a file to another path (overwrites the destination). Uses the system's privileges (cp/copy) — in YOLO there is no sandbox.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"from": map[string]any{"type": "string"},
				"to":   map[string]any{"type": "string"},
			},
			"required": []string{"from", "to"},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				From string `json:"from"`
				To   string `json:"to"`
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return "", fmt.Errorf("invalid input: %w", err)
			}
			if c.From == "" || c.To == "" {
				return "", fmt.Errorf("from and to are required")
			}
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/C", "copy", "/Y", resolvePath(cwd, c.From), resolvePath(cwd, c.To))
			} else {
				cmd = exec.Command("cp", resolvePath(cwd, c.From), resolvePath(cwd, c.To))
			}
			cmd.Dir = cwd
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Sprintf("failed: %v\n%s", err, string(out)), nil
			}
			return fmt.Sprintf("ok: %s → %s", c.From, c.To), nil
		},
	}
}

// ── list ─────────────────────────────────────────────────────────────

func specListDir() Spec {
	return Spec{
		Name:        "list",
		Description: "Lists a directory's entries one per line (default: the agent's cwd), recursively down to depth (default 1, max 3). Directories end with '/'; files show their size. To find files by name pattern, use glob; use read to read a file's contents.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":  map[string]any{"type": "string"},
				"depth": map[string]any{"type": "integer", "description": "depth; default 1, maximum 3"},
			},
		},
		Exec: func(_ context.Context, cwd string, raw json.RawMessage) (string, error) {
			var c struct {
				Path  string `json:"path"`
				Depth int    `json:"depth"`
			}
			_ = json.Unmarshal(raw, &c)
			root := cwd
			if c.Path != "" {
				root = resolvePath(cwd, c.Path)
			}
			if c.Depth <= 0 {
				c.Depth = 1
			}
			if c.Depth > 3 {
				c.Depth = 3
			}
			var sb strings.Builder
			var walk func(p string, lvl int)
			walk = func(p string, lvl int) {
				ents, err := os.ReadDir(p)
				if err != nil {
					fmt.Fprintf(&sb, "(error reading %s: %v)\n", p, err)
					return
				}
				for _, e := range ents {
					name := e.Name()
					if strings.HasPrefix(name, ".") {
						continue
					}
					full := filepath.Join(p, name)
					if e.IsDir() {
						fmt.Fprintf(&sb, "%s/\n", full)
						if lvl < c.Depth {
							walk(full, lvl+1)
						}
						continue
					}
					info, e2 := e.Info()
					if e2 != nil {
						fmt.Fprintf(&sb, "%s (error: %v)\n", full, e2)
						continue
					}
					fmt.Fprintf(&sb, "%s (%d bytes)\n", full, info.Size())
				}
			}
			walk(root, 1)
			return truncate(sb.String(), outputCap), nil
		},
	}
}

// ── context_read (the agent's own live context file) ─────────────────────────

func specCtxRead() Spec {
	return Spec{
		Name:        "context_read",
		Description: "Reads the session's live context file (what the agent is doing, decisions and pending items). In slice 0, the context is BARROSO.md + the transcript itself; this returns a snapshot of the state.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Exec: func(_ context.Context, cwd string, _ json.RawMessage) (string, error) {
			content, _ := liveContext(cwd)
			return truncate(strings.TrimSpace("current live context:\n"+content), outputCap), nil
		},
	}
}

// liveContext returns an informative template; the editable version
// (context_write/context_append) arrives with persistent sessions in slice 1.
func liveContext(cwd string) (string, error) {
	b, err := os.ReadFile(filepath.Join(cwd, "BARROSO.md"))
	if err != nil {
		return "(no BARROSO.md in the project)", nil
	}
	return string(b), nil
}
