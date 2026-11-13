package looper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Task amendment through checked controller requests (AX-020). An amend
// request names one task, the revision of that task the caller read, and the
// complete amended task. The controller refuses a changed task, a held
// task-file edit, a smuggled status or identity change, and a contract that
// work still holds; it validates the amended dependencies and write scope
// before committing anything.
//
// Two application shapes exist. An open task — no attempt holds its contract
// — is amended in place. A done task is never rewritten: changed
// requirements create a follow-up task carrying them, and the accepted task
// keeps its record and results exactly as they were. Active or restartable
// work (running, verifying, retained results, resume attempts) owns its
// contract until it drains: the amendment is refused naming that boundary,
// because the startup contract guard would strand the retained work against
// a moved contract.

// controlTag reports whether one task tag encodes an engine-owned decision
// rather than caller content: completion (project-done), operator ownership,
// deferral, or finding identity. An amendment cannot add or remove these —
// the held-import task-file edit remains the route that changes them, with
// the export guard it already answers to.
func controlTag(tag string) bool {
	return tag == "project-done" || tag == operatorTaskTag || tag == deferredTaskTag ||
		strings.HasPrefix(tag, findingKeyPrefix)
}

// controlTagChanges lists the control tags one amendment would add or drop.
func controlTagChanges(original, amended Task) []string {
	has := func(t Task, tag string) bool { return hasTaskTag(t, tag) }
	var changes []string
	seen := map[string]bool{}
	for _, tag := range append(append([]string{}, original.Tags...), amended.Tags...) {
		if !controlTag(tag) || seen[tag] {
			continue
		}
		seen[tag] = true
		if has(original, tag) != has(amended, tag) {
			verb := "adds"
			if has(original, tag) {
				verb = "removes"
			}
			changes = append(changes, fmt.Sprintf("%s %s", verb, tag))
		}
	}
	sort.Strings(changes)
	return changes
}

// amendHoldsContract reports whether one attempt owns the contract of its
// task against amendment. The startup contract guard compares every attempt
// outside done, blocked, and cancelled — running and verifying work, retained
// results waiting on their check, and restartable resume work — so amending
// under one of them would strand the work at the next start. Done and blocked
// attempts keep their recorded results for inspection; the resume path
// already skips a blocked attempt whose contract moved.
func amendHoldsContract(a *Attempt) bool {
	switch a.Phase {
	case "done", "blocked", "cancelled":
		return false
	}
	return true
}

// appliedAmendment is the task one amendment commits: the amendable fields
// come from the caller's payload; identity, status, and creation history stay
// with the task the controller holds. updated_at is engine-set at commit.
func appliedAmendment(original, amended Task) Task {
	applied := amended
	applied.ID = original.ID
	applied.Status = original.Status
	applied.CreatedAt = original.CreatedAt
	applied.UpdatedAt = original.UpdatedAt
	return applied
}

// amendedFieldNames lists, in a stable order, the amendable fields one
// application changed, so the recorded summary names the real delta.
func amendedFieldNames(original, applied Task) []string {
	var changed []string
	track := func(name string, differs bool) {
		if differs {
			changed = append(changed, name)
		}
	}
	track("title", original.Title != applied.Title)
	track("priority", original.Priority != applied.Priority)
	track("details", original.Details != applied.Details)
	track("steps", !equalStrings(original.Steps, applied.Steps))
	track("depends_on", !equalStrings(original.DependsOn, applied.DependsOn))
	track("files", !equalStrings(original.Files, applied.Files))
	track("blockers", !equalStrings(original.Blockers, applied.Blockers))
	track("tags", !equalStrings(original.Tags, applied.Tags))
	track("unblock_when", original.UnblockWhen != applied.UnblockWhen)
	return changed
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// followUpTaskID derives the identifier one done task's amendment creates:
// the first free `<id>-amend-<n>`. Amendment never reuses the accepted task's
// identity, because that identity names the history the accepted result
// belongs to.
func followUpTaskID(b Backlog, id string) string {
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("%s-amend-%d", id, n)
		if b.Task(candidate) == nil {
			return candidate
		}
	}
}

func (e *Engine) processAmendRequests() {
	if e.controller == nil {
		return
	}
	for {
		req, err := e.claimRequest(RequestTypeAmend)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyAmendRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		if err = os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json")); err != nil {
			e.err = err
			return
		}
		// A poisoned engine exits at the next loop check; this request is
		// answered and consumed, so the pass stops here.
		if e.err != nil {
			return
		}
	}
}

