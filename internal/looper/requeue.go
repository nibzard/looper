package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Requeue returns one blocked task to todo. The ordinary request keeps the
// retained partial implementation for reuse. The fresh request
// (requeue-fresh) retires every reusable attempt as evidence, so the next
// dispatch creates a new attempt under the current worker role. The preview
// reports the retirement read-only and changes nothing.

// runRequeue serves the requeue command: preview, live submission, and
// stopped-run application.
func runRequeue(c Config, out, stderr io.Writer) int {
	revision, err := readTaskRevision(c.Todo, c.RequeueTask)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	attemptsRevision := readAttemptSetRevision(c, c.RequeueTask)
	if c.Preview {
		return runRequeuePreview(c, out, stderr)
	}
	if repositoryOwned(c) {
		return runRequeueOnline(c, revision, attemptsRevision, out, stderr)
	}
	return runRequeueStopped(c, revision, attemptsRevision, out, stderr)
}

// readAttemptSetRevision reads the attempt-set revision of one task from the
// saved run, read-only. An unreadable store reports the empty string: the
// controller then checks only the task revision, exactly like the callers
// that never sent an attempt revision.
func readAttemptSetRevision(c Config, taskID string) string {
	_, dir := projectPaths(c)
	store, err := openObservedStore(dir)
	if err != nil {
		return ""
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil || snap.State == nil {
		return ""
	}
	return attemptSetRevision(snap.State, taskID)
}

// requeuePreviewEvidence is the read-only retirement report.
type requeuePreviewEvidence struct {
	s        *State
	task     *Task
	attempts []*Attempt
	retiring []*Attempt
	reusing  []*Attempt
}

func collectRequeuePreview(c Config) (*requeuePreviewEvidence, error) {
	_, dir := projectPaths(c)
	store, err := openObservedStore(dir)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		return nil, err
	}
	if snap.State == nil {
		return nil, &recoveryRefusal{message: "the store holds no saved state yet"}
	}
	ev := &requeuePreviewEvidence{s: snap.State}
	ev.task = snap.State.Backlog.Task(c.RequeueTask)
	if ev.task == nil {
		return nil, &recoveryRefusal{message: fmt.Sprintf("task %s is not in the saved backlog", c.RequeueTask)}
	}
	for _, a := range snap.State.Attempts {
		if a.Task.ID != c.RequeueTask {
			continue
		}
		ev.attempts = append(ev.attempts, a)
		switch {
		case a.Phase == "done" || a.Phase == "cancelled":
			// History stays as it is.
		case attemptRetired(a):
			// Already retired evidence.
		case c.RequeueFresh:
			ev.retiring = append(ev.retiring, a)
		default:
			ev.reusing = append(ev.reusing, a)
		}
	}
	return ev, nil
}

