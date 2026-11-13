package looper

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Transport exit codes from docs/contracts/exit-codes.json. Under contract
// version 2 a code reports how the call ended on the transport; the operation
// outcome is a field of the returned document. The legacy contract keeps
// today's 0, 1, and 2 meanings.
const (
	exitOK              = 0
	exitOperationFailed = 1
	exitUsage           = 2
	exitUnavailable     = 3
	exitIncompatible    = 4
	exitWaitTimeout     = 5
	exitCancelled       = 6
)

// Retry guidance values from docs/contracts/schemas/v2/error.schema.json.
const (
	retryYes         = "yes"
	retryNo          = "no"
	retryAfterChange = "after-change"
)

// The version 2 error document identity.
const (
	errorSchemaVersion = 2
	errorKind          = "looper.error"
)

// Bounds the error schema enforces. Values are clamped, never dropped into
// invalid JSON.
const (
	maxErrorPaths    = 32
	maxErrorEvidence = 16
	maxErrorActions  = 8
	maxErrorUnknown  = 16
	maxErrorString   = 1024
	maxErrorMessage  = 2000
)

// ErrorEvidence references retained evidence a caller can read on its own.
type ErrorEvidence struct {
	Kind      string // event, log, file, or request
	Reference string
	Command   string // a read-only command that retrieves the evidence
}

// ErrorAction is one supported next step. A step with only a description
// names an operation that has no single command yet.
type ErrorAction struct {
	Description string
	Command     string
	Operation   string
}

// PublicError is the structured failure every CLI command reports. One set of
// fields renders the version 2 error document and the plain text, so a caller
// distinguishes retry, changed input, and unavailable evidence from the code
// and the fields instead of parsing prose. Errors never carry caller file
// contents, command arguments, or private agent reasoning.
type PublicError struct {
	Code            string
	Message         string
	Retry           string
	Paths           []string
	Expected        map[string]any
	Observed        map[string]any
	Evidence        []ErrorEvidence
	Actions         []ErrorAction
	UnknownEvidence []string

	// ExitCode is the transport exit code under contract version 2.
	ExitCode int

	// legacyExit is the exit code the legacy contract returns. It is set at
	// the construction site because the same failure class can exit 1 or 2
	// depending on which command reports it.
	legacyExit int

	// legacy, when set, pins the exact pre-contract text and stream so the
	// default contract's output stays unchanged.
	legacy *legacyRender
}

type legacyRender struct {
	text   string
	stdout bool
	code   int
}

func (e *PublicError) Error() string { return e.Message }

func (e *PublicError) withLegacy(text string, code int) *PublicError {
	e.legacy = &legacyRender{text: text, code: code}
	return e
}

func (e *PublicError) withLegacyStdout(text string, code int) *PublicError {
	e.legacy = &legacyRender{text: text, code: code, stdout: true}
	return e
}

// operationFailure reports a terminal refused or failed operation.
func operationFailure(code, message string) *PublicError {
	return &PublicError{Code: code, Message: message, Retry: retryAfterChange, ExitCode: exitOperationFailed, legacyExit: exitOperationFailed}
}

// usageFailure reports an invalid command line or input: nothing was
// attempted and nothing was written.
func usageFailure(code, message string) *PublicError {
	return &PublicError{Code: code, Message: message, Retry: retryAfterChange, ExitCode: exitUsage, legacyExit: exitUsage}
}

// unavailableFailure reports state the command could not read.
func unavailableFailure(code, message string) *PublicError {
	return &PublicError{Code: code, Message: message, Retry: retryAfterChange, ExitCode: exitUnavailable, legacyExit: exitOperationFailed}
}

// incompatibleFailure reports a client and controller that cannot work
// together: an unsupported capability, contract, or session.
func incompatibleFailure(code, message string) *PublicError {
	return &PublicError{Code: code, Message: message, Retry: retryAfterChange, ExitCode: exitIncompatible, legacyExit: exitOperationFailed}
}

