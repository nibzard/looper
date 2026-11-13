package looper

import (
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

// Task cancellation through checked controller requests (AX-019). A cancel
// request names one open task and the revision of that task the caller read.
// The controller refuses a changed task, a held task-file edit, and done or
// already-cancelled work; a task with live dependents is refused naming them
// unless the caller cascades. Cancelled work is never encoded as done: the
// status is its own terminal representation, and retained results stay
// recorded beside their attempts for inspection.
//
// Two phases exist. An active cancellation — no target work in flight —
// applies at the boundary that claims it. A pending cancellation — an
// attempt of the target set is running, verifying, or retained — is decided
// durably (a waiting receipt plus a PendingCancellation in the saved state)
// and applies at the first boundary where the work drained. A task that
// completed before that boundary keeps its accepted result; the cancellation
// then records that outcome instead of discarding it.

// cancellableStatus reports whether one task may still be cancelled: open
// work in todo, doing, or blocked. Done work is accepted history and
// cancelled work already decided.
func cancellableStatus(t *Task) bool {
	return t.Status == "todo" || t.Status == "doing" || t.Status == "blocked"
}

// liveDependents lists, transitively and in stable order, the tasks that
// depend on id and have not finished: not done and not already cancelled. A
// cancelled dependency strands such tasks — they can never become ready — so
// a cancellation must either take them along or name them in a refusal.
func liveDependents(b Backlog, id string) []string {
	successors := map[string][]string{}
	for _, t := range b.Tasks {
		if t.Status == "done" || t.Status == "cancelled" || t.ID == id {
			continue
		}
		for _, dep := range t.DependsOn {
			successors[dep] = append(successors[dep], t.ID)
		}
	}
	var out []string
	seen := map[string]bool{}
	queue := append([]string{}, successors[id]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		out = append(out, next)
		queue = append(queue, successors[next]...)
	}
	sort.Strings(out)
	return out
}

// boundedIDList renders a task identifier list with an explicit bound, so a
// wide dependency fan-out cannot overflow a refusal message.
func boundedIDList(ids []string, max int) string {
	if len(ids) <= max {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:max], ", ") + fmt.Sprintf(" (+%d more)", len(ids)-max)
}

// pendingCancelTask reports whether one task sits in a decided cancellation
// that still waits for active work. Dispatch skips it: launching new work
// for a task the controller already decided to cancel would only produce a
// result the cancellation then has to preserve.
func (e *Engine) pendingCancelTask(id string) bool {
	for _, pc := range e.State.Cancellations {
		if pc == nil {
			continue
		}
		for _, tid := range pc.TaskIDs {
			if tid == id {
				return true
			}
		}
	}
	return false
}

// cancelBlockingAttempt reports whether one attempt owns work a decided
// cancellation must let finish: a live worker, a check in flight, or a
// retained result waiting on its publication. A resume attempt owns no
// result and no worker — it is restartable retained work, so cancelling its
// task retires it instead of waiting for a relaunch the decision itself
// prevents.
func cancelBlockingAttempt(a *Attempt) bool {
	switch a.Phase {
	case "done", "blocked", "cancelled", "resume":
		return false
	}
	return true
}

// cancellationActive reports whether any task in ids holds a
// blocking attempt. Those attempts own results the cancellation must let
// finish.
func (e *Engine) cancellationActive(ids []string) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	for _, a := range e.State.Attempts {
		if a != nil && set[a.Task.ID] && cancelBlockingAttempt(a) {
			return true
		}
	}
	return false
}

// cancelBoundaryHeld reports whether a held task-file edit or an input wait
// blocks every backlog mutation at this boundary. A cancellation exports the
// backlog, exactly as add-task and requeue do, so it waits for the same
// imports instead of colliding with the edit at the export guard.
func (e *Engine) cancelBoundaryHeld() bool {
	if e.inputPending {
		return true
	}
	if edit := e.classifyBacklogFile(); edit.Changed && !edit.Equivalent {
		return true
	}
	return false
}

