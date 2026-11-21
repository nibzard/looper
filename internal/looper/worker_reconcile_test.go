package looper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Worker reconciliation (T139): a worker summary's follow-ups may carry the
// same transition fields a review reports. The worker cannot read the
// accepted revision mid-flight, so its prompt names the base its worktree
// assembled from and the guard accepts that base while the accepted revision
// is still a tree the attempt itself verified.

// workerReconcileEngine returns an engine holding one blocked task beside the
// task a worker reports from, plus the base revision that worker's worktree
// assembled from.
func workerReconcileEngine(t *testing.T) (*Engine, *Task, string) {
	t.Helper()
	e, _ := exportTestEngine(t)
	e.State.Finished = false
	e.State.Tip = "base-1"
	e.State.Backlog.Tasks = []Task{
		{ID: "T1", Title: "Load records", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "Import records", Details: "Load every record once.", Priority: 1, Status: "blocked",
			Blockers: []string{"The loader lands first."}, UnblockWhen: "The loader lands; set this task to todo."},
	}
	return e, e.State.Backlog.Task("T2"), "base-1"
}

// workerAttempt stages one finished worker on the engine: its worktree
// assembled from base, and its summary reports one reconciliation follow-up.
func workerAttempt(e *Engine, base string, f Finding) *Attempt {
	a := &Attempt{ID: "task-1", Task: e.State.Backlog.Tasks[0], Base: base, Directory: e.Config.Root}
	e.State.Attempts = append(e.State.Attempts, a)
	a.Summary = &Summary{TaskID: a.Task.ID, Status: "done", Summary: "work complete", Files: []string{}, Blockers: []string{}, Followups: []Finding{f}}
	return a
}

func workerReconciliation(title, revision string) Finding {
	return Finding{Title: title, Details: "The condition holds.", Evidence: "the loader landed", Owner: "agent",
		Key: "import-readiness", ExistingTaskID: "T2", Transition: transitionResume, Revision: revision, Priority: 1}
}

// A blocked report applies while the controller still sits on the base the
// worker read, which is the revision the worker could name.
func TestWorkerFollowupReconcilesFromItsOwnBase(t *testing.T) {
	e, task, base := workerReconcileEngine(t)
	before := task.Contract()
	a := workerAttempt(e, base, workerReconciliation("Import records can start", base))
	a.Summary.Status = "blocked"
	a.Summary.Blockers = []string{"waiting on the loader"}
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	task = e.State.Backlog.Task("T2")
	if task.Status != "todo" || len(task.Blockers) != 0 || task.UnblockWhen != "" || task.BlockReason != nil {
		t.Fatalf("the worker report did not resume the blocked task: %+v", task)
	}
	if ready := e.State.Backlog.Ready(); len(ready) != 1 || ready[0].ID != "T2" {
		t.Fatalf("the resumed task is not schedulable: %+v", ready)
	}
	if reporting := e.State.Backlog.Task("T1"); reporting.Status != "blocked" || len(reporting.Blockers) != 1 {
		t.Fatalf("the reporting task did not keep its blocked outcome: %+v", reporting)
	}
	if task.Contract() != before {
		t.Fatal("the worker reconciliation changed the task contract")
	}
	assertFindingEvidence(t, e, "T2", "Reconciled resume")
}

// Completed work is the stronger case: the controller accepted the worker's
// own verification commit, whose tree is the base plus that worker's verified
// changes, so evidence gathered on the base still holds.
func TestWorkerFollowupReconcilesAfterItsOwnVerification(t *testing.T) {
	e, _, base := workerReconcileEngine(t)
	a := workerAttempt(e, base, workerReconciliation("Import records can start", base))
	a.Verification = &Candidate{Base: base, Commit: "verify-1", OK: true}
	e.State.Tip = "verify-1"
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if task := e.State.Backlog.Task("T2"); task.Status != "todo" || len(task.Blockers) != 0 {
		t.Fatalf("the verified worker report did not resume the blocked task: %+v", task)
	}
	assertFindingEvidence(t, e, "T2", "Reconciled resume")
}

// A verification that merged another attempt's work first changed the tree
// beyond what this worker read, so its evidence is stale exactly as a review
// at an older revision is: the report is refused with the teaching note.
func TestWorkerFollowupRefusesABaselineItNeverRead(t *testing.T) {
	e, _, base := workerReconcileEngine(t)
	a := workerAttempt(e, base, workerReconciliation("Import records can start", base))
	a.Verification = &Candidate{Base: "coworker-1", Commit: "verify-1", OK: true}
	e.State.Tip = "verify-1"
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if task := e.State.Backlog.Task("T2"); task.Status != "blocked" || len(task.Blockers) != 1 {
		t.Fatalf("a reconciliation past unread work changed the task: %+v", task)
	}
	assertFindingEvidence(t, e, "", "the accepted revision holds work this attempt never read")
}

