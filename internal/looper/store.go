package looper

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db                *sql.DB
	session           string
	dir               string
	previous          *previousSession
	shutdownAttempted bool
	// identity and epoch scope continuation cursors to this retained event
	// store. A replaced database reports a new identity; an in-place journal
	// reset records a higher epoch.
	identity string
	epoch    int64
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
CREATE TABLE IF NOT EXISTS store_meta (id INTEGER PRIMARY KEY CHECK(id=1), identity TEXT NOT NULL, epoch INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS checks (id INTEGER PRIMARY KEY, time TEXT NOT NULL, attempt_id TEXT NOT NULL, revision TEXT NOT NULL, kind TEXT NOT NULL, result TEXT NOT NULL, artifact TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS timing_metadata (id INTEGER PRIMARY KEY CHECK(id=1), recorded_since TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY, pid INTEGER NOT NULL, identity TEXT NOT NULL, started TEXT NOT NULL, ended TEXT, outcome TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS processes (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, attempt_id TEXT NOT NULL, kind TEXT NOT NULL, artifact TEXT NOT NULL, pid INTEGER NOT NULL, identity TEXT NOT NULL, started TEXT NOT NULL, ended TEXT, outcome TEXT NOT NULL, source TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS processes_session ON processes(session_id);
CREATE UNIQUE INDEX IF NOT EXISTS processes_artifact_start ON processes(artifact,started);
CREATE TABLE IF NOT EXISTS usage_reports (id TEXT PRIMARY KEY, artifact TEXT NOT NULL, started TEXT NOT NULL, recorded_at TEXT NOT NULL, data TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS usage_report_time ON usage_reports(recorded_at);
CREATE UNIQUE INDEX IF NOT EXISTS usage_report_artifact_start ON usage_reports(artifact,started);
CREATE INDEX IF NOT EXISTS events_kind ON events(kind,id);
CREATE INDEX IF NOT EXISTS events_task ON events(json_extract(data,'$.task_id'),id);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("INSERT OR IGNORE INTO timing_metadata(id,recorded_since) VALUES(1,?)", wallClock().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, err
	}
	// An existing store keeps its recorded identity; a store from before this
	// row existed records one now. The epoch starts at one.
	if _, err = db.Exec("INSERT OR IGNORE INTO store_meta(id,identity,epoch) VALUES(1,?,1)", identifier("store")); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, dir: filepath.Dir(path)}
	s.identity, s.epoch = readStoreMeta(db)
	return s, nil
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
	result, err := s.db.Exec("INSERT INTO events(time,kind,message,data) VALUES(?,?,?,?)", wallClock().UTC().Format(time.RFC3339Nano), kind, message, string(b))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}
func (s *Store) Check(attempt, revision, kind, result, artifact string) error {
	_, err := s.db.Exec("INSERT INTO checks(time,attempt_id,revision,kind,result,artifact) VALUES(?,?,?,?,?,?)", wallClock().UTC().Format(time.RFC3339Nano), attempt, revision, kind, result, artifact)
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

// StoreSnapshot is the saved state together with the committed event
// high-water mark and the event store identity, read from one database
// snapshot. Because both values come from one read, the state a caller
// reports and the cursor it hands out can never disagree. A stopped run has
// no published view; its cursor comes from here.
type StoreSnapshot struct {
	State     *State // nil when no run is saved
	HighWater int64  // committed event high-water mark at this snapshot
	StoreID   string // identity of the retained event store
	Epoch     int64  // store epoch; a cursor from another epoch is invalid
}

// Snapshot returns the saved state, the committed event high-water mark, and
// the event store identity from one consistent read. Every read shares one
// transaction, so a concurrent commit appears in the state and the
// high-water mark together or in neither: a caller that reads events after
// this cursor misses no committed transition (AX-003).
func (s *Store) Snapshot() (*StoreSnapshot, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	snap := &StoreSnapshot{StoreID: s.identity, Epoch: s.epoch}
	var b string
	switch err = tx.QueryRow("SELECT data FROM state WHERE id=1").Scan(&b); {
	case errors.Is(err, sql.ErrNoRows):
		err = nil
	case err != nil:
		return nil, err
	default:
		var v State
		if err = json.Unmarshal([]byte(b), &v); err != nil {
			return nil, err
		}
		snap.State = &v
	}
	if err = tx.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&snap.HighWater); err != nil {
		return nil, err
	}
	return snap, nil
}

// EventCursor names a continuation position in the retained event store.
// EventID is exclusive: reading resumes strictly after it. StoreID and
// StoreEpoch scope the position to one retained event store, so a reset or
// replaced store is detected instead of silently returning an empty page.
// ControllerSession is set only when the position is scoped to one
// controller session; such a cursor never borrows another session's
// position and does not survive a controller restart.
type EventCursor struct {
	EventID           int64  `json:"event_id"`
	StoreID           string `json:"store_id,omitempty"`
	StoreEpoch        int64  `json:"store_epoch,omitempty"`
	ControllerSession string `json:"controller_session,omitempty"`
}

// Scope reports the cursor's continuation scope. Store positions survive
// controller restarts; session positions end with their controller.
func (c EventCursor) Scope() string {
	if c.ControllerSession != "" {
		return "session"
	}
	return "store"
}

// cursorInvalidError reports a continuation cursor that does not belong to
// the store being read. The message always names the recovery: restart
// observation from the current store, or continue from the store cursor.
type cursorInvalidError struct {
	reason    string
	detail    string
	cursor    EventCursor
	storeID   string
	epoch     int64
	highWater int64
}

// Code is the stable code for the machine error contract; it matches the
// cursor_invalid error in docs/contracts/schemas/v2/error.schema.json.
func (e *cursorInvalidError) Code() string { return "cursor_invalid" }

func (e *cursorInvalidError) Error() string { return e.reason + ": " + e.detail }

// readStoreMeta reads the retained event store's identity and epoch. The
// values only aid cursor validation, so an unreadable record (a read-only
// attach to a store from before this table existed) reports an unknown
// identity and epoch one; the committed high-water mark still validates
// bare event identifiers.
func readStoreMeta(q interface {
	QueryRow(query string, args ...any) *sql.Row
}) (string, int64) {
	var identity string
	var epoch int64
	if err := q.QueryRow("SELECT identity,epoch FROM store_meta WHERE id=1").Scan(&identity, &epoch); err != nil {
		return "", 1
	}
	if epoch < 1 {
		epoch = 1
	}
	return identity, epoch
}

// highWater returns the committed event high-water mark.
func (s *Store) highWater() (int64, error) {
	var high int64
	err := s.db.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&high)
	return high, err
}

// latestSession returns the newest controller session recorded in the
// store, or "" when none is recorded. A store opened for writing reports
// its own session, which the repository lock keeps current; observers read
// the sessions table.
func (s *Store) latestSession() (string, error) {
	if s.session != "" {
		return s.session, nil
	}
	var id string
	err := s.db.QueryRow("SELECT id FROM sessions ORDER BY rowid DESC LIMIT 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		// A store from before the sessions table existed records none.
		if strings.Contains(err.Error(), "no such table") {
			return "", nil
		}
		return "", err
	}
	return id, nil
}

// ValidateCursor checks a continuation cursor against this store: the store
// identity and epoch must match, the position must not be beyond the
// committed high-water mark, and a session-scoped cursor must name the
// current controller session. It returns nil when the position continues
// here and an actionable error when the store was reset or replaced.
func (s *Store) ValidateCursor(c EventCursor) error {
	high, err := s.highWater()
	if err != nil {
		return err
	}
	return s.validateCursorAt(c, high)
}

func (s *Store) validateCursorAt(c EventCursor, high int64) error {
	if c.EventID > high {
		return &cursorInvalidError{
			reason: fmt.Sprintf("continuation cursor %d is beyond this event store's committed high-water mark %d", c.EventID, high),
			detail: "the event store was reset or replaced since the cursor was taken; restart observation from the current store with: looper events",
			cursor: c, storeID: s.identity, epoch: s.epoch, highWater: high,
		}
	}
	if c.StoreEpoch != 0 && s.epoch != 0 && c.StoreEpoch != s.epoch {
		return &cursorInvalidError{
			reason: fmt.Sprintf("continuation cursor %d is from epoch %d of this event store, which is now at epoch %d", c.EventID, c.StoreEpoch, s.epoch),
			detail: "the event journal was reset in place; restart observation from the current store with: looper events",
			cursor: c, storeID: s.identity, epoch: s.epoch, highWater: high,
		}
	}
	if c.StoreID != "" && c.StoreID != s.identity {
		current := s.identity
		if current == "" {
			current = "identity not recorded"
		}
		return &cursorInvalidError{
			reason: fmt.Sprintf("continuation cursor %d belongs to event store %s, not this store (%s)", c.EventID, c.StoreID, current),
			detail: "the event store was replaced; restart observation from the current store with: looper events",
			cursor: c, storeID: s.identity, epoch: s.epoch, highWater: high,
		}
	}
	if c.ControllerSession != "" {
		latest, err := s.latestSession()
		if err != nil {
			return err
		}
		if latest != c.ControllerSession {
			current := latest
			if current == "" {
				current = "none"
			}
			return &cursorInvalidError{
				reason: fmt.Sprintf("session-scoped cursor %d names controller session %s, which is not the current session (%s)", c.EventID, c.ControllerSession, current),
				detail: "a session position never transfers to another controller; continue from the store cursor with: looper events --after " + fmt.Sprint(c.EventID),
				cursor: c, storeID: s.identity, epoch: s.epoch, highWater: high,
			}
		}
	}
	return nil
}

// EventsPage is one finite page of events read with exclusive-after
// semantics.
type EventsPage struct {
	Items     []Event
	After     int64 // the exclusive lower bound the page was read with
	HighWater int64 // the committed high-water mark fixed when the page started
	HasMore   bool  // true when another matching event follows within HighWater
	// Cursor is the continuation position for the next page: the last
	// returned identifier while more matches may follow, or HighWater once
	// no more do. Advancing to the mark on an empty or exhausted page keeps
	// a filtered query from rescanning the range it already read.
	Cursor int64
}

// EventQuery describes one bounded, filtered read of the retained event
// journal. Filters match structured columns and recorded payload fields
// only; message text never decides a match. Every bound applies inside the
// database, so a large journal never serializes into memory just to be
// paged.
type EventQuery struct {
	// After is the exclusive continuation position for forward reads.
	After int64
	// Tail reads the newest Limit matching events, oldest first within that
	// tail, instead of a forward page.
	Tail bool
	// Limit bounds the page; zero selects the default page size.
	Limit int
	// Full reads every matching retained event, ignoring Limit. A full read
	// is always a forward read from the beginning of the store.
	Full bool
	// Task matches events that carry this structured task identity in their
	// recorded payload (the data field's task_id). Historical events without
	// a structured identity do not match; they stay readable without the
	// filter.
	Task string
	// Kind matches the event kind exactly.
	Kind string
}

// filterSQL returns the predicate fragment shared by filtered event reads,
// with its arguments.
func (q EventQuery) filterSQL() (string, []any) {
	var clauses []string
	var args []any
	if q.Kind != "" {
		clauses = append(clauses, "kind=?")
		args = append(args, q.Kind)
	}
	if q.Task != "" {
		clauses = append(clauses, "json_extract(data,'$.task_id')=?")
		args = append(args, q.Task)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

// EventPage returns one finite page of events. The page fixes its high-water
// mark from the store when it starts, so concurrent writers cannot keep the
// page open: events committed after the page started arrive on the next
// page. A position beyond the retained store is an actionable cursor error,
// never a silently empty page.
func (s *Store) EventPage(q EventQuery) (*EventsPage, error) {
	high, err := s.highWater()
	if err != nil {
		return nil, err
	}
	if !q.Tail {
		if err = s.validateCursorAt(EventCursor{EventID: q.After}, high); err != nil {
			return nil, err
		}
	}
	return s.readEventPage(q, high)
}

// EventsPage returns one finite page of events strictly after the exclusive
// cursor position. The page fixes its high-water mark from the store when
// it starts, so concurrent writers cannot keep the page open: events
// committed after the page started arrive on the next page. An invalid
// cursor returns an actionable error rather than a silently empty page.
func (s *Store) EventsPage(after EventCursor, limit int) (*EventsPage, error) {
	if limit <= 0 {
		limit = 40
	}
	high, err := s.highWater()
	if err != nil {
		return nil, err
	}
	if err = s.validateCursorAt(after, high); err != nil {
		return nil, err
	}
	return s.readEventPage(EventQuery{After: after.EventID, Limit: limit}, high)
}

// readEventPage reads the filtered rows below the fixed high-water mark and
// resolves the page's continuation.
func (s *Store) readEventPage(q EventQuery, high int64) (*EventsPage, error) {
	if q.Full {
		// The complete history is a forward read from the beginning of the
		// store with no bound.
		q.Tail, q.After, q.Limit = false, 0, 0
	}
	limit := q.Limit
	if limit <= 0 && !q.Full {
		limit = defaultEventPageLimit
	}
	filter, filterArgs := q.filterSQL()
	page := &EventsPage{After: q.After, HighWater: high, Cursor: q.After}
	var (
		rows *sql.Rows
		err  error
	)
	if q.Tail {
		query := "SELECT id,time,kind,message,data FROM (SELECT id,time,kind,message,data FROM events WHERE id<=?" + filter + " ORDER BY id DESC LIMIT ?) ORDER BY id"
		args := append([]any{high}, filterArgs...)
		rows, err = s.db.Query(query, append(args, limit)...)
	} else {
		query := "SELECT id,time,kind,message,data FROM events WHERE id>? AND id<=?" + filter + " ORDER BY id"
		args := append([]any{q.After, high}, filterArgs...)
		if limit > 0 {
			query += " LIMIT ?"
			args = append(args, limit)
		}
		rows, err = s.db.Query(query, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.Cursor = items[n-1].ID
	}
	switch {
	case q.Tail:
		// A tail contains the newest matching events, so nothing more
		// matches below the fixed mark: continue from the mark itself.
		page.HasMore, page.Cursor = false, high
	case limit > 0 && len(items) == limit:
		// The page stopped at its bound. Probe whether another matching
		// event follows within the fixed mark; when none does, the
		// continuation advances to the mark instead of the last item.
		more, err := s.eventExistsAfter(page.Cursor, high, filter, filterArgs)
		if err != nil {
			return nil, err
		}
		page.HasMore = more
		if !more {
			page.Cursor = high
		}
	default:
		// The read exhausted every match below the fixed mark.
		page.HasMore, page.Cursor = false, high
	}
	return page, nil
}

// chattyEventKinds repeat without changing durable state: progress ticks,
// activity heartbeats, configuration echoes, and wait notices. The last
// durable transition ignores them, so a compact status names a real change
// instead of the newest heartbeat.
var chattyEventKinds = []string{"progress", "worker_activity", "planner-activity", "review-activity", "review-heartbeat", "waiting", "config", "session"}

// LastTransition returns the newest event that reports a durable transition,
// or nil when the journal holds none. It reads one indexed row, so a compact
// status never scans the journal it summarizes.
func (s *Store) LastTransition() (*Event, error) {
	placeholders := make([]string, len(chattyEventKinds))
	args := make([]any, len(chattyEventKinds))
	for i, kind := range chattyEventKinds {
		placeholders[i] = "?"
		args[i] = kind
	}
	query := "SELECT id,time,kind,message,data FROM events WHERE kind NOT IN (" +
		strings.Join(placeholders, ",") + ") ORDER BY id DESC LIMIT 1"
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events, err := scanEvents(rows)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}
	return &events[0], nil
}

// CheckRecord is one verification record from the checks table. The row
// identifier pages a task's check history (AX-005): it is store-scoped and
// increases with insertion order, so an exclusive-after page never repeats or
// skips a record.
type CheckRecord struct {
	ID        int64  `json:"id"`
	Time      string `json:"time"`
	AttemptID string `json:"attempt_id"`
	Revision  string `json:"revision"`
	Kind      string `json:"kind"`
	Result    string `json:"result"`
	Artifact  string `json:"artifact"`
}

// ChecksPage is one bounded page of a task's check records. Total is the
// complete count across every attempt named by the query, not only the page.
type ChecksPage struct {
	Items   []CheckRecord
	After   int64 // the exclusive lower bound the page was read with
	Cursor  int64 // the newest identifier in the page, or After with none
	HasMore bool  // another record exists beyond the returned window
	Total   int
}

// checksQueryChunk bounds the identifiers one checks query carries, so a
// task whose history grew past the database's variable limit still pages.
const checksQueryChunk = 400

// ChecksPage returns one bounded page of the check records of the given
// attempts. Tail selects the newest records; otherwise the page reads strictly
// after the given row identifier, oldest first. Every bound applies inside the
// database, so a task with a long repair history never serializes all of it
// just to show the newest page. Attempt identifiers are queried in chunks and
// the per-chunk windows merged, so the page is exact at any history size.
func (s *Store) ChecksPage(attemptIDs []string, after int64, limit int, tail bool) (*ChecksPage, error) {
	page := &ChecksPage{After: after, Items: []CheckRecord{}}
	if len(attemptIDs) == 0 || limit <= 0 {
		return page, nil
	}
	countArgs := func(ids []string) []any {
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		return args
	}
	scan := func(rows *sql.Rows) ([]CheckRecord, error) {
		var out []CheckRecord
		for rows.Next() {
			var c CheckRecord
			if err := rows.Scan(&c.ID, &c.Time, &c.AttemptID, &c.Revision, &c.Kind, &c.Result, &c.Artifact); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, c)
		}
		err := rows.Err()
		rows.Close()
		return out, err
	}
	const cols = "id,time,attempt_id,revision,kind,result,artifact"
	for start := 0; start < len(attemptIDs); start += checksQueryChunk {
		ids := attemptIDs[start : start+min(checksQueryChunk, len(attemptIDs)-start)]
		placeholders := strings.Repeat("?,", len(ids))
		in := "(" + placeholders[:len(placeholders)-1] + ")"
		var total int
		if err := s.db.QueryRow("SELECT count(*) FROM checks WHERE attempt_id IN "+in, countArgs(ids)...).Scan(&total); err != nil {
			return nil, err
		}
		page.Total += total
		var (
			rows *sql.Rows
			err  error
		)
		if tail {
			query := "SELECT " + cols + " FROM (SELECT " + cols + " FROM checks WHERE attempt_id IN " + in + " ORDER BY id DESC LIMIT ?) ORDER BY id"
			rows, err = s.db.Query(query, append(countArgs(ids), limit)...)
		} else {
			query := "SELECT " + cols + " FROM checks WHERE attempt_id IN " + in + " AND id>? ORDER BY id LIMIT ?"
			rows, err = s.db.Query(query, append(countArgs(ids), after, limit)...)
		}
		if err != nil {
			return nil, err
		}
		items, err := scan(rows)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, items...)
		if !tail && len(items) == limit {
			// The chunk filled its bound; probe whether another record
			// follows, so the continuation advances only when it should.
			var more bool
			probe := "SELECT EXISTS(SELECT 1 FROM checks WHERE attempt_id IN " + in + " AND id>?)"
			if err = s.db.QueryRow(probe, append(countArgs(ids), items[len(items)-1].ID)...).Scan(&more); err != nil {
				return nil, err
			}
			page.HasMore = page.HasMore || more
		}
	}
	// Merge the chunk windows oldest first, then keep the page: the first
	// records of a forward read, the newest records of a tail.
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].ID < page.Items[j].ID })
	if len(page.Items) > limit {
		page.HasMore = true
		if tail {
			page.Items = page.Items[len(page.Items)-limit:]
		} else {
			page.Items = page.Items[:limit]
		}
	}
	if n := len(page.Items); n > 0 {
		page.Cursor = page.Items[n-1].ID
	}
	if tail {
		// The tail holds the newest records; older ones exist below it.
		page.HasMore = page.Total > len(page.Items)
	}
	return page, nil
}

// eventExistsAfter reports whether another matching event exists strictly
// after the position and at or below the fixed high-water mark.
func (s *Store) eventExistsAfter(after, high int64, filter string, filterArgs []any) (bool, error) {
	query := "SELECT EXISTS(SELECT 1 FROM events WHERE id>? AND id<=?" + filter + ")"
	var exists bool
	err := s.db.QueryRow(query, append([]any{after, high}, filterArgs...)...).Scan(&exists)
	return exists, err
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
