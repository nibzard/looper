package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The bounded request query contract,
// docs/contracts/schemas/v2/request-record.schema.json (AX-010). One document
// answers either form: `looper requests <id>` returns that request's record,
// and `looper requests` returns one bounded page of the run's durable
// receipts. The receipts are public evidence: a caller diagnoses a pending
// import or a settings request without reading the acknowledgement directory
// or the state database, including on a run whose controller stopped.
const (
	requestRecordSchemaVersion = 2
	requestRecordKind          = "looper.request.record"
	// requestsBytesBudget bounds one document, either form.
	requestsBytesBudget = 8192
	// requestsDefaultLimit is the default page size of the list form.
	requestsDefaultLimit = 20
	// maxRequestsPageLimit is the largest page --limit may request. The list
	// has no --full export: it is bounded by design and reports what it
	// omits, so every record is reached through its pages.
	maxRequestsPageLimit = 100
)

// requestFit names the bounds one rendering pass applies. A document over
// the byte budget renders again with the next fit: strings shrink first,
// then collections drop entries. Every string bound counts the bytes the
// JSON encoder emits, so an escape-heavy identifier shrinks with the ladder
// instead of overflowing the fit that stated it. Both ladders end in a
// terminal fit whose strings are one rune at worst and whose collections
// are empty, so a valid document always fits whatever the saved receipts
// hold.
type requestFit struct {
	stringMax  int
	recordsMax int
	listMax    int
}

// requestTerminalFits are the ladders' last resorts: the single record keeps
// the record itself with no collections, and the list page keeps no records
// at all — counts and continuation alone.
var (
	requestsTerminalSingleFit = requestFit{stringMax: clampJSONStringMin, recordsMax: 1, listMax: 0}
	requestsTerminalListFit   = requestFit{stringMax: clampJSONStringMin, recordsMax: 0, listMax: 0}
)

// requestsSingleFitsFor builds the ladder for one record query.
func requestsSingleFitsFor() []requestFit {
	return []requestFit{
		{256, 1, 16},
		{160, 1, 8},
		{96, 1, 4},
		{64, 1, 2},
		{48, 1, 1},
		requestsTerminalSingleFit,
	}
}

// requestsListFitsFor builds the ladder for one list page. The first fits
// honor the requested page; the later ones trade entry count and string size
// for the byte budget.
func requestsListFitsFor(limit int) []requestFit {
	return []requestFit{
		{256, limit, 16},
		{160, limit, 8},
		{96, min(limit, 12), 4},
		{64, min(limit, 6), 2},
		{48, min(limit, 3), 1},
		requestsTerminalListFit,
	}
}

// requestRecordDocument is the one JSON document `looper requests` serves
// under contract version 2. Exactly one form carries the content: `request`
// for a single record, `requests` for one bounded page.
type requestRecordDocument struct {
	ContractVersion int                 `json:"contract_version"`
	SchemaVersion   int                 `json:"schema_version"`
	Kind            string              `json:"kind"`
	Request         *requestRecordView  `json:"request,omitempty"`
	Requests        []requestRecordView `json:"requests,omitempty"`
	Continuation    requestContinuation `json:"continuation"`
	List            *requestListSummary `json:"list,omitempty"`
}

// requestRecordView is one request's public record: the lifecycle state from
// docs/contracts/request-states.json, its age, the prerequisite a waiting
// request names, the tasks and plan coverage it affects, and safe error
// evidence for a terminal failure.
type requestRecordView struct {
	ID                string               `json:"id"`
	Type              string               `json:"type"`
	State             string               `json:"state"`
	AgeSeconds        int                  `json:"age_seconds"`
	ControllerSession string               `json:"controller_session,omitempty"`
	IdempotencyKey    string               `json:"idempotency_key,omitempty"`
	SubmittedAt       string               `json:"submitted_at,omitempty"`
	DecidedAt         string               `json:"decided_at,omitempty"`
	WaitingOn         *requestWaitingOn    `json:"waiting_on,omitempty"`
	AffectedTasks     []string             `json:"affected_tasks,omitempty"`
	PlanCoverage      *requestPlanCoverage `json:"plan_coverage,omitempty"`
	Result            *requestResultView   `json:"result,omitempty"`
	Error             *errorDocumentBody   `json:"error,omitempty"`
}

