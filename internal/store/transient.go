package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// busyTimeoutStr is the sqlite busy_timeout in ms. Several `bh -acp` processes
// (one per conversation) share one sessions.db; after a connector restart they
// all open it at once, and 5s was not enough for the writer queue to drain.
const busyTimeoutStr = "30000"

// pingTentativas is how many times Open pings before giving up.
const pingTentativas = 5

// transitório reports whether err is a sqlite lock (SQLITE_BUSY,
// "database is locked") or any other failure that clears by retrying.
//
// It is deliberately narrow: only lock cases count as transient. A missing
// session (ErrSessaoInexistente) is permanent — retrying it would keep a client
// waiting on a conversation that will never appear.
func transitório(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrTransitorio) {
		return true
	}
	if errors.Is(err, ErrSessaoInexistente) || errors.Is(err, sql.ErrNoRows) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "database schema is locked")
}

// backoff returns the wait before retry n (0-based): 200ms, 400ms, ... capped at 2s.
func backoff(n int) time.Duration {
	d := 200 * time.Millisecond << uint(n)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d
}

// pingRetry pings the database, retrying only transient (lock) failures.
func pingRetry(db *sql.DB, tentativas int) error {
	var err error
	for i := 0; i < tentativas; i++ {
		if err = db.Ping(); err == nil || !transitório(err) {
			return err
		}
		time.Sleep(backoff(i))
	}
	return err
}

// retryLeitura runs a read, retrying lock failures with backoff. If the lock
// persists, the error is tagged with ErrTransitorio so callers can tell
// "try again later" from "this session does not exist".
func retryLeitura[T any](fn func() (T, error)) (T, error) {
	var (
		v   T
		err error
	)
	for i := 0; i < 5; i++ {
		v, err = fn()
		if err == nil || !transitório(err) {
			return v, err
		}
		time.Sleep(backoff(i))
	}
	return v, fmt.Errorf("%w: %w", ErrTransitorio, err)
}
