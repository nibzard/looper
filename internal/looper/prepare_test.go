package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// conflictingResultsEngine seeds two finished workers that both edited one
// shared file, so integrating the second conflicts with the first. The verify
// command sleeps long enough that several loop ticks pass while its check runs.
func conflictingResultsEngine(t *testing.T, verify string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, "")
	t.Setenv("LOOPER_FAKE_REVIEW", `{"status":"reviewed","summary":"checked","findings":[],"complete":true}`)
	c := repoConfig(t, "run", "--jobs", "2", "--check-jobs", "2")
	c.CodexBin, c.ClaudeBin = bin, bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "first", Priority: 1, Status: "doing"},
		{ID: "T002", Title: "second", Priority: 1, Status: "doing"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	// No milestone or final review: this test measures prepare retries, and a
	// review pass would consult an agent about an unrelated question.
	e.Config.ReviewEvery = 0
	e.State.ReviewedTip = e.State.Tip
	e.State.ReviewedAt = 1
	e.State.Plan = &Plan{Verify: verify, Tasks: []Scope{
		{TaskID: "T001", Paths: []string{"shared.txt"}},
		{TaskID: "T002", Paths: []string{"shared.txt"}},
	}}
	contracts := []string{}
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	// Two finished results over the same file with different contents.
	base := e.State.Tip
	for _, id := range []string{"T001", "T002"} {
		work, err := e.Repo.Workspace("task-"+strings.ToLower(id), base, e.State.Backlog)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, "shared.txt"), []byte(id+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
		tree, err := e.Repo.Snapshot(work)
		if err != nil {
			t.Fatal(err)
		}
		commit, err := e.Repo.Commit(tree, []string{base}, "Finish "+id)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.Dir, "task-"+strings.ToLower(id))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		e.State.Attempts = append(e.State.Attempts, &Attempt{
			ID: "task-" + strings.ToLower(id), Task: *e.State.Backlog.Task(id), Agent: "codex",
			Base: base, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/task-" + strings.ToLower(id),
			Phase: "ready", ResultCommit: commit, Worktrees: []string{work},
			Summary: &Summary{TaskID: id, Status: "done", Summary: "edited the shared file", Files: []string{"shared.txt"}, Blockers: []string{}},
		})
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, c
}

// While a check is in flight, a head that conflicts with the integrating
// chain waits instead of repeating a full prepare on every loop tick.
func TestConflictingHeadPreparesOncePerCompletedCheck(t *testing.T) {
	clearSettingEnv(t)
	e, _ := conflictingResultsEngine(t, "sleep 1")
	// The window must stay long enough for the first check to complete and
	// the deferred head to reach its decision; both are fsync-bound, so it
	// scales with host pressure. Once the retried head is blocked it stays
	// decided, so a longer window cannot add defers.
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(10*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 130 || err != context.DeadlineExceeded {
		t.Fatalf("engine exited %d (%v); want a live loop cancelled at the deadline", code, err)
	}
	events := storedEvents(t, e)
	defers, checks := 0, 0
	for _, ev := range events {
		switch ev.Kind {
		case "defer":
			defers++
		case "check":
			checks++
		}
	}
	// The first check runs about twenty loop ticks; an unthrottled retry
	// would defer once per tick. A second check slot is what lets the run
	// attempt the conflicting head while the first check is still in flight.
	if defers != 1 {
		t.Fatalf("a conflicting head must defer exactly once per completed check, got %d defers over %d checks", defers, checks)
	}
	if checks < 1 {
		t.Fatal("the fixture must run at least one check")
	}
	// After that check completed, the chain base moved and the deferred head
	// was prepared exactly once more; the still-conflicting result then
	// reaches a decision instead of waiting deferred forever.
	second := e.State.Attempts[1]
	if second.Phase != "blocked" || !strings.Contains(second.Feedback, "conflicts") {
		t.Fatalf("the retried head must reach a decision, got phase %s: %q", second.Phase, second.Feedback)
	}
	if !deferredFor(events, "T002") {
		t.Fatal("the conflicting head must be T002")
	}
}

func deferredFor(events []Event, taskID string) bool {
	for _, ev := range events {
		if ev.Kind == "defer" && strings.HasPrefix(ev.Message, taskID) {
			return true
		}
	}
	return false
}
