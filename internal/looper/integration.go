package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (e *Engine) prepare(a *Attempt, base string) (Candidate, error) {
	c := Candidate{Base: base, AttemptID: a.ID}
	work, err := e.Repo.Workspace(identifier("check"), base, e.State.Backlog)
	if err != nil {
		return c, err
	}
	if err = e.Repo.Merge(work, a.ResultCommit); err != nil {
		e.Repo.Remove(work)
		c.Feedback = "The task conflicts with the integrated project.\n" + err.Error()
		return c, nil
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		e.Repo.Remove(work)
		return c, err
	}
	commit, err := e.Repo.Commit(tree, []string{base, a.ResultCommit}, "Integrate "+a.Task.ID)
	if err != nil {
		e.Repo.Remove(work)
		return c, err
	}
	if err = e.Repo.Ref(a.Ref+"-candidate", commit); err != nil {
		e.Repo.Remove(work)
		return c, err
	}
	c.OK = true
	c.Commit = commit
	c.Tree = tree
	c.Worktree = work
	return c, nil
}
func (e *Engine) verify(ctx context.Context, op operation, a Attempt, c Candidate, p Plan) Candidate {
	if op.config.Setup != "" {
		p.Setup = op.config.Setup
	}
	if op.config.Verify != "" {
		p.Verify = op.config.Verify
	}
	defer e.Repo.Remove(c.Worktree)
	dir := filepath.Join(a.Directory, "checks", c.Commit)
	ctx, finish, err := beginResourceOperation(ctx, dir)
	if err != nil {
		c.OK = false
		c.LaunchFailure = &LaunchError{"resource ownership", err}
		return c
	}
	defer finish()
	if err := writeJSON(filepath.Join(dir, "verify-command.json"), map[string]string{"command": p.Verify}); err != nil {
		c.OK = false
		c.Feedback = "Cannot record verification command: " + err.Error()
		return c
	}
	if err := e.command(ctx, p.Setup, c.Worktree, dir, "setup"); err != nil {
		c.OK = false
		c.Feedback = commandFeedback("Integration setup failed:", p.Setup, err, filepath.Join(dir, "setup.log"))
		var launch *LaunchError
		if errors.As(err, &launch) {
			c.LaunchFailure = err
		}
		return c
	}
	err = e.command(ctx, p.Verify, c.Worktree, dir, "verify")
	if err != nil {
		c.OK = false
		c.Feedback = commandFeedback("Combined verification failed:", p.Verify, err, filepath.Join(dir, "verify.log"))
		var launch *LaunchError
		if errors.As(err, &launch) {
			c.LaunchFailure = err
		}
		return c
	}
	tree, err := e.Repo.Snapshot(c.Worktree)
	if err != nil {
		c.OK = false
		c.Feedback = "Verification command passed, but snapshot capture failed: " + err.Error()
		return c
	}
	if tree != c.Tree {
		c.OK = false
		comparison, diffErr := gitCommand(e.Repo.Root, nil, nil, "diff", "--no-ext-diff", "--name-status", c.Tree, tree, "--")
		path := filepath.Join(dir, "file-changes.txt")
		if diffErr == nil {
			diffErr = atomicWrite(path, comparison, 0600)
		}
		c.Feedback = "Verification command passed, but changed project files.\n"
		if diffErr != nil {
			c.Feedback += "Cannot retain file comparison: " + diffErr.Error()
		} else {
			lines := strings.Split(strings.TrimSpace(string(comparison)), "\n")
			if len(lines) > 20 {
				lines = append(lines[:20], "... additional paths in the artifact")
			}
			c.Feedback += strings.Join(lines, "\n") + "\nArtifacts: " + path
		}
		return c
	}
	if strings.TrimSpace(op.config.Assess) != "" {
		e.assess(ctx, op, a, c, dir)
		if ctx.Err() != nil {
			// Controller cancellation discards the check rather than turning
			// it into an advisory outcome; restart re-verifies the retained
			// result without repeating implementation.
			c.OK = false
			c.Feedback = "Verification was cancelled during the advisory assessment."
			return c
		}
		tree, err = e.Repo.Snapshot(c.Worktree)
		if err != nil || tree != c.Tree {
			c.OK = false
			c.Feedback = "The assessment command changed project files."
			return c
		}
	}
	c.Worktree = ""
	return c
}
func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	if len(b) > 16000 {
		b = b[len(b)-16000:]
	}
	return string(b)
}

