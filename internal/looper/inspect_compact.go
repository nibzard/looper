package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The compact task inspection contract,
// docs/contracts/schemas/v2/inspect-compact.schema.json.
const (
	inspectCompactSchemaVersion = 2
	inspectCompactKind          = "looper.inspect.compact"
	// inspectBytesBudget is the response budget for one compact inspection:
	// the task contract, the current attempt, a bounded window of history,
	// the newest checks, timing provenance, and evidence references stay
	// within 8 KiB no matter how many repairs or how large the recorded
	// feedback grew. The complete document stays one --full away.
	inspectBytesBudget = 8192
	// defaultInspectPageLimit is the bounded tail a bare inspection returns.
	defaultInspectPageLimit = 10
	// maxInspectPageLimit is the largest attempt or check page one compact
	// inspection may return; a bigger need is an explicit --full read. The
	// byte budget may still return fewer than requested; the page reports
	// what it returned and the continuation keeps the history gapless.
	maxInspectPageLimit = 100
	// maxInspectEvidence bounds the evidence references.
	maxInspectEvidence = 4
	// maxInspectList bounds the short identifier lists of the task contract.
	maxInspectList = 8
	// inspectRouteID bounds an identifier that appears inside a detail
	// command. A longer identifier would make the route itself unbounded, so
	// the route is dropped instead of truncated into a wrong command.
	inspectRouteID = 64
)

// inspectRouteIDOk reports whether an identifier is short enough to embed in
// an executable route command, counting the bytes the JSON encoder emits so
// an escape-heavy identifier cannot turn the route itself unbounded.
func inspectRouteIDOk(id string) bool { return jsonEscapedLen(id) <= inspectRouteID }

// inspectFit names the bounds one rendering pass applies. A document that
// exceeds the byte budget renders again with the next fit: strings and
// excerpts shrink first, then bounded collections drop entries, oldest first
// for a tail page and newest-bounded for a forward page, so the continuation
// stays gapless at every fit. Every bound is measured in the bytes the JSON
// encoder emits, so text that escapes heavily — an ampersand becomes six
// bytes — shrinks with the same ladder instead of overflowing it. The ladder
// ends in inspectTerminalFit, which keeps one attempt, one check, one log,
// and a task contract whose every string is one rune at worst, so a valid
// document always fits whatever the recorded content holds.
type inspectFit struct {
	stringMax   int
	excerptMax  int
	attemptsMax int
	checksMax   int
	logsMax     int
	linesMax    int
}

var inspectFits = []inspectFit{
	{160, 512, 10, 10, 6, 8},
	{128, 384, 8, 8, 5, 6},
	{96, 256, 6, 6, 4, 4},
	{72, 160, 4, 4, 3, 3},
	{56, 96, 3, 3, 2, 2},
	{40, 64, 2, 2, 1, 1},
	{28, 32, 1, 1, 1, 0},
	inspectTerminalFit,
}

// inspectTerminalFit is the ladder's last resort. Its string bounds leave
// every emitted string a single rune at worst, so the document it renders is
// structure alone — a size fixed by the contract's fields, not by how large
// the recorded content grew. TestInspectTerminalFitBoundsAnyContent pins
// that worst case under the byte budget.
var inspectTerminalFit = inspectFit{stringMax: 8, excerptMax: 16, attemptsMax: 1, checksMax: 1, logsMax: 1, linesMax: 0}

// inspectCompactDocument is the one JSON document `looper inspect <task>
// --contract v2` returns: the task contract, the current attempt, the task's
// blockers, a bounded page of attempt and check history, the timing
// provenance, and the evidence references that keep every remaining byte
// reachable. It validates against the published schema and stays within
// inspectBytesBudget.
type inspectCompactDocument struct {
	ContractVersion int                  `json:"contract_version"`
	SchemaVersion   int                  `json:"schema_version"`
	Kind            string               `json:"kind"`
	GeneratedAt     string               `json:"generated_at"`
	Project         inspectProject       `json:"project"`
	Run             inspectRunRef        `json:"run"`
	Controller      inspectControllerRef `json:"controller"`
	Task            inspectTaskContract  `json:"task"`
	Attempts        inspectAttemptsDoc   `json:"attempts"`
	Checks          inspectChecksDoc     `json:"checks"`
	Timing          TaskTimingReport     `json:"timing"`
	Evidence        []inspectEvidenceRef `json:"evidence,omitempty"`
	Limits          inspectLimits        `json:"limits"`
	Detail          inspectDetail        `json:"detail"`
}

