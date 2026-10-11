package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	defer e.Repo.Remove(c.Worktree)
	dir := filepath.Join(a.Directory, "checks", c.Commit)
	if err := e.command(ctx, p.Setup, c.Worktree, dir, "setup"); err != nil {
		c.OK = false
		c.Feedback = "Integration setup failed:\n" + logTail(filepath.Join(dir, "setup.log"))
		return c
	}
	err := e.command(ctx, p.Verify, c.Worktree, dir, "verify")
	if err != nil {
		c.OK = false
		c.Feedback = "Combined verification failed:\n" + logTail(filepath.Join(dir, "verify.log"))
		return c
	}
	tree, err := e.Repo.Snapshot(c.Worktree)
	if err != nil || tree != c.Tree {
		c.OK = false
		c.Feedback = "Verification changed project files or failed to capture its result.\n"
		if err != nil {
			c.Feedback += err.Error()
		}
		return c
	}
	if op.config.Assess != "" {
		e.assess(ctx, op, a, c, dir)
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

// A semantic report is advisory. Its exit status never substitutes for tests.
// Reports and exact evidence references remain beside the candidate's checks.
func (e *Engine) assess(ctx context.Context, op operation, a Attempt, c Candidate, dir string) {
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
	err = runProcess(ctx, c.Worktree, filepath.Join(dir, "assessment.log"), []string{"bash", "-lc", op.config.Assess}, nil, extra)
	if err != nil {
		_ = writeJSON(filepath.Join(dir, "assessment-error.json"), map[string]string{"outcome": "error", "error": err.Error()})
	}
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
		feedback = "Fresh diagnosis required. The previous approach repeats the same failure. Find its cause and use a different approach. Split remaining work into evidence-backed followups only if this task's acceptance criteria remain satisfied.\n" + feedback
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
	e.launch(ctx, a, out)
	return nil
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
	observed, err := e.Repo.Snapshot(e.Config.Root)
	if err != nil {
		return err
	}
	if observed == p.Before && p.Before != p.Tree {
		if err = e.Repo.Apply(p.Before, p.Tree); err != nil {
			return err
		}
		observed, err = e.Repo.Snapshot(e.Config.Root)
		if err != nil {
			return err
		}
	}
	if observed != p.Tree {
		return fmt.Errorf("project files changed during integration; verified work is retained without marking the task done")
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

type reviewResult struct {
	Status   string    `json:"status"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
	Complete bool      `json:"complete"`
}

func (e *Engine) review(ctx context.Context, final bool) error {
	name := identifier("review")
	work, err := e.Repo.Workspace(name, e.State.Tip, e.State.Backlog)
	if err != nil {
		return err
	}
	defer e.Repo.Remove(work)
	backlog, _ := json.Marshal(e.State.Backlog)
	kind := "milestone"
	if final {
		kind = "final"
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
Read repository instructions, project goal, source specifications, and this backlog:
%s
Exercise complete user workflows where possible. Compare the integrated product with requirements,
not only task summaries. Verify suspected defects with code or executable evidence.
Capture missing capabilities and missing ownership. Respect the project goal and exclusions.
Do not modify any files, weaken checks, or invent unrelated improvements.
Return JSON {"status":"reviewed","summary":"...","findings":[{"title":"...","details":"acceptance criteria","evidence":"observed facts and file or command","depends_on":[],"priority":1}],"complete":false}.
Set complete true only when the intended goal is satisfied and no required work is blocked or unfinished.
Duplicate findings already tracked in the backlog must not become new tasks.`, kind, e.State.Tip, backlog)
	if len(evidence) > 0 {
		prompt += "\nAdvisory semantic reports and their evidence packets are in these directories:\n" + strings.Join(evidence, "\n") + "\nThese reports are unvalidated signals. Independently reproduce suspected defects before adding tasks."
	}
	raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, filepath.Join(e.Dir, name), prompt, false)
	if err != nil {
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
	// Older adapters append findings to a private backlog. Accept only additions;
	// this preserves the legacy contract without giving review ownership of state.
	var reviewed Backlog
	if err = readJSON(filepath.Join(work, e.Repo.Excluded[0]), &reviewed); err != nil {
		return err
	}
	if err = reviewed.Validate(); err != nil {
		return err
	}
	for _, t := range e.State.Backlog.Tasks {
		v := reviewed.Task(t.ID)
		if v == nil || hashJSON(*v) != hashJSON(t) {
			return fmt.Errorf("review changed an existing task")
		}
	}
	for _, t := range reviewed.Tasks {
		if e.State.Backlog.Task(t.ID) == nil {
			if t.Marker() {
				result.Complete = true
				continue
			}
			if t.Status != "todo" {
				return fmt.Errorf("review added a task that was already marked complete")
			}
			e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, t)
		}
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
