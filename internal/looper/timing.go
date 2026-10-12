package looper

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type timingContextKey struct{}

func withTiming(ctx context.Context, store *Store) context.Context {
	return context.WithValue(ctx, timingContextKey{}, store)
}

// beginSession runs under the repository lock. An earlier controller cannot
// still own the project. Its missing end stays unknown after a crash.
func (s *Store) beginSession(info *ControllerInfo) error {
	if err := s.loadPreviousSession(); err != nil {
		return err
	}
	if err := s.importProcessTimes(); err != nil {
		return err
	}
	identity, _ := runIdentityCommand(info.PID)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE sessions SET outcome='interrupted' WHERE ended IS NULL AND outcome='running'"); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE processes SET outcome='interrupted' WHERE ended IS NULL AND outcome='running'"); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO sessions(id,pid,identity,started,outcome) VALUES(?,?,?,?,'running')", info.Session, info.PID, identity, info.Started); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.session = info.Session
	s.shutdownAttempted = false
	return nil
}

func (s *Store) attemptForArtifact(artifact string) string {
	rel, err := filepath.Rel(s.dir, artifact)
	if err != nil {
		return ""
	}
	first := strings.Split(filepath.ToSlash(rel), "/")[0]
	if strings.HasPrefix(first, "task-") {
		return first
	}
	return ""
}

func (s *Store) beginProcess(artifact string, p ProcessObservation) (string, error) {
	id := identifier("process")
	_, err := s.db.Exec(`INSERT INTO processes(id,session_id,attempt_id,kind,artifact,pid,identity,started,outcome,source)
 VALUES(?,?,?,?,?,?,?,?,'running','recorded')`, id, s.session, s.attemptForArtifact(artifact), strings.TrimSuffix(filepath.Base(artifact), filepath.Ext(artifact)), artifact, p.PID, p.Identity, p.Started)
	return id, err
}

func (s *Store) endProcess(id, ended, outcome string) error {
	_, err := s.db.Exec("UPDATE processes SET ended=?,outcome=? WHERE id=? AND ended IS NULL", ended, outcome, id)
	return err
}

// Import only Looper-owned process metadata, never agent transcripts. The
// artifact path and start time identify retries separately and prevent double
// counting when a recorded process also has a metadata file.
func (s *Store) importProcessTimes() error {
	// Task records include setup, checks, and report recovery.
	for _, pattern := range []string{
		"task-*/*.process.json", "task-*/*/*.process.json", "task-*/*/*/*.process.json",
		"plan-*/*.process.json", "review-*/*.process.json", "bootstrap-*/*.process.json",
	} {
		paths, err := filepath.Glob(filepath.Join(s.dir, pattern))
		if err != nil {
			return err
		}
		for _, path := range paths {
			st, err := os.Lstat(path)
			if err != nil || !st.Mode().IsRegular() {
				continue
			}
			var p ProcessObservation
			if readJSON(path, &p) != nil {
				continue
			}
			start, err := time.Parse(time.RFC3339Nano, p.Started)
			if err != nil {
				continue
			}
			var ended any
			outcome := "unknown"
			if p.Ended != "" {
				end, err := time.Parse(time.RFC3339Nano, p.Ended)
				if err != nil || end.Before(start) {
					continue
				}
				ended = p.Ended
				outcome = "unknown_exit"
			}
			artifact := strings.TrimSuffix(path, ".process.json")
			id := "import-" + digest([]byte(artifact+"\x00"+p.Started))
			_, err = s.db.Exec(`INSERT OR IGNORE INTO processes(id,session_id,attempt_id,kind,artifact,pid,identity,started,ended,outcome,source)
 VALUES(?, '', ?, ?, ?, ?, ?, ?, ?, ?, 'imported')`,
				id, s.attemptForArtifact(artifact), strings.TrimSuffix(filepath.Base(artifact), filepath.Ext(artifact)), artifact, p.PID, p.Identity, p.Started, ended, outcome)
			if err != nil {
				return err
			}
			if ended != nil {
				// Recover a crash between writing the sidecar and committing
				// the database end. The sidecar does not establish exit status.
				if _, err = s.db.Exec("UPDATE processes SET ended=?,outcome='unknown_exit' WHERE artifact=? AND started=? AND ended IS NULL", ended, artifact, p.Started); err != nil {
					return err
				}
			}
			if err = s.importUsage(artifact, p); err != nil {
				return err
			}
		}
	}
	return nil
}

type TimingReport struct {
	Tasks             []TaskTimingReport `json:"tasks"`
	Version           int                `json:"schema_version"`
	Available         bool               `json:"available"`
	RecordedSince     string             `json:"recorded_since,omitempty"`
	FirstRecorded     string             `json:"first_recorded_at,omitempty"`
	LastRecorded      string             `json:"last_recorded_at,omitempty"`
	HistoryComplete   bool               `json:"history_complete"`
	ElapsedSeconds    *float64           `json:"elapsed_seconds"`
	ActiveSeconds     *float64           `json:"active_seconds"`
	WorkerSeconds     *float64           `json:"worker_seconds"`
	ControllerSeconds *float64           `json:"controller_seconds"`
	UnknownSessions   int                `json:"unknown_sessions"`
	UnknownProcesses  int                `json:"unknown_processes"`
	RunningProcesses  int                `json:"running_processes"`
	ImportedProcesses int                `json:"imported_processes"`
}

type timeInterval struct{ start, end time.Time }

func intervalSeconds(intervals []timeInterval, union bool) float64 {
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	current := intervals[0]
	total := 0.0
	for _, next := range intervals[1:] {
		if union && !next.start.After(current.end) {
			if next.end.After(current.end) {
				current.end = next.end
			}
		} else {
			total += current.end.Sub(current.start).Seconds()
			current = next
		}
	}
	return total + current.end.Sub(current.start).Seconds()
}

