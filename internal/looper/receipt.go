package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The immediate mutation receipt (AX-011),
// docs/contracts/schemas/v2/request-receipt.schema.json. Every mutation —
// add-task, add-plan, settings, requeue, and stop — returns this document as
// its default response right after the request is durable. The receipt names
// the request, never the applied result: the caller queries the request
// record, or passes --wait for one bounded wait whose timeout is an outcome
// of the wait, never a cancellation or rejection.
const (
	requestReceiptSchemaVersion = 2
	requestReceiptKind          = "looper.request.receipt"
)

// requestReceiptDocument is the one JSON document a mutation returns
// immediately. It validates against the receipt schema under both contract
// selections: the immediate response is one shape whatever the caller
// negotiated, and only the explicit --wait keeps the older acknowledgement
// documents.
type requestReceiptDocument struct {
	ContractVersion int                   `json:"contract_version"`
	SchemaVersion   int                   `json:"schema_version"`
	Kind            string                `json:"kind"`
	Request         requestReceiptRequest `json:"request"`
	// Wait names the finite wait this request can be observed with (AX-013).
	Wait  *requestReceiptWait `json:"wait,omitempty"`
	Query requestReceiptQuery `json:"query"`
}

type requestReceiptRequest struct {
	ID                string `json:"id"`
	Type              string `json:"type"`
	State             string `json:"state"`
	CreatedAt         string `json:"created_at"`
	ControllerSession string `json:"controller_session,omitempty"`
	// IdempotencyKey, PayloadDigest, and Duplicate carry the caller-key
	// semantics (AX-012): the key this submission was made under, the
	// canonical digest of its payload, and whether the receipt returns an
	// earlier request the same key and payload already recorded.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	PayloadDigest  string `json:"payload_digest,omitempty"`
	Duplicate      bool   `json:"duplicate,omitempty"`
}

type requestReceiptQuery struct {
	Command string `json:"command"`
}

// requestReceiptWait names the finite wait this request can be waited on with
// and the bound it suggests (AX-013). It is the wait object the receipt schema
// has carried since AX-001 defined it: the command names `looper wait` and the
// bound names the seconds it runs.
type requestReceiptWait struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Note           string `json:"note"`
}

// newMutationReceipt builds the receipt for one published request. state is
// the lifecycle state the durable evidence holds at return time: submitted
// for a fresh envelope, whose acceptance the controller records separately.
func newMutationReceipt(id, requestType, state, createdAt, session string) *requestReceiptDocument {
	return &requestReceiptDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   requestReceiptSchemaVersion,
		Kind:            requestReceiptKind,
		Request: requestReceiptRequest{
			ID: id, Type: requestType, State: state,
			CreatedAt: createdAt, ControllerSession: session,
		},
		Query: requestReceiptQuery{Command: requestQueryCommand(id)},
	}
}

// keyedMutationReceipt marks one receipt with its caller key (AX-012): the
// key, the canonical payload digest, and whether the receipt returns an
// earlier request recorded under the same key and payload.
func keyedMutationReceipt(r *requestReceiptDocument, key, payloadDigest string, duplicate bool) *requestReceiptDocument {
	r.Request.IdempotencyKey, r.Request.PayloadDigest, r.Request.Duplicate = key, payloadDigest, duplicate
	return r
}

// requestQueryCommand names the read-only command that returns this request's
// record. The record route requires the version 2 contract, so the command
// selects it; a plain text caller runs it without --json.
func requestQueryCommand(id string) string {
	if !statusRouteIDOk(id) {
		return "looper requests --contract v2 --json"
	}
	return "looper requests " + id + " --contract v2"
}

