package looper

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cancelledHoldState builds the saved state a session cancelled mid-hold
// leaves behind: one in-progress task, an accepted export on disk, and the
// attempt shape the caller describes. The controller closes before the
// fixture returns, so the restart opens the same saved state under a fresh
// lock exactly the way a new controller does.
func cancelledHoldState(t *testing.T, attempt func(e *Engine)) (Config, State) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "hold", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "doing"},
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
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"src/"}}}}
	e.State.PlanSignature = "live"
	if attempt != nil {
		attempt(e)
	}
	saved := clone(*e.State)
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	return c, saved
}

// reopenEngine opens a second controller over a cancelled run's saved state
// without starting its run loop, so a test can drive initialize exactly the
// way startup does.
func reopenEngine(t *testing.T, c Config) (*Engine, *os.File) {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	return e, lock
}

// readyAttempt appends the attempt a cancelled hold leaves for a verified
// result: phase ready, its result commit beside the accepted baseline.
func readyAttempt(e *Engine) {
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-hold", Task: *e.State.Backlog.Task("T1"), Agent: "codex",
		Base: e.State.Tip, ResultCommit: e.State.Tip, Phase: "ready",
		Directory: filepath.Join(e.Dir, "task-hold"), Ref: "refs/looper-tasks/task-hold",
	})
}

// TestStartupImportsEditLeftByCancelledHold reproduces the recorded incident
// shape: a session cancelled mid-hold leaves the edited task file beside the
// old export hash. Startup must import the edit through the held refresh for
// replay-safe retained work instead of demanding a restore.
func TestStartupImportsEditLeftByCancelledHold(t *testing.T) {
	c, saved := cancelledHoldState(t, readyAttempt)
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	e, _ := reopenEngine(t, c)
	if err := e.initialize(context.Background()); err != nil {
		t.Fatalf("startup must import the edit instead of demanding a restore: %v", err)
	}
	if e.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the startup import must accept the edited file")
	}
	if e.State.Backlog.Task("T2") == nil || e.State.Backlog.Task("T3") == nil {
		t.Fatalf("the startup import must accept the edited tasks: %+v", e.State.Backlog.Tasks)
	}
	if e.State.Tip == saved.Tip {
		t.Fatal("the startup import must move the baseline")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit != saved.Attempts[0].ResultCommit {
		t.Fatalf("the retained result must stay queued with its commit: %+v", e.State.Attempts)
	}
	if e.State.Plan == nil || e.State.PlanSignature != "" {
		t.Fatal("the startup import must keep the live plan and clear only its signature")
	}
	if countKindEvent(t, e.Dir, "recovery", "Imported the task-file edit left by a cancelled hold") != 1 {
		t.Fatal("the startup import must record one recovery event")
	}
}

// TestStartupHeldEditStillDemandsRestoreWithoutReplayableWork pins the gate:
// retained work whose result cannot replay keeps the restore demand, because
// the imported baseline would strand that implementation.
func TestStartupHeldEditStillDemandsRestoreWithoutReplayableWork(t *testing.T) {
	c, _ := cancelledHoldState(t, func(e *Engine) {
		// A running attempt with no receipt and no result commit reruns its
		// worker; the edit must not import over it.
		e.State.Attempts = append(e.State.Attempts, &Attempt{
			ID: "task-run", Task: *e.State.Backlog.Task("T1"), Agent: "codex",
			Base: e.State.Tip, Phase: "running", Worktree: filepath.Join(e.Dir, "task-run"),
			Directory: filepath.Join(e.Dir, "task-run"), Ref: "refs/looper-tasks/task-run",
		})
	})
	edited := editedBacklog()
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	e, _ := reopenEngine(t, c)
	err := e.initialize(context.Background())
	var retained *retentionError
	if !errors.As(err, &retained) || retained.code != "backlog_contract_changed" {
		t.Fatalf("work that cannot replay must keep the restore demand: %v", err)
	}
	if !strings.Contains(err.Error(), "restore its saved contract before resuming") {
		t.Fatalf("the demand keeps its message: %v", err)
	}
	var b Backlog
	if err = readJSON(c.Todo, &b); err != nil || b.Task("T2") == nil {
		t.Fatal("the refused startup must leave the edit untouched")
	}
}

