package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The checked recovery application (AX-017). A preview (AX-016) enumerates
// the supported recovery choices and binds them to the state they observed
// through a token; this file applies those choices. Two transports share one
// application: a live controller claims a recovery request envelope at a loop
// boundary, exactly like cancel, and a stopped run applies under the
// repository lock the controller itself takes — the same locking discipline,
// with no agent launched. Every application re-derives the preview token and
// refuses a stale preview without mutation, so a preview can never replay
// over changed work.

// Recovery choice names, the vocabulary the preview enumerates and the
// application accepts. The names are schema-pinned in
// docs/contracts/schemas/v2/recovery-preview.schema.json.
const (
	RecoveryChoiceResume          = "resume"
	RecoveryChoicePublishRetained = "publish-retained"
	RecoveryChoiceRestoreTaskFile = "restore-task-file"
	RecoveryChoiceImportBacklog   = "import-backlog"
	RecoveryChoiceDiscardRetained = "discard-retained"
)

// recoveryTokenPattern bounds the token one application accepts. The preview
// derives it as a sha256 digest, so a valid token is always 64 hex digits.
var recoveryTokenPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// recoveryRefusal marks one application the guards refused: no file, state,
// or Git ref changed, the retained work stays exactly as it was, and the
// message names the concrete limitation.
type recoveryRefusal struct{ message string }

func (r *recoveryRefusal) Error() string { return r.message }

// processRecoveryRequests claims and answers pending recovery requests at a
// loop boundary, mirroring the cancel processor: the acknowledgement and the
// envelope removal follow the application, and a poisoned engine stops the
// pass after the request it answered.
func (e *Engine) processRecoveryRequests() {
	if e.controller == nil {
		return
	}
	for {
		req, err := e.claimRequest(RequestTypeRecovery)
		if err != nil {
			e.event("request-error", err.Error())
			return
		}
		if req == nil {
			return
		}
		ack := e.applyRecoveryRequest(req)
		if err = writeJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), ack); err != nil {
			e.err = err
			return
		}
		if err = os.Remove(filepath.Join(processingDir(e.Dir), req.ID+".json")); err != nil {
			e.err = err
			return
		}
		if e.err != nil {
			return
		}
	}
}

// recoveryApplyEvidence collects the evidence one application checks against.
// It is the same read the preview performs, except that the state it compares
// is the live engine state: a boundary may hold state the store has not
// committed yet, and the application must not validate against older bytes
// than the ones it is about to change.
func (e *Engine) recoveryApplyEvidence() (*recoveryEvidence, error) {
	snap, err := e.Store.Snapshot()
	if err != nil {
		return nil, err
	}
	return collectRecoveryEvidenceFor(e.Config, e.Dir, snap, e.State)
}

