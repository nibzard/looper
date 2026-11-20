package looper

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Reconciliation (T124): a review finding that confirms a blocked task's
// condition, a delivered outcome, or a wrong owner produces a guarded
// transition. The fixtures mirror the delivery-audit cases: a task blocked
// behind a satisfied condition (Ohya T093), an outcome that exists while the
// task stays blocked (Charlie T077), an operator gate without an operator
// action (Ohya T109), conditions that name the blocked task itself (Charlie
// T027, T066), and the identifier-free circular wording (Charlie T040).

// reconcileEngine returns an engine holding one blocked task with a satisfied
// condition, plus the revision its findings must carry.
func reconcileEngine(t *testing.T) (*Engine, *Task, string) {
	t.Helper()
	e := snapshotEngine(t)
	e.State.Finished = false
	e.State.Tip = "tip-1"
	e.State.Backlog.Tasks = []Task{
		{ID: "T001", Title: "Import records", Details: "Load every record once.", Priority: 1, Status: "blocked",
			Blockers: []string{"The loader lands first."}, UnblockWhen: "The loader lands; set this task to todo.",
			DependsOn: []string{"T002"}},
		{ID: "T002", Title: "Load records", Priority: 1, Status: "done"},
	}
	return e, e.State.Backlog.Task("T001"), "tip-1"
}

func reconciliationFinding(title string) Finding {
	return Finding{Title: title, Details: "The condition holds.", Evidence: "check log records 1:1", Owner: "agent",
		Key: "import-readiness", ExistingTaskID: "T001", Transition: transitionResume, Revision: "tip-1", Priority: 1}
}

func decisionTask(e *Engine) *Task {
	return e.State.Backlog.Task("T003")
}

func TestReconciliationResumesBlockedTask(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	before := task.Contract()
	applyFinding(t, e, reconciliationFinding("Import records can start"))
	task = e.State.Backlog.Task("T001")
	if task.Status != "todo" || len(task.Blockers) != 0 || task.UnblockWhen != "" || task.BlockReason != nil {
		t.Fatalf("blocked task was not resumed: %+v", task)
	}
	if len(e.State.Backlog.Ready()) != 1 || e.State.Backlog.Ready()[0].ID != "T001" {
		t.Fatal("resumed task is not schedulable")
	}
	if task.Contract() != before {
		t.Fatal("resume changed the task contract")
	}
	if e.State.Finished || e.State.PlanSignature != "" {
		t.Fatal("resume left stale completion or plan state")
	}
	assertFindingEvidence(t, e, "T001", "Reconciled resume")
}

func TestReconciliationCompletesDeliveredOutcome(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	task.Blockers = []string{"The fuse outcome is missing."}
	applyFinding(t, e, Finding{Title: "Fuse consumption already exists", Details: "The outcome is delivered.",
		Evidence: "the campaign consumes fuses", Owner: "agent", Key: "fuse-outcome", ExistingTaskID: "T001",
		Transition: transitionComplete, Revision: "tip-1", Priority: 1})
	task = e.State.Backlog.Task("T001")
	if task.Status != "done" || len(task.Blockers) != 0 || task.BlockReason != nil {
		t.Fatalf("delivered outcome was not closed: %+v", task)
	}
	if len(e.State.Backlog.Ready()) != 0 {
		t.Fatal("completed outcome entered the worker queue")
	}
	// The completed outcome is never implemented again: repeats and a later
	// resume proposal both leave the recorded result alone.
	applyFinding(t, e, Finding{Title: "Fuse consumption already exists", Details: "Still delivered.",
		Evidence: "second review", Owner: "agent", Key: "fuse-outcome", ExistingTaskID: "T001",
		Transition: transitionComplete, Revision: "tip-1", Priority: 1})
	resume := reconciliationFinding("Import records can start")
	resume.ExistingTaskID = "T001"
	applyFinding(t, e, resume)
	if task = e.State.Backlog.Task("T001"); task.Status != "done" || len(e.State.Backlog.Tasks) != 2 {
		t.Fatalf("completed outcome was reopened or duplicated: %+v", task)
	}
	assertFindingEvidence(t, e, "T001", "no worker implements it again")
}

func TestReconciliationRefusesStaleRevision(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	e.State.Tip = "tip-2"
	stale := reconciliationFinding("Import records can start")
	stale.Revision = "tip-1"
	applyFinding(t, e, stale)
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" || len(task.Blockers) != 1 {
		t.Fatal("a stale revision changed the task state")
	}
	assertFindingEvidence(t, e, "", "Ignored reconciliation")
}

