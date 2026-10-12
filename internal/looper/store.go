package looper

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db                *sql.DB
	session           string
	dir               string
	previous          *previousSession
	shutdownAttempted bool
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS state (id INTEGER PRIMARY KEY CHECK(id=1), data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY, time TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL, data TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS checks (id INTEGER PRIMARY KEY, time TEXT NOT NULL, attempt_id TEXT NOT NULL, revision TEXT NOT NULL, kind TEXT NOT NULL, result TEXT NOT NULL, artifact TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS timing_metadata (id INTEGER PRIMARY KEY CHECK(id=1), recorded_since TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, pid INTEGER NOT NULL, identity TEXT NOT NULL, started TEXT NOT NULL, ended TEXT, outcome TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS processes (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, attempt_id TEXT NOT NULL, kind TEXT NOT NULL, artifact TEXT NOT NULL, pid INTEGER NOT NULL, identity TEXT NOT NULL, started TEXT NOT NULL, ended TEXT, outcome TEXT NOT NULL, source TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS processes_session ON processes(session_id);
CREATE UNIQUE INDEX IF NOT EXISTS processes_artifact_start ON processes(artifact,started);
CREATE TABLE IF NOT EXISTS usage_reports (id TEXT PRIMARY KEY, artifact TEXT NOT NULL, started TEXT NOT NULL, recorded_at TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS usage_report_time ON usage_reports(recorded_at);
CREATE UNIQUE INDEX IF NOT EXISTS usage_report_artifact_start ON usage_reports(artifact,started);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO timing_metadata(id,recorded_since) VALUES(1,?)", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dir: filepath.Dir(path)}, nil
}
func (s *Store) Close() error {
	var endErr error
	if s.session != "" && !s.shutdownAttempted {
		_, endErr = s.db.Exec("UPDATE sessions SET ended=?,outcome='stopped' WHERE id=? AND ended IS NULL", time.Now().UTC().Format(time.RFC3339Nano), s.session)
	}
	return errors.Join(endErr, s.db.Close())
}
func (s *Store) Load() (*State, error) {
	var b string
	err := s.db.QueryRow("SELECT data FROM state WHERE id=1").Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var v State
	err = json.Unmarshal([]byte(b), &v)
	return &v, err
}
func (s *Store) Save(v *State) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO state(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data", string(b))
	return err
}
func (s *Store) Event(kind, message string, data any) (int64, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	result, err := s.db.Exec("INSERT INTO events(time,kind,message,data) VALUES(?,?,?,?)", time.Now().UTC().Format(time.RFC3339Nano), kind, message, string(b))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
func (s *Store) Check(attempt, revision, kind, result, artifact string) error {
	_, err := s.db.Exec("INSERT INTO checks(time,attempt_id,revision,kind,result,artifact) VALUES(?,?,?,?,?,?)", time.Now().UTC().Format(time.RFC3339Nano), attempt, revision, kind, result, artifact)
	return err
}
func (s *Store) Events(after int64) ([]Event, error) {
	rows, err := s.db.Query("SELECT id,time,kind,message,data FROM events WHERE id>? ORDER BY id", after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

type Event struct {
	Data    json.RawMessage `json:"data,omitempty"`
	ID      int64           `json:"id"`
	Time    string          `json:"time"`
	Kind    string          `json:"kind"`
	Message string          `json:"message"`
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".looper-write-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600)
}
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
