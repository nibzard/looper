package looper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The finite wait contract, docs/contracts/schemas/v2/wait.schema.json
// (AX-013). One command answers the four predicates the capabilities document
// advertises: a changed cursor, a task state, a request outcome, and a run
// outcome. Every wait is bounded by an explicit timeout, observes only, and
// never cancels the operation it watches: a timeout is an outcome of the
// wait, so the request, task, or controller underneath keeps its state.
const (
	waitSchemaVersion = 2
	waitKind          = "looper.wait"
	// waitPollInterval is the observation cadence of one wait. A wait reads
	// durable records, so it polls the store the way a followed event stream
	// does; this interval keeps the loop far from busy while a caller with a
	// short bound still sees a transition quickly.
	waitPollInterval = 250 * time.Millisecond
	// waitStringMax bounds every content-derived string the document carries.
	// The embedded request record keeps its own published bounds.
	waitStringMax = 256
	// waitRouteID bounds an identifier a route command embeds, in the bytes
	// the JSON encoder emits, so a caller-supplied identifier cannot grow the
	// document without limit. The shared status bound is tighter, so it gates
	// the routes first.
	waitRouteID = 256
)

// Wait predicate names, the vocabulary the capabilities contract describes.
const (
	WaitCursorChange   = "cursor_change"
	WaitTaskState      = "task_state"
	WaitRequestOutcome = "request_outcome"
	WaitRunOutcome     = "run_outcome"
)

// clientWaits lists the wait predicates this executable serves. `looper
// version --contract v2 --json` publishes them beside the command list, so a
// caller learns what it can wait for before it tries.
var clientWaits = []string{WaitCursorChange, WaitTaskState, WaitRequestOutcome, WaitRunOutcome}

// waitTaskStatuses are the task statuses the backlog schema defines. A wait
// may target any of them; the engine owns the transitions between them.
var waitTaskStatuses = map[string]bool{
	"todo": true, "doing": true, "blocked": true, "done": true, "cancelled": true,
}

// Wait outcome names. `satisfied` and `timeout` are the two results the
// command exists for; `cancelled` reports a caller that interrupted the wait,
// with the latest observation so the wait can resume from it.
const (
	waitSatisfied = "satisfied"
	waitTimeout   = "timeout"
	waitCancelled = "cancelled"
)

// waitDocument is the one JSON document `looper wait --contract v2 --json`
// returns, whatever the outcome was. It names the condition, the outcome, the
// latest cursor, and the evidence the predicate observed: the task's status,
// the request's durable record, or the controller session's end.
type waitDocument struct {
	ContractVersion int                `json:"contract_version"`
	SchemaVersion   int                `json:"schema_version"`
	Kind            string             `json:"kind"`
	Outcome         string             `json:"outcome"`
	Condition       waitConditionView  `json:"condition"`
	Observed        waitObserved       `json:"observed"`
	Cursor          eventsCursorRef    `json:"cursor"`
	Store           *eventsStoreRef    `json:"store,omitempty"`
	Controller      *waitControllerRef `json:"controller,omitempty"`
	Wait            waitTiming         `json:"wait"`
	Detail          waitDetailRoutes   `json:"detail"`
}

// waitConditionView names what the wait was for. Exactly one predicate's
// fields carry a target; the binding fields record the identity the wait
// started against, so a caller can tell a new state from an old one.
type waitConditionView struct {
	Kind string `json:"kind"`
	// Cursor is the exclusive position a cursor wait moves past.
	Cursor int64 `json:"cursor,omitempty"`
	// TaskID names the task a task wait observes; Status is the target status,
	// empty when the wait accepts any change. From records the status observed
	// when the wait began, so a change is distinguishable from a state that
	// held all along.
	TaskID string `json:"task_id,omitempty"`
	Status string `json:"status,omitempty"`
	From   string `json:"from,omitempty"`
	// RequestID names the durable request a request wait observes.
	RequestID string `json:"request_id,omitempty"`
	// Session binds a run wait to one controller session: the live owner when
	// the wait began, else the newest recorded session.
	Session string `json:"session,omitempty"`
}