// applyRecoveryRequest validates and applies one claimed recovery request.
// The guards mirror the other backlog mutations: replay by identifier or
// caller key first, then the session check, then the checked application
// itself. A refusal records a rejected receipt and changes nothing; the
// applied outcome commits its receipt beside the mutation it governs.
func (e *Engine) applyRecoveryRequest(req *SettingsRequest) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID(), Status: AckError}
	if previous, ok := e.acks[req.ID]; ok {
		return previous
	}
	if replayed, ok := e.replayedAck(req); ok {
		e.rememberAck(req.ID, replayed)
		return replayed
	}
	// A caller key answers from the receipt that recorded it, before any
	// session check, exactly like the other mutations (AX-012): a retry after
	// a controller restart returns the recorded outcome and never applies the
	// recovery twice.
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
	if req.RecoveryChoice == "" || req.RecoveryToken == "" {
		ack.Status = AckError
		ack.Message = "the request names no recovery choice or no preview token"
		return refuse()
	}
	if !recoveryTokenPattern.MatchString(req.RecoveryToken) {
		ack.Status = AckError
		ack.Message = "the preview token is malformed; a token is 64 hex digits from looper recovery --preview"
		return refuse()
	}
	if req.Session != "" && req.Session != e.controllerSessionID() {
		ack.Status = AckStale
		ack.Message = "The controller session changed. Preview the recovery again and resubmit."
		return refuse()
	}
	// A live controller owns its own drained boundary: an import while work
	// is in flight would race the boundary that exists to import it. The
	// stopped-run application has no live work and applies under the lock.
	if req.RecoveryChoice == RecoveryChoiceImportBacklog && e.controller != nil && e.recoveryWorkInFlight() {
		ack.Status = AckConflict
		ack.Message = "Work is in flight: the controller imports a changed task file at its own drained boundary, so no request is needed. Preview again when the controller is idle, or stop it and apply under the stopped-run lock."
		return refuse()
	}
	ev, err := e.recoveryApplyEvidence()
	if err != nil {
		ack.Message = err.Error()
		return refuse()
	}
	// The checked application (AX-017): the token the preview issued must
	// re-derive from the state as it stands now. A changed task file, head,
	// branch, working tree, retained attempt, publication, or event journal
	// rejects the preview without mutation.
	if observed := recoveryToken(ev); observed != req.RecoveryToken {
		ack.Status = AckConflict
		ack.Message = "The preview is stale: the saved run, the task file, or the project changed since it was taken. Nothing was applied. Preview again with looper recovery --preview."
		return refuse()
	}
	var summary string
	switch req.RecoveryChoice {
	case RecoveryChoiceRestoreTaskFile:
		summary, err = e.applyRestoreTaskFile(ev)
	case RecoveryChoiceImportBacklog:
		summary, err = e.applyImportBacklog(ev)
	case RecoveryChoiceResume:
		err = &recoveryRefusal{message: "resume is not a request: it starts the controller and its startup recovery. Run: looper"}
	case RecoveryChoicePublishRetained:
		err = &recoveryRefusal{message: "the retained publication applies automatically when the run resumes; no separate publish operation exists. Run: looper"}
	case RecoveryChoiceDiscardRetained:
		err = &recoveryRefusal{message: "no supported discard exists; Looper never deletes retained work through recovery, and no force action is proposed"}
	default:
		err = &recoveryRefusal{message: fmt.Sprintf("unknown recovery choice %q; preview the supported choices with looper recovery --preview", req.RecoveryChoice)}
	}
	var refusal *recoveryRefusal
	switch {
	case err == nil:
		rec := e.beginRequestReceipt(req)
		rec.Summary = summary
		e.commitRecoveryReceipt(rec, RequestApplied, summary, "")
		ack.Status = AckApplied
		ack.Message = summary
		id := e.identity()
		id.RequestID = req.ID
		e.eventData("recovery-applied", summary, id.merge(map[string]any{"choice": req.RecoveryChoice}))
		return ack
	case errors.As(err, &refusal):
		ack.Status = AckConflict
		ack.Message = refusal.message + ". Nothing was applied; the retained work is unchanged."
		e.commitRecoveryReceipt(e.recoveryReceiptFor(req), RequestRejected, "", refusal.message)
		e.requestEvent("recovery-rejected", fmt.Sprintf("Recovery request %s refused: %s", req.ID, refusal.message), req.ID)
		return ack
	default:
		// An operational failure: nothing was committed by the guards above,
		// and the failed receipt keeps the outcome queryable.
		ack.Message = err.Error()
		e.commitRecoveryReceipt(e.recoveryReceiptFor(req), RequestFailed, "", err.Error())
		e.requestEvent("recovery-rejected", fmt.Sprintf("Recovery request %s failed: %s", req.ID, err.Error()), req.ID)
		return ack
	}
}

// recoveryWorkInFlight reports whether any attempt owns live work a file
// import must not race: a worker, a check, or a result waiting on its
// publication. Retained restartable work (a resume phase) holds no live
// worker and never blocks the application.
func (e *Engine) recoveryWorkInFlight() bool {
	for _, a := range e.State.Attempts {
		if a != nil && cancelBlockingAttempt(a) {
			return true
		}
	}
	return false
}

