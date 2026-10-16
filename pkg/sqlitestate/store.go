// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
package sqlitestate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mephistofox/yookassa-to-mynalog/pkg/state"
)

type Store struct {
	db   *sql.DB
	lock *os.File
	path string
}
type Record = state.Record

func Open(path string, writable bool) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: abs}
	fail := func(err error) (*Store, error) { s.Close(); return nil, err }
	if writable {
		if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, err
		}
		s.lock, err = os.OpenFile(abs+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, err
		}
		if err = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			return fail(errors.New("another receipt writer is running"))
		}
		f, e := os.OpenFile(abs, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if e != nil {
			return fail(e)
		}
		f.Close()
		if err = os.Chmod(abs, 0600); err != nil {
			return fail(err)
		}
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("_busy_timeout", "5000")
	q.Set("_foreign_keys", "on")
	if writable {
		q.Set("_journal_mode", "WAL")
		q.Set("_synchronous", "FULL")
	} else {
		q.Set("mode", "ro")
	}
	u.RawQuery = q.Encode()
	s.db, err = sql.Open("sqlite3", u.String())
	if err != nil {
		return fail(err)
	}
	s.db.SetMaxOpenConns(1)
	if err = s.db.Ping(); err != nil {
		return fail(err)
	}
	if writable {
		var version int
		if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return fail(err)
		}
		if version > 1 {
			return fail(errors.New("state schema is newer than this binary"))
		}
		var pythonState int
		if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='sync_state'").Scan(&pythonState); err != nil {
			return fail(err)
		}
		if pythonState > 0 {
			return fail(errors.New("Python state is not compatible; select a separate Go STATE_DB"))
		}
		_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,phase TEXT NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(kind,id));
CREATE INDEX IF NOT EXISTS records_phase ON records(kind,phase);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
PRAGMA user_version=1;`)
		if err != nil {
			return fail(err)
		}
	}
	return s, nil
}
func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.lock != nil {
		s.lock.Close()
	}
	return err
}
func (s *Store) Meta(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec("INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
func (s *Store) Get(kind, id string, out any) (bool, error) {
	var payload string
	err := s.db.QueryRow("SELECT payload FROM records WHERE kind=? AND id=?", kind, id).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(payload), out)
}
func (s *Store) List(kind string) ([]json.RawMessage, error) {
	rows, err := s.db.Query("SELECT payload FROM records WHERE kind=? ORDER BY id", kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []json.RawMessage
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		result = append(result, json.RawMessage(b))
	}
	return result, rows.Err()
}

// Save atomically commits domain state and its delivery outbox entry.
func (s *Store) Save(records ...Record) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		b, e := json.Marshal(r.Value)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO records(kind,id,phase,payload) VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET phase=excluded.phase,payload=excluded.payload", r.Kind, r.ID, r.Phase, string(b)); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Counts() (map[string]int, error) {
	rows, err := s.db.Query("SELECT kind,phase,count(*) FROM records GROUP BY kind,phase ORDER BY kind,phase")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]int{}
	for rows.Next() {
		var k, p string
		var n int
		if err = rows.Scan(&k, &p, &n); err != nil {
			return nil, err
		}
		result[k+"."+p] = n
	}
	return result, rows.Err()
}
func (s *Store) Backup(ctx context.Context, destination string) error {
	abs, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("backup destination must not exist and its parent must be writable")
	}
	f.Close()
	// SQLite VACUUM INTO takes an expression and produces a transactionally
	// consistent copy including WAL data; never copy just the live .db file.
	if _, err = s.db.ExecContext(ctx, "VACUUM INTO ?", abs); err != nil {
		os.Remove(abs)
		return fmt.Errorf("sqlite backup: %w", err)
	}
	return os.Chmod(abs, 0600)
}

type PendingJob = state.PendingJob

// Pending returns operator diagnostics without customer contacts or secrets.
func (s *Store) Pending() ([]PendingJob, error) {
	rows, err := s.db.Query("SELECT kind,id,phase,payload FROM records WHERE phase NOT IN ('done','sent','superseded') ORDER BY kind,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PendingJob{}
	for rows.Next() {
		var p PendingJob
		var payload []byte
		if err = rows.Scan(&p.Kind, &p.ID, &p.Phase, &payload); err != nil {
			return nil, err
		}
		var detail struct {
			Attempts int
			Next     string
			Error    string
		}
		if err = json.Unmarshal(payload, &detail); err != nil {
			return nil, err
		}
		p.Attempts = detail.Attempts
		p.Next = detail.Next
		p.Error = detail.Error
		result = append(result, p)
	}
	return result, rows.Err()
}
