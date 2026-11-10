package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

func TestTransitorioClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{ErrSessaoInexistente, false},
		{sql.ErrNoRows, false},
		{errors.New("database is locked (261)"), true},
		{fmt.Errorf("store: session x: %w", errors.New("SQLITE_BUSY")), true},
		{errors.New("disk full"), false},
		{ErrTransitorio, true},
	}
	for _, c := range cases {
		if got := transitório(c.err); got != c.want {
			t.Errorf("transitório(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestRetryLeituraRecoversFromLock(t *testing.T) {
	n := 0
	v, err := retryLeitura(func() (int, error) {
		n++
		if n < 3 {
			return 0, errors.New("database is locked (261)")
		}
		return 42, nil
	})
	if err != nil || v != 42 || n != 3 {
		t.Fatalf("v=%d err=%v n=%d", v, err, n)
	}
}

func TestRetryLeituraMarksPersistentLockAsTransient(t *testing.T) {
	_, err := retryLeitura(func() (int, error) { return 0, errors.New("database is locked (261)") })
	if !errors.Is(err, ErrTransitorio) {
		t.Fatalf("persistent lock must wrap ErrTransitorio, got %v", err)
	}
}

func TestRetryLeituraDoesNotRetryMissingSession(t *testing.T) {
	n := 0
	_, err := retryLeitura(func() (int, error) { n++; return 0, ErrSessaoInexistente })
	if !errors.Is(err, ErrSessaoInexistente) || errors.Is(err, ErrTransitorio) || n != 1 {
		t.Fatalf("err=%v n=%d", err, n)
	}
}

func TestGetSessionMissingStaysPermanent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fechar()
	if _, err := st.GetSession("ghost"); !errors.Is(err, ErrSessaoInexistente) {
		t.Fatalf("got %v", err)
	}
}
