package looper

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func timingStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err = s.db.Exec("UPDATE timing_metadata SET recorded_since='2026-01-01T00:00:00Z'"); err != nil {
		t.Fatal(err)
	}
	return s
}

func timingAt(seconds int) string {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(seconds) * time.Second).Format(time.RFC3339Nano)
}

func addTiming(t *testing.T, s *Store, table, id string, start, end int, outcome string) {
	t.Helper()
	var ended any
	if end >= 0 {
		ended = timingAt(end)
	}
	var err error
	if table == "sessions" {
		_, err = s.db.Exec("INSERT INTO sessions VALUES(?,123,'identity',?,?,?)", id, timingAt(start), ended, outcome)
	} else {
		_, err = s.db.Exec("INSERT INTO processes VALUES(?, '', '', 'verify', ?,123,'identity',?,?,?,'recorded')", id, id, timingAt(start), ended, outcome)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestTimingSeparatesElapsedActiveAndWorkerTime(t *testing.T) {
	s := timingStore(t)
	addTiming(t, s, "sessions", "one", 0, 50, "stopped")
	addTiming(t, s, "sessions", "two", 60, 100, "stopped")
	addTiming(t, s, "processes", "a", 10, 30, "success")
	addTiming(t, s, "processes", "b", 20, 40, "success")
	addTiming(t, s, "processes", "c", 60, 70, "success")
	report, err := s.timing(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), func(int, string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if *report.ElapsedSeconds != 100 || *report.ActiveSeconds != 40 || *report.WorkerSeconds != 50 || *report.ControllerSeconds != 90 {
		t.Fatalf("incorrect time totals: %+v", report)
	}
	if !report.HistoryComplete {
		t.Fatal("known intervals should have complete history")
	}
}

func TestTimingDoesNotCountCrashDowntime(t *testing.T) {
	s := timingStore(t)
	addTiming(t, s, "sessions", "dead", 0, -1, "running")
	addTiming(t, s, "processes", "dead", 10, -1, "running")
	addTiming(t, s, "processes", "live", 20, -1, "running")
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	// Mark the dead worker's identity differently from the live worker.
	if _, err := s.db.Exec("UPDATE processes SET identity='dead' WHERE id='dead'"); err != nil {
		t.Fatal(err)
	}
	report, err := s.timing(now, func(_ int, identity string) bool { return identity != "dead" })
	if err != nil {
		t.Fatal(err)
	}
	if *report.WorkerSeconds != 80 || report.UnknownProcesses != 1 || report.RunningProcesses != 1 || report.HistoryComplete {
		t.Fatalf("unknown end must not count as work: %+v", report)
	}
	// PID reuse must also leave an unknown end, rather than extending to now.
	report, err = s.timing(now, func(int, string) bool { return false })
	if err != nil || *report.WorkerSeconds != 0 || report.UnknownProcesses != 2 || report.UnknownSessions != 1 {
		t.Fatalf("dead processes must not accrue time: %+v %v", report, err)
	}
}

func TestTimingImportsAreIdempotent(t *testing.T) {
	s := timingStore(t)
	path := filepath.Join(s.dir, "task-test", "checks", "revision", "verify.log")
	p := ProcessObservation{PID: 123, Identity: "old", Started: timingAt(10), Ended: timingAt(30)}
	if err := writeJSON(path+".process.json", p); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.importProcessTimes(); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := s.db.QueryRow("SELECT count(*) FROM processes").Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate import: %d %v", count, err)
	}
	// A native database record with a sidecar must not be counted twice.
	p.Started, p.Ended = timingAt(40), timingAt(50)
	id, err := s.beginProcess(path, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.endProcess(id, p.Ended, "success"); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(path+".process.json", p); err != nil {
		t.Fatal(err)
	}
	if err = s.importProcessTimes(); err != nil {
		t.Fatal(err)
	}
	report, err := s.timing(time.Now(), func(int, string) bool { return false })
	if err != nil || report.ImportedProcesses != 1 || *report.WorkerSeconds != 30 {
		t.Fatalf("duplicate native record: %+v %v", report, err)
	}
	// The sidecar can finish just before a crash prevents the database write.
	p.Started, p.Ended = timingAt(60), timingAt(70)
	if _, err = s.beginProcess(path, p); err != nil {
		t.Fatal(err)
	}
	if err = writeJSON(path+".process.json", p); err != nil {
		t.Fatal(err)
	}
	if err = s.importProcessTimes(); err != nil {
		t.Fatal(err)
	}
	report, err = s.timing(time.Now(), func(int, string) bool { return false })
	if err != nil || report.ImportedProcesses != 1 || *report.WorkerSeconds != 40 || report.UnknownProcesses != 0 {
		t.Fatalf("missing database end was not recovered: %+v %v", report, err)
	}
}

func TestTimingReadonlyLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE state(id INTEGER PRIMARY KEY, data TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report, err := s.Timing(time.Now())
	if err != nil || report.Available || report.WorkerSeconds != nil {
		t.Fatalf("legacy timing must be unavailable: %+v %v", report, err)
	}
	var count int
	if err = s.db.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='timing_metadata'").Scan(&count); err != nil || count != 0 {
		t.Fatal("observer migrated the database")
	}
}

func TestTimingSessionCloseAndCrashRecovery(t *testing.T) {
	s := timingStore(t)
	addTiming(t, s, "sessions", "crashed", 0, -1, "running")
	addTiming(t, s, "processes", "crashed", 0, -1, "running")
	info := &ControllerInfo{Session: "new", PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.beginSession(info); err != nil {
		t.Fatal(err)
	}
	var outcome string
	var ended sql.NullString
	if err := s.db.QueryRow("SELECT outcome,ended FROM sessions WHERE id='crashed'").Scan(&outcome, &ended); err != nil || outcome != "interrupted" || ended.Valid {
		t.Fatal("restart invented an old end time")
	}
	path := filepath.Join(s.dir, "state.db")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	read, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	if err = read.db.QueryRow("SELECT outcome,ended FROM sessions WHERE id='new'").Scan(&outcome, &ended); err != nil || outcome != "stopped" || !ended.Valid {
		t.Fatal("controller end was not recorded")
	}
}

func TestTimingRecordsProcessOutcomes(t *testing.T) {
	s := timingStore(t)
	for _, test := range []struct {
		command, outcome string
		cancel           bool
	}{
		{"exit 0", "success", false}, {"exit 7", "failed", false}, {"sleep 10", "cancelled", true},
	} {
		ctx := withTiming(context.Background(), s)
		cancel := func() {}
		if test.cancel {
			ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
		}
		path := filepath.Join(s.dir, "task-test", test.outcome, "verify.log")
		err := runProcess(ctx, t.TempDir(), path, []string{"bash", "-c", test.command}, nil, nil)
		cancel()
		if (test.outcome == "success") != (err == nil) {
			t.Fatalf("command result: %v", err)
		}
		var outcome, attempt string
		var ended sql.NullString
		if err = s.db.QueryRow("SELECT outcome,ended,attempt_id FROM processes WHERE artifact=?", path).Scan(&outcome, &ended, &attempt); err != nil || outcome != test.outcome || !ended.Valid || attempt != "task-test" {
			t.Fatalf("missing process result: %s %v %s %v", outcome, ended, attempt, err)
		}
	}
}

func TestTimingJSONNullsForLegacyHistory(t *testing.T) {
	b, err := json.Marshal(TimingReport{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	var report map[string]any
	if err = json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report["worker_seconds"] != nil || report["active_seconds"] != nil {
		t.Fatal("missing history reported zero work")
	}
}

func TestTimingObservationCutoff(t *testing.T) {
	s := timingStore(t)
	addTiming(t, s, "processes", "ended during query", 10, 110, "success")
	addTiming(t, s, "processes", "started during query", 101, -1, "running")
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	report, err := s.timing(now, func(int, string) bool { return true })
	if err != nil || *report.WorkerSeconds != 90 || report.RunningProcesses != 0 {
		t.Fatalf("concurrent writes exceeded the observation: %+v %v", report, err)
	}
}

func TestTimingMigrationPreservesLegacyRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE state(id INTEGER PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE events(id INTEGER PRIMARY KEY,time TEXT NOT NULL,kind TEXT NOT NULL,message TEXT NOT NULL,data TEXT NOT NULL);
INSERT INTO state VALUES(1,'{"version":2,"root":"original"}');
INSERT INTO events VALUES(1,'2025-01-01T00:00:00Z','start','old run','null');`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.Load()
	if err != nil || state.Root != "original" || state.Version != 2 {
		t.Fatal("timing migration changed saved state")
	}
	events, err := s.Events(0)
	if err != nil || len(events) != 1 || events[0].Message != "old run" {
		t.Fatal("timing migration changed events")
	}
	report, err := s.Timing(time.Now())
	if err != nil || !report.Available || report.HistoryComplete {
		t.Fatalf("migration must identify historical gaps: %+v %v", report, err)
	}
}
