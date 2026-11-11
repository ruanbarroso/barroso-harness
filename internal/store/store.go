// Package store is the append-only session log: pure-Go SQLite
// (modernc.org/sqlite, no CGO) with transcript messages recorded as events.
// The log is the source of truth — the in-memory session can be rebuilt from
// it (Resume), and searching/retrieving archived or compacted content
// (Search/Retrieve) goes through here.
//
// Layout (one file per session). Under the directory that holds the path given
// to Open:
//
//	sessions/<id>.db   one SQLite database per conversation (sessions + events)
//	blobs/<aa>/<hash>  large tool outputs, one file per sha256
//	sessions.db        LEGACY single database; never written again, only read
//
// Why: every `bh -acp` process used to share ONE sessions.db, so a lock held by
// any process (or the whole herd opening at once after a connector restart)
// stalled every conversation, and a failed open meant silent amnesia. Now each
// conversation owns its file: processes only contend when they touch the same
// session, which never happens in normal operation.
//
// Sessions that exist only in the legacy database are copied to their own file
// the first time they are used (events keep their seq numbers); blobs are read
// from the legacy table when no blob file exists.
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
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// ErrTransitorio marks failures that are EXPECTED to clear by retrying: the
// sqlite file lock (SQLITE_BUSY "database is locked") above all. It is what
// tells session/load "retry" instead of "this session is gone" — the difference
// between the connector reopening a conversation and silently replacing it with
// an empty one. Callers must NOT treat it as a missing session.
var ErrTransitorio = errors.New("store: transient failure (retry)")

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

// Store is the handle on the log directory. Session databases are opened
// lazily and cached, one *sql.DB (a single connection) per session. A
// database/sql *sql.DB is already safe for concurrent use; the mutex only
// guards the write order, because tool rounds run in parallel in the runtime.
type Store struct {
	path     string // the legacy sessions.db path, as given to Open (absolute)
	sessDir  string
	blobsDir string

	mu sync.Mutex // serializes StoreEvent: call order is log order

	cmu sync.Mutex // guards dbs
	dbs map[string]*sql.DB

	lmu       sync.Mutex // guards the lazily opened legacy handle
	legacyDB  *sql.DB
	legacyTry bool
}

var (
	registry   = map[string]*Store{}
	registryMu sync.Mutex
)

// Open prepares the store whose legacy database lives at path (the new
// per-session files go next to it). Opening the same path twice returns the
// SAME *Store (cached by path), so two sessions in one process share their
// connections.
//
// Open itself touches no database: it only creates the directories. A lock in
// some session file can therefore never make Open fail, and what to do on an
// error here (permissions, full disk) is up to the caller: in the CLI
// persistence is optional, so callers treat the failure as a warning.
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
	root := filepath.Dir(abs)
	st := &Store{
		path:     abs,
		sessDir:  filepath.Join(root, "sessions"),
		blobsDir: filepath.Join(root, "blobs"),
		dbs:      map[string]*sql.DB{},
	}
	for _, d := range []string{st.sessDir, st.blobsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("store: create %s: %w", d, err)
		}
	}
	st.sweepOrphans()
	registry[abs] = st
	return st, nil
}

// Fechar releases every connection and removes the store from the path cache.
func (s *Store) Fechar() error {
	if s == nil {
		return nil
	}
	registryMu.Lock()
	if cur, ok := registry[s.path]; ok && cur == s {
		delete(registry, s.path)
	}
	registryMu.Unlock()

	var first error
	s.cmu.Lock()
	for id, db := range s.dbs {
		if err := db.Close(); err != nil && first == nil {
			first = err
		}
		delete(s.dbs, id)
	}
	s.cmu.Unlock()
	s.lmu.Lock()
	if s.legacyDB != nil {
		_ = s.legacyDB.Close()
		s.legacyDB = nil
	}
	s.legacyTry = false
	s.lmu.Unlock()
	return first
}

// Caminho returns the absolute path of the legacy database file (the anchor
// of the store: per-session files live next to it).
func (s *Store) Caminho() string { return s.path }

// ── files ─────────────────────────────────────────────────────────────────

