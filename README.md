# Barroso Harness

An agent harness written in **Go** from scratch: a single binary, no third-party base, for Windows/macOS/Linux.

- **Permanent YOLO execution**: tools run without approval prompts, allowlists or allow/ask/deny. Technical limits (timeout, output cap, cancellation) are operational — not permissions.
- **Configurable provider** following the **OpenAI Responses API** by default (`base_url` + key + model), with Chat Completions fallback only when the Responses endpoint is unavailable.
- **Interactive session** with an in-memory transcript and the commands `/model`, `/effort`, `/provider`, `/new`, `/resume`, `/save`, `/update`, `/help`, `/exit`. In a terminal, output uses an ANSI-256 palette and glyphs (`▶` tool, `✗` error, `✓` ok, `◆` guardrail) with a live status line and a per-turn token footer; in a pipe/`-p` it emits raw text, with no escapes.
- **`BARROSO.md`** becomes the system instruction: from the project directory (and its parents) and/or the global one at `~/.barroso-harness/BARROSO.md`.
- **Context with a guardrail**: a budget per model window, an estimator calibrated by the real `usage`, and monotonic proactive/reactive compaction. Each turn's transcript is only committed after a complete round.
- **Persistent sessions** in append-only SQLite (`sessions.db`), with `/resume` and blobs for large tool results.
- **Built-in tools**: `shell` (`cmd /C` on Windows, `bash -c` elsewhere), `read`, `write`, `append`, `copy`, `list`, `grep` (pure-Go regex content search), `glob` (`**` file patterns, newest first) and `edit` (exact string replacement, CRLF-aware). Independent tool calls of one round run in parallel; rounds that touch the catalog or session (`mcp_add`, `context_write`, `subagent`, ...) run sequentially.
- **Subsystems**: MCP (stdio and streamable HTTP), skills (`SKILL.md`), `webfetch`/`websearch`, async subagents (`subagent`/`subagent_status`/`subagent_result`, with no recursive delegation) and **ACP** (`bh -acp`) for editors.
- **Self-update from the CLI**: `/update` downloads, verifies the SHA256 and installs the newest release.
- **Installation and recipes**: https://harness.barroso.tec.br

## Install

Release binaries (5 platforms + `SHA256SUMS`): the website has the ready-made commands. Or build from source:

```bash
git clone https://github.com/ruanbarroso/barroso-harness
cd barroso-harness && go build -o barroso-harness ./cmd/barroso-harness
```

On Windows, `build-local.cmd [version]` builds into `dist\bh.exe` inside the repo; installing is the job of `/update`.

## Configure and use

On a fresh install with neither `BARROSO_BASE_URL` nor `BARROSO_AUTH_TOKEN` set, the harness starts directly on OpenCode Zen (`https://opencode.ai/zen/v1`), using the public key `public` and model `big-pickle` (Big Pickle, 200k context). No `bh -init` or setup step is required. The model is the stable default in Zen's catalog; other free models can be transient. Reasoning effort is disabled by default.

To configure a different provider, use `/provider` in the REPL and enter its URL/key. To remove custom providers and restore the public Zen setup, use `/provider zen`. If either environment variable is set, it takes precedence and `/provider` is disabled until those variables are unset.

The active key, base URL, provider and model are stored locally in `~/.barroso-harness/config.json` (file mode `0600` where supported; directory `0700`). The key must never be copied into code, README, Git or logs. `BARROSO_HARNESS_HOME` can change only the data directory. `-init` creates the layout non-destructively.

```bash
bh                                  # REPL (fresh install already on OpenCode Zen)
bh -p "list the files and tell me what you see"  # one non-interactive turn; uses the saved config
bh -version
bh -init
```

In-session commands:

```text
/model                 lists the text models, full catalog in alphabetical order
/model barroso-chat    switches directly if the ID is in the catalog
/effort                shows the current value and common levels (low, medium, high, xhigh, max)
/effort high           sends reasoning_effort=high on the next requests
/effort -              clears it; reasoning_effort is omitted (no silent fallback)
/provider              changes base URL + key; validates /models before saving
/provider zen          removes custom providers and restores public OpenCode Zen + big-pickle
/protocol              shows the preferred protocol (default: Responses; Chat fallback only if the Responses endpoint is unavailable)
/protocol responses    prefer Responses; falls back to Chat only if that endpoint is unavailable
/protocol chat         force Chat Completions (aliases: chat, completions, chat_completions)
/new                   clears the session transcript (new conversation, config intact)
/update                downloads and installs the newest release from the channel
/skills                lists the skills; /<name> [args] runs one
/tasks                 lists the sub-agents; in the full-screen TUI (also ctrl+x a) choosing one opens its live conversation
                       (thoughts, tool calls and output, final answer; tab switches, j/k/pgup/pgdn scroll);
                       /tasks <id> prints it, /tasks cancel <id> cancels
/context               active compaction policy and what each one has done
/help
/exit                  also /quit
```

