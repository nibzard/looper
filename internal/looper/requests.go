package looper

import (
	"os"
	"path/filepath"
	"time"
)

// The durable request lifecycle (AX-009). One receipt store covers every
// common request type — add-task, add-plan, settings, requeue, and stop —
// with the states docs/contracts/request-states.json defines. The names and
// terminal flags mirror that registry; the cross-check test keeps them
// identical.
const (
	RequestSubmitted = "submitted"
	RequestAccepted  = "accepted"
	RequestWaiting   = "waiting"
	RequestApplied   = "applied"
	RequestRejected  = "rejected"
	RequestFailed    = "failed"
	RequestCancelled = "cancelled"
)

// requestStateRegistry mirrors docs/contracts/request-states.json: which
// states are terminal. A terminal state never transitions again; a receipt in
// one is the request's permanent answer.
var requestStateRegistry = map[string]bool{
	RequestSubmitted: false,
	RequestAccepted:  false,
	RequestWaiting:   false,
	RequestApplied:   true,
	RequestRejected:  true,
	RequestFailed:    true,
	RequestCancelled: true,
}

// requestStateTerminal reports whether one lifecycle state is terminal.
// An unlisted state reports true: an unknown record is never re-driven.
func requestStateTerminal(state string) bool {
	terminal, listed := requestStateRegistry[state]
	if !listed {
		return true
	}
	return terminal
}

// maxRetainedRequestReceipts bounds the terminal receipts one run retains.
// Every non-terminal receipt stays whatever the count, because each names
// work the controller still owes an outcome. The oldest terminal receipts
// drop first; the retained bound is the documented receipt retention
// guarantee until a dedicated retention task narrows it.
const maxRetainedRequestReceipts = 200

// RequestRecord is one request's durable receipt. It lives in the saved
// state, so a receipt commits in the same save as the mutation it records
// and survives the controller restart that follows. The acknowledgement
// transport (acks/<id>.json) stays the version 1 reply a submitter polls;
// this record is the outcome a later query reads, including after the
// acknowledgement directory has been swept.
type RequestRecord struct {
	ID    string `json:"id"`
	Type  string `json:"type"` // the public request type name
	State string `json:"state"`
	// Session is the controller session that decided the record. A record
	// written before any decision names the session the envelope addressed.
	Session       string `json:"controller_session,omitempty"`
	TargetSession string `json:"target_session,omitempty"`
	SubmittedAt   string `json:"submitted_at,omitempty"`
	DecidedAt     string `json:"decided_at,omitempty"`
	// WaitReason, WaitDetail, and WaitSince describe a waiting request's
	// prerequisite and when it started waiting.
	WaitReason    string   `json:"wait_reason,omitempty"`
	WaitDetail    string   `json:"wait_detail,omitempty"`
	WaitSince     string   `json:"wait_since,omitempty"`
	AffectedTasks []string `json:"affected_tasks,omitempty"`
	// IdempotencyKey is the caller key this request carried, when it carried
	// one (AX-012); PayloadDigest is that submission's canonical payload
	// digest. The key resolves inside the request type that recorded it: the
	// same key and digest returns this request's result, and a different
	// digest under the same key is a conflict.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	PayloadDigest  string `json:"payload_digest,omitempty"`
	// Fields lists the settings keys an applied settings request changed.
	Fields  []string `json:"applied_fields,omitempty"`
	Summary string   `json:"summary,omitempty"`
	Error   string   `json:"error,omitempty"`
	// Ack is the recorded legacy acknowledgement for the settings-family
	// transports. A replayed request returns it instead of applying twice.
	Ack *SettingsAck `json:"ack,omitempty"`
}

// publicRequestType maps one envelope type to the public request type name
// the capabilities contract advertises. The stop transport has no envelope;
// its receipts name the public type directly.
func publicRequestType(envelopeType string) string {
	if public, ok := publicRequestNames[envelopeType]; ok {
		return public
	}
	if envelopeType == "" {
		return "unknown"
	}
	return envelopeType
}

// rememberAck caches one answered request's acknowledgement for the
// in-process duplicate-identifier check. Protocol engines built without the
// map keep no cache; the durable receipt still answers replays.
func (e *Engine) rememberAck(id string, ack SettingsAck) {
	if e.acks != nil {
		e.acks[id] = ack
	}
}