type requestWaitingOn struct {
	Reason     string `json:"reason"`
	AgeSeconds *int   `json:"age_seconds,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// requestPlanCoverage is the additive scope one plan import reported: the
// tasks it added, the requirements existing work already covered, the
// conflicts and adjustments its analysis named, and the dependency gates it
// placed (AX-011 keeps gates beside the rest of the applied evidence).
type requestPlanCoverage struct {
	AddedTasks  []string            `json:"added_tasks,omitempty"`
	Covered     []requestCoveredRef `json:"covered,omitempty"`
	Conflicts   []string            `json:"conflicts,omitempty"`
	Adjustments []string            `json:"adjustments,omitempty"`
	Gates       []requestGateRef    `json:"gates,omitempty"`
}

// requestGateRef is one dependency gate an import placed on an existing task.
type requestGateRef struct {
	TaskID    string   `json:"task_id"`
	DependsOn []string `json:"depends_on,omitempty"`
}

type requestCoveredRef struct {
	Requirement string `json:"requirement"`
	TaskID      string `json:"task_id,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
}

type requestResultView struct {
	Summary               string   `json:"summary,omitempty"`
	AddedTaskIDs          []string `json:"added_task_ids,omitempty"`
	AppliedFields         []string `json:"applied_fields,omitempty"`
	RestartRequiredFields []string `json:"restart_required_fields,omitempty"`
}

type requestContinuation struct {
	Command string `json:"command"`
	List    string `json:"list,omitempty"`
}

type requestListSummary struct {
	Returned int            `json:"returned"`
	Limit    int            `json:"limit,omitempty"`
	HasMore  bool           `json:"has_more"`
	Omitted  map[string]int `json:"omitted,omitempty"`
}

// requestQueryRecord is one request's collected evidence before the fit
// bounds clamp it into a public view. It reads durable evidence only: the
// saved receipt, the import record behind a plan import, or the envelope of
// a request no receipt has answered yet.
type requestQueryRecord struct {
	id      string
	typ     string
	state   string
	session string
	key     string

	submittedAt string
	decidedAt   string

	waitReason string
	waitDetail string
	waitSince  string

	affectedTasks []string
	appliedFields []string
	restartFields []string
	summary       string
	errText       string

	coverage *requestPlanCoverage

	// envelopeTime is the modification time of a submitted request's
	// envelope, the age evidence a receipt-less request owns.
	envelopeTime time.Time
	// now is the read time the ages report against.
	now time.Time
}

// requestQueryEvidence is every durable request record one run holds.
type requestQueryEvidence struct {
	records []*requestQueryRecord
	byID    map[string]*requestQueryRecord
}

// ageBase is the timestamp the record's age counts from: the submitted time
// when one is recorded, else the envelope's modification time.
func (r *requestQueryRecord) ageBase() time.Time {
	if t, err := time.Parse(time.RFC3339Nano, r.submittedAt); err == nil {
		return t
	}
	return r.envelopeTime
}

func (r *requestQueryRecord) ageSeconds() int {
	base := r.ageBase()
	if base.IsZero() || base.After(r.now) {
		return 0
	}
	return int(r.now.Sub(base).Seconds())
}

