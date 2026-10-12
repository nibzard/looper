package looper

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

type TaskTimingReport struct {
	TaskID           string   `json:"task_id"`
	Available        bool     `json:"available"`
	HistoryComplete  bool     `json:"history_complete"`
	Attempts         int      `json:"attempts"`
	RecordedAttempts int      `json:"recorded_attempts"`
	WorkerSeconds    *float64 `json:"worker_seconds"`
	CheckSeconds     *float64 `json:"check_seconds"`
	OtherSeconds     *float64 `json:"other_seconds"`
	ActiveSeconds    *float64 `json:"active_seconds"`
	ElapsedSeconds   *float64 `json:"elapsed_seconds"`
	UnknownProcesses int      `json:"unknown_processes"`
	RunningProcesses int      `json:"running_processes"`
}

type taskTimes struct {
	report                      TaskTimingReport
	seen                        map[string]bool
	workers, checks, other, all []timeInterval
	first, last                 time.Time
}

type taskTimingCollection struct {
	ordered   []*taskTimes
	byAttempt map[string]*taskTimes
	dir       string
}

func newTaskTimings(state *State, dir string) taskTimingCollection {
	c := taskTimingCollection{dir: dir, byAttempt: map[string]*taskTimes{}}
	if state == nil {
		return c
	}
	byTask := map[string]*taskTimes{}
	for _, task := range state.Backlog.Tasks {
		if task.Marker() {
			continue
		}
		times := &taskTimes{report: TaskTimingReport{TaskID: task.ID, HistoryComplete: true}, seen: map[string]bool{}}
		c.ordered = append(c.ordered, times)
		byTask[task.ID] = times
	}
	for _, attempt := range state.Attempts {
		if times := byTask[attempt.Task.ID]; times != nil {
			c.byAttempt[attempt.ID] = times
			times.report.Attempts++
		}
	}
	return c
}

func (c taskTimingCollection) observe(attempt, artifact string, start, end time.Time, known, running, legacy bool) {
	times := c.byAttempt[attempt]
	if times == nil {
		return
	}
	times.report.Available = true
	times.seen[attempt] = true
	if times.first.IsZero() || start.Before(times.first) {
		times.first = start
	}
	if times.last.IsZero() || start.After(times.last) {
		times.last = start
	}
	if legacy {
		times.report.HistoryComplete = false
	}
	if !known {
		times.report.UnknownProcesses++
		times.report.HistoryComplete = false
		return
	}
	if running {
		times.report.RunningProcesses++
	}
	if end.After(times.last) {
		times.last = end
	}
	interval := timeInterval{start, end}
	times.all = append(times.all, interval)
	rel, err := filepath.Rel(filepath.Join(c.dir, attempt), artifact)
	if err != nil {
		rel = ""
	}
	switch {
	case strings.HasPrefix(rel, "work-"):
		times.workers = append(times.workers, interval)
	case strings.HasPrefix(rel, "checks"+string(filepath.Separator)):
		times.checks = append(times.checks, interval)
	default:
		times.other = append(times.other, interval)
	}
}

func (c taskTimingCollection) reports() []TaskTimingReport {
	reports := make([]TaskTimingReport, 0, len(c.ordered))
	for _, times := range c.ordered {
		r := times.report
		r.RecordedAttempts = len(times.seen)
		if !r.Available || r.RecordedAttempts < r.Attempts {
			r.HistoryComplete = false
		}
		if r.Available {
			worker, check, other := intervalSeconds(times.workers, false), intervalSeconds(times.checks, false), intervalSeconds(times.other, false)
			active, elapsed := intervalSeconds(times.all, true), times.last.Sub(times.first).Seconds()
			r.WorkerSeconds, r.CheckSeconds, r.OtherSeconds, r.ActiveSeconds, r.ElapsedSeconds = &worker, &check, &other, &active, &elapsed
		}
		reports = append(reports, r)
	}
	return reports
}

func (r TimingReport) task(id string) TaskTimingReport {
	for _, task := range r.Tasks {
		if task.TaskID == id {
			return task
		}
	}
	return TaskTimingReport{TaskID: id}
}

func printTaskTiming(out io.Writer, timing TaskTimingReport) {
	if !timing.Available {
		fmt.Fprintln(out, "Task timing: unavailable; no recorded task processes")
		return
	}
	duration := func(value *float64) time.Duration {
		return (time.Duration(*value * float64(time.Second))).Round(time.Second)
	}
	fmt.Fprintf(out, "Task timing: %s workers | %s checks | %s other | %s active | %s elapsed span\n", duration(timing.WorkerSeconds), duration(timing.CheckSeconds), duration(timing.OtherSeconds), duration(timing.ActiveSeconds), duration(timing.ElapsedSeconds))
	if !timing.HistoryComplete {
		fmt.Fprintf(out, "Task timing coverage: partial | %d unknown process ends | %d/%d attempts recorded\n", timing.UnknownProcesses, timing.RecordedAttempts, timing.Attempts)
	}
}
