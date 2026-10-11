package looper

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// viewPath is the atomically published presentation snapshot. It is never an
// input to task execution; the event journal and state stay authoritative.
func viewPath(dir string) string { return filepath.Join(dir, "view.json") }

// snapshot copies the owner's presentation state. The UI never reads mutable
// engine objects.
func (e *Engine) snapshot(draining bool) ui.ViewSnapshot {
	done, blocked, total := 0, 0, 0
	for _, t := range e.State.Backlog.Tasks {
		if t.Marker() {
			continue
		}
		total++
		switch t.Status {
		case "done":
			done++
		case "blocked":
			blocked++
		}
	}
	v := ui.ViewSnapshot{
		Version: ui.ViewVersion, Goal: e.State.Backlog.Goal,
		Now:         time.Now().UTC().Format(time.RFC3339),
		EventCursor: e.eventCursor,
		State:       e.activity, ActivityDetail: e.activityDetail, ActivityLog: e.activityLog, ActivityUpdated: e.activityUpdated,
		Draining: draining, Finished: e.State.Finished,
	}
	if e.controller != nil {
		v.Session, v.Mode, v.Started = e.controller.Session, e.controller.Mode, e.controller.Started
	}
	if e.State != nil {
		v.Counters = ui.ViewCounters{
			Done: done, Total: total, Ready: len(e.State.Backlog.Ready()),
			Blocked: blocked, Integrated: e.State.Integrated,
		}
		for _, a := range e.State.Attempts {
			switch a.Phase {
			case "done", "blocked", "abandoned":
				continue
			}
			v.Attempts = append(v.Attempts, ui.AttemptView{
				ID: a.ID, TaskID: a.Task.ID, Title: a.Task.Title, Phase: a.Phase,
				Agent: a.Agent, Repairs: a.Repairs, Base: a.Base, Worktree: a.Worktree,
			})
			switch a.Phase {
			case "verifying":
				v.Counters.Checking++
			case "ready":
			default:
				v.Counters.InFlight++
			}
		}
		for _, t := range e.State.Backlog.Ready() {
			claimed := false
			for _, a := range e.State.Attempts {
				if a.Task.ID == t.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "abandoned" {
					claimed = true
				}
			}
			if !claimed {
				v.Ready = append(v.Ready, ui.TaskView{ID: t.ID, Title: t.Title})
			}
		}
		for _, t := range e.State.Backlog.Tasks {
			if t.Status != "blocked" {
				continue
			}
			v.Blocked = append(v.Blocked, ui.BlockedView{ID: t.ID, Title: t.Title, Reasons: t.Blockers})
		}
	}
	return v
}

// publishView writes the snapshot atomically and notifies an in-process
// dashboard through a bounded, nonblocking channel. A slow or closed
// dashboard coalesces to the latest snapshot and never blocks the engine.
func (e *Engine) publishView(draining bool) {
	if e.controller == nil {
		return
	}
	v := e.snapshot(draining)
	if data, err := json.Marshal(v); err == nil {
		_ = atomicWrite(viewPath(e.Dir), data, 0600)
	}
	if e.viewNotify != nil {
		select {
		case e.viewNotify <- v:
		default: // coalesce: the latest snapshot replaces the pending one
			select {
			case <-e.viewNotify:
			default:
			}
			select {
			case e.viewNotify <- v:
			default:
			}
		}
	}
}

// clearView removes the stale view when ownership ends. Observers treat a
// missing view as no active run instead of trusting old data.
func clearView(dir string) { os.Remove(viewPath(dir)) }

// LoadView reads the published snapshot for observers. A missing or invalid
// file returns nil; callers keep their last valid view and mark it stale.
func LoadView(dir string) *ui.ViewSnapshot {
	data, err := os.ReadFile(viewPath(dir))
	if err != nil {
		return nil
	}
	var v ui.ViewSnapshot
	if json.Unmarshal(data, &v) != nil || v.Version != ui.ViewVersion {
		return nil
	}
	return &v
}

// OpenStoreReadonly attaches to an existing store without schema setup or
// writes. The regular OpenStore creates tables and sets journal modes, which
// observers must not do against a live controller.
func OpenStoreReadonly(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// RecentEvents pages the journal by identifier with a bounded tail. after < 0
// returns the newest bounded tail; otherwise it returns events after the
// identifier.
func (s *Store) RecentEvents(after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 40
	}
	if after < 0 {
		rows, err := s.db.Query("SELECT id,time,kind,message FROM (SELECT id,time,kind,message FROM events ORDER BY id DESC LIMIT ?) ORDER BY id", limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanEvents(rows)
	}
	rows, err := s.db.Query("SELECT id,time,kind,message FROM events WHERE id>? ORDER BY id LIMIT ?", after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEvents(rows)
}

func scanEvents(rows *sql.Rows) ([]Event, error) {
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Time, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
