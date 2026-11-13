package looper

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// Engine has one owner goroutine. Workers return values; they never mutate state.
type Engine struct {
	Config              Config
	Repo                *Repository
	Store               *Store
	State               *State
	Agent               Agent
	Dir, Logs           string
	Output              io.Writer
	Render              *Presenter
	controller          *ControllerInfo
	acks                map[string]SettingsAck
	viewNotify          chan ui.ViewSnapshot
	eventCursor         int64
	viewDirty           bool
	workerReaders       map[string]*plannerActivityReader
	lastObservation     time.Time
	checkSlots          chan struct{}
	wg                  sync.WaitGroup
	capacity, successes int
	err                 error

	activity, activityDetail, activityLog string
	activityUpdated                       string
	activityAgent                         string

	// inputPending marks an idle wait for a corrected task file. The last
	// accepted state and the user's file stay untouched while it is set.
	inputPending bool
	inputProblem string

	// suppressRequestWake keeps one pass's idle wait from waking on queued
	// request envelopes the boundary could not finish: a claim error, or a
	// flooded queue whose shedding continues at the next boundaries. Either
	// would otherwise return the idle wait at once and spin the loop without
	// sleep. The next loop boundary clears it and retries.
	suppressRequestWake bool

	// uncleanPreviousNote carries the previous session's unclean end until
	// startup has succeeded, so its recovery note never streams before a
	// startup guard's error document (AX-022).
	uncleanPreviousNote *previousSession

	// sweptRequests holds the envelopes the startup sweep removed before the
	// saved state existed to record them in. The first state save after
	// initialization flushes them into the receipt store, so even a swept
	// request keeps a queryable outcome (AX-009).
	sweptRequests []requestRefusal

	// journaledFailure names the run-ending error whose bounded summary was
	// already journaled with its task identity where it happened, so the run
	// boundary does not repeat it session-wide (AX-021).
	journaledFailure error

	// assessDeadline overrides the fixed assessment deadline for tests of the
	// full pipeline; zero keeps the production deadline. It is set before the
	// run starts and never mutated while workers are live.
	assessDeadline time.Duration

	// importHeartbeat overrides the fixed import-analysis heartbeat interval
	// for tests of the full pipeline; zero keeps the production interval. It
	// is set before the run starts and never mutated while agents are live.
	importHeartbeat time.Duration

	// consultHeartbeat overrides the fixed diagnosis consultation heartbeat
	// interval for tests of the full pipeline; zero keeps the production
	// interval. It is set before the run starts and never mutated while
	// agents are live.
	consultHeartbeat time.Duration
}

// operation is the immutable execution configuration captured before each
// worker, check, planning, or review operation starts. Retries within the
// operation reuse the captured values; workers never read mutable engine
// configuration after launch.
type operation struct {
	config Config
	agent  Agent
}

// capture freezes the effective configuration and the agent runner built
// from it. It runs on the owner goroutine before dispatching a goroutine.
func (e *Engine) capture() operation {
	c := e.Config
	if e.State != nil && e.State.RecoveredCommands != nil {
		r := e.State.RecoveredCommands
		if c.Setup == r.BeforeSetup {
			c.Setup = r.Setup
		}
		if c.Verify == r.BeforeVerify {
			c.Verify = r.Verify
		}
	}
	return operation{config: c, agent: Agent{c}}
}

func projectPaths(c Config) (string, string) {
	h := sha1.Sum([]byte(c.Root))
	slug := strings.Trim(regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(filepath.Base(c.Root), "_"), "_")
	logs := filepath.Join(c.BaseDir, slug+"-"+hex.EncodeToString(h[:4]))
	return logs, filepath.Join(logs, "runtime")
}
func identifier(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}
func clone[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }

func NewEngine(c Config, out io.Writer) (*Engine, *os.File, error) {
	logs, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	r, err := OpenRepository(c, dir)
	if err != nil {
		return nil, nil, err
	}
	lock, err := lockRepository(r.Common)
	if err != nil {
		return nil, nil, err
	}
	s, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		lock.Close()
		return nil, nil, err
	}
	// Preserve drain requests received before or during startup. A completed
	// drain removes its request at the final safe boundary in Run.
	entries, _ := os.ReadDir(filepath.Join(dir, "worktrees"))
	for _, entry := range entries {
		for _, prefix := range []string{"plan-", "review-", "check-", "bootstrap-"} {
			if strings.HasPrefix(entry.Name(), prefix) {
				r.Remove(filepath.Join(dir, "worktrees", entry.Name()))
				break
			}
		}
	}
	// Controller identity publishes under the repository lock. Settings files
	// from earlier sessions clear here; plan-import requests survive. Every
	// removed envelope is kept for the receipt the first state save records.
	swept := clearSessionFiles(dir)
	mode := ControllerModeForeground
	if c.Detached {
		mode = ControllerModeDetached
	}
	info, err := publishController(dir, mode, c.ReviewAgent)
	if err != nil {
		s.Close()
		lock.Close()
		return nil, nil, err
	}
	if err = s.beginSession(info); err != nil {
		removeController(dir, info.Session)
		s.Close()
		lock.Close()
		return nil, nil, err
	}
	engine := &Engine{Config: c, Repo: r, Store: s, Agent: Agent{c}, Dir: dir, Logs: logs, Output: out, controller: info, acks: map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1), checkSlots: make(chan struct{}, c.Checks), capacity: c.Jobs, sweptRequests: swept}
	// The presentation cursor starts at the retained store's committed
	// high-water mark, so the first view a restart publishes points observers
	// at a valid continuation position instead of the beginning of history.
	if high, err := s.highWater(); err == nil {
		engine.eventCursor = high
	}
	if !c.JSON {
		width := func() int {
			if f, ok := out.(*os.File); ok {
				return terminalWidth(f)
			}
			return 0
		}
		engine.Render = NewPresenter(DetectMode(os.Stdin, out, c.Plain), out, width)
	}
	return engine, lock, nil
}