type inspectProject struct {
	Root     string `json:"root"`
	TodoPath string `json:"todo_path"`
}

type inspectRunRef struct {
	ID                string `json:"id"`
	ControllerSession string `json:"controller_session,omitempty"`
	Mode              string `json:"mode"`
}

// inspectControllerRef carries the ownership answer that makes active and
// retained work distinguishable: an active attempt a controller owns is work
// in flight, the same attempt without an owner is retained work.
type inspectControllerRef struct {
	State string `json:"state"`
	Owned bool   `json:"owned"`
}

// inspectTaskContract is the editable outcome the acceptance invariant guards.
// The digest is the task contract hash the engine itself compares, so a caller
// can tell a reopened task from a changed requirement.
type inspectTaskContract struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Status         string         `json:"status"`
	Priority       int            `json:"priority"`
	ContractDigest string         `json:"contract_digest"`
	CreatedAt      string         `json:"created_at,omitempty"`
	UpdatedAt      string         `json:"updated_at,omitempty"`
	UnblockWhen    string         `json:"unblock_when,omitempty"`
	DependsOn      []string       `json:"depends_on"`
	Files          []string       `json:"files"`
	Steps          []string       `json:"steps,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
	Blockers       []string       `json:"blockers"`
	Details        inspectExcerpt `json:"details"`
}

// inspectExcerpt bounds one free-text string: the head and, when the recorded
// text is longer, the tail, with the complete byte count and a truncation
// marker, so truncation is stated, never silent.
type inspectExcerpt struct {
	Text      string `json:"text"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

// inspectAttempt is one attempt of the task's history. The ordinal is the
// attempt's one-based position in the saved history: it is the stable paging
// position, attempts only ever append, and a forward read after zero covers
// every record, so pages have no gaps.
type inspectAttempt struct {
	Ordinal      int                     `json:"ordinal"`
	AttemptID    string                  `json:"attempt_id"`
	Phase        string                  `json:"phase"`
	Agent        string                  `json:"agent,omitempty"`
	Active       bool                    `json:"active"`
	Retained     bool                    `json:"retained"`
	Repairs      int                     `json:"repairs"`
	Repeats      int                     `json:"repeats"`
	Diagnosed    bool                    `json:"diagnosed,omitempty"`
	Base         string                  `json:"base,omitempty"`
	Directory    string                  `json:"directory,omitempty"`
	ResultCommit string                  `json:"result_commit,omitempty"`
	FailureHash  string                  `json:"failure_hash,omitempty"`
	Summary      *inspectAttemptSummary  `json:"summary,omitempty"`
	Verification *inspectVerificationRef `json:"verification,omitempty"`
	Feedback     *inspectExcerpt         `json:"feedback,omitempty"`
	Output       *inspectOutputRef       `json:"output,omitempty"`
	Logs         []string                `json:"logs"`
	LogsTotal    int                     `json:"logs_total"`
}

type inspectAttemptSummary struct {
	Status string `json:"status"`
	Text   string `json:"text"`
}

type inspectVerificationRef struct {
	OK       bool           `json:"ok"`
	Feedback inspectExcerpt `json:"feedback"`
}

// inspectOutputRef is the live output evidence of an active attempt. Progress
// describes output, never task progress: unknown means no output at all.
type inspectOutputRef struct {
	Log        string `json:"log"`
	Bytes      int64  `json:"bytes"`
	AgeSeconds *int   `json:"age_seconds,omitempty"`
	Progress   string `json:"progress"`
}

type inspectPageMeta struct {
	After    int64  `json:"after"`
	Returned int    `json:"returned"`
	Limit    int    `json:"limit"`
	Order    string `json:"order"`
	HasMore  bool   `json:"has_more"`
}

type inspectContinuation struct {
	Command string `json:"command"`
	HasMore bool   `json:"has_more"`
}

type inspectAttemptsDoc struct {
	Total        int                 `json:"total"`
	Current      *inspectAttempt     `json:"current"`
	Items        []inspectAttempt    `json:"items"`
	Page         inspectPageMeta     `json:"page"`
	Continuation inspectContinuation `json:"continuation"`
}

type inspectCheckItem struct {
	ID        int64  `json:"id"`
	Time      string `json:"time"`
	AttemptID string `json:"attempt_id"`
	Revision  string `json:"revision"`
	Kind      string `json:"kind"`
	Result    string `json:"result"`
	Artifact  string `json:"artifact"`
}