// applyAmendRequest validates and applies one claimed amendment. The guards
// mirror cancel: replay by identifier or caller key, the session check, the
// held-edit check, and the expected revision — then the amendment's own
// validation before any commit. Every outcome records its durable receipt.
func (e *Engine) applyAmendRequest(req *SettingsRequest) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if replayed, ok := e.replayedAck(req); ok {
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	// A caller key answers from the receipt that recorded it, before any
	// session check, exactly like the other backlog mutations (AX-012): a
	// retry after a controller restart returns the recorded outcome and
	// never amends twice.
	if req.IdempotencyKey != "" {
		if _, original, conflict := e.resolveIdempotencyKey(req, nil); original != nil {
			return e.replayIdempotentAck(req, original)
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
	if len(req.Tasks) != 1 {
		ack.Status = AckError
		ack.Message = fmt.Sprintf("an amendment carries exactly one task; the request carries %d", len(req.Tasks))
		return refuse()
	}
	amended := req.Tasks[0]
	if req.TaskID == "" {
		ack.Status = AckError
		ack.Message = "the request names no task"
		return refuse()
	}
	if amended.ID != req.TaskID {
		ack.Status = AckError
		ack.Message = fmt.Sprintf("the amended task names id %q but the request targets %q", amended.ID, req.TaskID)
		return refuse()
	}
	if req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Reload the task before amending."
		return refuse()
	}
	if e.inputPending {
		ack.Status = AckConflict
		ack.Message = "The task file needs correction before Looper can apply changes. Correct it to continue."
		return refuse()
	}
	// Equivalent formatting is not a change (AX-018): only an edit that
	// changed content makes the request wait for its boundary import.
	if edit := e.classifyBacklogFile(); edit.Changed && !edit.Equivalent {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		return refuse()
	}
	t := e.State.Backlog.Task(req.TaskID)
	if t == nil {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Task %s is not in the backlog. Inspect the current tasks before amending.", req.TaskID)
		return refuse()
	}
	if req.TaskRevision != hashJSON(*t) {
		ack.Status = AckConflict
		ack.Message = "The task changed. Inspect its current state before amending."
		return refuse()
	}
	if amended.Status != t.Status {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("An amendment does not change status: %s is %s. Status transitions stay with the engine; open work can be cancelled with looper cancel.", req.TaskID, t.Status)
		return refuse()
	}
	if changes := controlTagChanges(*t, amended); len(changes) > 0 {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("An amendment cannot change control tags: it %s. Edit the task file itself for that change; Looper imports it at a safe boundary.", strings.Join(changes, ", "))
		return refuse()
	}
	// The safe boundary (AX-020): work that holds this task's contract — a
	// live worker, a check, a retained result, or restartable resume work —
	// must drain first, because the next startup compares those attempts
	// against this contract and refuses a moved one.
	for _, a := range e.State.Attempts {
		if a != nil && a.Task.ID == req.TaskID && amendHoldsContract(a) {
			ack.Status = AckConflict
			ack.Message = fmt.Sprintf("Task %s holds %s work that owns its contract; it cannot change under a worker. Wait for the work to finish (or cancel it with looper cancel), then amend the drained task.", req.TaskID, a.Phase)
			return refuse()
		}
	}
	switch t.Status {
	case "cancelled":
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Task %s is cancelled; decided work is not amended. Add the changed requirements as a new task with looper add-task.", req.TaskID)
		return refuse()
	case "done":
		ack = e.applyDoneAmendment(req, *t, amended)
		return ack
	}
	applied := appliedAmendment(*t, amended)
	if len(applied.invalidFiles()) > 0 {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Task %s names write paths an amendment cannot accept: %s. Write paths stay relative to the repository root, inside it, and literal.", req.TaskID, boundedIDList(applied.invalidFiles(), 4))
		return refuse()
	}
	if changed := amendedFieldNames(*t, applied); len(changed) == 0 {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("The amendment changes nothing in task %s; nothing was applied.", req.TaskID)
		return refuse()
	}
	// The assignment keeps the deferred rememberAck answering the decided
	// outcome, exactly as cancel assigns its commit result.
	ack = e.commitInPlaceAmendment(req, *t, applied)
	return ack
}

// commitInPlaceAmendment applies one amendment to an open, unheld task. The
// application stages with the backlog mutation protocol add-task, cancel, and
// requeue share: the export commits the amended task beside the receipt, and
// a failed export rolls both back with a failed receipt.
func (e *Engine) commitInPlaceAmendment(req *SettingsRequest, original, applied Task) SettingsAck {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	staged := stagedBacklogRequest{
		backlog: clone(e.State.Backlog), planSignature: e.State.PlanSignature,
		finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		attempts: append([]*Attempt{}, e.State.Attempts...),
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	staged.attemptPhases = make([]string, len(staged.attempts))
	for i, a := range staged.attempts {
		staged.attemptPhases[i] = a.Phase
	}
	// The candidate builds on a copied task list: a refused validation must
	// leave the live backlog untouched, exactly as a refusal leaves it.
	next := e.State.Backlog
	next.Tasks = append([]Task{}, e.State.Backlog.Tasks...)
	applied.UpdatedAt = now
	for i := range next.Tasks {
		if next.Tasks[i].ID == applied.ID {
			next.Tasks[i] = applied
		}
	}
	if err := next.Validate(); err != nil {
		ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckConflict,
			Message: fmt.Sprintf("The amended task is invalid: %v", err)}
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = []string{applied.ID}
	// A changed write scope must not dispatch against the retained plan: the
	// scope drops so the task holds the whole-tree default — parallel
	// dispatch waits — until the next planning pass rebuilds it. The
	// signature clears for every amendment, because a files-only change does
	// not move Contract() and the stale plan would otherwise survive.
	var droppedScope *Scope
	if e.State.Plan != nil {
		droppedScope = new(Scope)
		*droppedScope = e.State.Plan.Scope(applied.ID)
		e.State.Plan.dropScope(applied.ID)
	}
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	e.State.Backlog = next
	changed := amendedFieldNames(original, applied)
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckApplied,
		Message: fmt.Sprintf("Amended %s (%s).", applied.ID, strings.Join(changed, ", "))}
	rec.State = RequestApplied
	rec.DecidedAt = now
	rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
	rec.Summary, rec.Ack = ack.Message, &ack
	e.upsertRequest(rec)
	if err := e.export(); err != nil {
		// Roll the staged application back and persist the rollback with a
		// failed receipt before the poisoned engine exits, mirroring
		// add-task and cancel. The dropped plan scope returns with the
		// signature it belonged to, so a rolled-back amendment leaves the
		// retained plan exactly as it found it.
		if droppedScope != nil && e.State.Plan != nil {
			e.State.Plan.Tasks = append(e.State.Plan.Tasks, *droppedScope)
		}
		staged.rollback(e)
		ack.Status = AckError
		ack.Message = err.Error()
		e.failStagedRequest(rec, ack, err)
		if saveErr := e.persistRequestReceipts(); saveErr != nil {
			ack.Message = errors.Join(err, saveErr).Error()
		}
		e.err = err
		return ack
	}
	id := e.identity()
	id.RequestID = req.ID
	e.eventData("amend", ack.Message, id.merge(map[string]any{"tasks": []string{applied.ID}, "changed_fields": changed}))
	return ack
}

// applyDoneAmendment turns an amendment against completed work into follow-up
// work: the accepted task keeps its identity, record, and results exactly as
// they are, and a new task carries the changed requirements. The follow-up
// depends on nothing beyond what the amended task named — the caller controls
// its dependencies, and the completed task never becomes an implicit gate.
func (e *Engine) applyDoneAmendment(req *SettingsRequest, done, amended Task) SettingsAck {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	followUp := appliedAmendment(done, amended)
	followUp.ID = followUpTaskID(e.State.Backlog, done.ID)
	followUp.Status = "todo"
	followUp.CreatedAt = now
	followUp.UpdatedAt = now
	if len(followUp.invalidFiles()) > 0 {
		ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckConflict,
			Message: fmt.Sprintf("Task %s names write paths an amendment cannot accept: %s. Write paths stay relative to the repository root, inside it, and literal.", done.ID, boundedIDList(followUp.invalidFiles(), 4))}
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	staged := stagedBacklogRequest{
		backlog: clone(e.State.Backlog), planSignature: e.State.PlanSignature,
		finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	// The candidate builds on a copied task list: a refused validation must
	// leave the live backlog untouched.
	next := e.State.Backlog
	next.Tasks = append(append([]Task{}, next.Tasks...), followUp)
	if err := next.Validate(); err != nil {
		ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckConflict,
			Message: fmt.Sprintf("The amended requirements are invalid as a follow-up task: %v", err)}
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = []string{followUp.ID}
	// The add-task staging discipline: the retained plan keeps serving
	// in-flight neighbours while the follow-up, absent from it, holds the
	// whole-tree default until the next planning pass adds it.
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	e.State.Backlog = next
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckApplied,
		Message: fmt.Sprintf("Task %s is done and stays as accepted; the changed requirements created follow-up %s.", done.ID, followUp.ID)}
	rec.State = RequestApplied
	rec.DecidedAt = now
	rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
	rec.Summary, rec.Ack = ack.Message, &ack
	e.upsertRequest(rec)
	if err := e.export(); err != nil {
		staged.rollback(e)
		ack.Status = AckError
		ack.Message = err.Error()
		e.failStagedRequest(rec, ack, err)
		if saveErr := e.persistRequestReceipts(); saveErr != nil {
			ack.Message = errors.Join(err, saveErr).Error()
		}
		e.err = err
		return ack
	}
	id := e.identity()
	id.RequestID = req.ID
	e.eventData("amend", ack.Message, id.merge(map[string]any{"tasks": []string{followUp.ID}, "amended": done.ID, "mode": "follow-up"}))
	return ack
}

// decodeAmendedTask reads the amendment payload: exactly one task object,
// unknown fields rejected. The engine decodes the same payload again from its
// envelope, so a hand-written envelope cannot smuggle what the CLI refuses.
func decodeAmendedTask(data []byte) (Task, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return Task{}, fmt.Errorf("the amendment input is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var task Task
	if err := decoder.Decode(&task); err != nil {
		return Task{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Task{}, fmt.Errorf("expected one JSON task object")
	}
	return task, nil
}

// runAmend submits one task amendment through the controller request
// transport (AX-020). The expected revision comes from the caller's task
// file, exactly like cancel: the controller compares it against the live
// backlog, so a task that changed since the caller read it is a conflict,
// never an overwrite. The default response is the immediate receipt; --wait
// opts into one bounded wait that reports the record.
func runAmend(c Config, out, stderr io.Writer) int {
	if c.AmendTask == "" || c.AmendFile == "" {
		p := usageFailure("usage_invalid", "usage: looper amend <task-id> <task.json> [--json] [--wait]")
		p.withLegacy("usage: looper amend <task-id> <task.json> [--json] [--wait]", 2)
		return emitError(c, p, out, stderr)
	}
	data, err := os.ReadFile(c.AmendFile)
	if err != nil {
		p := usageFailure("input_unreadable", err.Error())
		p.legacyExit = exitOperationFailed
		p.Paths = []string{c.AmendFile}
		return emitError(c, p, out, stderr)
	}
	if len(data) > 48<<10 {
		p := usageFailure("input_malformed", "amendment input exceeds 48 KiB")
		p.Expected = map[string]any{"amendment_input_bytes": 48 << 10}
		p.Observed = map[string]any{"amendment_input_bytes": len(data)}
		p.Paths = []string{c.AmendFile}
		return emitError(c, p, out, stderr)
	}
	amended, err := decodeAmendedTask(data)
	if err != nil {
		p := usageFailure("input_malformed", err.Error())
		p.Paths = []string{c.AmendFile}
		return emitError(c, p, out, stderr)
	}
	if !repositoryOwned(c) {
		p := runNotFoundError("", "looper")
		p.Message = "No active run. Start Looper before amending tasks."
		p.Paths = nil
		p.withLegacy("No active run. Start Looper before amending tasks.", 1)
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		return emitError(c, controllerMetadataError(err), out, stderr)
	}
	// The expected revision comes from the caller's task file; the amendment
	// file rides the envelope beside it. Both inputs are usage-side errors:
	// nothing was attempted.
	revision, original, err := readTaskForAmend(c.Todo, c.AmendTask)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	if amended.ID == "" {
		amended.ID = c.AmendTask
	}
	if amended.ID != c.AmendTask {
		p := usageFailure("input_mismatch", fmt.Sprintf("the amendment names task %s but the command targets %s", amended.ID, c.AmendTask))
		p.Expected = map[string]any{"task_id": c.AmendTask}
		p.Observed = map[string]any{"task_id": clampString(amended.ID, maxErrorString)}
		p.Paths = []string{c.AmendFile}
		return emitError(c, p, out, stderr)
	}
	if amended.Status == "" {
		// An omitted status means "keep the current one"; the engine still
		// refuses a status that disagrees with the live task.
		amended.Status = original.Status
	} else if amended.Status != original.Status {
		p := usageFailure("status_not_amendable", fmt.Sprintf("an amendment does not change status: %s is %s in %s", c.AmendTask, original.Status, c.Todo))
		p.Expected = map[string]any{"status": original.Status}
		p.Observed = map[string]any{"status": clampString(amended.Status, maxErrorString)}
		p.Paths = []string{c.AmendFile, c.Todo}
		p.Actions = []ErrorAction{{Description: "Cancel open work instead of amending its status.", Command: "looper cancel " + c.AmendTask}}
		return emitError(c, p, out, stderr)
	}
	if changes := controlTagChanges(original, amended); len(changes) > 0 {
		p := operationFailure("control_tags_not_amendable", fmt.Sprintf("an amendment cannot change control tags: it %s", strings.Join(changes, ", ")))
		p.Retry = retryNo
		p.Observed = map[string]any{"control_tag_changes": clampStrings(changes, maxErrorString)}
		p.Actions = []ErrorAction{{Description: "Edit the task file itself; Looper imports the edit at a safe boundary.", Command: c.Todo}}
		return emitError(c, p, out, stderr)
	}
	payloadDigest := requestPayloadDigest(&SettingsRequest{Type: RequestTypeAmend, TaskID: c.AmendTask, TaskRevision: revision, Tasks: []Task{amended}}, nil)
	// A caller key answers from the retained receipts before any envelope is
	// published (AX-012).
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestAmendTask, c.IdempotencyKey, payloadDigest, out, stderr); handled {
		return code
	}
	envelope := SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeAmend, Session: info.Session,
		TaskID: c.AmendTask, TaskRevision: revision, Tasks: []Task{amended}, IdempotencyKey: c.IdempotencyKey}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, envelope, c.waitBound())
		return reportAmendAck(c, ack, err, out, stderr)
	}
	req := envelope
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	note := ""
	if mutationStoreExists(dir) {
		note = heldInputNote(c, dir)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestAmendTask, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false),
		fmt.Sprintf("Submitted amendment of %s.", c.AmendTask), note, out)
}