// runRequeuePreview reports what one requeue would do. It reads the saved
// run and changes nothing.
func runRequeuePreview(c Config, out, stderr io.Writer) int {
	if c.JSON {
		p := usageFailure("usage_invalid", "the requeue preview is a report; request it as text without --json")
		p.withLegacy("usage: looper requeue <task-id> [--fresh] --preview", 2)
		return emitError(c, p, out, stderr)
	}
	ev, err := collectRequeuePreview(c)
	if err != nil {
		var refusal *recoveryRefusal
		if errors.As(err, &refusal) {
			p := operationFailure("requeue_unavailable", refusal.message)
			p.Retry = retryNo
			return emitError(c, p, out, stderr)
		}
		_, dir := projectPaths(c)
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	mode := "requeue (reuse retained work)"
	if c.RequeueFresh {
		mode = "requeue --fresh (retire retained work)"
	}
	line("Requeue preview for %s — %s (read-only; nothing was changed)", c.RequeueTask, mode)
	line("Task status: %s | dependencies: %s", ev.task.Status, dependencySummary(ev.task))
	line("History: %d attempt(s) recorded for this task", len(ev.attempts))
	for _, a := range ev.attempts {
		marker := "kept as history"
		switch {
		case containsAttempt(ev.retiring, a):
			marker = "retired as evidence"
		case containsAttempt(ev.reusing, a):
			marker = "reused on the next dispatch"
		case attemptRetired(a):
			marker = "already retired (" + orDefault(a.Retired.Reason, "?") + ")"
		}
		line("  %s: phase %s — %s", a.ID, a.Phase, marker)
	}
	if c.RequeueFresh {
		if len(ev.retiring) == 0 {
			line("No reusable attempt to retire; the next dispatch starts clean either way.")
		} else {
			line("Retirement preserves every artifact, result commit, and history entry;")
			line("the attempts are never resumed, reused, or re-entered.")
		}
		line("The next dispatch uses the current worker role: %s", c.Agent)
	} else if len(ev.reusing) == 0 {
		line("No retained implementation to reuse; the next dispatch starts clean.")
	} else {
		line("The next dispatch resumes the newest retained attempt with its worktree.")
	}
	line("Task identity, contract, dependencies, and history stay untouched.")
	return 0
}

func containsAttempt(list []*Attempt, want *Attempt) bool {
	for _, a := range list {
		if a == want {
			return true
		}
	}
	return false
}

func dependencySummary(t *Task) string {
	if len(t.DependsOn) == 0 {
		return "none"
	}
	return fmt.Sprintf("%s", t.DependsOn)
}

// runRequeueOnline submits the requeue to the live controller through the
// request protocol. A controller that does not advertise requeue-fresh is
// refused before any envelope is published, so an older controller never
// queues a request it would answer as an ordinary requeue.
func runRequeueOnline(c Config, revision, attemptsRevision string, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		return emitError(c, controllerMetadataError(err), out, stderr)
	}
	reqType, public := RequestTypeRequeue, PublicRequestRequeue
	if c.RequeueFresh {
		reqType, public = RequestTypeRequeueFresh, PublicRequestRequeueFresh
		if err := validateControllerRequest(info, public); err != nil {
			return emitError(c, requestSubmitError(err), out, stderr)
		}
	}
	keyed := &SettingsRequest{Type: reqType, TaskID: c.RequeueTask, TaskRevision: revision, AttemptsRevision: attemptsRevision}
	payloadDigest := requestPayloadDigest(keyed, nil)
	if code, handled := answerFromRetainedKey(c, dir, public, c.IdempotencyKey, payloadDigest, out, stderr, legacyRequestPayloadDigest(keyed)); handled {
		return code
	}
	req := SettingsRequest{Version: SettingsRequestVersion, Type: reqType, Session: info.Session,
		TaskID: c.RequeueTask, TaskRevision: revision, AttemptsRevision: attemptsRevision, IdempotencyKey: c.IdempotencyKey}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, req, c.waitBound())
		return reportRequeueAck(c, ack, err, out, stderr)
	}
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	note := ""
	if mutationStoreExists(dir) {
		note = heldInputNote(c, dir)
	}
	summary := fmt.Sprintf("Submitted requeue of %s.", c.RequeueTask)
	if c.RequeueFresh {
		summary = fmt.Sprintf("Submitted fresh requeue of %s.", c.RequeueTask)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, public, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false), summary, note, out)
}