type inspectChecksDoc struct {
	Total        int                 `json:"total"`
	Items        []inspectCheckItem  `json:"items"`
	Page         inspectPageMeta     `json:"page"`
	Continuation inspectContinuation `json:"continuation"`
}

type inspectEvidenceRef struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
	Command   string `json:"command,omitempty"`
}

type inspectLimits struct {
	BytesBudget int            `json:"bytes_budget"`
	StringMax   int            `json:"string_max"`
	ExcerptMax  int            `json:"excerpt_max"`
	AttemptsMax int            `json:"attempts_max"`
	ChecksMax   int            `json:"checks_max"`
	LogsMax     int            `json:"logs_max"`
	Omitted     map[string]int `json:"omitted"`
}

type inspectDetail struct {
	Full    string `json:"full,omitempty"`
	History string `json:"history,omitempty"`
	Events  string `json:"events,omitempty"`
}

// excerptOf bounds one string for the compact document: the head and, for
// longer text, the tail around a truncation marker, cut on rune boundaries so
// the excerpt is always valid text. The bound is the size the JSON encoder
// emits for the excerpt, so text that escapes heavily is cut harder than
// plain text instead of overflowing the document budget.
func excerptOf(s string, max int) inspectExcerpt {
	if max < 8 {
		max = 8
	}
	if jsonEscapedLen(s) <= max {
		return inspectExcerpt{Text: s, Bytes: len(s)}
	}
	const mark = " … "
	inner := max - len(mark)
	head := jsonPrefixLen(s, inner*3/5)
	tail := len(s)
	if budget := inner - jsonEscapedLen(s[:head]); budget > 0 {
		if start := jsonSuffixStart(s, budget); start > head {
			tail = start
		}
	}
	return inspectExcerpt{Text: s[:head] + mark + s[tail:], Bytes: len(s), Truncated: true}
}

// inspectEvidence is the bounded read one compact inspection renders from.
// The attempt and check windows are fetched once, bounded by the requested
// limit; every later rendering pass only drops entries.
type inspectEvidence struct {
	config   Config
	store    *Store
	snap     *StoreSnapshot
	status   *statusEvidence
	task     *Task
	attempts []*Attempt // the task's attempts, oldest first
	logs     [][]string // each attempt's log paths, index-aligned, newest last
	outputs  map[string]*WorkerObservation
	window   inspectWindow
	checks   *ChecksPage
	timing   TaskTimingReport
	now      time.Time
	// newestCheck is the task's newest check record, whatever check page the
	// caller reads, so the newest-verification-artifact reference stays true.
	newestCheck *CheckRecord
	// checkLimit is the page bound the checks were fetched with.
	checkLimit int
}

// inspectWindow is the fetched attempt window: ascending ordinals, the
// exclusive lower bound they were read after, and whether more attempts exist
// above it.
type inspectWindow struct {
	tail         bool
	after        int64
	limit        int
	ordinals     []int64
	hasMoreAbove bool
}

// collectInspectEvidence performs the bounded reads a compact inspection
// needs. It shares the compact status evidence, so ownership, liveness, and
// output activity come from the same summary model the status document uses.
func collectInspectEvidence(c Config, store *Store, snap *StoreSnapshot, t *Task, now time.Time) (*inspectEvidence, error) {
	status, err := collectStatusEvidence(c, store, snap, now)
	if err != nil {
		return nil, err
	}
	limit := c.EventLimit
	if limit <= 0 {
		limit = defaultInspectPageLimit
	}
	ev := &inspectEvidence{config: c, store: store, snap: snap, status: status, task: t, now: now, checkLimit: limit}
	for _, a := range snap.State.Attempts {
		if a.Task.ID == t.ID {
			ev.attempts = append(ev.attempts, a)
			ev.logs = append(ev.logs, attemptLogs(a))
		}
	}
	ev.outputs = map[string]*WorkerObservation{}
	for i := range status.obs.Workers {
		if status.obs.Workers[i].TaskID == t.ID {
			ev.outputs[status.obs.Workers[i].AttemptID] = &status.obs.Workers[i]
		}
	}
	ev.window = attemptWindow(len(ev.attempts), c.After, limit, c.AfterSet)
	ids := make([]string, len(ev.attempts))
	for i, a := range ev.attempts {
		ids[i] = a.ID
	}
	checks, err := store.ChecksPage(ids, c.CheckAfter, limit, !c.CheckAfterSet)
	if err != nil {
		return nil, err
	}
	ev.checks = checks
	// The evidence section names the task's newest verification artifact. A
	// forward check page can stop far below the newest row, so that record
	// comes from its own one-row tail read rather than from the fetched page.
	newest := checks
	if c.CheckAfterSet {
		if newest, err = store.ChecksPage(ids, 0, 1, true); err != nil {
			return nil, err
		}
	}
	if n := len(newest.Items); n > 0 {
		ev.newestCheck = &newest.Items[n-1]
	}
	timing, err := store.Timing(now)
	if err != nil {
		return nil, err
	}
	// The report echoes the task identifier; renderInspectDocument bounds it
	// with the fit that applied, like every other string in the document.
	ev.timing = timing.task(t.ID)
	return ev, nil
}

