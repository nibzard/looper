package looper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// openCursorStore returns a read-write store plus a read-only observer on the
// same database, mirroring the controller and a status command.
func openCursorStore(t *testing.T) (*Store, *Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	w, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	if _, err = w.Event("start", "seed", nil); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	return w, ro
}

// TestSnapshotPairsStateWithCursorUnderConcurrentCommits covers the AX-003
// acceptance that concurrent commits between status and events cause no
// missed transition. The writer commits each state change before its journal
// event, the ordering the engine's result path uses. Every observer snapshot
// must then pair a state whose transitions are all covered by the cursor it
// reports: the cursor may lag the state but can never lead it.
func TestSnapshotPairsStateWithCursorUnderConcurrentCommits(t *testing.T) {
	w, ro := openCursorStore(t)
	const commits = 120
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for n := 1; n <= commits; n++ {
			s := &State{Version: 1, Backlog: Backlog{Goal: fmt.Sprintf("g%d", n)}}
			if err := w.Save(s); err != nil {
				t.Error(err)
				return
			}
			if _, err := w.Event("tick", fmt.Sprintf("commit %d", n), map[string]int{"n": n}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	snapshots := 0
	var last *StoreSnapshot
	for {
		snap, err := ro.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		snapshots++
		if snap.State != nil {
			var n int
			if _, err = fmt.Sscanf(snap.State.Backlog.Goal, "g%d", &n); err != nil {
				t.Fatalf("goal %q: %v", snap.State.Backlog.Goal, err)
			}
			// The seed event plus n-1 committed ticks are always visible with
			// state n; the nth event may or may not have committed yet.
			if snap.HighWater < int64(n) || snap.HighWater > int64(n)+1 {
				t.Fatalf("cursor %d does not pair with state %d", snap.HighWater, n)
			}
		}
		last = snap
		if last.HighWater >= int64(commits)+1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	if snapshots < 2 {
		t.Fatalf("only %d snapshots observed; the race was not exercised", snapshots)
	}
	// Continuation: everything after the first observed cursor reaches the
	// final high-water mark with no gap.
	first := int64(1)
	seen := map[int64]bool{}
	for {
		page, err := ro.EventsPage(EventCursor{EventID: first}, 7)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Items {
			if e.ID <= first || seen[e.ID] {
				t.Fatalf("event %d repeated or out of order after cursor %d", e.ID, first)
			}
			seen[e.ID] = true
		}
		if n := len(page.Items); n > 0 {
			first = page.Items[n-1].ID
		}
		if !page.HasMore {
			break
		}
	}
	for id := int64(2); id <= last.HighWater; id++ {
		if !seen[id] {
			t.Fatalf("event %d missing from continuation below high-water %d", id, last.HighWater)
		}
	}
}

// TestEventsPageFixesHighWater covers a finite page fixing its high-water
// mark when it starts. Events committed after the page started arrive on the
// next page; a completed page reports has_more correctly.
func TestEventsPageFixesHighWater(t *testing.T) {
	w, _ := openCursorStore(t)
	for i := 0; i < 9; i++ {
		if _, err := w.Event("progress", fmt.Sprintf("p%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	page, err := w.EventsPage(EventCursor{}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if page.HighWater != 10 || len(page.Items) != 4 || !page.HasMore {
		t.Fatalf("first page wrong: high %d items %d more %v", page.HighWater, len(page.Items), page.HasMore)
	}
	after := EventCursor{EventID: page.Items[3].ID}
	page, err = w.EventsPage(after, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 || !page.HasMore {
		t.Fatalf("second page wrong: items %d more %v", len(page.Items), page.HasMore)
	}
	after.EventID = page.Items[3].ID
	page, err = w.EventsPage(after, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.HasMore {
		t.Fatalf("final page wrong: items %d more %v", len(page.Items), page.HasMore)
	}
	// The fixed mark does not move for the page that took it, and a later
	// page sees the newly committed events.
	fixed := page.HighWater
	if _, err = w.Event("late", "after the page", nil); err != nil {
		t.Fatal(err)
	}
	if page.HighWater != fixed {
		t.Fatal("a finished page changed its high-water mark")
	}
	next, err := w.EventsPage(EventCursor{EventID: fixed}, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Items) != 1 || next.HighWater != fixed+1 {
		t.Fatalf("late event not paged: items %d high %d", len(next.Items), next.HighWater)
	}
}

// TestStatusCursorComesFromStoreNotView covers stopped runs reporting the
// current cursor: the store's committed high-water mark is authoritative and
// a published view, which is presentation only, never supplies the position.
func TestStatusCursorComesFromStoreNotView(t *testing.T) {
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Command: "status", JSON: true}
	_, dir := projectPaths(c)
	w, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &State{Version: 1, Backlog: Backlog{Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}}
	if err = w.Save(s); err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := 0; i < 4; i++ {
		if last, err = w.Event("progress", fmt.Sprintf("p%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	report := func() Observation {
		t.Helper()
		var out strings.Builder
		if err = inspect(context.Background(), c, &out); err != nil {
			t.Fatal(err)
		}
		var status struct {
			Observation Observation `json:"observation"`
		}
		if err = json.Unmarshal([]byte(out.String()), &status); err != nil {
			t.Fatal(err)
		}
		return status.Observation
	}
	// Stopped run: no controller, no view, current cursor reported.
	if o := report(); o.EventCursor != last || o.View != nil {
		t.Fatalf("stopped run cursor %d (want %d), view %+v", o.EventCursor, last, o.View)
	}
	// A stale view with a wrong cursor must not leak into the report.
	if err = writeJSON(viewPath(dir), &ui.ViewSnapshot{Version: ui.ViewVersion, Session: "run-old", EventCursor: last + 999}); err != nil {
		t.Fatal(err)
	}
	if o := report(); o.EventCursor != last {
		t.Fatalf("view leaked its cursor: %d (want %d)", o.EventCursor, last)
	}
	var out strings.Builder
	if code := doctorJSON(c, &out); code != 0 {
		t.Fatalf("doctor code %d: %s", code, out.String())
	}
	var doc struct {
		Observation Observation `json:"observation"`
	}
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Observation.EventCursor != last {
		t.Fatalf("doctor cursor %d (want %d)", doc.Observation.EventCursor, last)
	}
}

// TestEventsAfterExclusiveAndResetDetected covers exclusive-after semantics
// on the events command and the actionable error for a position beyond the
// retained store.
func TestEventsAfterExclusiveAndResetDetected(t *testing.T) {
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Command: "events", JSON: true}
	_, dir := projectPaths(c)
	w, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	var last int64
	for i := 0; i < 4; i++ {
		if last, err = w.Event("progress", fmt.Sprintf("p%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	run := func(after int64) (string, error) {
		cc := c
		cc.After = after
		var out strings.Builder
		err := inspect(context.Background(), cc, &out)
		return out.String(), err
	}
	// after is exclusive: only the event after the identifier returns.
	out, err := run(last - 1)
	if err != nil || !strings.Contains(out, "p3") || strings.Contains(out, "p2") {
		t.Fatalf("exclusive-after wrong: err %v out %s", err, out)
	}
	if out, err = run(last); err != nil || strings.TrimSpace(out) != "" {
		t.Fatalf("current cursor must return nothing: err %v out %s", err, out)
	}
	// A position beyond the store is a reset, not a silent empty page.
	_, err = run(last + 5)
	if err == nil {
		t.Fatal("a cursor beyond the store must fail")
	}
	var invalid *cursorInvalidError
	if !errors.As(err, &invalid) || invalid.Code() != "cursor_invalid" {
		t.Fatalf("wrong error: %v", err)
	}
	if !strings.Contains(err.Error(), "high-water") || !strings.Contains(err.Error(), "looper events") {
		t.Fatalf("error is not actionable: %v", err)
	}
	// after zero reads from the beginning of the store.
	out, err = run(0)
	if err != nil || !strings.Contains(out, "p0") {
		t.Fatalf("after zero must read the whole store: err %v out %s", err, out)
	}
}

// TestCursorInvalidAfterStoreReset covers a replaced event store: the new
// store has a new identity and a lower high-water mark, so an old cursor is
// refused with an action instead of a silently empty page.
func TestCursorInvalidAfterStoreReset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	a, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 30; i++ {
		if _, err = a.Event("progress", fmt.Sprintf("p%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := a.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	old := EventCursor{EventID: snap.HighWater, StoreID: snap.StoreID, StoreEpoch: snap.Epoch}
	idA, highA := snap.StoreID, snap.HighWater
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	// A closed store checkpoints its WAL away; sidecar files may or may not
	// remain. Both are gone with the database either way.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err = os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	b, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	for i := 0; i < 3; i++ {
		if _, err = b.Event("fresh", fmt.Sprintf("f%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = b.ValidateCursor(old); err == nil {
		t.Fatal("a cursor from the replaced store must be invalid")
	} else {
		var invalid *cursorInvalidError
		if !errors.As(err, &invalid) {
			t.Fatalf("wrong error type: %v", err)
		}
		if invalid.cursor.StoreID != idA || invalid.highWater >= highA {
			t.Fatalf("error lost its evidence: %+v", invalid)
		}
	}
	if _, err = b.RecentEvents(old.EventID, 10); err == nil {
		t.Fatal("events must refuse the replaced store's cursor")
	}
	if _, err = b.EventsPage(old, 10); err == nil {
		t.Fatal("pages must refuse the replaced store's cursor")
	}
	// Observation restarts cleanly from the current store.
	if err = b.ValidateCursor(EventCursor{}); err != nil {
		t.Fatal(err)
	}
	page, err := b.EventsPage(EventCursor{}, 10)
	if err != nil || len(page.Items) != 3 || page.HighWater != 3 {
		t.Fatalf("fresh observation broken: err %v items %d high %d", err, len(page.Items), page.HighWater)
	}
	if page, err = b.EventsPage(EventCursor{EventID: 1, StoreID: idA, StoreEpoch: 1}, 10); err == nil || (page != nil && len(page.Items) != 0) {
		t.Fatalf("foreign identity accepted: err %v page %+v", err, page)
	}
	// An in-place journal reset bumps the epoch; a cursor from the earlier
	// epoch is refused even when the identifiers have regrown past it.
	if _, err = b.db.Exec("UPDATE store_meta SET epoch=epoch+1"); err != nil {
		t.Fatal(err)
	}
	c, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.identity != b.identity || c.epoch != 2 {
		t.Fatalf("epoch not persisted: identity %q epoch %d", c.identity, c.epoch)
	}
	if err = c.ValidateCursor(EventCursor{EventID: 2, StoreID: c.identity, StoreEpoch: 1}); err == nil {
		t.Fatal("an earlier epoch must be invalid")
	} else if !strings.Contains(err.Error(), "epoch") {
		t.Fatalf("epoch error wrong: %v", err)
	}
	if err = c.ValidateCursor(EventCursor{EventID: 2, StoreID: c.identity, StoreEpoch: 2}); err != nil {
		t.Fatalf("the current epoch must continue: %v", err)
	}
}

// TestCursorSurvivesRestartWhileSessionScopeDoesNot covers the two cursor
// scopes. A store-scoped cursor keeps its continuation across a controller
// restart; a session-scoped cursor never borrows another session's position.
func TestCursorSurvivesRestartWhileSessionScopeDoesNot(t *testing.T) {
	w, _ := openCursorStore(t)
	info := &ControllerInfo{Version: 1, Session: "run-one", PID: os.Getpid(), Mode: ControllerModeForeground, Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := w.beginSession(info); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := w.Event("work", fmt.Sprintf("w%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := w.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	store := EventCursor{EventID: snap.HighWater, StoreID: snap.StoreID, StoreEpoch: snap.Epoch}
	session := store
	session.ControllerSession = "run-one"
	for _, c := range []EventCursor{store, session} {
		if err = w.ValidateCursor(c); err != nil {
			t.Fatalf("cursor before restart must be valid: %v", err)
		}
	}
	// A restart begins a new controller session on the same retained store.
	next := &ControllerInfo{Version: 1, Session: "run-two", PID: os.Getpid(), Mode: ControllerModeForeground, Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err = w.beginSession(next); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Event("work", "after restart", nil); err != nil {
		t.Fatal(err)
	}
	if err = w.ValidateCursor(store); err != nil {
		t.Fatalf("store continuation must survive a restart: %v", err)
	}
	events, err := w.RecentEvents(store.EventID, 10)
	if err != nil || len(events) == 0 {
		t.Fatalf("continuation after restart returned nothing: err %v events %d", err, len(events))
	}
	err = w.ValidateCursor(session)
	if err == nil {
		t.Fatal("a session cursor must not transfer to the next controller")
	}
	var invalid *cursorInvalidError
	if !errors.As(err, &invalid) {
		t.Fatalf("wrong error type: %v", err)
	}
	if !strings.Contains(err.Error(), "run-one") || !strings.Contains(err.Error(), "run-two") || !strings.Contains(err.Error(), "looper events --after") {
		t.Fatalf("session error is not actionable: %v", err)
	}
}

// TestLegacyStoreWithoutMetaStillPages covers a read-only attach to a store
// from before store identity existed: the unknown identity degrades to
// high-water validation alone and reading still works.
func TestLegacyStoreWithoutMetaStillPages(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE events (id INTEGER PRIMARY KEY, time TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL, data TEXT NOT NULL); INSERT INTO events(time,kind,message,data) VALUES('2026-11-05T10:00:00Z','progress','old store','null')"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.identity != "" || ro.epoch != 1 {
		t.Fatalf("legacy identity wrong: %q %d", ro.identity, ro.epoch)
	}
	events, err := ro.RecentEvents(0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("legacy paging broken: err %v events %d", err, len(events))
	}
	if _, err = ro.RecentEvents(5, 10); err == nil {
		t.Fatal("a reset legacy store must still refuse a dead cursor")
	}
	if err = ro.ValidateCursor(EventCursor{EventID: 1, StoreID: "store-other"}); err == nil {
		t.Fatal("an unknown identity must not validate a foreign store cursor")
	}
}

// TestEngineViewCursorStartsAtStoreHighWater covers the presentation cursor
// after a restart: the first published view points at the retained store's
// committed position instead of the beginning of history.
func TestEngineViewCursorStartsAtStoreHighWater(t *testing.T) {
	c := repoConfig(t, "run")
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Goal: "g"}); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	w, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err = w.Event("progress", fmt.Sprintf("p%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		removeController(e.Dir, e.controller.Session)
		e.Store.Close()
		lock.Close()
	})
	if e.eventCursor <= 0 {
		t.Fatalf("presentation cursor starts at %d; the retained position was lost", e.eventCursor)
	}
	ro, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	snap, err := ro.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if e.eventCursor != snap.HighWater {
		t.Fatalf("engine cursor %d is not the committed high-water mark %d", e.eventCursor, snap.HighWater)
	}
}