// pendingError marks a bounded wait that ended without an answer. The
// submitted request keeps running; a timeout never cancels or rejects it.
type pendingError struct {
	message  string
	request  string
	waitKind string
}

func (e *pendingError) Error() string { return e.message }

// inputError marks rejected caller input. Nothing was attempted and nothing
// was written, so the transport class is usage.
type inputError struct {
	code     string // input_unreadable or input_malformed
	message  string
	path     string
	expected map[string]any
	observed map[string]any
}

func (e *inputError) Error() string { return e.message }

// retentionError marks a guard that retained work instead of acting. The
// structured form carries the saved and observed values the guard compared,
// and the concrete differences AX-015 requires: bounded changed-path evidence
// with its detail route, the parsed backlog comparison, and the evidence that
// stayed unknown.
type retentionError struct {
	code     string
	message  string
	release  string // runtime directory holding the retained work
	todo     string
	root     string // resolved project root, when revisions or trees were compared
	expected map[string]any
	observed map[string]any
	changed  []pathDifference
	backlog  *backlogDifference
	unknown  []string
	// actions carries the guard-specific recovery steps the document lists
	// before its generic ones, for example the accepted-byte restore a
	// blocked export journal names (AX-009).
	actions []ErrorAction
}

func (e *retentionError) Error() string { return e.message }

// runNotFoundError reports that no saved run exists for a command that needs
// one. startHint names the command that creates the run.
func runNotFoundError(statePath, startHint string) *PublicError {
	p := unavailableFailure("run_not_found", "No saved Looper run exists in this project.")
	p.Paths = []string{statePath}
	p.Observed = map[string]any{"saved_run": false}
	p.Actions = []ErrorAction{{
		Description: "Start or resume a controller in this project.", Command: startHint,
	}}
	return p
}

// runNotFoundForRun enriches a no-saved-run error with the resolved project
// root, the task path, and the searched base and runtime locations (AX-025),
// so a caller that landed in the wrong working directory or with the wrong
// LOOPER_BASE_DIR sees every location that was searched. The discovery
// command joins as a next action: it lists every saved run under the base
// directory.
func runNotFoundForRun(c Config, statePath, startHint string) *PublicError {
	p := runNotFoundError(statePath, startHint)
	paths := []string{}
	if statePath != "" {
		paths = append(paths, statePath)
	}
	if _, runtime := projectPaths(c); runtime != "" {
		if runtimeDB := filepath.Join(runtime, "state.db"); statePath == "" || runtimeDB != statePath {
			paths = append(paths, runtimeDB)
		}
		if c.BaseDir != "" {
			paths = append(paths, c.BaseDir)
		}
	}
	for _, path := range []string{c.Root, c.Todo} {
		if path != "" {
			paths = append(paths, path)
		}
	}
	p.Paths = clampList(paths, maxErrorPaths)
	p.Observed = map[string]any{
		"saved_run": false,
		"base_dir":  clampString(c.BaseDir, maxErrorString),
	}
	p.Actions = append(p.Actions, ErrorAction{
		Description: "Discover the saved runs under the configured base directory.",
		Command:     "looper runs --contract v2 --json",
	})
	return p
}

// stateUnreadableError reports a state database that exists but cannot be
// read. The cause stays in the message; the file stays untouched.
func stateUnreadableError(statePath string, err error) *PublicError {
	p := unavailableFailure("state_unreadable", err.Error())
	p.Paths = []string{statePath}
	p.Actions = []ErrorAction{{
		Description: "Check the path and permissions, then read the run again.",
		Command:     "looper doctor",
	}}
	return p
}

// taskNotFoundError reports an inspect target that no backlog contains.
func taskNotFoundError(taskID string, total int) *PublicError {
	p := operationFailure("task_not_found", fmt.Sprintf("unknown task %q", taskID))
	p.Observed = map[string]any{"task_id": taskID, "backlog_tasks": total}
	p.Actions = []ErrorAction{{
		Description: "List the task identifiers and statuses the run saved.",
		Command:     "looper status --json",
	}}
	return p
}