func (e *Engine) processCancelRequests() {
	if e.controller == nil {
		return
	}
	for {
		req, err := e.claimRequest(RequestTypeCancel)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyCancelRequest(req)
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

// applyCancelRequest validates and decides one claimed cancellation. The
// guards mirror requeue: replay by identifier or caller key, the session
// check, the held-edit check, and the expected revision. The decision is one
// of three outcomes — applied at once, waiting for active work, or refused —
// and every outcome records its durable receipt.
func (e *Engine) applyCancelRequest(req *SettingsRequest) SettingsAck {
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
	// never cancels twice.
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
	if req.TaskID == "" {
		ack.Status = AckError
		ack.Message = "the request names no task"
		return refuse()
	}
	if req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Reload the task before cancelling."
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
		ack.Message = fmt.Sprintf("Task %s is not in the backlog. Inspect the current tasks before cancelling.", req.TaskID)
		return refuse()
	}
	if t.Status == "done" {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Task %s is done. Accepted work stays; cancel open work instead.", req.TaskID)
		return refuse()
	}
	if t.Status == "cancelled" {
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Task %s is already cancelled.", req.TaskID)
		return refuse()
	}
	if req.TaskRevision != hashJSON(*t) {
		ack.Status = AckConflict
		ack.Message = "The task changed. Inspect its current state before cancelling."
		return refuse()
	}
	ids := []string{req.TaskID}
	if dependents := liveDependents(e.State.Backlog, req.TaskID); len(dependents) > 0 {
		if !req.Cascade {
			ack.Status = AckConflict
			ack.Message = fmt.Sprintf("Task %s has live dependents: %s. Cancel them too with --cascade, or cancel them first.", req.TaskID, boundedIDList(dependents, 8))
			return refuse()
		}
		ids = append(ids, dependents...)
	}
	if e.cancellationActive(ids) {
		ack = e.recordPendingCancellation(req, ids)
		return ack
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = ids
	ack = e.commitCancellation(rec, ids)
	return ack
}

// recordPendingCancellation decides a cancellation whose target work is in
// flight. The decision and its waiting receipt commit in one save, so a
// restart that interrupts the wait keeps the decision and drives it at the
// first drained boundary. A decision this request already recorded keeps
// its answer: a claim replayed after a crash between the save and the
// acknowledgement write must not record the decision twice.
func (e *Engine) recordPendingCancellation(req *SettingsRequest, ids []string) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	for _, pc := range e.State.Cancellations {
		if pc != nil && pc.RequestID == req.ID {
			ack.Status = AckWaiting
			ack.Message = "The cancellation is recorded. It applies when the active work finishes; the results of that work are preserved."
			return ack
		}
	}
	pc := &PendingCancellation{
		RequestID: req.ID, TaskIDs: ids,
		DecidedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = ids
	e.State.Cancellations = append(e.State.Cancellations, pc)
	e.waitRequestReceipt(rec, "active_work",
		fmt.Sprintf("%s and its decided set cancel when the active work finishes", req.TaskID))
	if err := e.save(); err != nil {
		// Roll the staged decision back and record the failure before the
		// poisoned engine exits, mirroring the add-task rollback.
		e.dropPendingCancellation(req.ID)
		ack.Message = err.Error()
		e.failStagedRequest(rec, ack, err)
		if saveErr := e.persistRequestReceipts(); saveErr != nil {
			ack.Message = errors.Join(err, saveErr).Error()
		}
		e.err = err
		return ack
	}
	ack.Status = AckWaiting
	ack.Message = "The cancellation is recorded. It applies when the active work finishes; the results of that work are preserved."
	id := e.identity()
	id.RequestID = req.ID
	e.eventData("cancel-pending", fmt.Sprintf("Cancellation of %s waits for active work; %d task(s) in the decided set.", req.TaskID, len(ids)), id.merge(map[string]any{"tasks": ids}))
	return ack
}

// dropPendingCancellation removes one decided cancellation by request id.
func (e *Engine) dropPendingCancellation(requestID string) {
	kept := make([]*PendingCancellation, 0, len(e.State.Cancellations))
	for _, pc := range e.State.Cancellations {
		if pc == nil || pc.RequestID != requestID {
			kept = append(kept, pc)
		}
	}
	e.State.Cancellations = kept
}

// drivePendingCancellations applies decided cancellations whose target work
// has drained. It runs at every loop boundary before new requests are
// claimed, so a restart or a busy boundary never loses the decision. A held
// task-file edit defers the drive exactly as it refuses fresh requests: the
// edit imports first, then the cancellation applies against the imported
// backlog.
func (e *Engine) drivePendingCancellations() {
	if e.State == nil || len(e.State.Cancellations) == 0 {
		return
	}
	if e.cancelBoundaryHeld() {
		return
	}
	for _, pc := range append([]*PendingCancellation{}, e.State.Cancellations...) {
		if pc == nil {
			continue
		}
		if rec := e.requestReceipt(pc.RequestID); rec != nil && requestStateTerminal(rec.State) {
			// The receipt already decided — for example a failed export
			// poisoned the run after it recorded the failure. The decision
			// never re-decides; the stale entry goes.
			e.dropPendingCancellation(pc.RequestID)
			continue
		}
		if e.cancellationActive(pc.TaskIDs) {
			continue
		}
		rec := e.requestReceipt(pc.RequestID)
		if rec == nil {
			// The receipt never survived, but the decided set did: rebuild a
			// minimal receipt so the outcome stays queryable.
			rec = &RequestRecord{ID: pc.RequestID, Type: PublicRequestCancel, Session: e.controllerSessionID(), SubmittedAt: pc.DecidedAt}
		}
		e.commitCancellation(rec, pc.TaskIDs)
		if e.err != nil {
			return
		}
	}
}

// commitCancellation applies one decided cancellation at a boundary where no
// target work is active. The application stages with the backlog mutation
// protocol add-task and requeue share: the export commits the mutated
// statuses beside the receipt, and a failed export rolls both back with a
// failed receipt. Tasks that completed or vanished before the boundary keep
// their outcome — an accepted result is never discarded — and are named in
// the recorded summary.
func (e *Engine) commitCancellation(rec *RequestRecord, ids []string) SettingsAck {
	if requestStateTerminal(rec.State) {
		// A decided request never re-decides: return the recorded outcome,
		// whatever reached this commit a second time.
		if rec.Ack != nil {
			return *rec.Ack
		}
		return SettingsAck{Version: SettingsRequestVersion, ID: rec.ID, Session: e.controllerSessionID(), Status: AckError,
			Message: fmt.Sprintf("the request is already %s: %s", rec.State, orDefault(rec.Error, rec.Summary))}
	}
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
	var cancelled, completed, missing []string
	cancelledSet := map[string]bool{}
	for _, id := range ids {
		t := e.State.Backlog.Task(id)
		if t == nil {
			missing = append(missing, id)
			continue
		}
		if !cancellableStatus(t) {
			completed = append(completed, id)
			continue
		}
		t.Status = "cancelled"
		t.UpdatedAt = now
		cancelled = append(cancelled, id)
		cancelledSet[id] = true
	}
	// A cancelled task's resume attempts retire with it: they hold no result
	// and no worker, and a phase that recovery would re-drive would resurrect
	// cancelled work at the next start. Done and blocked attempts keep their
	// recorded results for inspection.
	for _, a := range e.State.Attempts {
		if a != nil && cancelledSet[a.Task.ID] && a.Phase == "resume" {
			a.Phase = "cancelled"
		}
	}
	// Retained results stay beside their attempts: done and blocked attempts
	// keep their recorded commit and summary for inspection, and only the
	// result-less resume attempts retired above changed phase.
	ack := SettingsAck{Version: SettingsRequestVersion, ID: rec.ID, Session: e.controllerSessionID()}
	if len(cancelled) == 0 {
		// Nothing is left to cancel. The decision cannot apply, and the
		// completed work keeps its accepted results.
		e.dropPendingCancellation(rec.ID)
		ack.Status = AckConflict
		ack.Message = fmt.Sprintf("Nothing was cancelled: %s completed or left the backlog before the cancellation applied; accepted results stay.", strings.Join(boundedIDsOrNone(completed, missing), ", "))
		e.decideRequestReceipt(rec, RequestRejected, "", ack.Message)
		rec.Ack = &ack
		if err := e.persistRequestReceipts(); err != nil && e.err == nil {
			e.err = err
		}
		id := e.identity()
		id.RequestID = rec.ID
		e.eventData("cancel", ack.Message, id.merge(nil))
		return ack
	}
	// A status-only mutation keeps the live plan for in-flight neighbours
	// and clears only its signature, exactly as requeue does.
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	ack.Status = AckApplied
	ack.Message = cancelOutcomeMessage(cancelled, completed, missing)
	rec.State = RequestApplied
	rec.DecidedAt = now
	rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
	rec.Summary, rec.Ack = ack.Message, &ack
	// The receipt commits in the same save as the statuses: a drive whose
	// receipt had to be rebuilt lands it here, and a request-path receipt is
	// replaced by its decided form.
	e.upsertRequest(rec)
	if err := e.export(); err != nil {
		// Roll the staged application back and persist the rollback with a
		// failed receipt before the poisoned engine exits, mirroring
		// add-task and requeue.
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
	e.dropPendingCancellation(rec.ID)
	id := e.identity()
	id.RequestID = rec.ID
	data := map[string]any{"tasks": cancelled}
	if len(completed) > 0 {
		data["completed_first"] = completed
	}
	e.eventData("cancel", ack.Message, id.merge(data))
	return ack
}

// boundedIDsOrNone names the tasks a cancellation did not cancel, or states
// that none existed.
func boundedIDsOrNone(completed, missing []string) []string {
	var out []string
	for _, id := range completed {
		out = append(out, id+" (completed)")
	}
	for _, id := range missing {
		out = append(out, id+" (left the backlog)")
	}
	if out == nil {
		out = []string{"no task of the decided set"}
	}
	return out
}

// cancelOutcomeMessage renders one applied cancellation's outcome, naming
// the tasks that cancelled and the ones whose earlier completion or removal
// the cancellation preserved instead.
func cancelOutcomeMessage(cancelled, completed, missing []string) string {
	message := fmt.Sprintf("Cancelled %s.", strings.Join(cancelled, ", "))
	if len(completed) > 0 || len(missing) > 0 {
		message += " Kept as they were: " + strings.Join(boundedIDsOrNone(completed, missing), ", ") + "."
	}
	return message
}

// runCancel submits a task cancellation through the controller request
// transport (AX-019). The expected revision comes from the caller's task
// file: the controller compares it against the live backlog, so a task that
// changed since the caller read it is a conflict, never an overwrite. The
// default response is the immediate receipt; --wait opts into one bounded
// wait that reports the record — waiting on active work or applied.
func runCancel(c Config, out, stderr io.Writer) int {
	if c.CancelTask == "" {
		p := usageFailure("usage_invalid", "usage: looper cancel <task-id> [--cascade] [--wait]")
		p.withLegacy("usage: looper cancel <task-id> [--cascade] [--wait]", 2)
		return emitError(c, p, out, stderr)
	}
	if !repositoryOwned(c) {
		p := runNotFoundError("", "looper")
		p.Message = "No active run. Start Looper before cancelling tasks."
		p.Paths = nil
		p.withLegacy("No active run. Start Looper before cancelling tasks.", 1)
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		return emitError(c, controllerMetadataError(err), out, stderr)
	}
	revision, err := readTaskRevision(c.Todo, c.CancelTask)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	payloadDigest := requestPayloadDigest(&SettingsRequest{Type: RequestTypeCancel, TaskID: c.CancelTask, TaskRevision: revision, Cascade: c.Cascade}, nil)
	// A caller key answers from the retained receipts before any envelope is
	// published (AX-012).
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestCancel, c.IdempotencyKey, payloadDigest, out, stderr); handled {
		return code
	}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeCancel, Session: info.Session, TaskID: c.CancelTask, TaskRevision: revision, Cascade: c.Cascade, IdempotencyKey: c.IdempotencyKey}, c.waitBound())
		return reportCancelAck(c, ack, err, out, stderr)
	}
	req := SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeCancel, Session: info.Session, TaskID: c.CancelTask, TaskRevision: revision, Cascade: c.Cascade, IdempotencyKey: c.IdempotencyKey}
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	note := ""
	if mutationStoreExists(dir) {
		note = heldInputNote(c, dir)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestCancel, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false),
		fmt.Sprintf("Submitted cancellation of %s.", c.CancelTask), note, out)
}