// attemptWindow selects the ordinals of one attempt page. Ordinals are
// one-based positions in the task's saved history, so a forward read after
// zero, like the event journal's, covers every record. A bare query reads the
// newest tail; an explicit after reads strictly after that position.
func attemptWindow(total int, after int64, limit int, afterSet bool) inspectWindow {
	w := inspectWindow{tail: !afterSet, after: after, limit: limit}
	if total == 0 || limit <= 0 || after < 0 {
		return w
	}
	last := int64(total)
	if !afterSet {
		lo := int64(1)
		if int64(limit) < int64(total) {
			lo = int64(total) - int64(limit) + 1
		}
		for o := lo; o <= last; o++ {
			w.ordinals = append(w.ordinals, o)
		}
		return w
	}
	if after >= last {
		return w
	}
	hi := min(last, after+int64(limit))
	for o := after + 1; o <= hi; o++ {
		w.ordinals = append(w.ordinals, o)
	}
	w.hasMoreAbove = hi < last
	return w
}

// compactInspectTask serves the negotiated compact inspection under contract
// version 2. The legacy document stays the default and the --full response.
func compactInspectTask(c Config, store *Store, out io.Writer) error {
	if c.InspectTask == "" {
		p := usageFailure("usage_invalid", "usage: looper inspect <task-id> [--json]")
		// The legacy contract reported this from the read path, not parsing.
		p.legacyExit = exitOperationFailed
		return p
	}
	snap, err := store.Snapshot()
	if err != nil {
		_, dir := projectPaths(c)
		return stateUnreadableError(filepath.Join(dir, "state.db"), err)
	}
	if snap.State == nil {
		p := runNotFoundForRun(c, "", "looper")
		p.Message = "no saved run"
		return p
	}
	var task *Task
	for i := range snap.State.Backlog.Tasks {
		if snap.State.Backlog.Tasks[i].ID == c.InspectTask {
			task = &snap.State.Backlog.Tasks[i]
			break
		}
	}
	if task == nil {
		return taskNotFoundError(c.InspectTask, len(snap.State.Backlog.Tasks))
	}
	ev, err := collectInspectEvidence(c, store, snap, task, time.Now())
	if err != nil {
		return err
	}
	doc := fitInspectDocument(ev)
	if c.JSON {
		return json.NewEncoder(out).Encode(doc)
	}
	renderCompactInspect(doc, out)
	return nil
}

// fitInspectDocument renders the evidence within the byte budget, shrinking
// bounds until the document fits. Because every bound counts emitted bytes
// and the ladder ends in a fit whose strings are one rune at worst, the
// returned document is always valid JSON at or below inspectBytesBudget,
// whatever the recorded content holds.
func fitInspectDocument(ev *inspectEvidence) *inspectCompactDocument {
	var doc *inspectCompactDocument
	for _, fit := range inspectFits {
		doc = renderInspectDocument(ev, fit)
		if inspectEncoded(doc) <= inspectBytesBudget {
			return doc
		}
	}
	return doc
}

// inspectEncoded reports the size of the document as the CLI emits it,
// including the trailing newline the encoder writes.
func inspectEncoded(doc *inspectCompactDocument) int {
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(doc)
	return buf.Len()
}