// requestReceipt finds one request's durable receipt in the saved state.
func (e *Engine) requestReceipt(id string) *RequestRecord {
	if e.State == nil || id == "" {
		return nil
	}
	for _, r := range e.State.Requests {
		if r != nil && r.ID == id {
			return r
		}
	}
	return nil
}

// upsertRequest inserts or replaces one receipt and prunes the terminal
// receipts beyond the retention bound. A nil state (a protocol-only engine
// or a controller that never loaded state) keeps no receipts; the
// acknowledgement transport alone answers there, exactly as before.
func (e *Engine) upsertRequest(rec *RequestRecord) {
	if e.State == nil || rec == nil || rec.ID == "" {
		return
	}
	replaced := false
	for i, existing := range e.State.Requests {
		if existing != nil && existing.ID == rec.ID {
			e.State.Requests[i] = rec
			replaced = true
			break
		}
	}
	if !replaced {
		e.State.Requests = append(e.State.Requests, rec)
	}
	e.pruneRequestReceipts()
}

// pruneRequestReceipts drops the oldest terminal receipts beyond the
// retention bound. Non-terminal receipts never count against the bound.
func (e *Engine) pruneRequestReceipts() {
	terminal := 0
	for i := len(e.State.Requests) - 1; i >= 0; i-- {
		if r := e.State.Requests[i]; r == nil || !requestStateTerminal(r.State) {
			continue
		}
		terminal++
		if terminal > maxRetainedRequestReceipts {
			e.State.Requests = append(e.State.Requests[:i], e.State.Requests[i+1:]...)
		}
	}
}

// beginRequestReceipt records durable acceptance of one request. Acceptance
// is the moment the controller takes responsibility: from here the request
// survives a restart or receives an explicit terminal result. The caller
// commits the receipt with the mutation it governs.
func (e *Engine) beginRequestReceipt(req *SettingsRequest) *RequestRecord {
	rec := &RequestRecord{
		ID: req.ID, Type: publicRequestType(req.Type), State: RequestAccepted,
		Session: e.controllerSessionID(), TargetSession: req.Session,
		SubmittedAt: orDefault(req.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)),
	}
	if req.IdempotencyKey != "" {
		rec.IdempotencyKey = req.IdempotencyKey
		rec.PayloadDigest = requestPayloadDigest(req, nil)
	}
	if previous := e.requestReceipt(req.ID); previous != nil {
		// A receipt already exists: this is a replay, not a fresh acceptance.
		return previous
	}
	e.upsertRequest(rec)
	return rec
}

// decideRequestReceipt moves one receipt to a terminal state with its
// outcome. A receipt already in a terminal state keeps that state; a decided
// request never re-decides.
func (e *Engine) decideRequestReceipt(rec *RequestRecord, state, summary, errText string) *RequestRecord {
	if rec == nil {
		return nil
	}
	if requestStateTerminal(rec.State) {
		return rec
	}
	rec.State = state
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
	rec.Summary = summary
	rec.Error = errText
	e.upsertRequest(rec)
	return rec
}

// waitRequestReceipt records that one accepted request waits for a named
// prerequisite. A receipt already waiting for the same reason stays as it
// is, so a repeated boundary does not rewrite the wait start.
func (e *Engine) waitRequestReceipt(rec *RequestRecord, reason, detail string) bool {
	if rec == nil {
		return false
	}
	if rec.State == RequestWaiting && rec.WaitReason == reason {
		return false
	}
	rec.State = RequestWaiting
	rec.WaitReason, rec.WaitDetail = reason, detail
	if rec.WaitSince == "" {
		rec.WaitSince = time.Now().UTC().Format(time.RFC3339Nano)
	}
	e.upsertRequest(rec)
	return true
}

// failStagedRequest records the terminal failure of a staged application
// whose commit rolled back. The staged receipt looked applied inside the
// aborted transaction; this overwrite is the one transition a rolled-back
// staging must force, and it only ever runs before the staging commits.
func (e *Engine) failStagedRequest(rec *RequestRecord, ack SettingsAck, failure error) {
	if rec == nil {
		return
	}
	rec.State = RequestFailed
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
	rec.Summary = ""
	rec.Error = failure.Error()
	rec.Ack = &ack
	e.upsertRequest(rec)
}

