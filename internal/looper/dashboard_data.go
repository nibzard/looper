package looper

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// dashboardControllerAlive checks process existence for a bound observer session.
// Session metadata remains authoritative; this check does not require Linux procfs.
func dashboardControllerAlive(info *ControllerInfo) bool {
	if info == nil || info.PID <= 1 {
		return false
	}
	err := syscall.Kill(info.PID, 0)
	return err == nil || err == syscall.EPERM
}

// view enriches a published copy. Observers never read the owner's mutable state.
func (o *observer) view(dir, session string) *ui.ViewSnapshot {
	info, err := readController(dir)
	if err != nil || info.Session != session || !dashboardControllerAlive(info) {
		return nil
	}
	v := LoadView(dir)
	if v == nil || v.Session != session {
		return nil
	}
	for i := range v.Attempts {
		enrichAttempt(&v.Attempts[i])
	}
	for i := range v.Tasks {
		a := v.Tasks[i].Attempt
		if a == nil {
			continue
		}
		for _, active := range v.Attempts {
			if active.ID == a.ID {
				*a = active
				break
			}
		}
	}
	if time.Since(o.paceAt) >= 10*time.Second || o.paceSession != session {
		o.pace, err = o.integrationPace(v.Started, time.Now())
		o.paceAt, o.paceSession = time.Now(), session
		if err != nil {
			o.pace = nil
		}
		o.outcomes, _ = o.recentOutcomes(v.Started)
	}
	v.Pace = o.pace
	v.RecentOutcomes = o.outcomes
	return v
}