// collectRequestEvidence reads one run's request records from the saved
// state and the public envelope directories. Receipts come first in the
// order the state holds them, which is the order their decisions were
// committed; envelopes no receipt answers follow, oldest first, so a page's
// ordering is deterministic for the records the run holds.
func collectRequestEvidence(dir string, s *State, now time.Time) *requestQueryEvidence {
	ev := &requestQueryEvidence{byID: map[string]*requestQueryRecord{}}
	if s == nil {
		return ev
	}
	imports := map[string]*ImportRecord{}
	for _, imp := range s.Imports {
		if imp != nil && imp.RequestID != "" {
			imports[imp.RequestID] = imp
		}
	}
	for _, rec := range s.Requests {
		if rec == nil || rec.ID == "" {
			continue
		}
		q := requestQueryRecordFromReceipt(rec, imports[rec.ID], now)
		ev.records = append(ev.records, q)
		ev.byID[q.id] = q
	}
	type pendingEnvelope struct {
		id   string
		path string
		mod  time.Time
	}
	var envelopes []pendingEnvelope
	for _, d := range []string{requestDir(dir), processingDir(dir)} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".json")
			if _, answered := ev.byID[id]; answered {
				continue
			}
			path := filepath.Join(d, entry.Name())
			mod := time.Time{}
			if info, err := entry.Info(); err == nil {
				mod = info.ModTime()
			}
			envelopes = append(envelopes, pendingEnvelope{id: id, path: path, mod: mod})
		}
	}
	sort.Slice(envelopes, func(i, j int) bool {
		if envelopes[i].mod.Equal(envelopes[j].mod) {
			return envelopes[i].id < envelopes[j].id
		}
		return envelopes[i].mod.Before(envelopes[j].mod)
	})
	for _, env := range envelopes {
		q := requestQueryRecordFromEnvelope(env.id, env.path, env.mod, now)
		ev.records = append(ev.records, q)
		ev.byID[q.id] = q
	}
	// The stop transport's envelope is the scoped stop file, not a request
	// envelope: a stop request no receipt has decided stays queryable under
	// its derived identity, so the receipt `looper stop` returns names a
	// record the query can find (AX-011).
	if req, info, err := readStopRequest(dir); err == nil && req != nil {
		if id := stopRequestID(req); id != "" {
			if _, answered := ev.byID[id]; !answered {
				mod := time.Time{}
				if info != nil {
					mod = info.ModTime()
				}
				q := requestQueryRecordFromEnvelope(id, stopRequestPath(dir), mod, now)
				q.typ = PublicRequestStop
				ev.records = append(ev.records, q)
				ev.byID[id] = q
			}
		}
	}
	return ev
}

// requestQueryRecordFromReceipt maps one durable receipt to its query form.
// A state the registry does not define renders as a failed record whose
// error names the unrecognized state, never as a state the contract forbids.
func requestQueryRecordFromReceipt(rec *RequestRecord, imp *ImportRecord, now time.Time) *requestQueryRecord {
	q := &requestQueryRecord{
		id: rec.ID, typ: rec.Type, state: rec.State, session: rec.Session,
		key:         rec.IdempotencyKey,
		submittedAt: rec.SubmittedAt, decidedAt: rec.DecidedAt,
		waitReason: rec.WaitReason, waitDetail: rec.WaitDetail, waitSince: rec.WaitSince,
		affectedTasks: rec.AffectedTasks, appliedFields: rec.Fields,
		summary: rec.Summary, errText: rec.Error, now: now,
	}
	if _, listed := requestStateRegistry[rec.State]; !listed {
		q.errText = fmt.Sprintf("the saved receipt names state %q, which the request state registry does not define; %s", rec.State, q.errText)
		q.state = RequestFailed
	}
	if rec.Ack != nil {
		keys := make([]string, 0, len(rec.Ack.Outcomes))
		for key := range rec.Ack.Outcomes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if rec.Ack.Outcomes[key] == "saved; applies on the next controller run" {
				q.restartFields = append(q.restartFields, key)
			}
		}
	}
	if imp != nil {
		q.coverage = planCoverageOf(imp)
		if len(imp.AddedTasks) > 0 && len(q.affectedTasks) == 0 {
			q.affectedTasks = imp.AddedTasks
		}
	}
	if q.typ == "" {
		q.typ = "unknown"
	}
	return q
}

// requestQueryRecordFromEnvelope maps one unanswered envelope to its query
// form: a submitted request, with the evidence its own envelope carries.
// An envelope that cannot be parsed stays queryable with its identifier and
// an explicit unknown type rather than being hidden.
func requestQueryRecordFromEnvelope(id, path string, mod time.Time, now time.Time) *requestQueryRecord {
	q := &requestQueryRecord{id: id, typ: "unknown", state: RequestSubmitted, envelopeTime: mod, now: now}
	if req, err := loadSettingsRequest(path); err == nil && req != nil {
		q.typ = publicRequestType(req.Type)
		q.submittedAt = req.CreatedAt
	}
	return q
}

// planCoverageOf reads one import record's coverage evidence, gates included.
func planCoverageOf(imp *ImportRecord) *requestPlanCoverage {
	if imp == nil {
		return nil
	}
	coverage := &requestPlanCoverage{
		AddedTasks: imp.AddedTasks, Conflicts: imp.Conflicts, Adjustments: imp.Adjustments,
		Covered: coveredRefsOf(imp.Covered),
	}
	for _, gate := range imp.Gates {
		coverage.Gates = append(coverage.Gates, requestGateRef{TaskID: gate.TaskID, DependsOn: gate.DependsOn})
	}
	if coverage.AddedTasks == nil && coverage.Covered == nil && coverage.Conflicts == nil &&
		coverage.Adjustments == nil && coverage.Gates == nil {
		return nil
	}
	return coverage
}