// assessmentDeadline bounds any optional assessment command. It leaves time
// around an adapter's own evaluation deadline for startup and report writing.
const assessmentDeadline = 60 * time.Second

// A semantic report is advisory. Its exit status never substitutes for tests.
// Reports and exact evidence references remain beside the candidate's checks.
func (e *Engine) assess(ctx context.Context, op operation, a Attempt, c Candidate, dir string) {
	deadline := assessmentDeadline
	if e.assessDeadline > 0 {
		deadline = e.assessDeadline
	}
	e.assessWithin(ctx, op, a, c, dir, deadline)
}

// assessWithin runs one assessment under the supplied deadline; production
// always passes assessmentDeadline and tests pass a short duration. A
// deadline or a nonzero exit is advisory. Controller cancellation is not: it
// leaves no advisory record so the cancelled check can never read as a
// timeout that permits publication.
func (e *Engine) assessWithin(ctx context.Context, op operation, a Attempt, c Candidate, dir string, timeout time.Duration) {
	patch, err := gitCommand(e.Repo.Root, nil, nil, "diff", "--no-ext-diff", "--no-textconv", c.Base, c.Commit, "--")
	if err != nil {
		return
	}
	packet := map[string]any{"version": 1, "task": a.Task, "base": c.Base, "revision": c.Commit, "summary": a.Summary, "patch": string(patch), "verification": logTail(filepath.Join(dir, "verify.log")), "mode": "shadow"}
	path := filepath.Join(dir, "evidence.json")
	if err = writeJSON(path, packet); err != nil {
		return
	}
	extra := resourceEnv(dir)
	extra["LOOPER_EVIDENCE_FILE"] = path
	extra["LOOPER_ASSESS_REPORT"] = filepath.Join(dir, "assessment.json")
	logfile := filepath.Join(dir, "assessment.log")
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err = runShell(actx, c.Worktree, dir, "assessment", op.config.Assess, extra)
	if err == nil {
		return
	}
	if ctx.Err() != nil {
		return
	}
	_ = writeJSON(filepath.Join(dir, "assessment-error.json"), map[string]string{
		"outcome": "error", "error": err.Error(), "command": op.config.Assess, "log": logfile,
	})
	// Reporting happens on the owner goroutine: noteAssessment reads this
	// record when the check result is consumed and emits the advisory event.
}

func (e *Engine) repair(ctx context.Context, a *Attempt, feedback string, out chan<- workerResult) error {
	signature := digest([]byte(feedback))
	if signature == a.FailureHash {
		a.Repeats++
	} else {
		a.Repeats = 1
		a.Diagnosed = false
	}
	a.FailureHash = signature
	if a.Repairs >= e.Config.MaxRepairs {
		return e.hold(a, "Repair limit reached. "+feedback)
	}
	if a.Repeats >= 2 {
		if a.Diagnosed {
			return e.hold(a, "The same failure persists after a fresh diagnosis. "+feedback)
		}
		a.Diagnosed = true
		if err := e.save(); err != nil {
			return err
		}
		if d, ok := e.diagnose(ctx, a, feedback); ok {
			if d.RunRepair != nil {
				e.State.PendingRunRepair = &pendingRunRepair{Identity: e.recoveryIdentity(), Proposal: d}
				return e.hold(a, feedback)
			}
			feedback = "An independent diagnosis by the review agent found this root cause: " + d.RootCause +
				"\nEvidence: " + d.Evidence +
				"\nUse a different approach: " + d.FixApproach + "\n" + feedback
		} else {
			feedback = "Fresh diagnosis required. The previous approach repeats the same failure. Find its cause and use a different approach. Split remaining work into evidence-backed followups only if this task's acceptance criteria remain satisfied.\n" + feedback
		}
	}
	a.Repairs++
	e.capacity = max(1, e.capacity-1)
	e.successes = 0
	work, err := e.Repo.Workspace(identifier("repair"), e.State.Tip, e.State.Backlog)
	if err != nil {
		return err
	}
	if a.ResultCommit != "" {
		_ = e.Repo.Merge(work, a.ResultCommit)
	} else {
		e.Repo.Remove(work)
		work = a.Worktree
	}
	a.Worktree = work
	a.Worktrees = append(a.Worktrees, work)
	a.Base = e.State.Tip
	a.Phase = "running"
	a.Feedback = feedback
	a.ResultCommit = ""
	a.Verification = nil
	if err = e.save(); err != nil {
		return err
	}
	e.event("repair", "Repairing "+a.Task.ID+" with the recorded failure evidence.")
	return e.launch(ctx, a, out)
}