func TestReconciliationRequiresRevisionAndTarget(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	missing := reconciliationFinding("Import records can start")
	missing.Revision = ""
	applyFinding(t, e, missing)
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("a revision-less reconciliation changed the task state")
	}
	// A transition never creates work: it must name the task it reconciles.
	fresh := reconciliationFinding("Unknown readiness")
	fresh.ExistingTaskID = ""
	fresh.Key = ""
	fresh.Title = "A title no task carries"
	applyFinding(t, e, fresh)
	if len(e.State.Backlog.Tasks) != 2 {
		t.Fatal("a reconciliation without a target created a task")
	}
}

func TestReconciliationRefusesWorkThatHoldsTheContract(t *testing.T) {
	for _, phase := range []string{"running", "verifying", "resume"} {
		t.Run(phase, func(t *testing.T) {
			e, task, _ := reconcileEngine(t)
			before := task.Contract()
			e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "a9", Task: *task, Phase: phase})
			applyFinding(t, e, reconciliationFinding("Import records can start"))
			task = e.State.Backlog.Task("T001")
			if task.Status != "blocked" || task.Contract() != before {
				t.Fatalf("reconciliation moved held work: %+v", task)
			}
			assertFindingEvidence(t, e, "", "Ignored reconciliation")
		})
	}
}

func TestReconciliationRefusesUndoneDependency(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	e.State.Backlog.Task("T002").Status = "todo"
	applyFinding(t, e, reconciliationFinding("Import records can start"))
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("a resume against an undone dependency changed the task state")
	}
	assertFindingEvidence(t, e, "", "dependency T002 is not done")
}

func TestReconciliationProtectsOperatorGate(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	task.Tags = []string{operatorTaskTag}
	task.BlockReason = operatorBlockReason()
	applyFinding(t, e, reconciliationFinding("The gate has no operator action"))
	if task = e.State.Backlog.Task("T001"); task.Status != "blocked" || !task.OperatorOnly() {
		t.Fatalf("an agent finding moved an operator gate: %+v", task)
	}
	decision := decisionTask(e)
	if decision == nil || decision.Status != "blocked" || !decision.OperatorOnly() || len(e.State.Backlog.Ready()) != 0 {
		t.Fatalf("the operator decision is not a protected action: %+v", decision)
	}
	if !hasTaskTag(*decision, findingKeyPrefix+reconciliationKey("T001")) {
		t.Fatal("the decision lacks its stable key")
	}
	if !strings.Contains(decision.Details, "the operator gate stays with the operator") || !strings.Contains(decision.Details, "looper cancel T001") {
		t.Fatal("the decision names neither the refusal nor the action")
	}
	// Five reports, one decision — the Ohya T109 shape.
	for i := 0; i < 4; i++ {
		applyFinding(t, e, reconciliationFinding("The gate has no operator action"))
	}
	if count := len(e.State.Backlog.Tasks); count != 3 {
		t.Fatalf("repeated refusals created %d tasks", count)
	}
	assertFindingEvidence(t, e, decision.ID, "still needs the recorded decision")
}

func TestReconciliationProtectsDeferralGate(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	deferTask(task, "The round for imports is scheduled.")
	applyFinding(t, e, reconciliationFinding("The deferred condition holds"))
	if task = e.State.Backlog.Task("T001"); !task.Deferred() || task.Status != "blocked" {
		t.Fatalf("an agent finding removed a deferral: %+v", task)
	}
	if decision := decisionTask(e); decision == nil || !strings.Contains(decision.Details, "looper:deferred") {
		t.Fatal("the deferral decision is missing its action")
	}
}

func TestReconciliationEscalatesOwnershipCorrection(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	proposal := reconciliationFinding("The task is agent work")
	proposal.Transition = transitionReassign
	applyFinding(t, e, proposal)
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("an unapproved ownership correction changed the task")
	}
	decision := decisionTask(e)
	if decision == nil || !strings.Contains(decision.Details, "an ownership change needs the operator") {
		t.Fatal("the ownership correction was not escalated")
	}
	if task := e.State.Backlog.Task("T001"); task.OperatorOnly() || task.Deferred() {
		t.Fatal("the escalation relabelled the software task it blocks")
	}
}

// A decision task is the operator's action itself: a report against one
// records evidence and never escalates a decision about the decision.
func TestReconciliationDoesNotChainDecisions(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	task := e.State.Backlog.Task("T001")
	task.Tags = []string{operatorTaskTag}
	applyFinding(t, e, reconciliationFinding("The gate has no operator action"))
	decision := decisionTask(e)
	if decision == nil {
		t.Fatal("the operator decision is missing")
	}
	for i := 0; i < 3; i++ {
		applyFinding(t, e, Finding{Title: "The decision was made", Details: "The operator acted.", Evidence: "the log records it",
			Owner: "agent", Key: "decision-made", ExistingTaskID: decision.ID, Transition: transitionComplete, Revision: "tip-1", Priority: 1})
	}
	if len(e.State.Backlog.Tasks) != 3 {
		t.Fatalf("reports against the decision created more tasks: %d", len(e.State.Backlog.Tasks))
	}
	if decision.Status != "blocked" || !decision.OperatorOnly() {
		t.Fatalf("an agent finding decided the operator's task: %+v", decision)
	}
	assertFindingEvidence(t, e, decision.ID, "the decision records its own outcome")
}

