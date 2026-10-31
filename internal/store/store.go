// Package store is the append-only session log: pure-Go SQLite
// (modernc.org/sqlite, no CGO) with transcript messages recorded as events.
// The log is the source of truth — the in-memory session can be rebuilt from
// it (Resume), and searching/retrieving archived or compacted content
// (Search/Retrieve) goes through here.
//
// Serialization rule: every payload is JSON produced by marshalSorted, with
// sorted keys. Go maps iterate in random order; without sorting, the same
// payload would produce different bytes on every write and the log would no
// longer be reproducible (blob hashes, diffs and tests depend on it).
package store

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	// Pure-Go driver (no CGO): registers the "sqlite" name in database/sql.
	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var sqlSchema string

// NomeDriver is the driver registered by modernc.org/sqlite. Exported so that
// store integrators can recognize where an error came from.
const NomeDriver = "sqlite"

// ErrSessaoInexistente is returned when an id does not exist in sessions.
var ErrSessaoInexistente = errors.New("session does not exist")

// TsFormato is the timestamp format in the log: RFC3339 with millisecond
// precision, always in UTC. Fixed so that the lexicographic order of
// updated_at (ListSessions) matches chronological order.
const TsFormato = "2006-01-02T15:04:05.000Z07:00"

func nowTS() string { return time.Now().UTC().Format(TsFormato) }

// Event is a log row, already materialized.
type Event struct {
	Seq     int    `json:"seq"`
	Session string `json:"session_id"`
	Ts      string `json:"ts"`
	Kind    string `json:"kind"`
	Payload string `json:"payload_json"`
}

// Session is the index row of a conversation.
type Session struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Title     string `json:"title"`
}

// Hit is a search result: the whole event is not returned, only the snippet
// matching the query and the context window around it.
type Hit struct {
	Seq     int    `json:"seq"`
	Time    string `json:"ts"`
	Kind    string `json:"kind"`
	Snippet string `json:"snippet"`
}

// Store is the database handle. A database/sql *sql.DB is already safe for
// concurrent use; the mutex only guards the write counter, because tool
// rounds run in parallel in the runtime.
type Store struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
}

// Open opens (or creates) the database at path, applies the schema and turns
// on WAL mode.
//
// WAL is what lets the REPL write to the log while another connection reads
// without blocking; without it, every event write would compete with the
// /resume read. `busy_timeout` gives the writer extra slack before giving up.
//
// Opening the same path twice returns the SAME *Store (cached by path), so
// that two sessions in the same process share one connection instead of
// fighting over the SQLite lock.
var (
	registry   = map[string]*Store{}
	registryMu sync.Mutex
)

// Open opens the database at path. What to do on an open error is up to the
// caller: in the CLI persistence is optional (the REPL keeps going without
// it), so callers treat the failure as a warning rather than a stop.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("store: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if s, ok := registry[abs]; ok {
		return s, nil
	}
	dsn := "file:" + filepath.ToSlash(abs) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open(NomeDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	// A single connection: the log is sequential by nature and concurrent
	// writes under WAL still serialize on the file lock. The cache above
	// already keeps a second connection from being opened anyway.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", abs, err)
	}
	st := &Store{path: abs, db: db}
	if err := st.schema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	registry[abs] = st
	return st, nil
}

// Fechar releases the connection and removes the store from the path cache.
func (s *Store) Fechar() error {
	if s == nil {
		return nil
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if cur, ok := registry[s.path]; ok && cur == s {
		delete(registry, s.path)
	}
	return s.db.Close()
}

// Caminho returns the absolute path of the database.
func (s *Store) Caminho() string { return s.path }

func (s *Store) schema() error {
	if _, err := s.db.Exec(sqlSchema); err != nil {
		return fmt.Errorf("store: schema: %w", err)
	}
	return nil
}

// ── deterministic serialization ───────────────────────────────────────────

// marshalSorted serializes with keys in lexicographic order, stable across
// runs. Numbers may vary in representation (int vs float64), so payloads that
// carry counts use int instead of accepting any number.
func marshalSorted(payload map[string]any) (string, error) {
	if len(payload) == 0 {
		return "{}", nil
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return "", err
		}
		sb.Write(kb)
		sb.WriteByte(':')
		vb, err := json.Marshal(payload[k])
		if err != nil {
			return "", err
		}
		sb.Write(vb)
	}
	sb.WriteByte('}')
	return sb.String(), nil
}

// ── sessions ──────────────────────────────────────────────────────────────

