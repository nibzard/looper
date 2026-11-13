package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPlanBytes bounds one import, shared across all submitted files.
const maxPlanBytes = 1 << 20

// capturedPlan is one validated requirements document copied into immutable
// runtime storage. Later edits to the source file cannot change a queued
// import; workers and review read the captured text, never the source path.
type capturedPlan struct {
	Path     string
	Digest   string
	Artifact string
	Text     string
}

func importsDir(dir string) string { return filepath.Join(dir, "imports") }

// validatePlanFiles rejects unreadable files, directories, empty files,
// invalid text, and oversized input before any work starts. The rejections
// are typed input errors so the CLI can report them without echoing file
// contents; only the path and the reason become public.
func validatePlanFiles(paths []string) error {
	total := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			return &inputError{code: "input_unreadable", message: fmt.Sprintf("cannot read plan %s: %v", path, err), path: path}
		}
		if info.IsDir() {
			return &inputError{code: "input_malformed", message: fmt.Sprintf("plan %s is a directory", path), path: path}
		}
		if info.Size() == 0 {
			return &inputError{code: "input_malformed", message: fmt.Sprintf("plan %s is empty", path), path: path}
		}
		total += int(info.Size())
	}
	if total > maxPlanBytes {
		return &inputError{
			code: "input_malformed", path: paths[0],
			message:  fmt.Sprintf("plans total %d bytes; the combined limit is %d bytes", total, maxPlanBytes),
			expected: map[string]any{"combined_plan_bytes": maxPlanBytes}, observed: map[string]any{"combined_plan_bytes": total},
		}
	}
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return &inputError{code: "input_unreadable", message: err.Error(), path: path}
		}
		if !utf8.Valid(b) {
			return &inputError{code: "input_malformed", message: fmt.Sprintf("plan %s is not valid UTF-8 text", path), path: path}
		}
		if len(strings.TrimSpace(string(b))) == 0 {
			return &inputError{code: "input_malformed", message: fmt.Sprintf("plan %s has no content", path), path: path}
		}
	}
	return nil
}

// readPlans validates request files and computes their digests without
// writing runtime artifacts. Callers decide from the digests whether a
// captured copy is needed; deduplicated requests never persist one.
func readPlans(paths []string) ([]capturedPlan, error) {
	if err := validatePlanFiles(paths); err != nil {
		return nil, err
	}
	plans := make([]capturedPlan, 0, len(paths))
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		plans = append(plans, capturedPlan{Path: path, Digest: digest(b), Text: string(b)})
	}
	return plans, nil
}

// persistPlans copies the validated bytes into immutable runtime artifacts
// and records each artifact path on the same plans slice.
func persistPlans(dir, requestID string, plans []capturedPlan) error {
	base := filepath.Join(importsDir(dir), requestID)
	for i := range plans {
		artifact := filepath.Join(base, fmt.Sprintf("plan-%02d.md", i+1))
		if err := atomicWrite(artifact, []byte(plans[i].Text), 0600); err != nil {
			return err
		}
		plans[i].Artifact = artifact
	}
	return nil
}

// capturePlans reads and persists submitted bytes in one step, recording the
// original paths and digests. Capture happens before any acknowledgement of
// acceptance.
func capturePlans(dir, requestID string, paths []string) ([]capturedPlan, error) {
	plans, err := readPlans(paths)
	if err != nil {
		return nil, err
	}
	if err = persistPlans(dir, requestID, plans); err != nil {
		return nil, err
	}
	return plans, nil
}

// loadCapturedPlans reads artifacts back for queued receipts after restart.
func loadCapturedPlans(record *ImportRecord) ([]capturedPlan, error) {
	plans := make([]capturedPlan, 0, len(record.Artifacts))
	for i, artifact := range record.Artifacts {
		b, err := os.ReadFile(artifact)
		if err != nil {
			return nil, fmt.Errorf("import %s captured plan unavailable: %s: %w", record.RequestID, artifact, err)
		}
		if i < len(record.Digests) && digest(b) != record.Digests[i] {
			return nil, fmt.Errorf("import %s captured plan changed: %s", record.RequestID, artifact)
		}
		path := ""
		if i < len(record.SourcePaths) {
			path = record.SourcePaths[i]
		}
		plans = append(plans, capturedPlan{Path: path, Digest: digest(b), Artifact: artifact, Text: string(b)})
	}
	return plans, nil
}