// recoveryReceiptFor builds the undecided receipt one recovery request owns,
// for the outcomes that decide it outside beginRequestReceipt. A fresh record
// starts in the submitted state: the empty state reads as terminal to the
// lifecycle (an unknown record is never re-driven), which would drop the
// decision before it commits.
func (e *Engine) recoveryReceiptFor(req *SettingsRequest) *RequestRecord {
	rec := e.requestReceipt(req.ID)
	if rec != nil {
		return rec
	}
	rec = &RequestRecord{
		ID: req.ID, Type: PublicRequestRecovery, State: RequestSubmitted,
		Session:       e.controllerSessionID(),
		TargetSession: req.Session,
		SubmittedAt:   orDefault(req.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)),
	}
	if req.IdempotencyKey != "" {
		rec.IdempotencyKey = req.IdempotencyKey
		rec.PayloadDigest = requestPayloadDigest(req, nil)
	}
	return rec
}

// commitRecoveryReceipt decides one recovery receipt and commits it with the
// state. The applied restore changes no saved state, so its receipt is the
// one durable record of the application and must save on its own.
func (e *Engine) commitRecoveryReceipt(rec *RequestRecord, state, summary, errText string) {
	if requestStateTerminal(rec.State) {
		return
	}
	rec.State = state
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Summary, rec.Error = summary, errText
	e.upsertRequest(rec)
	if err := e.persistRequestReceipts(); err != nil && e.err == nil {
		e.err = err
	}
}

// applyRestoreTaskFile returns the task file to the bytes the saved run last
// accepted, discarding the external edit the caller previewed. The restore
// source is byte-pinned: the retained accepted copy when it holds exactly the
// saved digest, else the canonical projection of the saved backlog when its
// bytes digest to the same value. Any other situation keeps the edit and
// reports the concrete limitation.
func (e *Engine) applyRestoreTaskFile(ev *recoveryEvidence) (string, error) {
	if ev.todoErr != nil {
		return "", &recoveryRefusal{message: fmt.Sprintf("the task file could not be read: %v", ev.todoErr)}
	}
	if ev.todoDigest == e.State.ExportHash {
		return "", &recoveryRefusal{message: "the task file already matches the saved contract; there is no edit to discard"}
	}
	restore, source, err := restoreSourceFor(e.Dir, e.State)
	if err != nil {
		return "", err
	}
	if restore == nil {
		return "", &recoveryRefusal{message: fmt.Sprintf("no restore source holds the accepted bytes (digest %s): the retained accepted copy is absent or stale and the saved backlog does not re-derive them. Restore the accepted bytes by hand, or preview again after the next accepted export refreshes the copy.", e.State.ExportHash)}
	}
	// The write is atomic: a crash leaves either the edit or the complete
	// accepted bytes, never a partial file. The receipt that follows is the
	// durable record; a crash before it leaves a recoverable outcome — the
	// file matches, so the next preview reports the match and a retry refuses.
	if err = atomicWrite(e.Config.Todo, restore, 0600); err != nil {
		return "", err
	}
	return fmt.Sprintf("restored the task file to the saved contract (digest %s) from %s; the external edit was discarded and a pending export replays on the next run", e.State.ExportHash, source), nil
}

// restoreSourceFor finds the byte-exact restore source for the saved
// contract: the retained accepted copy, else the canonical projection of the
// saved backlog. The preview asks the same question read-only, so the choice
// it offers and the application that runs cannot disagree. A pending export
// mutated the saved backlog, so its projection is a different digest there
// and only the retained copy can answer.
func restoreSourceFor(dir string, s *State) ([]byte, string, error) {
	if path, ok := acceptedExportRestore(dir, s.ExportHash); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, "", err
		}
		if digest(data) == s.ExportHash {
			return data, "the retained accepted bytes", nil
		}
	}
	// The export writes json.MarshalIndent(backlog, "", "  ") plus one
	// newline, so an accepted backlog re-derives its own accepted bytes.
	data, err := json.MarshalIndent(s.Backlog, "", "  ")
	if err != nil {
		return nil, "", err
	}
	data = append(data, '\n')
	if digest(data) == s.ExportHash && !s.ExportPending {
		return data, "the saved backlog's canonical export", nil
	}
	return nil, "", nil
}