// diagnosis is the structured result of one repair consultation.
type diagnosis struct {
	RunRepair   *runRepair `json:"run_repair,omitempty"`
	RootCause   string     `json:"root_cause"`
	Evidence    string     `json:"evidence"`
	FixApproach string     `json:"fix_approach"`
}

// diagnose asks the review agent for an independent, read-only explanation of
// a repeated failure, so the next repair starts from a cause instead of the
// same evidence. The consultation uses an isolated copy of the retained work.
// Changed snapshots or unusable results leave the existing repair path intact.
func (e *Engine) diagnose(ctx context.Context, a *Attempt, feedback string) (diagnosis, bool) {
	before, err := e.Repo.Snapshot(a.Worktree)
	if err != nil {
		return diagnosis{}, false
	}
	commit, err := e.Repo.Commit(before, []string{a.Base}, "Inspect retained failure")
	if err != nil {
		return diagnosis{}, false
	}
	work, err := e.Repo.Workspace(identifier("diagnose"), commit, e.State.Backlog)
	if err != nil {
		return diagnosis{}, false
	}
	defer e.Repo.Remove(work)
	dir := filepath.Join(a.Directory, fmt.Sprintf("diagnose-%02d-%s", a.Repairs, identifier("session")))
	c, err := prepareAgentContext(e.Repo, work, dir, e.State.Backlog, &a.Task, feedback, nil, nil)
	if err != nil {
		return diagnosis{}, false
	}
	prompt := fmt.Sprintf(`Diagnose the repeated failure of task %s. Read the worktree and the attempt records under %s (summaries, check logs, receipts), and the failure feedback file. Do not change any files.
Failure feedback file: %s
Return one JSON object: {"root_cause":"why the current approach keeps failing","evidence":"files or logs that show the cause","fix_approach":"a different approach that resolves the cause"}.`, a.Task.ID, a.Directory, c.Feedback)
	op := e.capture()
	if e.State.Plan != nil {
		prompt += fmt.Sprintf("\nEffective run setup: %q\nEffective run verification: %q\n", e.effectiveRecoveryPlan().Setup, e.effectiveRecoveryPlan().Verify)
	}
	prompt += `If the task cannot repair a proven orchestration contract defect, include optional run_repair {"remove_sources":[],"setup":"dependency setup","verify":"meaningful current-stage check","reopen":["` + a.Task.ID + `"],"deferred_tasks":[]}. Remove only proven empty bootstrap context; preserve requirements and external blockers. Otherwise omit run_repair.`
	raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
	if err != nil {
		e.event("diagnosis-error", a.Task.ID+"  The diagnosis consultation failed; repairing with the recorded evidence.")
		return diagnosis{}, false
	}
	if err = c.validate(); err != nil {
		return diagnosis{}, false
	}
	var d diagnosis
	if decodeDiagnosis(raw, &d) != nil || strings.TrimSpace(d.RootCause) == "" || strings.TrimSpace(d.FixApproach) == "" {
		e.event("diagnosis-error", a.Task.ID+"  The diagnosis consultation returned no usable cause; repairing with the recorded evidence.")
		return diagnosis{}, false
	}
	if after, err := e.Repo.Snapshot(a.Worktree); err != nil || after != before {
		e.event("diagnosis-error", a.Task.ID+"  The diagnosis consultation changed files; its result was discarded.")
		return diagnosis{}, false
	}
	var inspected Backlog
	if err := readJSON(filepath.Join(work, e.Repo.Excluded[0]), &inspected); err != nil || hashJSON(inspected) != hashJSON(e.State.Backlog) {
		return diagnosis{}, false
	}
	if after, err := e.Repo.Snapshot(work); err != nil || after != before {
		return diagnosis{}, false
	}
	e.event("diagnosis", fmt.Sprintf("%s  Independent diagnosis: %s", a.Task.ID, d.RootCause))
	return d, true
}