// controllerMetadataError reports a repository a controller owns while its
// metadata cannot be verified, so no request can be targeted safely.
func controllerMetadataError(err error) *PublicError {
	p := unavailableFailure("controller_metadata_unreadable", "A controller owns this repository, but its metadata cannot be verified: "+err.Error())
	p.Actions = []ErrorAction{{
		Description: "Read the run state directly; the lock decides ownership.",
		Command:     "looper status --json",
	}}
	return p
}

// capabilityError renders the pre-contract incompatibility result for a
// controller that does not accept the request type.
func capabilityError(e *unsupportedRequestError) *PublicError {
	p := incompatibleFailure("capability_unsupported", e.Error())
	p.Expected = map[string]any{"controller_request_types": strings.Join(e.supported, ", ")}
	p.Observed = map[string]any{"requested_request_type": e.requestType, "controller_session": e.session}
	p.Evidence = []ErrorEvidence{{
		Kind: "request", Reference: "advertised capabilities of controller session " + e.session,
		Command: "looper version --contract v2 --json",
	}}
	p.Actions = []ErrorAction{{
		Description: "Read the capabilities this controller advertises before submitting.",
		Command:     "looper version --contract v2 --json",
	}}
	if e.absentMetadata {
		p.UnknownEvidence = []string{"the controller's full capabilities; its metadata predates capability advertisement"}
		p.Actions = append(p.Actions, ErrorAction{
			Description: "Restart the controller from a build that supports this request type, then resubmit.",
		})
	} else {
		p.Actions = append(p.Actions, ErrorAction{
			Description: "Resubmit to a controller that supports this request type.",
		})
	}
	return p
}

// staleSessionErrorDoc renders the stale-session outcome: the controller the
// request targeted no longer owns the repository.
func staleSessionErrorDoc(e *staleSessionError) *PublicError {
	p := incompatibleFailure("controller_session_stale", e.Error())
	p.Expected = map[string]any{"controller_session": e.targeted}
	p.Observed = map[string]any{"controller_session": e.current}
	p.Actions = []ErrorAction{{
		Description: "Check which controller owns the repository now, then resubmit against it.",
		Command:     "looper status --json",
	}}
	return p
}

// cursorErrorDoc renders an invalidated continuation cursor with the store
// values it was checked against.
func cursorErrorDoc(e *cursorInvalidError) *PublicError {
	identity := e.storeID
	if identity == "" {
		identity = "not recorded"
	}
	p := operationFailure(e.Code(), e.reason)
	p.Expected = map[string]any{
		"store_epoch": e.cursor.StoreEpoch, "store_identity": e.cursor.StoreID,
		"event_id_at_most": e.highWater,
	}
	p.Observed = map[string]any{"store_epoch": e.epoch, "store_identity": identity}
	p.Evidence = []ErrorEvidence{{
		Kind: "event", Reference: fmt.Sprintf("committed high-water mark %d", e.highWater),
		Command: "looper events --json",
	}}
	p.Actions = []ErrorAction{{
		Description: "Restart observation from the current event store.",
		Command:     "looper events",
	}}
	return p
}