// A revision the worker could not have read — the accepted revision itself,
// or a stale value — is refused. The refusal keeps the T124 guard for
// everything that is not the attempt's own verified revision.
func TestWorkerFollowupRefusesARevisionItCannotKnow(t *testing.T) {
	for _, revision := range []string{"verify-1", "older-1"} {
		t.Run(revision, func(t *testing.T) {
			e, _, base := workerReconcileEngine(t)
			a := workerAttempt(e, base, workerReconciliation("Import records can start", revision))
			a.Verification = &Candidate{Base: base, Commit: "verify-1", OK: true}
			e.State.Tip = "verify-1"
			if err := e.applyResult(a); err != nil {
				t.Fatal(err)
			}
			if task := e.State.Backlog.Task("T2"); task.Status != "blocked" {
				t.Fatalf("an unknowable revision changed the task: %+v", task)
			}
			assertFindingEvidence(t, e, "", "Ignored reconciliation")
		})
	}
}

// An attempt that records no base — a synthetic hold or a legacy retained
// result — cannot vouch for any revision, so its reconciliation is refused.
func TestWorkerFollowupWithoutABaseIsRefused(t *testing.T) {
	e, _, _ := workerReconcileEngine(t)
	a := workerAttempt(e, "", workerReconciliation("Import records can start", e.State.Tip))
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if task := e.State.Backlog.Task("T2"); task.Status != "blocked" {
		t.Fatalf("a baseless attempt reconciled recorded work: %+v", task)
	}
	assertFindingEvidence(t, e, "", "Ignored reconciliation")
}

// The review path keeps its own guard: only the accepted revision the review
// prompt named counts, never a worker base.
func TestReviewReconciliationKeepsTheAcceptedRevisionGuard(t *testing.T) {
	e, _, base := workerReconcileEngine(t)
	e.State.Tip = "tip-2"
	f := workerReconciliation("Import records can start", base)
	s := e.beginFindingStage()
	s.add(f)
	s.emit()
	if task := e.State.Backlog.Task("T2"); task.Status != "blocked" {
		t.Fatalf("a review revision outside the accepted tip changed the task: %+v", task)
	}
	assertFindingEvidence(t, e, "", "Ignored reconciliation")
}