// TestStartupHeldEditReplaysRunningReceipt proves the second replay-safe
// shape: a running attempt with a receipt imports the edit and adopts the
// receipt exactly as the retained recovery does, without rerunning the worker.
func TestStartupHeldEditReplaysRunningReceipt(t *testing.T) {
	c, _ := cancelledHoldState(t, func(e *Engine) {
		dir := filepath.Join(e.Dir, "task-run")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		a := &Attempt{
			ID: "task-run", Task: *e.State.Backlog.Task("T1"), Agent: "codex",
			Base: e.State.Tip, Phase: "running", Directory: dir,
			Worktree: dir, Ref: "refs/looper-tasks/task-run",
		}
		e.State.Attempts = append(e.State.Attempts, a)
		receipt := resultReceipt{
			Base: a.Base, Contract: a.Task.Contract(),
			Summary: Summary{TaskID: "T1", Status: "done", Summary: "implemented", Files: []string{}, Blockers: []string{}},
			Commit:  a.Base,
		}
		if err := writeJSON(filepath.Join(dir, "receipt.json"), receipt); err != nil {
			t.Fatal(err)
		}
	})
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	e, _ := reopenEngine(t, c)
	if err := e.initialize(context.Background()); err != nil {
		t.Fatalf("a running attempt with a receipt is replay-safe: %v", err)
	}
	if e.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the startup import must accept the edited file")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" {
		t.Fatalf("the receipt must replay as a ready result: %+v", e.State.Attempts)
	}
	if e.State.Attempts[0].Summary == nil || e.State.Attempts[0].Summary.Status != "done" {
		t.Fatalf("the receipt summary must be adopted: %+v", e.State.Attempts[0].Summary)
	}
}