// retentionErrorDoc renders a retained-work guard with its saved and observed
// values, the bounded changed-path evidence and its detail routes, the
// retained task identities, and the evidence that stayed unknown. The
// recovery preview stays owned by AX-016; the actions here name commands that
// exist today.
func retentionErrorDoc(e *retentionError) *PublicError {
	p := operationFailure(e.code, e.message)
	p.Expected, p.Observed = e.expected, e.observed
	if e.root != "" {
		p.Paths = append(p.Paths, e.root)
	}
	if e.todo != "" {
		p.Paths = append(p.Paths, e.todo)
	}
	if e.release != "" {
		p.Paths = append(p.Paths, e.release)
		p.Evidence = append(p.Evidence, ErrorEvidence{
			Kind: "file", Reference: "retained attempt directories under " + filepath.Base(e.release),
			Command: "looper status --json",
		})
	}
	for _, d := range e.changed {
		d.apply(p)
	}
	if e.backlog != nil {
		e.backlog.apply(p)
	}
	p.Actions = append(append([]ErrorAction{}, e.actions...),
		ErrorAction{Description: "Preview the supported recovery choices for this retained work without changing anything.", Command: "looper recovery --preview --contract v2 --json"},
		ErrorAction{Description: "Read the health findings and retained work for this project.", Command: "looper doctor"},
	)
	if ids, ok := e.observed["retained_tasks"].([]string); ok && len(ids) > 0 {
		p.Actions = append(p.Actions, ErrorAction{
			Description: "Read the retained attempt, its checks, and its logs for " + ids[0] + ".",
			Command:     "looper inspect " + ids[0] + " --json",
		})
	}
	p.Actions = append(p.Actions, ErrorAction{
		Description: "Restore the saved values named above, then resume; verified work is never repeated.",
		Command:     "looper",
	})
	p.UnknownEvidence = append(p.UnknownEvidence, e.unknown...)
	if len(p.UnknownEvidence) == 0 {
		if e.code == "backlog_contract_changed" || e.code == "task_contract_changed" {
			p.UnknownEvidence = []string{"which process changed the task file"}
		} else {
			p.UnknownEvidence = []string{"which process changed the project files"}
		}
	}
	return p
}

// inputErrorDoc renders rejected caller input. File contents never enter the
// document; only the path and the rejection reason do.
func inputErrorDoc(e *inputError) *PublicError {
	p := usageFailure(e.code, e.message)
	p.legacyExit = exitOperationFailed
	if e.path != "" {
		p.Paths = []string{e.path}
	}
	p.Expected, p.Observed = e.expected, e.observed
	p.Actions = []ErrorAction{{
		Description: "Fix the named input, then submit it again.",
	}}
	return p
}

// pendingErrorDoc renders a wait that ended without an answer.
func pendingErrorDoc(e *pendingError) *PublicError {
	p := &PublicError{
		Code: "request_pending", Message: e.message, Retry: retryYes,
		ExitCode: exitWaitTimeout, legacyExit: exitOperationFailed,
	}
	if e.request != "" {
		p.Observed = map[string]any{"request_id": e.request, "wait": e.waitKind}
		p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "durable request " + e.request}}
	}
	p.Actions = []ErrorAction{{
		Description: "The request keeps running. Query or watch it instead of resubmitting.",
		Command:     "looper events --json",
	}}
	return p
}

// requestAckError maps a controller acknowledgement to the failure it
// reports. The acknowledgement stays durable evidence for the request.
func requestAckError(ack *SettingsAck) *PublicError {
	message := ack.Message
	if message == "" {
		message = "The controller reported " + ack.Status + "."
	}
	var p *PublicError
	switch ack.Status {
	case AckConflict:
		p = operationFailure("request_conflict", message)
	case AckStale:
		p = incompatibleFailure("controller_session_stale", message)
	default:
		p = operationFailure("request_rejected", message)
	}
	p.Observed = map[string]any{"request_id": ack.ID, "controller_decision": ack.Status}
	p.Evidence = []ErrorEvidence{{Kind: "request", Reference: "controller acknowledgement " + ack.ID}}
	p.Actions = []ErrorAction{{Description: "Address the reported condition, then submit the request again."}}
	return p
}

// persistenceError reports a write Looper could not complete. Retrying the
// same call can succeed once the underlying store accepts writes again.
func persistenceError(message string) *PublicError {
	p := &PublicError{
		Code: "persistence_failed", Message: message, Retry: retryYes,
		ExitCode: exitOperationFailed, legacyExit: exitOperationFailed,
	}
	p.Actions = []ErrorAction{{Description: "Check disk space and permissions on the runtime directory, then retry."}}
	return p
}

