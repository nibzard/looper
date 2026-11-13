package looper

// Caller-supplied idempotency keys (AX-012). A key is scoped to the project
// and the operation: the receipts one run saves are per project, and a key
// resolves only inside the public request type that recorded it. Repeating a
// key with the same payload returns the original request and its result; a
// changed payload under the same key is a conflict. Identical content under
// distinct keys stays possible — keys deduplicate retries, never task
// content.

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

// maxIdempotencyKeyBytes bounds one caller key, the bound the receipt schema
// publishes (maxLength 128). Keys never name files, so the bound protects the
// durable receipts, not the file system.
const maxIdempotencyKeyBytes = 128

// validateIdempotencyKey bounds one caller key: non-empty, at most
// maxIdempotencyKeyBytes of valid UTF-8, and free of control characters. The
// same rule runs at parse time, at envelope publication, and at envelope
// load, so a hand-written envelope cannot smuggle an unbounded key into the
// durable receipts.
func validateIdempotencyKey(key string) error {
	if key == "" {
		return fmt.Errorf("the idempotency key is empty")
	}
	if len(key) > maxIdempotencyKeyBytes {
		return fmt.Errorf("the idempotency key exceeds %d bytes", maxIdempotencyKeyBytes)
	}
	if !utf8.ValidString(key) {
		return fmt.Errorf("the idempotency key is not valid UTF-8")
	}
	for _, r := range key {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("the idempotency key contains control characters")
		}
	}
	return nil
}

// canonicalPayload is the type-free canonical form one payload digest covers.
// Field order is fixed by the struct and map keys marshal sorted, so the same
// payload always digests identically whatever produced it. Transport fields —
// the identifier, the session, the submitter, the creation time — are
// excluded: a retry after a controller restart targets a new session and must
// still match.
type canonicalPayload struct {
	Tasks          []Task                     `json:"tasks,omitempty"`
	Fields         map[string]json.RawMessage `json:"fields,omitempty"`
	Reset          bool                       `json:"reset,omitempty"`
	PlanDigests    []string                   `json:"plan_digests,omitempty"`
	TaskID         string                     `json:"task_id,omitempty"`
	TaskRevision   string                     `json:"task_revision,omitempty"`
	Cascade        bool                       `json:"cascade,omitempty"`
	RecoveryChoice string                     `json:"recovery_choice,omitempty"`
	RecoveryToken  string                     `json:"recovery_token,omitempty"`
}

// requestPayloadDigest digests one request's canonical payload. Plan imports
// are content-addressed: planDigests carries the per-file content digests the
// caller read, so the same bytes digest the same whatever path carried them;
// without content digests a plan import reports no digest, and a key with no
// comparable digest resolves to a conflict, never a false replay.
func requestPayloadDigest(req *SettingsRequest, planDigests []string) string {
	if req == nil {
		return ""
	}
	if publicRequestType(req.Type) == PublicRequestAddPlan {
		if len(planDigests) == 0 {
			return ""
		}
		digests := append([]string{}, planDigests...)
		b, err := json.Marshal(canonicalPayload{PlanDigests: digests})
		if err != nil {
			return ""
		}
		return digest(b)
	}
	b, err := json.Marshal(canonicalPayload{
		Tasks: req.Tasks, Fields: req.Fields, Reset: req.Reset,
		TaskID: req.TaskID, TaskRevision: req.TaskRevision, Cascade: req.Cascade,
		RecoveryChoice: req.RecoveryChoice, RecoveryToken: req.RecoveryToken,
	})
	if err != nil {
		return ""
	}
	return digest(b)
}

// planContentDigests lists the content digests of one captured plan batch, in
// submission order — the input requestPayloadDigest takes for add-plan.
func planContentDigests(plans []capturedPlan) []string {
	digests := make([]string, 0, len(plans))
	for _, p := range plans {
		digests = append(digests, p.Digest)
	}
	return digests
}