// TestStartupHeldEditStillRefusesChangedContract pins the import gate (T119):
// the import runs for replay-safe work, but an edit that changes a retained
// task's contract must refuse at the import itself and keep the restore
// demand, because the recovery below would strand that work at the contract
// guard against the imported backlog.
func TestStartupHeldEditStillRefusesChangedContract(t *testing.T) {
	c, saved := cancelledHoldState(t, readyAttempt)
	edited := editedBacklog()
	edited.Task("T1").Details = "changed acceptance criteria"
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	e, _ := reopenEngine(t, c)
	err := e.initialize(context.Background())
	var retained *retentionError
	if !errors.As(err, &retained) || retained.code != "backlog_contract_changed" {
		t.Fatalf("a changed retained contract must refuse the import with the restore demand: %v", err)
	}
	if !strings.Contains(err.Error(), "changes the contract of retained task(s) T1") {
		t.Fatalf("the refusal must name the retained task: %v", err)
	}
	if !strings.Contains(err.Error(), "restore its saved contract before resuming") {
		t.Fatalf("the demand keeps its message: %v", err)
	}
	var b Backlog
	if err = readJSON(c.Todo, &b); err != nil || b.Task("T2") == nil || b.Task("T1").Details != "changed acceptance criteria" {
		t.Fatal("the refused startup must leave the edit untouched")
	}
	if e.State.ExportHash != saved.ExportHash || e.State.Tip != saved.Tip {
		t.Fatal("the refused startup must not move the baseline")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit != saved.Attempts[0].ResultCommit {
		t.Fatalf("the retained result must stay queued with its commit: %+v", e.State.Attempts)
	}
	if countKindEvent(t, e.Dir, "recovery", "Imported the task-file edit left by a cancelled hold") != 0 {
		t.Fatal("the refused startup must not journal an import")
	}
}

// TestStartupHeldEditStillRefusesChangedBaseline pins the baseline rule the
// held refresh already enforces: project files changed beside the edit keep
// the refusal, so the imported baseline never discards competing work.
func TestStartupHeldEditStillRefusesChangedBaseline(t *testing.T) {
	c, _ := cancelledHoldState(t, readyAttempt)
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(c.Root, "stray.txt")
	if err := os.WriteFile(stray, []byte("local edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	e, _ := reopenEngine(t, c)
	err := e.initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "work is retained") {
		t.Fatalf("changed project files must keep the baseline refusal: %v", err)
	}
}

// TestStartupRecoversCancelledHoldRun is the guarded crash-recovery fixture:
// a live controller holds on an external edit, the session is cancelled
// mid-hold, and the next controller imports the edit at startup, re-verifies
// the retained result, and integrates it without repeating the worker.
func TestStartupRecoversCancelledHoldRun(t *testing.T) {
	workerLog := filepath.Join(t.TempDir(), "workers")
	t.Setenv("LOOPER_FAKE_LOG", workerLog)
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"`+holdVerify+`","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T1","status":"done","summary":"implemented","files":["T1.txt"],"blockers":[]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("LOOPER_TEST_RELEASE", release)
	c := repoConfig(t, "run")
	c.Poll = 20 * time.Millisecond
	c.CodexBin = bin
	c.ClaudeBin = bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "hold", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan holdOutcome, 1)
	exited := make(chan struct{})
	go func() {
		code, err := e.Run(ctx)
		done <- holdOutcome{code, err}
		close(exited)
	}()
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hold to be reported", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "Dispatch is paused") >= 1
	})
	// Cancel while the gated check still runs: the edit can no longer import
	// at a live boundary, so the next controller must import it at startup.
	cancel()
	<-exited
	if err = e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = lock.Close(); err != nil {
		t.Fatal(err)
	}
	dir := e.Dir
	if workers, err := os.ReadFile(workerLog); err != nil || strings.Count(string(workers), "worker") != 1 {
		t.Fatalf("the cancelled controller must have run the worker once: %q %v", workers, err)
	}

	// The restart releases the gate up front, so the re-verification of the
	// retained result runs to completion.
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	second, lock2, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan holdOutcome, 1)
	exited2 := make(chan struct{})
	go func() {
		code, err := second.Run(ctx2)
		done2 <- holdOutcome{code, err}
		close(exited2)
	}()
	t.Cleanup(func() {
		cancel2()
		if !joined {
			select {
			case <-exited2:
			case <-time.After(testBudget(15 * time.Second)):
				t.Error("the restarted controller did not stop")
			}
		}
		second.Store.Close()
		lock2.Close()
	})
	waitFor(t, "the startup import", func() bool {
		return countKindEvent(t, dir, "recovery", "Imported the task-file edit left by a cancelled hold") >= 1
	})
	waitFor(t, "the re-verification", func() bool { return checkMarkers(t, dir) >= 2 })
	waitFor(t, "T1 to finish", func() bool {
		var b Backlog
		return readJSON(c.Todo, &b) == nil && b.Task("T1") != nil && b.Task("T1").Status == "done" && b.Task("T2") != nil
	})
	if err := os.WriteFile(filepath.Join(dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done2:
		joined = true
		if out.code != 0 || out.err != nil {
			t.Fatalf("the restarted controller must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("the restarted controller did not drain after stop")
	}
	if workers, err := os.ReadFile(workerLog); err != nil || strings.Count(string(workers), "worker") != 1 {
		t.Fatalf("the restart must replay the retained result without a second worker run: %q %v", workers, err)
	}
	if markers := checkMarkers(t, dir); markers != 2 {
		t.Fatalf("the check must run once before the cancel and once after the import, got %d", markers)
	}
	var final Backlog
	if err = readJSON(c.Todo, &final); err != nil {
		t.Fatal(err)
	}
	if final.Task("T2") == nil || final.Task("T2").Status != "todo" {
		t.Fatalf("the imported edit must survive the status export: %+v", final.Task("T2"))
	}
	if second.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the exported file must match the accepted export hash")
	}
}