func TestReconciliationEscalatesCircularCondition(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	task.Blockers = []string{"T001 waits for T001 to start implementation and lose its deferred tag"}
	applyFinding(t, e, reconciliationFinding("The task can start"))
	if task = e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("a self-referential condition was reconciled by the engine")
	}
	decision := decisionTask(e)
	if decision == nil || !strings.Contains(decision.Details, "names task T001 itself") {
		t.Fatal("the circular condition was not escalated with its reason")
	}
	for i := 0; i < 3; i++ {
		applyFinding(t, e, reconciliationFinding("The task can start"))
	}
	if len(e.State.Backlog.Tasks) != 3 {
		t.Fatal("repeated circular findings created another decision")
	}
}

// The Charlie T040 shape (delivery audit 2026-11-17): the condition names no
// task at all and still waits on the task's own work. The guard escalates
// instead of resuming, with a reason that names the wording, and repeated
// reports add evidence to the one decision.
func TestReconciliationEscalatesIdentifierFreeCircularCondition(t *testing.T) {
	e, task, _ := reconcileEngine(t)
	task.Title = "Auto-save system working per section 18"
	task.Blockers = []string{"Waits for save work to resume, or for dependent death-reload work to land"}
	applyFinding(t, e, reconciliationFinding("The task can start"))
	if task = e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("an identifier-free circular condition was reconciled by the engine")
	}
	decision := decisionTask(e)
	if decision == nil || !strings.Contains(decision.Details, "requires task T001's own work") {
		t.Fatal("the identifier-free circular condition was not escalated with its reason")
	}
	for i := 0; i < 3; i++ {
		applyFinding(t, e, reconciliationFinding("The task can start"))
	}
	if len(e.State.Backlog.Tasks) != 3 {
		t.Fatal("repeated identifier-free circular findings created another decision")
	}
	assertFindingEvidence(t, e, decision.ID, "still needs the recorded decision")
}

func TestRepeatedPlainFindingEscalatesOneDecision(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	plain := Finding{Title: "Import readiness holds", Details: "The condition is satisfied.", Evidence: "review one",
		Owner: "agent", Key: "import-readiness", ExistingTaskID: "T001", Priority: 1}
	applyFinding(t, e, plain)
	if decisionTask(e) != nil {
		t.Fatal("a first report escalated before it repeated")
	}
	plain.Evidence = "review two"
	applyFinding(t, e, plain)
	decision := decisionTask(e)
	if decision == nil || decision.Status != "blocked" || !decision.OperatorOnly() {
		t.Fatal("a repeated no-change finding produced no operator decision")
	}
	if !strings.Contains(decision.Details, "repeated findings change no state") {
		t.Fatal("the decision does not name the repeated condition")
	}
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" {
		t.Fatal("the escalation moved the blocked task")
	}
	for i := 0; i < 5; i++ {
		plain.Evidence = "review three"
		applyFinding(t, e, plain)
	}
	if len(e.State.Backlog.Tasks) != 3 {
		t.Fatal("repeated findings created another decision")
	}
	assertFindingEvidence(t, e, "T001", "Updated existing finding")
}

func TestRepeatedOperatorFindingKeepsTheGateAsItsOwnAction(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	plain := Finding{Title: "Grant import access", Details: "The operator supplies access.", Evidence: "401",
		Owner: "operator", Key: "import-access", ExistingTaskID: "T001", Priority: 1}
	applyFinding(t, e, plain)
	applyFinding(t, e, plain)
	if len(e.State.Backlog.Tasks) != 2 {
		t.Fatal("an operator prerequisite escalated into a second decision task")
	}
	assertFindingEvidence(t, e, "T001", "401")
}

func TestReconciliationInvalidTransitionIsRejected(t *testing.T) {
	e, _, _ := reconcileEngine(t)
	bad := reconciliationFinding("Import records can start")
	bad.Transition = "reopen"
	applyFinding(t, e, bad)
	if task := e.State.Backlog.Task("T001"); task.Status != "blocked" || len(e.State.Backlog.Tasks) != 2 {
		t.Fatal("an unknown transition changed recorded state")
	}
	assertFindingEvidence(t, e, "", "transition must be resume, complete, or reassign")
}