// coveredRefsOf converts import coverage references to their record view.
func coveredRefsOf(refs []CoveredRef) []requestCoveredRef {
	var out []requestCoveredRef
	for _, ref := range refs {
		out = append(out, requestCoveredRef{
			Requirement: ref.Requirement, TaskID: ref.TaskID, Evidence: ref.Evidence,
		})
	}
	return out
}

// runRequestsQuery serves the bounded request queries (AX-010): one record
// by identifier or one bounded page of the run's durable receipts. The read
// is one-shot and creates nothing beside a cleanly stopped run's database.
func runRequestsQuery(c Config, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The request record document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{
			Description: "Read the request records with the version 2 contract.",
			Command:     "looper requests --contract v2 --json",
		}}
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; request records live with a saved run"
		return emitError(c, p, out, stderr)
	}
	store, err := openObservedStore(dir)
	if err != nil {
		return emitError(c, stateUnreadableError(path, err), out, stderr)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		return emitError(c, stateUnreadableError(path, err), out, stderr)
	}
	if snap.State == nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; request records live with a saved run"
		return emitError(c, p, out, stderr)
	}
	ev := collectRequestEvidence(dir, snap.State, wallClock())
	if c.RequestID != "" {
		rec := ev.byID[c.RequestID]
		if rec == nil {
			return emitError(c, requestNotFoundError(c.RequestID, len(ev.records)), out, stderr)
		}
		doc := buildSingleRequestDocument(rec)
		if c.JSON {
			return encodeRequestDocument(doc, out)
		}
		renderRequestRecord(doc, out)
		return exitOK
	}
	// A continuation parsed before the command word resolved keeps no
	// identifier (the parser read it as a numeric position), so the page
	// starts from the beginning rather than failing on an empty cursor.
	after := c.AfterID
	if after != "" {
		if _, known := ev.byID[after]; !known {
			p := requestAfterUnknownError(after, len(ev.records))
			return emitError(c, p, out, stderr)
		}
	}
	limit := c.EventLimit
	if limit == 0 {
		limit = requestsDefaultLimit
	}
	doc := buildRequestListDocument(ev, limit, after)
	if c.JSON {
		return encodeRequestDocument(doc, out)
	}
	renderRequestList(doc, out)
	return exitOK
}

// requestNotFoundError reports an identifier no retained receipt or pending
// envelope names. The evidence names what the run does hold, so a missing
// and a pruned identifier are both precise.
func requestNotFoundError(id string, retained int) *PublicError {
	p := operationFailure("request_not_found", fmt.Sprintf("unknown request %q; no retained receipt or pending envelope names it in this run", id))
	p.Observed = map[string]any{"request_id": clampString(id, maxErrorString), "retained_records": retained}
	p.Evidence = []ErrorEvidence{{
		Kind: "request", Reference: "the run's retained request records",
		Command: "looper requests --contract v2 --json",
	}}
	p.Actions = []ErrorAction{{
		Description: "List the request records this run retains.",
		Command:     "looper requests --contract v2 --json",
	}}
	return p
}

// requestAfterUnknownError reports a continuation identifier the run no
// longer holds. Terminal receipts prune from the oldest, so a restart of the
// listing is always safe: records are read-only evidence.
func requestAfterUnknownError(id string, retained int) *PublicError {
	p := operationFailure("request_after_unknown", fmt.Sprintf("the continuation identifier %q names no retained request record", id))
	p.Observed = map[string]any{"after": clampString(id, maxErrorString), "retained_records": retained}
	p.Actions = []ErrorAction{{
		Description: "Read the first page again and continue from its newest record.",
		Command:     "looper requests --contract v2 --json",
	}}
	return p
}

// buildSingleRequestDocument renders one record within the byte budget.
func buildSingleRequestDocument(rec *requestQueryRecord) *requestRecordDocument {
	var doc *requestRecordDocument
	for _, fit := range requestsSingleFitsFor() {
		doc = renderSingleRequestDocument(rec, fit)
		if requestsEncoded(doc) <= requestsBytesBudget {
			return doc
		}
	}
	return doc
}