// renderInspectDocument renders the evidence into one compact document under
// the given bounds. Plain output renders from the same structure, so the two
// outputs cannot disagree. Every string bound counts the bytes the JSON
// encoder emits, quotes excluded, so escaped content cannot outgrow the bound.
func renderInspectDocument(ev *inspectEvidence, fit inspectFit) *inspectCompactDocument {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	s := ev.snap.State
	doc := &inspectCompactDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   inspectCompactSchemaVersion,
		Kind:            inspectCompactKind,
		GeneratedAt:     ev.now.UTC().Format(time.RFC3339),
	}
	root, todo := ev.config.Root, ev.config.Todo
	if s.Root != "" {
		root = s.Root
	}
	if s.Todo != "" {
		todo = s.Todo
	}
	doc.Project = inspectProject{Root: clamp(root), TodoPath: clamp(todo)}
	doc.Run = inspectRunRef{ID: clamp(runIdentifier(s, ev.status.info)), Mode: "unknown"}
	if ev.status.info != nil {
		doc.Run.ControllerSession = clamp(ev.status.info.Session)
		switch ev.status.info.Mode {
		case ControllerModeForeground, ControllerModeDetached:
			doc.Run.Mode = ev.status.info.Mode
		}
	}
	state, _ := controllerState(ev.status)
	doc.Controller = inspectControllerRef{State: state, Owned: ev.status.owned}
	doc.Task = inspectTaskContractOf(ev.task, fit)
	doc.Attempts = inspectAttemptsOf(ev, fit)
	doc.Checks = inspectChecksOf(ev, fit)
	doc.Timing = ev.timing
	// The timing report echoes the task identifier; bound it like every
	// other string so an adversarial identifier cannot bypass the budget.
	doc.Timing.TaskID = clamp(ev.timing.TaskID)
	doc.Evidence = inspectEvidenceOf(ev, fit)
	doc.Limits = inspectLimitsOf(ev, fit)
	doc.Detail = inspectDetailOf(ev)
	return doc
}

// inspectTaskContractOf renders the task contract with bounded collections
// and excerpts; counts of what the bounds left out live in limits.omitted.
func inspectTaskContractOf(t *Task, fit inspectFit) inspectTaskContract {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	c := inspectTaskContract{
		ID:             clamp(t.ID),
		Title:          clampJSONString(t.Title, fit.excerptMax),
		Status:         t.Status,
		Priority:       t.Priority,
		ContractDigest: t.Contract(),
		CreatedAt:      clamp(t.CreatedAt),
		UpdatedAt:      clamp(t.UpdatedAt),
		UnblockWhen:    clampJSONString(t.UnblockWhen, fit.excerptMax),
		DependsOn:      boundedIDs(t.DependsOn, fit.stringMax),
		Files:          boundedIDs(t.Files, fit.stringMax),
		Steps:          []string{},
		Blockers:       []string{},
		Details:        excerptOf(t.Details, fit.excerptMax),
	}
	for _, step := range t.Steps {
		if len(c.Steps) >= fit.linesMax {
			break
		}
		c.Steps = append(c.Steps, clampJSONString(step, fit.excerptMax))
	}
	for _, b := range t.Blockers {
		if len(c.Blockers) >= fit.linesMax {
			break
		}
		c.Blockers = append(c.Blockers, clampJSONString(b, fit.excerptMax))
	}
	if len(c.Steps) == 0 {
		c.Steps = nil
	}
	for _, tag := range t.Tags {
		if len(c.Tags) >= maxInspectList {
			break
		}
		c.Tags = append(c.Tags, clamp(tag))
	}
	return c
}

// boundedIDs clamps a short identifier list, keeping the first entries.
func boundedIDs(values []string, max int) []string {
	out := []string{}
	for _, v := range values {
		if len(out) >= maxInspectList {
			break
		}
		out = append(out, clampJSONString(v, max))
	}
	return out
}