func (o *observer) recentOutcomes(start string) ([]ui.JournalEvent, error) {
	rows, err := o.store.db.Query(`SELECT id,time,kind,message FROM (
	 SELECT id,time,kind,message FROM events WHERE julianday(time)>=julianday(?)
	 AND kind IN ('result','repair','blocker','hold','requeue','refresh','finish','review-error','hook-error','diagnosis-error','import','import-error')
	 ORDER BY id DESC LIMIT 5) ORDER BY id`, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ui.JournalEvent{}
	for rows.Next() {
		var e ui.JournalEvent
		if err = rows.Scan(&e.ID, &e.Time, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func enrichAttempt(a *ui.AttemptView) {
	w := observeWorker(&Attempt{ID: a.ID, Directory: a.LogDir, Phase: a.Phase})
	a.LastOutput = w.LastOutput
	if w.Process != nil {
		a.ProcessStarted = w.Process.Started
		switch {
		case w.Process.Running:
			a.ProcessState = "running"
		case w.Process.Ended != "":
			a.ProcessState = "exited"
		default:
			a.ProcessState = "absent"
		}
	}
}

// Counts come from the journal, not the viewer's bounded tail. Only published
// task results count; agent results and unsuccessful checks do not count.
func (o *observer) integrationPace(start string, now time.Time) (*ui.PaceView, error) {
	var p ui.PaceView
	var last *string
	err := o.store.db.QueryRow(`SELECT
 COALESCE(SUM(CASE WHEN julianday(time)>=julianday(?) THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN julianday(time)>=julianday(?) THEN 1 ELSE 0 END),0),
 COALESCE(SUM(CASE WHEN julianday(time)>=julianday(?) THEN 1 ELSE 0 END),0),
	 MAX(time) FROM events WHERE kind='result' AND substr(message,instr(message,'  '),9)='  done | '`,
		start, now.Add(-time.Hour).UTC().Format(time.RFC3339Nano), now.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano)).Scan(&p.Session, &p.Hour, &p.Day, &last)
	if last != nil {
		p.LastIntegration = *last
	}
	return &p, err
}

func dashboardAttempts(v *ui.ViewSnapshot) []ui.AttemptView {
	out := append([]ui.AttemptView(nil), v.Attempts...)
	seen := map[string]bool{}
	for _, a := range out {
		seen[a.ID] = true
	}
	for _, t := range v.Tasks {
		if t.Attempt != nil && !seen[t.Attempt.ID] {
			out = append(out, *t.Attempt)
			seen[t.Attempt.ID] = true
		}
	}
	return out
}

func (o *observer) inspect(id string) (ui.TaskDetail, error) {
	var d ui.TaskDetail
	s, err := o.store.Load()
	if err != nil {
		return d, err
	}
	if s == nil {
		return d, fmt.Errorf("No saved task state yet.")
	}
	t := s.Backlog.Task(id)
	if t == nil {
		return d, fmt.Errorf("Task %s is no longer in the backlog.", id)
	}
	d.Task = taskView(*t, s.Backlog)
	d.Files = append([]string(nil), t.Files...)
	var attempts []*Attempt
	for _, a := range s.Attempts {
		if a.Task.ID != id {
			continue
		}
		attempts = append(attempts, a)
		av := attemptView(a)
		enrichAttempt(&av)
		d.Attempts = append(d.Attempts, av)
		d.Task.Attempt = &av
		d.Feedback = a.Feedback
		if a.Summary != nil {
			d.Summary = a.Summary.Summary
		}
	}
	checks, err := taskChecks(o.store, attempts)
	if err != nil {
		return d, err
	}
	for _, c := range checks {
		d.Checks = append(d.Checks, ui.CheckView{Time: c.Time, Kind: c.Kind, Result: c.Result, Revision: c.Revision, Artifact: c.Artifact})
		// The command belongs to this check, rather than today's plan.
		var command struct {
			Command string `json:"command"`
		}
		if c.Kind == "executable" && readJSON(filepath.Join(filepath.Dir(c.Artifact), "verify-command.json"), &command) == nil {
			d.VerifyCommand = command.Command
		}
	}
	return d, nil
}

func requeueTask(dir, session, id, revision string) error {
	ack, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeRequeue,
		Session: session, TaskID: id, TaskRevision: revision}, 30*time.Second)
	if err != nil {
		return err
	}
	if ack.Status != AckApplied {
		return fmt.Errorf("%s", ack.Message)
	}
	return nil
}

func (e *Engine) processRequeueRequests() {
	for {
		req, err := claimNextRequest(e.Dir, RequestTypeRequeue)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyRequeueRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json"))
	}
}

// Requeue changes only status after explicit user confirmation. Task contracts
// and retained attempts stay intact. The controller is the sole writer.
func (e *Engine) applyRequeueRequest(req *SettingsRequest) (ack SettingsAck) {
	ack = SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controller.Session, Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	defer func() { e.acks[req.ID] = ack }()
	if req.Session != e.controller.Session {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Reload the task before requeuing."
		return
	}
	t := e.State.Backlog.Task(req.TaskID)
	if t == nil || t.Status != "blocked" || req.TaskRevision != hashJSON(t) {
		ack.Status = AckConflict
		ack.Message = "The task changed. Inspect its current state before requeuing."
		return
	}
	for _, a := range e.State.Attempts {
		if activeAttempt(a) {
			ack.Status = AckConflict
			ack.Message = "Wait until active work finishes before requeuing this task."
			return
		}
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		ack.Message = err.Error()
		return
	}
	if digest(data) != e.State.ExportHash {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		if e.inputPending {
			ack.Message = "The task file needs correction before Looper can apply changes. Correct it to continue."
		}
		return
	}
	t.Status = "todo"
	t.Blockers = nil
	t.UnblockWhen = ""
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	e.State.Finished = false
	e.State.Plan = nil
	e.State.PlanSignature = ""
	if err = e.export(); err != nil {
		e.err = err
		ack.Message = err.Error()
		return
	}
	ack.Status = AckApplied
	ack.Message = "Task requeued. Retained work is available for reuse."
	e.event("requeue", req.TaskID+" requeued after user confirmation")
	return
}