// runRequeueStopped applies the requeue to a stopped run under the
// repository lock, with a bare engine that launches no agents.
func runRequeueStopped(c Config, revision, attemptsRevision string, out, stderr io.Writer) int {
	repo := c
	repo.GitInit = false
	_, dir := projectPaths(c)
	statePath := filepath.Join(dir, "state.db")
	if _, err := os.Stat(statePath); err != nil {
		p := runNotFoundForRun(c, statePath, "looper")
		p.Message = "No active run. Start Looper before requeuing tasks."
		p.withLegacy("No active run. Start Looper before requeuing tasks.", 1)
		return emitError(c, p, out, stderr)
	}
	r, err := OpenRepository(repo, dir)
	if err != nil {
		return emitError(c, stateUnreadableError(statePath, err), out, stderr)
	}
	lock, err := lockRepository(r.Common)
	if err != nil {
		p := operationFailure("controller_owns_repository", "A controller owns this repository; the requeue application runs inside it.")
		p.withLegacy("A controller already owns this repository.", 1)
		return emitError(c, p, out, stderr)
	}
	defer lock.Close()
	store, err := OpenStore(statePath)
	if err != nil {
		return emitError(c, stateUnreadableError(statePath, err), out, stderr)
	}
	defer store.Close()
	s, err := store.Load()
	if err != nil {
		return emitError(c, stateUnreadableError(statePath, err), out, stderr)
	}
	if s == nil {
		p := runNotFoundForRun(c, statePath, "looper")
		p.Message = "no saved run; the store holds no state yet"
		return emitError(c, p, out, stderr)
	}
	// The same identity the stopped recovery application enforces: a saved
	// run that belongs to another project or task file never requeues here.
	if s.Version != 2 || s.Root != c.Root || s.Todo != c.Todo {
		p := operationFailure("run_not_current", "The saved run belongs to another project or task file.")
		p.Retry = retryNo
		p.Observed = map[string]any{"saved_root": clampString(s.Root, maxErrorString), "saved_todo": clampString(s.Todo, maxErrorString)}
		return emitError(c, p, out, stderr)
	}
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("requeue"), Type: RequestTypeRequeue,
		TaskID: c.RequeueTask, TaskRevision: revision, AttemptsRevision: attemptsRevision,
		IdempotencyKey: c.IdempotencyKey, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if c.RequeueFresh {
		req.Type = RequestTypeRequeueFresh
	}
	e := &Engine{Config: c, Repo: r, Store: store, State: s, Dir: dir, Output: io.Discard, acks: map[string]SettingsAck{}}
	// A caller key replays through the bare engine (AX-012): a retry after
	// the stopped application answers from the retained receipt and records
	// the mirror receipt a replay owes, instead of applying the requeue
	// twice. A changed payload under the same key refuses with the conflict.
	if original := idempotencyLookup(dir, publicRequestType(req.Type), c.IdempotencyKey); original != nil {
		digest := requestPayloadDigest(req, nil)
		if digest == "" || !payloadDigestMatches(original.PayloadDigest, digest, legacyRequestPayloadDigest(req)) {
			return emitError(c, idempotencyConflictError(c.IdempotencyKey, original, digest), out, stderr)
		}
		ack := e.replayIdempotentAck(req, original)
		if e.err != nil {
			return emitError(c, persistenceError(e.err.Error()), out, stderr)
		}
		return reportRequeueAck(c, &ack, nil, out, stderr)
	}
	if c.RequeueFresh {
		ack := e.applyRequeueFreshRequest(req)
		if e.err != nil && ack.Status == AckApplied {
			return emitError(c, persistenceError(e.err.Error()), out, stderr)
		}
		return reportRequeueAck(c, &ack, nil, out, stderr)
	}
	ack := e.applyRequeueRequest(req)
	if e.err != nil && ack.Status == AckApplied {
		return emitError(c, persistenceError(e.err.Error()), out, stderr)
	}
	return reportRequeueAck(c, &ack, nil, out, stderr)
}

// reportRequeueAck renders the decided requeue outcome. Version 2 reports
// the request record; the legacy contract keeps the acknowledgement shape.
func reportRequeueAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != AckApplied && ack.Status != AckWaiting {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, requeueAckRecordView(ack, requeueRequestType(c)), ack.ID, out, stderr)
	}
	if c.JSON {
		json.NewEncoder(out).Encode(ack)
		if ack.Status == AckApplied || ack.Status == AckWaiting {
			return 0
		}
		return 1
	}
	label := "Requeue"
	if c.RequeueFresh {
		label = "Fresh requeue"
	}
	switch ack.Status {
	case AckApplied, AckWaiting:
		fmt.Fprintf(out, "%s request %s: %s\n", label, ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(out, " ", ack.Message)
		}
		return 0
	default:
		fmt.Fprintf(stderr, "%s request %s: %s\n", label, ack.ID, ack.Status)
		if ack.Message != "" {
			fmt.Fprintln(stderr, " ", ack.Message)
		}
		return 1
	}
}

// requeueRequestType names the public request type one requeue submits.
func requeueRequestType(c Config) string {
	if c.RequeueFresh {
		return PublicRequestRequeueFresh
	}
	return PublicRequestRequeue
}

// requeueAckRecordView maps one requeue acknowledgement to its record view.
func requeueAckRecordView(ack *SettingsAck, requestType string) *requestQueryRecord {
	state, summary := RequestRejected, ack.Message
	switch ack.Status {
	case AckApplied:
		state, summary = RequestApplied, "applied: "+ack.Message
	case AckWaiting:
		state = RequestWaiting
	}
	q := ackRecordView(ack.ID, requestType, state, summary, nil)
	q.errText = ack.Message
	return q
}