// safeID: ids that can be a file name as they are. Lowercase only, so two ids
// never collide on a case-insensitive filesystem. Anything else (path
// separators, upper case, odd length) is stored under a hash of the id; the
// real id is always the one inside the file.
var safeID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

func (s *Store) fileFor(id string) string {
	name := id
	if !safeID.MatchString(id) || strings.HasSuffix(id, ".db") {
		name = "x-" + sha256Hex([]byte(id))[:32]
	}
	return filepath.Join(s.sessDir, name+".db")
}

func dsnRW(path string) string {
	return "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(" + busyTimeoutStr + ")&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
}

func dsnRO(path string) string {
	return "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(" + busyTimeoutStr + ")"
}

// retryErr runs fn, retrying only transient (lock) failures with backoff.
func retryErr(fn func() error) error {
	var err error
	for i := 0; i < pingTentativas; i++ {
		if err = fn(); err == nil || !transitório(err) {
			return err
		}
		time.Sleep(backoff(i))
	}
	return err
}

// openSessionFile opens (creating if needed) one session database and makes
// sure its schema exists.
func openSessionFile(file string) (*sql.DB, error) {
	db, err := sql.Open(NomeDriver, dsnRW(file))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", file, err)
	}
	// A single connection: the log is sequential by nature, and the file is
	// shared with nobody in normal operation.
	db.SetMaxOpenConns(1)
	if err := pingRetry(db, pingTentativas); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", file, err)
	}
	if err := retryErr(func() error { _, e := db.Exec(sqlSchema); return e }); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: schema %s: %w", file, err)
	}
	return db, nil
}

// sessionDB returns the (cached) database of a session. With create=false a
// session that has no file — and is not in the legacy database either —
// yields ErrSessaoInexistente; with create=true its file is created.
func (s *Store) sessionDB(id string, create bool) (*sql.DB, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("store: empty session id")
	}
	s.cmu.Lock()
	defer s.cmu.Unlock()
	if db, ok := s.dbs[id]; ok {
		return db, nil
	}
	file := s.fileFor(id)
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		found, err := s.migrateLegacy(id, file)
		if err != nil {
			return nil, err
		}
		if !found && !create {
			return nil, ErrSessaoInexistente
		}
	} else if err != nil {
		return nil, fmt.Errorf("store: stat %s: %w", file, err)
	}
	db, err := openSessionFile(file)
	if err != nil {
		return nil, err
	}
	// Pick up what an older bh wrote to the legacy file after the copy. If
	// that cannot be done now (lock), the session is not cached and the error
	// is transient: the next call tries again, nothing is skipped.
	if err := s.syncLegacyTail(id, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s.dbs[id] = db
	return db, nil
}

// ── legacy sessions.db (read only) ────────────────────────────────────────

// legacy returns a read-only handle on the old shared database, or nil when
// there is none (fresh install) or it cannot be read.
func (s *Store) legacy() *sql.DB {
	s.lmu.Lock()
	defer s.lmu.Unlock()
	if s.legacyTry {
		return s.legacyDB
	}
	s.legacyTry = true
	if _, err := os.Stat(s.path); err != nil {
		return nil
	}
	db, err := sql.Open(NomeDriver, dsnRO(s.path))
	if err != nil {
		return nil
	}
	db.SetMaxOpenConns(2)
	s.legacyDB = db
	return db
}

// orphanAge: a migration temp file untouched for this long belongs to a
// process that died; a live copy keeps writing to its file.
const orphanAge = 15 * time.Minute

// tmpSeq makes temp names unique inside one process (clock resolution is not
// enough: two goroutines can read the same instant).
var tmpSeq atomic.Uint64

// Keys of the meta table written by the migration.
const (
	metaSyncedSeq = "legacy_synced_seq"
	metaTitle     = "legacy_title"
)

// dsnTmp: the migration copy is built in rollback-journal mode, so the file is
// self-contained the moment the transaction commits (no -wal to forget).
func dsnTmp(path string) string {
	return "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(" + busyTimeoutStr + ")&_pragma=journal_mode(DELETE)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
}

// noSuchTable: a legacy file without our tables holds no sessions. Any other
// failure reading it is NOT "absent": treating it so would create an empty
// session under an id that exists, hiding its history.
func noSuchTable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such table")
}

