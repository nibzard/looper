package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Review cost control (T128): a due review whose inputs did not move does
// not re-run the agent. The fixtures mirror the audit cases: the Looper
// self-run's full review after a notes-only edit (event 17808), and the
// repeated readiness findings against blocked tasks (Ohya T093, Charlie
// T077) whose reconciliation T124 landed.

// costEngine returns an engine at a recorded review coverage: one applied
// review read the published tree, reported the given verdict, and saw the
// imports recorded on the state. The fake review agent stays unset, so any
// agent pass answers "{}" and fails its report — a cycle that must not reach
// the agent proves it by returning without error.
func costEngine(t *testing.T, backlog Backlog, complete bool) *Engine {
	t.Helper()
	e, _ := reviewEngine(t, "", nil, backlog)
	e.State.ReviewedTree = e.State.Published
	e.State.ReviewedComplete = complete
	e.State.ReviewedImports = appliedImportCount(e.State)
	e.State.Integrated, e.State.ReviewedAt = 3, 3
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e
}

// notesEdit changes one file outside the backlog's source coverage and moves
// the run baseline the way an idle refresh does, so the next review cycle
// sees a published tree that differs from the recorded one.
func notesEdit(t *testing.T, e *Engine, note string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.Config.Root, "NOTES.md"), []byte("operator notes: "+note+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshIdle(); err != nil {
		t.Fatal(err)
	}
}

// taskFileEdit imports an external task-file edit at an identical tree: the
// tip moves, no project file does.
func taskFileEdit(t *testing.T, e *Engine, mutate func(*Backlog)) {
	t.Helper()
	b := clone(e.State.Backlog)
	mutate(&b)
	if err := writeJSON(e.Config.Todo, b); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshIdle(); err != nil {
		t.Fatal(err)
	}
}

func agentPasses(t *testing.T, log string) int {
	t.Helper()
	data, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}