// buildRequestListDocument renders one page within the byte budget. The
// cursor is exclusive: the page starts after the named record.
func buildRequestListDocument(ev *requestQueryEvidence, limit int, after string) *requestRecordDocument {
	start := 0
	if after != "" {
		for i, rec := range ev.records {
			if rec.id == after {
				start = i + 1
				break
			}
		}
	}
	var doc *requestRecordDocument
	for _, fit := range requestsListFitsFor(limit) {
		doc = renderRequestListDocument(ev, limit, start, fit)
		if requestsEncoded(doc) <= requestsBytesBudget {
			return doc
		}
	}
	return doc
}

// requestsEncoded reports the size of the document as the CLI emits it.
func requestsEncoded(doc *requestRecordDocument) int {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(doc)
	return buf.Len()
}

func encodeRequestDocument(doc *requestRecordDocument, out io.Writer) int {
	if err := json.NewEncoder(out).Encode(doc); err != nil {
		return exitOperationFailed
	}
	return exitOK
}

// renderSingleRequestDocument renders one record under the fit bounds.
func renderSingleRequestDocument(rec *requestQueryRecord, fit requestFit) *requestRecordDocument {
	doc := &requestRecordDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   requestRecordSchemaVersion,
		Kind:            requestRecordKind,
	}
	view := requestRecordViewOf(rec, fit)
	doc.Request = &view
	if statusRouteIDOk(rec.id) {
		doc.Continuation = requestContinuation{
			Command: "looper requests " + rec.id + " --contract v2",
			List:    "looper requests --contract v2 --after " + rec.id,
		}
	} else {
		// An identifier too escape-heavy to embed in a route keeps a valid
		// command: the list itself.
		doc.Continuation = requestContinuation{Command: "looper requests --contract v2 --json"}
	}
	return doc
}

// renderRequestListDocument renders one page under the fit bounds.
func renderRequestListDocument(ev *requestQueryEvidence, limit, start int, fit requestFit) *requestRecordDocument {
	doc := &requestRecordDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   requestRecordSchemaVersion,
		Kind:            requestRecordKind,
		Continuation:    requestContinuation{Command: "looper requests --contract v2 --json"},
	}
	page := ev.records
	if start > 0 {
		if start >= len(page) {
			page = nil
		} else {
			page = page[start:]
		}
	}
	returned := min(len(page), fit.recordsMax)
	doc.Requests = []requestRecordView{}
	for _, rec := range page[:returned] {
		doc.Requests = append(doc.Requests, requestRecordViewOf(rec, fit))
	}
	hasMore := returned < len(page)
	doc.List = &requestListSummary{
		Returned: returned, Limit: limit, HasMore: hasMore,
		Omitted: map[string]int{"records": max(0, len(ev.records)-returned)},
	}
	if hasMore && returned > 0 {
		last := page[returned-1].id
		if statusRouteIDOk(last) {
			doc.Continuation.List = "looper requests --contract v2 --after " + last + " --limit " + strconv.Itoa(limit)
		}
	}
	return doc
}

// requestRecordViewOf renders one record under the fit bounds. Every string
// is clamped in the bytes the JSON encoder emits, so the document stays
// within its budget whatever the saved receipts hold.
func requestRecordViewOf(r *requestQueryRecord, fit requestFit) requestRecordView {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	view := requestRecordView{
		ID: clamp(r.id), Type: clamp(r.typ), State: r.state, AgeSeconds: r.ageSeconds(),
		ControllerSession: clamp(r.session),
		IdempotencyKey:    clamp(r.key),
		SubmittedAt:       clamp(r.submittedAt), DecidedAt: clamp(r.decidedAt),
	}
	if r.state == RequestWaiting || r.waitReason != "" {
		reason := r.waitReason
		if reason == "" {
			reason = "unknown"
		}
		waiting := requestWaitingOn{Reason: clamp(reason), Detail: clamp(r.waitDetail)}
		if since, err := time.Parse(time.RFC3339Nano, r.waitSince); err == nil && !since.After(r.now) {
			age := int(r.now.Sub(since).Seconds())
			waiting.AgeSeconds = &age
		}
		view.WaitingOn = &waiting
	}
	view.AffectedTasks = clampRequestStrings(r.affectedTasks, fit)
	if r.coverage != nil {
		coverage := &requestPlanCoverage{
			AddedTasks:  clampRequestStrings(r.coverage.AddedTasks, fit),
			Conflicts:   clampRequestStrings(r.coverage.Conflicts, fit),
			Adjustments: clampRequestStrings(r.coverage.Adjustments, fit),
		}
		for i, ref := range r.coverage.Covered {
			if i >= fit.listMax {
				break
			}
			coverage.Covered = append(coverage.Covered, requestCoveredRef{
				Requirement: clamp(ref.Requirement), TaskID: clamp(ref.TaskID), Evidence: clamp(ref.Evidence),
			})
		}
		for i, gate := range r.coverage.Gates {
			if i >= fit.listMax {
				break
			}
			coverage.Gates = append(coverage.Gates, requestGateRef{
				TaskID: clamp(gate.TaskID), DependsOn: clampRequestStrings(gate.DependsOn, fit),
			})
		}
		if len(coverage.AddedTasks) > 0 || len(coverage.Covered) > 0 || len(coverage.Conflicts) > 0 ||
			len(coverage.Adjustments) > 0 || len(coverage.Gates) > 0 {
			view.PlanCoverage = coverage
		}
	}
	view.Result = requestResultViewOf(r, fit)
	if r.state == RequestRejected || r.state == RequestFailed {
		view.Error = requestErrorView(r, fit)
	}
	return view
}