// startupFailure reports a detached controller that never reached readiness.
// The daemon log stays the evidence; the child's own report names the cause
// when it could deliver one.
func startupFailure(message, log string) *PublicError {
	p := operationFailure("startup_failed", message)
	if log != "" {
		p.Evidence = []ErrorEvidence{{Kind: "log", Reference: log}}
		p.withLegacy(message+"\nLog: "+log, 1)
	}
	p.Actions = []ErrorAction{{
		Description: "Read the readiness failure and the retained log, then start again once the cause is fixed.",
		Command:     "looper doctor",
	}}
	return p
}

// publicErrorFor converts any error to its public form. Recognized failures
// keep their structured fields; anything unrecognized still carries a stable
// code and retry guidance instead of free-form text alone.
func publicErrorFor(err error) *PublicError {
	if err == nil {
		return nil
	}
	var p *PublicError
	if errors.As(err, &p) {
		return p
	}
	var input *inputError
	if errors.As(err, &input) {
		return inputErrorDoc(input)
	}
	var retained *retentionError
	if errors.As(err, &retained) {
		return retentionErrorDoc(retained)
	}
	var unsupported *unsupportedRequestError
	if errors.As(err, &unsupported) {
		return capabilityError(unsupported)
	}
	var stale *staleSessionError
	if errors.As(err, &stale) {
		return staleSessionErrorDoc(stale)
	}
	var cursor *cursorInvalidError
	if errors.As(err, &cursor) {
		return cursorErrorDoc(cursor)
	}
	var pending *pendingError
	if errors.As(err, &pending) {
		return pendingErrorDoc(pending)
	}
	// Failures that carry a bounded summary (AX-021) render it here: the
	// code names the failing class, the observed values carry the exit status
	// and outcome, and the evidence references point at Looper-owned records
	// and the one focused query that retrieves every summary.
	var carrier failureCarrier
	if errors.As(err, &carrier) {
		if p := failureErrorDoc(err, carrier.failureSummary()); p != nil {
			return p
		}
	}
	return &PublicError{
		Code: "operation_failed", Message: err.Error(), Retry: retryNo,
		ExitCode: exitOperationFailed, legacyExit: exitOperationFailed,
	}
}

// failureQueryCommand is the one focused query that retrieves every bounded
// failure summary a run journaled (AX-021).
const failureQueryCommand = "looper events --contract v2 --json --kind failure"

// failureErrorDoc renders the version 2 error document for a failure that
// carries its bounded summary. The message keeps the error's own text, so the
// legacy rendering stays byte-identical.
func failureErrorDoc(err error, s *FailureSummary) *PublicError {
	if s == nil {
		return nil
	}
	p := operationFailure(failureErrorCode(s), err.Error())
	p.Retry = failureRetry(err, s)
	p.Observed = map[string]any{"outcome": s.Outcome}
	if s.ExitStatus != nil {
		p.Observed["exit_status"] = *s.ExitStatus
	}
	if s.Signal != "" {
		p.Observed["signal"] = s.Signal
	}
	for _, ev := range clampList(s.Evidence, maxErrorEvidence-1) {
		p.Evidence = append(p.Evidence, ErrorEvidence{Kind: ev.Kind, Reference: ev.Reference})
	}
	p.Evidence = append(p.Evidence, ErrorEvidence{
		Kind: "event", Reference: "bounded failure summaries", Command: failureQueryCommand,
	})
	p.Actions = []ErrorAction{
		{Description: "Read the bounded failure summaries with their exit statuses and evidence.", Command: failureQueryCommand},
		{Description: "Read the retained evidence named above; it stays inside the Looper runtime directory."},
	}
	return p
}

// failureErrorCode names the failing class. The operation decides the stable
// code, so a caller distinguishes a launcher failure from an agent or
// verification failure without parsing the message.
func failureErrorCode(s *FailureSummary) string {
	switch s.Operation {
	case opLaunch:
		return "launch_failed"
	case opSetup, opVerify:
		return "verification_failed"
	case opCleanup:
		return "cleanup_failed"
	default:
		return "agent_failed"
	}
}

