package looper

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// cancelEngine builds a protocol engine with one exported backlog, the
// pattern requeueEngine uses, so cancellation tests exercise the real
// export guard and receipt store.
func cancelEngine(t *testing.T, tasks []Task, attempts ...*Attempt) *Engine {
	t.Helper()
	e := protocolEngine(t)
	e.Config.Todo = filepath.Join(e.Dir, "to-do.json")
	e.State = &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: tasks}, Attempts: attempts}
	if err := writeJSON(e.Config.Todo, e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		t.Fatal(err)
	}
	e.State.ExportHash = digest(data)
	return e
}

func cancelRequest(e *Engine, taskID string, cascade bool) *SettingsRequest {
	revision := ""
	if t := e.State.Backlog.Task(taskID); t != nil {
		revision = hashJSON(*t)
	}
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("cancel"), Type: RequestTypeCancel,
		Session: e.controller.Session, TaskID: taskID, TaskRevision: revision}
	req.Cascade = cascade
	return req
}

// submitCancelClaim publishes one cancel envelope and claims it, the path
// processControllerRequests drives.
func submitCancelClaim(t *testing.T, e *Engine, req *SettingsRequest) SettingsAck {
	t.Helper()
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processCancelRequests()
	if e.err != nil {
		t.Fatalf("cancel pass failed: %v", e.err)
	}
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return ack
}

func cancelEvent(e *Engine, kind string) *Event {
	events, err := e.Store.Events(0)
	if err != nil {
		return nil
	}
	for i := range events {
		if events[i].Kind == kind {
			return &events[i]
		}
	}
	return nil
}

// lastReceipt returns the newest recorded receipt, for assertions that name
// no identifier.
func lastReceipt(e *Engine) *RequestRecord {
	var last *RequestRecord
	for _, r := range e.State.Requests {
		if r != nil {
			last = r
		}
	}
	return last
}