// applyImportBacklog applies the external task-file edit through the T058
// retained-backlog import: the same refreshHeld path the live boundary and
// the startup recovery run, never a second import. An equivalent edit (AX-018)
// adopts the observed bytes as the accepted export instead of importing.
func (e *Engine) applyImportBacklog(ev *recoveryEvidence) (string, error) {
	if ev.todoErr != nil {
		return "", &recoveryRefusal{message: fmt.Sprintf("the task file could not be read: %v", ev.todoErr)}
	}
	edit := e.classifyBacklog(ev.todoData)
	if !edit.Changed {
		return "", &recoveryRefusal{message: "the task file already matches the saved contract; there is no edit to import"}
	}
	if edit.Reason != "" {
		return "", &recoveryRefusal{message: "the edited task file cannot be parsed, so no import can run: " + edit.Reason}
	}
	if edit.Equivalent {
		// Apply equivalent formatting (AX-018): the bytes become the accepted
		// export without moving the baseline, because the content is the same.
		if err := e.adoptBacklogBytes(edit); err != nil {
			return "", err
		}
		return fmt.Sprintf("applied the formatting-only edit: the equivalent bytes (digest %s) are the accepted export and the saved contract is unchanged", edit.Observed), nil
	}
	if !retained(e.State) {
		return "", &recoveryRefusal{message: "no work is retained; the next run archives the saved state and starts a new baseline from the edited file, so no import is needed"}
	}
	if !replaySafeRetainedWork(e.State) {
		return "", &recoveryRefusal{message: fmt.Sprintf("the edit cannot import: %d retained attempt(s) hold no replayable result, so importing would rerun implementation against the moved baseline. Restore the saved contract instead (looper recovery --apply restore-task-file).", recoveryNonReplayable(e.State))}
	}
	// The edit must leave every retained task's contract intact: the next
	// startup compares the retained attempts against the imported backlog,
	// and without this gate an edit that changes a retained contract would
	// strand that work at the contract guard. Every import path refuses the
	// same edit through the same list (T119). The preview states the same
	// limitation.
	if breakers := retainedContractBreakers(e.State, ev.todoData); len(breakers) > 0 {
		return "", &recoveryRefusal{message: fmt.Sprintf("the edit cannot import: it changes the contract of retained task(s) %s while their work is retained, so every import path refuses it and the next startup keeps the restore demand. Edit outside the retained tasks, restore the saved contract (looper recovery --apply restore-task-file), or wait for the retained work to finish.", boundedIDList(breakers, 8))}
	}
	// The token check above proved the baseline unchanged; the import still
	// re-verifies it, exactly as the startup path does.
	if err := e.importRetainedEdit(edit.Observed); err != nil {
		return "", err
	}
	_, total := retainedTaskIDs(e.State)
	return fmt.Sprintf("imported the edited backlog (digest %s) through the retained-backlog import; %d retained result(s) replay against the imported baseline without repeating implementation", edit.Observed, total), nil
}

// recoveryNonReplayable counts the retained attempts that hold neither a
// result commit nor a durable receipt the recovery adopts.
func recoveryNonReplayable(s *State) int {
	n := 0
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
			continue
		}
		if a.ResultCommit != "" {
			continue
		}
		if _, ok := retainedReceipt(a); ok {
			continue
		}
		n++
	}
	return n
}

// retainedContractBreakers lists the retained tasks whose contract the edited
// task-file bytes change or remove. The next startup compares the retained
// attempts against the imported backlog, so an edit on this list would strand
// the work at the task-contract guard instead of replaying it; every import
// path refuses such an edit (T119).
func retainedContractBreakers(s *State, data []byte) []string {
	imported, err := decodeBacklogStrict(data)
	if err != nil {
		return nil
	}
	return contractBreakers(s, &imported)
}