// failureRetry maps the failure's own classification onto retry guidance:
// transient failures retry as-is, account and request-shape failures repeat
// identically until the input changes, and everything else retries after a
// change.
func failureRetry(err error, s *FailureSummary) string {
	var agent *AgentError
	if errors.As(err, &agent) {
		switch agent.Kind {
		case "transient":
			return retryYes
		case "configuration", "invalid_request":
			return retryNo
		}
	}
	if s.Outcome == outcomeAbsent {
		return retryYes
	}
	return retryAfterChange
}

// errorDocument is the version 2 error document. It validates against
// docs/contracts/schemas/v2/error.schema.json.
type errorDocument struct {
	ContractVersion int               `json:"contract_version"`
	SchemaVersion   int               `json:"schema_version"`
	Kind            string            `json:"kind"`
	Command         string            `json:"command,omitempty"`
	Error           errorDocumentBody `json:"error"`
}

type errorDocumentBody struct {
	Code            string            `json:"code"`
	Message         string            `json:"message"`
	Retry           string            `json:"retry"`
	Paths           []string          `json:"paths,omitempty"`
	Expected        map[string]any    `json:"expected,omitempty"`
	Observed        map[string]any    `json:"observed,omitempty"`
	Evidence        []errorEvidenceVD `json:"evidence,omitempty"`
	NextActions     []errorActionVD   `json:"next_actions,omitempty"`
	UnknownEvidence []string          `json:"unknown_evidence,omitempty"`
}

type errorEvidenceVD struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Command   string `json:"command,omitempty"`
}

type errorActionVD struct {
	Description string `json:"description"`
	Command     string `json:"command,omitempty"`
	Operation   string `json:"operation,omitempty"`
}

// document builds the version 2 error document for one failure. Collections
// and strings are clamped to the schema bounds so the document is always
// valid JSON regardless of what failed.
func (e *PublicError) document(command string) *errorDocument {
	body := errorDocumentBody{
		Code:    e.Code,
		Message: clampString(e.Message, maxErrorMessage),
		Retry:   e.Retry,
	}
	for _, p := range clampList(e.Paths, maxErrorPaths) {
		body.Paths = append(body.Paths, clampString(p, maxErrorString))
	}
	body.Expected = clampValues(e.Expected)
	body.Observed = clampValues(e.Observed)
	for _, ev := range clampList(e.Evidence, maxErrorEvidence) {
		body.Evidence = append(body.Evidence, errorEvidenceVD{
			Kind: ev.Kind, Reference: clampString(ev.Reference, maxErrorString), Command: ev.Command,
		})
	}
	for _, a := range clampList(e.Actions, maxErrorActions) {
		body.NextActions = append(body.NextActions, errorActionVD{
			Description: clampString(a.Description, maxErrorString), Command: a.Command, Operation: a.Operation,
		})
	}
	for _, u := range clampList(e.UnknownEvidence, maxErrorUnknown) {
		body.UnknownEvidence = append(body.UnknownEvidence, clampString(u, maxErrorString))
	}
	return &errorDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   errorSchemaVersion,
		Kind:            errorKind,
		Command:         clampString(command, maxErrorString),
		Error:           body,
	}
}

// plain renders the same fields as text for callers without a JSON parser.
func (e *PublicError) plain() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Error: %s\n", clampString(e.Message, maxErrorMessage))
	fmt.Fprintf(&b, "Code: %s (retry: %s)\n", e.Code, e.Retry)
	for _, p := range clampList(e.Paths, maxErrorPaths) {
		fmt.Fprintf(&b, "Path: %s\n", p)
	}
	for _, k := range sortedKeys(e.Expected) {
		observed := "unknown"
		if v, ok := e.Observed[k]; ok {
			observed = fmt.Sprint(v)
		}
		fmt.Fprintf(&b, "%s: expected %v, observed %s\n", k, e.Expected[k], observed)
	}
	for _, u := range clampList(e.UnknownEvidence, maxErrorUnknown) {
		fmt.Fprintf(&b, "Unknown: %s\n", u)
	}
	for _, ev := range clampList(e.Evidence, maxErrorEvidence) {
		if ev.Command != "" {
			fmt.Fprintf(&b, "Evidence: %s (%s; read it with: %s)\n", ev.Kind, ev.Reference, ev.Command)
		} else {
			fmt.Fprintf(&b, "Evidence: %s (%s)\n", ev.Kind, ev.Reference)
		}
	}
	for _, a := range clampList(e.Actions, maxErrorActions) {
		switch {
		case a.Command != "":
			fmt.Fprintf(&b, "Next: %s (%s)\n", a.Description, a.Command)
		case a.Operation != "":
			fmt.Fprintf(&b, "Next: %s (%s)\n", a.Description, a.Operation)
		default:
			fmt.Fprintf(&b, "Next: %s\n", a.Description)
		}
	}
	return b.String()
}