// Timing reads only the database. Open intervals count through now only while
// their process identity still matches; a dead process has an unknown end.
func (s *Store) Timing(now time.Time) (TimingReport, error) {
	return s.timing(now, func(pid int, identity string) bool {
		current, err := runIdentityCommand(pid)
		return err == nil && identity != "" && current == identity
	})
}

func (s *Store) timing(now time.Time, alive func(int, string) bool) (TimingReport, error) {
	report := TimingReport{Version: 1, Tasks: []TaskTimingReport{}}
	var tables int
	if err := s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='timing_metadata'").Scan(&tables); err != nil {
		return report, err
	}
	if tables == 0 {
		return report, nil
	}
	if err := s.db.QueryRow("SELECT recorded_since FROM timing_metadata WHERE id=1").Scan(&report.RecordedSince); err != nil {
		return report, err
	}
	report.Available, report.HistoryComplete = true, true
	state, err := s.Load()
	if err != nil {
		return report, err
	}
	taskTimes := newTaskTimings(state, s.dir)
	var first, last time.Time
	extend := func(t time.Time) {
		if first.IsZero() || t.Before(first) {
			first = t
		}
		if last.IsZero() || t.After(last) {
			last = t
		}
	}
	var eventFirst, eventLast sql.NullString
	if err := s.db.QueryRow("SELECT min(time),max(time) FROM events").Scan(&eventFirst, &eventLast); err != nil {
		return report, err
	}
	since, err := time.Parse(time.RFC3339Nano, report.RecordedSince)
	if err != nil {
		return report, err
	}
	for _, value := range []sql.NullString{eventFirst, eventLast} {
		if value.Valid {
			t, err := time.Parse(time.RFC3339Nano, value.String)
			if err != nil {
				return report, err
			}
			if t.After(now) {
				continue
			}
			extend(t)
			if t.Before(since) {
				report.HistoryComplete = false
			}
		}
	}
	var workers, controllers []timeInterval
	for _, table := range []string{"sessions", "processes"} {
		source := "''"
		taskColumns := "'', ''"
		if table == "processes" {
			source = "source"
			taskColumns = "attempt_id,artifact"
		}
		rows, err := s.db.Query("SELECT pid,identity,started,ended,outcome," + source + "," + taskColumns + " FROM " + table)
		if err != nil {
			return report, err
		}
		for rows.Next() {
			var pid int
			var identity, started, outcome, origin string
			var attempt, artifact string
			var ended sql.NullString
			if err = rows.Scan(&pid, &identity, &started, &ended, &outcome, &origin, &attempt, &artifact); err != nil {
				rows.Close()
				return report, err
			}
			start, err := time.Parse(time.RFC3339Nano, started)
			if err != nil {
				rows.Close()
				return report, err
			}
			// A process can start while status is reading the database.
			// Keep every interval within this observation's timestamp.
			if start.After(now) {
				continue
			}
			extend(start)
			if start.Before(since) {
				report.HistoryComplete = false
			}
			if origin == "imported" {
				report.ImportedProcesses++
			}
			end := now
			known := ended.Valid
			if known {
				end, err = time.Parse(time.RFC3339Nano, ended.String)
				if err != nil {
					rows.Close()
					return report, err
				}
			} else if outcome == "running" && alive(pid, identity) {
				known = true
				if table == "processes" {
					report.RunningProcesses++
				}
			}
			if !known {
				report.HistoryComplete = false
				if table == "processes" {
					taskTimes.observe(attempt, artifact, start, time.Time{}, false, false, start.Before(since))
					report.UnknownProcesses++
				} else {
					report.UnknownSessions++
				}
				continue
			}
			if end.After(now) {
				end = now
			}
			if end.Before(start) {
				rows.Close()
				return report, fmt.Errorf("%s interval ends before it starts", table)
			}
			extend(end)
			interval := timeInterval{start, end}
			if table == "processes" {
				taskTimes.observe(attempt, artifact, start, end, true, !ended.Valid, start.Before(since))
				workers = append(workers, interval)
			} else {
				controllers = append(controllers, interval)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return report, err
		}
	}
	elapsed := 0.0
	if !first.IsZero() {
		report.FirstRecorded = first.UTC().Format(time.RFC3339Nano)
		report.LastRecorded = last.UTC().Format(time.RFC3339Nano)
		elapsed = last.Sub(first).Seconds()
	}
	active, worker, controller := intervalSeconds(workers, true), intervalSeconds(workers, false), intervalSeconds(controllers, true)
	report.ElapsedSeconds, report.ActiveSeconds, report.WorkerSeconds, report.ControllerSeconds = &elapsed, &active, &worker, &controller
	report.Tasks = taskTimes.reports()
	return report, nil
}

func printTiming(out io.Writer, timing TimingReport) {
	if !timing.Available {
		fmt.Fprintln(out, "Timing: unavailable for this saved run")
		return
	}
	duration := func(seconds *float64) time.Duration {
		return (time.Duration(*seconds * float64(time.Second))).Round(time.Second)
	}
	fmt.Fprintf(out, "Timing: %s elapsed | %s active | %s combined workers | %s controller\n",
		duration(timing.ElapsedSeconds), duration(timing.ActiveSeconds), duration(timing.WorkerSeconds), duration(timing.ControllerSeconds))
	if !timing.HistoryComplete {
		fmt.Fprintf(out, "Timing coverage: partial history | %d unknown controller ends | %d unknown process ends\n", timing.UnknownSessions, timing.UnknownProcesses)
	}
}
