package looper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const RequestTypeAddTask = "add-task"

// decodeNewTasks accepts one task or a batch. Unknown fields are errors.
func decodeNewTasks(data []byte) ([]Task, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, fmt.Errorf("the task input is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var tasks []Task
	if data[0] == '[' {
		if err := decoder.Decode(&tasks); err != nil {
			return nil, err
		}
	} else {
		var task Task
		if err := decoder.Decode(&task); err != nil {
			return nil, err
		}
		tasks = []Task{task}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON value")
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("supply at least one task")
	}
	for i := range tasks {
		if tasks[i].Status == "" {
			tasks[i].Status = "todo"
		}
		if tasks[i].Status != "todo" && tasks[i].Status != "blocked" {
			return nil, fmt.Errorf("new task %s must be todo or blocked", tasks[i].ID)
		}
	}
	return tasks, nil
}

// runAddTask submits new tasks through the controller request transport.
// Input rejections are usage failures because nothing was attempted; a run
// or controller that cannot be read is unavailable; refusals from the
// controller keep their request identity as evidence. The default response
// is the immediate receipt (AX-011): the envelope is durable and the caller
// queries the record; --wait opts into one bounded wait for the applied
// acknowledgement.
func runAddTask(c Config, out, stderr io.Writer) int {
	if len(c.Plans) != 1 {
		p := usageFailure("usage_invalid", "usage: looper add-task <file.json> [--json] [--wait]")
		p.withLegacy("usage: looper add-task <file.json> [--json] [--wait]", 2)
		return emitError(c, p, out, stderr)
	}
	data, err := os.ReadFile(c.Plans[0])
	if err != nil {
		p := usageFailure("input_unreadable", err.Error())
		// The legacy contract reported a missing input from the read step.
		p.legacyExit = exitOperationFailed
		p.Paths = []string{c.Plans[0]}
		return emitError(c, p, out, stderr)
	}
	if len(data) > 48<<10 {
		p := usageFailure("input_malformed", "task input exceeds 48 KiB")
		p.Expected = map[string]any{"task_input_bytes": 48 << 10}
		p.Observed = map[string]any{"task_input_bytes": len(data)}
		p.Paths = []string{c.Plans[0]}
		return emitError(c, p, out, stderr)
	}
	tasks, err := decodeNewTasks(data)
	if err != nil {
		// The decoder names the structural problem; the file contents stay
		// out of the message.
		p := usageFailure("input_malformed", err.Error())
		p.Paths = []string{c.Plans[0]}
		return emitError(c, p, out, stderr)
	}
	if !repositoryOwned(c) {
		p := runNotFoundError("", "looper")
		p.Message = "No active run. Start Looper before adding tasks."
		p.Paths = nil
		p.withLegacy("No active run. Start Looper before adding tasks.", 1)
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		return emitError(c, controllerMetadataError(err), out, stderr)
	}
	// A caller key answers from the retained receipts before any envelope is
	// published: the same key and tasks returns the original request, and a
	// changed task batch under the same key is a conflict (AX-012).
	payloadDigest := requestPayloadDigest(&SettingsRequest{Type: RequestTypeAddTask, Tasks: tasks}, nil)
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestAddTask, c.IdempotencyKey, payloadDigest, out, stderr); handled {
		return code
	}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeAddTask, Session: info.Session, Tasks: tasks, IdempotencyKey: c.IdempotencyKey}, c.waitBound())
		return reportAddTaskAck(c, ack, err, out, stderr)
	}
	req := SettingsRequest{Version: SettingsRequestVersion, Type: RequestTypeAddTask, Session: info.Session, Tasks: tasks, IdempotencyKey: c.IdempotencyKey}
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	note := ""
	if mutationStoreExists(dir) {
		note = heldInputNote(c, dir)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestAddTask, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false),
		fmt.Sprintf("Submitted %d task(s).", len(tasks)), note, out)
}

// reportAddTaskAck renders the acknowledgement one --wait received. Version
// 2 reports the request record — the added task identifiers ride beside the
// lifecycle state; the legacy contract keeps the shapes it printed before
// AX-011, now behind the explicit wait.
func reportAddTaskAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		// A wait timeout reports a still-running request; the legacy JSON
		// path keeps printing nothing but stderr, exactly as before.
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != AckApplied {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, settingsAckRecordView(ack, PublicRequestAddTask), ack.ID, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(ack)
		if ack.Status == AckApplied {
			return 0
		}
		return 1
	}
	if ack.Status != AckApplied {
		fmt.Fprintln(stderr, ack.Status+": "+ack.Message)
		return 1
	}
	fmt.Fprintln(out, ack.Message)
	return 0
}