// The full review path: the prompted agent reports a transition, and the
// accepted review applies it against the revision the prompt named.
func TestReviewReconciliationRunsThroughTheReviewAgent(t *testing.T) {
	blocked := Task{ID: "T2", Title: "two", Priority: 1, Status: "blocked",
		Blockers: []string{"missing input"}, UnblockWhen: "Input arrives; set this task to todo."}
	e, _ := reviewEngine(t, reviewIncomplete, nil, doneBacklog(blocked))
	result := fmt.Sprintf(`{"status":"reviewed","summary":"checked","findings":[{"title":"Input arrived","details":"The condition holds.","evidence":"the input file is present","depends_on":[],"priority":1,"owner":"agent","key":"input-arrived","existing_task_id":"T2","transition":"resume","revision":%q}],"complete":false}`, e.State.Tip)
	t.Setenv("LOOPER_FAKE_REVIEW", result)
	if err := e.review(context.Background(), false, reviewScope{Mode: reviewScopeFull}); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T2")
	if task == nil || task.Status != "todo" || len(task.Blockers) != 0 || task.UnblockWhen != "" {
		t.Fatalf("the accepted review did not resume the blocked task: %+v", task)
	}
	if !hasTaskTag(*task, findingKeyPrefix+"input-arrived") {
		t.Fatal("the reconciliation lost its finding identity")
	}
	assertFindingEvidence(t, e, "T2", "Reconciled resume")
}

func TestReconciliationStagesUntilTheExportCommits(t *testing.T) {
	e := guardEngine(t)
	blocked := Task{ID: "T2", Title: "stuck", Priority: 1, Status: "blocked", Blockers: []string{"no credentials"}}
	e.State.Backlog.Tasks[1] = blocked
	e.State.Tip = "tip-1"
	a := followupAttempt(e)
	// The reporting worker assembled its worktree from the accepted revision,
	// so its reconciliation stages (T139) instead of being refused.
	a.Base = "tip-1"
	a.Summary.Followups = []Finding{{
		Title: "Credentials are available", Details: "The condition holds.", Evidence: "the vault answers",
		Owner: "agent", Key: "credentials", ExistingTaskID: "T2", Transition: transitionResume, Revision: "tip-1", Priority: 1,
	}}
	if err := e.applyResult(a); err == nil || !strings.Contains(err.Error(), "changed outside Looper") {
		t.Fatalf("expected the export guard, got %v", err)
	}
	if task := e.State.Backlog.Task("T2"); task.Status != "blocked" || len(task.Blockers) != 1 {
		t.Fatalf("a failed commit kept a staged resume: %+v", task)
	}
	if reconciled := messages(journal(t, e), "finding", "Reconciled resume"); len(reconciled) != 0 {
		t.Fatalf("an uncommitted resume reported success: %q", reconciled)
	}
}

// The audit's verification fixture: a blocked task whose dependency is now
// done, a review that proposes resumption, and a committed review outcome that
// makes the task schedulable through the permitted transition.
func TestReviewReconciliationResumesThroughCommittedReview(t *testing.T) {
	e, _ := exportTestEngine(t)
	e.State.Tip = "tip-1"
	e.State.Integrated = 2
	e.State.ReviewedAt = 1
	t3 := e.State.Backlog.Task("T3")
	t3.Status = "blocked"
	t3.Blockers = []string{"Task T2 must integrate first."}
	t3.UnblockWhen = "T2 is done; set this task to todo."
	e.State.Backlog.Task("T2").Status = "done"
	result := reviewResult{Status: "reviewed", Summary: "checked", Complete: false, Findings: []Finding{{
		Title: "T3 can start", Details: "Its dependency integrated.", Evidence: "T2 result accepted at tip-1",
		Owner: "agent", Key: "t3-readiness", ExistingTaskID: "T3", Transition: transitionResume, Revision: "tip-1", Priority: 1,
	}}}
	if err := e.applyReview(result, false, "review-1"); err != nil {
		t.Fatal(err)
	}
	if t3 = e.State.Backlog.Task("T3"); t3.Status != "todo" || len(t3.Blockers) != 0 || t3.UnblockWhen != "" {
		t.Fatalf("the committed review did not resume the task: %+v", t3)
	}
	ready := e.State.Backlog.Ready()
	if len(ready) != 2 || ready[0].ID != "T3" && ready[1].ID != "T3" {
		t.Fatalf("the resumed task is not schedulable: %+v", ready)
	}
	if reconciled := messages(journal(t, e), "finding", "Reconciled resume"); len(reconciled) != 1 {
		t.Fatalf("the committed resume has no outcome event: %q", reconciled)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if task := saved.Backlog.Task("T3"); task == nil || task.Status != "todo" {
		t.Fatal("the resume did not reach the saved state")
	}
}
