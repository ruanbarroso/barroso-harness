package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openTestStore opens in a temp directory; no network, no user disk.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Fechar() })
	return st
}

func TestOpenCreatesFileAndSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Fechar()

	// WAL leaves the file on disk; if the schema had not run, every query
	// below would blow up, so the test covers both things.
	for _, table := range []string{"sessions", "events", "blobs"} {
		var n int
		if err := st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("table %s missing after schema: %v", table, err)
		}
	}
	if st.Caminho() != path {
		// The absolute path is normalized; on Windows the case may differ, so
		// we only check the suffix.
		if !strings.HasSuffix(st.Caminho(), "sessions.db") {
			t.Fatalf("unexpected path: %q", st.Caminho())
		}
	}
}

func TestOpenSamePathReturnsSameStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Fechar()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("opening the same path twice created two connections")
	}
}

func TestAppendAndReadEvents(t *testing.T) {
	st := openTestStore(t)
	const id = "bh-20240101120000-abcd"
	if err := st.NewSession(id); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	seq1, err := st.StoreEvent(id, "user_message", map[string]any{"role": "user", "content": "hi"})
	if err != nil {
		t.Fatalf("StoreEvent 1: %v", err)
	}
	seq2, err := st.StoreEvent(id, "assistant", map[string]any{"role": "assistant", "content": "hello"})
	if err != nil {
		t.Fatalf("StoreEvent 2: %v", err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seqs = %d, %d; want 1, 2 (autoincrement from zero)", seq1, seq2)
	}

	all, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("events = %d; want 2", len(all))
	}
	if all[0].Kind != "user_message" || all[1].Kind != "assistant" {
		t.Fatalf("event order reversed: %q, %q", all[0].Kind, all[1].Kind)
	}
	if all[0].Payload != `{"content":"hi","role":"user"}` {
		t.Fatalf("payload keys out of order: %s", all[0].Payload)
	}

	// fromSeq is inclusive.
	partial, err := st.ListEvents(id, seq2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(partial) != 1 || partial[0].Seq != seq2 {
		t.Fatalf("inclusive fromSeq failed: %+v", partial)
	}

	// limit counts from the start of the requested range.
	limited, err := st.ListEvents(id, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 || limited[0].Seq != seq1 {
		t.Fatalf("limit should return the first event: %+v", limited)
	}

	one, err := st.GetEvent(id, seq2)
	if err != nil {
		t.Fatalf("GetEvent: %v", err)
	}
	if one.Kind != "assistant" {
		t.Fatalf("GetEvent seq %d returned %q", seq2, one.Kind)
	}
	if _, err := st.GetEvent(id, 999); err == nil {
		t.Fatal("GetEvent of a missing seq should fail")
	}
}

func TestBlobRoundtrip(t *testing.T) {
	st := openTestStore(t)
	const id = "bh-20240101130000-0001"
	data := []byte(strings.Repeat("large tool output\n", 2000))

	ref, err := st.PutBlob(data)
	if err != nil {
		t.Fatalf("PutBlob: %v", err)
	}
	hash, _ := ref["blob"].(string)
	if len(hash) != 64 {
		t.Fatalf("sha256 hash should have 64 hex chars, got %q", hash)
	}
	if ref["size"].(int) != len(data) {
		t.Fatalf("size = %v, want %d", ref["size"], len(data))
	}

	back, err := st.GetBlob(hash)
	if err != nil {
		t.Fatalf("GetBlob: %v", err)
	}
	if string(back) != string(data) {
		t.Fatal("blob did not round-trip unchanged")
	}

	// Idempotent: the same content neither duplicates the row nor changes the hash.
	ref2, err := st.PutBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	if ref2["blob"] != ref["blob"] {
		t.Fatalf("unstable hash: %v vs %v", ref2["blob"], ref["blob"])
	}
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("duplicated blobs: %d rows", n)
	}

	// An event referencing the blob: the payload stays short and rehydration
	// returns the whole content.
	payload := map[string]any{
		"role":    "tool_result",
		"content": "huge content",
		"blob":    hash,
		"size":    len(data),
		"preview": "first bytes…",
	}
	if _, err := st.StoreEvent(id, "tool_result", payload); err != nil {
		t.Fatal(err)
	}
	events, err := st.ListEvents(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d", len(events))
	}
	if len(events[0].Payload) > 512 {
		t.Fatalf("payload with blob grew large (%d bytes)", len(events[0].Payload))
	}
	full, err := st.Rehydrate(events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	// Rehydrating returns the WHOLE content without repeating the preview:
	// the preview is the beginning of the blob itself, and adding them would
	// duplicate the first bytes.
	if full != string(data) {
		t.Fatalf("rehydration should return the whole blob (%d bytes), got %d", len(data), len(full))
	}

	// A missing blob does not break anything: the payload comes back as is.
	if _, err := st.GetBlob(strings.Repeat("0", 64)); err == nil {
		t.Fatal("GetBlob of a missing hash should fail")
	}
	noBlob, err := st.Rehydrate(`{"role":"user","content":"hi"}`)
	if err != nil || noBlob != `{"role":"user","content":"hi"}` {
		t.Fatalf("payload without blob came back changed: %q (%v)", noBlob, err)
	}
}

func TestListSessionsOrdersByUpdatedAtDesc(t *testing.T) {
	st := openTestStore(t)
	ids := []string{"s1", "s2", "s3"}
	for _, id := range ids {
		if err := st.NewSession(id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		if _, err := st.StoreEvent(id, "user_message", map[string]any{"content": id}); err != nil {
			t.Fatal(err)
		}
	}
	// updated_at has millisecond resolution: on a fast machine the events and
	// the touch below land in the same millisecond, and the tie breaks by id.
	time.Sleep(2 * time.Millisecond)
	// s2 was used last → top of the list.
	if err := st.TouchSession("s2"); err != nil {
		t.Fatal(err)
	}
	list, err := st.ListSessions(10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("sessions = %d", len(list))
	}
	if list[0].ID != "s2" {
		t.Fatalf("most recent should be s2, got %q", list[0].ID)
	}

	n, err := st.ContarEventos("s1")
	if err != nil || n != 1 {
		t.Fatalf("ContarEventos(s1) = %d, %v", n, err)
	}
	if n, _ := st.ContarEventos("does-not-exist"); n != 0 {
		t.Fatalf("ContarEventos of a missing session = %d", n)
	}

	// Title and GetSession.
	if err := st.SetTitle("s2", "conversation about the log"); err != nil {
		t.Fatal(err)
	}
	se, err := st.GetSession("s2")
	if err != nil {
		t.Fatal(err)
	}
	if se.Title != "conversation about the log" {
		t.Fatalf("title = %q", se.Title)
	}
	if se.CreatedAt == "" || se.UpdatedAt == "" {
		t.Fatal("timestamps not filled in")
	}
	if _, err := st.GetSession("does-not-exist"); err == nil {
		t.Fatal("GetSession of a missing session should fail")
	}

	// limit is honored.
	onlyTwo, _ := st.ListSessions(2)
	if len(onlyTwo) != 2 {
		t.Fatalf("limit=2 returned %d", len(onlyTwo))
	}
}

func TestSearchCaseInsensitiveSubstring(t *testing.T) {
	st := openTestStore(t)
	const id = "bh-20240101140000-0002"
	corpus := []map[string]any{
		{"role": "user", "content": "I need to fix the Windows Shell_Quoting"},
		{"role": "assistant", "content": "sure, let's look at the shell"},
		{"role": "tool_call", "tool_name": "shell", "tool_id": "call_1", "args": `{"command":"echo hi"}`},
		{"role": "tool_result", "content": "exit_code: 0\nhi"},
	}
	for _, p := range corpus {
		if _, err := st.StoreEvent(id, p["role"].(string), p); err != nil {
			t.Fatal(err)
		}
	}
	// Case-insensitive search over the content field.
	hits, err := st.Buscar(id, "SHELL_QUOTING", 10)
	if err != nil {
		t.Fatalf("Buscar: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("search returned nothing")
	}
	if !strings.Contains(strings.ToLower(hits[0].Snippet), "shell") {
		t.Fatalf("snippet missing the term: %q", hits[0].Snippet)
	}
	// Search over tool_name (an indexed human field).
	hits, err = st.Buscar(id, "shell", 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawToolCall bool
	for _, h := range hits {
		if h.Kind == "tool_call" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Fatalf("tool_name was not indexed by search: %+v", hits)
	}
	// A hit carries seq, time and kind.
	if hits[0].Seq <= 0 || hits[0].Time == "" || hits[0].Kind == "" {
		t.Fatalf("incomplete Hit: %+v", hits[0])
	}
	// A missing term is not an error.
	empty, err := st.Buscar(id, "zzz-does-not-exist", 10)
	if err != nil || len(empty) != 0 {
		t.Fatalf("non-matching search returned %v, %v", empty, err)
	}
	// An empty query is a caller error.
	if _, err := st.Buscar(id, "", 10); err == nil {
		t.Fatal("empty query should fail")
	}
}

func TestContextWindow(t *testing.T) {
	long := strings.Repeat("a", 400) + "NEEDLE" + strings.Repeat("b", 400)
	start := strings.Index(long, "NEEDLE")
	snippet := window(long, start, 6)
	if len(snippet) > 300 {
		t.Fatalf("window did not cut: %d bytes", len(snippet))
	}
	if !strings.Contains(snippet, "NEEDLE") {
		t.Fatalf("window lost the needle: %q", snippet)
	}
	if !strings.HasPrefix(snippet, "…") || !strings.HasSuffix(snippet, "…") {
		t.Fatalf("window should add ellipses on both sides: %q", snippet)
	}
	// The start and end of the string get no ellipsis.
	if w := window("abc", 0, 3); w != "abc" {
		t.Fatalf("window of the whole string = %q", w)
	}
}

func TestMarshalSortedDeterministic(t *testing.T) {
	// The same map, across different iterations, must produce the SAME bytes —
	// that is what makes the log reproducible.
	build := func() map[string]any {
		return map[string]any{
			"zebra":  1,
			"alpha":  "two",
			"middle": []any{3, 4},
			"center": map[string]any{"b": true, "a": nil},
			"empty":  "",
			"number": 7,
		}
	}
	first, err := marshalSorted(build())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		other, err := marshalSorted(build())
		if err != nil {
			t.Fatal(err)
		}
		if other != first {
			t.Fatalf("marshalSorted unstable:\n%s\n%s", first, other)
		}
	}
	want := `{"alpha":"two","center":{"a":null,"b":true},"empty":"","middle":[3,4],"number":7,"zebra":1}`
	if first != want {
		t.Fatalf("wrong order/value:\n want %s\n  got %s", want, first)
	}
	if empty, _ := marshalSorted(nil); empty != "{}" {
		t.Fatalf("empty payload = %q", empty)
	}
	if empty, _ := marshalSorted(map[string]any{}); empty != "{}" {
		t.Fatalf("empty payload (map) = %q", empty)
	}
	// A non-serializable value returns an error, not a partial row.
	if _, err := marshalSorted(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("payload with a func should fail")
	}
}

func TestStoreEventRejectsBadInput(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.StoreEvent("", "k", nil); err == nil {
		t.Fatal("empty sessionID should fail")
	}
	if _, err := st.StoreEvent("s", "", nil); err == nil {
		t.Fatal("empty kind should fail")
	}
	if _, err := st.StoreEvent("s", "k", map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("non-serializable payload should fail")
	}
	// Nothing was written with the invalid inputs.
	events, err := st.ListEvents("s", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events written despite the error: %+v", events)
	}
}

func TestStoreEventCreatesSessionAutomatically(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.StoreEvent("new", "user_message", map[string]any{"content": "x"}); err != nil {
		t.Fatalf("StoreEvent on a missing session: %v", err)
	}
	if _, err := st.GetSession("new"); err != nil {
		t.Fatalf("session was not indexed: %v", err)
	}
}

// Touching a session that does not exist does not create it: opening bh
// without chatting must not leave an empty session in the database. Setting
// the title does create it.
func TestTouchSessionDoesNotCreateEmptySession(t *testing.T) {
	st := openTestStore(t)
	if err := st.TouchSession("ghost"); err != nil {
		t.Fatalf("TouchSession of a missing session: %v", err)
	}
	if _, err := st.GetSession("ghost"); err == nil {
		t.Fatal("TouchSession created an empty session")
	}
	if err := st.SetTitle("ghost", "name"); err != nil {
		t.Fatal(err)
	}
	if se, err := st.GetSession("ghost"); err != nil || se.Title != "name" {
		t.Fatalf("SetTitle should create it with the title: %v %v", se, err)
	}
}

// PrimeiraFala is the provisional name of an untitled session in /resume.
func TestFirstUserMessage(t *testing.T) {
	st := openTestStore(t)
	if got := st.PrimeiraFala("x"); got != "" {
		t.Fatalf("missing session = %q", got)
	}
	_, _ = st.StoreEvent("x", "user_message", map[string]any{"role": "user", "content": "review the parser"})
	_, _ = st.StoreEvent("x", "user_message", map[string]any{"role": "user", "content": "second"})
	if got := st.PrimeiraFala("x"); got != "review the parser" {
		t.Fatalf("first message = %q", got)
	}
}

// Synthetic user messages (compaction working state, in both the legacy
// Portuguese and the English marker, plus output limit and subagent notices)
// never become the session name.
func TestFirstUserMessageSkipsSyntheticMessages(t *testing.T) {
	st := openTestStore(t)
	for _, synthetic := range []string{
		"[estado de trabalho] legacy summary",
		"[working state] summary",
		"[output limit] notice",
		"[subagents] notice",
	} {
		_, _ = st.StoreEvent("y", "user_message", map[string]any{"role": "user", "content": synthetic})
	}
	_, _ = st.StoreEvent("y", "user_message", map[string]any{"role": "user", "content": "real question"})
	if got := st.PrimeiraFala("y"); got != "real question" {
		t.Fatalf("first message = %q", got)
	}
}