// A cancellation with no in-flight work applies at once: the statuses commit
// through the export, the receipt names the decided set, retained results
// stay beside their attempts, and a replay returns the recorded answer.
func TestCancelAppliesCheckedRequest(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "blocked",
		ResultCommit: "commit-1", Summary: &Summary{TaskID: "T1", Status: "blocked", Summary: "retained", Files: []string{}, Blockers: []string{"external"}}}
	e := cancelEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "blocked", Blockers: []string{"external"}},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	}, attempt)
	e.State.Plan, e.State.PlanSignature = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"one/"}}}}, "stale"
	req := cancelRequest(e, "T1", false)
	ack := submitCancelClaim(t, e, req)
	if ack.Status != AckApplied {
		t.Fatalf("cancel: %+v", ack)
	}
	task := e.State.Backlog.Task("T1")
	if task.Status != "cancelled" || task.UpdatedAt == "" {
		t.Fatalf("task not cancelled: %+v", task)
	}
	if e.State.Backlog.Task("T2").Status != "todo" {
		t.Fatal("an unrelated task changed")
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || exported.Tasks[0].Status != "cancelled" {
		t.Fatalf("export: %v", err)
	}
	if e.State.Plan == nil || e.State.PlanSignature != "" {
		t.Fatal("cancel discarded the live plan instead of its signature")
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil || rec.State != RequestApplied || strings.Join(rec.AffectedTasks, ",") != "T1" {
		t.Fatalf("receipt: %+v", rec)
	}
	if attempt.Phase != "blocked" || attempt.ResultCommit != "commit-1" || attempt.Summary == nil {
		t.Fatal("cancel discarded a retained result")
	}
	event := cancelEvent(e, "cancel")
	if event == nil {
		t.Fatal("no cancel event")
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil || payload["request_id"] != req.ID {
		t.Fatalf("cancel event identity: %s", event.Data)
	}
	if repeated := e.applyCancelRequest(req); repeated.Status != AckApplied || repeated.Message != ack.Message {
		t.Fatalf("replay: %+v", repeated)
	}
	if e.State.Backlog.Task("T1").Status != "cancelled" {
		t.Fatal("replay mutated twice")
	}
}

// Every guard refuses without touching saved work: a changed task, an
// unknown task, done work, an already-cancelled task, a stale session, and
// a held external edit.
func TestCancelGuardsRefuseWithoutMutation(t *testing.T) {
	for _, mode := range []string{"stale-revision", "unknown-task", "done-task", "already-cancelled", "stale-session", "held-edit"} {
		t.Run(mode, func(t *testing.T) {
			e := cancelEngine(t, []Task{
				{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
				{ID: "T9", Title: "done work", Priority: 1, Status: "done"},
			})
			var req *SettingsRequest
			switch mode {
			case "stale-revision":
				req = &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("cancel"), Type: RequestTypeCancel,
					Session: e.controller.Session, TaskID: "T1", TaskRevision: "changed"}
			case "unknown-task":
				req = cancelRequest(e, "T404", false)
			case "done-task":
				req = cancelRequest(e, "T9", false)
			case "already-cancelled":
				task := e.State.Backlog.Task("T1")
				task.Status = "cancelled"
				req = cancelRequest(e, "T1", false)
			case "stale-session":
				req = cancelRequest(e, "T1", false)
				req.Session = "run-old"
			case "held-edit":
				req = cancelRequest(e, "T1", false)
				edited := clone(e.State.Backlog)
				edited.Tasks[0].Title = "edited outside"
				if err := writeJSON(e.Config.Todo, edited); err != nil {
					t.Fatal(err)
				}
			}
			before := hashJSON(e.State.Backlog)
			ack := e.applyCancelRequest(req)
			if ack.Status != AckConflict && ack.Status != AckStale {
				t.Fatalf("%s: %+v", mode, ack)
			}
			if ack.Message == "" {
				t.Fatalf("%s: refusal without a reason", mode)
			}
			if hashJSON(e.State.Backlog) != before {
				t.Fatalf("%s: a refusal mutated the backlog", mode)
			}
			rec := e.requestReceipt(req.ID)
			if rec == nil || rec.State != RequestRejected {
				t.Fatalf("%s: receipt %+v", mode, rec)
			}
		})
	}
}

// Live dependents strand when their dependency cancels, so a plain cancel
// refuses naming them; --cascade cancels the transitive live set while done
// dependents keep their accepted history.
func TestCancelDependentsRefusedThenCascaded(t *testing.T) {
	tasks := func() []Task {
		return []Task{
			{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
			{ID: "T3", Title: "three", Priority: 1, Status: "todo", DependsOn: []string{"T2"}},
			{ID: "T4", Title: "four", Priority: 1, Status: "todo", DependsOn: []string{"T3"}},
			{ID: "T9", Title: "done dependent", Priority: 1, Status: "done", DependsOn: []string{"T3"}},
		}
	}
	e := cancelEngine(t, tasks())
	refused := submitCancelClaim(t, e, cancelRequest(e, "T2", false))
	if refused.Status != AckConflict {
		t.Fatalf("plain cancel of a depended-on task: %+v", refused)
	}
	for _, id := range []string{"T3", "T4"} {
		if !strings.Contains(refused.Message, id) {
			t.Fatalf("refusal hides dependent %s: %s", id, refused.Message)
		}
	}
	if e.State.Backlog.Task("T2").Status != "todo" {
		t.Fatal("the refused cancel mutated the backlog")
	}
	// A fresh engine carries the cascade: the refusal above consumed the
	// revision window it checked.
	e = cancelEngine(t, tasks())
	ack := submitCancelClaim(t, e, cancelRequest(e, "T2", true))
	if ack.Status != AckApplied {
		t.Fatalf("cascade: %+v", ack)
	}
	for _, id := range []string{"T2", "T3", "T4"} {
		if e.State.Backlog.Task(id).Status != "cancelled" {
			t.Fatalf("%s not cancelled", id)
		}
	}
	if e.State.Backlog.Task("T9").Status != "done" {
		t.Fatal("cascade cancelled done work")
	}
	if rec := lastReceipt(e); strings.Join(rec.AffectedTasks, ",") != "T2,T3,T4" {
		t.Fatalf("receipt set: %v", rec.AffectedTasks)
	}
}

// A cancellation whose target work is in flight is decided, not refused: the
// receipt waits on active work, the decision survives in the saved state,
// dispatch skips the decided set, and the drive applies it once the work
// reaches a terminal phase — keeping the retained result.
func TestCancelWaitsForActiveWorkThenApplies(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := cancelEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "doing"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo", DependsOn: []string{"T1"}},
	}, attempt)
	req := cancelRequest(e, "T1", true)
	ack := submitCancelClaim(t, e, req)
	if ack.Status != AckWaiting {
		t.Fatalf("active cancel: %+v", ack)
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil || rec.State != RequestWaiting || rec.WaitReason != "active_work" {
		t.Fatalf("waiting receipt: %+v", rec)
	}
	if len(e.State.Cancellations) != 1 || strings.Join(e.State.Cancellations[0].TaskIDs, ",") != "T1,T2" {
		t.Fatalf("pending decision: %+v", e.State.Cancellations)
	}
	saved, err := e.Store.Load()
	if err != nil || len(saved.Cancellations) != 1 {
		t.Fatalf("the pending decision did not survive a save: %v %+v", err, saved.Cancellations)
	}
	if !e.pendingCancelTask("T2") {
		t.Fatal("dispatch does not skip the decided set")
	}
	if cancelEvent(e, "cancel-pending") == nil {
		t.Fatal("no pending event")
	}
	// The active attempt is still live: the drive must not apply yet.
	e.drivePendingCancellations()
	if e.State.Backlog.Task("T1").Status != "doing" {
		t.Fatal("the drive cancelled under active work")
	}
	// The worker finishes blocked: its result stays, then the cancellation
	// applies at the boundary.
	attempt.Phase = "blocked"
	attempt.ResultCommit = "commit-1"
	attempt.Summary = &Summary{TaskID: "T1", Status: "blocked", Summary: "external gap", Files: []string{}, Blockers: []string{"missing"}}
	e.drivePendingCancellations()
	for _, id := range []string{"T1", "T2"} {
		if e.State.Backlog.Task(id).Status != "cancelled" {
			t.Fatalf("%s not cancelled after the drain", id)
		}
	}
	if attempt.ResultCommit != "commit-1" || attempt.Summary == nil {
		t.Fatal("the drive discarded the finished result")
	}
	rec = e.requestReceipt(req.ID)
	if rec.State != RequestApplied || rec.WaitReason != "" {
		t.Fatalf("decided receipt: %+v", rec)
	}
	if len(e.State.Cancellations) != 0 {
		t.Fatal("the applied decision stayed pending")
	}
}

// Work that completes before a pending cancellation applies keeps its
// accepted result: the cancellation records that it cancelled nothing.
func TestPendingCancelKeepsCompletedWork(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	req := cancelRequest(e, "T1", false)
	if ack := submitCancelClaim(t, e, req); ack.Status != AckWaiting {
		t.Fatalf("active cancel: %+v", ack)
	}
	// The worker's verified result applies: done work is accepted history.
	task := e.State.Backlog.Task("T1")
	task.Status = "done"
	attempt.Phase = "done"
	attempt.ResultCommit = "commit-2"
	e.drivePendingCancellations()
	if e.State.Backlog.Task("T1").Status != "done" || attempt.Phase != "done" {
		t.Fatal("the drive discarded accepted work")
	}
	rec := e.requestReceipt(req.ID)
	if rec.State != RequestRejected || !strings.Contains(rec.Error, "T1 (completed)") {
		t.Fatalf("receipt: %+v", rec)
	}
	if len(e.State.Cancellations) != 0 {
		t.Fatal("the decided cancellation stayed")
	}
}

// A restart between the decision and the drain keeps the decision: the next
// controller loads the pending cancellation and drives it.
func TestCancelRestartRetainsDecision(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	req := cancelRequest(e, "T1", false)
	if ack := submitCancelClaim(t, e, req); ack.Status != AckWaiting {
		t.Fatalf("active cancel: %+v", ack)
	}
	// The restarted controller reopens the same store and state.
	next := protocolEngineOn(t, e.Dir, e.controller.Session)
	next.Config.Todo = e.Config.Todo
	s, err := next.Store.Load()
	if err != nil || s == nil {
		t.Fatalf("load: %v", err)
	}
	next.State = s
	if len(next.State.Cancellations) != 1 || next.State.Cancellations[0].RequestID != req.ID {
		t.Fatalf("restart lost the decision: %+v", next.State.Cancellations)
	}
	if rec := next.requestReceipt(req.ID); rec == nil || rec.State != RequestWaiting {
		t.Fatalf("restart lost the waiting receipt: %+v", rec)
	}
	next.State.Attempts[0].Phase = "blocked"
	next.drivePendingCancellations()
	if next.State.Backlog.Task("T1").Status != "cancelled" {
		t.Fatal("the restarted controller did not drive the decision")
	}
	if rec := next.requestReceipt(req.ID); rec.State != RequestApplied {
		t.Fatalf("receipt after restart: %+v", rec)
	}
}

// A receipt that already decided — a failed export poisoned the run after
// recording the failure — is never re-driven, and a claim replayed after a
// crash between the decision save and the acknowledgement write records the
// decision once.
func TestPendingCancelDecidedOnce(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	req := cancelRequest(e, "T1", false)
	if ack := submitCancelClaim(t, e, req); ack.Status != AckWaiting {
		t.Fatalf("active cancel: %+v", ack)
	}
	// A claim replay after a restart (empty in-process cache) keeps one
	// decision and one waiting answer.
	next := protocolEngineOn(t, e.Dir, e.controller.Session)
	next.Config.Todo = e.Config.Todo
	s, err := next.Store.Load()
	if err != nil || s == nil {
		t.Fatalf("load: %v", err)
	}
	next.State = s
	if replayed := next.applyCancelRequest(req); replayed.Status != AckWaiting {
		t.Fatalf("replayed decision: %+v", replayed)
	}
	if len(next.State.Cancellations) != 1 {
		t.Fatalf("the replay recorded a second decision: %+v", next.State.Cancellations)
	}
	// The original engine's state holds the single decision. A receipt the
	// run already decided is not re-driven: the failed export that poisoned
	// the earlier run keeps its recorded failure.
	failed := e.requestReceipt(req.ID)
	failed.State = RequestFailed
	failed.Error = "the earlier export failed"
	failed.Ack = &SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Status: AckError, Message: "the earlier export failed"}
	attempt.Phase = "blocked"
	e.drivePendingCancellations()
	if e.State.Backlog.Task("T1").Status != "doing" {
		t.Fatal("a decided receipt was re-driven")
	}
	if len(e.State.Cancellations) != 0 {
		t.Fatalf("the stale decision stayed: %+v", e.State.Cancellations)
	}
	if again := e.commitCancellation(failed, []string{"T1"}); again.Status != AckError {
		t.Fatalf("a terminal receipt re-decided: %+v", again)
	}
}

// A resume attempt holds no result and no worker, so cancelling its task
// applies at once and retires the attempt instead of waiting for a relaunch
// the decision itself prevents — the live exercise found that deadlock. The
// retired attempt stops counting as retained work and recovery never
// re-drives it.
func TestCancelRetiresResumeAttempt(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "resume", Worktree: "retained"}
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	if cancelBlockingAttempt(attempt) {
		t.Fatal("precondition: a resume attempt must not block a cancellation")
	}
	req := cancelRequest(e, "T1", false)
	ack := submitCancelClaim(t, e, req)
	if ack.Status != AckApplied {
		t.Fatalf("resume-only work cancelled actively: %+v", ack)
	}
	if len(e.State.Cancellations) != 0 {
		t.Fatal("the cancellation pended on restartable work")
	}
	if attempt.Phase != "cancelled" {
		t.Fatalf("the resume attempt stayed %q", attempt.Phase)
	}
	if activeAttempt(attempt) || retained(e.State) || e.pendingCancelTask("T1") {
		t.Fatal("the retired attempt still counts as retained work")
	}
	if !replaySafeRetainedWork(e.State) {
		t.Fatal("the retired attempt still blocks a held import")
	}
}