// planImportRequest is the durable transport for additional requirements.
// It reuses the settings envelope with typed plan-import payloads.
type planImportRequest struct {
	Version        int      `json:"version"`
	ID             string   `json:"id"`
	Type           string   `json:"type"`
	Session        string   `json:"session"`
	Paths          []string `json:"paths"`
	CreatedAt      string   `json:"created_at,omitempty"`
	IdempotencyKey string   `json:"idempotency_key,omitempty"`
}

// submitPlanImportRequest writes a request for the active controller and
// returns it without waiting; the receipt names its identifier and creation
// time. The request survives controller restarts with its captured
// artifacts, so a session change is not a refusal; the advertised request
// types are still validated before publication. A caller key rides the
// envelope so the claiming controller resolves it against the retained
// receipts (AX-012).
func submitPlanImportRequest(dir, session, idempotencyKey string, paths []string) (*planImportRequest, error) {
	if err := validatePlanFiles(paths); err != nil {
		return nil, err
	}
	if idempotencyKey != "" {
		if err := validateIdempotencyKey(idempotencyKey); err != nil {
			return nil, &inputError{code: "option_value_invalid", message: err.Error()}
		}
	}
	if err := validateRequestPublication(dir, session, RequestTypePlanImport); err != nil {
		return nil, err
	}
	req := &planImportRequest{
		Version: SettingsRequestVersion, ID: identifier("import"),
		Type: RequestTypePlanImport, Session: session, Paths: paths,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), IdempotencyKey: idempotencyKey,
	}
	if err := writeJSON(filepath.Join(requestDir(dir), req.ID+".json"), req); err != nil {
		return nil, err
	}
	return req, nil
}

// submitPlanImport returns just the request identifier, for callers that
// follow the import through its receipt instead of its creation time.
func submitPlanImport(dir, session, idempotencyKey string, paths []string) (string, error) {
	req, err := submitPlanImportRequest(dir, session, idempotencyKey, paths)
	if err != nil {
		return "", err
	}
	return req.ID, nil
}

// claimPlanImports claims submitted plan-import requests, validates and
// captures them, and persists queued receipts before acknowledging
// acceptance. A queued artifact without that receipt is unaccepted.
func (e *Engine) claimPlanImports() {
	for {
		raw, err := e.claimRequest(RequestTypePlanImport)
		if err != nil {
			e.event("import-error", err.Error())
			return
		}
		if raw == nil {
			return
		}
		e.acceptPlanImport(raw, raw.Session != e.controller.Session)
		if e.err != nil {
			return
		}
		if err := os.Remove(filepath.Join(processingDir(e.Dir), raw.ID+".json")); err != nil {
			e.err = err
			return
		}
	}
}