// eventIdentity is the structured identity one lifecycle event carries
// (AX-007): the controller session that wrote it, the task and attempt it
// concerns, the attempt phase at the moment it was written, and the
// controller request that caused it. A field is written only when it
// applies, so a historical record without these fields stays readable and
// explicitly incomplete. Message text stays for people; nothing parses it
// for identity.
type eventIdentity struct {
	Session   string `json:"session,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Phase     string `json:"phase,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// merge returns data with this identity's fields set, so a payload can carry
// identity beside its own content. A wholly empty identity keeps a nil
// payload nil, so writers without a session record no empty object.
func (i eventIdentity) merge(data map[string]any) map[string]any {
	if data == nil && i == (eventIdentity{}) {
		return nil
	}
	if data == nil {
		data = map[string]any{}
	}
	if i.Session != "" {
		data["session"] = i.Session
	}
	if i.TaskID != "" {
		data["task_id"] = i.TaskID
	}
	if i.AttemptID != "" {
		data["attempt_id"] = i.AttemptID
	}
	if i.Phase != "" {
		data["phase"] = i.Phase
	}
	if i.RequestID != "" {
		data["request_id"] = i.RequestID
	}
	return data
}

// controllerSessionID returns the session of the controller writing events,
// or "" when none is published.
func (e *Engine) controllerSessionID() string {
	if e.controller == nil {
		return ""
	}
	return e.controller.Session
}

// identity is the session identity every controller-owned lifecycle event
// carries.
func (e *Engine) identity() eventIdentity {
	return eventIdentity{Session: e.controllerSessionID()}
}

// attemptIdentity is the identity of one attempt's lifecycle event: the
// controller session, the task, the attempt, and the phase recorded when the
// event was written.
func (e *Engine) attemptIdentity(a *Attempt) eventIdentity {
	return eventIdentity{Session: e.controllerSessionID(), TaskID: a.Task.ID, AttemptID: a.ID, Phase: a.Phase}
}

func (e *Engine) event(kind, message string) {
	e.eventData(kind, message, e.identity().merge(nil))
}

// taskEvent writes one attempt lifecycle event with its structured identity.
func (e *Engine) taskEvent(kind, message string, a *Attempt) {
	e.eventData(kind, message, e.attemptIdentity(a).merge(nil))
}

// taskIDEvent writes a task lifecycle event whose attempt is not at hand,
// such as a recovery note that names only the task.
func (e *Engine) taskIDEvent(kind, message, taskID string) {
	id := e.identity()
	id.TaskID = taskID
	e.eventData(kind, message, id.merge(nil))
}

// requestEvent writes one controller request outcome with the identity that
// correlates the journal note with its receipt.
func (e *Engine) requestEvent(kind, message, requestID string) {
	id := e.identity()
	id.RequestID = requestID
	e.eventData(kind, message, id.merge(nil))
}

func (e *Engine) eventData(kind, message string, data any) {
	if e.err != nil {
		return
	}
	id, err := e.Store.Event(kind, message, data)
	if err != nil {
		e.err = err
		return
	}
	e.eventCursor = id
	e.viewDirty = true
	if e.Config.JSON {
		var payload json.RawMessage
		if data != nil {
			payload, _ = json.Marshal(data)
		}
		b, _ := json.Marshal(Event{Time: time.Now().UTC().Format(time.RFC3339), Kind: kind, Message: message, Data: payload})
		fmt.Fprintln(e.Output, string(b))
	} else if e.Render != nil {
		e.Render.Event(kind, message)
	} else {
		fmt.Fprintln(e.Output, message)
	}
}
func (e *Engine) save() error {
	if e.err != nil {
		return e.err
	}
	return e.Store.Save(e.State)
}

// Export is journaled in SQLite first. Recovery recognizes both the old and new
// file contents. An external edit is never silently replaced by a status export.
func (e *Engine) export() error {
	if err := e.State.Backlog.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(e.State.Backlog, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	next := digest(data)
	if !e.State.ExportPending {
		// The journal that follows overwrites the task file, so record the
		// accepted bytes first: a replay this edit blocks names this copy as
		// the restore source (AX-009).
		e.retainAcceptedTaskFile()
		e.State.ExportBefore = e.State.ExportHash
		e.State.ExportPending = true
		if err = e.save(); err != nil {
			return err
		}
	}
	current, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	observed := digest(current)
	if observed != e.State.ExportBefore && observed != next {
		edit := e.classifyBacklog(current)
		if !edit.Equivalent {
			return e.retain(pendingExportGuardError(e, e.State.ExportBefore, observed, edit))
		}
		// Equivalent formatting (AX-018): the external edit changed no
		// content, so the export below replaces it with the canonical
		// projection of the same backlog, exactly as it replaces the
		// previously accepted bytes.
	}
	if observed != next {
		if err = atomicWrite(e.Config.Todo, data, 0600); err != nil {
			return err
		}
	}
	e.State.ExportHash = next
	e.State.ExportSemantics = e.State.Backlog.semanticFingerprint()
	e.State.ExportBefore = ""
	e.State.ExportPending = false
	return e.save()
}

// acceptedExportPath is the runtime copy of the task-file bytes the saved
// state last accepted. A guard a pending export or a retained run raises
// names this copy as the restore source, so the operator restores the exact
// accepted bytes instead of re-deriving them (AX-009).
func acceptedExportPath(dir string) string { return filepath.Join(dir, "exports", "accepted.json") }

// retainAcceptedTaskFile records the accepted task-file bytes. The copy is a
// byte copy, never a hard link: an editor that rewrites the task file in
// place would mutate the shared inode a link points to, and the retained
// "accepted" bytes would silently become the edit the guard must refuse. A
// task file that no longer matches the accepted digest keeps the previous
// copy untouched.
func (e *Engine) retainAcceptedTaskFile() {
	if e.Dir == "" {
		// A protocol engine without a runtime directory records nothing.
		return
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil || e.State == nil || digest(data) != e.State.ExportHash {
		return
	}
	_ = atomicWrite(acceptedExportPath(e.Dir), data, 0600)
}

// acceptedExportRestore names the retained accepted-bytes copy when it holds
// exactly the digest a guard demands. Only a byte-identical restore replays:
// a semantically equal rewrite still differs from the recorded digest.
func acceptedExportRestore(dir, wantDigest string) (path string, ok bool) {
	if wantDigest == "" {
		return "", false
	}
	data, err := os.ReadFile(acceptedExportPath(dir))
	if err != nil || digest(data) != wantDigest {
		return "", false
	}
	return acceptedExportPath(dir), true
}

// pendingExportGuardError builds the refusal a blocked export journal
// raises. The message names the expected digest and the recovery step: the
// retained accepted bytes and the restore that replays the journal.
func pendingExportGuardError(e *Engine, wantDigest, observed string, edit backlogEdit) *retentionError {
	rerr := &retentionError{
		code:     "backlog_contract_changed",
		message:  fmt.Sprintf("the backlog changed outside Looper; work is retained in %s", e.Dir),
		expected: map[string]any{"task_file_digest": wantDigest},
		observed: map[string]any{"task_file_digest": observed},
		backlog:  editDifference(edit),
	}
	rerr.addRestoreAction(e, wantDigest)
	return rerr
}

// addRestoreAction names the accepted-byte restore for the digest this guard
// demands, when the retained copy holds exactly those bytes. The recovery
// step rides in the structured error's actions — the legacy one-line text
// stays frozen — because the copy is refreshed at every accepted export and
// a stale copy means no restore source exists; pretending otherwise would
// mislead. Guards without a usable copy still name the digest their restore
// needs.
func (r *retentionError) addRestoreAction(e *Engine, wantDigest string) {
	path, ok := acceptedExportRestore(e.Dir, wantDigest)
	if !ok {
		if wantDigest != "" {
			r.actions = append(r.actions, ErrorAction{
				Description: fmt.Sprintf("Restore the task file to Looper's last exported bytes (digest %s) so the pending export replays.", wantDigest),
			})
		}
		return
	}
	r.actions = append(r.actions, ErrorAction{
		Description: "Restore the task file to Looper's retained accepted bytes so the pending export replays.",
		Command:     fmt.Sprintf("cp %s %s", path, e.Config.Todo),
	})
}

// original moved to branch_recovery.go: the publication guard and the
// startup recovery share one acceptance decision (T046).
func (e *Engine) contract(a *Attempt) error {
	t := e.State.Backlog.Task(a.Task.ID)
	if t == nil || t.Contract() != a.Task.Contract() {
		expected := "task absent from the backlog"
		if t != nil {
			expected = t.Contract()
		}
		return e.retain(&retentionError{
			code:     "task_contract_changed",
			message:  fmt.Sprintf("task %s changed while its work was retained", a.Task.ID),
			expected: map[string]any{"task_contract": expected},
			observed: map[string]any{"task_contract": a.Task.Contract()},
		})
	}
	return nil
}
func retained(s *State) bool {
	if s.Publication != nil {
		return true
	}
	for _, a := range s.Attempts {
		switch a.Phase {
		case "done", "blocked", "cancelled":
		default:
			return true
		}
	}
	return false
}
func (e *Engine) initialize(ctx context.Context) error {
	// Startup plan input is rejected before bootstrap, recovery, retained
	// publication, or any dispatch can start: an invalid invocation must
	// not begin work. Capture revalidates and stays authoritative.
	if err := validatePlanFiles(e.Config.Plans); err != nil {
		e.event("import-error", err.Error())
		return err
	}
	if err := e.initializeState(ctx); err != nil {
		// A failed startup still records what the sweep removed, so the
		// swept envelopes keep their queryable outcome whatever ended the
		// run.
		e.recordRefusedRequests(e.sweptRequests)
		e.sweptRequests = nil
		return err
	}
	// A single task with an invalid files entry must not end the controller
	// before it starts: the quarantine holds that task blocked and the rest
	// of the backlog runs.
	if err := e.quarantineInvalidFiles(); err != nil {
		return err
	}
	if err := e.recoverPlanImports(); err != nil {
		return err
	}
	e.startupImportPlans()
	if e.err != nil {
		return e.err
	}
	// The startup sweep ran before the saved state existed; its removals
	// become rejected receipts now that the receipt store is loaded.
	e.recordRefusedRequests(e.sweptRequests)
	e.sweptRequests = nil
	return nil
}

// seedStartupState makes the saved run publicly queryable before a detached
// controller reports readiness (AX-022): a successful start may not precede
// the state a status query reads. A run that already saved state keeps it. A
// fresh run saves its initial baseline now, so even a bootstrap that runs for
// minutes leaves a queryable run behind the readiness pipe. The seed carries
// StartupSeed and initializeState treats it as absent, so startup still
// bootstraps, imports legacy work, and seeds the real baseline exactly as
// before; the fresh seed adopts the startup snapshot when nothing moved.
func (e *Engine) seedStartupState() error {
	s, err := e.Store.Load()
	if err != nil || s != nil {
		return err
	}
	var b Backlog
	exportHash := ""
	data, readErr := os.ReadFile(e.Config.Todo)
	switch {
	case readErr == nil:
		parsed, parseErr := parseBacklogStrict(data)
		if parseErr != nil {
			return fmt.Errorf("invalid backlog: %w", parseErr)
		}
		if err = parsed.Validate(); err != nil {
			return err
		}
		b, exportHash = parsed, digest(data)
	case errors.Is(readErr, os.ErrNotExist):
		// No task file yet: bootstrap creates it during initialization, and
		// the seed reports a starting run with an empty backlog meanwhile.
		b = Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}
	default:
		return readErr
	}
	head, branch := e.Repo.Head()
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Snapshot project for Looper")
	if err != nil {
		return err
	}
	ref := "refs/looper/" + identifier("run")
	if err = e.Repo.Ref(ref, seed); err != nil {
		return err
	}
	e.State = &State{Version: 2, Root: e.Config.Root, Todo: e.Config.Todo, Head: head, Branch: branch, Ref: ref, Tip: seed, Published: tree, Backlog: b, ExportHash: exportHash, ExportSemantics: b.semanticFingerprint(), Attempts: []*Attempt{}, StartupSeed: true}
	e.retainAcceptedTaskFile()
	return e.save()
}

func (e *Engine) initializeState(ctx context.Context) error {
	// Saved identity is checked before anything can write: bootstrapping a
	// different task-file selection would run an agent and create a second
	// backlog before the mismatch is detected.
	s, err := e.Store.Load()
	if err != nil {
		return err
	}
	// A startup seed (AX-022) exists only to make the run queryable while
	// initialization runs; it is not a baseline to resume. Treat it as absent
	// and keep it for the fresh path below, which adopts its snapshot when the
	// project did not move under it.
	var startupSeed *State
	if s != nil && s.StartupSeed {
		startupSeed, s = s, nil
	}
	if s != nil && (s.Version != 2 || s.Root != e.Config.Root || s.Todo != e.Config.Todo) {
		if s.Todo != e.Config.Todo {
			return fmt.Errorf("saved state belongs to task file %q, but %q is selected; rerun with the saved task file", s.Todo, e.Config.Todo)
		}
		return fmt.Errorf("saved state belongs to another project or version")
	}
	var bootstrapPlans []capturedPlan
	if _, statErr := os.Stat(e.Config.Todo); errors.Is(statErr, os.ErrNotExist) {
		if s != nil {
			// Bootstrap must not rebuild the backlog of a saved run: the
			// operator restores the saved task file instead.
			return fmt.Errorf("the saved task file %s is missing; restore it before resuming the retained run", e.Config.Todo)
		}
		// A fresh project reads project context and all explicitly supplied
		// plans together, with a recoverable staging record.
		if len(e.Config.Plans) > 0 {
			if bootstrapPlans, err = e.bootstrapStartupPlans(identifier("import")); err != nil {
				return err
			}
		}
		if err = e.bootstrap(ctx, bootstrapPlans); err != nil {
			return err
		}
	}
	if _, err = os.Stat(e.Config.SchemaPath()); errors.Is(err, os.ErrNotExist) {
		if err = atomicWrite(e.Config.SchemaPath(), []byte(backlogSchema), 0644); err != nil {
			return err
		}
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	// The startup parse is strict: duplicate keys and ambiguous documents
	// are invalid input, never silently normalized (AX-018).
	b, err := parseBacklogStrict(data)
	if err != nil {
		return fmt.Errorf("invalid backlog: %w", err)
	}
	if err = b.Validate(); err != nil {
		return err
	}
	if s == nil {
		s, err = e.importLegacy(b, data)
		if err != nil {
			return err
		}
	}
	if s != nil {
		e.State = s
		e.retainAcceptedTaskFile()
		if s.ExportPending {
			if err = e.export(); err != nil {
				return err
			}
			data, err = os.ReadFile(e.Config.Todo)
			if err != nil {
				return err
			}
			b = clone(s.Backlog)
		}
		if retained(s) {
			if observed := digest(data); observed != s.ExportHash {
				edit := e.classifyBacklog(data)
				if edit.Equivalent {
					// Equivalent formatting (AX-018): the edit changed no
					// content. Startup adopts the observed bytes as the accepted
					// export instead of stranding the retained work; the file
					// keeps the operator's formatting and the event log records
					// the digests both ways.
					if err = e.adoptBacklogBytes(edit); err != nil {
						return err
					}
				} else if replaySafeRetainedWork(s) {
					// A session cancelled mid-hold leaves the external edit
					// beside the old export hash (T058). Startup imports it
					// through the same held refresh the live boundary runs,
					// because every retained result replays against the moved
					// baseline: the recovery below re-prepares and re-verifies
					// the result commits and replays the receipts. The edit
					// must also leave every retained task's contract intact:
					// the recovery below compares the retained attempts against
					// the imported backlog, so a contract-changing edit would
					// strand that work at the contract guard instead of
					// replaying it. Startup refuses it with the restore demand,
					// exactly as the checked application and the preview do
					// (T119).
					if breakers := retainedContractBreakers(s, data); len(breakers) > 0 {
						rerr := &retentionError{
							code:     "backlog_contract_changed",
							message:  fmt.Sprintf("the edited task file changes the contract of retained task(s) %s while their work is retained, so the edit cannot import; restore its saved contract before resuming", boundedIDList(breakers, 8)),
							expected: map[string]any{"task_file_digest": s.ExportHash},
							observed: map[string]any{"task_file_digest": observed},
							backlog:  editDifference(edit),
						}
						rerr.addRestoreAction(e, s.ExportHash)
						return e.retain(rerr)
					}
					if err = e.importRetainedEdit(observed); err != nil {
						return err
					}
				} else {
					rerr := &retentionError{
						code:     "backlog_contract_changed",
						message:  "the backlog changed while work was retained; restore its saved contract before resuming",
						expected: map[string]any{"task_file_digest": s.ExportHash},
						observed: map[string]any{"task_file_digest": observed},
						backlog:  editDifference(edit),
					}
					rerr.addRestoreAction(e, s.ExportHash)
					return e.retain(rerr)
				}
			}
			if _, err = e.recoverCommittedHead(); err != nil {
				return err
			}
			if s.Publication != nil {
				// A head that moved between the recovery above and this
				// publication folds into the baseline and requeues it; the
				// attempts loop below then recovers the requeued result like
				// any other retained one (T046).
				if err = e.publish(); err != nil && !errors.Is(err, errBaselineMoved) {
					return err
				}
			}
			tree, err := e.Repo.SnapshotFrom(e.Config.Root, s.Published)
			if err != nil {
				return err
			}
			if tree != s.Published {
				return e.retain(&retentionError{
					code:     "project_tree_changed",
					message:  fmt.Sprintf("project files changed since the interrupted run; work is retained in %s", e.Dir),
					expected: map[string]any{"project_tree": s.Published},
					observed: map[string]any{"project_tree": tree},
				}, e.changedPaths("project_tree_changes", s.Published, tree))
			}
			for _, a := range s.Attempts {
				if a.Phase == "done" || a.Phase == "blocked" || a.Phase == "cancelled" {
					continue
				}
				if err = e.contract(a); err != nil {
					return err
				}
				if a.Phase == "running" || a.Phase == "resume" {
					if receipt, ok := retainedReceipt(a); ok {
						a.Summary = &receipt.Summary
						a.ResultCommit = receipt.Commit
						if receipt.Summary.Status == "blocked" {
							if err = e.applyResult(a); err != nil {
								return err
							}
							continue
						}
					}
				}
				if a.Phase == "integrated" {
					if err = e.applyResult(a); err != nil {
						return err
					}
					continue
				}
				if a.ResultCommit != "" {
					a.Phase = "ready"
					e.observeScope(a)
				} else {
					a.Phase = "resume"
				}
			}
			// Commit the resumed phases before the journal note (AX-003): a
			// reader that pairs state with the cursor in one snapshot then
			// sees the resume event with the state that matches it.
			if err = e.save(); err != nil {
				return err
			}
			resumeData, _ := e.previousExitData().(map[string]any)
			e.eventData("resume", e.resumeMessage(), e.identity().merge(resumeData))
			return nil
		}
		// With no in-flight work the editable plan and current working tree may
		// start a new baseline. Archived state remains available for inspection.
		if err = writeJSON(filepath.Join(e.Dir, "history", identifier("state")+".json"), s); err != nil {
			return err
		}
		return e.refreshIdle()
	}
	head, branch := e.Repo.Head()
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	if startupSeed != nil && startupSeed.Root == e.Config.Root && startupSeed.Todo == e.Config.Todo &&
		startupSeed.Head == head && startupSeed.Branch == branch && startupSeed.Published == tree &&
		startupSeed.ExportHash == digest(data) {
		// The startup seed already snapshotted this exact baseline (AX-022);
		// adopt it instead of committing a second copy of the same tree.
		startupSeed.StartupSeed = false
		startupSeed.Backlog = b
		e.State = startupSeed
		e.retainAcceptedTaskFile()
		e.adoptStagedImports()
		return e.save()
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Snapshot project for Looper")
	if err != nil {
		return err
	}
	// The startup seed's ref already names this run (AX-022); advance it to
	// the real baseline so a bootstrapped project keeps one run ref instead
	// of leaving the seed's ref orphaned beside it.
	ref := "refs/looper/" + identifier("run")
	if startupSeed != nil && startupSeed.Ref != "" {
		ref = startupSeed.Ref
	}
	if err = e.Repo.Ref(ref, seed); err != nil {
		return err
	}
	e.State = &State{Version: 2, Root: e.Config.Root, Todo: e.Config.Todo, Head: head, Branch: branch, Ref: ref, Tip: seed, Published: tree, Backlog: b, ExportHash: digest(data), ExportSemantics: b.semanticFingerprint(), Attempts: []*Attempt{}}
	e.retainAcceptedTaskFile()
	// Fresh state adopts staged bootstrap imports so restarts never import
	// them twice.
	e.adoptStagedImports()
	if err := e.save(); err != nil {
		return err
	}
	return nil
}

// retainedReceipt reads the result receipt one attempt left behind and reports
// whether startup recovery adopts it. The predicate is exactly the one the
// retained-attempt loop applies, so the import gate and the replay itself can
// never drift apart.
func retainedReceipt(a *Attempt) (resultReceipt, bool) {
	var receipt resultReceipt
	if readJSON(filepath.Join(a.Directory, "receipt.json"), &receipt) != nil {
		return receipt, false
	}
	if receipt.Base != a.Base || receipt.Contract != a.Task.Contract() || receipt.Repairs != a.Repairs {
		return receipt, false
	}
	if err := receipt.Summary.Validate(a.Task.ID); err != nil {
		return receipt, false
	}
	return receipt, true
}

// replaySafeRetainedWork reports whether every retained attempt holds a result
// that replays after the held import (T058): a result commit the next run
// re-prepares and re-verifies, or a receipt whose summary and commit reapply
// like a fresh report. An attempt with neither would rerun its worker against
// the imported baseline, so its edit cannot import and the restore demand
// stands.
func replaySafeRetainedWork(s *State) bool {
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
		return false
	}
	return true
}

// importRetainedEdit runs the held import at startup for an external task-file
// edit a cancelled hold left beside the old export hash (T058). The retained
// results replay against the imported baseline exactly as they do at the live
// held boundary, and the retained recovery below then continues against the
// moved baseline. A publication saved between its verification and the
// cancelled boundary requeues like the T046 fold, because the import moved the
// base its verification used.
func (e *Engine) importRetainedEdit(observed string) error {
	previous := e.State.ExportHash
	if err := e.refreshHeld(); err != nil {
		return err
	}
	if p := e.State.Publication; p != nil && p.Base != e.State.Tip {
		a := e.attempt(p.AttemptID)
		if a == nil {
			return fmt.Errorf("publication references an unknown attempt")
		}
		// The verified result re-prepares against the imported baseline and
		// verifies again; its saved verification stays recorded beside the
		// attempt for inspection.
		a.Phase = "ready"
		e.State.Publication = nil
		if err := e.save(); err != nil {
			return err
		}
	}
	ids, total := retainedTaskIDs(e.State)
	e.eventData("recovery", fmt.Sprintf("Imported the task-file edit left by a cancelled hold; %d retained result(s) replay against the imported baseline.", total), e.identity().merge(map[string]any{
		"previous_digest": previous,
		"observed_digest": observed,
		"retained_tasks":  ids,
	}))
	return e.err
}

func (e *Engine) bootstrap(ctx context.Context, plans []capturedPlan) error {
	return e.bootstrapResponse(ctx, plans, "")
}

func (e *Engine) bootstrapResponse(ctx context.Context, plans []capturedPlan, correction string) error {
	head, _ := e.Repo.Head()
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Inspect project for Looper")
	if err != nil {
		return err
	}
	name := identifier("bootstrap")
	empty := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}
	work, err := e.Repo.Workspace(name, seed, empty)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	op := e.capture()
	e.event("bootstrap", "Reading project requirements and creating the first tasks...")
	prompt := `Bootstrap a development backlog for Looper. Read repository instructions and project specifications.
Return one JSON object: {"schema_version":1,"goal":"the existing project's intended outcome","source_files":["relative requirements files"],"tasks":[{"id":"T001","title":"outcome","priority":1,"status":"todo","details":"acceptance criteria","depends_on":[]}]}.
Only plan work grounded in the existing requirements. Include setup and executable verification where missing.
List only project requirement files in source_files. Never list Looper-private context or backlog snapshots; supplied plan documents are retained separately.
Use dependencies. Separate external prerequisites from implementable work. Do not modify any files.` + "\n" + taskClassificationInstructions
	dir := filepath.Join(e.Dir, name)
	c, err := prepareAgentContext(e.Repo, work, dir, empty, nil, "", nil, documentsFromPlans(plans))
	if err != nil {
		return err
	}
	if c.NewRequirements != "" {
		prompt += "\nThese plan documents are part of the project requirements and must be covered exactly once. Read every document in this index: " + c.NewRequirements
	}
	prompt += c.references()
	prompt += "\nUse only fields in this JSON schema. Priorities must be integers from 1 to 5. Put development rounds in details or tags, not extra fields.\n" + backlogSchema
	if correction != "" {
		prompt += "\nYour previous backlog was rejected: " + correction + "\nReturn a corrected backlog that satisfies the schema and preserves all project requirements. Do not include private context sources."
	}
	raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
	if err != nil {
		return err
	}
	if err = c.validate(); err != nil {
		return err
	}
	var b Backlog
	validationErr := json.Unmarshal(raw, &b)
	if validationErr == nil {
		validationErr = b.Validate()
	}
	if validationErr == nil {
		validationErr = e.Repo.validateBootstrapSources(b.Sources)
	}
	if validationErr != nil {
		if correction == "" {
			after, captureErr := e.Repo.Snapshot(work)
			if captureErr != nil {
				return captureErr
			}
			if after != tree {
				return fmt.Errorf("bootstrap changed code; its result was not applied")
			}
			e.event("bootstrap-correction", "Rejected invalid backlog; requesting one corrected backlog: "+validationErr.Error())
			return e.bootstrapResponse(ctx, plans, validationErr.Error())
		}
		return fmt.Errorf("bootstrap backlog rejected after correction (result: %s): %w", filepath.Join(dir, "result.json"), validationErr)
	}
	after, err := e.Repo.Snapshot(work)
	if err != nil {
		return err
	}
	if after != tree {
		return fmt.Errorf("bootstrap changed code; its result was not applied")
	}
	return writeJSON(e.Config.Todo, b)
}

func (e *Engine) plan(ctx context.Context) error {
	documents, err := e.appliedContext()
	if err != nil {
		return err
	}
	contracts := []string{}
	for _, t := range e.State.Backlog.Tasks {
		contracts = append(contracts, t.Contract())
	}
	signature := hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if e.State.Plan != nil && e.State.PlanSignature == signature {
		return nil
	}
	name := identifier("plan")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	op := e.capture()
	e.event("plan", "Planning independent tasks and executable checks...")
	dir := filepath.Join(e.Dir, name)
	c, err := prepareAgentContext(e.Repo, work, dir, e.State.Backlog, nil, "", documents, nil)
	if err != nil {
		return err
	}
	raw, err := e.runPlanner(ctx, op, work, dir, planPrompt(e.State.Backlog, c))
	if err != nil {
		return err
	}
	if err = c.validate(); err != nil {
		return err
	}
	var p Plan
	if err = json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if e.Config.Setup != "" {
		p.Setup = e.Config.Setup
	}
	e.recoveryCommands(&p)
	if err = e.quarantineInvalidScopes(&p, op.config.Verify); err != nil {
		return err
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		return err
	}
	if tree != e.State.Published {
		return fmt.Errorf("planning changed code; its result was not applied")
	}
	e.State.Plan = &p
	e.State.PlanSignature = signature
	return e.save()
}

// command runs one shell command through the check semaphore. The semaphore
// bounds concurrent setup and verify processes across workers and checks
// together. The separate len(checks) bound in Run limits the speculative
// integration chain; the two limits cover different resources.
func (e *Engine) command(ctx context.Context, command, work, dir, name string) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	select {
	case e.checkSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.checkSlots }()
	return runShell(ctx, work, dir, name, command, resourceEnv(dir))
}

type workerResult struct {
	ID      string
	Summary *Summary
	Commit  string
	Err     error
}

type resultReceipt struct {
	Base     string  `json:"base"`
	Contract string  `json:"contract"`
	Repairs  int     `json:"repairs"`
	Summary  Summary `json:"summary"`
	Commit   string  `json:"commit"`
}

func (e *Engine) execute(ctx context.Context, op operation, a Attempt, b Backlog, p Plan, documents []contextDocument) (r workerResult) {
	if op.config.Setup != "" {
		p.Setup = op.config.Setup
	}
	r.ID = a.ID
	dir := filepath.Join(a.Directory, fmt.Sprintf("work-%02d-%s", a.Repairs, identifier("session")))
	ctx, finish, err := beginResourceOperation(ctx, dir)
	if err != nil {
		r.Err = &LaunchError{"resource ownership", err}
		return r
	}
	defer finish()
	defer func() {
		if r.Err == nil && r.Summary != nil {
			r.Err = writeJSON(filepath.Join(a.Directory, "receipt.json"), resultReceipt{a.Base, a.Task.Contract(), a.Repairs, *r.Summary, r.Commit})
		}
	}()
	feedback := a.Feedback
	if err := e.command(ctx, p.Setup, a.Worktree, dir, "setup"); err != nil {
		if ctx.Err() != nil {
			r.Err = ctx.Err()
			return r
		}
		var launchErr *LaunchError
		if errors.As(err, &launchErr) {
			r.Err = err
			return r
		}
		feedback += "\n" + commandFeedback("Workspace setup failed. Diagnose and repair the setup before completing the task. If it requires an external prerequisite, report blocked.", p.Setup, err, filepath.Join(dir, "setup.log"))
	}
	c, err := prepareAgentContext(e.Repo, a.Worktree, dir, b, &a.Task, feedback, documents, nil)
	if err != nil {
		r.Err = err
		return r
	}
	raw, err := op.agent.Run(ctx, a.Agent, a.Worktree, dir, workerPrompt(b, a.Task, c), true)
	var summary Summary
	if err == nil {
		if err = json.Unmarshal(raw, &summary); err == nil {
			err = summary.Validate(a.Task.ID)
		}
		if err != nil {
			err = &AgentError{Kind: "invalid_result", Detail: err.Error()}
		}
	}
	var agentErr *AgentError
	if errors.As(err, &agentErr) && agentErr.Kind == "invalid_result" {
		// Recover only the report. An invalid report must not rerun implementation.
		before, snapshotErr := e.Repo.Snapshot(a.Worktree)
		if snapshotErr != nil {
			r.Err = snapshotErr
			return r
		}
		prompt := fmt.Sprintf("Recover the final task report for %s. Read the retained implementation and logs in %s. Do not change any files or repeat implementation. Return the task summary JSON contract from the original prompt. Report blocked if completion cannot be established.", a.Task.ID, dir)
		raw, err = op.agent.Run(ctx, a.Agent, a.Worktree, filepath.Join(dir, "report-recovery"), prompt, true)
		after, snapshotErr := e.Repo.Snapshot(a.Worktree)
		if snapshotErr != nil {
			r.Err = snapshotErr
			return r
		}
		if after != before {
			r.Err = fmt.Errorf("report recovery changed code; task result is retained for inspection")
			return r
		}
	}
	if err != nil {
		r.Err = err
		return r
	}
	if err = c.validate(); err != nil {
		r.Err = err
		return r
	}
	if err = json.Unmarshal(raw, &summary); err == nil {
		err = summary.Validate(a.Task.ID)
	}
	if err != nil {
		r.Err = err
		return r
	}
	if err = writeJSON(filepath.Join(a.Directory, "summary.json"), summary); err != nil {
		r.Err = err
		return r
	}
	r.Summary = &summary
	if summary.Status == "blocked" {
		return r
	}
	tree, err := e.Repo.Snapshot(a.Worktree)
	if err != nil {
		r.Err = err
		return r
	}
	commit, err := e.Repo.Commit(tree, []string{a.Base}, "Complete "+a.Task.ID)
	if err != nil {
		r.Err = err
		return r
	}
	if err = e.Repo.Ref(a.Ref, commit); err != nil {
		r.Err = err
		return r
	}
	r.Summary.Files, err = e.Repo.Changes(a.Base, commit)
	if err != nil {
		r.Err = err
		return r
	}
	if err = writeJSON(filepath.Join(a.Directory, "summary.json"), r.Summary); err != nil {
		r.Err = err
		return r
	}
	r.Commit = commit
	return r
}

// errPlanMissing marks dispatch work deferred because the live plan is nil.
// The loop waits for a planning pass to rebuild the plan instead of
// dereferencing it. Mid-flight invalidations keep the plan and clear only its
// signature, so a live nil plan comes from retained state; the recovery
// transition in Run rebuilds the plan once live workers and checks finish.
var errPlanMissing = errors.New("execution plan is missing; a planning pass must rebuild it")

// A retained attempt holds no live worker, so it cannot reserve its scope:
// two retained attempts with overlapping scopes would veto each other forever
// and no task would dispatch again. start() relaunches a retained attempt and
// marks it running before the next candidate is evaluated, which restores
// exclusivity from that moment.
func (e *Engine) compatible(t Task) bool {
	if e.State.Plan == nil {
		// Without a plan there is no scope to gate concurrency against, so
		// the task is not dispatchable until a planning pass rebuilds it.
		return false
	}
	scope := e.State.Plan.Scope(t.ID)
	for _, a := range e.State.Attempts {
		switch a.Phase {
		case "done", "blocked", "resume", "cancelled":
			continue
		}
		if a.Task.ID == t.ID {
			continue
		}
		if overlaps(scope, e.State.Plan.Scope(a.Task.ID)) {
			return false
		}
	}
	return true
}

// A plan can understate what a worker touches. Observed files from its result
// commit expand the task's write scope so later dispatch decisions see the
// overlap, as observe_scope did in the earlier engine.
func (e *Engine) observeScope(a *Attempt) {
	if e.State.Plan == nil || a.Summary == nil {
		return
	}
	var extra []string
	seen := map[string]bool{}
	for _, f := range a.Summary.Files {
		p := filepath.ToSlash(filepath.Clean(f))
		// The same predicate the plan and files rules use, so the three
		// cannot drift; a whole-repository, repeated, or excluded path needs
		// no separate scope entry.
		if _, ok := validWritePath(p); !ok || p == "." || seen[p] || e.Repo.excluded(p) {
			continue
		}
		seen[p] = true
		if !overlaps(Scope{TaskID: a.Task.ID, Paths: []string{p}}, e.State.Plan.Scope(a.Task.ID)) {
			extra = append(extra, p)
		}
	}
	if len(extra) == 0 {
		return
	}
	for i := range e.State.Plan.Tasks {
		if e.State.Plan.Tasks[i].TaskID == a.Task.ID {
			e.State.Plan.Tasks[i].Paths = append(e.State.Plan.Tasks[i].Paths, extra...)
			e.taskEvent("scope", fmt.Sprintf("%s  Plan scope expanded with observed paths: %s", a.Task.ID, strings.Join(extra, ", ")), a)
			return
		}
	}
}
func (e *Engine) attempt(id string) *Attempt {
	for _, a := range e.State.Attempts {
		if a.ID == id {
			return a
		}
	}
	return nil
}
func (e *Engine) start(ctx context.Context, t Task, out chan<- workerResult) (*Attempt, error) {
	if _, err := e.appliedContext(); err != nil {
		return nil, err
	}
	if e.State.Plan == nil {
		// Defer before any attempt state exists, so the task dispatches
		// cleanly after a planning pass rebuilds the plan.
		return nil, errPlanMissing
	}
	var a *Attempt
	for _, v := range e.State.Attempts {
		if v.Task.ID == t.ID && v.Phase == "resume" {
			a = v
			break
		}
	}
	if a == nil {
		for i := len(e.State.Attempts) - 1; i >= 0; i-- {
			v := e.State.Attempts[i]
			if v.Task.ID != t.ID || v.Phase != "blocked" || v.Task.Contract() != t.Contract() {
				continue
			}
			if _, err := os.Stat(v.Worktree); err != nil {
				continue
			}
			a = v
			if a.Base != e.State.Tip {
				tree, err := e.Repo.Snapshot(a.Worktree)
				if err != nil {
					return nil, err
				}
				partial, err := e.Repo.Commit(tree, []string{a.Base}, "Retain partial "+t.ID)
				if err != nil {
					return nil, err
				}
				if err = e.Repo.Ref(a.Ref, partial); err != nil {
					return nil, err
				}
				work, err := e.Repo.Workspace(identifier("resume"), e.State.Tip, e.State.Backlog)
				if err != nil {
					return nil, err
				}
				if err = e.Repo.Merge(work, partial); err != nil {
					e.Repo.Remove(work)
					return a, e.hold(a, mergeFailureFeedback("retained partial implementation", err))
				}
				a.Worktree = work
				a.Worktrees = append(a.Worktrees, work)
				a.Base = e.State.Tip
			}
			a.Task = t
			a.Summary = nil
			a.ResultCommit = ""
			a.Verification = nil
			a.Feedback = "Continue the retained partial implementation. The task was explicitly reopened."
			a.Repairs = 0
			a.Repeats = 0
			a.Diagnosed = false
			if err := os.Remove(filepath.Join(a.Directory, "receipt.json")); err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			break
		}
	}
	if a == nil {
		id := identifier("task")
		dir := filepath.Join(e.Dir, id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		work, err := e.Repo.Workspace(id, e.State.Tip, e.State.Backlog)
		if err != nil {
			return nil, err
		}
		a = &Attempt{ID: id, Task: t, Agent: e.Config.Agent, Base: e.State.Tip, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/" + id, Phase: "running", Worktrees: []string{work}}
		e.State.Attempts = append(e.State.Attempts, a)
	}
	a.Phase = "running"
	e.State.Backlog.Task(t.ID).Status = "doing"
	if err := e.export(); err != nil {
		return nil, err
	}
	if err := e.launch(ctx, a, out); err != nil {
		return a, err
	}
	return a, e.recoveryDispatch(t)
}
func (e *Engine) launch(ctx context.Context, a *Attempt, out chan<- workerResult) error {
	documents, err := e.appliedContext()
	if err != nil {
		return err
	}
	if e.State.Plan == nil {
		// The worker snapshot needs the plan. Defer the launch until a
		// planning pass rebuilds it instead of cloning a nil pointer, and
		// report no start event for work that did not begin.
		return errPlanMissing
	}
	e.taskEvent("start", a.Task.ID+"  "+a.Task.Title, a)
	snapshot, backlog, plan := clone(*a), clone(e.State.Backlog), clone(*e.State.Plan)
	op := e.capture()
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		workerCtx := ctx
		cancel := func() {}
		if op.config.Timeout > 0 {
			workerCtx, cancel = context.WithTimeout(ctx, op.config.Timeout)
		}
		defer cancel()
		result := e.execute(workerCtx, op, snapshot, backlog, plan, documents)
		select {
		case out <- result:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (e *Engine) hold(a *Attempt, reason string) error {
	a.Phase = "blocked"
	a.Feedback = reason
	a.Summary = &Summary{TaskID: a.Task.ID, Status: "blocked", Summary: "Work is retained for recovery.", Files: []string{}, Blockers: []string{reason}, UnblockWhen: "Resolve the recorded condition and set this task to todo."}
	return e.applyResult(a)
}
func (e *Engine) applyResult(a *Attempt) error {
	if err := e.contract(a); err != nil {
		return err
	}
	if a.Summary == nil {
		return fmt.Errorf("missing summary for %s", a.Task.ID)
	}
	t := e.State.Backlog.Task(a.Task.ID)
	s := a.Summary
	// The application, including follow-ups, stages until the export
	// commits it. A failure before that commit reports no success and
	// leaves no in-memory-only task: recovery replays the retained result.
	findings := e.beginFindingStage()
	t.Status = s.Status
	// History accumulates across attempts; the latest report never erases
	// earlier evidence, and a hold keeps the files already recorded. A
	// reported entry no scope can use is rejected at this merge, so the files
	// list stays scannable and the next planning pass cannot copy that entry
	// into its write paths (session run-8240e3d146f82a22).
	usable, rejected := reportedFiles(s.Files)
	t.Files = mergeUnique(t.Files, usable)
	t.Blockers = mergeUnique(t.Blockers, s.Blockers)
	t.UnblockWhen = s.UnblockWhen
	t.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if s.Status == "done" {
		a.Phase = "done"
		e.recoveryProgress(t.ID)
		e.State.Integrated++
		e.State.Finished = false
		e.successes++
		if e.successes >= 3 {
			e.capacity = min(e.Config.Jobs, e.capacity+1)
			e.successes = 0
		}
	} else {
		a.Phase = "blocked"
		e.recoveryBlocked(t.ID)
	}
	for _, f := range s.Followups {
		findings.add(f)
	}
	if err := e.export(); err != nil {
		findings.rollback()
		return err
	}
	findings.emit()
	// The rejection follows the reported outcome, not the task: the entry the
	// worker asserted cannot scope repository work is dropped and logged, and
	// the rest of the report applies as reported.
	for _, path := range rejected {
		e.taskEvent("quarantine", a.Task.ID+"  rejected reported entry; outcome kept: "+(&writePathError{TaskID: a.Task.ID, Path: path}).Error(), a)
	}
	e.taskEvent("result", fmt.Sprintf("%s  %s | %s", t.ID, s.Status, s.Summary), a)
	e.sweepResources()
	for _, b := range s.Blockers {
		e.taskEvent("blocker", "  "+b, a)
	}
	if e.Config.Hook != "" {
		path := filepath.Join(a.Directory, "summary.json")
		if err := writeJSON(path, s); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(withTiming(context.Background(), e.Store), 30*time.Second)
		err := runProcess(ctx, e.Config.Root, filepath.Join(a.Directory, "hook.log"), []string{e.Config.Hook, t.ID, s.Status, path, a.ID}, nil, nil)
		cancel()
		if err != nil {
			e.taskEvent("hook-error", "Result hook failed: "+err.Error(), a)
		}
	}
	if s.Status == "done" {
		for _, w := range a.Worktrees {
			e.Repo.Remove(w)
		}
	}
	return nil
}
func mergeUnique(old, fresh []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range append(append([]string{}, old...), fresh...) {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}