// A held task-file edit defers the drive exactly as it refuses fresh
// requests: the edit imports first, then the decision applies.
func TestPendingCancelWaitsForHeldEdit(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	req := cancelRequest(e, "T1", false)
	if ack := submitCancelClaim(t, e, req); ack.Status != AckWaiting {
		t.Fatalf("active cancel: %+v", ack)
	}
	attempt.Phase = "blocked"
	edited := clone(e.State.Backlog)
	edited.Tasks = append(edited.Tasks, Task{ID: "T7", Title: "external addition", Priority: 2, Status: "todo"})
	if err := writeJSON(e.Config.Todo, edited); err != nil {
		t.Fatal(err)
	}
	e.drivePendingCancellations()
	if e.State.Backlog.Task("T1").Status != "doing" || e.State.Backlog.Task("T7") != nil {
		t.Fatal("the drive applied a cancellation beside a held edit")
	}
	if req2 := cancelRequest(e, "T7", false); e.applyCancelRequest(req2).Status != AckConflict {
		t.Fatal("a fresh cancel applied beside a held edit")
	}
}

// A failed export rolls the staged cancellation back and records the
// failure, mirroring add-task and requeue. The store closes before the
// request so the export's own state save fails after the task file read.
func TestCancelExportFailureRollsBack(t *testing.T) {
	e := cancelEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}})
	before := hashJSON(e.State.Backlog)
	if err := e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	req := cancelRequest(e, "T1", false)
	ack := e.applyCancelRequest(req)
	if ack.Status != AckError || e.err == nil {
		t.Fatalf("export failure: %+v err=%v", ack, e.err)
	}
	if hashJSON(e.State.Backlog) != before {
		t.Fatal("the failed export kept the staged cancellation")
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil || rec.State != RequestFailed {
		t.Fatalf("failed receipt: %+v", rec)
	}
}