func (e *Engine) processAddTaskRequests() {
	for {
		req, err := e.claimRequest(RequestTypeAddTask)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyAddTaskRequest(req)
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

// stagedBacklogRequest snapshots one backlog-mutating request application —
// add-task, cancel, and requeue share it: the backlog fields it mutates, the
// attempt phases it retires, and the receipt queue it appends to. Rollback
// restores all of them when the saved state/export protocol fails before
// committing the application, so a failed request leaves neither an
// in-memory-only task nor a staged receipt, mirroring the findingStage and
// stagedImport patterns (AX-009).
type stagedBacklogRequest struct {
	backlog       Backlog
	planSignature string
	finished      bool
	reviewedTip   string
	attempts      []*Attempt
	attemptPhases []string
	requests      []*RequestRecord
}

func (s stagedBacklogRequest) rollback(e *Engine) {
	e.State.Backlog = s.backlog
	e.State.PlanSignature = s.planSignature
	e.State.Finished = s.finished
	e.State.ReviewedTip = s.reviewedTip
	e.State.Attempts = s.attempts
	for i, a := range s.attempts {
		if i < len(s.attemptPhases) {
			a.Phase = s.attemptPhases[i]
		}
	}
	e.State.Requests = s.requests
}

// replayedAck answers a request whose durable receipt already holds a
// terminal outcome. A controller that restarts between the export commit and
// the acknowledgement write replays the recorded answer instead of applying
// the mutation twice.
func (e *Engine) replayedAck(req *SettingsRequest) (SettingsAck, bool) {
	if rec := e.requestReceipt(req.ID); rec != nil && rec.Ack != nil && requestStateTerminal(rec.State) {
		return *rec.Ack, true
	}
	return SettingsAck{}, false
}

func (e *Engine) applyAddTaskRequest(req *SettingsRequest) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if replayed, ok := e.replayedAck(req); ok {
		// A durable receipt already decided this identifier: return the
		// recorded acknowledgement without applying the tasks again.
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	// A caller key answers from the receipt that recorded it, before any
	// session check: a retry after a controller restart targets a new session
	// and must still return the original outcome, never a second mutation
	// (AX-012).
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
	if req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Resubmit the tasks."
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	if e.inputPending {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	// Equivalent formatting is not a change (AX-018): only an edit that
	// changed content makes the request wait for its boundary import.
	if edit := e.classifyBacklogFile(); edit.Changed && !edit.Equivalent {
		ack.Status = AckConflict
		ack.Message = "The task file changed. Wait for Looper to import the edit."
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	raw, err := json.Marshal(req.Tasks)
	if err != nil {
		ack.Message = err.Error()
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	tasks, err := decodeNewTasks(raw)
	if err != nil {
		ack.Message = err.Error()
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	next := e.State.Backlog
	next.Tasks = append(append([]Task{}, next.Tasks...), tasks...)
	if err = next.Validate(); err != nil {
		ack.Message = err.Error()
		e.rejectRequestReceipt(req, ack)
		return ack
	}
	// The application and its receipt stage together until the saved
	// state/export protocol commits them. The receipt rides the same saves
	// export() makes, so a crash between the export commit and the
	// acknowledgement write leaves an applied receipt a restart replays
	// instead of applying the tasks twice.
	staged := stagedBacklogRequest{
		backlog: clone(e.State.Backlog), planSignature: e.State.PlanSignature,
		finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	rec := e.beginRequestReceipt(req)
	rec.AffectedTasks = taskIDs(tasks)
	e.State.Backlog = next
	// A request can apply while workers run, and the dispatch loop reads
	// scopes from the live plan on every tick. Keep it and invalidate only
	// the signature: the next planning pass rebuilds with the added tasks,
	// and a task missing from the retained plan scopes to the whole tree,
	// so parallel dispatch stays blocked until then.
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	ack.Status = AckApplied
	ack.Message = fmt.Sprintf("Added %d task(s). Active work continues.", len(tasks))
	rec.State = RequestApplied
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Summary, rec.Ack = ack.Message, &ack
	if err = e.export(); err != nil {
		// Roll the staged application back and persist the rollback with a
		// failed receipt before the poisoned engine exits: the export's own
		// journaling save already holds the staged backlog and receipt, and
		// without this persist the next start would replay those tasks into
		// the file under a request this controller answered with an error.
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
	e.rememberAck(req.ID, ack)
	// The added task identities ride beside the request identity: one request
	// mutates many tasks, so the payload lists them instead of naming one.
	added := e.identity()
	added.RequestID = req.ID
	data := map[string]any{}
	if ids := taskIDs(req.Tasks); len(ids) > 0 {
		data["tasks"] = ids
	}
	e.eventData("add-task", ack.Message, added.merge(data))
	return ack
}

// taskIDs lists the identifiers of a task batch.
func taskIDs(tasks []Task) []string {
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.ID != "" {
			ids = append(ids, t.ID)
		}
	}
	return ids
}