// requestByIdempotencyKey finds the earliest receipt one key recorded for a
// request type. Receipts sit in commit order, so the earliest is the original
// submission; later mirrors and conflicts name it instead of owning the key.
func (e *Engine) requestByIdempotencyKey(requestType, key string) *RequestRecord {
	if e.State == nil || key == "" {
		return nil
	}
	for _, r := range e.State.Requests {
		if r != nil && r.Type == requestType && r.IdempotencyKey == key {
			return r
		}
	}
	return nil
}

// resolveIdempotencyKey answers one claimed request through its caller key.
// The digest is the claim's canonical payload digest; original names the
// recorded request the same key and payload must answer from; conflict names
// the recorded request a changed or unverifiable payload refuses against.
// All three are zero values when the request carries no key.
func (e *Engine) resolveIdempotencyKey(req *SettingsRequest, planDigests []string) (digest string, original, conflict *RequestRecord) {
	if req == nil || req.IdempotencyKey == "" {
		return "", nil, nil
	}
	digest = requestPayloadDigest(req, planDigests)
	// An envelope without a type is a settings request, the default the
	// loader applies; the receipts name the public type the same way.
	envelopeType := orDefault(req.Type, RequestTypeSettings)
	found := e.requestByIdempotencyKey(publicRequestType(envelopeType), req.IdempotencyKey)
	if found == nil {
		return digest, nil, nil
	}
	if digest != "" && found.PayloadDigest == digest {
		return digest, found, nil
	}
	return digest, nil, found
}

// keyConflictMessage names why one keyed claim refuses: the key already
// decided other content, or its recorded digest cannot be compared.
func keyConflictMessage(key string, found *RequestRecord, digest string) string {
	if digest == "" || found.PayloadDigest == "" {
		return fmt.Sprintf("The idempotency key %s was already used by request %s, whose payload digest was not recorded; resubmit under a new key.", key, found.ID)
	}
	return fmt.Sprintf("The idempotency key %s was already used by request %s with different content; a changed payload under the same key is rejected. Resubmit under a new key.", key, found.ID)
}

// replayIdempotentAck answers one claimed request with the outcome its key
// already recorded, so a retry after lost output or a controller restart
// applies no second mutation. The answer rides the claimed request's
// acknowledgement; the mirror receipt names the original, and the key keeps
// resolving to the original's receipt.
func (e *Engine) replayIdempotentAck(req *SettingsRequest, original *RequestRecord) SettingsAck {
	ack := SettingsAck{Version: SettingsRequestVersion, ID: req.ID, Session: e.controllerSessionID()}
	if original.Ack != nil {
		ack = *original.Ack
		ack.ID = req.ID
	} else if original.State == RequestApplied {
		ack.Status = AckApplied
		ack.Message = original.Summary
	} else {
		ack.Status = AckError
		ack.Message = fmt.Sprintf("the original request %s is %s: %s", original.ID, original.State, orDefault(original.Error, original.Summary))
	}
	e.rememberAck(req.ID, ack)
	if e.State != nil {
		state, decided := original.State, ""
		if !requestStateTerminal(original.State) {
			// The original is still in flight: the retry is refused pending it,
			// and the mirror stays terminal so it never reads as pending work.
			state = RequestRejected
			if ack.Status == AckApplied || ack.Status == AckReset {
				ack.Status = AckConflict
			}
			if ack.Message == "" {
				ack.Message = keyInFlightMessage(req.IdempotencyKey, original)
			}
		} else {
			decided = time.Now().UTC().Format(time.RFC3339Nano)
		}
		e.upsertRequest(&RequestRecord{
			ID: req.ID, Type: publicRequestType(req.Type), State: state,
			Session: e.controllerSessionID(), TargetSession: req.Session,
			SubmittedAt: orDefault(req.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)), DecidedAt: decided,
			AffectedTasks: original.AffectedTasks,
			Summary:       "idempotent replay of request " + original.ID,
			Ack:           &ack,
		})
		if err := e.Store.Save(e.State); err != nil && e.err == nil {
			e.err = err
		}
	}
	return ack
}