// requestResultViewOf renders the outcome an applied request recorded. A
// request with no outcome evidence carries no result object rather than an
// empty one.
func requestResultViewOf(r *requestQueryRecord, fit requestFit) *requestResultView {
	result := &requestResultView{}
	if r.summary != "" {
		clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
		result.Summary = clamp(r.summary)
	}
	if (r.typ == PublicRequestAddTask || r.typ == PublicRequestAddPlan) && r.state == RequestApplied {
		result.AddedTaskIDs = clampRequestStrings(r.affectedTasks, fit)
	}
	result.AppliedFields = clampRequestStrings(r.appliedFields, fit)
	result.RestartRequiredFields = clampRequestStrings(r.restartFields, fit)
	if result.Summary == "" && len(result.AddedTaskIDs) == 0 && len(result.AppliedFields) == 0 && len(result.RestartRequiredFields) == 0 {
		return nil
	}
	return result
}

// requestErrorView renders one terminal failure's safe error evidence: a
// stable code, the recorded reason, and the read-only route back to this
// record. The recorded text is clamped, never dropped into invalid JSON.
func requestErrorView(r *requestQueryRecord, fit requestFit) *errorDocumentBody {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	code := "request_rejected"
	if r.state == RequestFailed {
		code = "request_failed"
	}
	body := &errorDocumentBody{
		Code: code, Message: clamp(r.errText), Retry: retryAfterChange,
		Evidence: []errorEvidenceVD{{
			Kind: "request", Reference: clamp("durable receipt " + r.id),
		}},
	}
	if statusRouteIDOk(r.id) {
		route := "looper requests " + r.id + " --contract v2"
		body.Evidence[0].Command = route
		body.NextActions = []errorActionVD{
			{Description: "Read this record again after addressing the reported condition.", Command: route},
			{Description: "Submit a new request once the condition changed; this one is decided."},
		}
	} else {
		body.NextActions = []errorActionVD{
			{Description: "List the request records this run retains.", Command: "looper requests --contract v2 --json"},
			{Description: "Submit a new request once the condition changed; this one is decided."},
		}
	}
	return body
}

// clampRequestStrings clamps one bounded identifier collection to the fit's
// entry bound and string bound. The schema caps these collections; the fit
// may cap them further for the byte budget.
func clampRequestStrings(values []string, fit requestFit) []string {
	var out []string
	for i, v := range values {
		if i >= fit.listMax {
			break
		}
		out = append(out, clampJSONString(v, fit.stringMax))
	}
	return out
}