// The summary schema is the structured-output contract a worker's adapter
// enforces; the two fields must be legal there or no worker could report
// them, whatever the prompt teaches.
func TestSummarySchemaAdmitsReconciliationFields(t *testing.T) {
	var document struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(summarySchema), &document); err != nil {
		t.Fatal(err)
	}
	var followups struct {
		Items struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"items"`
	}
	if err := json.Unmarshal(document.Properties["followups"], &followups); err != nil {
		t.Fatal(err)
	}
	var transition struct {
		Enum []string `json:"enum"`
	}
	if raw, ok := followups.Items.Properties["transition"]; !ok || json.Unmarshal(raw, &transition) != nil {
		t.Fatalf("transition is not a defined follow-up property: %v", followups.Items.Properties)
	}
	want := []string{transitionResume, transitionComplete, transitionReassign}
	if len(transition.Enum) != len(want) {
		t.Fatalf("transition is not the enum of the three transitions: %v", transition.Enum)
	}
	for _, expected := range want {
		found := false
		for _, got := range transition.Enum {
			if got == expected {
				found = true
			}
		}
		if !found {
			t.Fatalf("transition enum lacks %q: %v", expected, transition.Enum)
		}
	}
	var revision struct {
		Type string `json:"type"`
	}
	if raw, ok := followups.Items.Properties["revision"]; !ok || json.Unmarshal(raw, &revision) != nil || revision.Type != "string" {
		t.Fatalf("revision is not a free-form string property: %v", followups.Items.Properties)
	}
	// Both stay optional: an ordinary summary without them must keep the
	// required list it always had.
	for _, key := range []string{"transition", "revision"} {
		for _, required := range followups.Items.Required {
			if key == required {
				t.Fatalf("optional reconciliation field %q became required", key)
			}
		}
	}
	for _, required := range []string{"title", "details", "evidence", "depends_on", "priority", "owner", "key", "existing_task_id", "defer_until"} {
		if _, ok := followups.Items.Properties[required]; !ok {
			t.Fatalf("required property %q lost its schema", required)
		}
	}
}

// The prompt teaches the fields and names the base revision inside the
// 4 KiB budget the long-goal scenario pins: the teaching must fit without
// weakening that guard.
func TestWorkerPromptTeachesReconciliationInsideTheBudget(t *testing.T) {
	task := Task{ID: "T1", Title: "one", Status: "todo"}
	goal := strings.Repeat("Keep every mode playable. ", 18) + "The final score target is 987654321098765432109876543210. Extended play rules follow."
	base := strings.Repeat("ab", 20)
	prompt := workerPrompt(Backlog{Goal: goal}, task, agentContext{Backlog: "backlog.json"}, base)
	for _, phrase := range []string{
		"existing_task_id, transition, revision",
		`transition "resume", "complete", or "reassign"`,
		"revision " + base + " (this worktree's base revision)",
	} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("worker prompt lost the reconciliation teaching: %q\n%s", phrase, prompt)
		}
	}
	if len(prompt) > 4<<10 {
		t.Fatalf("the teaching broke the worker prompt budget: %d bytes", len(prompt))
	}
}

// reconcileAgentStub patches the fake agent so it copies the base revision
// out of its own prompt — exactly what a real worker does — and reports it on
// the reconciliation follow-up where the summary carries @BASE@. A worker
// prompt for any task but the first also substitutes @TASK@, so the resumed
// task finishes under its own report instead of the reconciling one.
func reconcileAgentStub(t *testing.T, e *Engine) {
	t.Helper()
	bin := e.Config.CodexBin
	if bin == "" {
		bin = e.Config.ClaudeBin
	}
	script, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	patch := `tid="$(printf '%s' "$p" | head -1 | sed -E 's/^Complete task ([^:]+):.*/\1/')"
if [ "$kind" = worker ]; then
  : > "$tid.txt"
  if [ -n "$LOOPER_FAKE_OTHER" ] && [ "$tid" != "T1" ]; then result="$LOOPER_FAKE_OTHER"; fi
  base="$(printf '%s' "$p" | grep -oE 'revision [0-9a-f]{40,} \(this worktree' | head -1 | cut -d' ' -f2)"
  if [ -n "$base" ]; then result="${result//@BASE@/$base}"; fi
  result="${result//@TASK@/$tid}"
fi
`
	if !bytes.Contains(script, []byte(`if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi`)) {
		t.Fatal("fake agent lost its result write")
	}
	script = bytes.Replace(script, []byte(`if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi`), []byte(patch+`if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi`), 1)
	if err := os.WriteFile(bin, script, 0755); err != nil {
		t.Fatal(err)
	}
}

// The full worker path: the prompted agent reads the base revision out of its
// prompt, reports the transition on its summary, and the applied result
// resumes the blocked task — no review involved.
func TestWorkerReconciliationRunsThroughTheWorkerAgent(t *testing.T) {
	e, _ := reviewEngine(t, reviewComplete, nil, Backlog{Version: 1, Sources: []string{}, Goal: "complete one task", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "blocked", Blockers: []string{"missing input"}, UnblockWhen: "Input arrives; set this task to todo."},
	}})
	reconcileAgentStub(t, e)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"@TASK@","status":"done","summary":"implemented","files":["T1.txt"],"blockers":[],"followups":[{"title":"Input arrived","details":"The condition holds.","evidence":"the input file is present","depends_on":[],"priority":1,"owner":"agent","key":"input-arrived","existing_task_id":"T2","transition":"resume","revision":"@BASE@"}],"unblock_when":""}`)
	t.Setenv("LOOPER_FAKE_OTHER", `{"task_id":"@TASK@","status":"done","summary":"implemented","files":["T2.txt"],"blockers":[],"followups":[],"unblock_when":""}`)
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"test -f T1.txt","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]},{"task_id":"T2","write_paths":["."],"resources":[]}]}`)
	e.Config.Once = true
	ctx, cancel := runCtx(t, 30*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if err != nil || code != 0 {
		t.Fatalf("run failed: code %d, %v", err, code)
	}
	if _, err := os.Stat(filepath.Join(e.Config.Root, "T1.txt")); err != nil {
		t.Fatalf("the verified worker change was not published: %v", err)
	}
	task := e.State.Backlog.Task("T2")
	if task == nil || task.Status != "done" || len(task.Blockers) != 0 || task.UnblockWhen != "" {
		t.Fatalf("the resumed task did not run to completion: %+v", task)
	}
	if !hasTaskTag(*task, findingKeyPrefix+"input-arrived") {
		t.Fatal("the reconciliation lost its finding identity")
	}
	if reconciled := messages(journal(t, e), "finding", "Reconciled resume"); len(reconciled) != 1 {
		t.Fatalf("the worker reconciliation has no outcome event: %q", reconciled)
	}
	// The review that follows reports nothing, so the transition stayed the
	// worker's own act, not a review's.
	if reports := messages(journal(t, e), "finding", "Ignored reconciliation"); len(reports) != 0 {
		t.Fatalf("the worker reconciliation was refused: %q", reports)
	}
}

// Recovery replays a retained result: the saved attempt keeps its base and
// verification, so the replayed reconciliation re-applies against the same
// guard instead of duplicating state.
func TestWorkerReconciliationReplaysFromRetainedState(t *testing.T) {
	e, _, base := workerReconcileEngine(t)
	a := workerAttempt(e, base, workerReconciliation("Import records can start", base))
	a.Verification = &Candidate{Base: base, Commit: "verify-1", OK: true}
	e.State.Tip = "verify-1"
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if task := e.State.Backlog.Task("T2"); task.Status != "todo" || len(task.Blockers) != 0 {
		t.Fatalf("the replayed reconciliation changed the resumed task: %+v", task)
	}
	if reconciled := messages(journal(t, e), "finding", "Reconciled resume"); len(reconciled) != 1 {
		t.Fatalf("the replay reported a second resume: %q", reconciled)
	}
	if confirmed := messages(journal(t, e), "finding", "already schedulable"); len(confirmed) != 1 {
		t.Fatalf("the replay did not record its evidence: %q", confirmed)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if resumed := saved.Backlog.Task("T2"); resumed == nil || resumed.Status != "todo" {
		t.Fatalf("the resume did not reach the saved state: %+v", resumed)
	}
}