// The journal is committed before applying files. Restart accepts only the
// complete before or after tree. A partial apply is retained for inspection.
func (e *Engine) publish() error {
	p := e.State.Publication
	if p == nil {
		return nil
	}
	a := e.attempt(p.AttemptID)
	if a == nil {
		return fmt.Errorf("publication references an unknown attempt")
	}
	if err := e.original(); err != nil {
		return err
	}
	if err := e.contract(a); err != nil {
		return err
	}
	if p.Base != e.State.Tip {
		return fmt.Errorf("publication base does not match the verified integration base")
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return err
	}
	if digest(data) != e.State.ExportHash {
		return fmt.Errorf("the backlog changed outside Looper before integration")
	}
	observed, err := e.Repo.SnapshotFrom(e.Config.Root, p.Tree, p.Before)
	if err != nil {
		return err
	}
	if observed != p.Tree {
		before, err := e.Repo.SnapshotFrom(e.Config.Root, p.Before, p.Tree)
		if err != nil {
			return err
		}
		if before == p.Before {
			if err = e.Repo.Apply(p.Before, p.Tree); err != nil {
				return err
			}
			observed, err = e.Repo.SnapshotFrom(e.Config.Root, p.Tree, p.Before)
			if err != nil {
				return err
			}
		}
	}
	if observed != p.Tree {
		return e.publicationConflict(a, p, observed)
	}
	if err = e.Repo.Ref(e.State.Ref, p.Commit); err != nil {
		return err
	}
	e.State.Tip = p.Commit
	e.State.Published = p.Tree
	a.Phase = "integrated"
	v := *p
	a.Verification = &v
	e.State.Publication = nil
	return e.save()
}

func (e *Engine) publicationConflict(a *Attempt, p *Candidate, observed string) error {
	var message strings.Builder
	fmt.Fprintf(&message, "project files match neither the accepted tree nor the verified candidate for %s. Verified work is retained without marking the task done.", a.Task.ID)
	for _, expected := range []struct{ label, tree string }{
		{"accepted", p.Before}, {"verified candidate", p.Tree},
	} {
		fmt.Fprintf(&message, "\nDifferences from the %s tree %s:", expected.label, expected.tree)
		paths, err := e.Repo.Changes(expected.tree, observed)
		if err != nil {
			fmt.Fprintf(&message, "\n  Cannot list differing files: %v", err)
			continue
		}
		for i, path := range paths {
			if i == 20 {
				fmt.Fprintf(&message, "\n  ... and %d more files", len(paths)-i)
				break
			}
			fmt.Fprintf(&message, "\n  %q", path)
		}
	}
	fmt.Fprintf(&message, "\nRetained work: %s", e.Dir)
	message.WriteString("\nPreserve your local edits. Restore project files to the accepted tree, then rerun Looper. Merge and verify your local edits after integration. Git HEAD can be older than the accepted tree. Keep the retained work and task file.")
	return errors.New(message.String())
}