// emitError renders one CLI failure for the negotiated contract and returns
// the process exit code. Under contract version 2 the structured document
// replaces free-form stderr: a JSON caller receives exactly one document on
// standard output with nothing human beside it, and a plain caller receives
// the same fields as text. The legacy contract keeps today's rendering.
func emitError(c Config, err error, out, stderr io.Writer) int {
	p := publicErrorFor(err)
	if c.Contract == ContractVersionV2 {
		if c.JSON {
			if encErr := json.NewEncoder(out).Encode(p.document(commandLabel(c))); encErr != nil {
				fmt.Fprintln(stderr, "Error:", p.Message)
				return p.ExitCode
			}
			return p.ExitCode
		}
		fmt.Fprint(stderr, p.plain())
		return p.ExitCode
	}
	if p.legacy != nil {
		if p.legacy.stdout {
			fmt.Fprintln(out, p.legacy.text)
		} else {
			fmt.Fprintln(stderr, p.legacy.text)
		}
		return p.legacy.code
	}
	fmt.Fprintln(stderr, "Error:", p.Message)
	if p.legacyExit != 0 {
		return p.legacyExit
	}
	return exitOperationFailed
}

// commandLabel names the failing command in the error document.
func commandLabel(c Config) string {
	if c.Command == "" {
		return ""
	}
	label := "looper " + c.Command
	if c.Command == "inspect" && c.InspectTask != "" {
		label += " " + c.InspectTask
	}
	if c.Command == "requests" && c.RequestID != "" {
		label += " " + c.RequestID
	}
	if c.Command == "wait" {
		// The wait predicates are flags, so the label names the one target
		// the caller asked about.
		switch {
		case c.WaitCursorSet:
			label += " --cursor " + strconv.FormatInt(c.WaitCursor, 10)
		case c.EventTask != "":
			label += " --task " + c.EventTask
		case c.WaitRequestID != "":
			label += " --request " + c.WaitRequestID
		case c.WaitRunOutcome:
			label += " --run"
		}
	}
	if c.Contract == ContractVersionV2 {
		label += " --contract v2"
	}
	return label
}

// emitParseError renders a failure from command-line parsing. Parsing can
// fail before the contract selection resolves, so the negotiation is
// recovered from the raw arguments and the environment. Parse errors were
// always usage failures in the legacy contract, so their legacy exit code is
// fixed here rather than per constructor.
func emitParseError(args []string, err error, out, stderr io.Writer) int {
	c := Config{Contract: DefaultContractVersion}
	flagSeen := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			c.JSON = true
		case "--contract":
			if i+1 < len(args) {
				if v, e := parseContractVersion(args[i+1]); e == nil {
					c.Contract, flagSeen = v, true
				}
			}
		}
	}
	if !flagSeen {
		if v, ok := os.LookupEnv("LOOPER_CONTRACT"); ok {
			if parsed, e := parseContractVersion(v); e == nil {
				c.Contract = parsed
			}
		}
	}
	c.Command = commandFromArgs(args)
	p := publicErrorFor(err)
	p.legacyExit = exitUsage
	return emitError(c, p, out, stderr)
}