// inspectAttemptOf renders one attempt under the fit bounds. The ordinal is
// the attempt's one-based position in the task's saved history. Log paths are
// relative to the attempt directory, which the entry names, so several logs
// stay small without losing their location.
func inspectAttemptOf(ev *inspectEvidence, ordinal int, fit inspectFit) inspectAttempt {
	a := ev.attempts[ordinal-1]
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	entry := inspectAttempt{
		Ordinal:      ordinal,
		AttemptID:    clamp(a.ID),
		Phase:        clamp(a.Phase),
		Agent:        clamp(a.Agent),
		Active:       activeAttempt(a),
		Retained:     activeAttempt(a) && !ev.status.owned,
		Repairs:      a.Repairs,
		Repeats:      a.Repeats,
		Diagnosed:    a.Diagnosed,
		Base:         clamp(a.Base),
		Directory:    clamp(a.Directory),
		ResultCommit: clamp(a.ResultCommit),
		FailureHash:  clamp(a.FailureHash),
		Logs:         []string{},
	}
	if a.Summary != nil {
		entry.Summary = &inspectAttemptSummary{
			Status: a.Summary.Status,
			Text:   clampJSONString(a.Summary.Summary, fit.excerptMax),
		}
	}
	if a.Verification != nil {
		verdict := inspectVerificationRef{OK: a.Verification.OK, Feedback: excerptOf(a.Verification.Feedback, fit.excerptMax)}
		entry.Verification = &verdict
	}
	if a.Feedback != "" {
		feedback := excerptOf(a.Feedback, fit.excerptMax)
		entry.Feedback = &feedback
	}
	if entry.Active {
		if w := ev.outputs[a.ID]; w != nil {
			logPath := w.Log
			if rel, relErr := filepath.Rel(a.Directory, w.Log); relErr == nil && !strings.HasPrefix(rel, "..") {
				logPath = rel
			}
			output := inspectOutputRef{Log: clamp(logPath), Bytes: w.OutputBytes, Progress: "unknown"}
			if w.OutputAge != nil {
				age := int(*w.OutputAge)
				output.AgeSeconds = &age
				output.Progress = "advancing"
				if *w.OutputAge > int64(statusQuietAfter.Seconds()) {
					output.Progress = "quiet"
				}
			}
			entry.Output = &output
		}
	}
	logs := ev.logs[ordinal-1]
	entry.LogsTotal = len(logs)
	newest := logs
	if len(newest) > fit.logsMax {
		newest = newest[len(newest)-fit.logsMax:]
	}
	for _, path := range newest {
		if rel, err := filepath.Rel(a.Directory, path); err == nil && !strings.HasPrefix(rel, "..") {
			entry.Logs = append(entry.Logs, clamp(rel))
			continue
		}
		entry.Logs = append(entry.Logs, clamp(path))
	}
	return entry
}

// inspectAttemptsOf renders the attempt page and the current attempt. The
// current attempt is always the newest one, whatever page the caller reads,
// so a bounded history read never hides the live state.
func inspectAttemptsOf(ev *inspectEvidence, fit inspectFit) inspectAttemptsDoc {
	section := inspectAttemptsDoc{Total: len(ev.attempts), Items: []inspectAttempt{}}
	if total := len(ev.attempts); total > 0 {
		current := inspectAttemptOf(ev, total, fit)
		section.Current = &current
	}
	shown := ev.window.ordinals
	dropped := 0
	if len(shown) > fit.attemptsMax {
		dropped = len(shown) - fit.attemptsMax
		if ev.window.tail {
			shown = shown[len(shown)-fit.attemptsMax:]
		} else {
			shown = shown[:fit.attemptsMax]
		}
	}
	for _, o := range shown {
		section.Items = append(section.Items, inspectAttemptOf(ev, int(o), fit))
	}
	hasMore := false
	if len(shown) > 0 {
		if ev.window.tail {
			hasMore = shown[0] > 1
		} else {
			hasMore = dropped > 0 || ev.window.hasMoreAbove
		}
	}
	after := int64(0)
	if !ev.window.tail {
		after = ev.window.after
	}
	section.Page = inspectPageMeta{
		After: after, Returned: len(shown), Limit: ev.window.limit,
		Order: "ascending", HasMore: hasMore,
	}
	cursor := after
	if len(shown) > 0 {
		cursor = shown[len(shown)-1]
	}
	section.Continuation = inspectContinuation{
		Command: inspectContinuationCommand(ev.config.InspectTask, cursor, ev.window.limit),
		HasMore: hasMore,
	}
	if ev.window.tail && hasMore {
		// The tail stopped at its bound with older attempts below it; the
		// route into that history is the forward read from the beginning.
		section.Continuation.Command = inspectContinuationCommand(ev.config.InspectTask, 0, ev.window.limit)
	}
	return section
}

// inspectRouteTarget names the task a route command may embed: the
// identifier when it fits, else a placeholder, so the command never carries
// an unbounded or wrongly truncated identifier.
func inspectRouteTarget(id string) string {
	if inspectRouteIDOk(id) {
		return id
	}
	return "<task-id>"
}