// keyInFlightMessage refuses a retry whose original request has not decided
// or whose recorded outcome carries no repliable result. The caller polls the
// original, exactly as the duplicate receipt at submission time tells it to.
func keyInFlightMessage(key string, original *RequestRecord) string {
	return fmt.Sprintf("The idempotency key %s belongs to request %s, which is %s; query that request instead of resubmitting.", key, original.ID, original.State)
}

// idempotencyLookup reads the retained receipts for the original request one
// key names. No store, no state, or no match reports nil: nothing has
// recorded the key, so the caller publishes its envelope.
func idempotencyLookup(dir, requestType, key string) *RequestRecord {
	if key == "" || !mutationStoreExists(dir) {
		return nil
	}
	store, err := openObservedStore(dir)
	if err != nil {
		return nil
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil || snap.State == nil {
		return nil
	}
	for _, r := range snap.State.Requests {
		if r != nil && r.Type == requestType && r.IdempotencyKey == key {
			return r
		}
	}
	return nil
}

// answerFromRetainedKey answers one keyed submission before any envelope is
// published (AX-012). The same key and payload returns the original request —
// as the immediate receipt under its identifier, or as the record a --wait
// reports when the original already decided. A changed payload under the same
// key is a conflict. handled false means the key has no retained receipt: the
// caller publishes and the controller owns the outcome, with the claim-time
// resolution covering the races a pre-check cannot see.
func answerFromRetainedKey(c Config, dir, requestType, key, payloadDigest string, out, stderr io.Writer) (int, bool) {
	if key == "" {
		return 0, false
	}
	original := idempotencyLookup(dir, requestType, key)
	if original == nil {
		return 0, false
	}
	if payloadDigest == "" || original.PayloadDigest != payloadDigest {
		return emitError(c, idempotencyConflictError(key, original, payloadDigest), out, stderr), true
	}
	if c.Wait && requestStateTerminal(original.State) {
		return emitWaitRecord(c, requestQueryRecordFromReceipt(original, nil, wallClock()), original.ID, out, stderr), true
	}
	createdAt := orDefault(original.SubmittedAt, wallClock().UTC().Format(time.RFC3339Nano))
	r := keyedMutationReceipt(newMutationReceipt(original.ID, requestType, original.State, createdAt, original.Session),
		key, payloadDigest, true)
	note := ""
	if !requestStateTerminal(original.State) {
		note = "The original request is still running; query it for the outcome."
	}
	return emitMutationReceipt(c, r,
		fmt.Sprintf("Idempotency key %s returns the earlier request %s.", key, original.ID), note, out), true
}

// idempotencyConflictError reports a key reused with different — or
// unverifiable — content. The original request stays the key's answer, so the
// evidence routes to it and the fix is a new key.
func idempotencyConflictError(key string, original *RequestRecord, digest string) *PublicError {
	p := operationFailure("idempotency_key_conflict", keyConflictMessage(key, original, digest))
	p.Retry = retryAfterChange
	observedDigest := digest
	if observedDigest == "" {
		observedDigest = "unknown"
	}
	p.Expected = map[string]any{
		"idempotency_key":  clampString(key, maxIdempotencyKeyBytes),
		"original_request": original.ID,
		"payload_digest":   orDefault(original.PayloadDigest, "not recorded"),
	}
	p.Observed = map[string]any{"payload_digest": observedDigest}
	p.Evidence = []ErrorEvidence{{
		Kind: "request", Reference: "durable receipt " + original.ID,
		Command: requestQueryCommand(original.ID),
	}}
	p.Actions = []ErrorAction{
		{Description: "Read the original request this key recorded.", Command: requestQueryCommand(original.ID)},
		{Description: "Submit the changed content under a new idempotency key."},
	}
	p.withLegacy(keyConflictMessage(key, original, digest), 1)
	return p
}