### `/update`

The channel is the repository's GitHub Releases, published by the deployer. The repository is public, so `/update` needs no token; if `GITHUB_TOKEN` (or `TOKEN_GITHUB`) is set in the environment it is sent anyway to lift GitHub's anonymous rate limit. The token never goes into `config.json` or the URL: it only travels in the `Authorization` header.

```bash
export GITHUB_TOKEN=ghp_...      # or TOKEN_GITHUB
bh                               # inside the REPL:
/update
```

Before installing anything, the cycle proves three times that the file is what it claims to be:

1. **SHA256** checked against the `SHA256SUMS` published in the same release — it hashes while downloading and deletes the staged file on mismatch.
2. **Size** checked against the asset metadata.
3. **Exec-check**: the staged binary is executed (`-version`) and its output must contain the expected tag. This is what prevents a channel error HTML page from being installed as an executable.

If the release does not publish `SHA256SUMS`, or it does not list this platform's binary, the update **aborts** instead of proceeding on "trust". On any failure before installation, the running executable is left intact — a failed update never leaves the machine without a harness.

The chosen version is the **highest number** among stable releases, not the first in the list: the API returns them in creation order, and a republish can put an older version first. Trusting the position would offer a downgrade. `v0.1.0` (stable) is newer than `v0.1.0-bh.10`, and the suffix compares numerically — `bh.10` > `bh.9`.

On Windows, the running executable cannot be renamed. When that happens, the command prints the PowerShell `Move-Item` to run afterwards; it also swaps the `bh.exe`/`barroso-harness.exe` alias, which are two files and need two staged copies.

**Request protocol.** Each provider in `config.json` accepts an optional `"protocol"`. When absent, BH tries `responses` first and falls back to `/chat/completions` only when `/responses` clearly does not exist or is not implemented. Once detected, that fallback is cached for the active session. Authentication, model, request validation, rate-limit, server, timeout and generation errors are not treated as endpoint absence. `/protocol responses` selects this preferred-with-fallback behavior; `/protocol chat` explicitly forces Chat Completions. Existing explicit `chat_completions` settings remain honored.

Whether effort levels are accepted depends on the provider/model. If a request is rejected, the error is shown with no automatic downgrade; use `/effort -` to clear it. Model/provider switches keep the committed text messages; pending tool calls are never left orphaned in the transcript.

`bh -p` asks nothing: if the provider/key/model is missing, it exits with an instruction to start `bh` interactively.

## ACP — editor mode

`bh -acp` serves the **Agent Client Protocol** (JSON-RPC 2.0, newline-delimited) over stdio, so an editor or client can drive the turns:

```bash
bh -acp
```

`stdout` is the protocol wire — no banner or log on it; diagnostics go to `stderr`. Methods: `initialize`, `session/new`, `session/prompt`, `session/cancel` and the `session/update` notification (including `tool_call`, so the client can render tools in real time).

`session/new` returns a `sessionId` that is **always filled in** and stores the session in the same `sessions.db` as the REPL - a conversation started in an editor can be resumed in the terminal with `/resume`. Each session gets the same subsystem wiring (MCP, sub-agents) that the REPL builds, so the client sees the full tool catalog, and the child never gets `subagent` (delegation is not recursive).

A failing MCP server becomes a warning on `stderr` and does not bring the protocol down; the config is captured once, so `session/new` does not re-read the disk.

## Terminal experience

The interactive session (`bh` with no arguments, in a real terminal) uses the TUI look:

```text
Barroso Harness
type /help for commands · plain text goes to the model

provider keys-barroso · effort high · model barroso-chat · window 250k
> barroso-chat ·high
▶ shell 120ms ok
  exit_code: 0
tokens ↑1200 ↓45 · 890ms
```

As for what you type: the prompt has a space before the effort marker, and the status line is **recomputed on every iteration** — `/effort high` and `/model` show up immediately, without relying on the previous command's summary.

