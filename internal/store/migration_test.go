package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newLegacy creates a legacy sessions.db in a temp dir with session "old-1"
// (seq 7 and 9) and one blob. It returns the path and the blob hash.
func newLegacy(t *testing.T) (string, string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open(NomeDriver, "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blob := []byte(strings.Repeat("legacy blob ", 50))
	hash := sha256Hex(blob)
	for _, q := range []string{
		legacySchema,
		`INSERT INTO sessions VALUES ('old-1','2024-01-01T10:00:00.000Z','2024-01-01T11:00:00.000Z','t0','{}')`,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (7,'old-1','2024-01-01T10:00:00.000Z','user_message','{"content":"hello","role":"user"}')`,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (9,'old-1','2024-01-01T10:01:00.000Z','assistant','{"blob":"` + hash + `","size":600}')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO blobs VALUES (?, ?, ?)`, hash, len(blob), blob); err != nil {
		t.Fatal(err)
	}
	return path, hash, blob
}

// legacyWrite plays an OLD bh process appending to the legacy database.
func legacyWrite(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open(NomeDriver, "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func seqs(evs []Event) []int {
	var out []int
	for _, e := range evs {
		out = append(out, e.Seq)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// An older bh keeps writing to the legacy file after the session was copied:
// the next open picks up the tail, once, with title and updated_at.
func TestLegacyTailWrittenAfterMigrationIsPickedUp(t *testing.T) {
	path, _, _ := newLegacy(t)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if evs, err := st.ListEvents("old-1", 0, 0); err != nil || !eq(seqs(evs), []int{7, 9}) {
		t.Fatalf("first read: %v %v", seqs(evs), err)
	}
	_ = st.Fechar()

	legacyWrite(t, path,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (11,'old-1','2024-01-01T12:00:00.000Z','user_message','{"content":"late","role":"user"}')`,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (12,'old-1','2024-01-01T12:01:00.000Z','assistant','{"content":"late too","role":"assistant"}')`,
		`UPDATE sessions SET title='new title', updated_at='2024-01-01T12:01:00.000Z' WHERE id='old-1'`)

	for round := 0; round < 3; round++ { // reopening must not duplicate anything
		st, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		evs, err := st.ListEvents("old-1", 0, 0)
		if err != nil || !eq(seqs(evs), []int{7, 9, 11, 12}) {
			t.Fatalf("round %d: seqs %v, %v", round, seqs(evs), err)
		}
		se, err := st.GetSession("old-1")
		if err != nil || se.Title != "new title" || se.UpdatedAt != "2024-01-01T12:01:00.000Z" {
			t.Fatalf("round %d: session %+v, %v", round, se, err)
		}
		_ = st.Fechar()
	}
}

// A title set in the new file wins over a stale legacy title, and a tail event
// whose seq collides with one already used in the file gets the next free seq.
func TestLegacyTailKeepsLocalTitleAndNeverCollides(t *testing.T) {
	path, _, _ := newLegacy(t)
	st, _ := Open(path)
	if err := st.SetTitle("old-1", "local title"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // own file now reaches seq 12
		if _, err := st.StoreEvent("old-1", "user_message", map[string]any{"content": "x", "role": "user"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Fechar()
	legacyWrite(t, path,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (10,'old-1','2024-01-01T12:00:00.000Z','user_message','{"content":"from old bh","role":"user"}')`,
		`UPDATE sessions SET title='legacy renamed' WHERE id='old-1'`)

	st, _ = Open(path)
	defer st.Fechar()
	evs, err := st.ListEvents("old-1", 0, 0)
	if err != nil || len(evs) != 6 {
		t.Fatalf("events %v, %v", seqs(evs), err)
	}
	last := evs[len(evs)-1]
	if last.Seq != 13 || !strings.Contains(last.Payload, "from old bh") {
		t.Fatalf("tail event = %+v", last)
	}
	if se, _ := st.GetSession("old-1"); se.Title != "local title" {
		t.Fatalf("local title lost: %q", se.Title)
	}
}

// Blobs of a migrated session live in the blobs dir: the legacy file can go.
func TestMigrationCopiesBlobsAndLegacyCanBeRemoved(t *testing.T) {
	path, hash, blob := newLegacy(t)
	st, _ := Open(path)
	if _, err := st.ListEvents("old-1", 0, 0); err != nil {
		t.Fatal(err)
	}
	_ = st.Fechar()
	for _, suf := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(path + suf)
	}
	st, _ = Open(path)
	defer st.Fechar()
	got, err := st.GetBlob(hash)
	if err != nil || string(got) != string(blob) {
		t.Fatalf("blob after legacy removal: %v", err)
	}
	if evs, err := st.ListEvents("old-1", 0, 0); err != nil || len(evs) != 2 {
		t.Fatalf("events after legacy removal: %v, %v", seqs(evs), err)
	}
}

// Many processes opening the same session at once (a connector restart): one
// verified file, no duplicates, no leftovers.
func TestConcurrentMigrationOfSameSession(t *testing.T) {
	path, _, _ := newLegacy(t)
	st, _ := Open(path)
	defer st.Fechar()
	file := st.fileFor("old-1")

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.migrateLegacy("old-1", file) // bypasses the per-store lock, like separate processes
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	evs, err := st.ListEvents("old-1", 0, 0)
	if err != nil || !eq(seqs(evs), []int{7, 9}) {
		t.Fatalf("events %v, %v", seqs(evs), err)
	}
	entries, _ := os.ReadDir(st.sessDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".migrating-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

// Leftovers of a process that died mid-migration are removed; a recent one
// (maybe a live migration) is left alone.
func TestOrphanTempFilesAreSwept(t *testing.T) {
	path, _, _ := newLegacy(t)
	sess := filepath.Join(filepath.Dir(path), "sessions")
	if err := os.MkdirAll(sess, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(sess, "dead.db.migrating-1-2")
	oldWal := old + "-journal"
	fresh := filepath.Join(sess, "live.db.migrating-3-4")
	for _, p := range []string{old, oldWal, fresh} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(old, past, past)
	_ = os.Chtimes(oldWal, past, past)

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fechar()
	for _, p := range []string{old, oldWal} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("orphan %s not removed", p)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("recent temp file removed: %v", err)
	}
	// And it never shows up as a session.
	if list, _ := st.ListSessions(10); len(list) != 1 {
		t.Fatalf("list = %+v", list)
	}
}

// A legacy file that cannot be read is an error, never "session does not
// exist": answering the latter would create an empty session that hides the
// real history for good.
func TestUnreadableLegacyIsNotMissingSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	st, _ := Open(path)
	defer st.Fechar()
	_, err := st.GetSession("whatever")
	if err == nil || errors.Is(err, ErrSessaoInexistente) {
		t.Fatalf("want a real error, got %v", err)
	}
	if _, err := os.Stat(st.fileFor("whatever")); err == nil {
		t.Fatal("empty session file created under an unreadable legacy")
	}
}

// A legacy file without our tables, or no legacy file at all, simply has no
// sessions.
func TestMissingOrEmptyLegacyHasNoSessions(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.GetSession("x"); !errors.Is(err, ErrSessaoInexistente) {
		t.Fatalf("no legacy: %v", err)
	}
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, _ := sql.Open(NomeDriver, "file:"+filepath.ToSlash(path))
	if _, err := db.Exec(`CREATE TABLE unrelated (a)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	st2, _ := Open(path)
	defer st2.Fechar()
	if _, err := st2.GetSession("x"); !errors.Is(err, ErrSessaoInexistente) {
		t.Fatalf("empty legacy: %v", err)
	}
	if err := st2.NewSession("x"); err != nil {
		t.Fatal(err)
	}
}

// Ids that are not file-name safe still migrate, under a hashed name.
func TestMigrationOfUnusualSessionIDs(t *testing.T) {
	path, _, _ := newLegacy(t)
	id := "Weird/ID ../é"
	legacyWrite(t, path,
		`INSERT INTO sessions VALUES ('`+id+`','2024-02-01T10:00:00.000Z','2024-02-01T10:00:00.000Z','','{}')`,
		`INSERT INTO events (seq, session_id, ts, kind, payload_json) VALUES (20,'`+id+`','2024-02-01T10:00:00.000Z','user_message','{"content":"hi","role":"user"}')`)
	st, _ := Open(path)
	defer st.Fechar()
	evs, err := st.ListEvents(id, 0, 0)
	if err != nil || !eq(seqs(evs), []int{20}) {
		t.Fatalf("events %v, %v", seqs(evs), err)
	}
	if rel, _ := filepath.Rel(st.sessDir, st.fileFor(id)); strings.ContainsAny(rel, `/\`) {
		t.Fatalf("file escapes sessions dir: %s", rel)
	}
}
