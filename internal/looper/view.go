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
		Root: e.Config.Root, Project: filepath.Base(e.Config.Root), Tasks: make([]ui.TaskView, 0),
		WorkerLimit: e.capacity, CheckLimit: e.Config.Checks,
		Now:         time.Now().UTC().Format(time.RFC3339),
		EventCursor: e.eventCursor,
		State:       e.activity, ActivityDetail: e.activityDetail, ActivityLog: e.activityLog, ActivityUpdated: e.activityUpdated,
		ActivityAgent: e.activityAgent,
		Draining:      draining,
		// The completion banner stays hidden while input needs correction,
		// even when the last accepted state was finished.
		Finished: e.State.Finished && !e.inputPending,
	}
	if e.inputPending {
		v.State, v.ActivityDetail = "waiting", e.inputProblem
		v.ActivityLog, v.ActivityUpdated, v.ActivityAgent = "", "", ""
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
			case "done", "blocked", "cancelled":
				continue
			}
			v.Attempts = append(v.Attempts, attemptView(a))
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
				if a.Task.ID == t.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "cancelled" {
					claimed = true
				}
			}
			if !claimed {
				v.Ready = append(v.Ready, ui.TaskView{ID: t.ID, Title: t.Title})
			}
		}
		for _, t := range e.State.Backlog.Tasks {
			if t.Marker() {
				continue
			}
			tv := taskView(t, e.State.Backlog)
			// Full acceptance text is loaded only when the inspector opens.
			tv.Details = ""
			for i := len(e.State.Attempts) - 1; i >= 0; i-- {
				if a := e.State.Attempts[i]; a.Task.ID == t.ID {
					av := attemptView(a)
					tv.Attempt = &av
					break
				}
			}
			v.Tasks = append(v.Tasks, tv)
			if t.Status != "blocked" {
				continue
			}
			v.Blocked = append(v.Blocked, ui.BlockedView{ID: t.ID, Title: t.Title, Reasons: append([]string(nil), t.Blockers...), UnblockWhen: t.UnblockWhen})
		}
		for _, r := range e.State.Imports {
			v.Imports = append(v.Imports, importView(r))
		}
	}
	if v.State == "" && !draining && len(v.Attempts) == 0 && v.Counters.Ready > 0 {
		if e.reviewDue() {
			v.DispatchReason = "Milestone review is due before dispatch"
		} else {
			v.DispatchReason = "Preparing ready tasks for dispatch"
		}
	}
	return v
}

// importView copies one durable receipt into the presentation contract.
// Dashboards read import state from these records, never from event text.
func importView(r *ImportRecord) ui.ImportView {
	v := ui.ImportView{
		RequestID: r.RequestID, Status: r.Status,
		Plans: append([]string(nil), r.SourcePaths...), AddedTasks: append([]string(nil), r.AddedTasks...),
		Conflicts: append([]string(nil), r.Conflicts...), Adjustments: append([]string(nil), r.Adjustments...),
		Error: r.Error, Enqueued: r.Enqueued, Applied: r.Applied,
	}
	for _, c := range r.Covered {
		v.Covered = append(v.Covered, ui.CoveredView{Requirement: c.Requirement, TaskID: c.TaskID, Evidence: c.Evidence})
	}
	for _, g := range r.Gates {
		v.Gates = append(v.Gates, ui.ImportGateView{TaskID: g.TaskID, DependsOn: append([]string(nil), g.DependsOn...)})
	}
	return v
}

func attemptView(a *Attempt) ui.AttemptView {
	return ui.AttemptView{ID: a.ID, TaskID: a.Task.ID, Title: a.Task.Title, Phase: a.Phase,
		Agent: a.Agent, Repairs: a.Repairs, Repeats: a.Repeats, Base: a.Base, Worktree: a.Worktree, LogDir: a.Directory}
}

