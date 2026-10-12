package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

const observationVersion = 1

type WorkerObservation struct {
	AttemptID   string              `json:"attempt_id"`
	TaskID      string              `json:"task_id"`
	Phase       string              `json:"phase"`
	Log         string              `json:"log,omitempty"`
	LastOutput  string              `json:"last_output_at,omitempty"`
	OutputAge   *int64              `json:"output_age_seconds,omitempty"`
	OutputBytes int64               `json:"output_bytes"`
	Process     *ProcessObservation `json:"process,omitempty"`
	Progress    string              `json:"progress"`
}

type ProcessObservation struct {
	PID      int    `json:"pid"`
	Identity string `json:"identity,omitempty"`
	Started  string `json:"started"`
	Ended    string `json:"ended,omitempty"`
	Running  bool   `json:"running"`
}

type Observation struct {
	Version               int                 `json:"schema_version"`
	ObservedAt            string              `json:"observed_at"`
	ControllerOwned       bool                `json:"controller_owned"`
	Controller            *ControllerInfo     `json:"controller,omitempty"`
	View                  *ui.ViewSnapshot    `json:"view,omitempty"`
	ViewMatchesController bool                `json:"view_matches_controller"`
	Workers               []WorkerObservation `json:"workers"`
	EventCursor           int64               `json:"event_cursor"`
}

func activeAttempt(a *Attempt) bool {
	return a.Phase != "done" && a.Phase != "blocked"
}

// Restrict discovery to Looper-owned artifacts. Never inspect agent home files.
func attemptLogs(a *Attempt) []string {
	var paths []string
	if a.Directory == "" {
		return paths
	}
	for _, pattern := range []string{"work-*/events-*.jsonl", "work-*/report-recovery/events-*.jsonl", "work-*/setup.log", "checks/*/setup.log", "checks/*/verify.log"} {
		matches, _ := filepath.Glob(filepath.Join(a.Directory, pattern))
		for _, p := range matches {
			if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
				paths = append(paths, p)
			}
		}
	}
	sort.Slice(paths, func(i, j int) bool {
		a, ea := os.Stat(paths[i])
		b, eb := os.Stat(paths[j])
		if ea != nil || eb != nil {
			return paths[i] < paths[j]
		}
		if a.ModTime().Equal(b.ModTime()) {
			return paths[i] < paths[j]
		}
		return a.ModTime().Before(b.ModTime())
	})
	return paths
}

func observeWorker(a *Attempt) WorkerObservation {
	w := WorkerObservation{AttemptID: a.ID, TaskID: a.Task.ID, Phase: a.Phase, Progress: "unknown"}
	paths := attemptLogs(a)
	// A repair's old check logs must not masquerade as its worker output.
	// A queued check similarly must not show an implementation process as live.
	if a.Phase == "running" || a.Phase == "verifying" {
		filtered := paths[:0]
		for _, path := range paths {
			rel, err := filepath.Rel(a.Directory, path)
			isCheck := strings.HasPrefix(rel, "checks"+string(filepath.Separator))
			if err == nil && ((a.Phase == "verifying" && isCheck) || (a.Phase == "running" && !isCheck)) {
				filtered = append(filtered, path)
			}
		}
		paths = filtered
	}
	if len(paths) == 0 {
		return w
	}
	w.Log = paths[len(paths)-1]
	if st, err := os.Stat(w.Log); err == nil {
		w.OutputBytes = st.Size()
		if st.Size() > 0 {
			w.LastOutput = st.ModTime().UTC().Format(time.RFC3339Nano)
			age := int64(max(0, time.Since(st.ModTime()).Seconds()))
			w.OutputAge = &age
		}
	}
	var p ProcessObservation
	if readJSON(w.Log+".process.json", &p) == nil {
		identity, err := runIdentityCommand(p.PID)
		p.Running = p.Ended == "" && err == nil && identity == p.Identity && p.Identity != ""
		w.Process = &p
	}
	return w
}

func observe(c Config, s *State) Observation {
	_, dir := projectPaths(c)
	o := Observation{Version: observationVersion, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ControllerOwned: repositoryOwned(c), View: LoadView(dir), Workers: []WorkerObservation{}}
	o.Controller, _ = readController(dir)
	o.ViewMatchesController = o.ControllerOwned && o.Controller != nil && o.View != nil && o.View.Session == o.Controller.Session
	if o.View != nil {
		o.EventCursor = o.View.EventCursor
	}
	if s != nil {
		for _, a := range s.Attempts {
			if activeAttempt(a) {
				o.Workers = append(o.Workers, observeWorker(a))
			}
		}
	}
	return o
}