// inspectChecksOf renders the check page. Checks page by their row
// identifier, independent of the attempt window, so a heavily repaired
// attempt's check history stays reachable page by page.
func inspectChecksOf(ev *inspectEvidence, fit inspectFit) inspectChecksDoc {
	section := inspectChecksDoc{Total: ev.checks.Total, Items: []inspectCheckItem{}}
	items := ev.checks.Items
	dropped := 0
	if len(items) > fit.checksMax {
		dropped = len(items) - fit.checksMax
		if !ev.config.CheckAfterSet {
			// A tail keeps its newest records.
			items = items[len(items)-fit.checksMax:]
		} else {
			items = items[:fit.checksMax]
		}
	}
	for _, check := range items {
		section.Items = append(section.Items, inspectCheckItem{
			ID: check.ID, Time: clampJSONString(check.Time, fit.stringMax), AttemptID: clampJSONString(check.AttemptID, fit.stringMax),
			Revision: clampJSONString(check.Revision, fit.stringMax), Kind: clampJSONString(check.Kind, fit.stringMax), Result: clampJSONString(check.Result, fit.stringMax),
			Artifact: clampJSONString(check.Artifact, fit.excerptMax),
		})
	}
	hasMore := ev.checks.HasMore || dropped > 0
	section.Page = inspectPageMeta{
		After: ev.checks.After, Returned: len(items), Limit: ev.checkLimit,
		Order: "ascending", HasMore: hasMore,
	}
	// The continuation advances past exactly what was shown. When the byte
	// budget dropped records from a fetched forward window, advancing to the
	// fetched cursor would skip them, so the position is the last shown item.
	cursor := ev.checks.After
	if !ev.config.CheckAfterSet {
		cursor = 0
	}
	if len(items) > 0 {
		cursor = items[len(items)-1].ID
	}
	section.Continuation = inspectContinuation{
		Command: inspectCheckContinuationCommand(ev.config.InspectTask, cursor, ev.checkLimit),
		HasMore: hasMore,
	}
	if !ev.config.CheckAfterSet && hasMore {
		// The tail names the forward read that walks the whole history.
		section.Continuation.Command = inspectCheckContinuationCommand(ev.config.InspectTask, 0, ev.checkLimit)
	}
	return section
}

// inspectEvidenceOf names the retained evidence a caller can read on its
// own: the live output, the newest verification artifact, the result commit,
// and the task's journal page. Paths render relative to their attempt
// directory, like the per-attempt logs, so a long runtime path cannot push
// the reference past its bound.
func inspectEvidenceOf(ev *inspectEvidence, fit inspectFit) []inspectEvidenceRef {
	clamp := func(s string) string { return clampJSONString(s, fit.stringMax) }
	out := []inspectEvidenceRef{}
	add := func(kind, reference, command string) {
		if len(out) >= maxInspectEvidence || reference == "" {
			return
		}
		out = append(out, inspectEvidenceRef{Kind: kind, Reference: clamp(reference), Command: command})
	}
	// relativeTo names a path relative to the attempt that owns it.
	relativeTo := func(attemptID, path string) string {
		for _, a := range ev.attempts {
			if a.ID != attemptID {
				continue
			}
			if rel, err := filepath.Rel(a.Directory, path); err == nil && !strings.HasPrefix(rel, "..") {
				return rel
			}
			break
		}
		return path
	}
	if total := len(ev.attempts); total > 0 {
		newest := ev.attempts[total-1]
		if w := ev.outputs[newest.ID]; w != nil && w.Log != "" {
			add("log", relativeTo(newest.ID, w.Log), "")
		}
		add("commit", newest.ResultCommit, "")
	}
	// The newest verification artifact belongs to the task, not to whatever
	// check page the caller read: a forward page can stop far below the
	// newest row, so the reference comes from the task's newest record.
	if newest := ev.newestCheck; newest != nil {
		add("log", relativeTo(newest.AttemptID, newest.Artifact), "")
	}
	add("event", "task events "+ev.task.ID, "looper events --contract v2 --task "+inspectRouteTarget(ev.task.ID))
	return out
}

func inspectLimitsOf(ev *inspectEvidence, fit inspectFit) inspectLimits {
	task := ev.task
	omitted := map[string]int{
		"attempts":   max(0, len(ev.window.ordinals)-fit.attemptsMax),
		"checks":     max(0, len(ev.checks.Items)-fit.checksMax),
		"blockers":   max(0, len(task.Blockers)-fit.linesMax),
		"steps":      max(0, len(task.Steps)-fit.linesMax),
		"depends_on": max(0, len(task.DependsOn)-maxInspectList),
		"files":      max(0, len(task.Files)-maxInspectList),
		"tags":       max(0, len(task.Tags)-maxInspectList),
	}
	logsOmitted := 0
	for _, logs := range ev.logs {
		logsOmitted += max(0, len(logs)-fit.logsMax)
	}
	omitted["logs"] = logsOmitted
	return inspectLimits{
		BytesBudget: inspectBytesBudget, StringMax: fit.stringMax, ExcerptMax: fit.excerptMax,
		AttemptsMax: fit.attemptsMax, ChecksMax: fit.checksMax, LogsMax: fit.logsMax,
		Omitted: omitted,
	}
}