// Caller keys make cancel retries return the recorded outcome, and a changed
// payload under the same key is a conflict (AX-012).
func TestCancelIdempotencyKey(t *testing.T) {
	e := cancelEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	})
	first := cancelRequest(e, "T1", false)
	first.IdempotencyKey = "cancel-once"
	original := e.applyCancelRequest(first)
	if original.Status != AckApplied {
		t.Fatalf("keyed cancel: %+v", original)
	}
	// A retry sends the payload the original sent: the revision the caller
	// read, not the post-cancellation task.
	retry := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("cancel"), Type: RequestTypeCancel,
		Session: e.controller.Session, TaskID: first.TaskID, TaskRevision: first.TaskRevision, IdempotencyKey: "cancel-once"}
	ack := e.applyCancelRequest(retry)
	if ack.Status != AckApplied || !strings.Contains(ack.Message, original.Message) {
		t.Fatalf("replay: %+v", ack)
	}
	if len(e.State.Requests) < 2 {
		t.Fatalf("replay recorded nothing: %+v", e.State.Requests)
	}
	changed := cancelRequest(e, "T2", false)
	changed.IdempotencyKey = "cancel-once"
	if ack = e.applyCancelRequest(changed); ack.Status != AckConflict {
		t.Fatalf("changed payload under the key: %+v", ack)
	}
	if e.State.Backlog.Task("T2").Status != "todo" {
		t.Fatal("the conflicting retry mutated the backlog")
	}
}

