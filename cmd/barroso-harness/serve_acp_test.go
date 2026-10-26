// Tests for the ACP mode session factory.
//
// The bug these tests pin down: the factory returned "" as the session id. The
// ACP agent registered the session in its map under that key and answered
// session/new with an empty sessionId — and session/prompt, which requires
// the id, rejected everything as an unknown session. No test caught it
// because the defect was in the caller (cmd/), not in the internal/acp
// package, whose own production factory generates a correct id.
package main

import (
	"path/filepath"
	"testing"

	"github.com/ruanbarroso/barroso-harness/internal/acp"
	"github.com/ruanbarroso/barroso-harness/internal/config"
	"github.com/ruanbarroso/barroso-harness/internal/runtime"
	"github.com/ruanbarroso/barroso-harness/internal/store"
	"github.com/ruanbarroso/barroso-harness/internal/wiring"
)

// newTestACPFactory returns (factory, store) with the database in a temporary
// directory. The store is left open on purpose: closing it before the test
// ends would lose the log.
func newTestACPFactory(t *testing.T) (func(config.Config) (acp.Turno, string), *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Fechar() })
	var open []*wiring.Conjunto
	return func(c config.Config) (acp.Turno, string) {
		return newACPSession(c, st, t.TempDir(), &open, nil)
	}, st
}

// The regression that matters: the id must come back filled in.
func TestNewACPSessionReturnsNonEmptyID(t *testing.T) {
	factory, _ := newTestACPFactory(t)
	turn, id := factory(config.Config{})
	if turn == nil {
		t.Fatal("turn must not be nil")
	}
	if id == "" {
		t.Fatal("ACP session id came back empty: session/new would answer an empty sessionId and session/prompt would reject it as an unknown session")
	}
}

// Two sessions must not share an id: the second would overwrite the first in
// the agent's map, and the client would see the other conversation's history.
func TestNewACPSessionReturnsDistinctIDs(t *testing.T) {
	factory, _ := newTestACPFactory(t)
	_, a := factory(config.Config{})
	_, b := factory(config.Config{})
	if a == b {
		t.Fatalf("two ACP sessions shared the id %q", a)
	}
}

// The given config must reach the session: the ACP agent captures the config
// on session/new and the factory is what hands it to the runtime.
func TestNewACPSessionPropagatesConfig(t *testing.T) {
	factory, _ := newTestACPFactory(t)
	cfg := config.Config{Model: "test-model", Effort: "high"}
	turn, _ := factory(cfg)
	sess, ok := turn.(*runtime.Session)
	if !ok {
		t.Fatalf("turn should be *runtime.Session, got %T", turn)
	}
	if got := sess.Config().Model; got != "test-model" {
		t.Fatalf("session model = %q, want \"test-model\"", got)
	}
}

// With an open database the session is persistent: that is what allows
// resuming in the terminal a conversation started in an ACP client.
func TestNewACPSessionPersistsWhenStoreAvailable(t *testing.T) {
	factory, st := newTestACPFactory(t)
	turn, id := factory(config.Config{})
	sess := turn.(*runtime.Session)
	if !sess.Persistente() {
		t.Fatal("ACP session should be persistent with a store available")
	}
	if sess.ID() != id {
		t.Fatalf("session id = %q, id returned by factory = %q", sess.ID(), id)
	}
	// The session must be indexed in the database, not just bound in memory.
	if err := st.NewSession(id); err != nil {
		t.Fatalf("NewSession(%q) after factory: %v", id, err)
	}
}

// Without a database the factory still returns a valid id and the turn works
// — losing persistence is a degradation, not a failure.
func TestNewACPSessionWorksWithoutStore(t *testing.T) {
	factory, _ := newTestACPFactory(t)
	var open []*wiring.Conjunto
	turn, id := newACPSession(config.Config{}, nil, t.TempDir(), &open, nil)
	if turn == nil {
		t.Fatal("turn must not be nil without a store")
	}
	if id == "" {
		t.Fatal("empty id without a store: a missing database must not break the id")
	}
	_ = factory
	if turn.(*runtime.Session).Persistente() {
		t.Fatal("without a store the session should not report persistence")
	}
}

// session/load: a stored session comes back with its transcript; an unknown
// id is an error (it must not become an empty session with the requested id).
func TestLoadACPSessionReopensFromDatabase(t *testing.T) {
	factory, st := newTestACPFactory(t)
	_, id := factory(config.Config{})
	if _, err := st.StoreEvent(id, "user_message", map[string]any{"role": "user", "content": "hello from before"}); err != nil {
		t.Fatalf("StoreEvent: %v", err)
	}
	var open []*wiring.Conjunto
	turn, err := loadACPSession(config.Config{}, st, t.TempDir(), id, &open, nil)
	if err != nil {
		t.Fatalf("loadACPSession: %v", err)
	}
	h := turn.(*runtime.Session).History()
	if len(h) != 1 || h[0].Conteudo != "hello from before" {
		t.Fatalf("history not rebuilt: %+v", h)
	}
	if _, err := loadACPSession(config.Config{}, st, t.TempDir(), "bh-does-not-exist", &open, nil); err == nil {
		t.Fatal("unknown id should return an error")
	}
}