// NewSession creates the session index row (INSERT OR IGNORE: calling it
// twice with the same id neither resets created_at nor deletes events).
func (s *Store) NewSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: empty session id")
	}
	now := nowTS()
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, created_at, updated_at, title, metadata_json)
		 VALUES (?, ?, ?, '', '{}')
		 ON CONFLICT(id) DO NOTHING`,
		id, now, now,
	)
	if err != nil {
		return fmt.Errorf("store: new session %s: %w", id, err)
	}
	return nil
}

// TouchSession refreshes updated_at — it is what orders ListSessions and what
// makes /resume list conversations in the order they were used. A session
// not yet indexed is left alone: the row is created by the first event (or
// the title), so opening bh without chatting does not leave an empty session
// in the database.
func (s *Store) TouchSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: empty session id")
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, nowTS(), id); err != nil {
		return fmt.Errorf("store: touch %s: %w", id, err)
	}
	return nil
}

// SetTitle stores the session title (what /save writes and /resume shows).
func (s *Store) SetTitle(id, title string) error {
	if err := s.NewSession(id); err != nil {
		return err
	}
	if err := s.TouchSession(id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`UPDATE sessions SET title = ? WHERE id = ?`, title, id); err != nil {
		return fmt.Errorf("store: title %s: %w", id, err)
	}
	return nil
}

// GetSession returns the session; the error distinguishes "does not exist"
// from a real failure.
func (s *Store) GetSession(id string) (*Session, error) {
	row := s.db.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions WHERE id = ?`, id)
	var se Session
	err := row.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessaoInexistente
	}
	if err != nil {
		return nil, fmt.Errorf("store: session %s: %w", id, err)
	}
	return &se, nil
}