// reviewPrompt returns the newest saved review prompt, so a test can prove
// which pass ran and what it was scoped to.
func reviewPrompt(t *testing.T, e *Engine) string {
	t.Helper()
	entries, err := os.ReadDir(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		if !strings.HasPrefix(name, "review-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(e.Dir, name, "prompt.txt"))
		if err == nil {
			return string(data)
		}
	}
	return ""
}

// The audit's unchanged-revision case: a task-file edit that moves no project
// file must not buy another full review. The cycle settles locally.
func TestUnchangedRevisionSkipsTheReviewPass(t *testing.T) {
	log := agentLogPath(t)
	e := costEngine(t, doneBacklog(), true)
	before := e.State.Tip
	taskFileEdit(t, e, func(b *Backlog) { b.Tasks[0].Title = "one, renamed by the operator" })
	if e.State.ReviewedTip != "" || e.State.Tip == before || e.State.Published != e.State.ReviewedTree {
		t.Fatalf("the fixture must move the tip without moving the tree: %+v", e.State)
	}
	e.State.ReviewPending = true
	if err := e.reviewCycle(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := agentPasses(t, log); n != 0 {
		t.Fatalf("an unchanged revision ran %d agent pass(es)", n)
	}
	if e.State.ReviewPending || e.State.ReviewedTip != e.State.Tip || e.State.ReviewedAt != e.State.Integrated {
		t.Fatalf("the skipped cycle did not settle the review bookkeeping: %+v", e.State)
	}
	if !e.State.Finished {
		t.Fatal("a complete recorded verdict at unchanged content must re-affirm completion")
	}
	events := journal(t, e)
	if got := messages(events, "review", "Skipped the review pass"); len(got) != 1 {
		t.Fatalf("the skip has no journal note: %q", got)
	}
	if got := messages(events, "review", "unchanged since the review at"); len(got) != 1 {
		t.Fatalf("the skip note does not name the recorded review: %q", got)
	}
}

// The same unresolved finding at an unchanged revision escalates exactly one
// stable operator decision, and later cycles change nothing.
func TestUnchangedCycleEscalatesOneStableDecision(t *testing.T) {
	blocked := Task{ID: "T2", Title: "Import records", Priority: 1, Status: "blocked",
		Blockers: []string{"The loader lands first."}, UnblockWhen: "The loader lands; set this task to todo.",
		Tags: []string{findingKeyPrefix + "import-readiness"}}
	e := costEngine(t, doneBacklog(blocked), false)
	for cycle := 0; cycle < 3; cycle++ {
		note := fmt.Sprintf("operator note %d", cycle)
		taskFileEdit(t, e, func(b *Backlog) { b.Tasks[0].Details = note })
		e.State.ReviewPending = true
		if err := e.reviewCycle(context.Background(), true); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(e.State.Backlog.Tasks); got != 3 {
		t.Fatalf("repeated unchanged cycles created %d tasks, want one decision beside two fixtures", got)
	}
	decision := e.State.Backlog.Task("T003")
	if decision == nil || decision.Status != "blocked" || !decision.OperatorOnly() {
		t.Fatalf("the cycle presented no stable operator action: %+v", decision)
	}
	if !hasTaskTag(*decision, findingKeyPrefix+reconciliationKey("T2")) {
		t.Fatal("the decision lacks its stable key")
	}
	if !strings.Contains(decision.Details, "repeated findings change no state") {
		t.Fatalf("the decision does not name the repeated condition: %s", decision.Details)
	}
	if e.State.Finished {
		t.Fatal("an open blocked task finished the goal")
	}
	// The blocked task itself never moved, and the later cycles added no
	// evidence notes: no new report exists.
	if task := e.State.Backlog.Task("T2"); task.Status != "blocked" || len(task.Blockers) != 1 {
		t.Fatalf("the sweep moved the blocked task: %+v", task)
	}
	if notes := messages(journal(t, e), "finding", "Escalated reconciliation of T2"); len(notes) != 1 {
		t.Fatalf("the escalation journaled %d times, want once", len(notes))
	}
}

// A gated task presents its own operator action; the cycle adds no decision
// beside it.
func TestUnchangedCycleKeepsGatesAsTheirOwnAction(t *testing.T) {
	gated := Task{ID: "T2", Title: "Grant import access", Priority: 1, Status: "blocked",
		Blockers:    []string{"An operator must complete this prerequisite; coding workers cannot execute it."},
		UnblockWhen: "Record evidence that the operator completed the action, then mark this task done.",
		Tags:        []string{operatorTaskTag, findingKeyPrefix + "import-access"}}
	e := costEngine(t, doneBacklog(gated), false)
	e.State.ReviewPending = true
	if err := e.reviewCycle(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if got := len(e.State.Backlog.Tasks); got != 2 {
		t.Fatalf("the cycle escalated a gated task into %d tasks", got)
	}
}

// The event-17808 shape: a notes-only edit on an idle complete run receives
// no full review. Coverage permits the skip because no reviewed source file
// changed.
func TestNotesOnlyEditSkipsTheFinalReview(t *testing.T) {
	log := agentLogPath(t)
	backlog := doneBacklog()
	backlog.Sources = []string{"docs/UI-PLAN.md", "internal/looper/run.go"}
	e := costEngine(t, backlog, true)
	notesEdit(t, e, "session record")
	if e.State.ReviewedTip != "" || e.State.Published == e.State.ReviewedTree {
		t.Fatalf("the fixture must move the tree outside coverage: %+v", e.State)
	}
	if err := e.reviewCycle(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if n := agentPasses(t, log); n != 0 {
		t.Fatalf("a notes-only edit ran %d agent pass(es)", n)
	}
	if !e.State.Finished {
		t.Fatal("the recorded completion must survive a change outside coverage")
	}
	if e.State.ReviewedTree != e.State.Published {
		t.Fatal("the skip did not advance the recorded coverage")
	}
	if got := messages(journal(t, e), "review", "outside the reviewed source files"); len(got) != 1 {
		t.Fatalf("the skip note does not name the coverage decision: %q", got)
	}
}

// The same notes-only edit with open work receives a focused pass: the agent
// verifies only the changed paths, and the recorded coverage advances.
func TestNotesOnlyEditWithOpenWorkRunsAFocusedPass(t *testing.T) {
	backlog := doneBacklog(Task{ID: "T2", Title: "two", Priority: 1, Status: "blocked", Blockers: []string{"missing input"}})
	backlog.Sources = []string{"docs/UI-PLAN.md"}
	e := costEngine(t, backlog, false)
	notesEdit(t, e, "session record")
	t.Setenv("LOOPER_FAKE_REVIEW", `{"status":"reviewed","summary":"checked the notes edit","findings":[],"complete":false}`)
	if err := e.reviewCycle(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e.State.ReviewedTree != e.State.Published || e.State.ReviewPending {
		t.Fatalf("the focused pass did not settle the review record: %+v", e.State)
	}
	if e.State.Finished {
		t.Fatal("open work finished the goal")
	}
	prompt := reviewPrompt(t, e)
	if !strings.Contains(prompt, "This pass is focused") || !strings.Contains(prompt, "NOTES.md") {
		t.Fatalf("the focused prompt does not scope the pass to the change")
	}
	if !strings.Contains(prompt, "review pass for Looper") {
		t.Fatal("the focused prompt lost the review identity")
	}
}

// A changed path inside the reviewed source files keeps the full pass.
func TestCoveredChangeKeepsTheFullPass(t *testing.T) {
	backlog := doneBacklog()
	backlog.Sources = []string{"docs/UI-PLAN.md"}
	e := costEngine(t, backlog, true)
	notesEdit(t, e, "session record")
	if err := os.MkdirAll(filepath.Join(e.Config.Root, "docs"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.Config.Root, "docs", "UI-PLAN.md"), []byte("# changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshIdle(); err != nil {
		t.Fatal(err)
	}
	scope := e.reviewScope(true)
	if scope.Mode != reviewScopeFull {
		t.Fatalf("a covered change must keep the full pass: %+v", scope)
	}
	if !strings.Contains(scope.Reason, "reviewed source file changed") {
		t.Fatalf("the full scope does not name the covered change: %+v", scope)
	}
}

// Without source_files the snapshot records no coverage, so every change
// keeps the full pass and nothing is skipped on an empty list.
func TestNoRecordedCoverageKeepsFullPasses(t *testing.T) {
	e := costEngine(t, doneBacklog(), true)
	notesEdit(t, e, "session record")
	if scope := e.reviewScope(true); scope.Mode != reviewScopeFull {
		t.Fatalf("an empty source list must not permit a cheaper pass: %+v", scope)
	}
}

// An import that applied after the recorded review forces the full pass: the
// final review must cover every applied import.
func TestAppliedImportForcesTheFullPass(t *testing.T) {
	backlog := doneBacklog()
	backlog.Sources = []string{"docs/UI-PLAN.md"}
	e := costEngine(t, backlog, true)
	e.State.Imports = append(e.State.Imports, &ImportRecord{RequestID: "import-02", Digests: []string{"d"}, Artifacts: []string{"a"}, Status: ImportApplied, Enqueued: "now", Applied: "now"})
	scope := e.reviewScope(true)
	if scope.Mode != reviewScopeFull {
		t.Fatalf("an unseen import must keep the full pass: %+v", scope)
	}
	if !strings.Contains(scope.Reason, "import(s) applied since the review") {
		t.Fatalf("the full scope does not name the import: %+v", scope)
	}
}

// A state saved before T128 records no coverage, so its first review runs
// the full pass and then records one.
func TestFirstReviewRecordsItsCoverage(t *testing.T) {
	e, _ := reviewEngine(t, reviewComplete, nil, doneBacklog())
	if e.State.ReviewedTree != "" {
		t.Fatal("a fresh engine must start without recorded coverage")
	}
	if scope := e.reviewScope(true); scope.Mode != reviewScopeFull {
		t.Fatalf("without recorded coverage the pass must stay full: %+v", scope)
	}
	if err := e.review(context.Background(), true, reviewScope{Mode: reviewScopeFull}); err != nil {
		t.Fatal(err)
	}
	if e.State.ReviewedTree != e.State.Published || !e.State.ReviewedComplete || e.State.ReviewedImports != 0 {
		t.Fatalf("the applied review did not record its coverage: %+v", e.State)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewedTree != e.State.ReviewedTree {
		t.Fatal("the coverage record did not reach the saved state")
	}
}

// coversSources follows the recorded entries: exact paths, paths below a
// directory entry, and unusable or absent entries cover everything.
func TestCoversSourcesFollowsRecordedEntries(t *testing.T) {
	b := Backlog{Sources: []string{"docs/UI-PLAN.md", "internal/looper"}}
	if b.coversSources([]string{"NOTES.md"}) {
		t.Fatal("an unrelated path counted as covered")
	}
	if !b.coversSources([]string{"NOTES.md", "internal/looper/run.go"}) {
		t.Fatal("a path below a directory entry did not count as covered")
	}
	if !b.coversSources([]string{"docs/UI-PLAN.md"}) {
		t.Fatal("an exact entry did not count as covered")
	}
	patterned := Backlog{Sources: []string{"docs/*.md"}}
	if !patterned.coversSources([]string{"NOTES.md"}) {
		t.Fatal("a patterned entry must keep the full pass")
	}
	if !(Backlog{Sources: []string{}}).coversSources([]string{"NOTES.md"}) {
		t.Fatal("an empty source list must count every change as covered")
	}
}

// The loop-level shape: an idle complete run that keeps receiving notes-only
// edits never launches a review agent, and its review record settles through
// the skips alone.
func TestIdleCompleteRunSkipsReviewsAcrossNotesEdits(t *testing.T) {
	log := agentLogPath(t)
	backlog := doneBacklog()
	backlog.Sources = []string{"docs/UI-PLAN.md"}
	e := costEngine(t, backlog, true)
	for i := 0; i < 3; i++ {
		notesEdit(t, e, fmt.Sprintf("note %d", i))
		e.State.ReviewPending = true
		if err := e.save(); err != nil {
			t.Fatal(err)
		}
		if err := e.reviewCycle(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		if !e.State.Finished {
			t.Fatalf("cycle %d lost the recorded completion", i)
		}
	}
	if n := agentPasses(t, log); n != 0 {
		t.Fatalf("three notes-only edits ran %d agent pass(es)", n)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewedTree != saved.Published || saved.ReviewPending {
		t.Fatalf("the saved review record did not settle: %+v", saved)
	}
	events := journal(t, e)
	if got := len(messages(events, "review", "Skipped the review pass")); got != 3 {
		t.Fatalf("expected three skip notes, got %d", got)
	}
	var lastPayload map[string]any
	for _, ev := range events {
		if ev.Kind == "review" && strings.Contains(ev.Message, "Skipped the review pass") && len(ev.Data) > 0 {
			if err := json.Unmarshal(ev.Data, &lastPayload); err != nil {
				t.Fatalf("the skip note payload does not parse: %v", err)
			}
		}
	}
	if lastPayload == nil {
		t.Fatal("the skip note carries no structured payload")
	}
	if _, ok := lastPayload["reviewed_tree"].(string); !ok {
		t.Fatalf("the skip note does not name the recorded tree: %v", lastPayload)
	}
	if _, ok := lastPayload["changed"].([]any); !ok {
		t.Fatalf("the skip note does not list the changed paths: %v", lastPayload)
	}
}

// The run loop itself: an idle complete run whose working tree receives a
// notes-only edit settles the next final review through the skip, without
// launching the review agent or failing a review.
func TestRunLoopSkipsReviewsAfterNotesEdits(t *testing.T) {
	log := agentLogPath(t)
	backlog := doneBacklog()
	backlog.Sources = []string{"docs/UI-PLAN.md"}
	e, c := reviewEngine(t, "", nil, backlog)
	c.Poll = 100 * time.Millisecond
	e.State.ReviewedTree = e.State.Published
	e.State.ReviewedComplete = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		code, err := e.Run(ctx)
		if code != 130 || !errors.Is(err, context.Canceled) {
			t.Errorf("run ended unexpectedly: %d %v", code, err)
		}
	}()
	waitForEvents(t, e, func(events []Event) bool {
		for _, ev := range events {
			if ev.Kind == "waiting" && strings.Contains(ev.Message, "Waiting for changed requirements") {
				return true
			}
		}
		return false
	})
	if err := os.WriteFile(filepath.Join(c.Root, "NOTES.md"), []byte("run-level notes edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	waitForEvents(t, e, func(events []Event) bool {
		for _, ev := range events {
			if ev.Kind == "review" && strings.Contains(ev.Message, "Skipped the review pass") {
				return true
			}
		}
		return false
	})
	if got := len(messages(journal(t, e), "review-error", "")); got != 0 {
		t.Fatalf("a skipped review still reported %d error(s)", got)
	}
	cancel()
	<-done
	if n := agentPasses(t, log); n != 0 {
		t.Fatalf("the idle run launched %d agent pass(es) for a notes-only edit", n)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Finished || saved.ReviewPending || saved.ReviewedTip != saved.Tip {
		t.Fatalf("the saved run lost its completion across the skip: %+v", saved)
	}
}