// The schema migration is additive: backlogs written before cancellations
// still validate, cancelled tasks validate, unknown statuses stay refused, a
// saved state from before AX-019 loads with no pending cancellations, and a
// cancelled dependency never satisfies a dependent.
func TestCancelledStatusMigrationKeepsOldBacklogs(t *testing.T) {
	old := `{"schema_version":1,"source_files":[],"tasks":[
		{"id":"T1","title":"one","priority":1,"status":"todo"},
		{"id":"T2","title":"two","priority":1,"status":"blocked"}]}`
	b, err := parseBacklogStrict([]byte(old))
	if err != nil {
		t.Fatalf("old backlog: %v", err)
	}
	if err = b.Validate(); err != nil {
		t.Fatalf("old backlog invalid: %v", err)
	}
	fresh := `{"schema_version":1,"source_files":[],"tasks":[
		{"id":"T1","title":"one","priority":1,"status":"cancelled"}]}`
	if b, err = parseBacklogStrict([]byte(fresh)); err != nil {
		t.Fatalf("cancelled backlog: %v", err)
	}
	if err = b.Validate(); err != nil {
		t.Fatalf("cancelled status invalid: %v", err)
	}
	bad := strings.Replace(fresh, `"cancelled"`, `"withdrawn"`, 1)
	if b, err = parseBacklogStrict([]byte(bad)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if err = b.Validate(); err == nil {
		t.Fatal("an unknown status validated")
	}
	// A saved state from before AX-019 carries no cancellations field and
	// still loads.
	preAX19 := `{"version":2,"root":"/p","todo":"/p/to-do.json","head":"h","branch":"b","ref":"r","tip":"t","published":"p",
		"backlog":{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","title":"one","priority":1,"status":"todo"}]},
		"export_hash":"x","attempts":[]}`
	var s State
	if err = json.Unmarshal([]byte(preAX19), &s); err != nil {
		t.Fatalf("old state: %v", err)
	}
	if s.Cancellations != nil {
		t.Fatal("an old state gained cancellations")
	}
	// Cancelled work is not plannable and never counts as a satisfied
	// dependency.
	s.Backlog.Tasks[0].Status = "cancelled"
	s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T2", Title: "two", Priority: 1, Status: "todo", DependsOn: []string{"T1"}})
	if ids := s.Backlog.Ready(); len(ids) != 0 {
		t.Fatalf("a cancelled dependency satisfied a dependent: %v", ids)
	}
	plan := &Plan{Verify: "true"}
	if err = plan.Validate(s.Backlog, ""); err != nil {
		t.Fatalf("plan over a cancelled task: %v", err)
	}
	for _, scope := range plan.Tasks {
		if scope.TaskID == "T1" {
			t.Fatal("the default plan gave cancelled work a write scope")
		}
	}
}

// The public CLI path: the receipt returns at once naming the durable
// request, the identifier is queryable as a cancel-task record, a live
// engine decides the envelope, an unknown task is a usage-side failure, and
// a bare cancel is a usage error.
func TestCancelCLIReceiptAndQuery(t *testing.T) {
	c, dir := receiptProject(t, "cancel")
	b := testBacklog()
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}}
	if err = store.Save(state); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	cc := v2Config(c, "cancel", true)
	cc.CancelTask = "T1"
	code := runCancel(cc, &out, &stderr)
	if code != exitOK {
		t.Fatalf("cancel exit %d: %s%s", code, out.String(), stderr.String())
	}
	receipt := decodeReceipt(t, out.String(), stderr.String())
	if receipt.Request.Type != PublicRequestCancel || receipt.Request.State != RequestSubmitted {
		t.Fatalf("receipt: %+v", receipt.Request)
	}
	if receipt.Request.ControllerSession != "run-live" {
		t.Fatalf("receipt session: %+v", receipt.Request)
	}
	queryCode, doc, _ := runRequestsCLI(t, receipt.Request.ID, "--contract", "v2", "--json")
	if queryCode != exitOK {
		t.Fatalf("query exit %d", queryCode)
	}
	if doc.Request.Type != PublicRequestCancel || doc.Request.State != RequestSubmitted {
		t.Fatalf("record: %+v", doc.Request)
	}
	// The controller decides the queued envelope; the record answers.
	e := &Engine{Dir: dir, Output: io.Discard, Store: store, State: state, Config: c,
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1)}
	e.processCancelRequests()
	if state.Backlog.Task("T1").Status != "cancelled" {
		t.Fatalf("queued envelope not applied: %+v", state.Backlog.Tasks)
	}
	// An unknown task is a usage-side failure with the fix in the message.
	missing := v2Config(c, "cancel", true)
	missing.CancelTask = "T404"
	out.Reset()
	stderr.Reset()
	if code = runCancel(missing, &out, &stderr); code != exitOperationFailed {
		t.Fatalf("unknown task exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "T404") {
		t.Fatalf("unknown task message: %s%s", out.String(), stderr.String())
	}
	// No task argument is a usage error.
	out.Reset()
	stderr.Reset()
	if code = runCancel(v2Config(c, "cancel", true), &out, &stderr); code != exitUsage {
		t.Fatalf("bare cancel exit %d: %s%s", code, out.String(), stderr.String())
	}
}