// evDigest fingerprints an event stream (seq, ts, kind, payload, in order) so
// the copy can be compared with its source without keeping either in memory.
type evDigest struct {
	h      hash.Hash
	n, max int
}

func newDigest() *evDigest { return &evDigest{h: sha256.New()} }

func (d *evDigest) add(e Event) {
	fmt.Fprintf(d.h, "%d\x00%s\x00%s\x00%d\x00%s\x00", e.Seq, e.Ts, e.Kind, len(e.Payload), e.Payload)
	d.n++
	d.max = e.Seq
}

func (d *evDigest) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// sweepOrphans removes leftovers of migrations whose process died (temp files
// and their sidecars) and of interrupted blob writes. Best effort.
func (s *Store) sweepOrphans() {
	cut := time.Now().Add(-orphanAge)
	if entries, err := os.ReadDir(s.sessDir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.Contains(e.Name(), ".migrating-") {
				continue
			}
			if info, err := e.Info(); err == nil && info.ModTime().Before(cut) {
				_ = os.Remove(filepath.Join(s.sessDir, e.Name()))
			}
		}
	}
	blobCut := time.Now().Add(-time.Hour)
	dirs, err := os.ReadDir(s.blobsDir)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sub := filepath.Join(s.blobsDir, d.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !strings.HasPrefix(f.Name(), ".tmp-") {
				continue
			}
			if info, err := f.Info(); err == nil && info.ModTime().Before(blobCut) {
				_ = os.Remove(filepath.Join(sub, f.Name()))
			}
		}
	}
}

// migrateLegacy copies a session from the legacy database into its own file
// and reports whether the session existed there.
//
//   - The source is read inside ONE read transaction (a snapshot), so the copy
//     is consistent even if an older bh process is writing to the legacy file.
//   - The copy is built in a temp file, read back from disk and compared with
//     the source (event count, last seq, SHA-256 of every event) BEFORE it is
//     published. A copy that does not match is discarded, never published.
//   - It is published with a hard link: a reader never sees a half-copied
//     session and two processes migrating the same id cannot clobber each
//     other (the loser's copy is discarded).
//   - Whatever the legacy database receives after the snapshot is picked up
//     by syncLegacyTail the next time the session is opened.
func (s *Store) migrateLegacy(id, file string) (bool, error) {
	leg := s.legacy()
	if leg == nil {
		return false, nil
	}
	tmp := fmt.Sprintf("%s.migrating-%d-%d", file, os.Getpid(), tmpSeq.Add(1))
	removeTmp := func() {
		for _, suf := range []string{"", "-journal", "-wal", "-shm"} {
			_ = os.Remove(tmp + suf)
		}
	}
	defer removeTmp()

	var (
		found bool
		se    Session
		blobs []string
	)
	err := retryErr(func() error {
		removeTmp()
		var e error
		found, se, blobs, e = s.copySession(leg, tmp, id)
		return e
	})
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	// Keep the file's mtime equal to the session's last use: ListSessions
	// uses it to pick which files to read.
	if t, err := time.Parse(TsFormato, se.UpdatedAt); err == nil {
		_ = os.Chtimes(tmp, t, t)
	}
	if err := publish(tmp, file); err != nil {
		return false, err
	}
	s.copyBlobs(leg, blobs)
	return true, nil
}

// publish makes tmp appear as file atomically, without replacing a file that
// is already there.
func publish(tmp, file string) error {
	err := os.Link(tmp, file)
	if err == nil || errors.Is(err, fs.ErrExist) {
		return nil
	}
	// A filesystem without hard links: fall back to rename, never over an
	// existing file.
	if _, serr := os.Stat(file); serr == nil {
		return nil
	}
	if rerr := os.Rename(tmp, file); rerr != nil {
		return fmt.Errorf("store: publish %s: %w", file, err)
	}
	return nil
}