// emitMutationReceipt prints one immediate receipt and returns the exit code.
// A JSON caller receives exactly one document; a plain caller receives the
// same facts as text. summary and note are plain-text context — what was
// submitted, and any condition the caller should know now — because the
// receipt schema carries no free-text field.
func emitMutationReceipt(c Config, r *requestReceiptDocument, summary, note string, out io.Writer) int {
	if r.Wait == nil {
		// The wait object names the finite wait this request can be observed
		// with and the bound the caller's own --wait would use (AX-013). Its
		// timeout is an outcome of the wait: the request keeps running.
		bound := c.waitBound()
		r.Wait = &requestReceiptWait{
			Command:        waitRequestCommand(r.Request.ID, bound),
			TimeoutSeconds: max(1, int(bound/time.Second)),
			Note:           "A wait timeout is an outcome of the wait; it never cancels the request.",
		}
	}
	if c.JSON {
		if err := json.NewEncoder(out).Encode(r); err != nil {
			return exitOperationFailed
		}
		return exitOK
	}
	if summary != "" {
		fmt.Fprintln(out, summary)
	}
	session := ""
	if r.Request.ControllerSession != "" {
		session = " to controller session " + r.Request.ControllerSession
	}
	if r.Request.Duplicate {
		fmt.Fprintf(out, "Request %s [%s] returned for idempotency key %s; state %s.\n",
			r.Request.ID, r.Request.Type, r.Request.IdempotencyKey, r.Request.State)
	} else {
		fmt.Fprintf(out, "Request %s [%s] submitted%s.\n", r.Request.ID, r.Request.Type, session)
	}
	if note != "" {
		fmt.Fprintln(out, note)
	}
	if r.Request.Duplicate {
		fmt.Fprintf(out, "Query the outcome with: %s\n", r.Query.Command)
		printReceiptWait(r, out)
		return exitOK
	}
	fmt.Fprintf(out, "The controller owns the outcome. Query it with: %s\n", r.Query.Command)
	printReceiptWait(r, out)
	return exitOK
}

// printReceiptWait names the finite wait beside the query route. The plain
// form keeps the same bound the JSON document carries.
func printReceiptWait(r *requestReceiptDocument, out io.Writer) {
	if r.Wait == nil || !strings.HasPrefix(r.Wait.Command, "looper wait") {
		return
	}
	fmt.Fprintf(out, "Wait for the outcome with: %s\n", r.Wait.Command)
}

// waitBound resolves the bounded wait one mutation command uses. --wait-timeout
// wins; otherwise the command family's documented default applies: the
// acknowledgement wait for settings-family requests and the plan-import wait
// for add-plan. Both are finite, so a wait always ends in an answer or a
// timeout outcome.
func (c Config) waitBound() time.Duration {
	if c.WaitTimeout > 0 {
		return c.WaitTimeout
	}
	if c.Command == "add-plan" {
		return planImportWaitJSON
	}
	return requestSubmitWait
}

// requestRecordForWait reads the record a finished --wait reports. The
// terminal acknowledgement the caller waited for commits with its receipt, so
// the record carries the applied result — task identifiers, plan coverage,
// adjustments, and gates — beside the lifecycle state. The acknowledgement
// stays the fallback: a record the store cannot serve yet answers from the
// acknowledgement the wait already observed.
func requestRecordForWait(dir, id string, fallback *requestQueryRecord) (*requestRecordDocument, error) {
	if store, err := openObservedStore(dir); err == nil {
		if snap, snapErr := store.Snapshot(); snapErr == nil && snap.State != nil {
			if rec := collectRequestEvidence(dir, snap.State, wallClock()).byID[id]; rec != nil {
				store.Close()
				return buildSingleRequestDocument(rec), nil
			}
		}
		store.Close()
	}
	if fallback != nil {
		return buildSingleRequestDocument(fallback), nil
	}
	return nil, requestNotFoundError(id, 0)
}

// emitWaitRecord reports one finished version 2 wait: the request record the
// caller waited for, read from the run's durable evidence with the
// acknowledgement the wait observed as its fallback. JSON and plain text
// render from the same document.
func emitWaitRecord(c Config, fallback *requestQueryRecord, id string, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	doc, err := requestRecordForWait(dir, id, fallback)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	if c.JSON {
		return encodeRequestDocument(doc, out)
	}
	renderRequestRecord(doc, out)
	return exitOK
}

// ackRecordView builds one record view from a terminal acknowledgement, for
// the window between an acknowledgement the controller wrote and the receipt
// read that must already sit behind it. The acknowledgement is durable
// evidence: it states the decision the record reports.
func ackRecordView(id, requestType, state, summary string, affected []string) *requestQueryRecord {
	return &requestQueryRecord{
		id: id, typ: requestType, state: state, now: wallClock(),
		summary: summary, affectedTasks: affected,
	}
}

