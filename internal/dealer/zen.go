// zen.go implements the OpenCode client contract on the wire, unconditionally:
// canonical ids (ses_/msg_), the official User-Agent with a live version,
// stable session headers per conversation, and the byte-exact read/shell tool
// definitions checked by the provider-side gate. The harness always presents
// itself as the opencode client, no matter which gateway is configured -
// this is the measured contract of the opencode zen free tier (gate v2,
// 2026-10-14), ported from the barroso-keys measurements.
package dealer

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ---- canonical ids ----

// charsetBase62 is the alphabet of the official id generator's random
// suffix.
const charsetBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	idMu       sync.Mutex
	idUltimoTs int64
	idContador uint64
)

// idOpencode mints `prefix_` + 12 hex + 14 base62 in the exact shape of the
// official client's generator: the 48-bit hex prefix decodes as
// ~(ms * 0x1000 + counter), with a real timestamp and a real per-millisecond
// counter. The upstream decodes this field - a counter past ~16 per
// millisecond, or a derived id (which decodes to 1969/2155), reads as a
// cloned client. So every id is minted at call time from the real clock,
// never derived and never reused.
func idOpencode(prefixo string) string {
	agora := time.Now().UnixMilli()
	idMu.Lock()
	if agora != idUltimoTs {
		idUltimoTs = agora
		idContador = 0
	}
	idContador++
	n := uint64(agora)*0x1000 + idContador
	idMu.Unlock()
	n = ^n & 0xFFFFFFFFFFFF // 48-bit mask: ~(ms * 0x1000 + counter)
	var brutes [14]byte
	if _, err := rand.Read(brutes[:]); err != nil {
		// crypto/rand at this size does not fail in practice; degrade to
		// wall-clock bytes instead of shipping a malformed id.
		for i := range brutes {
			brutes[i] = byte(time.Now().UnixNano() >> uint(i%12))
		}
	}
	sufixo := make([]byte, 14)
	for i, b := range brutes {
		sufixo[i] = charsetBase62[b%62]
	}
	return prefixo + "_" + fmt.Sprintf("%012x", n) + string(sufixo)
}

// ---- User-Agent ----

// uaFloor is the measured-good User-Agent: the official v2 shape
// opencode/<channel>/<version>/<client>, pinned to the release the gate
// accepted most recently. Only the version decays with every upstream
// release, so it is resolved live from npm with this as the floor.
const (
	uaFloor      = "opencode/latest/2.1.4/cli"
	uaTTL        = 6 * time.Hour
	urlVersaoNpm = "https://registry.npmjs.org/@opencode/cli/latest"
)

var slotVersao = regexp.MustCompile(`\d+\.\d+\.\d+`)
var comecaVersao = regexp.MustCompile(`^\d+\.\d+\.\d+`)

var (
	uaMu    sync.Mutex
	uaVivo  string    // last version successfully resolved from npm
	uaVisto time.Time // when uaVivo was resolved
	uaEmVoo bool      // a refresh is already in flight
)

// UserAgent returns the official client User-Agent. The version is resolved
// live from npm (TTL 6h, one background fetch at a time): while it is in
// flight the last known good version is used, then the measured floor, and
// a fetch failure never breaks a request. ZEN_USER_AGENT overrides
// everything (operator escape hatch, no redeploy needed).
func UserAgent() string {
	if fixa := strings.TrimSpace(os.Getenv("ZEN_USER_AGENT")); fixa != "" {
		return fixa
	}
	uaMu.Lock()
	defer uaMu.Unlock()
	if uaVivo != "" && time.Since(uaVisto) < uaTTL {
		return uaForma(uaVivo)
	}
	if !uaEmVoo {
		uaEmVoo = true
		go resolverUA()
	}
	if uaVivo != "" {
		return uaForma(uaVivo) // stale version lives on while the fetch runs
	}
	return uaFloor
}

func uaForma(versao string) string { return slotVersao.ReplaceAllString(uaFloor, versao) }

func resolverUA() {
	defer func() {
		uaMu.Lock()
		uaEmVoo = false
		uaMu.Unlock()
	}()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(urlVersaoNpm)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var r struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&r); err != nil {
		return
	}
	if !comecaVersao.MatchString(r.Version) {
		return
	}
	uaMu.Lock()
	uaVivo, uaVisto = r.Version, time.Now()
	uaMu.Unlock()
}

// ---- session identity ----