// copySession builds the verified copy of one legacy session at tmp.
func (s *Store) copySession(leg *sql.DB, tmp, id string) (bool, Session, []string, error) {
	var se Session
	fail := func(format string, a ...any) (bool, Session, []string, error) {
		return false, se, nil, fmt.Errorf(format, a...)
	}
	ltx, err := leg.Begin()
	if err != nil {
		return fail("store: legacy session %s: %w", id, err)
	}
	defer func() { _ = ltx.Rollback() }()

	var meta string
	err = ltx.QueryRow(`SELECT id, created_at, updated_at, title, metadata_json FROM sessions WHERE id = ?`, id).
		Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title, &meta)
	if errors.Is(err, sql.ErrNoRows) || noSuchTable(err) {
		return false, se, nil, nil
	}
	if err != nil {
		return fail("store: legacy session %s: %w", id, err)
	}
	rows, err := ltx.Query(`SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return fail("store: migrate %s: read events: %w", id, err)
	}
	defer rows.Close()

	db, err := sql.Open(NomeDriver, dsnTmp(tmp))
	if err != nil {
		return fail("store: migrate %s: %w", id, err)
	}
	db.SetMaxOpenConns(1)
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()
	if _, err := db.Exec(sqlSchema); err != nil {
		return fail("store: migrate %s: schema: %w", id, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fail("store: migrate %s: %w", id, err)
	}
	if _, err := tx.Exec(`INSERT INTO sessions (id, created_at, updated_at, title, metadata_json) VALUES (?, ?, ?, ?, ?)`,
		se.ID, se.CreatedAt, se.UpdatedAt, se.Title, meta); err != nil {
		_ = tx.Rollback()
		return fail("store: migrate %s: %w", id, err)
	}
	src := newDigest()
	seen := map[string]bool{}
	var blobs []string
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload); err != nil {
			_ = tx.Rollback()
			return fail("store: migrate %s: read event: %w", id, err)
		}
		if _, err := tx.Exec(`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?, ?)`,
			e.Seq, e.Session, e.Ts, e.Kind, e.Payload); err != nil {
			_ = tx.Rollback()
			return fail("store: migrate %s: write event: %w", id, err)
		}
		src.add(e)
		if strings.Contains(e.Payload, `"blob"`) {
			var p struct {
				Blob string `json:"blob"`
			}
			if json.Unmarshal([]byte(e.Payload), &p) == nil && p.Blob != "" && !seen[p.Blob] {
				seen[p.Blob] = true
				blobs = append(blobs, p.Blob)
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return fail("store: migrate %s: iterate events: %w", id, err)
	}
	for k, v := range map[string]string{metaSyncedSeq: strconv.Itoa(src.max), metaTitle: se.Title} {
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`, k, v); err != nil {
			_ = tx.Rollback()
			return fail("store: migrate %s: meta: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail("store: migrate %s: commit: %w", id, err)
	}
	closed = true
	if err := db.Close(); err != nil {
		return fail("store: migrate %s: close: %w", id, err)
	}
	if err := verifyCopy(tmp, se, meta, src); err != nil {
		return fail("store: migrate %s: verification failed, copy discarded: %w", id, err)
	}
	return true, se, blobs, nil
}