// importAckRecordView maps one terminal plan-import acknowledgement to its
// record view, keeping the applied evidence AX-011 preserves in text and
// JSON: the added task identifiers, coverage, conflicts, adjustments, and
// gates.
func importAckRecordView(ack *planImportAck) *requestQueryRecord {
	state := RequestFailed
	if ack.Status == ImportApplied {
		state = RequestApplied
	}
	q := ackRecordView(ack.ID, PublicRequestAddPlan, state, importOutcome(ack.AddedTasks, ack.Covered), ack.AddedTasks)
	coverage := &requestPlanCoverage{
		AddedTasks: ack.AddedTasks, Conflicts: ack.Conflicts,
		Adjustments: ack.Adjustments,
		Covered:     coveredRefsOf(ack.Covered),
	}
	for _, gate := range ack.Gates {
		coverage.Gates = append(coverage.Gates, requestGateRef{TaskID: gate.TaskID, DependsOn: gate.DependsOn})
	}
	q.coverage = coverage
	q.errText = ack.Error
	return q
}

// settingsAckRecordView maps one terminal settings-family acknowledgement to
// its record view.
func settingsAckRecordView(ack *SettingsAck, requestType string) *requestQueryRecord {
	state, summary := RequestRejected, ack.Message
	if ack.Status == AckApplied || ack.Status == AckReset {
		state = RequestApplied
		summary = ack.Status + ": " + ack.Message
	}
	q := ackRecordView(ack.ID, requestType, state, summary, nil)
	q.errText = ack.Message
	if state == RequestApplied {
		q.appliedFields = settingsAckFields(ack)
	}
	return q
}

// settingsAckFields lists the keys one applied settings acknowledgement
// changed, in a stable order.
func settingsAckFields(ack *SettingsAck) []string {
	keys := make([]string, 0, len(ack.Outcomes))
	for key := range ack.Outcomes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// drainingImportNote explains, at submission time, the one condition the
// receipt cannot carry in its schema-bound fields: a controller already
// draining applies no import before it stops, so a plan submitted now stays
// queued for the next controller run (AX-011's draining case).
func drainingImportNote(dir string, info *ControllerInfo) string {
	if info == nil {
		return ""
	}
	req, stat, err := readStopRequest(dir)
	if err != nil || req == nil {
		return ""
	}
	targets := false
	switch {
	case req.Session != "":
		targets = req.Session == info.Session
	case req.PID != 0:
		targets = req.PID == info.PID
	default:
		if started, parseErr := time.Parse(time.RFC3339, info.Started); parseErr == nil && stat != nil {
			targets = !stat.ModTime().Before(started)
		}
	}
	if !targets {
		return ""
	}
	return "The controller is draining: the import stays queued and the next controller run applies it."
}

// heldInputNote names the held-input case at submission time: a task file
// whose content changed against the accepted export makes the controller
// answer add-task and requeue requests with a conflict until its boundary
// imports the edit. The check mirrors the engine's equivalence rule — a
// formatting-only edit adopts and never holds — so the note never fires for
// bytes the controller would accept.
func heldInputNote(c Config, dir string) string {
	store, err := openObservedStore(dir)
	if err != nil {
		return ""
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil || snap.State == nil {
		return ""
	}
	if snap.State.StartupSeed {
		// A startup seed (AX-022) has no accepted export to compare against:
		// the controller is still creating the first backlog.
		return ""
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil || digest(data) == snap.State.ExportHash {
		return ""
	}
	if semantics := snap.State.acceptedSemantics(); semantics != "" {
		if parsed, parseErr := decodeBacklogStrict(data); parseErr == nil && parsed.semanticFingerprint() == semantics {
			return ""
		}
	}
	return "The task file changed outside Looper: the controller imports the edit at its next boundary before it answers this request."
}

// mutationStoreExists reports whether the runtime directory holds a state
// database, so held-input probes skip projects without a saved run.
func mutationStoreExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "state.db"))
	return err == nil
}