// Preserve existing status fields; the observation is an additive contract.
func statusReport(c Config, s *State, timing TimingReport, statistics ui.StatisticsView) any {
	return struct {
		*State
		Observation Observation       `json:"observation"`
		Timing      TimingReport      `json:"timing"`
		Statistics  ui.StatisticsView `json:"statistics"`
	}{s, observe(c, s), timing, statistics}
}

func inspectTask(c Config, store *Store, out io.Writer) error {
	if c.InspectTask == "" {
		return fmt.Errorf("usage: looper inspect <task-id> [--json]")
	}
	s, err := store.Load()
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no saved run")
	}
	for _, t := range s.Backlog.Tasks {
		if t.ID != c.InspectTask {
			continue
		}
		attempts := []*Attempt{}
		workers := []WorkerObservation{}
		logs := []string{}
		for _, a := range s.Attempts {
			if a.Task.ID == t.ID {
				attempts = append(attempts, a)
				workers = append(workers, observeWorker(a))
				logs = append(logs, attemptLogs(a)...)
			}
		}
		checks, err := taskChecks(store, attempts)
		if err != nil {
			return err
		}
		timing, err := store.Timing(time.Now().UTC())
		if err != nil {
			return err
		}
		taskTiming := timing.task(t.ID)
		report := struct {
			Version  int                 `json:"schema_version"`
			Task     Task                `json:"task"`
			Attempts []*Attempt          `json:"attempts"`
			Workers  []WorkerObservation `json:"workers"`
			Checks   []CheckObservation  `json:"checks"`
			Logs     []string            `json:"logs"`
			Timing   TaskTimingReport    `json:"timing"`
		}{observationVersion, t, attempts, workers, checks, logs, taskTiming}
		if c.JSON {
			return json.NewEncoder(out).Encode(report)
		}
		fmt.Fprintf(out, "%s  %s  %s\n", t.ID, t.Status, sanitize(t.Title))
		printTaskTiming(out, taskTiming)
		for _, b := range t.Blockers {
			fmt.Fprintln(out, "Blocked:", sanitize(b))
		}
		for _, w := range workers {
			fmt.Fprintf(out, "%s  %s  %s\n", w.AttemptID, w.Phase, w.Log)
		}
		return nil
	}
	return fmt.Errorf("unknown task %q", c.InspectTask)
}

type HealthFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	TaskID   string `json:"task_id,omitempty"`
	Message  string `json:"message"`
	Action   string `json:"suggested_action"`
}

func diagnose(o Observation, s *State) []HealthFinding {
	f := []HealthFinding{}
	if o.ControllerOwned && !o.ViewMatchesController {
		f = append(f, HealthFinding{Code: "view_unavailable", Severity: "warning", Message: "The repository is owned but no matching view is available.", Action: "Check controller metadata and logs; startup may still be in progress."})
	}
	if !o.ControllerOwned && len(o.Workers) > 0 {
		f = append(f, HealthFinding{Code: "retained_work", Severity: "info", Message: "Attempts are retained without an active controller.", Action: "Run looper to resume retained work."})
	}
	for _, w := range o.Workers {
		if o.ControllerOwned && w.Process != nil && w.Process.Ended == "" && !w.Process.Running {
			f = append(f, HealthFinding{Code: "worker_process_missing", Severity: "warning", TaskID: w.TaskID, Message: "The recorded worker process is absent.", Action: "Read task events; the controller may be processing its exit."})
		}
		if o.ControllerOwned && w.OutputAge != nil && *w.OutputAge > 300 {
			f = append(f, HealthFinding{Code: "output_quiet", Severity: "info", TaskID: w.TaskID, Message: "No worker log output for over five minutes. Progress is unknown.", Action: "Inspect the task and check logs. Quiet output alone does not justify a restart."})
		}
	}
	if s != nil {
		for _, t := range s.Backlog.Tasks {
			if t.Status == "blocked" {
				f = append(f, HealthFinding{Code: "task_blocked", Severity: "warning", TaskID: t.ID, Message: "The task reports an unmet prerequisite.", Action: "Read blockers with looper inspect " + t.ID + " --json."})
			}
		}
		for _, a := range s.Attempts {
			if activeAttempt(a) && a.Repeats > 0 {
				f = append(f, HealthFinding{Code: "repeated_failure", Severity: "warning", TaskID: a.Task.ID, Message: "An attempt repeated a failure.", Action: "Inspect its feedback and checks before another repair."})
			}
		}
	}
	return f
}