// verifyCopy reopens the finished temp file read-only and compares it with
// what was read from the legacy database.
func verifyCopy(tmp string, want Session, wantMeta string, src *evDigest) error {
	db, err := sql.Open(NomeDriver, dsnRO(tmp))
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var chk string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&chk); err != nil {
		return err
	}
	if chk != "ok" {
		return fmt.Errorf("integrity check: %s", chk)
	}
	var got Session
	var gotMeta string
	if err := db.QueryRow(`SELECT id, created_at, updated_at, title, metadata_json FROM sessions`).
		Scan(&got.ID, &got.CreatedAt, &got.UpdatedAt, &got.Title, &gotMeta); err != nil {
		return fmt.Errorf("session row: %w", err)
	}
	if got != want || gotMeta != wantMeta {
		return fmt.Errorf("session row differs")
	}
	rows, err := db.Query(`SELECT seq, session_id, ts, kind, payload_json FROM events ORDER BY seq ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	dst := newDigest()
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload); err != nil {
			return err
		}
		dst.add(e)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if dst.n != src.n || dst.max != src.max || dst.sum() != src.sum() {
		return fmt.Errorf("events differ: copy has %d (last seq %d), source %d (last seq %d)", dst.n, dst.max, src.n, src.max)
	}
	return nil
}

// copyBlobs brings the blobs referenced by a migrated session into the blobs
// directory, so the session no longer depends on the legacy file. Best effort:
// a blob that cannot be copied stays readable from the legacy table (GetBlob
// falls back to it). Content is only written under the hash it matches.
func (s *Store) copyBlobs(leg *sql.DB, hashes []string) {
	for _, h := range hashes {
		if !isHash(h) {
			continue
		}
		if _, err := os.Stat(s.blobPath(h)); err == nil {
			continue
		}
		var b []byte
		err := retryErr(func() error { return leg.QueryRow(`SELECT data FROM blobs WHERE hash = ?`, h).Scan(&b) })
		if err != nil || len(b) == 0 || sha256Hex(b) != h {
			continue
		}
		_ = s.writeBlob(h, b)
	}
}

// syncLegacyTail picks up what an older bh process wrote to the legacy
// database for a session that was already migrated: events past the last seq
// copied, a newer updated_at and a title changed there. It runs when a session
// is opened. Nothing is lost and nothing is duplicated: the copied position is
// advanced in the same transaction that inserts the events, and re-read inside
// it, so concurrent processes cannot apply the same tail twice.
//
// A tail event keeps its legacy seq when that is past the end of the file's
// log (legacy seqs are global, so it normally is); otherwise it gets the next
// free seq. Either way log order is the order the events were written.
func (s *Store) syncLegacyTail(id string, db *sql.DB) error {
	synced, legTitle, ok, err := readSyncMeta(db)
	if err != nil {
		return fmt.Errorf("store: sync %s: %w", id, err)
	}
	if !ok {
		return nil // not a migrated session
	}
	leg := s.legacy()
	if leg == nil {
		return nil
	}
	var (
		tail []Event
		lse  Session
		have bool
	)
	err = retryErr(func() error {
		tail, have = nil, false
		ltx, err := leg.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = ltx.Rollback() }()
		err = ltx.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions WHERE id = ?`, id).
			Scan(&lse.ID, &lse.CreatedAt, &lse.UpdatedAt, &lse.Title)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		have = true
		rows, err := ltx.Query(`SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq > ? ORDER BY seq ASC`, id, synced)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			if err := rows.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload); err != nil {
				return err
			}
			tail = append(tail, e)
		}
		return rows.Err()
	})
	if err != nil {
		if transitório(err) {
			return fmt.Errorf("store: sync %s from legacy: %w", id, err)
		}
		return nil // legacy unreadable for good: the verified copy stands
	}
	if !have {
		return nil
	}
	var own Session
	if err := db.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions WHERE id = ?`, id).
		Scan(&own.ID, &own.CreatedAt, &own.UpdatedAt, &own.Title); err != nil {
		return fmt.Errorf("store: sync %s: %w", id, err)
	}
	newTitle := lse.Title != legTitle && own.Title == legTitle
	newTime := lse.UpdatedAt > own.UpdatedAt
	if len(tail) == 0 && !newTitle && !newTime {
		return nil
	}
	return retryErr(func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		// A write first, so the lock is taken up front (busy timeout applies)
		// and the position below cannot change under us.
		if _, err := tx.Exec(`UPDATE meta SET value = value WHERE key = ?`, metaSyncedSeq); err != nil {
			_ = tx.Rollback()
			return err
		}
		var cur string
		if err := tx.QueryRow(`SELECT value FROM meta WHERE key = ?`, metaSyncedSeq).Scan(&cur); err != nil {
			_ = tx.Rollback()
			return err
		}
		pos, _ := strconv.Atoi(cur)
		var maxOwn int
		if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&maxOwn); err != nil {
			_ = tx.Rollback()
			return err
		}
		for _, e := range tail {
			if e.Seq <= pos {
				continue // another process already applied it
			}
			if e.Seq > maxOwn {
				if _, err := tx.Exec(`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?, ?)`,
					e.Seq, id, e.Ts, e.Kind, e.Payload); err != nil {
					_ = tx.Rollback()
					return err
				}
				maxOwn = e.Seq
			} else {
				res, err := tx.Exec(`INSERT INTO events (session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?)`,
					id, e.Ts, e.Kind, e.Payload)
				if err != nil {
					_ = tx.Rollback()
					return err
				}
				n, _ := res.LastInsertId()
				maxOwn = int(n)
			}
			pos = e.Seq
		}
		if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = ?`, strconv.Itoa(pos), metaSyncedSeq); err != nil {
			_ = tx.Rollback()
			return err
		}
		if newTime {
			if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ? AND updated_at < ?`, lse.UpdatedAt, id, lse.UpdatedAt); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if newTitle {
			if _, err := tx.Exec(`UPDATE sessions SET title = ? WHERE id = ? AND title = ?`, lse.Title, id, legTitle); err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.Exec(`UPDATE meta SET value = ? WHERE key = ?`, lse.Title, metaTitle); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		return tx.Commit()
	})
}

func readSyncMeta(db *sql.DB) (synced int, title string, ok bool, err error) {
	rows, err := db.Query(`SELECT key, value FROM meta WHERE key IN (?, ?)`, metaSyncedSeq, metaTitle)
	if err != nil {
		return 0, "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return 0, "", false, err
		}
		switch k {
		case metaSyncedSeq:
			synced, _ = strconv.Atoi(v)
			ok = true
		case metaTitle:
			title = v
		}
	}
	return synced, title, ok, rows.Err()
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

const insertSession = `INSERT INTO sessions (id, created_at, updated_at, title, metadata_json)
		 VALUES (?, ?, ?, '', '{}')
		 ON CONFLICT(id) DO NOTHING`

// NewSession creates the session (its file and index row). Calling it twice
// with the same id neither resets created_at nor deletes events.
func (s *Store) NewSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: empty session id")
	}
	db, err := s.sessionDB(id, true)
	if err != nil {
		return fmt.Errorf("store: new session %s: %w", id, err)
	}
	now := nowTS()
	if _, err := db.Exec(insertSession, id, now, now); err != nil {
		return fmt.Errorf("store: new session %s: %w", id, err)
	}
	return nil
}

// TouchSession refreshes updated_at — it is what orders ListSessions and what
// makes /resume list conversations in the order they were used. A session
// that does not exist yet is left alone: it is created by the first event (or
// the title), so opening bh without chatting does not leave an empty session
// on disk.
func (s *Store) TouchSession(id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("store: empty session id")
	}
	db, err := s.sessionDB(id, false)
	if errors.Is(err, ErrSessaoInexistente) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: touch %s: %w", id, err)
	}
	if _, err := db.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, nowTS(), id); err != nil {
		return fmt.Errorf("store: touch %s: %w", id, err)
	}
	return nil
}

// SetTitle stores the session title (what /save writes and /resume shows).
func (s *Store) SetTitle(id, title string) error {
	if err := s.NewSession(id); err != nil {
		return err
	}
	db, err := s.sessionDB(id, true)
	if err != nil {
		return fmt.Errorf("store: title %s: %w", id, err)
	}
	if _, err := db.Exec(`UPDATE sessions SET title = ?, updated_at = ? WHERE id = ?`, title, nowTS(), id); err != nil {
		return fmt.Errorf("store: title %s: %w", id, err)
	}
	return nil
}

// GetSession returns the session; the error distinguishes "does not exist"
// from a real failure.
func (s *Store) GetSession(id string) (*Session, error) {
	return retryLeitura(func() (*Session, error) { return s.getSession(id) })
}

func (s *Store) getSession(id string) (*Session, error) {
	db, err := s.sessionDB(id, false)
	if err != nil {
		if errors.Is(err, ErrSessaoInexistente) {
			return nil, ErrSessaoInexistente
		}
		return nil, fmt.Errorf("store: session %s: %w", id, err)
	}
	row := db.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions WHERE id = ?`, id)
	var se Session
	err = row.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessaoInexistente
	}
	if err != nil {
		return nil, fmt.Errorf("store: session %s: %w", id, err)
	}
	return &se, nil
}