// readTaskRevision reads the caller's task file and returns the expected
// revision of one task: the digest the controller compares against the live
// backlog. A file the caller cannot serve is a usage failure, because
// nothing was attempted.
func readTaskRevision(todo, taskID string) (string, error) {
	data, err := os.ReadFile(todo)
	if err != nil {
		p := usageFailure("input_unreadable", err.Error())
		p.legacyExit = exitOperationFailed
		p.Paths = []string{todo}
		return "", p
	}
	b, err := parseBacklogStrict(data)
	if err != nil {
		p := usageFailure("input_malformed", fmt.Sprintf("%s is not valid JSON: %s", todo, err.Error()))
		p.Paths = []string{todo}
		return "", p
	}
	if err = b.Validate(); err != nil {
		p := usageFailure("input_malformed", fmt.Sprintf("%s has invalid content: %s", todo, err.Error()))
		p.Paths = []string{todo}
		return "", p
	}
	t := b.Task(taskID)
	if t == nil {
		p := operationFailure("task_not_found", fmt.Sprintf("Task %s is not in %s. List the tasks with: looper status --ls todo", taskID, todo))
		p.Retry = retryNo
		p.Observed = map[string]any{"task_id": clampString(taskID, maxErrorString)}
		p.Paths = []string{todo}
		p.Actions = []ErrorAction{{Description: "Read the current tasks before cancelling.", Command: "looper status --ls todo"}}
		p.withLegacy(fmt.Sprintf("No task %s in %s.", taskID, todo), 1)
		return "", p
	}
	return hashJSON(*t), nil
}