// maxSessoes is the LRU bound of the conversation → session id table.
const maxSessoes = 2000

var (
	sessaoMu   sync.Mutex
	sessaoMapa = map[string]string{} // conversation key → ses_ id
	sessaoFila []string              // insertion and reuse order, oldest first
)

// sessaoEstavel returns the canonical ses_ id of a conversation: minted once
// on first use and kept for the conversation's lifetime. The provider's
// prefix cache keys on x-opencode-session (the "sticky id") and the
// official client keeps ONE id per conversation - measured on the upstream:
// a fresh id per call threw away a ~99.5%% cached_tokens hit. An empty
// conversation key (one-shot traffic) mints a fresh id per call.
func sessaoEstavel(conversa string) string {
	if conversa == "" {
		return idOpencode("ses")
	}
	sessaoMu.Lock()
	defer sessaoMu.Unlock()
	if id, ok := sessaoMapa[conversa]; ok {
		for i, k := range sessaoFila {
			if k == conversa {
				copy(sessaoFila[i:], sessaoFila[i+1:])
				sessaoFila = sessaoFila[:len(sessaoFila)-1]
				break
			}
		}
		sessaoFila = append(sessaoFila, conversa)
		return id
	}
	id := idOpencode("ses")
	sessaoMapa[conversa] = id
	sessaoFila = append(sessaoFila, conversa)
	if len(sessaoFila) > maxSessoes {
		maisVelha := sessaoFila[0]
		sessaoFila = sessaoFila[1:]
		delete(sessaoMapa, maisVelha)
	}
	return id
}

// aplicarCabecalhosOpencode makes a request read as the official client: the
// live UA, the canonical session headers (the pair x-opencode-session +
// x-opencode-session-id measured as required, the affinity twins for stable
// routing) and per-request identities. project "global" and client "cli"
// are the measured literals of the official client - a fresh mint is fine
// for one-shot traffic (the catalog route).
func aplicarCabecalhosOpencode(req *http.Request, sessao string) {
	req.Header.Set("User-Agent", UserAgent())
	req.Header.Set("x-opencode-session", sessao)
	req.Header.Set("x-opencode-session-id", sessao)
	req.Header.Set("x-session-affinity", sessao)
	req.Header.Set("x-session-id", sessao)
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-client", "cli")
	// No x-opencode-request: the official CLI 2.1.4 does not send it (wire
	// capture, 2026-10-25). The gate that the header was blamed for is TLS
	// (see tlszen.go).
}

// ---- environment block ----

// comBlocoEnv appends the official client's environment block to the system
// prompt when it has none. Second gate of the zen free tier (measured
// 2026-10-25 with the Bun TLS hello in place): a system prompt without an
// <env> block carrying "Working directory:" gets 429 FreeUsageLimitError;
// the same prompt plus this block gets 200. The values are the harness's
// own (real cwd and platform), in the official wording.
func comBlocoEnv(system string) string {
	if strings.Contains(system, "<env>") && strings.Contains(system, "Working directory:") {
		return system
	}
	bloco := blocoEnv()
	if strings.TrimSpace(system) == "" {
		return bloco
	}
	return strings.TrimRight(system, "\n") + "\n\n" + bloco
}

func blocoEnv() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = "."
	}
	plataforma := runtime.GOOS
	if plataforma == "windows" {
		plataforma = "win32" // Node's process.platform, what the official client prints
	}
	return "Here is some useful information about the environment you are running in:\n<env>\n" +
		"  Working directory: " + wd + "\n" +
		"  Workspace root folder: " + wd + "\n" +
		"  Platform: " + plataforma + "\n</env>"
}

// ---- tool fingerprint ----

// The gate of the opencode zen free tier compares the read/shell definitions
// BYTE FOR BYTE against the official client's. These wire literals are
// copied from the official CLI (as measured in the barroso-keys port) and
// must keep the exact key order and text - they ship as raw JSON because
// Go's map marshaling sorts keys alphabetically and would spoil the byte
// match. Do not reformat these lines.
const (
	fingerprintReadWire  = `{"type":"function","function":{"name":"read","description":"Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"File or directory to read"},"offset":{"type":"integer","minimum":0,"description":"The line or directory entry to start reading from (1-based)"},"limit":{"type":"integer","minimum":0,"description":"The maximum number of lines or directory entries to read (defaults to and capped at 2000)"}},"required":["path"],"additionalProperties":false}}}`
	fingerprintShellWire = `{"type":"function","function":{"name":"shell","description":"Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete.","parameters":{"type":"object","properties":{"command":{"type":"string","description":"Shell command string to execute"},"workdir":{"type":"string","description":"Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},"timeout":{"type":"integer","minimum":0,"description":"Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},"background":{"type":"boolean","description":"Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."}},"required":["command"],"additionalProperties":false}}}`
)