func taskView(t Task, b Backlog) ui.TaskView {
	v := ui.TaskView{ID: t.ID, Title: t.Title, Status: t.Status, Deferred: t.Deferred(), Revision: hashJSON(t), Details: t.Details,
		DependsOn: append([]string(nil), t.DependsOn...), Blockers: append([]string(nil), t.Blockers...), UnblockWhen: t.UnblockWhen}
	for _, id := range t.DependsOn {
		if dep := b.Task(id); dep == nil || dep.Status != "done" {
			v.WaitingOn = append(v.WaitingOn, id)
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
	return openStoreURI(path, "mode=ro")
}

// OpenStoreImmutable attaches to a fully checkpointed store so the read
// creates no file at all: a plain read-only attach to a WAL database
// materializes the shared-memory sidecars beside it, which a reader that
// must change nothing (AX-025 run discovery) cannot do. The caller promises
// no live writer holds the store and no write-ahead log waits to be folded
// in; a database with a pending log needs OpenStoreReadonly instead.
func OpenStoreImmutable(path string) (*Store, error) {
	return openStoreURI(path, "mode=ro&immutable=1")
}

func openStoreURI(path, options string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+options)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	// A read-only attach still meets the writer's locks: a controller's
	// shutdown checkpoint holds the database briefly, and a status query that
	// races it must wait the writer out (AX-022), not report the run missing.
	if _, err = db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, err
	}
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, dir: filepath.Dir(path)}
	// A store that predates store_meta, still owned by an older controller,
	// reports an unknown identity; cursors from it validate against the
	// committed high-water mark alone.
	s.identity, s.epoch = readStoreMeta(db)
	return s, nil
}

// openRunStore attaches to one run's state database without writing a byte
// of the run's records. A plain read-only attach to a WAL database
// materializes the shared-memory sidecars beside it, so a run no live
// controller owns opens immutable instead: its database is fully
// checkpointed and the read creates nothing. A live controller, or a
// write-ahead log a crashed controller left behind, needs the through-WAL
// read — the sidecars then exist or belong to that run's own recovery
// anyway.
func openRunStore(statePath string, owning bool) (*Store, error) {
	if owning {
		return OpenStoreReadonly(statePath)
	}
	if _, err := os.Stat(statePath + "-wal"); err == nil {
		return OpenStoreReadonly(statePath)
	}
	return OpenStoreImmutable(statePath)
}

// openObservedStore attaches a one-shot observer to the current project's
// saved run: the status, inspect, events, tail, doctor, preflight, and
// recovery-preview reads. It applies the same decision run discovery makes,
// so observing a cleanly stopped run creates no state.db-wal or state.db-shm
// beside the database (T105). Readers that hold their connection across the
// controllers that may start later — a followed event stream, the dashboard —
// keep OpenStoreReadonly instead: immutable ignores the log a later
// controller appends and cannot survive its checkpoints.
func openObservedStore(dir string) (*Store, error) {
	statePath := filepath.Join(dir, "state.db")
	owning := false
	if info, err := readController(dir); err == nil {
		owning = controllerAlive(info)
	}
	return openRunStore(statePath, owning)
}

// RecentEvents pages the journal by identifier with a bounded tail. after < 0
// returns the newest bounded tail; otherwise it returns events after the
// exclusive identifier. A position beyond the retained store means the store
// was reset or replaced: it returns an actionable cursor error instead of a
// silently empty page.
func (s *Store) RecentEvents(after int64, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 40
	}
	if after < 0 {
		rows, err := s.db.Query("SELECT id,time,kind,message,data FROM (SELECT id,time,kind,message,data FROM events ORDER BY id DESC LIMIT ?) ORDER BY id", limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanEvents(rows)
	}
	if err := s.ValidateCursor(EventCursor{EventID: after}); err != nil {
		return nil, err
	}
	rows, err := s.db.Query("SELECT id,time,kind,message,data FROM events WHERE id>? ORDER BY id LIMIT ?", after, limit)
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
		var data string
		if err := rows.Scan(&e.ID, &e.Time, &e.Kind, &e.Message, &data); err != nil {
			return nil, err
		}
		if data != "null" && json.Valid([]byte(data)) {
			e.Data = json.RawMessage(data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