// clampStrings bounds every entry of a bounded string list.
func clampStrings(values []string, max int) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, clampString(v, max))
	}
	return out
}

// readTaskForAmend reads the caller's task file and returns the expected
// revision of one task together with the task itself, so the CLI can compare
// the amendment against the contract the caller actually read. A file the
// caller cannot serve is a usage failure, because nothing was attempted.
func readTaskForAmend(todo, taskID string) (string, Task, error) {
	data, err := os.ReadFile(todo)
	if err != nil {
		p := usageFailure("input_unreadable", err.Error())
		p.legacyExit = exitOperationFailed
		p.Paths = []string{todo}
		return "", Task{}, p
	}
	b, err := parseBacklogStrict(data)
	if err != nil {
		p := usageFailure("input_malformed", fmt.Sprintf("%s is not valid JSON: %s", todo, err.Error()))
		p.Paths = []string{todo}
		return "", Task{}, p
	}
	if err = b.Validate(); err != nil {
		p := usageFailure("input_malformed", fmt.Sprintf("%s has invalid content: %s", todo, err.Error()))
		p.Paths = []string{todo}
		return "", Task{}, p
	}
	t := b.Task(taskID)
	if t == nil {
		p := operationFailure("task_not_found", fmt.Sprintf("Task %s is not in %s. List the tasks with: looper status --ls todo", taskID, todo))
		p.Retry = retryNo
		p.Observed = map[string]any{"task_id": clampString(taskID, maxErrorString)}
		p.Paths = []string{todo}
		p.Actions = []ErrorAction{{Description: "Read the current tasks before amending.", Command: "looper status --ls todo"}}
		p.withLegacy(fmt.Sprintf("No task %s in %s.", taskID, todo), 1)
		return "", Task{}, p
	}
	return hashJSON(*t), *t, nil
}

// reportAmendAck renders the acknowledgement one amend --wait received.
// Version 2 reports the request record; the legacy contract keeps the shapes
// it printed before, and a refusal or conflict exits nonzero.
func reportAmendAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != AckApplied {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, amendAckRecordView(ack), ack.ID, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(ack)
		if ack.Status == AckApplied {
			return 0
		}
		return 1
	}
	switch ack.Status {
	case AckApplied:
		fmt.Fprintf(out, "Amend request %s: %s\n", ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(out, " ", ack.Message)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "Amend request %s: %s\n", ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(stderr, " ", ack.Message)
		}
		return 1
	}
}

// amendAckRecordView maps one terminal amend acknowledgement to its record
// view, so a --wait that outlived the store read still reports the decision
// it observed.
func amendAckRecordView(ack *SettingsAck) *requestQueryRecord {
	state, summary := RequestRejected, ack.Message
	if ack.Status == AckApplied {
		state, summary = RequestApplied, "applied: "+ack.Message
	}
	q := ackRecordView(ack.ID, PublicRequestAmendTask, state, summary, nil)
	q.errText = ack.Message
	return q
}