// readSessionRow reads the index row of one session file through a short-lived
// read-only connection, so listing many sessions does not keep them open.
func readSessionRow(file string) (Session, bool) {
	var se Session
	db, err := sql.Open(NomeDriver, dsnRO(file))
	if err != nil {
		return se, false
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	err = retryErr(func() error {
		return db.QueryRow(`SELECT id, created_at, updated_at, title FROM sessions LIMIT 1`).
			Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title)
	})
	return se, err == nil
}

type sessionFile struct {
	path string
	mt   time.Time
}

// sessionFiles lists the session databases, most recently written first. The
// mtime of the file or of its WAL is a good proxy for updated_at, because
// every write that moves updated_at also writes the file.
func (s *Store) sessionFiles() ([]sessionFile, error) {
	entries, err := os.ReadDir(s.sessDir)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	var out []sessionFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		p := filepath.Join(s.sessDir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if w, err := os.Stat(p + "-wal"); err == nil && w.ModTime().After(mt) {
			mt = w.ModTime()
		}
		out = append(out, sessionFile{path: p, mt: mt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].mt.After(out[j].mt) })
	return out, nil
}

// ListSessions returns the most recent sessions first (updated_at DESC),
// merging the per-session files with sessions that still live only in the
// legacy database.
func (s *Store) ListSessions(limit int) ([]Session, error) {
	if limit <= 0 {
		limit = 50
	}
	files, err := s.sessionFiles()
	if err != nil {
		return nil, err
	}
	// Read a margin beyond limit: mtime is a proxy, not the ordering key.
	readN := limit * 3
	if readN < 100 {
		readN = 100
	}
	var out []Session
	for i, f := range files {
		if i >= readN {
			break
		}
		if se, ok := readSessionRow(f.path); ok {
			out = append(out, se)
		}
	}
	if leg := s.legacy(); leg != nil {
		rows, err := leg.Query(
			`SELECT id, created_at, updated_at, title FROM sessions ORDER BY updated_at DESC, id ASC LIMIT ?`,
			limit+len(files))
		if err != nil {
			if len(out) == 0 && transitório(err) {
				return nil, fmt.Errorf("store: list sessions: %w", err)
			}
		} else {
			for rows.Next() {
				var se Session
				if err := rows.Scan(&se.ID, &se.CreatedAt, &se.UpdatedAt, &se.Title); err != nil {
					break
				}
				if _, err := os.Stat(s.fileFor(se.ID)); err == nil {
					continue // already migrated: the per-session file wins
				}
				out = append(out, se)
			}
			_ = rows.Close()
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PrimeiraFala returns the text of the user's first message in the session
// ("" if there is none) — the provisional name of an untitled session.
// Synthetic user messages (compaction working state, output limit and
// subagent notices) are skipped; both the legacy Portuguese and the English
// working-state markers are excluded so older logs keep working.
func (s *Store) PrimeiraFala(id string) string {
	db, err := s.sessionDB(id, false)
	if err != nil {
		return ""
	}
	var body string
	err = db.QueryRow(
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
	db, err := s.sessionDB(id, false)
	if errors.Is(err, ErrSessaoInexistente) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("store: count events %s: %w", id, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE session_id = ?`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count events %s: %w", id, err)
	}
	return n, nil
}

// ── events ────────────────────────────────────────────────────────────────

// StoreEvent appends an event to the session's log. The payload is serialized
// with sorted keys; non-serializable values (channel, func) return an error
// and NOTHING is written — the log does not accept partial rows.
//
// seq is assigned by SQLite, per session. The lock spans the whole process
// because two goroutines writing at the same time (parallel tool rounds) must
// come out in a deterministic order: call order is log order.
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
	db, err := s.sessionDB(sessionID, true)
	if err != nil {
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	// One transaction: index row, event and updated_at land together or not
	// at all. The first statement is a write, so the busy timeout applies.
	tx, err := db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	now := nowTS()
	if _, err := tx.Exec(insertSession, sessionID, now, now); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	res, err := tx.Exec(
		`INSERT INTO events (session_id, ts, kind, payload_json) VALUES (?, ?, ?, ?)`,
		sessionID, now, kind, body)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: seq of %s: %w", kind, err)
	}
	if _, err := tx.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, sessionID); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("store: touch after %s: %w", kind, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: write %s to %s: %w", kind, sessionID, err)
	}
	return int(seq), nil
}

// ListEvents returns the session's events starting at fromSeq (inclusive), at
// most limit of them. limit <= 0 means "all". Order: ascending seq. A session
// that does not exist has no events.
func (s *Store) ListEvents(sessionID string, fromSeq, limit int) ([]Event, error) {
	return retryLeitura(func() ([]Event, error) { return s.listEvents(sessionID, fromSeq, limit) })
}

func (s *Store) listEvents(sessionID string, fromSeq, limit int) ([]Event, error) {
	db, err := s.sessionDB(sessionID, false)
	if errors.Is(err, ErrSessaoInexistente) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: events of %s: %w", sessionID, err)
	}
	q := `SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq >= ? ORDER BY seq ASC`
	args := []any{sessionID, fromSeq}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := db.Query(q, args...)
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
	db, err := s.sessionDB(sessionID, false)
	if errors.Is(err, ErrSessaoInexistente) {
		return nil, fmt.Errorf("store: event %d does not exist in %s: %w", seq, sessionID, ErrSessaoInexistente)
	}
	if err != nil {
		return nil, fmt.Errorf("store: event %d: %w", seq, err)
	}
	row := db.QueryRow(
		`SELECT seq, session_id, ts, kind, payload_json FROM events WHERE session_id = ? AND seq = ?`,
		sessionID, seq)
	var e Event
	err = row.Scan(&e.Seq, &e.Session, &e.Ts, &e.Kind, &e.Payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("store: event %d does not exist in %s: %w", seq, sessionID, ErrSessaoInexistente)
	}
	if err != nil {
		return nil, fmt.Errorf("store: event %d: %w", seq, err)
	}
	return &e, nil
}

// ── blobs ─────────────────────────────────────────────────────────────────

func isHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, c := range h {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.blobsDir, hash[:2], hash)
}

// PutBlob stores the content and returns the reference ready to go into the
// event payload. The hash is the sha256 of the content: the same bytes always
// produce the same reference, so writing twice does not duplicate the file and
// the log stays reproducible. The file is written under a temp name and
// renamed, so a reader never sees a partial blob.
func (s *Store) PutBlob(data []byte) (map[string]any, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("store: empty blob")
	}
	sum := sha256Hex(data)
	dst := s.blobPath(sum)
	ref := map[string]any{"blob": sum, "size": len(data)}
	if info, err := os.Stat(dst); err == nil && info.Size() == int64(len(data)) {
		return ref, nil
	}
	if err := s.writeBlob(sum, data); err != nil {
		return nil, err
	}
	return ref, nil
}

// writeBlob writes the content under its hash through a temp file + rename.
func (s *Store) writeBlob(sum string, data []byte) error {
	dst := s.blobPath(sum)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("store: write blob: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(dst), ".tmp-"+sum[:8]+"-*")
	if err != nil {
		return fmt.Errorf("store: write blob: %w", err)
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: write blob: %w", werr)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("store: write blob: %w", err)
	}
	return nil
}

// GetBlob returns the content for a hash: the blob file, or else the legacy
// database's blobs table.
func (s *Store) GetBlob(hash string) ([]byte, error) {
	if !isHash(hash) {
		return nil, fmt.Errorf("store: blob %s does not exist", hash)
	}
	data, err := os.ReadFile(s.blobPath(hash))
	if err == nil {
		return data, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("store: blob %s: %w", hash, err)
	}
	if leg := s.legacy(); leg != nil {
		var b []byte
		qerr := leg.QueryRow(`SELECT data FROM blobs WHERE hash = ?`, hash).Scan(&b)
		if qerr == nil {
			return b, nil
		}
		if !errors.Is(qerr, sql.ErrNoRows) {
			return nil, fmt.Errorf("store: blob %s: %w", hash, qerr)
		}
	}
	return nil, fmt.Errorf("store: blob %s does not exist", hash)
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
		// Missing blob (store copied without the data?): the preview is what is left.
		return payloadJSON, nil
	}
	return string(data), nil
}