// waitObserved carries the evidence the last observation read. HighWater is
// always the store's committed position, so a caller continues from it whether
// the condition held or not.
type waitObserved struct {
	At        string               `json:"at"`
	HighWater int64                `json:"high_water"`
	Task      *waitTaskObserved    `json:"task,omitempty"`
	Request   *requestRecordView   `json:"request,omitempty"`
	Session   *waitSessionObserved `json:"session,omitempty"`
	// Note states what the last observation showed when the condition did not
	// hold: the state that persists, and — when nothing can change it — that no
	// controller owns the repository.
	Note string `json:"note,omitempty"`
}

// waitTaskObserved reports one task's saved status.
type waitTaskObserved struct {
	Present bool   `json:"present"`
	Status  string `json:"status,omitempty"`
	Changed bool   `json:"changed,omitempty"`
}

// waitSessionObserved reports how the bound controller session ended. Outcome
// carries the store's recorded value — stopped, failed, cancelled, panicked,
// or interrupted — or running while the session holds, or unclean when the
// controller process ended without recording an exit.
type waitSessionObserved struct {
	ID              string `json:"id,omitempty"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	Unclean         bool   `json:"unclean,omitempty"`
	ShutdownEventID int64  `json:"shutdown_event_id,omitempty"`
}

// waitControllerRef names the ownership the wait observed, in the vocabulary
// the runs listing uses: owning, stale, stopped, or unknown.
type waitControllerRef struct {
	State   string `json:"state"`
	Session string `json:"session,omitempty"`
}

type waitTiming struct {
	TimeoutSeconds int `json:"timeout_seconds"`
	ElapsedSeconds int `json:"elapsed_seconds"`
}

type waitDetailRoutes struct {
	// Command re-issues the same wait with the position the observation
	// reached.
	Command string `json:"command"`
	Events  string `json:"events"`
	Status  string `json:"status"`
	// Follow names the streaming alternative a caller that can hold a stream
	// uses instead of a finite wait.
	Follow string `json:"follow"`
}

// exitCode reports how the wait ended on the transport. A satisfied wait is a
// completed command; a timeout is the wait's own outcome, and a cancellation
// is the caller's. Both keep the document, which carries the same facts the
// satisfied form does.
func (d *waitDocument) exitCode() int {
	switch d.Outcome {
	case waitTimeout:
		return exitWaitTimeout
	case waitCancelled:
		return exitCancelled
	default:
		return exitOK
	}
}

// waitTarget is the predicate one wait observes, with the identity it is bound
// to, captured when the wait began.
type waitTarget struct {
	kind string

	// cursor_change: satisfied once the store's committed high-water mark
	// passes this exclusive position.
	cursor int64

	// task_state: satisfied when the task reaches Status, or — with no target
	// status — when its saved status differs from the one recorded at
	// invocation. Absent is a status, so a task an import adds satisfies a
	// change wait by appearing.
	taskID      string
	taskStatus  string
	fromPresent bool
	fromStatus  string

	// request_outcome: satisfied when the durable record reaches a terminal
	// state, whatever that state decides.
	requestID string

	// run_outcome: bound to one controller session.
	session string
}

// waitRun carries one wait's connection and its latest observation. The
// attach follows the decision every one-shot observer makes (T105): a run no
// live controller owns opens immutable, so a wait on a cleanly stopped run
// creates nothing beside its database. Unlike a one-shot read, a wait spans
// the controllers that may start later, and an immutable attach cannot see
// past a checkpoint — so each observation checks the evidence that a writer
// appeared and re-attaches through the WAL read when it did. The durable
// records therefore stay readable and no transition is missed, without the
// wait ever writing a byte of the run's own state.
type waitRun struct {
	c     Config
	dir   string
	store *Store

	// storeStat is the database state the attach read; a change means a
	// writer checkpointed since, and an immutable attach must be refreshed.
	storeStat storeStamp
	// immutableAttach records that the attach opened immutable: it sees the
	// file as it was, so a writer that appears calls for a re-attach through
	// the WAL read.
	immutableAttach bool

	target  *waitTarget
	started time.Time

	// last holds the newest observation; satisfied reports whether it met the
	// condition.
	last      waitEvidence
	satisfied bool

	// controller holds the ownership read at invocation. A run wait refreshes
	// it every observation, because its predicate is about the controller.
	controller waitControllerRef

	// probe, when set, runs once per observation. Tests use it to pin the
	// polling cadence; production leaves it nil.
	probe func()
}

// storeStamp is the observable state of one database file: its size and
// modification time. A writer that checkpoints changes both, so a reader that
// attached immutable can tell its snapshot aged past what it can see.
type storeStamp struct {
	size int64
	mod  time.Time
}

// stampStore records the current state of one database file.
func stampStore(path string) storeStamp {
	info, err := os.Stat(path)
	if err != nil {
		return storeStamp{}
	}
	return storeStamp{size: info.Size(), mod: info.ModTime()}
}

// attach opens the store the wait reads, applying the same decision every
// one-shot observer makes: a run a live controller owns, or one whose
// write-ahead log still waits, attaches through the WAL; a cleanly stopped
// run attaches immutable, so the read creates nothing beside its database.
func (w *waitRun) attach() error {
	statePath := filepath.Join(w.dir, "state.db")
	owning := false
	if info, err := readController(w.dir); err == nil {
		owning = controllerAlive(info)
	}
	_, wal := os.Stat(statePath + "-wal")
	store, err := openRunStore(statePath, owning)
	if err != nil {
		return err
	}
	w.store = store
	w.storeStat = stampStore(statePath)
	// wal is the stat error: set means no log file waits. openRunStore
	// attached immutable exactly then — no live controller, no log — and only
	// that attach can miss a writer that appears later.
	w.immutableAttach = !owning && wal != nil
	return nil
}

// refresh re-attaches when the evidence says the current attach can no longer
// see the newest records: a live controller owns the repository, a write-ahead
// log waits to be folded in, or the database itself changed — a writer that
// started and stopped between two observations. A through-WAL attach already
// sees every later commit, so it is kept for the rest of the wait. A re-attach
// that fails keeps the current attach: a writer mid-checkpoint holds the
// database for a moment, and one busy poll is not an answer — the next
// observation retries.
func (w *waitRun) refresh() error {
	if w.store == nil {
		return w.attach()
	}
	if !w.immutableAttach {
		return nil
	}
	statePath := filepath.Join(w.dir, "state.db")
	_, wal := os.Stat(statePath + "-wal")
	state, _ := waitControllerState(w.dir)
	if wal == nil || state == "owning" || stampStore(statePath) != w.storeStat {
		_ = w.attach()
	}
	return nil
}

// close releases the attach. Every exit path — satisfied, timeout,
// cancellation, and error — runs it, so a wait never holds the run's records.
func (w *waitRun) close() {
	if w.store != nil {
		w.store.Close()
		w.store = nil
	}
}

// waitEvidence is what one observation of the target read.
type waitEvidence struct {
	at        time.Time
	highWater int64

	task    *waitTaskObserved
	request *requestQueryRecord
	session *waitSessionObserved
}

// runWait serves the finite wait command (AX-013). The command observes one
// saved run and returns one structured outcome: the satisfied condition, a
// timeout, or a caller cancellation, each with the latest cursor and the
// evidence the predicate read.
func runWait(ctx context.Context, c Config, out, stderr io.Writer) int {
	if c.JSON && c.Contract != ContractVersionV2 {
		p := incompatibleFailure("contract_version_required", "The wait outcome document is a contract version 2 document.")
		p.Expected = map[string]any{"contract": "v2"}
		p.Observed = map[string]any{"contract": "legacy", "option": "--json"}
		p.Actions = []ErrorAction{{
			Description: "Wait with the version 2 contract.",
			Command:     "looper wait --contract v2 --cursor 0 --timeout 30s",
		}}
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	path := filepath.Join(dir, "state.db")
	if _, err := os.Stat(path); err != nil {
		p := runNotFoundForRun(c, path, "looper")
		p.Message = "no saved run; a wait observes a saved run's records"
		return emitError(c, p, out, stderr)
	}
	w := &waitRun{c: c, dir: dir, started: wallClock()}
	defer w.close()
	if err := w.attach(); err != nil {
		return emitError(c, stateUnreadableError(path, err), out, stderr)
	}
	if err := w.resolveTarget(); err != nil {
		return emitError(c, stateUnreadableError(path, err), out, stderr)
	}
	w.controller.State, w.controller.Session = waitControllerState(dir)
	outcome, err := w.observeUntil(ctx, c.WaitFor)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	doc := w.document(outcome)
	if c.JSON {
		if err := json.NewEncoder(out).Encode(doc); err != nil {
			return exitOperationFailed
		}
		return doc.exitCode()
	}
	renderWaitDocument(doc, out)
	return doc.exitCode()
}

// resolveTarget turns the configured predicate into one bound target. The
// bindings are read once, here, so the wait reports the state it started
// against and never re-derives a moving target. A first read that cannot be
// served fails the wait: a change wait must not mistake an unreadable
// starting state for an absent task.
func (w *waitRun) resolveTarget() error {
	c, t := w.c, &waitTarget{}
	switch {
	case c.WaitCursorSet:
		t.kind, t.cursor = WaitCursorChange, c.WaitCursor
	case c.WaitRequestID != "":
		t.kind, t.requestID = WaitRequestOutcome, c.WaitRequestID
	case c.WaitRunOutcome:
		t.kind = WaitRunOutcome
		t.session = runWaitBinding(w.dir, w.store)
	default:
		t.kind, t.taskID, t.taskStatus = WaitTaskState, c.EventTask, c.WaitTaskStatus
		snap, err := w.store.Snapshot()
		if err != nil {
			return err
		}
		status, present := waitTaskStatusIn(snap.State, t.taskID)
		t.fromStatus, t.fromPresent = status, present
	}
	w.target = t
	return nil
}

// runWaitBinding names the controller session a run wait is bound to: the live
// owner when the wait begins, else the newest session the store recorded. A
// bound wait never reports a replacement controller's session as the outcome
// of the run it was asked about.
func runWaitBinding(dir string, store *Store) string {
	if info, err := readController(dir); err == nil && controllerAlive(info) {
		return info.Session
	}
	latest, err := store.latestSession()
	if err != nil {
		return ""
	}
	return latest
}

// observeUntil polls the target until it is satisfied, the bound expires, or
// the caller cancels. The first observation happens before any sleep, so a
// condition that already holds returns at once, and one more observation
// follows the deadline before a timeout is reported, so a transition that
// commits during the last sleep is never missed.
func (w *waitRun) observeUntil(ctx context.Context, bound time.Duration) (string, error) {
	deadline := w.started.Add(bound)
	for {
		satisfied, err := w.observe()
		if err != nil {
			return "", err
		}
		if satisfied {
			return waitSatisfied, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return w.observeFinal()
		}
		select {
		case <-ctx.Done():
			// The caller interrupted the wait. Nothing is cancelled: the
			// request, task, or controller underneath keeps its state, and the
			// returned cursor names where observation reached.
			return w.observeFinalAfterCancellation()
		case <-time.After(min(waitPollInterval, remaining)):
		}
	}
}

// observeFinal reads once more after the deadline, then answers.
func (w *waitRun) observeFinal() (string, error) {
	satisfied, err := w.observe()
	if err != nil {
		return "", err
	}
	if satisfied {
		return waitSatisfied, nil
	}
	return waitTimeout, nil
}

// observeFinalAfterCancellation reads once more so a satisfied condition that
// raced the interruption is reported as satisfied, never as a cancellation
// that hid the answer.
func (w *waitRun) observeFinalAfterCancellation() (string, error) {
	satisfied, err := w.observe()
	if err != nil {
		return "", err
	}
	if satisfied {
		return waitSatisfied, nil
	}
	return waitCancelled, nil
}

// observe reads the durable records once and evaluates the predicate against
// them. Every read is read-only; a wait never writes a byte of the run it
// watches.
func (w *waitRun) observe() (bool, error) {
	if w.probe != nil {
		w.probe()
	}
	// A controller may have started since the last observation; an immutable
	// attach cannot see its commits, so the attach is refreshed first.
	if err := w.refresh(); err != nil {
		return false, err
	}
	w.last = waitEvidence{at: wallClock()}
	switch w.target.kind {
	case WaitCursorChange:
		high, err := w.store.highWater()
		if err != nil {
			return false, err
		}
		w.last.highWater = high
		w.satisfied = high > w.target.cursor
		return w.satisfied, nil
	case WaitTaskState:
		snap, err := w.store.Snapshot()
		if err != nil {
			return false, err
		}
		w.last.highWater = snap.HighWater
		status, present := waitTaskStatusIn(snap.State, w.target.taskID)
		changed := present != w.target.fromPresent || status != w.target.fromStatus
		w.last.task = &waitTaskObserved{Present: present, Status: status, Changed: changed}
		if w.target.taskStatus != "" {
			w.satisfied = present && status == w.target.taskStatus
		} else {
			w.satisfied = changed
		}
		return w.satisfied, nil
	case WaitRequestOutcome:
		snap, err := w.store.Snapshot()
		if err != nil {
			return false, err
		}
		w.last.highWater = snap.HighWater
		rec := collectRequestEvidence(w.dir, snap.State, w.last.at).byID[w.target.requestID]
		w.last.request = rec
		w.satisfied = rec != nil && requestStateTerminal(rec.state)
		return w.satisfied, nil
	default:
		return w.observeRunOutcome()
	}
}

// observeRunOutcome evaluates the run predicate. The bound session's own
// record decides: a recorded end satisfies the wait with the outcome the store
// kept, and a session whose controller process ended without recording one
// satisfies it as unclean, because the run is over however it ended.
func (w *waitRun) observeRunOutcome() (bool, error) {
	w.controller.State, w.controller.Session = waitControllerState(w.dir)
	snap, err := w.store.Snapshot()
	if err != nil {
		return false, err
	}
	w.last.highWater = snap.HighWater
	row, err := waitSessionRow(w.store, w.target.session)
	if err != nil {
		return false, err
	}
	obs := &waitSessionObserved{ID: w.target.session}
	satisfied := false
	switch {
	case row != nil && row.outcome != "" && row.outcome != "running":
		// The store recorded the end, with the reason and exit code in the
		// shutdown event that committed beside it.
		obs.Outcome = row.outcome
		if exit := newestShutdownFor(w.store, w.target.session); exit != nil {
			obs.Reason = exit.Reason
			code := exit.ExitCode
			obs.ExitCode = &code
		}
		satisfied = true
	case row != nil && w.controller.State != "owning":
		// The session row still says running, but no live controller owns the
		// repository: the process ended without recording its exit.
		obs.Outcome, obs.Unclean = "unclean", true
		obs.Reason = "the controller process ended without recording an exit"
		satisfied = true
	case row != nil:
		obs.Outcome = "running"
	case w.controller.State == "owning" && w.controller.Session == w.target.session:
		// A live controller owns the repository under the bound session; its
		// session record has not committed yet.
		obs.Outcome = "running"
	default:
		// No session record names the bound session and no live controller
		// owns the repository: the run ended, and how it ended stayed unknown.
		obs.Outcome, obs.Unclean = "unknown", true
		obs.Reason = "no controller session is recorded and no controller owns the repository"
		satisfied = true
	}
	w.last.session = obs
	w.satisfied = satisfied
	return satisfied, nil
}

// waitTaskStatusIn reads one task's saved status. A state with no backlog, or
// a backlog without the task, reports absent: a task an import adds later is a
// status change from absent.
func waitTaskStatusIn(s *State, taskID string) (string, bool) {
	if s == nil {
		return "", false
	}
	for _, t := range s.Backlog.Tasks {
		if t.ID == taskID {
			return t.Status, true
		}
	}
	return "", false
}

// waitSessionRow reads one controller session row by identifier. A store from
// before the sessions table existed records none, which reads as no row.
func waitSessionRow(store *Store, id string) (*runSessionRow, error) {
	if id == "" {
		return nil, nil
	}
	var pid int
	var ended sql.NullString
	row := &runSessionRow{}
	err := store.db.QueryRow("SELECT id,pid,started,ended,outcome FROM sessions WHERE id=?", id).
		Scan(&row.id, &pid, &row.started, &ended, &row.outcome)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return nil, nil
		}
		return nil, err
	}
	if ended.Valid {
		row.ended = ended.String
	}
	return row, nil
}

// waitControllerState reads the ownership a wait observes: controller metadata
// plus process liveness, in the vocabulary the runs listing uses. A clean
// shutdown removes its own metadata, so a missing record is a stopped run,
// while a record that cannot be read or parsed stays unknown. Metadata a
// running controller has not written yet also reads as stopped, which a run
// wait treats as no live owner — the readiness a start reports is AX-022's
// contract, not this command's.
func waitControllerState(dir string) (state, session string) {
	if _, err := os.Stat(controllerPath(dir)); os.IsNotExist(err) {
		return "stopped", ""
	}
	info, err := readController(dir)
	switch {
	case err != nil:
		return "unknown", ""
	case controllerAlive(info):
		return "owning", info.Session
	default:
		return "stale", info.Session
	}
}

// document renders the wait's outcome. The same structure serves the
// satisfied, timeout, and cancelled forms: each names the condition, the
// evidence the last observation read, and the routes that continue
// observation.
func (w *waitRun) document(outcome string) *waitDocument {
	clamp := func(s string) string { return clampJSONString(s, waitStringMax) }
	doc := &waitDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   waitSchemaVersion,
		Kind:            waitKind,
		Outcome:         outcome,
		Condition: waitConditionView{
			Kind: w.target.kind, Cursor: w.target.cursor,
			TaskID: clamp(w.target.taskID), Status: w.target.taskStatus, From: w.target.fromStatus,
			RequestID: clamp(w.target.requestID), Session: clamp(w.target.session),
		},
	}
	if !w.target.fromPresent && w.target.kind == WaitTaskState {
		doc.Condition.From = "absent"
	}
	doc.Observed = waitObserved{
		At: w.last.at.UTC().Format(time.RFC3339Nano), HighWater: w.last.highWater,
		Note: clamp(w.timeoutNote()),
	}
	if w.last.task != nil {
		doc.Observed.Task = w.last.task
	}
	if w.last.request != nil {
		// The record view keeps its own published bounds, so the latest
		// receipt rides beside the outcome exactly as `looper requests`
		// serves it.
		doc.Observed.Request = requestRecordViewPtrOf(w.last.request)
	}
	if w.last.session != nil {
		session := *w.last.session
		session.ID = clamp(session.ID)
		session.Reason = clamp(session.Reason)
		doc.Observed.Session = &session
	}
	doc.Cursor = eventsCursorRef{EventID: w.last.highWater, Scope: "store", StoreID: w.store.identity, StoreEpoch: w.store.epoch}
	if w.store.identity != "" {
		doc.Store = &eventsStoreRef{ID: w.store.identity, Epoch: w.store.epoch}
	}
	if w.controller.State != "" {
		doc.Controller = &waitControllerRef{State: w.controller.State, Session: clamp(w.controller.Session)}
	}
	elapsed := w.last.at.Sub(w.started)
	if elapsed < 0 {
		elapsed = 0
	}
	doc.Wait = waitTiming{
		TimeoutSeconds: int(w.c.WaitFor / time.Second),
		ElapsedSeconds: int(elapsed.Truncate(time.Second) / time.Second),
	}
	doc.Detail = w.detailRoutes()
	return doc
}

// requestRecordViewPtrOf renders one collected record under the single-record
// fit the request queries publish.
func requestRecordViewPtrOf(rec *requestQueryRecord) *requestRecordView {
	view := requestRecordViewOf(rec, requestFit{stringMax: 256, recordsMax: 1, listMax: 16})
	return &view
}

// timeoutNote names what the last observation showed when the condition did
// not hold, including the one fact that explains a wait that cannot progress:
// no controller owns the repository, so only an operator or a start changes
// the records.
func (w *waitRun) timeoutNote() string {
	if w.satisfied {
		return ""
	}
	idle := ""
	if w.controller.State != "owning" {
		idle = " No controller owns the repository, so nothing changes the saved records while this wait runs."
	}
	switch w.target.kind {
	case WaitCursorChange:
		return fmt.Sprintf("no event was committed past %d.%s", w.target.cursor, idle)
	case WaitTaskState:
		if w.last.task == nil {
			return "the saved state could not be read." + idle
		}
		if !w.last.task.Present {
			return fmt.Sprintf("task %s is not in the saved backlog.%s", w.target.taskID, idle)
		}
		if w.target.taskStatus != "" {
			return fmt.Sprintf("task %s is still %s.%s", w.target.taskID, w.last.task.Status, idle)
		}
		return fmt.Sprintf("task %s is still %s, as it was when the wait began.%s", w.target.taskID, w.last.task.Status, idle)
	case WaitRequestOutcome:
		if w.last.request == nil {
			return fmt.Sprintf("no retained receipt or pending envelope names request %s yet.%s", w.target.requestID, idle)
		}
		waiting := ""
		if w.last.request.waitReason != "" {
			waiting = " It waits on " + w.last.request.waitReason + "."
		}
		return fmt.Sprintf("request %s is still %s.%s%s", w.target.requestID, w.last.request.state, waiting, idle)
	default:
		if w.last.session != nil && w.last.session.Outcome == "running" {
			return fmt.Sprintf("controller session %s still holds the run.", w.target.session)
		}
		return "the bound controller session has not ended."
	}
}

// detailRoutes names the exact next calls: the same wait from the position the
// observation reached, the event page that continues from the cursor, the
// compact status, and the followed stream a caller that can hold one uses
// instead of a finite wait. A route embeds its identifier only when the
// identifier fits the bound, so an oversized or escape-heavy caller-supplied
// identifier cannot grow the document without limit; the ungated form names
// the bounded queries instead.
func (w *waitRun) detailRoutes() waitDetailRoutes {
	bound := formatWaitSeconds(w.c.WaitFor)
	follow := "looper events --contract v2 --follow --json"
	status := "looper status --contract v2 --json"
	taskRoute := func(id string) bool { return id != "" && jsonEscapedLen(id) <= waitRouteID }
	switch w.target.kind {
	case WaitCursorChange:
		return waitDetailRoutes{
			Command: fmt.Sprintf("looper wait --contract v2 --cursor %d --timeout %s", w.last.highWater, bound),
			Events:  fmt.Sprintf("looper events --contract v2 --after %d", w.last.highWater),
			Status:  status,
			Follow:  follow,
		}
	case WaitTaskState:
		target := ""
		if w.target.taskStatus != "" {
			target = " --status " + w.target.taskStatus
		}
		if !taskRoute(w.target.taskID) {
			return waitDetailRoutes{Command: status, Events: status, Status: status, Follow: follow}
		}
		return waitDetailRoutes{
			Command: fmt.Sprintf("looper wait --contract v2 --task %s%s --timeout %s", w.target.taskID, target, bound),
			Events:  "looper events --contract v2 --task " + w.target.taskID,
			Status:  status,
			Follow:  follow + " --task " + w.target.taskID,
		}
	case WaitRequestOutcome:
		if !taskRoute(w.target.requestID) {
			return waitDetailRoutes{Command: status, Events: "looper requests --contract v2 --json", Status: status, Follow: follow}
		}
		return waitDetailRoutes{
			Command: "looper wait --contract v2 --request " + w.target.requestID + " --timeout " + bound,
			Events:  "looper requests " + w.target.requestID + " --contract v2",
			Status:  status,
			Follow:  follow,
		}
	default:
		return waitDetailRoutes{
			Command: "looper wait --contract v2 --run --timeout " + bound,
			Events:  fmt.Sprintf("looper events --contract v2 --after %d", w.last.highWater),
			Status:  status,
			Follow:  follow,
		}
	}
}

// renderWaitDocument renders the same document as text under both contract
// versions: the condition and its outcome first, then the evidence, the
// cursor, and the routes that continue observation.
func renderWaitDocument(doc *waitDocument, out io.Writer) {
	line := func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) }
	condition := waitConditionText(doc.Condition)
	line("Wait: %s | %s after %s of %s", condition, doc.Outcome,
		statusDuration(doc.Wait.ElapsedSeconds), statusDuration(doc.Wait.TimeoutSeconds))
	if doc.Observed.Note != "" {
		line("  %s", doc.Observed.Note)
	}
	if doc.Observed.Task != nil {
		if doc.Observed.Task.Present {
			line("  task %s: %s", doc.Condition.TaskID, doc.Observed.Task.Status)
		} else {
			line("  task %s: not in the saved backlog", doc.Condition.TaskID)
		}
	}
	if doc.Observed.Request != nil {
		line("  request %s: %s (type %s)", doc.Observed.Request.ID, doc.Observed.Request.State, doc.Observed.Request.Type)
		if doc.Observed.Request.WaitingOn != nil {
			line("    waits on %s: %s", doc.Observed.Request.WaitingOn.Reason, doc.Observed.Request.WaitingOn.Detail)
		}
	}
	if doc.Observed.Session != nil {
		end := doc.Observed.Session.Outcome
		if doc.Observed.Session.Reason != "" {
			end += " (" + doc.Observed.Session.Reason + ")"
		}
		if doc.Observed.Session.ExitCode != nil {
			end += fmt.Sprintf(", exit %d", *doc.Observed.Session.ExitCode)
		}
		if doc.Observed.Session.ID != "" {
			line("  session %s: %s", doc.Observed.Session.ID, end)
		} else {
			line("  run: %s (no controller session is recorded)", end)
		}
	}
	if doc.Controller != nil {
		owner := doc.Controller.State
		if doc.Controller.Session != "" {
			owner += " (session " + doc.Controller.Session + ")"
		}
		line("  controller: %s", owner)
	}
	line("  cursor: %d (store scope)", doc.Cursor.EventID)
	line("  continue: %s", doc.Detail.Command)
	line("  detail: %s | %s", doc.Detail.Events, doc.Detail.Status)
	line("  streaming: %s", doc.Detail.Follow)
}

// waitConditionText names the condition in words for the plain rendering.
func waitConditionText(c waitConditionView) string {
	switch c.Kind {
	case WaitCursorChange:
		return fmt.Sprintf("cursor change past event %d", c.Cursor)
	case WaitTaskState:
		if c.Status != "" {
			return fmt.Sprintf("task %s reaches %s", c.TaskID, c.Status)
		}
		return fmt.Sprintf("task %s changes from %s", c.TaskID, orUnknown(c.From))
	case WaitRequestOutcome:
		return fmt.Sprintf("request %s reaches a terminal state", c.RequestID)
	default:
		return fmt.Sprintf("run outcome for session %s", orUnknown(c.Session))
	}
}

// formatWaitSeconds renders one wait bound the way the receipt's wait object
// and the detail routes name it: a whole number of seconds.
func formatWaitSeconds(d time.Duration) string {
	return strconv.Itoa(int(d/time.Second)) + "s"
}

// waitRequestCommand names the finite wait one returned request can be
// waited on with, and the bound the receipt suggests for it.
func waitRequestCommand(id string, bound time.Duration) string {
	if !statusRouteIDOk(id) || bound < time.Second {
		return "looper requests --contract v2 --json"
	}
	return "looper wait --contract v2 --request " + id + " --timeout " + formatWaitSeconds(bound)
}
