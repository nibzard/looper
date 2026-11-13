package looper

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The audited incident: a worker or review reports a follow-up, the journal
// records "Added follow-up", and the saved state/export protocol then fails.
// The success event must not exist before the change is durable, and no task
// may survive only in memory. Each test injects one failure class.

// exportTestEngine returns an engine with a real store, a real task file,
// and the export hash of the file content currently on disk.
func exportTestEngine(t *testing.T) (*Engine, []byte) {
	t.Helper()
	root := t.TempDir()
	todo := filepath.Join(root, "to-do.json")
	b := testBacklog()
	if err := writeJSON(todo, b); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(filepath.Join(root, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	original, err := os.ReadFile(todo)
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{Config: Config{Root: root, Todo: todo}, Repo: &Repository{Root: root}, Store: s, State: &State{Version: 2, Root: root, Todo: todo, Backlog: b, ExportHash: digest(original)}, Output: io.Discard, Dir: filepath.Join(root, "runtime")}
	return e, original
}

// guardEngine builds an engine whose next export hits the external-edit
// guard: a pending export journal and a task file that matches neither the
// recorded before-state nor the next serialized backlog. The pending journal
// also means the failed export saves nothing, so nothing from the staged
// application reaches the store.
func guardEngine(t *testing.T) *Engine {
	t.Helper()
	e, original := exportTestEngine(t)
	e.State.ExportPending = true
	e.State.ExportBefore = digest(original)
	edited := append([]byte(nil), original...)
	edited = append(edited, "  // external edit\n"...)
	if err := os.WriteFile(e.Config.Todo, edited, 0600); err != nil {
		t.Fatal(err)
	}
	return e
}

func followupAttempt(e *Engine) *Attempt {
	b := e.State.Backlog
	a := &Attempt{ID: "task-1", Task: b.Tasks[0], Directory: e.Config.Root}
	e.State.Attempts = append(e.State.Attempts, a)
	a.Summary = &Summary{TaskID: b.Tasks[0].ID, Status: "done", Summary: "work complete", Files: []string{}, Blockers: []string{}, Followups: []Finding{{Title: "Fix observed behavior", Details: "Check added output", Evidence: "Observed fixture failure", DependsOn: []string{}, Priority: 1}}}
	return a
}

func journal(t *testing.T, e *Engine) []Event {
	t.Helper()
	events, err := e.Store.RecentEvents(-1, 100)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func messages(events []Event, kind, fragment string) []string {
	var found []string
	for _, event := range events {
		if kind != "" && event.Kind != kind {
			continue
		}
		if fragment == "" || strings.Contains(event.Message, fragment) {
			found = append(found, event.Message)
		}
	}
	return found
}

func countTitled(b Backlog, title string) int {
	count := 0
	for _, task := range b.Tasks {
		if task.Title == title {
			count++
		}
	}
	return count
}

// The export guard rejects the application: no success event may exist and
// no in-memory-only follow-up may survive the failed commit.
func TestApplyResultExportGuardFailureStagesNoSuccess(t *testing.T) {
	e := guardEngine(t)
	a := followupAttempt(e)
	err := e.applyResult(a)
	if err == nil || !strings.Contains(err.Error(), "changed outside Looper") {
		t.Fatalf("expected the export guard, got %v", err)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 0 {
		t.Fatalf("uncommitted follow-up reported success: %q", added)
	}
	if notes := messages(journal(t, e), "finding", "Fix observed behavior"); len(notes) != 0 {
		t.Fatalf("uncommitted finding left a journal note: %q", notes)
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 0 {
		t.Fatal("failed commit kept an in-memory-only follow-up task")
	}
	if task := e.State.Backlog.Task("T1"); task.Status != "todo" {
		t.Fatalf("failed commit kept a staged status: %s", task.Status)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Nothing from the staged application may be durable; a store that never
	// held a saved state also proves that.
	if saved != nil {
		if count := countTitled(saved.Backlog, "Fix observed behavior"); count != 0 {
			t.Fatalf("failed commit persisted a staged backlog: %d follow-up tasks", count)
		}
	}
}

// The state save itself fails while the event journal still accepts writes:
// the exact false-success shape of the audited incident. No event, no task.
func TestApplyResultStateSaveFailureLeavesNoFalseSuccess(t *testing.T) {
	e, _ := exportTestEngine(t)
	a := followupAttempt(e)
	// The event journal keeps working; only the state row cannot be saved.
	if _, err := e.Store.db.Exec("DROP TABLE state"); err != nil {
		t.Fatal(err)
	}
	if err := e.applyResult(a); err == nil {
		t.Fatal("a failed state save must fail the application")
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 0 {
		t.Fatalf("uncommitted follow-up reported success: %q", added)
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 0 {
		t.Fatal("failed save kept an in-memory-only follow-up task")
	}
	// The injection split the two store writes, proving the missing success
	// event is the protocol holding back, not a dead journal.
	e.event("probe", "journal alive")
	if probe := messages(journal(t, e), "probe", "journal alive"); len(probe) != 1 {
		t.Fatal("event journal unavailable; injection did not isolate the state save")
	}
}

// The export journal commits the follow-up but the task-file write fails.
// Recovery finishes the pending export and returns exactly one durable
// follow-up task; the journal never claimed the addition.
func TestApplyResultExportWriteFailureRecoversExactlyOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not stop the root user")
	}
	e, _ := exportTestEngine(t)
	a := followupAttempt(e)
	if err := os.Chmod(e.Config.Root, 0555); err != nil {
		t.Fatal(err)
	}
	err := e.applyResult(a)
	if err == nil {
		t.Fatal("a failed task-file write must fail the application")
	}
	if err := os.Chmod(e.Config.Root, 0755); err != nil {
		t.Fatal(err)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 0 {
		t.Fatalf("uncommitted follow-up reported success: %q", added)
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 0 {
		t.Fatal("failed export kept an in-memory-only follow-up task")
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || !saved.ExportPending {
		t.Fatal("the pending export journal was not recorded")
	}
	if count := countTitled(saved.Backlog, "Fix observed behavior"); count != 1 {
		t.Fatalf("saved state holds %d follow-up tasks", count)
	}
	// Recovery: a fresh engine finishes the pending export from saved state.
	e2 := &Engine{Config: e.Config, Repo: e.Repo, Store: e.Store, State: saved, Output: io.Discard}
	if err := e2.export(); err != nil {
		t.Fatal(err)
	}
	file, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		t.Fatal(err)
	}
	if digest(file) != e2.State.ExportHash || e2.State.ExportPending {
		t.Fatal("the pending export did not finish")
	}
	if count := countTitled(e2.State.Backlog, "Fix observed behavior"); count != 1 {
		t.Fatalf("recovered task file holds %d follow-up tasks", count)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 0 {
		t.Fatalf("recovery invented a success event: %q", added)
	}
}

// After a committed application, replaying the retained result keeps exactly
// one durable task and one "Added follow-up" outcome; the repeat records its
// evidence against the existing task instead.
func TestApplyResultFollowupReplayKeepsOneTaskAndOneOutcome(t *testing.T) {
	e, _ := exportTestEngine(t)
	if err := e.applyResult(followupAttempt(e)); err != nil {
		t.Fatal(err)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 1 {
		t.Fatalf("expected exactly one added follow-up event, got %d", len(added))
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 1 {
		t.Fatal("committed follow-up task missing")
	}
	// Recovery replays the same report against the committed backlog.
	if err := e.applyResult(followupAttempt(e)); err != nil {
		t.Fatal(err)
	}
	if count := countTitled(e.State.Backlog, "Fix observed behavior"); count != 1 {
		t.Fatalf("replay duplicated the follow-up: %d tasks", count)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 1 {
		t.Fatalf("replay emitted another success event: %d", len(added))
	}
	if linked := messages(journal(t, e), "finding", "Updated existing finding"); len(linked) == 0 {
		t.Fatal("replayed evidence was not linked to the existing task")
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if count := countTitled(saved.Backlog, "Fix observed behavior"); count != 1 {
		t.Fatalf("saved state holds %d follow-up tasks after replay", count)
	}
}

// The review application stages the same way: a failed export leaves no
// finding or review outcome and no half-applied review state. A committing
// report emits its outcomes only after the export finished.
func TestApplyReviewStagesFindingsUntilCommit(t *testing.T) {
	result := reviewResult{Status: "reviewed", Summary: "checked", Complete: true, Findings: []Finding{{Title: "Fix observed behavior", Details: "Check added output", Evidence: "Observed fixture failure", DependsOn: []string{}, Priority: 1}}}
	e, original := exportTestEngine(t)
	e.State.Integrated = 1
	e.State.ReviewPending = true
	e.State.ExportPending = true
	e.State.ExportBefore = digest(original)
	edited := append([]byte(nil), original...)
	edited = append(edited, "  // external edit\n"...)
	if err := os.WriteFile(e.Config.Todo, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.applyReview(result, true, "review-1"); err == nil || !strings.Contains(err.Error(), "changed outside Looper") {
		t.Fatalf("expected the export guard, got %v", err)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 0 {
		t.Fatalf("uncommitted review finding reported success: %q", added)
	}
	if reviewed := messages(journal(t, e), "review", ""); len(reviewed) != 0 {
		t.Fatalf("uncommitted review reported an outcome: %q", reviewed)
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 0 {
		t.Fatal("failed review commit kept a follow-up task")
	}
	if !e.State.ReviewPending || e.State.ReviewedAt != 0 || e.State.Finished {
		t.Fatal("failed review commit kept staged review state")
	}
	// Restoring the file lets the same report commit; its outcomes appear
	// exactly once, after the export.
	if err := os.WriteFile(e.Config.Todo, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.applyReview(result, true, "review-1"); err != nil {
		t.Fatal(err)
	}
	if added := messages(journal(t, e), "finding", "Added follow-up"); len(added) != 1 {
		t.Fatalf("committed review finding missing its outcome: %d", len(added))
	}
	if reviewed := messages(journal(t, e), "review", "checked"); len(reviewed) != 1 {
		t.Fatalf("committed review missing its outcome: %d", len(reviewed))
	}
	if countTitled(e.State.Backlog, "Fix observed behavior") != 1 {
		t.Fatal("committed review did not add the follow-up task")
	}
	// Open work remains, so the run cannot be finished; the review itself
	// must be recorded as completed.
	if e.State.ReviewPending || e.State.ReviewedAt != e.State.Integrated {
		t.Fatal("committed review state not applied")
	}
}