// inspectDetailOf names the routes that return what the bounded document left
// out: the complete document, the paged attempt history, and the task's
// journal page. A route whose identifier would be unbounded is omitted
// rather than truncated into a wrong command.
func inspectDetailOf(ev *inspectEvidence) inspectDetail {
	id := ev.config.InspectTask
	if !inspectRouteIDOk(id) {
		return inspectDetail{}
	}
	limit := ev.window.limit
	return inspectDetail{
		Full:    "looper inspect " + id + " --contract v2 --full --json",
		History: inspectContinuationCommand(id, 0, limit),
		Events:  "looper events --contract v2 --task " + id,
	}
}

// inspectContinuationCommand names the exact next attempt page: the same
// contract and page size, continuing after the given attempt ordinal.
func inspectContinuationCommand(taskID string, after int64, limit int) string {
	return strings.Join([]string{
		"looper", "inspect", inspectRouteTarget(taskID), "--contract", "v2",
		"--after", strconv.FormatInt(after, 10), "--limit", strconv.Itoa(limit),
	}, " ")
}

// inspectCheckContinuationCommand names the exact next check page, continuing
// after the given check identifier.
func inspectCheckContinuationCommand(taskID string, after int64, limit int) string {
	return strings.Join([]string{
		"looper", "inspect", inspectRouteTarget(taskID), "--contract", "v2",
		"--checks-after", strconv.FormatInt(after, 10), "--limit", strconv.Itoa(limit),
	}, " ")
}

// renderCompactInspect renders the same document as text: the task contract,
// the current attempt, and the routes into the history it bounded. Text
// passes through sanitize so recorded content cannot control the terminal.
func renderCompactInspect(doc *inspectCompactDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	text := func(s string) string { return sanitize(s) }
	line("Task: %s  %s  priority %d", text(doc.Task.ID), doc.Task.Status, doc.Task.Priority)
	if doc.Controller.Owned {
		line("Controller: %s | run %s", doc.Controller.State, text(doc.Run.ID))
	} else {
		line("Controller: %s | no controller owns the repository", doc.Controller.State)
	}
	if doc.Task.Details.Text != "" {
		line("Details: %s", text(doc.Task.Details.Text))
		if doc.Task.Details.Truncated {
			line("Details: %d bytes recorded; the complete text is in the full document", doc.Task.Details.Bytes)
		}
	}
	line("Contract digest: %s", doc.Task.ContractDigest)
	for _, b := range doc.Task.Blockers {
		line("Blocked: %s", text(b))
	}
	if current := doc.Attempts.Current; current != nil {
		state := "historical"
		switch {
		case current.Active && current.Retained:
			state = "active | retained: no controller owns this work"
		case current.Active:
			state = "active"
		}
		line("Current attempt: %s | %s | repairs %d | repeats %d | %s", text(current.AttemptID), current.Phase, current.Repairs, current.Repeats, state)
		if current.Feedback != nil {
			line("  feedback: %s", text(current.Feedback.Text))
			if current.Feedback.Truncated {
				line("  feedback: %d bytes recorded; read the rest with: %s", current.Feedback.Bytes, doc.Detail.Full)
			}
		}
		if current.Output != nil {
			line("  output: %s", text(current.Output.Log))
		}
	} else {
		line("Current attempt: none recorded")
	}
	omitted := ""
	if n := doc.Limits.Omitted["attempts"]; n > 0 {
		omitted = fmt.Sprintf(" | %d more not shown", n)
	}
	line("Attempts: %d shown of %d%s | %s", doc.Attempts.Page.Returned, doc.Attempts.Total, omitted, doc.Attempts.Continuation.Command)
	omitted = ""
	if n := doc.Limits.Omitted["checks"]; n > 0 {
		omitted = fmt.Sprintf(" | %d more not shown", n)
	}
	line("Checks: %d shown of %d%s | %s", doc.Checks.Page.Returned, doc.Checks.Total, omitted, doc.Checks.Continuation.Command)
	printTaskTiming(out, doc.Timing)
	for _, e := range doc.Evidence {
		command := ""
		if e.Command != "" {
			command = " | " + e.Command
		}
		line("Evidence: %s %s%s", e.Kind, text(e.Reference), command)
	}
	line("Detail: %s", doc.Detail.Full)
	line("History: %s", doc.Detail.History)
	line("Events: %s", doc.Detail.Events)
}
