package looper

import (
	"context"
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

func (e *Engine) Run(parent context.Context) (int, error) {
	ctx, cancel := context.WithCancel(parent)
	defer func() { cancel(); e.wg.Wait() }()
	if e.controller != nil {
		// Only this session's metadata is removed on any exit path.
		defer removeController(e.Dir, e.controller.Session)
		defer clearView(e.Dir)
	}
	if err := e.initialize(ctx); err != nil {
		return 1, err
	}
	if e.Config.Verbose {
		e.event("config", fmt.Sprintf("agents %s (review %s) | jobs %d | checks %d | poll %s | retry %d at %s | worker timeout %s | git timeout %s | task file %s",
			e.Config.Agent, e.Config.ReviewAgent, e.Config.Jobs, e.Config.Checks, e.Config.Poll, e.Config.RetryLimit, e.Config.RetryDelay, e.Config.Timeout, gitTimeout, e.Config.Todo))
	}
	e.event("start", fmt.Sprintf("Looper | up to %d implementing | %d concurrent checks", e.capacity, e.Config.Checks))
	stopPath := filepath.Join(e.Dir, "stop")
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
	for {
		if ctx.Err() != nil {
			return 130, ctx.Err()
		}
		if e.err != nil {
			return 1, e.err
		}
		if _, err := os.Stat(stopPath); err == nil {
			draining = true
		}
		// Settings requests and plan imports apply at loop boundaries,
		// including idle polling. Synchronous planning and review above are
		// never interrupted.
		e.processSettingsRequests()
		if e.err != nil {
			return 1, e.err
		}
		e.processRequeueRequests()
		if e.err != nil {
			return 1, e.err
		}
		e.processPlanImports(ctx)
		if e.err != nil {
			return 1, e.err
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
				a := e.attempt(r.ID)
				if a == nil {
					return 1, fmt.Errorf("unknown completed attempt %s", r.ID)
				}
				if r.Err != nil {
					if ctx.Err() != nil {
						return 130, ctx.Err()
					}
					if err := e.hold(a, r.Err.Error()); err != nil {
						return 1, err
					}
					continue
				}
				a.Summary = r.Summary
				a.ResultCommit = r.Commit
				if r.Summary.Status == "blocked" {
					if err := e.applyResult(a); err != nil {
						return 1, err
					}
				} else {
					e.observeScope(a)
					a.Phase = "ready"
					pending = append(pending, a)
					if err := e.save(); err != nil {
						return 1, err
					}
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
			a := e.attempt(entry.AttemptID)
			if err := e.Store.Check(a.ID, result.Commit, "executable", map[bool]string{true: "pass", false: "fail"}[result.OK], filepath.Join(a.Directory, "checks", result.Commit, "verify.log")); err != nil {
				return 1, err
			}
			if result.Base != e.State.Tip {
				a.Phase = "ready"
				pending = append(pending, a)
				continue
			}
			if !result.OK {
				if err := e.repair(ctx, a, failureEvidence(result.Feedback), results); err != nil {
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
			if deferred == pending[0].ID {
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
					e.event("defer", a.Task.ID+" conflicts with the integrating chain; retrying after the running check.")
					break
				}
				pending = pending[1:]
				if err = e.repair(ctx, a, candidate.Feedback, results); err != nil {
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
			e.event("check", "Checking "+a.Task.ID+" with integrated work...")
			aCopy, pCopy := clone(*a), clone(*e.State.Plan)
			op := e.capture()
			e.wg.Add(1)
			go func(c Candidate, ch chan Candidate) { defer e.wg.Done(); ch <- e.verify(ctx, op, aCopy, c, pCopy) }(candidate, entry.Done)
		}
		busy := len(active)+len(pending)+len(checks) > 0
		if !busy {
			if draining {
				e.event("stop", "Stopped after draining active work. Run looper to resume.")
				return 0, e.save()
			}
			if err := e.refreshIdle(); err != nil {
				return 1, err
			}
			ready := e.State.Backlog.Ready()
			final := len(ready) == 0
			if e.State.ReviewPending || (final && e.State.ReviewedTip != e.State.Tip) {
				if err := e.review(ctx, final); err != nil {
					reviewFailures++
					e.event("review-error", err.Error())
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
						e.event("finish", "Project goal satisfied.")
						return 0, nil
					}
					e.event("waiting", "No runnable tasks remain. Blocked work is retained.")
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
		if !draining && !e.State.ReviewPending && milestoneRoom() && (e.Config.Limit == 0 || attempted < e.Config.Limit) {
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
				if claimed || !e.compatible(t) {
					continue
				}
				a, err := e.start(ctx, t, results)
				if err != nil {
					return 1, err
				}
				active[a.ID] = true
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

// Only import edits at a boundary with no running work. Execution status remains
// in SQLite; the task file is its human-editable, version-checked projection.
func (e *Engine) refreshIdle() error {
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	tree, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	head, branch := e.Repo.Head()
	if digest(data) == e.State.ExportHash && tree == e.State.Published && head == e.State.Head && branch == e.State.Branch {
		return nil
	}
	var b Backlog
	if err = readJSON(e.Config.Todo, &b); err != nil {
		return err
	}
	if err = b.Validate(); err != nil {
		return err
	}
	for _, a := range e.State.Attempts {
		if a.Phase != "done" && a.Phase != "blocked" {
			return fmt.Errorf("cannot change the project baseline while work is retained")
		}
	}
	seed, err := e.Repo.Commit(tree, []string{head}, "Refresh project baseline for Looper")
	if err != nil {
		return err
	}
	if err = e.Repo.Ref(e.State.Ref, seed); err != nil {
		return err
	}
	e.State.Backlog = b
	e.State.ExportHash = digest(data)
	e.State.Head = head
	e.State.Branch = branch
	e.State.Tip = seed
	e.State.Published = tree
	e.State.Plan = nil
	e.State.PlanSignature = ""
	e.State.Finished = false
	e.State.ReviewedTip = ""
	e.event("refresh", "Imported changed project requirements or files.")
	return e.save()
}

// idleWait sleeps for the poll interval unless a controller request arrives,
// in which case it returns early so the loop boundary processes it. It
// returns false when the context was cancelled.
func (e *Engine) idleWait(ctx context.Context, poll time.Duration) bool {
	deadline := time.Now().Add(poll)
	for {
		if requestsWaiting(e.Dir) || e.drainingNow() {
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