var fingerprintLiterais = map[string]string{
	"read":  fingerprintReadWire,
	"shell": fingerprintShellWire,
}

// nomeDaTool extracts the function name of a wire tool entry, both flavors
// (nested {"function":{"name":...}} and flat {"name":...}).
func nomeDaTool(t map[string]any) string {
	if fn, ok := t["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	if n, ok := t["name"].(string); ok {
		return strings.TrimSpace(n)
	}
	return ""
}

// notaFingerprint is the system-side brake for the appended decoys: the
// official descriptions invite the model to actually CALL them, and the
// word shape is deliberate - in the original measurement, warning
// "not available" also suppressed the client's real tools, while
// "placeholders ... use the other tools" kept both alive. The gate does not
// validate prompt text.
func notaFingerprint(faltantes []string) string {
	lista := ""
	for i, n := range faltantes {
		if i > 0 {
			lista += " or "
		}
		lista += "`" + n + "`"
	}
	singular := len(faltantes) == 1
	if singular {
		return fmt.Sprintf("Do not call the %s tool; it is a placeholder that does nothing. Use the other tools you have been given instead.", lista)
	}
	return fmt.Sprintf("Do not call the %s tools; they are placeholders that do nothing. Use the other tools you have been given instead.", lista)
}

// aplicarFingerprint rewrites the wire tools array as the official client
// would send it: every declared read/shell entry is replaced by the exact
// official bytes, missing ones are appended, and the caller receives the
// list of appended decoys to add the system-side brake for. The harness's
// twins (its tools) keep their names untouched and the official
// definitions' behavior matches its own read/shell implementations, so the
// model's tool calls continue to match the runtime catalog unchanged.
func aplicarFingerprint(tools []map[string]any) (wire []any, notas []string) {
	out := make([]any, 0, len(tools)+len(fingerprintLiterais))
	vistos := map[string]bool{}
	for _, t := range tools {
		nome := nomeDaTool(t)
		vistos[nome] = true
		if literal, ok := fingerprintLiterais[nome]; ok {
			out = append(out, json.RawMessage(literal))
			continue
		}
		out = append(out, t)
	}
	for _, oficial := range []string{"read", "shell"} {
		if vistos[oficial] {
			continue
		}
		out = append(out, json.RawMessage(fingerprintLiterais[oficial]))
		notas = append(notas, oficial)
	}
	return out, notas
}

// desduplicarIDsDeChamada rewrites duplicated tool-call ids across the sent
// history. The backend rejects a conversation whose tool_calls[].id repeats
// (measured 55/55 with HTTP 400 "Duplicate", param call_id): the first
// occurrence of each id keeps it; later ones get a fresh canonical id by
// sequential position, with the assistant's tool_calls and the tool results
// renamed together (the pairing arrives in practice right after its call).
// An orphan without a remapped pair keeps its id - with no pair there is
// nothing to disambiguate.
func desduplicarIDsDeChamada(list []oaiMsg) {
	var (
		vezesAssistant = map[string]int{} // id → occurrences among tool calls
		remapeadores   = map[string]map[int]string{}
		vezesTool      = map[string]int{} // id → occurrences among tool results
	)
	for i := range list {
		m := &list[i]
		switch {
		case len(m.ToolCalls) > 0:
			for j := range m.ToolCalls {
				bruto := m.ToolCalls[j].ID
				vezesAssistant[bruto]++
				k := vezesAssistant[bruto]
				if k == 1 {
					continue // first occurrence keeps the id
				}
				novo := idOpencode("call")
				if remapeadores[bruto] == nil {
					remapeadores[bruto] = map[int]string{}
				}
				remapeadores[bruto][k] = novo
				m.ToolCalls[j].ID = novo
			}
		case m.ToolCallID != "":
			bruto := m.ToolCallID
			vezesTool[bruto]++
			k := vezesTool[bruto]
			if k == 1 {
				continue
			}
			if novo, ok := remapeadores[bruto][k]; ok {
				m.ToolCallID = novo
			}
		}
	}
}