type reviewResult struct {
	Status   string    `json:"status"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Complete bool      `json:"complete"`
}

func (e *Engine) review(ctx context.Context, final bool) error {
	documents, err := e.appliedContext()
	if err != nil {
		return err
	}
	e.activity = "reviewing"
	e.activityAgent = e.Config.ReviewAgent
	e.publishView(false)
	defer func() { e.activity, e.activityAgent = "", ""; e.publishView(false) }()
	name := identifier("review")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	kind := "milestone"
	if final {
		kind = "final"
	}
	dir := filepath.Join(e.Dir, name)
	c, err := prepareAgentContext(e.Repo, work, dir, e.State.Backlog, nil, "", documents, nil)
	if err != nil {
		return err
	}
	op := e.capture()
	e.event("review", "Checking "+kind+" product behavior against the project goal...")
	var evidence []string
	for _, a := range e.State.Attempts {
		if a.Verification != nil {
			dir := filepath.Join(a.Directory, "checks", a.Verification.Commit)
			if _, err := os.Stat(filepath.Join(dir, "assessment.json")); err == nil {
				evidence = append(evidence, dir)
			}
		}
	}
	if len(evidence) > 10 {
		evidence = evidence[len(evidence)-10:]
	}
	prompt := fmt.Sprintf(`You are running a %s review pass for Looper at revision %s.
Read repository instructions, project goal, source specifications, and the accepted backlog snapshot:
%s
Exercise complete user workflows where possible. Compare the integrated product with requirements,
not only task summaries. Verify suspected defects with code or executable evidence.
Capture missing capabilities and missing ownership. Respect the project goal and exclusions.
Do not modify any files, weaken checks, or invent unrelated improvements.
Return JSON {"status":"reviewed","summary":"...","findings":[{"title":"...","details":"acceptance criteria","evidence":"observed facts and file or command","depends_on":[],"priority":1,"owner":"agent","key":"stable-outcome-key","existing_task_id":""}],"complete":false}.
Set complete true only when the intended goal is satisfied and no required work is blocked or unfinished.
Report gaps as findings only. Do not edit the task file or add tasks yourself; Looper owns task changes and rejects a review that edits the backlog.
Duplicate findings already tracked in the backlog must not become new tasks.
The final review must cover the original goal and every applied import, including imports that added no new tasks.`+"\n"+findingInstructions, kind, e.State.Tip, c.Backlog)
	prompt += c.references()
	prompt += "Read every document in the applied requirements index before declaring completion. Do not limit review to requirements selected by workers.\n"
	if len(evidence) > 0 {
		prompt += "\nAdvisory semantic reports and their evidence packets are in these directories:\n" + strings.Join(evidence, "\n") + "\nThese reports are unvalidated signals. Independently reproduce suspected defects before adding tasks."
	}
	raw, err := e.runReviewAgent(ctx, op, work, dir, prompt, 30*time.Second)
	if err != nil {
		return err
	}
	if err = c.validate(); err != nil {
		return err
	}
	if _, err = e.appliedContext(); err != nil {
		return err
	}
	var result reviewResult
	if err = json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.Status != "reviewed" || result.Summary == "" {
		return fmt.Errorf("review returned an invalid report")
	}
	tree, err := e.Repo.Snapshot(work)
	if err != nil {
		return err
	}
	if tree != e.State.Published {
		return fmt.Errorf("review changed code; its result was not applied")
	}
	// Findings and completion come from the review response alone. The
	// private backlog belongs to the controller: any edit, including a
	// legacy project-done marker, rejects the review before anything applies.
	var reviewed Backlog
	if err = readJSON(filepath.Join(work, e.Repo.Excluded[0]), &reviewed); err != nil {
		return fmt.Errorf("review changed the private backlog: %w", err)
	}
	if err = reviewed.Validate(); err != nil {
		return fmt.Errorf("review changed the private backlog: %w", err)
	}
	if hashJSON(reviewed) != hashJSON(e.State.Backlog) {
		return fmt.Errorf("review changed the private backlog; findings and completion were not applied")
	}
	for _, f := range result.Findings {
		e.addFinding(f)
	}
	e.State.ReviewedAt = e.State.Integrated
	e.State.ReviewedTip = e.State.Tip
	e.State.ReviewPending = false
	e.State.PlanSignature = ""
	open := false
	for _, t := range e.State.Backlog.Tasks {
		if !t.Marker() && t.Status != "done" {
			open = true
		}
	}
	e.State.Finished = final && result.Complete && !open
	if err = e.Store.Check("", e.State.Tip, "milestone", map[bool]string{true: "complete", false: "reviewed"}[e.State.Finished], filepath.Join(e.Dir, name, "result.json")); err != nil {
		return err
	}
	e.event("review", result.Summary)
	return e.export()
}

func (e *Engine) noteAssessment(a *Attempt, c Candidate) {
	dir := filepath.Join(a.Directory, "checks", c.Commit)
	for _, name := range []string{"assessment.json", "assessment-error.json"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var report struct {
			Outcome   string `json:"outcome"`
			Aggregate struct {
				Outcome string `json:"outcome"`
			} `json:"aggregate"`
		}
		if json.Unmarshal(data, &report) != nil {
			e.event("assessment", a.Task.ID+"  Invalid advisory report; see "+dir)
			continue
		}
		outcome := report.Outcome
		if outcome == "" {
			outcome = report.Aggregate.Outcome
		}
		if outcome == "" {
			outcome = "unknown"
		}
		e.event("assessment", a.Task.ID+"  Advisory assessment: "+outcome)
		if outcome == "fail" || outcome == "review" {
			e.State.ReviewPending = true
		}
		if err = e.Store.Check(a.ID, c.Commit, "semantic", outcome, filepath.Join(dir, name)); err != nil {
			e.err = err
		}
	}
}

func failureEvidence(s string) string { // Strip operation paths before comparing failures across repairs.
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "Artifacts:") {
			lines = append(lines, l)
		}
	}
	return strings.Join(lines, "\n")
}
