package looper

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskTimingState(t *testing.T, s *Store) {
	t.Helper()
	state := State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "first", Status: "done", Priority: 1},
		{ID: "T2", Title: "second", Status: "todo", Priority: 1},
		{ID: "T3", Title: "legacy", Status: "done", Priority: 1},
	}}, Attempts: []*Attempt{
		{ID: "task-one", Task: Task{ID: "T1"}},
		{ID: "task-repair", Task: Task{ID: "T1"}},
		{ID: "task-two", Task: Task{ID: "T2"}},
		{ID: "task-legacy", Task: Task{ID: "T3"}},
	}}
	if err := s.Save(&state); err != nil {
		t.Fatal(err)
	}
}

func taskProcess(t *testing.T, s *Store, id, attempt, relative string, start, end int, outcome string) {
	t.Helper()
	addTiming(t, s, "processes", id, start, end, outcome)
	if _, err := s.db.Exec("UPDATE processes SET attempt_id=?,artifact=? WHERE id=?", attempt, filepath.Join(s.dir, attempt, relative), id); err != nil {
		t.Fatal(err)
	}
}

func TestTaskTimingSeparatesWorkersChecksAndRepairs(t *testing.T) {
	s := timingStore(t)
	taskTimingState(t, s)
	taskProcess(t, s, "worker", "task-one", "work-00-session/events-00.jsonl", 0, 20, "success")
	taskProcess(t, s, "verify", "task-one", "checks/revision/verify.log", 10, 30, "success")
	taskProcess(t, s, "assessment", "task-one", "checks/revision/assessment.log", 20, 25, "success")
	taskProcess(t, s, "repair", "task-repair", "work-01-session/events-00.jsonl", 50, 60, "success")
	taskProcess(t, s, "diagnosis", "task-one", "diagnose-00/events-00.jsonl", 30, 35, "success")
	taskProcess(t, s, "other task", "task-two", "work-00-session/events-00.jsonl", 5, 25, "success")
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	report, err := s.timing(now, func(int, string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	one := report.task("T1")
	if !one.Available || !one.HistoryComplete || one.Attempts != 2 || one.RecordedAttempts != 2 {
		t.Fatalf("coverage: %+v", one)
	}
	if *one.WorkerSeconds != 30 || *one.CheckSeconds != 25 || *one.OtherSeconds != 5 || *one.ActiveSeconds != 45 || *one.ElapsedSeconds != 60 {
		t.Fatalf("durations: %+v", one)
	}
	two := report.task("T2")
	if *two.WorkerSeconds != 20 || *two.CheckSeconds != 0 {
		t.Fatalf("cross-task contamination: %+v", two)
	}
	legacy := report.task("T3")
	if legacy.Available || legacy.WorkerSeconds != nil || legacy.CheckSeconds != nil || legacy.HistoryComplete {
		t.Fatalf("invented legacy time: %+v", legacy)
	}
}

func TestTaskTimingDoesNotCountCrashDowntimeOrFutureWork(t *testing.T) {
	s := timingStore(t)
	taskTimingState(t, s)
	taskProcess(t, s, "dead", "task-one", "work-00-session/events-00.jsonl", 0, -1, "interrupted")
	taskProcess(t, s, "live", "task-repair", "work-01-session/events-00.jsonl", 20, -1, "running")
	taskProcess(t, s, "ended after cutoff", "task-two", "checks/rev/verify.log", 90, 110, "success")
	taskProcess(t, s, "future", "task-two", "work-01-session/events-00.jsonl", 101, 120, "success")
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	report, err := s.timing(now, func(int, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	one := report.task("T1")
	if *one.WorkerSeconds != 80 || one.UnknownProcesses != 1 || one.RunningProcesses != 1 || one.HistoryComplete {
		t.Fatalf("unknown process counted: %+v", one)
	}
	two := report.task("T2")
	if *two.CheckSeconds != 10 || *two.WorkerSeconds != 0 {
		t.Fatalf("future timing exceeded cutoff: %+v", two)
	}
}

func TestTaskTimingMarksAttemptsWithoutRecords(t *testing.T) {
	s := timingStore(t)
	taskTimingState(t, s)
	taskProcess(t, s, "worker", "task-repair", "work-01-session/events-00.jsonl", 50, 60, "success")
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	report, err := s.timing(now, func(int, string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	one := report.task("T1")
	if one.HistoryComplete || one.RecordedAttempts != 1 || one.Attempts != 2 || *one.WorkerSeconds != 10 {
		t.Fatalf("missing attempt hidden: %+v", one)
	}
}

func TestInspectIncludesTaskTiming(t *testing.T) {
	s := timingStore(t)
	taskTimingState(t, s)
	taskProcess(t, s, "worker", "task-one", "work-00-session/events-00.jsonl", 0, 20, "success")
	taskProcess(t, s, "verify", "task-one", "checks/rev/verify.log", 20, 30, "success")
	var output bytes.Buffer
	if err := inspectTask(Config{InspectTask: "T1", JSON: true}, s, &output); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Timing TaskTimingReport `json:"timing"`
	}
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Timing.TaskID != "T1" || *report.Timing.WorkerSeconds != 20 || *report.Timing.CheckSeconds != 10 {
		t.Fatal("inspect lost task timing")
	}
	output.Reset()
	if err := inspectTask(Config{InspectTask: "T1"}, s, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "20s workers | 10s checks") {
		t.Fatal(output.String())
	}
}

func TestTaskTimingReadonlyPreservesProcessRoles(t *testing.T) {
	s := timingStore(t)
	taskTimingState(t, s)
	taskProcess(t, s, "worker", "task-one", "work-00-session/events-00.jsonl", 0, 20, "success")
	taskProcess(t, s, "verify", "task-one", "checks/rev/verify.log", 20, 30, "success")
	read, err := OpenStoreReadonly(filepath.Join(s.dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	now, _ := time.Parse(time.RFC3339Nano, timingAt(100))
	report, err := read.timing(now, func(int, string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	one := report.task("T1")
	if !one.Available || *one.WorkerSeconds != 20 || *one.CheckSeconds != 10 || *one.OtherSeconds != 0 {
		t.Fatalf("read-only process roles: %+v", one)
	}
}