// commandFromArgs names the command for a parse failure when one was
// recognizable.
func commandFromArgs(args []string) string {
	for _, a := range args {
		switch a {
		case "run", "start", "tui", "attach", "status", "stop", "doctor", "tail", "version", "config", "add-task", "add-plan", "cancel", "amend", "events", "inspect", "recovery", "preflight", "runs", "requests", "wait", "help":
			return a
		case "--version":
			return "version"
		}
	}
	return ""
}

func clampString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut inside the bound so the ellipsis keeps the result within it in both
	// bytes and code points, on a rune boundary so it stays valid UTF-8.
	cut := max - len("…")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// clampJSONStringMin is the smallest bound clampJSONString accepts. One rune
// at its worst escape costs six bytes, so a smaller bound cannot hold even a
// single character; the clamp then keeps that one rune and no marker.
const clampJSONStringMin = 8

// The six-byte escapes the JSON encoder writes for content it will not emit
// raw: the JavaScript line separators and each invalid UTF-8 byte.
const (
	jsonEscapedLineSep = len(`\u2028`)
	jsonEscapedBadRune = len(`\ufffd`)
)

// jsonRuneWidthOf reports how many bytes the JSON encoder emits for one
// decoded rune of size bytes, following encoding/json with HTML escaping on:
// the HTML-significant characters, both quote characters, and every control
// character escape, the two JavaScript line separators and each invalid UTF-8
// byte included, so one recorded byte can become six emitted bytes.
func jsonRuneWidthOf(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return jsonEscapedBadRune
	case r == '\u2028', r == '\u2029':
		return jsonEscapedLineSep
	case r < utf8.RuneSelf:
		switch byte(r) {
		case '"', '\\', '\n', '\r', '\t', '\b', '\f':
			return 2
		case '<', '>', '&':
			return 6
		}
		if r < 0x20 {
			return 6
		}
		return 1
	default:
		return size
	}
}

// jsonEscapedLen reports how many bytes the JSON encoder emits for the
// content of one string, quotes excluded. A byte budget that must hold for
// the emitted document bounds this length, not len(s).
func jsonEscapedLen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		n += jsonRuneWidthOf(r, size)
		i += size
	}
	return n
}

// jsonPrefixLen returns the byte length of the longest rune-aligned prefix of
// s whose escaped length stays within the budget.
func jsonPrefixLen(s string, budget int) int {
	end, used := 0, 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		w := jsonRuneWidthOf(r, size)
		if used+w > budget {
			break
		}
		used += w
		i += size
		end = i
	}
	return end
}

// jsonSuffixStart returns the index where the longest rune-aligned suffix of
// s begins whose escaped length stays within the budget.
func jsonSuffixStart(s string, budget int) int {
	start, used := len(s), 0
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(s[:start])
		w := jsonRuneWidthOf(r, size)
		if used+w > budget {
			break
		}
		used += w
		start -= size
	}
	return start
}

// clampJSONString bounds s so the JSON encoder emits at most max bytes for
// its content. Like clampString it cuts on a rune boundary, marks the cut
// with an ellipsis, and never returns an empty string while s has content, so
// identifiers keep the schemas' minLength 1. Because the escaped length never
// falls below the raw length, the bound holds for the raw bytes too.
func clampJSONString(s string, max int) string {
	if max < clampJSONStringMin {
		max = clampJSONStringMin
	}
	if jsonEscapedLen(s) <= max {
		return s
	}
	// The ellipsis never escapes, so the prefix gets the rest of the bound.
	if n := jsonPrefixLen(s, max-len("…")); n > 0 {
		return s[:n] + "…"
	}
	// No rune fits beside the marker. One rune at its worst escape is six
	// bytes, which the minimum bound above keeps room for.
	_, size := utf8.DecodeRuneInString(s)
	return s[:size]
}

func clampList[T any](values []T, max int) []T {
	if len(values) <= max {
		return values
	}
	return values[:max]
}

func clampValues(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]any, len(values))
	for k, v := range values {
		if s, ok := v.(string); ok {
			v = clampString(s, maxErrorString)
		}
		out[clampString(k, 256)] = v
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