// --wait against a live engine reports the request record: applied for a
// decided cancellation, waiting for one that waits on active work.
func TestCancelWaitReportsRecord(t *testing.T) {
	c, dir := receiptProject(t, "cancel")
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{Dir: dir, Output: io.Discard, Store: store, Config: c,
		State:      &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}},
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{},
		viewNotify: make(chan ui.ViewSnapshot, 1),
	}
	if err = store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 250; i++ {
			e.processCancelRequests()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { <-done }()
	cc := v2Config(c, "cancel", true)
	cc.CancelTask, cc.Wait, cc.WaitTimeout = "T1", true, 30*time.Second
	var out, stderr strings.Builder
	if code := runCancel(cc, &out, &stderr); code != exitOK {
		t.Fatalf("cancel --wait exit %d: %s%s", code, out.String(), stderr.String())
	}
	var doc requestRecordDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one record document: %v\n%s", err, out.String())
	}
	if doc.Kind != requestRecordKind || doc.Request.State != RequestApplied {
		t.Fatalf("record: %+v", doc.Request)
	}
}

// --wait against a cancellation that waits on active work reports the
// waiting record and exits 0.
func TestCancelWaitReportsWaiting(t *testing.T) {
	c, dir := receiptProject(t, "cancel")
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{Dir: dir, Output: io.Discard, Store: store, Config: c,
		State: &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data),
			Attempts: []*Attempt{{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}}},
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1),
	}
	if err = store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 250; i++ {
			e.processCancelRequests()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { <-done }()
	cc := v2Config(c, "cancel", true)
	cc.CancelTask, cc.Wait, cc.WaitTimeout = "T1", true, 10*time.Second
	var out, stderr strings.Builder
	if code := runCancel(cc, &out, &stderr); code != exitOK {
		t.Fatalf("cancel --wait exit %d: %s%s", code, out.String(), stderr.String())
	}
	var doc requestRecordDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one record document: %v\n%s", err, out.String())
	}
	if doc.Request.State != RequestWaiting || doc.Request.WaitingOn == nil || doc.Request.WaitingOn.Reason != "active_work" {
		t.Fatalf("waiting record: %+v", doc.Request)
	}
}