// reportCancelAck renders the acknowledgement one cancel --wait received.
// Waiting is a recorded decision, not a failure: version 2 reports the
// request record — waiting on active work or applied — and both contracts
// exit 0 for it.
func reportCancelAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != AckApplied && ack.Status != AckWaiting {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, cancelAckRecordView(ack), ack.ID, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(ack)
		if ack.Status == AckApplied || ack.Status == AckWaiting {
			return 0
		}
		return 1
	}
	switch ack.Status {
	case AckApplied, AckWaiting:
		fmt.Fprintf(out, "Cancel request %s: %s\n", ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(out, " ", ack.Message)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "Cancel request %s: %s\n", ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(stderr, " ", ack.Message)
		}
		return 1
	}
}

// cancelAckRecordView maps one terminal or waiting cancel acknowledgement to
// its record view, so a --wait that outlived the store read still reports
// the decision it observed.
func cancelAckRecordView(ack *SettingsAck) *requestQueryRecord {
	state, summary := RequestRejected, ack.Message
	switch ack.Status {
	case AckApplied:
		state, summary = RequestApplied, "applied: "+ack.Message
	case AckWaiting:
		state = RequestWaiting
	}
	q := ackRecordView(ack.ID, PublicRequestCancel, state, summary, nil)
	q.errText = ack.Message
	return q
}