// recoverPlanImports runs after state and pending exports recover, under the
// repository lock. Receipts own accepted content; source paths are read again
// only when a claim has no receipt. Such a claim was never acknowledged.
func (e *Engine) recoverPlanImports() error {
	// Restore missing acknowledgements, including receipts written by older
	// controllers whose session cleanup removed terminal acknowledgements, and
	// mirror every saved import into the common receipt store.
	for _, record := range e.State.Imports {
		e.writePlanAck(record)
		if e.err != nil {
			return e.err
		}
	}
	if len(e.State.Imports) > 0 {
		for _, record := range e.State.Imports {
			e.syncImportReceipt(record)
		}
		if err := e.save(); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(processingDir(e.Dir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(processingDir(e.Dir), entry.Name())
		req, err := loadSettingsRequest(path)
		if err != nil {
			return err
		}
		if req.Type != RequestTypePlanImport {
			continue
		}
		// A claim belongs to the interrupted controller. Recover it rather
		// than rejecting its old session. Acceptance replays an existing
		// receipt or compares fresh content with applied receipt digests.
		e.acceptPlanImport(req, false)
		if e.err != nil {
			return e.err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

// acceptPlanImport validates, captures, and receipts one import. Imports for
// an earlier session are rejected with a reload hint, mirroring settings. A
// caller key resolves against the retained receipts before any capture: the
// same key and content returns the original request's result, and changed
// content under the same key is refused (AX-012).
func (e *Engine) acceptPlanImport(req *SettingsRequest, staleSession bool) {
	requestID, key := req.ID, req.IdempotencyKey
	if record := e.importReceipt(requestID); record != nil {
		// Duplicate identifiers replay the recorded result.
		e.syncImportReceipt(record)
		e.writePlanAck(record)
		return
	}
	if staleSession {
		message := "the request targets an earlier controller session; resubmit it after reloading state"
		e.ackPlanImport(requestID, AckStale, message, nil)
		e.rejectImportReceipt(requestID, key, requestPayloadDigest(req, nil), message)
		return
	}
	plans, err := readPlans(req.Paths)
	if err != nil {
		e.ackPlanImport(requestID, AckError, err.Error(), nil)
		e.rejectImportReceipt(requestID, key, requestPayloadDigest(req, nil), err.Error())
		return
	}
	if key != "" {
		digest, original, conflict := e.resolveIdempotencyKey(req, planContentDigests(plans))
		if conflict != nil {
			message := keyConflictMessage(key, conflict, digest)
			e.ackPlanImport(requestID, AckError, message, nil)
			e.rejectImportReceipt(requestID, key, digest, message)
			return
		}
		if original != nil {
			// The key already recorded this content: answer with the original
			// request's result. A terminal original replays its outcome — an
			// applied one through the same content-dedup answer a fresh
			// duplicate gets — while an original still in flight refuses,
			// naming the request to poll. Neither re-analyzes the plans.
			if record := e.importReceipt(original.ID); record != nil {
				switch record.Status {
				case ImportApplied:
					e.ackPlanImport(requestID, ImportApplied, "", record)
					e.dedupImportReceipt(requestID, record)
					return
				case ImportFailed:
					e.ackPlanImport(requestID, AckError, record.Error, nil)
					e.dedupImportReceipt(requestID, record)
					return
				}
			}
			message := keyInFlightMessage(key, original)
			e.ackPlanImport(requestID, AckError, message, nil)
			e.rejectImportReceipt(requestID, key, digest, message)
			return
		}
	}
	if applied := e.appliedByDigest(plans); applied != nil {
		// Reply under the new request identifier while reusing the applied
		// result; the event names that recorded result. No import receipt
		// references a new capture, so none is written; the common receipt
		// records the deduplicated outcome under this identifier.
		e.ackPlanImport(requestID, ImportApplied, "", applied)
		e.dedupImportReceipt(requestID, applied)
		return
	}
	if err = persistPlans(e.Dir, requestID, plans); err != nil {
		e.ackPlanImport(requestID, AckError, err.Error(), nil)
		e.failImportReceipt(requestID, key, requestPayloadDigest(req, nil), err)
		return
	}
	record := &ImportRecord{
		RequestID: requestID, Status: ImportQueued,
		Enqueued:       time.Now().UTC().Format(time.RFC3339Nano),
		IdempotencyKey: key,
	}
	if key != "" {
		record.PayloadDigest = requestPayloadDigest(req, planContentDigests(plans))
	}
	for _, p := range plans {
		record.Digests = append(record.Digests, p.Digest)
		record.Artifacts = append(record.Artifacts, p.Artifact)
		record.SourcePaths = append(record.SourcePaths, p.Path)
	}
	e.State.Imports = append(e.State.Imports, record)
	// Durable acceptance commits with the receipt it owns: from this save on
	// the request survives restarts or receives an explicit terminal result
	// (AX-009).
	e.syncImportReceipt(record)
	if err = e.save(); err != nil {
		e.err = err
		return
	}
	e.requestEvent("import", fmt.Sprintf("Queued plan import %s (%d file(s)).", requestID, len(plans)), requestID)
	e.writePlanAck(record)
}

// syncImportReceipt mirrors one import receipt into the common request
// lifecycle store, so every import outcome stays queryable beside the
// settings, add-task, requeue, and stop receipts. The ImportRecord stays
// authoritative for the import; the mirror only restates its state.
func (e *Engine) syncImportReceipt(record *ImportRecord) {
	if e.State == nil || record == nil {
		return
	}
	e.upsertRequest(requestReceiptForImport(record, e.controllerSessionID()))
}

// rejectImportReceipt records one refused plan-import request. A refusal
// changed no saved work, so the receipt is the durable outcome the removed
// envelope leaves behind. A keyed refusal records the key with the digest the
// refusal could compute, so a later reuse of the key refuses against this
// request instead of replaying it.
func (e *Engine) rejectImportReceipt(requestID, key, payloadDigest, message string) {
	if e.State == nil {
		return
	}
	if rec := e.requestReceipt(requestID); rec != nil && requestStateTerminal(rec.State) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.upsertRequest(&RequestRecord{
		ID: requestID, Type: PublicRequestAddPlan, State: RequestRejected,
		Session: e.controllerSessionID(), SubmittedAt: now, DecidedAt: now, Error: message,
		IdempotencyKey: key, PayloadDigest: payloadDigest,
	})
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// failImportReceipt records one plan-import acceptance attempt that failed
// before the request was ever queued.
func (e *Engine) failImportReceipt(requestID, key, payloadDigest string, failure error) {
	if e.State == nil {
		return
	}
	if rec := e.requestReceipt(requestID); rec != nil && requestStateTerminal(rec.State) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.upsertRequest(&RequestRecord{
		ID: requestID, Type: PublicRequestAddPlan, State: RequestFailed,
		Session: e.controllerSessionID(), SubmittedAt: now, DecidedAt: now, Error: failure.Error(),
		IdempotencyKey: key, PayloadDigest: payloadDigest,
	})
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// dedupImportReceipt records the digest-deduplicated outcome under the
// requesting identifier, naming the recorded result that owns the content.
func (e *Engine) dedupImportReceipt(requestID string, applied *ImportRecord) {
	if e.State == nil || applied == nil {
		return
	}
	if rec := e.requestReceipt(requestID); rec != nil {
		return
	}
	rec := requestReceiptForImport(applied, e.controllerSessionID())
	rec.ID = requestID
	rec.Summary = importOutcome(applied.AddedTasks, applied.Covered) + "; matches import " + applied.RequestID
	e.upsertRequest(rec)
	if err := e.Store.Save(e.State); err != nil && e.err == nil {
		e.err = err
	}
}

// importReceipt finds a receipt by request identifier.
func (e *Engine) importReceipt(requestID string) *ImportRecord {
	for _, r := range e.State.Imports {
		if r.RequestID == requestID {
			return r
		}
	}
	return nil
}

// appliedByDigest matches already-applied content digests so repeated
// imports never add tasks twice.
func (e *Engine) appliedByDigest(plans []capturedPlan) *ImportRecord {
	return e.matchingImport(plans, ImportApplied)
}

func (e *Engine) matchingImport(plans []capturedPlan, statuses ...string) *ImportRecord {
	digests := map[string]bool{}
	for _, p := range plans {
		digests[p.Digest] = true
	}
	for _, r := range e.State.Imports {
		accepted := false
		for _, status := range statuses {
			accepted = accepted || r.Status == status
		}
		if !accepted || len(r.Digests) != len(digests) {
			continue
		}
		match := true
		for _, d := range r.Digests {
			if !digests[d] {
				match = false
			}
		}
		if match {
			return r
		}
	}
	return nil
}

// writePlanAck persists the acknowledgement file for a receipt.
func (e *Engine) writePlanAck(record *ImportRecord) {
	ack := planImportAck{
		Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: record.RequestID, Session: e.controller.Session,
		Status: record.Status, AddedTasks: record.AddedTasks, Covered: record.Covered,
		Conflicts: record.Conflicts, Error: record.Error, Adjustments: record.Adjustments, Gates: record.Gates,
	}
	if err := writeJSON(filepath.Join(ackDir(e.Dir), record.RequestID+".json"), ack); err != nil {
		e.err = err
	}
}

// waitImportReceipt mirrors one import's wait reason into its common
// receipt and reports whether the reason changed, so a held import records
// why it waits without rewriting the wait start on every boundary.
func (e *Engine) waitImportReceipt(record *ImportRecord, reason, detail string) bool {
	if e.State == nil || record == nil {
		return false
	}
	rec := e.requestReceipt(record.RequestID)
	if rec == nil {
		e.syncImportReceipt(record)
		rec = e.requestReceipt(record.RequestID)
	}
	if rec == nil || requestStateTerminal(rec.State) {
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

// importOutcome names one import's recorded result: the added task count,
// the coverage count, and the added task identifiers. Fresh applications and
// deduplicated replays report it in the same shape.
func importOutcome(added []string, covered []CoveredRef) string {
	detail := fmt.Sprintf("%d task(s) added, %d covered", len(added), len(covered))
	if len(added) > 0 {
		detail += " (" + strings.Join(added, ", ") + ")"
	}
	return detail
}

// ackPlanImport acknowledges one request outcome and writes its event. The
// message is a refusal or error reason; it rides in the acknowledgement error
// field and in the event. A digest-deduplicated replay passes the applied
// receipt without a message: its event then names the recorded result and the
// request that owns it, while the acknowledgement keeps its error-free shape.
func (e *Engine) ackPlanImport(id, status, message string, record *ImportRecord) {
	ack := planImportAck{Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: id, Session: e.controller.Session, Status: status}
	if message != "" {
		ack.Error = message
	}
	if record != nil {
		ack.AddedTasks, ack.Covered, ack.Conflicts = record.AddedTasks, record.Covered, record.Conflicts
		ack.Adjustments, ack.Gates = record.Adjustments, record.Gates
		if record.Status == ImportApplied {
			ack.Status = ImportApplied
		}
	}
	if err := writeJSON(filepath.Join(ackDir(e.Dir), id+".json"), ack); err != nil {
		e.err = err
	}
	detail := message
	if detail == "" && record != nil {
		detail = importOutcome(record.AddedTasks, record.Covered) + "; matches import " + record.RequestID
	}
	e.requestEvent("import", fmt.Sprintf("Plan import %s %s: %s", id, status, detail), id)
}

// planImportAck is the machine-readable request result.
type planImportAck struct {
	Version     int          `json:"version"`
	Type        string       `json:"type,omitempty"`
	ID          string       `json:"id"`
	Status      string       `json:"status"`
	Session     string       `json:"controller_session"`
	AddedTasks  []string     `json:"added_task_ids,omitempty"`
	Covered     []CoveredRef `json:"coverage,omitempty"`
	Conflicts   []string     `json:"conflicts,omitempty"`
	Adjustments []string     `json:"adjustments,omitempty"`
	Gates       []ImportGate `json:"gates,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// processPlanImports drives queued receipts at loop boundaries: wait for
// safe conditions, analyze, validate, and apply additively. A stop request
// takes precedence; queued imports remain for the next run. While the task
// file needs correction, new requests are still claimed and queued, but no
// import applies until the input is valid again.
func (e *Engine) processPlanImports(ctx context.Context) {
	e.claimPlanImports()
	if e.err != nil {
		return
	}
	if e.inputPending {
		return
	}
	for _, record := range e.State.Imports {
		if record.Status != ImportQueued && record.Status != ImportWaiting && record.Status != ImportPlanning {
			continue
		}
		if e.drainingNow() {
			// The controller's drain event explains why receipts stay queued.
			// Polling must not emit another outcome for every pending receipt.
			continue
		}
		if e.workRetained() {
			if record.Status != ImportWaiting {
				record.Status = ImportWaiting
				e.syncImportReceipt(record)
				if err := e.save(); err != nil {
					e.err = err
					return
				}
				e.requestEvent("import", "Plan import "+record.RequestID+" waits for active work to reach a boundary; new task dispatch is paused until the import applies.", record.RequestID)
			}
			continue
		}
		if err := e.applyPlanImport(ctx, record); err != nil {
			var bad *invalidBacklogError
			if errors.As(err, &bad) {
				// A corrected edit, not a new submission, unblocks this import.
				e.requestEvent("import", "Plan import "+record.RequestID+" waits for a corrected task file.", record.RequestID)
				if e.waitImportReceipt(record, "task_file_correction", "the task file needs correction before the import can apply") {
					if saveErr := e.save(); saveErr != nil {
						e.err = saveErr
						return
					}
				}
				continue
			}
			e.requestEvent("import-error", fmt.Sprintf("Plan import %s failed: %v", record.RequestID, err), record.RequestID)
			record.Status = ImportFailed
			record.Error = err.Error()
			e.syncImportReceipt(record)
			if err = e.save(); err != nil {
				e.err = err
				return
			}
			e.writePlanAck(record)
			continue
		}
	}
}

// importDispatchHold reports whether an accepted import pauses new ordinary
// task dispatch. Active attempts, repairs, checks, and publications keep
// running to their safe boundary; the import applies there and releases the
// pause. A failed analysis releases it the same way, because a terminal
// receipt holds no dispatch. An import waiting for a corrected task file
// keeps the pause, exactly as the input wait already holds dispatch.
func (e *Engine) importDispatchHold() bool {
	for _, r := range e.State.Imports {
		switch r.Status {
		case ImportQueued, ImportWaiting, ImportPlanning:
			return true
		}
	}
	return false
}

// workRetained reports whether unfinished attempts or pending publications
// still own the current contract.
func (e *Engine) workRetained() bool {
	if e.State.Publication != nil {
		return true
	}
	for _, a := range e.State.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
		default:
			return true
		}
	}
	return false
}

// stagedImport snapshots one plan-import application: the receipt and the
// backlog fields it mutates. Rollback restores them when the saved
// state/export protocol fails before committing the application.
type stagedImport struct {
	record        ImportRecord
	backlog       Backlog
	planSignature string
	finished      bool
	reviewedTip   string
	requests      []*RequestRecord
}

func (s stagedImport) rollback(e *Engine, record *ImportRecord) {
	*record = s.record
	e.State.Backlog = s.backlog
	e.State.PlanSignature = s.planSignature
	e.State.Finished = s.finished
	e.State.ReviewedTip = s.reviewedTip
	e.State.Requests = s.requests
}

// applyPlanImport refreshes the baseline, analyzes the captured plans, and
// applies additive scope in one journalled transition.
func (e *Engine) applyPlanImport(ctx context.Context, record *ImportRecord) error {
	plans, err := loadCapturedPlans(record)
	if err != nil {
		return err
	}
	if record.Status != ImportPlanning {
		record.Status = ImportPlanning
		e.syncImportReceipt(record)
		if err = e.save(); err != nil {
			return err
		}
		e.writePlanAck(record)
		// Analysis runs the agent synchronously and blocks the loop, so the
		// next boundary publish would only arrive after it finishes. Publish
		// now so dashboards see the planning state while the agent works.
		e.publishView(false)
	}
	// Refresh the baseline through the existing checks before analysis.
	if err = e.refreshIdle(); err != nil {
		return err
	}
	// Compare analysis against the refreshed baseline.
	backlogBefore := digestFile(e.Config.Todo)
	tipBefore := e.State.Tip
	proposal, err := e.analyzeImport(ctx, record.RequestID, plans)
	if err != nil {
		return err
	}
	if len(proposal.Conflicts) > 0 {
		record.Conflicts = proposal.Conflicts
		return fmt.Errorf("the plan conflicts with existing scope: %s", strings.Join(proposal.Conflicts, "; "))
	}
	if e.State.Tip != tipBefore {
		return fmt.Errorf("the project changed during analysis; resubmit the import")
	}
	// Equivalent formatting during analysis changed no content (AX-018), so
	// the analysis stays valid; a real edit still forces a resubmission.
	if digestFile(e.Config.Todo) != backlogBefore {
		if edit := e.classifyBacklogFile(); !edit.Equivalent {
			return fmt.Errorf("the project changed during analysis; resubmit the import")
		}
	}
	// The additive scope and the applied receipt stage until the saved
	// state/export protocol commits them. A failure before that commit rolls
	// both back, so a failed receipt never leaves tasks behind that only the
	// in-memory backlog held, and a resubmission cannot duplicate them. The
	// snapshot precedes applyProposal, which is the first mutating step.
	staged := stagedImport{
		record: clone(*record), backlog: clone(e.State.Backlog),
		planSignature: e.State.PlanSignature, finished: e.State.Finished, reviewedTip: e.State.ReviewedTip,
		requests: append([]*RequestRecord{}, e.State.Requests...),
	}
	added, covered, err := e.applyProposal(proposal)
	if err != nil {
		return err
	}
	record.Status = ImportApplied
	record.AddedTasks = added
	record.Covered = covered
	record.Adjustments = proposal.Adjustments
	record.Gates = proposal.Gates
	record.Applied = time.Now().UTC().Format(time.RFC3339Nano)
	e.syncImportReceipt(record)
	// Scope expansion invalidates execution planning and satisfaction. Keep
	// the live plan: the dispatch loop reads scopes from it while work runs,
	// and the cleared signature forces the next planning pass to rebuild.
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	if err = e.save(); err != nil {
		staged.rollback(e, record)
		return err
	}
	if err = e.export(); err != nil {
		staged.rollback(e, record)
		return err
	}
	detail := fmt.Sprintf("Applied plan import %s: %s", record.RequestID, importOutcome(added, covered))
	// The added task identities ride beside the request identity: one import
	// mutates many tasks, so the payload lists them instead of naming one.
	applied := e.identity()
	applied.RequestID = record.RequestID
	data := map[string]any{}
	if len(added) > 0 {
		data["tasks"] = added
	}
	e.eventData("import", detail+".", applied.merge(data))
	e.writePlanAck(record)
	return nil
}

func digestFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return digest(b)
}

// applyProposal allocates unique task identifiers, resolves proposal-local
// dependencies, and validates the combined backlog before any mutation.
// Already covered requirements reference existing tasks instead of creating
// duplicates.
func (e *Engine) applyProposal(proposal *importProposal) ([]string, []CoveredRef, error) {
	if len(proposal.Additions) == 0 && len(proposal.Covered) == 0 && len(proposal.Gates) == 0 {
		return nil, nil, fmt.Errorf("the analysis proposed neither additions, coverage, nor gates")
	}
	next := len(e.State.Backlog.Tasks) + 1
	alloc := func() string {
		for {
			id := fmt.Sprintf("T%03d", next)
			next++
			if e.State.Backlog.Task(id) == nil {
				return id
			}
		}
	}
	local := map[string]string{}
	tasks := make([]Task, 0, len(proposal.Additions))
	for i := range proposal.Additions {
		t := proposal.Additions[i]
		if strings.TrimSpace(t.Title) == "" || strings.TrimSpace(t.Details) == "" {
			return nil, nil, fmt.Errorf("addition %d lacks a title or acceptance criteria", i+1)
		}
		if strings.TrimSpace(t.ID) == "" || local[t.ID] != "" {
			return nil, nil, fmt.Errorf("addition %d has an empty or duplicate identifier", i+1)
		}
		local[t.ID] = alloc()
	}
	for i := range proposal.Additions {
		t := proposal.Additions[i]
		t.ID = local[t.ID]
		t.Status = "todo"
		if t.Deferred() {
			t.Status = "blocked"
		}
		t.Priority = max(1, min(5, t.Priority))
		deps := make([]string, 0, len(t.DependsOn))
		for _, d := range t.DependsOn {
			if mapped, ok := local[d]; ok {
				deps = append(deps, mapped)
				continue
			}
			if e.State.Backlog.Task(d) == nil {
				return nil, nil, fmt.Errorf("addition %s depends on unknown task %s", t.ID, d)
			}
			deps = append(deps, d)
		}
		t.DependsOn = deps
		tasks = append(tasks, t)
	}
	for _, ref := range proposal.Covered {
		if ref.TaskID != "" && e.State.Backlog.Task(ref.TaskID) == nil {
			return nil, nil, fmt.Errorf("coverage references unknown task %s", ref.TaskID)
		}
	}
	// Validate the combined backlog before mutating anything.
	combined := clone(e.State.Backlog)
	combined.Tasks = append(combined.Tasks, tasks...)
	resolvedGates := make([]ImportGate, 0, len(proposal.Gates))
	for _, gate := range proposal.Gates {
		target := e.State.Backlog.Task(gate.TaskID)
		if target == nil {
			return nil, nil, fmt.Errorf("gate references unknown task %s", gate.TaskID)
		}
		if target.Status == "done" || target.Status == "doing" {
			return nil, nil, fmt.Errorf("gate cannot change completed or active task %s", gate.TaskID)
		}
		for _, attempt := range e.State.Attempts {
			if attempt.Task.ID == gate.TaskID && attempt.Phase != "done" && attempt.Phase != "blocked" {
				return nil, nil, fmt.Errorf("gate cannot change retained task %s", gate.TaskID)
			}
		}
		if len(gate.DependsOn) == 0 {
			return nil, nil, fmt.Errorf("gate for %s has no dependencies", gate.TaskID)
		}
		resolved := ImportGate{TaskID: gate.TaskID}
		for _, dep := range gate.DependsOn {
			if mapped, ok := local[dep]; ok {
				dep = mapped
			}
			if combined.Task(dep) == nil {
				return nil, nil, fmt.Errorf("gate %s depends on unknown task %s", gate.TaskID, dep)
			}
			resolved.DependsOn = mergeUnique(resolved.DependsOn, []string{dep})
		}
		task := combined.Task(gate.TaskID)
		task.DependsOn = mergeUnique(task.DependsOn, resolved.DependsOn)
		resolvedGates = append(resolvedGates, resolved)
	}
	if err := combined.Validate(); err != nil {
		return nil, nil, fmt.Errorf("combined backlog is invalid: %w", err)
	}
	e.State.Backlog = combined
	proposal.Gates = resolvedGates
	added := make([]string, 0, len(tasks))
	for _, t := range tasks {
		added = append(added, t.ID)
	}
	return added, proposal.Covered, nil
}

// appliedContext never drops accepted requirements when evidence is missing.
func (e *Engine) appliedContext() ([]contextDocument, error) {
	var documents []contextDocument
	for _, r := range e.State.Imports {
		if r.Status != ImportApplied {
			continue
		}
		plans, err := loadCapturedPlans(r)
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			documents = append(documents, contextDocument{Import: r.RequestID, Source: p.Path, Text: []byte(p.Text)})
		}
	}
	return documents, nil
}

// waitForPlanImport polls a request's acknowledgement until it reaches a
// terminal status or the timeout. A timeout reports a pending import; the
// import itself keeps running and a later controller can still apply it.
func waitForPlanImport(dir, id string, timeout time.Duration) (*planImportAck, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var ack planImportAck
		if err := readJSON(filepath.Join(ackDir(dir), id+".json"), &ack); err == nil {
			switch ack.Status {
			case ImportApplied, ImportFailed, AckError, AckStale:
				return &ack, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return nil, &pendingError{
		message: fmt.Sprintf("import %s is still pending; watch it with looper tui", id),
		request: id, waitKind: "plan-import acknowledgement",
	}
}

// sortStrings orders deterministically for output.
func sortStrings(s []string) { sort.Strings(s) }