// renderRequestRecord renders one record as text from the same document the
// JSON form carries.
func renderRequestRecord(doc *requestRecordDocument, out io.Writer) {
	rec := doc.Request
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Request %s  [%s]  type %s  age %s", rec.ID, rec.State, rec.Type, statusDuration(rec.AgeSeconds))
	if rec.ControllerSession != "" {
		line("  decided by session %s", rec.ControllerSession)
	}
	if rec.IdempotencyKey != "" {
		line("  idempotency key: %s", rec.IdempotencyKey)
	}
	if rec.SubmittedAt != "" {
		line("  submitted %s", rec.SubmittedAt)
	}
	if rec.DecidedAt != "" {
		line("  decided %s", rec.DecidedAt)
	}
	if rec.WaitingOn != nil {
		if rec.WaitingOn.AgeSeconds != nil {
			line("  waiting on %s for %s: %s", rec.WaitingOn.Reason, statusDuration(*rec.WaitingOn.AgeSeconds), rec.WaitingOn.Detail)
		} else {
			line("  waiting on %s: %s", rec.WaitingOn.Reason, rec.WaitingOn.Detail)
		}
	}
	if len(rec.AffectedTasks) > 0 {
		line("  affected tasks: %s", strings.Join(rec.AffectedTasks, ", "))
	}
	if rec.PlanCoverage != nil {
		if len(rec.PlanCoverage.AddedTasks) > 0 {
			line("  plan added: %s", strings.Join(rec.PlanCoverage.AddedTasks, ", "))
		}
		for _, ref := range rec.PlanCoverage.Covered {
			line("  covered: %s -> %s %s", ref.Requirement, ref.TaskID, ref.Evidence)
		}
		for _, conflict := range rec.PlanCoverage.Conflicts {
			line("  conflict: %s", conflict)
		}
		for _, adjustment := range rec.PlanCoverage.Adjustments {
			line("  adjustment: %s", adjustment)
		}
		for _, gate := range rec.PlanCoverage.Gates {
			line("  gate: %s depends on %s", gate.TaskID, strings.Join(gate.DependsOn, ", "))
		}
	}
	if rec.Result != nil {
		if rec.Result.Summary != "" {
			line("  result: %s", rec.Result.Summary)
		}
		if len(rec.Result.AddedTaskIDs) > 0 {
			line("  added tasks: %s", strings.Join(rec.Result.AddedTaskIDs, ", "))
		}
		if len(rec.Result.AppliedFields) > 0 {
			line("  applied fields: %s", strings.Join(rec.Result.AppliedFields, ", "))
		}
		if len(rec.Result.RestartRequiredFields) > 0 {
			line("  fields that apply after a restart: %s", strings.Join(rec.Result.RestartRequiredFields, ", "))
		}
	}
	if rec.Error != nil {
		line("  error: %s", rec.Error.Message)
		line("  retry: %s", rec.Error.Retry)
	}
	line("Detail: %s", doc.Continuation.Command)
	if doc.Continuation.List != "" {
		line("List: %s", doc.Continuation.List)
	}
}

// renderRequestList renders one page as text from the same document the
// JSON form carries, with the route to the records the page omitted.
func renderRequestList(doc *requestRecordDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	line("Request records (read-only; from the run's durable receipts)")
	if len(doc.Requests) == 0 {
		if doc.List != nil && doc.List.HasMore {
			line("No records on this page; more exist. Read them with: %s", doc.Continuation.List)
		} else {
			line("No request records. Submissions and decisions appear here as they are recorded.")
		}
		line("Detail: %s", doc.Continuation.Command)
		return
	}
	omitted := 0
	if doc.List != nil {
		omitted = doc.List.Omitted["records"]
	}
	if omitted > 0 {
		line("Records: %d shown, %d more retained", len(doc.Requests), omitted)
	} else {
		line("Records: %d shown", len(doc.Requests))
	}
	for _, rec := range doc.Requests {
		waiting := ""
		if rec.WaitingOn != nil {
			waiting = " | waiting on " + rec.WaitingOn.Reason
		}
		key := ""
		if rec.IdempotencyKey != "" {
			key = " | key " + rec.IdempotencyKey
		}
		line("%s  [%s]  %s  age %s%s%s", rec.ID, rec.State, rec.Type, statusDuration(rec.AgeSeconds), waiting, key)
		if rec.WaitingOn != nil && rec.WaitingOn.Detail != "" {
			line("  %s", rec.WaitingOn.Detail)
		}
		if len(rec.AffectedTasks) > 0 {
			line("  affected tasks: %s", strings.Join(rec.AffectedTasks, ", "))
		}
		if rec.Error != nil {
			line("  error: %s", rec.Error.Message)
		}
		line("  detail: looper requests %s --contract v2", rec.ID)
	}
	if doc.Continuation.List != "" {
		line("Next page: %s", doc.Continuation.List)
	}
	line("Detail: %s", doc.Continuation.Command)
}