The ANSI-256 palette and the glyphs (`▶` tool, `✗` error, `✓` ok, `⚠` warning, `◆` guardrail) are only emitted when `Out` and `ErrOut` are real terminals — `x/term.IsTerminal`, no TUI framework. `NO_COLOR` (with a value) and `TERM=dumb` turn color off even in a terminal.

What changes when output is **not** a terminal — pipe (`bh | cat`), redirection (`bh > log`), input from a file (`bh < cmds`) and `-p`: it falls back to the **plain** renderer, raw text, not a single escape character. That is why the website recipes and the deploy log stay clean, and why tests always see the same literal substring (`[tool shell ok`).

## Development

```bash
go vet ./... && go test ./... && go build ./...
```

Releases are automatic: push to `main` → `deploy.barroso.tec.br` webhook → binaries published as a release (no GitHub Actions — the account forbids them; the contract is `.deploy/release.json`).

## ## Wire: the opencode client contract

Every dealer request — catalog or inference, any gateway — speaks the **official opencode client dialect** (`internal/dealer/zen.go`, measured against the zen free tier's gate). Nothing gates on the configured provider: the harness always presents itself as that client.

- **Canonical ids** (`prefixo_` + 12 hex + 14 base62): the hex prefix encodes `~(ms * 0x1000 + counter)`; the upstream validates the (timestamp, counter) pair, so ids are minted from the real clock, one fresh id per need, never derived from a hash or reused.
- **Session headers stable per conversation**: `x-opencode-session` + `x-opencode-session-id` + `x-session-affinity` + `x-session-id` carry the same `ses_` id for the lifetime of a session (the provider's prefix cache keys on it — measured ~99.5% `cached_tokens`); one-shot traffic (catalog) mints a fresh id per call. Plus `x-opencode-request: msg_…` per request, `x-opencode-project: global`, `x-opencode-client: cli`.
- **User-Agent** `opencode/latest/<version>/cli` with the version resolved live from npm (TTL 6h, floor on failure). `ZEN_USER_AGENT` overrides by hand.
- **Tool fingerprint**: the `read`/`shell` definitions travel as byte-exact raw-JSON literals of the official CLI (Go map marshaling sorts keys alphabetically, which would spoil the comparison); missing ones are appended as decoys, with a system-side note telling the model they are placeholders.
- **`tool_choice: "auto"` always**, with or without client tools (absence of `tool_choice` on a no-tools body is rejected upstream).
- **Duplicate `tool_calls[].id`** across the history get a fresh canonical id, call and paired result renamed together (the upstream 400s on duplicates).
- **Retry vocabulary** gains the free-tier flavors: the 429 `FreeUsageLimitError` is quota (terminal), not a throttle to hit again.

The runtime threads its session id in (`ComConversa`), the dealer handles the rest.

## Context: who decides to compact

`"context_policy"` in `config.json` selects the policy:

- `agente` (default): the model has the `context_compact` tool and compacts at a task milestone, writing the working state that replaces the history before the current request. The budget guardrail remains as a safety net.
- `limiar`: no `context_compact`; only the guardrail compacts, when the window overflows.

Each compaction is stored in `sessions.db` with the policy that produced it, and `/context` (old alias `/contexto`) aggregates by policy (agent compactions, proactive guardrail, **reactive** guardrail — the sign that the policy came too late — and how much the history shrank). `/resume` and `session/load` re-apply the compaction, so the resumed session comes back with the state, not with the already-summarized conversation.

## Prompt hooks

`"prompt_hooks"` in `config.json` runs commands before every prompt of a persistent session and sends what they print to the model beside the prompt — e.g. a memory recall on every turn:

```json
"prompt_hooks": [
  { "command": "python", "args": ["recall.py"], "timeout_segundos": 10 }
]
```

- The contract is Claude Code's `UserPromptSubmit`, so the same script serves both: JSON on stdin (`hook_event_name`, `prompt`, `cwd`, `session_id`, `permission_mode`); on exit 0, stdout is `hookSpecificOutput.additionalContext` or plain text. There is no `transcript_path` (the history lives in SQLite).
- `decision: "block"`, `continue: false` or exit 2 blocks the prompt: it is neither recorded nor sent, and the reason (or stderr) is shown.
- Hooks run in parallel, keep the configuration order in the output and are capped at 10,000 chars in total. Any other failure, a missing command or the timeout (10 s by default) skips the hook; the turn goes on.
- The context goes to the model in a `<hook-context>` block and stays in the history (it is resent on later turns until a compaction). Screen, session title, `/resume` and `session/load` show only what you typed.
- A timeout kills the hook's whole process tree (process group on POSIX, Job Object on Windows), and on Windows no console window opens.

## Parallel tool calls

Some OpenAI-compatible gateways lose the arguments of parallel tool calls (the call arrives with `"arguments": ""`). The harness then tells the model the arguments were lost and asks it to repeat the call alone, instead of running the tool. To avoid the round trip, turn parallel calls off — preferably only for the provider that needs it:

```json
"providers": { "meu-gateway": { "base_url": "https://…/v1", "disable_parallel_tool_calls": true } }
```

A top-level `"disable_parallel_tool_calls": true` applies to every provider. Either way the request carries `parallel_tool_calls: false` (only when it carries tools); turns take more rounds.

## Search

Search and fetch work out of the box, with **no account and no API key**.

- `websearch` tries, in order: the SearXNG at `search_url` (only when configured — it keeps precedence), the anonymous hosted MCP providers **Exa** (`mcp.exa.ai`) and **Parallel** (`search.parallel.ai`), and finally DuckDuckGo lite. A failing source only moves on to the next one; the error surfaces (on one line, naming each source) only when all of them fail.
- `webfetch` reads **public** URLs through the same providers first and falls back to a direct GET. Loopback, private, link-local and single-label hosts never leave the machine — they always go straight to the direct GET.
- `search_providers` in `config.json` reorders or disables the providers: `["parallel", "exa"]`, `["exa"]`, or `["off"]` for none. Absent = `["exa", "parallel"]`. Only names are accepted; the endpoints are fixed in the code.
- The providers are internal: their tools never enter the catalog (the model still sees only `websearch`/`webfetch`). Requests carry only `Content-Type`, `Accept`, `User-Agent` and the server's own `Mcp-Session-Id` — never a key, cookie or `Authorization`.
- Everything they return is untrusted external data: same warning, same caps (`websearch` 20000 chars, `webfetch` `maxChars` up to 60000), 4 MiB per response, 20 s (search) / 30 s (fetch) per call. A provider that fails (including an anonymous rate limit) is skipped for 2 minutes.

## ACP: `session/load`

With `sessions.db` available, `initialize` announces `loadSession: true`. `session/load` reopens the session by id, replays the conversation to the client as `session/update` (`user_message_chunk`, `tool_call`, `tool_call_update`, `agent_message_chunk`) and leaves the session ready for `session/prompt`. An unknown id is `-32602`.

## MCP and skill management by the agent

The model manages its own MCP servers and skills, without hand-editing `config.json`:

- `mcp_list` — configured servers, whether they connected, how many tools, and the error for those that did not connect.
- `mcp_add` — adds (or replaces) a `stdio` server (`command`, `args`, `env`) or an `http` one (`url`, `headers`), connects immediately and saves it to `config.json`. The server's tools are available from the next round of the same turn.
- `mcp_remove` — disconnects, removes the tools from the catalog and deletes it from `config.json`.
- `skill_create` / `skill_remove` — creates or deletes `<scope>/<name>/SKILL.md` (scope `project` = `.barroso/skills`, `global` = `~/.barroso-harness/skills`); the created skill can be invoked with `/<name>`.

Saving re-reads `config.json` from disk and replaces only the server list, so it does not undo a provider/model switch made in the REPL.

## Full screen

In a real terminal, `bh` opens full screen in the OpenCode layout: conversation in the middle, a side panel (title, context used, connected MCP servers), an input box pinned at the bottom and a footer with the directory and context. The panel appears at 110 columns or more.

Keys: `Enter` sends · `↑`/`↓` history of what you typed · mouse wheel and `PgUp`/`PgDn` scroll the conversation · `Esc` or `Ctrl+C` interrupts the response in progress · typing and sending during a response interrupts the turn and sends the new message · `Ctrl+C` twice exits · `Ctrl+U` clears the line · `Ctrl+L` redraws.

`Ctrl+V` with a screenshot in the clipboard inserts a yellow `[Image N]` placeholder; the image (saved under `~/.barroso-harness/attachments/`) is sent with the message, so the model must support vision (images whose placeholder you delete are not sent).

The session name appears in the terminal tab (`BH | <name>`) and in the panel: it is the first message, or whatever you set with `/rename new name`.

With the mouse wheel captured, selecting text to copy is `Shift` + drag (Windows Terminal and most terminals).

The iSH build (linux/386, iPhone/iPad) always uses scrolling mode, since iSH sends no mouse wheel events; every other build always uses full screen. Pipes, redirection and `bh -p` never use full screen.

## License

MIT — see [LICENSE](LICENSE).