// ListSessions returns the most recent sessions first (updated_at DESC).
// The event count is part of the query because /resume needs to show how many
// log rows each conversation has.
func (s *Store) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(
		`SELECT s.id, s.created_at, s.updated_at, s.title,
		        (SELECT COUNT(*) FROM events e WHERE e.session_id = s.id) AS n
		   FROM sessions s
		  ORDER BY s.updated_at DESC, s.id ASC
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var se Session
		var n int
		if err := rows.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title, &n); err != nil {
			return nil, fmt.Errorf("store: read session: %w", err)
		}
		out = append(out, se)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate sessions: %w", err)
	}
	return out, nil
}

// PrimeiraFala returns the text of the user's first message in the session
// ("" if there is none) — the provisional name of an untitled session.
// Synthetic user messages (compaction working state, output limit and
// subagent notices) are skipped; both the legacy Portuguese and the English
// working-state markers are excluded so older logs keep working.
func (s *Store) PrimeiraFala(id string) string {
	var body string
	err := s.db.QueryRow(
		`SELECT payload_json FROM events
		  WHERE session_id = ? AND kind = 'user_message'
		    AND payload_json NOT LIKE '%[estado de trabalho%'
		    AND payload_json NOT LIKE '%[working state%'
		    AND payload_json NOT LIKE '%[output limit]%'
		    AND payload_json NOT LIKE '%[tool call as text]%'
		    AND payload_json NOT LIKE '%[subagents]%' AND payload_json NOT LIKE '%[shell] Background jobs finished%'
		  ORDER BY seq LIMIT 1`, id).Scan(&body)
	if err != nil {
		return ""
	}
	var m struct {
		Content string `json:"content"`
	}
	if json.Unmarshal([]byte(body), &m) != nil {
		return ""
	}
	return m.Content
}

// ContarEventos returns how many events the session has (0 if it does not
// exist).
func (s *Store) ContarEventos(id string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count events %s: %w", id, err)
	}
	return n, nil
}

// ── events ────────────────────────────────────────────────────────────────

// StoreEvent appends an event to the log. The payload is serialized with
// sorted keys; non-serializable values (channel, func) return an error and
// NOTHING is written — the log does not accept partial rows.
//
// The lock spans the whole process because seq is assigned by SQLite: two
// goroutines writing at the same time (parallel tool rounds) must come out in
// a deterministic order, and the mutex guarantees call order is log order.
func (s *Store) StoreEvent(sessionID, kind string, payload map[string]any) (int, error) {
	if strings.TrimSpace(sessionID) == "" {
		return 0, fmt.Errorf("store: empty sessionID")
	}
	if strings.TrimSpace(kind) == "" {
		return 0, fmt.Errorf("store: empty kind")
	}
	body, err := marshalSorted(payload)
	if err != nil {
		return 0, fmt.Errorf("store: payload of %s: %w", kind, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.NewSession(sessionID); err != nil {
		return 0, err
	}
	res, err := s.db.Exec(
		`INSERT INTO events (session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?)`,
		sessionID, nowTS(), kind, body)
	if err != nil {
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: seq of %s: %w", kind, err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, nowTS(), sessionID); err != nil {
		return 0, fmt.Errorf("store: touch after %s: %w", kind, err)
	}
	return int(seq), nil
}

// ListEvents returns the session's events starting at fromSeq (inclusive), at
// most limit of them. limit <= 0 means "all". Order: ascending seq.
func (s *Store) ListEvents(sessionID string, fromSeq, limit int) ([]Event, error) {
	q := `SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq ASC`
	args := []any{sessionID, fromSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: events of %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload); err != nil {
			return nil, fmt.Errorf("store: read event: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate events: %w", err)
	}
	return out, nil
}

// Known reports whether a session id exists (a wrong id from a model is
// reported as "no session with id", not as an internal error).
func (s *Store) Known(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	_, err := s.GetSession(id)
	return err == nil
}

// GetEvent returns ONE event by seq (full-content retrieve).
func (s *Store) GetEvent(sessionID string, seq int) (*Event, error) {
	row := s.db.QueryRow(
		`SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq = ?`,
		sessionID, seq)
	var e Event
	err := row.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: event %d does not exist in %s: %w", seq, sessionID, ErrSessaoInexistente)
	}
	if err != nil {
		return nil, fmt.Errorf("store: event %d: %w", seq, err)
	}
	return &e, nil
}

// ── blobs ─────────────────────────────────────────────────────────────────

// PutBlob stores the content and returns the reference ready to go into the
// event payload. The hash is the sha256 of the content: the same bytes always
// produce the same reference, so writing twice does not duplicate the row
// (INSERT OR IGNORE) and the log stays reproducible.
func (s *Store) PutBlob(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("store: empty blob")
	}
	sum := sha256Hex(data)
	if _, err := s.db.Exec(
		`INSERT OR IGNORE INTO blobs (hash, size, data) VALUES (?, ?, ?)`,
		sum, len(data), data); err != nil {
		return nil, fmt.Errorf("store: write blob: %w", err)
	}
	return map[string]any{"blob": sum, "size": len(data)}, nil
}

// GetBlob returns the content for a hash.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	var data []byte
	err := s.db.QueryRow(`SELECT data FROM blobs WHERE hash = ?`, hash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: blob %s does not exist", hash)
	}
	if err != nil {
		return nil, fmt.Errorf("store: blob %s: %w", hash, err)
	}
	return data, nil
}

// sha256Hex in lowercase hexadecimal (the format the reference carries).
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── search ────────────────────────────────────────────────────────────────

// Buscar scans the session's events for a case-insensitive substring in the
// human-facing payload fields — role, content, tool name — and returns a
// snippet with a context window around the first occurrence.
//
// The scan runs over the database because the log is the source of truth;
// and it is capped at `limit` hits because nobody searching a 10k-row log
// expects everything back.
func (s *Store) Buscar(sessionID, query string, limit int) ([]Hit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: empty query")
	}
	if limit <= 0 {
		limit = 20
	}
	events, err := s.ListEvents(sessionID, 0, 0)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	var hits []Hit
	for _, e := range events {
		fields, err := textFields(e.Payload)
		if err != nil {
			continue // a corrupt/unreadable payload must not stop the search
		}
		for _, f := range fields {
			i := strings.Index(strings.ToLower(f), needle)
			if i < 0 {
				continue
			}
			hits = append(hits, Hit{
				Seq:     e.Seq,
				Time:    e.Ts,
				Kind:    e.Kind,
				Snippet: window(f, i, len(needle)),
			})
			break // one hit per event: the event is already in the result
		}
		if limit > 0 && len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

// textFields extracts from the payload only what a human searches for: role,
// content, tool name/id and event title. Bookkeeping fields (ts, kind, usage,
// blob...) are ignored because they are not conversation content.
func textFields(payloadJSON string) ([]string, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return nil, err
	}
	var out []string
	for _, k := range []string{"role", "content", "tool_name", "title", "text", "preview"} {
		if s, ok := p[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// window returns the snippet around the occurrence, with ellipses where it is
// cut.
func window(s string, start, needleLen int) string {
	const margin = 120
	from := start - margin
	if from < 0 {
		from = 0
	}
	to := start + needleLen + margin
	if to > len(s) {
		to = len(s)
	}
	snippet := s[from:to]
	if from > 0 {
		snippet = "…" + snippet
	}
	if to < len(s) {
		snippet += "…"
	}
	return strings.Join(strings.Fields(snippet), " ")
}

// Rehydrate returns the full content of a payload that references a blob.
//
// The preview is NOT concatenated: it is always the first bytes of the blob
// itself, so adding them would duplicate the beginning. The preview exists for
// whoever reads the LOG (search, inspection) without going to the blob;
// whoever rehydrates wants the whole content, exactly as it was.
//
// Payloads without a blob come back untouched — the common path, since most
// events fit in the log.
func (s *Store) Rehydrate(payloadJSON string) (string, error) {
	var p map[string]any
	if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
		return payloadJSON, nil // not a payload JSON: return it raw
	}
	hash, ok := p["blob"].(string)
	if !ok || hash == "" {
		return payloadJSON, nil
	}
	data, err := s.GetBlob(hash)
	if err != nil {
		// Missing blob (database copied without the data?): the preview is what is left.
		return payloadJSON, nil
	}
	return string(data), nil
}
