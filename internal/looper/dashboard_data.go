package looper

import (
	"errors"
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
// Before the controller publishes its first view, a live owner yields a
// bootstrap snapshot so that dashboards can show agent liveness instead of
// reporting no controller.
func (o *observer) view(dir, session string) *ui.ViewSnapshot {
	info, err := readController(dir)
	if err != nil || info.Session != session || !dashboardControllerAlive(info) {
		return nil
	}
	v := LoadView(dir)
	if v == nil || v.Session != session {
		return bootstrapView(dir, session, info)
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

// bootstrapStream is append-only evidence of bootstrap agent liveness: the
// newest event file, its size, and how long ago it last grew.
type bootstrapStream struct {
	log   string
	bytes int64
	idle  time.Duration
}

// latestBootstrap finds the newest bootstrap agent event stream. Retained
// attempts from earlier sessions sort behind the current one by write time.
func latestBootstrap(dir string) *bootstrapStream {
	paths, _ := filepath.Glob(filepath.Join(dir, "bootstrap-*", "events-*.jsonl"))
	var newest string
	var stamp time.Time
	var size int64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(stamp) {
			newest, stamp, size = path, info.ModTime(), info.Size()
		}
	}
	if newest == "" {
		return nil
	}
	return &bootstrapStream{log: newest, bytes: size, idle: time.Since(stamp)}
}

// bootstrapView synthesizes the pre-controller snapshot from the recorded
// owner and the bootstrap agent stream.
func bootstrapView(dir, session string, info *ControllerInfo) *ui.ViewSnapshot {
	v := &ui.ViewSnapshot{
		Version: ui.ViewVersion, Session: session, Started: info.Started, Root: info.Root,
		Now: time.Now().UTC().Format(time.RFC3339), Bootstrapping: true,
		ActivityAgent: info.BootstrapAgent,
	}
	if bs := latestBootstrap(dir); bs != nil {
		v.ActivityLog = bs.log
		v.BootstrapBytes = bs.bytes
		v.BootstrapIdleSeconds = bs.idle.Seconds()
	}
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

func requeueTask(dir, session, id, revision string, fresh bool) error {
	reqType := RequestTypeRequeue
	if fresh {
		reqType = RequestTypeRequeueFresh
	}
	ack, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, Type: reqType,
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
		req, err := e.claimRequest(RequestTypeRequeue)
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
		// A poisoned engine exits at the next loop check; this request is
		// answered and consumed, so the pass stops here.
		if e.err != nil {
			return
		}
	}
}

// Requeue changes only status after explicit user confirmation. Task contracts
// and retained attempts stay intact. The controller is the sole writer. The
// application and its receipt stage together until the saved state/export
// protocol commits them, exactly as add-task does (AX-009).
func (e *Engine) applyRequeueRequest(req *SettingsRequest) (ack SettingsAck) {
	ack = SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if replayed, ok := e.replayedAck(req); ok {
		// A durable receipt already decided this identifier.
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	// A caller key answers from the receipt that recorded it, before any
	// session check, exactly like add-task and settings (AX-012).
	if req.IdempotencyKey != "" {
		if _, original, conflict := e.resolveIdempotencyKey(req, nil); original != nil {
			ack = e.replayIdempotentAck(req, original)
			return ack
		} else if conflict != nil {
			ack.Status = AckConflict
			ack.Message = keyConflictMessage(req.IdempotencyKey, conflict, requestPayloadDigest(req, nil))
			e.rejectRequestReceipt(req, ack)
			return ack
		}
	}
	refuse := func() SettingsAck {
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	defer func() { e.rememberAck(req.ID, ack) }()
	if req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Reload the task before requeuing."
		return refuse()
	}
	t := e.State.Backlog.Task(req.TaskID)
	if t == nil || t.Status != "blocked" || req.TaskRevision != hashJSON(t) {
		ack.Status = AckConflict
		ack.Message = "The task changed. Inspect its current state before requeuing."
		return refuse()
	}
	for _, a := range e.State.Attempts {
		if activeAttempt(a) {
			ack.Status = AckConflict
			ack.Message = "Wait until active work finishes before requeuing this task."
			return refuse()
		}
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		ack.Message = err.Error()
		return refuse()
	}
	// Equivalent formatting is not a change (AX-018): only an edit that
	// changed content keeps the requeue waiting for its boundary import.
	if edit := e.classifyBacklog(data); edit.Changed && !edit.Equivalent {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		if e.inputPending {
			ack.Message = "The task file needs correction before Looper can apply changes. Correct it to continue."
		}
		return refuse()
	}
	if t.Deferred() {
		ack.Status = AckConflict
		ack.Message = "Task is deferred. Meet its resume condition, then remove looper:deferred and set todo in the task file."
		return refuse()
	}
	staged := stagedBacklogRequest{
		backlog: clone(e.State.Backlog), planSignature: e.State.PlanSignature,
		finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = []string{req.TaskID}
	t.Status = "todo"
	t.Blockers = nil
	t.BlockReason = nil
	t.UnblockWhen = ""
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	e.State.Finished = false
	// Keep the live plan: a worker can finish between the active-work guard
	// above and this export, leaving a pending attempt that reads its scope.
	// The cleared signature forces the next planning pass to rebuild.
	e.State.PlanSignature = ""
	ack.Status = AckApplied
	ack.Message = "Task requeued. Retained work is available for reuse."
	rec.State = RequestApplied
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Summary, rec.Ack = ack.Message, &ack
	if err = e.export(); err != nil {
		// Roll the staged requeue back and persist the rollback with a failed
		// receipt before the poisoned engine exits, mirroring add-task.
		staged.rollback(e)
		ack.Status = AckError
		ack.Message = err.Error()
		e.failStagedRequest(rec, ack, err)
		if saveErr := e.persistRequestReceipts(); saveErr != nil {
			ack.Message = errors.Join(err, saveErr).Error()
		}
		e.err = err
		return
	}
	requeue := e.identity()
	requeue.TaskID = req.TaskID
	requeue.RequestID = req.ID
	e.eventData("requeue", req.TaskID+" requeued after user confirmation", requeue.merge(nil))
	return
}

// processRequeueFreshRequests claims and answers requeue-fresh envelopes.
// It runs beside the ordinary requeue processing in the controller loop.
func (e *Engine) processRequeueFreshRequests() {
	for {
		req, err := e.claimRequest(RequestTypeRequeueFresh)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyRequeueFreshRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json"))
		if e.err != nil {
			return
		}
	}
}

// applyRequeueFreshRequest retires every reusable attempt of one blocked
// task and sets the task back to todo. The attempts keep their artifacts,
// results, and history as evidence, but no selector resumes or reuses them:
// the next dispatch creates a new attempt under the current worker role.
// The task identity, contract, dependencies, and history stay untouched.
func (e *Engine) applyRequeueFreshRequest(req *SettingsRequest) (ack SettingsAck) {
	ack = SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if replayed, ok := e.replayedAck(req); ok {
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	if req.IdempotencyKey != "" {
		if _, original, conflict := e.resolveIdempotencyKey(req, nil); original != nil {
			ack = e.replayIdempotentAck(req, original)
			return ack
		} else if conflict != nil {
			ack.Status = AckConflict
			ack.Message = keyConflictMessage(req.IdempotencyKey, conflict, requestPayloadDigest(req, nil))
			e.rejectRequestReceipt(req, ack)
			return ack
		}
	}
	refuse := func() SettingsAck {
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	defer func() { e.rememberAck(req.ID, ack) }()
	if req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Reload the task before requeuing."
		return refuse()
	}
	t := e.State.Backlog.Task(req.TaskID)
	if t == nil || t.Status != "blocked" || req.TaskRevision != hashJSON(t) {
		ack.Status = AckConflict
		ack.Message = "The task changed. Inspect its current state before requeuing."
		return refuse()
	}
	for _, a := range e.State.Attempts {
		if activeAttempt(a) {
			ack.Status = AckConflict
			ack.Message = "Wait until active work finishes before requeuing this task."
			return refuse()
		}
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		ack.Message = err.Error()
		return refuse()
	}
	if edit := e.classifyBacklog(data); edit.Changed && !edit.Equivalent {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		if e.inputPending {
			ack.Message = "The task file needs correction before Looper can apply changes. Correct it to continue."
		}
		return refuse()
	}
	if t.Deferred() {
		ack.Status = AckConflict
		ack.Message = "Task is deferred. Meet its resume condition, then remove looper:deferred and set todo in the task file."
		return refuse()
	}
	// Retirement must name every reusable attempt, so the attempt set the
	// caller read is part of the contract.
	if req.AttemptsRevision != "" && req.AttemptsRevision != attemptSetRevision(e.State, req.TaskID) {
		ack.Status = AckConflict
		ack.Message = "The task's attempts changed. Inspect the task before requeuing fresh."
		return refuse()
	}
	staged := stagedBacklogRequest{
		backlog: clone(e.State.Backlog), planSignature: e.State.PlanSignature,
		finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	for _, a := range e.State.Attempts {
		staged.attempts = append(staged.attempts, a)
		staged.attemptPhases = append(staged.attemptPhases, a.Phase)
		staged.attemptRetired = append(staged.attemptRetired, a.Retired)
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = []string{req.TaskID}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	retired := 0
	for _, a := range e.State.Attempts {
		if a.Task.ID == req.TaskID && a.Phase == "blocked" && !attemptRetired(a) {
			a.Retired = &AttemptRetired{RequestID: req.ID, At: now, Reason: "fresh_requeue"}
			retired++
		}
	}
	t.Status = "todo"
	t.Blockers = nil
	t.BlockReason = nil
	t.UnblockWhen = ""
	t.UpdatedAt = now
	e.State.Finished = false
	e.State.PlanSignature = ""
	ack.Status = AckApplied
	ack.Message = fmt.Sprintf("Task requeued fresh. %d attempt(s) retired as evidence; the next dispatch starts clean.", retired)
	rec.State = RequestApplied
	rec.DecidedAt = now
	rec.Summary, rec.Ack = ack.Message, &ack
	if err = e.export(); err != nil {
		staged.rollback(e)
		ack.Status = AckError
		ack.Message = err.Error()
		e.failStagedRequest(rec, ack, err)
		if saveErr := e.persistRequestReceipts(); saveErr != nil {
			ack.Message = errors.Join(err, saveErr).Error()
		}
		e.err = err
		return
	}
	ev := e.identity()
	ev.TaskID = req.TaskID
	ev.RequestID = req.ID
	e.eventData("requeue-fresh", fmt.Sprintf("%s requeued fresh: %d attempt(s) retired", req.TaskID, retired), ev.merge(nil))
	return
}

// attemptSetRevision digests the attempt evidence one task holds: identities,
// phases, and retirement markers in order. A fresh requeue compares it so
// retirement names every reusable attempt the caller saw.
func attemptSetRevision(s *State, taskID string) string {
	type row struct {
		ID      string `json:"id"`
		Phase   string `json:"phase"`
		Retired bool   `json:"retired"`
	}
	rows := []row{}
	for _, a := range s.Attempts {
		if a.Task.ID == taskID {
			rows = append(rows, row{ID: a.ID, Phase: a.Phase, Retired: attemptRetired(a)})
		}
	}
	return digest(mustMarshal(rows))
}
