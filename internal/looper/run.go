package looper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type checkRun struct {
	AttemptID string
	Candidate Candidate
	Done      chan Candidate
}

func (e *Engine) Run(parent context.Context) (code int, resultErr error) {
	if e.controller != nil {
		parent = context.WithValue(parent, resourceSessionKey{}, e.controller.Session)
	}
	ctx, cancel := context.WithCancel(withTiming(parent, e.Store))
	exitReason := "stopped"
	defer func() {
		crash := recover()
		if e.err != nil && !errors.Is(resultErr, e.err) {
			resultErr = errors.Join(resultErr, e.err)
		}
		if crash != nil {
			code, exitReason = 2, "panic"
			resultErr = fmt.Errorf("controller panic: %v", crash)
		} else if parent.Err() != nil {
			code, exitReason = 130, "cancelled"
			if !errors.Is(resultErr, parent.Err()) {
				resultErr = errors.Join(resultErr, parent.Err())
			}
		} else if resultErr != nil || code == 1 {
			code = 1
			exitReason = "error"
		}
		cancel()
		e.wg.Wait()
		if e.State != nil {
			e.sweepResources()
		}
		// A failure that ends the run is journaled as its bounded summary
		// (AX-021) before the shutdown record, so the cause stays retrievable
		// in one focused query even though no controller remains to restate
		// it. Failures that were already journaled with their task identity
		// where they happened are not repeated here.
		if resultErr != nil && crash == nil && (e.journaledFailure == nil || !errors.Is(resultErr, e.journaledFailure)) {
			if s := failureOf(resultErr); s != nil {
				e.failureEvent(nil, s)
			}
		}
		exit := sessionExit{Reason: exitReason, ExitCode: code}
		if resultErr != nil {
			exit.Error = resultErr.Error()
		}
		if err := e.Store.endSession(exit); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("record controller exit: %w", err))
			if code == 0 || code == 2 {
				code = 1
			}
		}
		if e.controller != nil {
			clearView(e.Dir)
			removeController(e.Dir, e.controller.Session)
		}
		// A request that arrived after the final boundary can no longer be
		// honored; removing it here, still under the repository lock, keeps
		// it from draining the next run.
		e.releaseStopRequests()
		if crash != nil {
			panic(crash)
		}
	}()
	if previous := e.Store.previous; previous != nil && previous.Unclean {
		// The unclean-end note journals only once startup has succeeded
		// (AX-022): a run that fails at a startup guard never streamed a
		// progress line, so its stdout holds exactly one value — the error
		// document — and no caller has to skip journal lines to read it. The
		// previous session's exit data rides on the resume event either way,
		// so a resumed run keeps its explanation.
		e.uncleanPreviousNote = previous
	}
	if err := e.initialize(ctx); err != nil {
		e.journalUncleanPreviousNote(true)
		return 1, err
	}
	e.journalUncleanPreviousNote(false)
	// Under the repository lock no other controller can be live: requests
	// that do not bind to this session were left for a controller that is
	// gone and must not drain this run. Legitimate startup requests survive.
	e.sweepStopRequests()
	e.sweepResources()
	if e.Config.Verbose {
		e.event("config", fmt.Sprintf("agents %s (review %s) | jobs %d | checks %d | poll %s | retry %d at %s | worker timeout %s | git timeout %s | task file %s",
			e.Config.Agent, e.Config.ReviewAgent, e.Config.Jobs, e.Config.Checks, e.Config.Poll, e.Config.RetryLimit, e.Config.RetryDelay, e.Config.Timeout, gitTimeout, e.Config.Todo))
	}
	e.event("start", fmt.Sprintf("Looper | up to %d implementing | %d concurrent checks", e.capacity, e.Config.Checks))
	active := map[string]bool{}
	results := make(chan workerResult, e.Config.Jobs)
	var pending []*Attempt
	var checks []*checkRun
	for _, a := range e.State.Attempts {
		if a.Phase == "ready" {
			pending = append(pending, a)
		}
	}
	attempted := 0
	draining := false
	lastProgress := ""
	reviewFailures := 0
	// A head that conflicts while a check runs waits for the chain to move
	// instead of repeating a full prepare on every loop tick.
	deferred := ""
	// A nil plan defers checks and dispatch until a planning pass rebuilds
	// it. The flag reports that deferral once instead of on every tick.
	planWait := false
	// An external task-file edit that lands while work runs holds the loop:
	// dispatch pauses, finished results wait, and the edit imports at the
	// first boundary with nothing in flight. This mirrors the input wait for
	// invalid task files, which holds the loop for a corrected edit.
	backlogHold := false
	var heldResults []workerResult
	for {
		if ctx.Err() != nil {
			return 130, ctx.Err()
		}
		if e.err != nil {
			return 1, e.err
		}
		if e.drainingNow() {
			draining = true
		}
		// This boundary check catches an external task-file edit while work
		// is in flight. The idle refresh imports only when the loop is fully
		// idle, which retained work prevents, so without the hold the next
		// publication or export would end the session. An edit that changed
		// only formatting never holds: the equivalent bytes become the
		// accepted export and dispatch continues (AX-018).
		if !backlogHold && (len(active) > 0 || len(pending) > 0 || len(checks) > 0) &&
			digestFile(e.Config.Todo) != e.State.ExportHash {
			if edit := e.classifyBacklogFile(); edit.Equivalent {
				if err := e.adoptBacklogBytes(edit); err != nil {
					return 1, err
				}
			} else {
				backlogHold = true
				e.backlogHoldEvent()
			}
		}
		if backlogHold && len(active) == 0 && len(checks) == 0 {
			// Everything the edit interrupted has drained. Import it now, then
			// let the requeued and buffered results continue against the new
			// baseline.
			err := e.refreshHeld()
			if err != nil {
				if !e.idleInputWait(err) {
					return 1, err
				}
				if draining {
					// A stop request outranks the correction wait, exactly as
					// on the idle input wait where the drain check runs before
					// the refresh: the hold releases to the drain path instead
					// of keeping the controller alive for an edit that may
					// never come. The unimported edit and the retained results
					// stay exactly as they are.
					exitReason = "drained"
					code, err := e.drainExit(fmt.Sprintf("Stopped after draining active work. The task-file edit still needs correction; %d result(s) are retained in %s and resume after the corrected edit imports.", len(pending)+len(heldResults), e.Dir))
					return code, err
				}
				// Invalid edits wait for a correction, mirroring the idle
				// input wait; dispatch stays paused meanwhile.
				e.waitForInput(err)
				// Queued requests are held through the wait, then answered
				// exactly as at the loop boundary below: settings apply,
				// add-task and requeue answer with a conflict without touching
				// the invalid file, and plan imports queue until the input is
				// valid again.
				if reqErr := e.processControllerRequests(ctx); reqErr != nil {
					return 1, reqErr
				}
				// This branch continues back to itself, so the wait must sleep
				// the poll interval even while request envelopes sit queued:
				// waking on one, as idleWait does, would spin the branch
				// without sleep whenever an envelope stays unclaimed.
				if !e.correctionWait(ctx, e.Config.Poll) {
					return 130, ctx.Err()
				}
				continue
			}
			if e.inputPending {
				e.resumeFromInputWait()
			}
			// Worker results that finished during the hold replay against the
			// imported baseline. Applying them earlier would have collided
			// with the edit at the export guard.
			for _, r := range heldResults {
				if err := e.applyFinishedWorker(ctx, r, &pending); err != nil {
					if ctx.Err() != nil {
						return 130, ctx.Err()
					}
					return 1, err
				}
			}
			heldResults = nil
			backlogHold = false
			continue
		}
		// Settings requests and plan imports apply at loop boundaries,
		// including idle polling. Synchronous planning and review above are
		// never interrupted.
		if err := e.processControllerRequests(ctx); err != nil {
			return 1, err
		}
		e.pollWorkerActivity()
		if e.viewDirty {
			e.publishView(draining)
			e.viewDirty = false
		}
		// Consume all finished workers without waiting for unrelated tasks.
		for more := true; more; {
			select {
			case r := <-results:
				delete(active, r.ID)
				if backlogHold {
					// The task file changed outside Looper: the report waits
					// for the boundary import, because applying it now would
					// collide with the edit at the export guard.
					heldResults = append(heldResults, r)
					continue
				}
				if err := e.applyFinishedWorker(ctx, r, &pending); err != nil {
					if ctx.Err() != nil {
						return 130, ctx.Err()
					}
					return 1, err
				}
			default:
				more = false
			}
		}
		// Speculative checks may overlap, but accepted revisions form one chain.
		for len(checks) > 0 {
			var result Candidate
			select {
			case result = <-checks[0].Done:
			default:
				goto checked
			}
			entry := checks[0]
			checks = checks[1:]
			deferred = ""
			if ctx.Err() != nil {
				// Cancellation discards the finished check instead of
				// publishing from a cancelled verification. The attempt stays
				// in its verifying phase and restart re-verifies it.
				return 130, ctx.Err()
			}
			a := e.attempt(entry.AttemptID)
			e.sweepResources()
			if result.LaunchFailure != nil {
				e.emitCheckFailure(a, &result)
				e.journaledFailure = result.LaunchFailure
				return 1, result.LaunchFailure
			}
			if err := e.Store.Check(a.ID, result.Commit, "executable", map[bool]string{true: "pass", false: "fail"}[result.OK], filepath.Join(a.Directory, "checks", result.Commit, "verify.log")); err != nil {
				return 1, err
			}
			if result.Base != e.State.Tip {
				a.Phase = "ready"
				pending = append(pending, a)
				continue
			}
			if backlogHold {
				// Publication and repair both wait for the imported baseline:
				// the verified result re-prepares against it, and a failed
				// check repeats its repair decision afterwards.
				a.Phase = "ready"
				pending = append(pending, a)
				if err := e.save(); err != nil {
					return 1, err
				}
				continue
			}
			if !result.OK {
				// The failed check journals its bounded summary (AX-021)
				// before the repair decision it feeds.
				e.emitCheckFailure(a, &result)
				// repair() normalizes the feedback itself: agents keep the
				// raw output and artifact paths for the retry.
				if err := e.repair(ctx, a, result.Feedback, results); err != nil {
					if errors.Is(err, errPlanMissing) {
						// The plan is gone; requeue the attempt so the
						// failed check reaches its repair after the next
						// planning pass.
						pending = append(pending, a)
						continue
					}
					return 1, err
				}
				if a.Phase == "running" {
					active[a.ID] = true
				}
				continue
			}
			e.noteAssessment(a, result)
			result.Before = e.State.Published
			e.State.Publication = &result
			if err := e.save(); err != nil {
				return 1, err
			}
			if err := e.publish(); err != nil {
				if errors.Is(err, errBacklogChanged) {
					// An edit landed inside this iteration, after the boundary
					// check at loop entry. Requeue the verified result; the
					// hold that follows imports the edit before the attempt
					// re-prepares against the moved baseline.
					e.State.Publication = nil
					a.Phase = "ready"
					pending = append(pending, a)
					if err = e.save(); err != nil {
						return 1, err
					}
					if !backlogHold {
						backlogHold = true
						e.backlogHoldEvent()
					}
					continue
				}
				if errors.Is(err, errBaselineMoved) {
					// The owner committed while this result verified (T046).
					// The acceptance folded the commit into the baseline and
					// requeued the publication; the result re-prepares against
					// the folded base and verifies again.
					if e.State.Publication != nil || a.Phase != "ready" {
						return 1, fmt.Errorf("the accepted commit left the publication of %s unrequeued", a.Task.ID)
					}
					pending = append(pending, a)
					continue
				}
				return 1, err
			}
			if err := e.applyResult(a); err != nil {
				return 1, err
			}
			if e.Config.ReviewEvery > 0 && e.State.Integrated-e.State.ReviewedAt >= e.Config.ReviewEvery {
				e.State.ReviewPending = true
			}
		}
	checked:
		for len(pending) > 0 && len(checks) < e.Config.Checks {
			if backlogHold || deferred == pending[0].ID || e.State.Plan == nil {
				// A held backlog imports before more work runs against it, a
				// conflicting head waits for the chain to move, and a nil plan
				// cannot be snapshotted for verification. Each deferral is
				// reported where it is detected.
				break
			}
			a := pending[0]
			base := e.State.Tip
			if len(checks) > 0 {
				base = checks[len(checks)-1].Candidate.Commit
			}
			candidate, err := e.prepare(a, base)
			if err != nil {
				return 1, err
			}
			if !candidate.OK {
				if len(checks) > 0 {
					deferred = a.ID
					// The retry message stays neutral: a merge failure that is
					// not a verified conflict must not read as one while it
					// waits for the chain to move.
					e.taskEvent("defer", a.Task.ID+" waits for the integrating chain; retrying after the running check.", a)
					break
				}
				pending = pending[1:]
				if err = e.repair(ctx, a, candidate.Feedback, results); err != nil {
					if errors.Is(err, errPlanMissing) {
						// The plan is gone; requeue the attempt so its
						// repair runs after the next planning pass.
						pending = append(pending, a)
						continue
					}
					return 1, err
				}
				if a.Phase == "running" {
					active[a.ID] = true
				}
				continue
			}
			pending = pending[1:]
			a.Phase = "verifying"
			if err = e.save(); err != nil {
				return 1, err
			}
			entry := &checkRun{AttemptID: a.ID, Candidate: candidate, Done: make(chan Candidate, 1)}
			checks = append(checks, entry)
			e.taskEvent("check", "Checking "+a.Task.ID+" with integrated work...", a)
			aCopy, pCopy := clone(*a), clone(*e.State.Plan)
			op := e.capture()
			e.wg.Add(1)
			go func(c Candidate, ch chan Candidate) { defer e.wg.Done(); ch <- e.verify(ctx, op, aCopy, c, pCopy) }(candidate, entry.Done)
		}
		// Live workers and checks share engine state, so recovery for a missing
		// plan waits until they finish instead of changing it under them.
		live := len(active)+len(checks) > 0
		if live && e.State.Plan == nil {
			// Work is in flight against a nil plan: checks and dispatch
			// defer until a planning pass rebuilds it. Report the wait once.
			if !planWait {
				planWait = true
				e.event("defer", "Execution plan is missing; checks and dispatch wait for a planning pass.")
			}
		} else {
			planWait = false
		}
		// Recovery transition for retained ready work without a plan: live work
		// has finished, but the retained results still hold the loop. Checks and
		// repairs both defer on a nil plan, and the idle path below stays
		// unreachable while pending work keeps the loop busy, so without this
		// transition the deferral above never ends and drain requests are
		// ignored. Drain saves the unverified results and exits; otherwise the
		// due review runs first and the plan is rebuilt before any retained
		// result is checked or repaired.
		if !backlogHold && !live && len(pending) > 0 && e.State.Plan == nil {
			if draining {
				// The drained work may have released a decided cancellation;
				// apply it before the exit so its receipt decides here (a
				// decision the exit would otherwise carry to the next run).
				e.drivePendingCancellations()
				if e.err != nil {
					return 1, e.err
				}
				exitReason = "drained"
				code, err := e.drainExit(fmt.Sprintf("Stopped after draining active work. %d unverified result(s) have no execution plan; they are retained in %s and the next run plans and verifies them.", len(pending), e.Dir))
				return code, err
			}
			if e.drainingNow() {
				continue
			}
			// A due review precedes planning, exactly as on the idle path,
			// including reviews the integration counters made due.
			if e.reviewDue() {
				// Persist before launching: cancellation must retain the review.
				e.State.ReviewPending = true
				if err := e.save(); err != nil {
					return 1, err
				}
				if err := e.review(ctx, false); err != nil {
					return 1, e.retainedRecoveryError("review", len(pending), err)
				}
				reviewFailures = 0
			}
			if err := e.plan(ctx); err != nil {
				return 1, e.retainedRecoveryError("planning", len(pending), err)
			}
		}
		busy := live || len(pending) > 0
		if !busy && !backlogHold {
			if draining {
				// Same as the retained drain above: one last drive so a
				// decided cancellation decides before the controller exits.
				e.drivePendingCancellations()
				if e.err != nil {
					return 1, e.err
				}
				exitReason = "drained"
				code, err := e.drainExit("Stopped after draining active work. Run looper to resume.")
				return code, err
			}
			if err := e.refreshIdle(); err != nil {
				if !e.idleInputWait(err) {
					return 1, err
				}
				// Invalid task-file input waits here for a corrected edit.
				// Planning, review, dispatch, and backlog writes stay paused;
				// queued requests are held, and no older contract runs.
				e.waitForInput(err)
				if !e.idleWait(ctx, e.Config.Poll) {
					return 130, ctx.Err()
				}
				continue
			}
			if e.inputPending {
				e.resumeFromInputWait()
			}
			// A check may have released the last retained attempt after the
			// import pass at loop entry. Apply queued requirements before
			// planning or dispatching more work against the old backlog.
			e.processPlanImports(ctx)
			if e.err != nil {
				return 1, e.err
			}
			if ctx.Err() != nil {
				return 130, ctx.Err()
			}
			if e.drainingNow() {
				continue
			}
			if len(e.State.Backlog.Ready()) == 0 {
				recovered, err := e.recoverRun(ctx)
				if err != nil {
					return 1, err
				}
				if recovered {
					continue
				}
				data, err := os.ReadFile(e.Config.Todo)
				if err != nil {
					return 1, err
				}
				if digest(data) != e.State.ExportHash {
					continue
				}
			}
			ready := e.State.Backlog.Ready()
			final := len(ready) == 0
			if e.reviewDue() || (final && e.State.ReviewedTip != e.State.Tip) {
				// Persist before launching: cancellation must retain the review.
				e.State.ReviewPending = true
				if err := e.save(); err != nil {
					return 1, err
				}
				if err := e.review(ctx, final); err != nil {
					reviewFailures++
					e.event("review-error", err.Error())
					var agent *AgentError
					if errors.As(err, &agent) && (agent.Kind == "configuration" || agent.Kind == "invalid_request") {
						// Account, credential, and request-shape failures repeat
						// identically on retry. Stop with work retained; the
						// surfaced agent message names the operator action.
						return 1, err
					}
					if reviewFailures >= 3 {
						return 1, err
					}
					continue
				}
				reviewFailures = 0
				ready = e.State.Backlog.Ready()
			}
			if len(ready) == 0 {
				if e.Config.Once {
					if e.State.Finished {
						exitReason = "complete"
						e.event("finish", "Project goal satisfied.")
						return 0, nil
					}
					e.event("waiting", "No runnable tasks remain. Blocked work is retained.")
					exitReason = "no_runnable_tasks"
					return 2, nil
				}
				message := "Waiting for changed requirements or resolved blockers. Use looper stop to finish."
				if lastProgress != message {
					e.event("waiting", message)
					lastProgress = message
				}
				// Sleep out the poll interval, but watch the request queues
				// cheaply so idle waiting still applies changes promptly.
				if !e.idleWait(ctx, e.Config.Poll) {
					return 130, ctx.Err()
				}
				continue
			}
			if e.Config.Limit > 0 && attempted >= e.Config.Limit {
				exitReason = "dispatch_limit"
				e.event("limit", "Dispatch limit reached; all active work is retained or integrated.")
				return 2, nil
			}
			if err := e.plan(ctx); err != nil {
				return 1, err
			}
		}
		milestoneRoom := func() bool {
			return e.Config.ReviewEvery == 0 || e.State.Integrated-e.State.ReviewedAt+len(active)+len(pending)+len(checks) < e.Config.ReviewEvery
		}
		// An accepted import pauses new ordinary dispatch here: retained
		// attempts, repairs, checks, and publications keep reaching their
		// boundary above, the import applies at the first free one, and
		// dispatch resumes against the expanded backlog only after that.
		if !draining && !backlogHold && !e.importDispatchHold() && !e.State.ReviewPending && milestoneRoom() && (e.Config.Limit == 0 || attempted < e.Config.Limit) {
			capacity := e.capacity
			if len(pending)+len(checks) > e.Config.Checks {
				capacity = max(1, capacity-1)
			}
			if m := availableMemory(); m > 0 && m < 1<<30 {
				capacity = 1
			}
			for _, t := range e.State.Backlog.Ready() {
				if len(active) >= capacity || !milestoneRoom() || (e.Config.Limit > 0 && attempted >= e.Config.Limit) {
					break
				}
				claimed := false
				for _, a := range e.State.Attempts {
					if a.Task.ID == t.ID && a.Phase != "done" && a.Phase != "blocked" && a.Phase != "resume" {
						claimed = true
					}
				}
				if claimed || e.pendingCancelTask(t.ID) || !e.compatible(t) {
					continue
				}
				a, err := e.start(ctx, t, results)
				if err != nil {
					if errors.Is(err, errPlanMissing) {
						// A nil plan cannot gate concurrency; the task
						// dispatches after the next planning pass.
						continue
					}
					return 1, err
				}
				if a.Phase == "running" {
					active[a.ID] = true
				}
				attempted++
			}
		}
		completed, total := 0, 0
		for _, t := range e.State.Backlog.Tasks {
			if !t.Marker() {
				total++
				if t.Status == "done" {
					completed++
				}
			}
		}
		// Live counts are session state; the completion fraction belongs to
		// the whole project, so the label keeps them distinct.
		progress := fmt.Sprintf("%d implementing | %d checking | project %d/%d complete", len(active), len(checks), completed, total)
		if progress != lastProgress {
			e.event("progress", progress)
			lastProgress = progress
		}
		select {
		case <-ctx.Done():
			return 130, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// journalUncleanPreviousNote records the previous session's unclean end. The
// note streams to callers only when startup succeeded; a run a startup guard
// refuses keeps its stdout to the launch event and the error document, while
// `looper events` still explains the restart from the store.
func (e *Engine) journalUncleanPreviousNote(storeOnly bool) {
	previous := e.uncleanPreviousNote
	if previous == nil {
		return
	}
	recoveryData, _ := e.previousExitData().(map[string]any)
	message := fmt.Sprintf("Previous controller session %s ended uncleanly; its cause is unknown. Last recorded event: %d.", previous.ID, previous.LastEventID)
	if storeOnly {
		_, _ = e.Store.Event("recovery", message, e.identity().merge(recoveryData))
		return
	}
	e.eventData("recovery", message, e.identity().merge(recoveryData))
}

// processControllerRequests claims and answers every queued controller
// request at a loop boundary: settings requests apply, add-task and requeue
// requests answer from their current guards, and plan imports queue or apply
// at a safe boundary. The queue is swept first, so junk entries and excess
// envelopes recover instead of wedging every claim. It returns the first
// engine failure so the caller can end the run; rejected requests report
// through acknowledgements instead.
func (e *Engine) processControllerRequests(ctx context.Context) error {
	// Decided cancellations drive first: their target work drained before
	// this boundary, and the statuses they free shape every answer the
	// claims below give (AX-019).
	e.drivePendingCancellations()
	if e.err != nil {
		return e.err
	}
	// A new pass retries the claims and continues any bounded shedding, so
	// the previous pass may wake on queued requests again.
	e.suppressRequestWake = false
	shed, cleaned, over := sweepRequestQueue(e.Dir, e.controllerSessionID())
	if over > 0 {
		e.suppressRequestWake = true
	}
	e.recordRefusedRequests(shed)
	if len(shed) > 0 || cleaned > 0 {
		var parts []string
		if len(shed) > 0 {
			parts = append(parts, fmt.Sprintf("%d envelope(s) beyond the %d pending limit were answered with an error acknowledgement and removed", len(shed), maxPendingRequests))
		}
		if cleaned > 0 {
			parts = append(parts, fmt.Sprintf("%d stale non-request entries were removed", cleaned))
		}
		if over > 0 {
			parts = append(parts, fmt.Sprintf("%d envelope(s) still beyond the limit continue at the next boundaries", over))
		}
		e.event("request-queue", "Recovered the request queue: "+strings.Join(parts, "; ")+".")
	}
	e.processSettingsRequests()
	if e.err != nil {
		return e.err
	}
	e.processAddTaskRequests()
	if e.err != nil {
		return e.err
	}
	e.processCancelRequests()
	if e.err != nil {
		return e.err
	}
	e.processAmendRequests()
	if e.err != nil {
		return e.err
	}
	e.processRecoveryRequests()
	if e.err != nil {
		return e.err
	}
	e.processRequeueRequests()
	if e.err != nil {
		return e.err
	}
	e.processPlanImports(ctx)
	return e.err
}

// drainExit completes a drain request. The request is consumed only after
// the state saves successfully, so an interrupted drain keeps its request and
// the next controller still resumes the retained work. The applied receipt
// commits in that same save, before the request file is removed, so the
// honored stop stays queryable after the file is gone (AX-009).
func (e *Engine) drainExit(message string) (int, error) {
	e.recordStopRequest(e.drainingRequest(), RequestApplied, message, "")
	if err := e.save(); err != nil {
		return 1, err
	}
	if err := os.Remove(stopRequestPath(e.Dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 1, fmt.Errorf("remove completed drain request: %w", err)
	}
	e.event("stop", message)
	return 0, nil
}

// drainingRequest returns the published stop request when it targets this
// controller, for the receipt a drain records.
func (e *Engine) drainingRequest() *stopRequest {
	req, info, err := readStopRequest(e.Dir)
	if err != nil || !e.requestTargets(req, info) {
		return nil
	}
	return req
}

// recordStopRequest writes one stop request's receipt into the receipt
// queue without committing it; the caller's own save commits the receipt
// beside the state it belongs with. A receipt that already holds a terminal
// outcome keeps it.
func (e *Engine) recordStopRequest(req *stopRequest, state, summary, errText string) {
	if e.State == nil || req == nil {
		return
	}
	id := stopRequestID(req)
	if rec := e.requestReceipt(id); rec != nil && requestStateTerminal(rec.State) {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	e.upsertRequest(&RequestRecord{
		ID: id, Type: PublicRequestStop, State: state,
		Session: e.controllerSessionID(), TargetSession: req.Session,
		SubmittedAt: now, DecidedAt: now, Summary: summary, Error: errText,
	})
}

// retainedRecoveryError explains a failed recovery step for retained ready
// work. The results stay retained with their commits, and the message names
// where, so the operator can act without mining the event log.
func (e *Engine) retainedRecoveryError(step string, retained int, err error) error {
	return fmt.Errorf("%s failed while recovering %d retained result(s); the work is retained in %s and the next run resumes it: %w", step, retained, e.Dir, err)
}

// invalidBacklogError marks task-file input that a corrected edit can fix.
// It is recoverable only at an idle boundary of a continuous run; startup,
// finite runs, and every other failure keep their existing fatal behavior.
type invalidBacklogError struct{ err error }

func (e *invalidBacklogError) Error() string { return e.err.Error() }
func (e *invalidBacklogError) Unwrap() error { return e.err }

// retainedContractRefusal reports a task-file edit that changes the contract
// of a retained task, which no import path may apply: startup compares the
// retained attempts against the imported backlog, so the import would strand
// that work at the task-contract guard instead of replaying it. Like invalid
// input, the refusal names an edit a correction can fix, so a continuous run
// holds the boundary open instead of failing.
type retainedContractRefusal struct{ tasks []string }

func (r *retainedContractRefusal) Error() string {
	return fmt.Sprintf("the task-file edit changes the contract of retained task(s) %s while their work is retained, so the edit cannot import; edit outside the retained tasks, restore the saved contract, or wait for the retained work to finish", boundedIDList(r.tasks, 8))
}

// idleInputWait reports whether an idle refresh error should hold the loop
// open for a corrected task file instead of failing the run.
func (e *Engine) idleInputWait(err error) bool {
	var bad *invalidBacklogError
	var refused *retainedContractRefusal
	return (errors.As(err, &bad) || errors.As(err, &refused)) && !e.Config.Once
}

// Only import edits at a boundary with no running work. Execution status remains
// in SQLite; the task file is its human-editable, version-checked projection.
func (e *Engine) refreshIdle() error { return e.refresh(false) }

// refreshHeld imports an external edit at a boundary the run held open. The
// work the edit interrupted has drained, and its results requeue or replay
// against the new baseline, so retained attempts alone no longer block the
// import. Startup recovery runs the same import for the edit a cancelled hold
// left behind (T058), once every retained result can replay. Project files,
// the branch, and the head must still match the accepted baseline; those
// changes keep their existing conflict handling.
func (e *Engine) refreshHeld() error { return e.refresh(true) }

func (e *Engine) refresh(held bool) error {
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	// Parse and validate the same bytes that are hashed below, so a concurrent
	// edit can never split the checked content from the accepted baseline.
	// The parse is strict: duplicate keys are invalid input, never a
	// normalized meaning (AX-018).
	var b Backlog
	if b, err = parseBacklogStrict(data); err != nil {
		return &invalidBacklogError{fmt.Errorf("%s is not valid JSON: %w", e.Config.Todo, err)}
	}
	if err = b.Validate(); err != nil {
		return &invalidBacklogError{fmt.Errorf("%s has invalid content: %w", e.Config.Todo, err)}
	}
	tree, err := e.Repo.SnapshotFrom(e.Config.Root, e.State.Published)
	if err != nil {
		return err
	}
	head, branch := e.Repo.Head()
	if digest(data) == e.State.ExportHash && tree == e.State.Published && head == e.State.Head && branch == e.State.Branch {
		return nil
	}
	// Equivalent formatting (AX-018): the bytes changed, the content did not,
	// and the project baseline is untouched. Adopting the observed bytes
	// keeps the run on its baseline — no replan, no re-review, no import —
	// where a real edit must move the baseline at a safe boundary.
	if head == e.State.Head && branch == e.State.Branch && tree == e.State.Published {
		if edit := e.classifyBacklog(data); edit.Equivalent {
			return e.adoptBacklogBytes(edit)
		}
	}
	if held {
		// A held boundary may carry requeued and buffered work, but the
		// baseline itself must be untouched: changed project files, branch,
		// or head conflict with the retained results. An owner commit that
		// touches no retained write scope folds into the baseline first
		// (T046); everything else still conflicts.
		if head != e.State.Head || branch != e.State.Branch || tree != e.State.Published {
			if moved, err := e.recoverCommittedHead(); err != nil {
				return err
			} else if moved {
				return e.refresh(held)
			}
			return fmt.Errorf("cannot change the project baseline while work is retained")
		}
	} else {
		for _, a := range e.State.Attempts {
			if a.Phase != "done" && a.Phase != "blocked" && a.Phase != "cancelled" {
				// Retained work pins the baseline. An owner commit that
				// touches no retained write scope folds into it (T046); the
				// refresh then settles against the folded baseline.
				if moved, err := e.recoverCommittedHead(); err != nil {
					return err
				} else if moved {
					return e.refresh(held)
				}
				return fmt.Errorf("cannot change the project baseline while work is retained")
			}
		}
	}
	// An import that carries retained results must leave every retained task's
	// contract intact: startup compares the retained attempts against the
	// imported backlog, so a changed contract would strand that work at the
	// task-contract guard instead of replaying it. The idle path refuses
	// earlier whenever an attempt is still active, so its list here is empty;
	// a held import refuses and keeps the hold (T119). The checked application
	// and the preview gate the same edit through the same predicate (AX-017).
	if breakers := contractBreakers(e.State, &b); len(breakers) > 0 {
		return &retainedContractRefusal{tasks: breakers}
	}
	// The idle refresh parents its seed on the project head, exactly as
	// before. A held boundary still carries results whose commits must merge
	// with the new baseline, so its seed descends from the current tip; a
	// project without commits would otherwise grow an unrelated second root.
	parent := head
	if held {
		parent = e.State.Tip
	}
	seed, err := e.Repo.Commit(tree, []string{parent}, "Refresh project baseline for Looper")
	if err != nil {
		return err
	}
	if err = e.Repo.Ref(e.State.Ref, seed); err != nil {
		return err
	}
	e.State.Backlog = b
	e.State.ExportHash = digest(data)
	e.State.ExportSemantics = b.semanticFingerprint()
	// The imported bytes are the accepted bytes now; record them before any
	// journal overwrites the file.
	e.retainAcceptedTaskFile()
	e.State.Head = head
	e.State.Branch = branch
	e.State.Tip = seed
	e.State.Published = tree
	if !held {
		// The idle path re-plans immediately after the refresh. A held
		// boundary still has queued work whose checks and dispatch read the
		// plan, so it keeps the plan and clears only the signature; the next
		// planning pass rebuilds against the imported backlog.
		e.State.Plan = nil
	}
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	// Commit the refreshed baseline before the journal note (AX-003): a
	// reader pairing state with the cursor in one snapshot never sees the
	// refresh event with the baseline that precedes it.
	if err = e.save(); err != nil {
		return err
	}
	e.event("refresh", "Imported changed project requirements or files.")
	// An imported edit can carry a files entry no plan scope can use. The
	// same startup quarantine holds that task blocked instead of letting the
	// next planning pass fail the controller.
	return e.quarantineInvalidFiles()
}

// backlogHoldEvent reports the dispatch pause for an external task-file edit
// that landed while work was in flight.
func (e *Engine) backlogHoldEvent() {
	e.event("waiting", "The task file changed outside Looper while work runs. Dispatch is paused; the edit imports when active work reaches a boundary.")
}

// applyFinishedWorker records one finished worker on the owner goroutine: a
// failed launch ends the run, an error report holds the attempt, and a
// blocked report applies directly. Completed work queues for its check.
func (e *Engine) applyFinishedWorker(ctx context.Context, r workerResult, pending *[]*Attempt) error {
	a := e.attempt(r.ID)
	e.sweepResources()
	if a == nil {
		return fmt.Errorf("unknown completed attempt %s", r.ID)
	}
	if r.Err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var launch *LaunchError
		var agent *AgentError
		if errors.As(r.Err, &launch) || (errors.As(r.Err, &agent) && agent.Kind == "invalid_request") {
			// The run-ending classes journal their summary here, with the
			// attempt identity a task filter matches, instead of once more at
			// the run boundary without it.
			e.workerFailureEvent(a, r.Err)
			e.journaledFailure = r.Err
			return r.Err
		}
		// A held worker failure journals its bounded summary (AX-021); the
		// run-ending classes above were already journaled with the attempt
		// identity a task filter matches.
		e.workerFailureEvent(a, r.Err)
		return e.hold(a, r.Err.Error())
	}
	a.Summary = r.Summary
	a.ResultCommit = r.Commit
	if r.Summary.Status == "blocked" {
		return e.applyResult(a)
	}
	e.observeScope(a)
	a.Phase = "ready"
	*pending = append(*pending, a)
	return e.save()
}

// waitForInput enters the recoverable wait for a corrected task file. The
// diagnostic is reported when the problem appears and when it changes, not on
// every poll. The accepted state and the user's file are preserved as they are.
func (e *Engine) waitForInput(err error) {
	e.inputPending = true
	diag := fmt.Sprintf("Task file input needs correction: %v. Correct the file to continue.", err)
	if diag != e.inputProblem {
		e.inputProblem = diag
		e.event("waiting", diag)
	}
	// Publish before idleWait sleeps, including when the problem changes.
	e.publishView(false)
}

// resumeFromInputWait clears the input wait after a refresh accepted the file
// again, including when the corrected bytes match the previous accepted file.
func (e *Engine) resumeFromInputWait() {
	e.inputPending = false
	e.inputProblem = ""
	e.event("waiting", "Task file accepted again; resuming.")
	e.publishView(false)
}

// idleWait sleeps for the poll interval unless a controller request arrives,
// in which case it returns early so the loop boundary processes it. A pass
// that failed a claim, or still sheds a flooded queue, does not wake early:
// those envelopes cannot be claimed now, and the next boundary retries them
// after the sleep. It returns false when the context was cancelled.
func (e *Engine) idleWait(ctx context.Context, poll time.Duration) bool {
	return e.waitInterval(ctx, poll, true)
}

// correctionWait sleeps out the poll interval while a held backlog waits for
// a corrected task file. Unlike idleWait it does not wake for queued request
// envelopes: the hold branch continues back to itself instead of reaching the
// loop boundary, so an envelope no processor claims, such as an unknown
// request type, would otherwise make the wait sleepless. Queued requests are
// held and answered by the branch once per pass; a stop request still ends
// the wait at once.
func (e *Engine) correctionWait(ctx context.Context, poll time.Duration) bool {
	return e.waitInterval(ctx, poll, false)
}

func (e *Engine) waitInterval(ctx context.Context, poll time.Duration, wakeOnRequests bool) bool {
	deadline := time.Now().Add(poll)
	for {
		if (wakeOnRequests && !e.suppressRequestWake && requestsWaiting(e.Dir)) || e.drainingNow() {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(min(remaining, 200*time.Millisecond)):
		}
	}
}

// requestsWaiting reports whether any request envelope sits in the queue.
func requestsWaiting(dir string) bool {
	entries, err := os.ReadDir(requestDir(dir))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			return true
		}
	}
	return false
}

// reviewDue also repairs old snapshots saved before ReviewPending was set.
func (e *Engine) reviewDue() bool {
	return e.State.ReviewPending || (e.Config.ReviewEvery > 0 && e.State.Integrated-e.State.ReviewedAt >= e.Config.ReviewEvery)
}