// The option surface: --cascade belongs to cancel alone, and the mutation
// wait and key options accept it.
func TestCancelOptionParsing(t *testing.T) {
	if _, err := Parse([]string{"status", "--cascade"}); err == nil {
		t.Fatal("--cascade parsed outside cancel")
	}
	c, err := Parse([]string{"cancel", "T1", "--cascade", "--wait", "--idempotency-key", "k1"})
	if err != nil {
		t.Fatalf("cancel options: %v", err)
	}
	if !c.Cascade || !c.Wait || c.IdempotencyKey != "k1" || c.CancelTask != "T1" || c.Command != "cancel" {
		t.Fatalf("parsed: %+v", c)
	}
	if _, err = Parse([]string{"cancel", "T1", "T2"}); err == nil {
		t.Fatal("cancel accepted two tasks")
	}
	// The cancelled word reached both schema copies; the byte identity of
	// the two is pinned by TestShippedSchemaMatchesDefault.
	if !strings.Contains(backlogSchema, `"cancelled"`) {
		t.Fatal("the in-code schema lost the cancelled status")
	}
	shipped, err := os.ReadFile(shippedSchemaPath(t))
	if err != nil {
		// The shipped copy is tracked content. A tree without it dropped
		// tracked files, which is an assembly defect, not a skip.
		t.Fatal(err)
	}
	if !strings.Contains(string(shipped), `"cancelled"`) {
		t.Fatal("the shipped schema lost the cancelled status")
	}
}