// persistRequestReceipts commits the current state directly. It backs the
// rollback paths, which must persist their rollback even though the engine
// is about to poison itself with the failure that caused it.
func (e *Engine) persistRequestReceipts() error {
	if e.State == nil {
		return nil
	}
	return e.Store.Save(e.State)
}

// rejectRequestReceipt records a refused request and commits the receipt.
// A refusal changes no saved work, so the receipt is the only durable
// outcome: it keeps the refusal queryable after the envelope and its
// acknowledgement are gone. A receipt that already holds a terminal outcome
// keeps it; the recorded answer stays authoritative.
func (e *Engine) rejectRequestReceipt(req *SettingsRequest, ack SettingsAck) {
	if e.State == nil {
		return
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil {
		rec = &RequestRecord{
			ID: req.ID, Type: publicRequestType(req.Type), Session: e.controllerSessionID(),
			TargetSession: req.Session,
			SubmittedAt:   orDefault(req.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)),
		}
		if req.IdempotencyKey != "" {
			rec.IdempotencyKey = req.IdempotencyKey
			// A plan import's payload digest is its content, which this
			// refusal does not carry; the empty digest makes a later reuse of
			// the key a conflict, never a false replay.
			rec.PayloadDigest = requestPayloadDigest(req, nil)
		}
	} else if requestStateTerminal(rec.State) {
		return
	}
	rec.State = RequestRejected
	rec.DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Error = ack.Message
	rec.Ack = &ack
	e.upsertRequest(rec)
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// requestReceiptForImport mirrors one plan-import receipt into the common
// lifecycle states: queued means durable acceptance, waiting and planning
// name their prerequisite, and applied or failed are terminal.
func requestReceiptForImport(r *ImportRecord, session string) *RequestRecord {
	rec := &RequestRecord{
		ID: r.RequestID, Type: PublicRequestAddPlan, Session: session,
		SubmittedAt: r.Enqueued, AffectedTasks: r.AddedTasks,
		IdempotencyKey: r.IdempotencyKey, PayloadDigest: r.PayloadDigest,
	}
	if summary := importSummary(r); summary != "" {
		rec.Summary = summary
	}
	switch r.Status {
	case ImportApplied:
		rec.State, rec.DecidedAt = RequestApplied, r.Applied
	case ImportFailed:
		rec.State, rec.Error = RequestFailed, r.Error
	case ImportWaiting:
		rec.State, rec.WaitReason, rec.WaitSince = RequestWaiting, "active_work", r.Enqueued
	case ImportPlanning:
		rec.State, rec.WaitReason, rec.WaitSince = RequestWaiting, "import_analysis", r.Enqueued
	default:
		rec.State = RequestAccepted
	}
	return rec
}

// importSummary is one line naming an import's recorded outcome.
func importSummary(r *ImportRecord) string {
	switch r.Status {
	case ImportApplied:
		return importOutcome(r.AddedTasks, r.Covered)
	case ImportFailed:
		return "the import failed: " + r.Error
	}
	return ""
}

// requestEnvelopeExists reports whether one request's envelope is still
// durable in the queue or a live claim.
func requestEnvelopeExists(dir, id string) bool {
	if id == "" {
		return false
	}
	for _, base := range []string{requestDir(dir), processingDir(dir)} {
		if _, err := os.Stat(filepath.Join(base, id+".json")); err == nil {
			return true
		}
	}
	return false
}

// requestLifecycleState reports one request's queryable outcome from
// durable evidence alone: the receipt decides, an envelope without a
// receipt is submitted, and neither leaves the request unknown to this run.
// The empty state means no durable trace exists here.
func requestLifecycleState(dir string, s *State, id string) (state string, receipt *RequestRecord) {
	if id == "" {
		return "", nil
	}
	if s != nil {
		for _, r := range s.Requests {
			if r != nil && r.ID == id {
				return r.State, r
			}
		}
	}
	if requestEnvelopeExists(dir, id) {
		return RequestSubmitted, nil
	}
	return "", nil
}