func doctorJSON(c Config, out io.Writer) int {
	_, dir := projectPaths(c)
	findings := []HealthFinding{}
	var s *State
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if os.IsNotExist(err) {
		findings = append(findings, HealthFinding{Code: "no_saved_run", Severity: "info", Message: "No saved run exists.", Action: "Start looper in this project."})
	} else if err != nil {
		findings = append(findings, HealthFinding{Code: "state_unreadable", Severity: "error", Message: err.Error(), Action: "Check the state database path and permissions."})
	} else {
		defer store.Close()
		s, err = store.Load()
		if err != nil {
			findings = append(findings, HealthFinding{Code: "state_unreadable", Severity: "error", Message: err.Error(), Action: "Retain the database and inspect the failure before restarting."})
		}
	}
	o := observe(c, s)
	findings = append(findings, diagnose(o, s)...)
	code := 0
	for _, f := range findings {
		if f.Severity == "error" {
			code = 1
		}
	}
	result := struct {
		Version           int             `json:"schema_version"`
		Observation       Observation     `json:"observation"`
		Findings          []HealthFinding `json:"findings"`
		AutomaticRecovery bool            `json:"automatic_recovery"`
	}{observationVersion, o, findings, false}
	if json.NewEncoder(out).Encode(result) != nil {
		return 1
	}
	return code
}

// Summaries omit arguments, tool output, and reasoning. Raw evidence stays local.
func workerActivity(line []byte) []string {
	var v struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Message struct {
			Content []struct {
				Type, Name string
				IsError    bool `json:"is_error"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exit_code"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	var actions []string
	for _, b := range v.Message.Content {
		if b.Type == "tool_use" {
			actions = append(actions, "Tool started: "+sanitize(b.Name))
		}
		if b.Type == "tool_result" && b.IsError {
			actions = append(actions, "Tool reported an error")
		}
	}
	if v.Item.Type == "command_execution" {
		if v.Type == "item.completed" && v.Item.ExitCode != nil {
			actions = append(actions, fmt.Sprintf("Command completed (exit %d)", *v.Item.ExitCode))
		} else {
			actions = append(actions, "Command started")
		}
	}
	if v.Type == "result" || v.Type == "turn.completed" {
		if v.IsError {
			actions = append(actions, "Agent reported an error")
		} else {
			actions = append(actions, "Agent returned a result")
		}
	}
	return actions
}

// Only the owner writes the journal. Work remains independent of observers.
func (e *Engine) pollWorkerActivity() {
	if time.Since(e.lastObservation) < time.Second {
		return
	}
	e.lastObservation = time.Now()
	if e.workerReaders == nil {
		e.workerReaders = map[string]*plannerActivityReader{}
	}
	live := map[string]bool{}
	for _, a := range e.State.Attempts {
		if !activeAttempt(a) {
			continue
		}
		for _, p := range attemptLogs(a) {
			if filepath.Ext(p) != ".jsonl" {
				continue
			}
			if e.controller != nil {
				start, _ := time.Parse(time.RFC3339, e.controller.Started)
				if st, err := os.Stat(p); err != nil || st.ModTime().Before(start) {
					continue
				}
			}
			live[p] = true
			r := e.workerReaders[p]
			if r == nil {
				r = &plannerActivityReader{decode: workerActivity}
				e.workerReaders[p] = r
			}
			r.poll(p, func(action string) {
				session := ""
				if e.controller != nil {
					session = e.controller.Session
				}
				id, err := e.Store.Event("worker_activity", a.Task.ID+" "+action, map[string]string{
					"session": session, "task_id": a.Task.ID, "attempt_id": a.ID, "phase": a.Phase, "log": p, "action": action,
				})
				if err != nil {
					e.err = err
					return
				}
				e.eventCursor = id
			})
		}
	}
	for p := range e.workerReaders {
		if !live[p] {
			delete(e.workerReaders, p)
		}
	}
	e.viewDirty = true
}

type CheckObservation struct {
	Time      string `json:"time"`
	AttemptID string `json:"attempt_id"`
	Revision  string `json:"revision"`
	Kind      string `json:"kind"`
	Result    string `json:"result"`
	Artifact  string `json:"artifact"`
}

func taskChecks(store *Store, attempts []*Attempt) ([]CheckObservation, error) {
	out := []CheckObservation{}
	for _, a := range attempts {
		rows, err := store.db.Query("SELECT time,attempt_id,revision,kind,result,artifact FROM checks WHERE attempt_id=? ORDER BY id", a.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var c CheckObservation
			if err = rows.Scan(&c.Time, &c.AttemptID, &c.Revision, &c.Kind, &c.Result, &c.Artifact); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, c)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