// contractBreakers lists the retained tasks whose contract the parsed backlog
// changes or removes. Every import path gates on this one list — the checked
// application, the preview, the startup import, and the held boundary — so no
// path can import an edit another path refuses.
func contractBreakers(s *State, imported *Backlog) []string {
	var breakers []string
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
			continue
		}
		if t := imported.Task(a.Task.ID); t == nil || t.Contract() != a.Task.Contract() {
			breakers = append(breakers, a.Task.ID)
		}
	}
	return breakers
}

// runRecoveryApply serves `looper recovery --apply <choice> --token <token>`
// (AX-017). A live controller receives the request through the common request
// protocol and the caller gets the immediate receipt; a stopped run applies
// under the repository lock the controller itself takes, with no agent
// launched, and reports the decided outcome.
func runRecoveryApply(c Config, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		p := runNotFoundForRun(c, filepath.Join(dir, "state.db"), "looper")
		p.Message = "no saved run; a recovery application needs one"
		return emitError(c, p, out, stderr)
	}
	if repositoryOwned(c) {
		return submitRecoveryApply(c, dir, out, stderr)
	}
	return applyStoppedRecovery(c, dir, out, stderr)
}

// submitRecoveryApply publishes one recovery request to the live controller
// and returns the immediate receipt, mirroring the other mutations (AX-011).
func submitRecoveryApply(c Config, dir string, out, stderr io.Writer) int {
	info, err := readController(dir)
	if err != nil {
		return emitError(c, controllerMetadataError(err), out, stderr)
	}
	// An older controller never claims a recovery envelope: the specific
	// incompatibility result keeps the request from becoming a dead letter
	// (AX-008).
	if err = validateControllerRequest(info, PublicRequestRecovery); err != nil {
		return emitError(c, err, out, stderr)
	}
	base := SettingsRequest{Type: RequestTypeRecovery, RecoveryChoice: c.RecoveryChoice, RecoveryToken: c.RecoveryToken}
	payloadDigest := requestPayloadDigest(&base, nil)
	// A caller key answers from the retained receipts before any envelope is
	// published (AX-012).
	if code, handled := answerFromRetainedKey(c, dir, PublicRequestRecovery, c.IdempotencyKey, payloadDigest, out, stderr); handled {
		return code
	}
	req := SettingsRequest{
		Version: SettingsRequestVersion, Type: RequestTypeRecovery, Session: info.Session,
		RecoveryChoice: c.RecoveryChoice, RecoveryToken: c.RecoveryToken, IdempotencyKey: c.IdempotencyKey,
	}
	if c.Wait {
		ack, err := submitSettingsRequest(dir, req, c.waitBound())
		return reportRecoveryAck(c, ack, err, out, stderr)
	}
	if err := publishSettingsRequest(dir, &req); err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	return emitMutationReceipt(c, keyedMutationReceipt(newMutationReceipt(req.ID, PublicRequestRecovery, RequestSubmitted, req.CreatedAt, info.Session),
		c.IdempotencyKey, payloadDigest, false),
		fmt.Sprintf("Submitted recovery application %q.", c.RecoveryChoice),
		"The controller re-checks the preview token before it applies; a stale preview is refused without mutation.", out)
}

// applyStoppedRecovery applies one recovery choice against a stopped run
// under the repository lock. The locking discipline is the controller's own:
// the exclusive lock serializes against any controller start, the store opens
// read-write, and no controller identity is published because no controller
// runs here. No agent is launched — the application only rewrites accepted
// bytes or replays the retained-backlog import.
func applyStoppedRecovery(c Config, dir string, out, stderr io.Writer) int {
	repo := c
	repo.GitInit = false
	r, err := OpenRepository(repo, dir)
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	lock, err := lockRepository(r.Common)
	if err != nil {
		// The ownership probe and the lock raced: a controller owns the
		// repository now, so the request belongs to it.
		p := operationFailure("controller_owns_repository", "A controller owns this repository; the recovery application runs inside it.")
		p.Actions = []ErrorAction{{Description: "Submit the recovery to the active controller.", Command: "looper recovery --apply " + c.RecoveryChoice + " --token <token>"}}
		p.withLegacy("A controller already owns this repository.", 1)
		return emitError(c, p, out, stderr)
	}
	defer lock.Close()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	defer store.Close()
	s, err := store.Load()
	if err != nil {
		return emitError(c, stateUnreadableError(filepath.Join(dir, "state.db"), err), out, stderr)
	}
	if s == nil {
		p := runNotFoundForRun(c, filepath.Join(dir, "state.db"), "looper")
		p.Message = "no saved run; the store holds no state yet"
		return emitError(c, p, out, stderr)
	}
	if s.Version != 2 || s.Root != c.Root || s.Todo != c.Todo {
		p := operationFailure("run_not_current", "The saved run belongs to another project or task file.")
		p.Retry = retryNo
		p.Observed = map[string]any{"saved_root": clampString(s.Root, maxErrorString), "saved_todo": clampString(s.Todo, maxErrorString)}
		return emitError(c, p, out, stderr)
	}
	// A bare engine owns the application: no controller identity, no view, no
	// workers. Its events journal with no session, exactly like the receipts
	// it records, and its output stays out of the CLI contract because the
	// decided outcome below is the answer.
	e := &Engine{Config: c, Repo: r, Store: store, State: s, Dir: dir, Output: io.Discard, acks: map[string]SettingsAck{}}
	req := &SettingsRequest{
		Version: SettingsRequestVersion, ID: identifier("recovery"), Type: RequestTypeRecovery,
		RecoveryChoice: c.RecoveryChoice, RecoveryToken: c.RecoveryToken, IdempotencyKey: c.IdempotencyKey,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	ack := e.applyRecoveryRequest(req)
	if e.err != nil && ack.Status == AckApplied {
		return emitError(c, persistenceError(e.err.Error()), out, stderr)
	}
	return reportRecoveryAck(c, &ack, nil, out, stderr)
}

// reportRecoveryAck renders one decided recovery outcome. Version 2 reports
// the request record — the applied summary or the refused limitation — and
// the legacy contract keeps the acknowledgement shape it prints for the other
// mutations.
func reportRecoveryAck(c Config, ack *SettingsAck, err error, out, stderr io.Writer) int {
	if err != nil {
		return emitError(c, requestSubmitError(err), out, stderr)
	}
	if c.Contract == ContractVersionV2 {
		if ack.Status != AckApplied {
			return emitError(c, requestAckError(ack), out, stderr)
		}
		return emitWaitRecord(c, recoveryAckRecordView(ack), ack.ID, out, stderr)
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
		fmt.Fprintf(out, "Recovery request %s: applied\n", ack.ID)
	default:
		fmt.Fprintf(out, "Recovery request %s: %s\n", ack.ID, ack.Status)
	}
	if ack.Message != "" {
		if ack.Status == AckApplied {
			fmt.Fprintln(out, " ", ack.Message)
		} else {
			fmt.Fprintln(stderr, " ", ack.Message)
		}
	}
	if ack.Status == AckApplied {
		return 0
	}
	return 1
}

// recoveryAckRecordView maps one recovery acknowledgement to its record view,
// so a --wait or a stopped-run application reports the decision it observed.
func recoveryAckRecordView(ack *SettingsAck) *requestQueryRecord {
	state, summary := RequestRejected, ack.Message
	if ack.Status == AckApplied {
		state, summary = RequestApplied, "applied: "+ack.Message
	}
	q := ackRecordView(ack.ID, PublicRequestRecovery, state, summary, nil)
	q.errText = ack.Message
	return q
}

// recoveryApplyUsage names the supported application choices for help and
// refusals, in preview order.
func recoveryApplyUsage() string {
	return strings.Join([]string{
		RecoveryChoiceRestoreTaskFile, RecoveryChoiceImportBacklog,
	}, ", ")
}
