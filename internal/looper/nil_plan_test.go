package looper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The dispatch path dereferenced a nil State.Plan at three sites that run
// while work is active (the T049 crash class): compatible's scope read,
// launch's plan snapshot, and the check spawn's plan snapshot. With no plan,
// each site defers its work until a planning pass rebuilds the plan instead
// of dereferencing the nil pointer.
func TestNilPlanDefersDispatchSites(t *testing.T) {
	e := requeueEngine(t)
	e.State.Plan = nil
	e.State.Backlog.Tasks = []Task{
		{ID: "T1", Title: "In flight", Priority: 1, Status: "doing"},
		{ID: "T2", Title: "Ready work", Priority: 1, Status: "todo"},
	}
	e.State.Attempts = []*Attempt{{ID: "a1", Task: Task{ID: "T1", Title: "In flight", Priority: 1}, Phase: "running", Worktree: "retained"}}
	ctx := context.Background()
	out := make(chan workerResult, 1)
	// compatible: without a plan there is no scope to gate concurrency
	// against. This call panicked on the nil plan before the guard.
	if e.compatible(e.State.Backlog.Tasks[1]) {
		t.Fatal("a nil plan must report the task not dispatchable")
	}
	// launch, start, and repair defer their work instead of cloning the nil
	// plan, and they leave the attempt state untouched.
	if err := e.launch(ctx, e.State.Attempts[0], out); !errors.Is(err, errPlanMissing) {
		t.Fatalf("launch with a nil plan: %v", err)
	}
	if _, err := e.start(ctx, e.State.Backlog.Tasks[1], out); !errors.Is(err, errPlanMissing) {
		t.Fatalf("start with a nil plan: %v", err)
	}
	if err := e.repair(ctx, e.State.Attempts[0], "check failed", out); !errors.Is(err, errPlanMissing) {
		t.Fatalf("repair with a nil plan: %v", err)
	}
	select {
	case r := <-out:
		t.Fatalf("deferred work launched anyway: %+v", r)
	default:
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "running" || e.State.Attempts[0].Repeats != 0 || e.State.Attempts[0].FailureHash != "" {
		t.Fatalf("deferral changed attempt state: %+v", e.State.Attempts[0])
	}
	// A live plan keeps its behavior: T2 scopes away from the running T1.
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T1", Paths: []string{"one/"}},
		{TaskID: "T2", Paths: []string{"two/"}},
	}}
	if !e.compatible(e.State.Backlog.Tasks[1]) {
		t.Fatal("a live plan must keep gating dispatch as before")
	}
}

// retainedPlanRun builds the config for a controller whose saved state is
// what a crashed pre-T049 controller could leave: T001's worker finished and
// waits for its check, T002 queues behind it, and no execution plan exists.
// The stub agent answers planning, review, and worker prompts; verify is the
// planner-selected verification command, used with no configuration override.
func retainedPlanRun(t *testing.T, verify string) Config {
	t.Helper()
	bin, _ := fakeAgent(t, "")
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.Once = true
	if c.Verify != "" {
		t.Fatalf("the fixture needs no verification override, got %q", c.Verify)
	}
	b := Backlog{Version: 1, Sources: []string{}, Goal: "recover retained work", Tasks: []Task{
		{ID: "T001", Title: "finished worker", Priority: 1, Status: "doing"},
		{ID: "T002", Title: "later work", Priority: 1, Status: "todo", DependsOn: []string{"T001"}},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_PLAN", fmt.Sprintf(`{"setup":"","verify":%q,"tasks":[{"task_id":"T001","write_paths":["one/"]},{"task_id":"T002","write_paths":["two/"]}]}`, verify))
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T002","status":"done","summary":"implemented","files":[],"blockers":[]}`)
	return c
}

// retainedResumeEngine starts a controller over state a previous run already
// saved, without reseeding it.
func retainedResumeEngine(t *testing.T, c Config) (*Engine, func()) {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	shutdown := func() {
		if closed {
			return
		}
		closed = true
		e.Store.Close()
		lock.Close()
	}
	t.Cleanup(shutdown)
	return e, shutdown
}

// retainedEngine seeds the retained nil-plan state and returns a controller
// whose Run resumes it. The attempt keeps its result commit, so startup
// recovers it as ready work waiting for verification, never a new worker.
// Each mutate function adjusts the saved state before it is persisted.
func retainedEngine(t *testing.T, c Config, mutate ...func(*State)) (*Engine, func()) {
	t.Helper()
	e, shutdown := retainedResumeEngine(t, c)
	if err := e.initializeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.Dir, "task-a1"), 0700); err != nil {
		t.Fatal(err)
	}
	e.State.Attempts = []*Attempt{{
		ID: "a1", Task: *e.State.Backlog.Task("T001"), Agent: e.Config.Agent,
		Base: e.State.Tip, ResultCommit: e.State.Tip,
		Worktree: filepath.Join(e.Dir, "task-a1"), Directory: filepath.Join(e.Dir, "task-a1"),
		Ref: "refs/looper-tasks/a1", Phase: "ready", Worktrees: []string{},
		Summary: &Summary{TaskID: "T001", Status: "done", Summary: "implemented", Files: []string{}, Blockers: []string{}},
	}}
	e.State.Plan = nil
	e.State.PlanSignature = ""
	for _, m := range mutate {
		m(e.State)
	}
	if err := e.Store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	e.State = nil
	return e, shutdown
}

// agentLogPath records which agent roles ran. The fake appends "worker" for
// worker prompts and "other" for every other invocation.
func agentLogPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-log")
	t.Setenv("LOOPER_FAKE_LOG", path)
	return path
}

func workerRuns(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "worker" {
			n++
		}
	}
	return n
}

// eventKinds returns the ordered kinds of the recorded events, so tests can
// prove that one boundary ran before another.
func eventKinds(t *testing.T, e *Engine) []string {
	t.Helper()
	rows, err := e.Store.db.Query(`SELECT kind FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var kinds []string
	for rows.Next() {
		var kind string
		if err = rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, kind)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return kinds
}

// TestNilPlanRetainedWorkRecovers replaces the steady-state deferral
// assertion: a ready attempt with a nil plan must reach verification instead
// of deferring forever. The retained result is verified without a new worker,
// and the planner-selected verification command runs with no override.
func TestNilPlanRetainedWorkRecovers(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verify-ran")
	c := retainedPlanRun(t, "printf planned-verify > "+marker)
	log := agentLogPath(t)
	e, _ := retainedEngine(t, c)
	// The recovery run verifies retained work without a new worker; its
	// durable steps are fsync-bound, so the budget must scale with host
	// pressure.
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("recovery run: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "complete" {
		t.Fatalf("recovery did not complete the project: %+v", exit)
	}
	for _, id := range []string{"T001", "T002"} {
		if task := e.State.Backlog.Task(id); task == nil || task.Status != "done" {
			t.Fatalf("%s did not finish: %+v", id, task)
		}
	}
	if e.State.Plan == nil || e.State.Plan.Verify != "printf planned-verify > "+marker {
		t.Fatalf("the recovery plan lost its verification command: %+v", e.State.Plan)
	}
	// The planner-selected command ran, and no configuration override
	// supplied it.
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the planner-selected verification never ran: %v", err)
	}
	if strings.TrimSpace(string(got)) != "planned-verify" {
		t.Fatalf("unexpected verification output %q", string(got))
	}
	// Only T002 ran a worker: the retained T001 result was verified, not
	// reimplemented.
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("expected exactly one implementation run, got %d", n)
	}
	var result string
	if err = e.Store.db.QueryRow(`SELECT result FROM checks WHERE attempt_id='a1' AND kind='executable'`).Scan(&result); err != nil {
		t.Fatalf("the retained result was never verified: %v", err)
	}
	if result != "pass" {
		t.Fatalf("retained verification result %q", result)
	}
}

// TestNilPlanPlanningFailureRetainsWork proves a failed recovery planning
// pass exits with an actionable error instead of resuming the deferral loop,
// and that the retained result commit survives it.
func TestNilPlanPlanningFailureRetainsWork(t *testing.T) {
	// An empty verification command is invalid; a zero-value plan is not a
	// recovery strategy.
	c := retainedPlanRun(t, "")
	log := agentLogPath(t)
	e, _ := retainedEngine(t, c)
	// The failed planning pass still writes durable state before it exits;
	// those steps are fsync-bound, so the budget must scale with host
	// pressure.
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 1 || err == nil {
		t.Fatalf("planning failure must fail the run: %d %v", code, err)
	}
	if !strings.Contains(err.Error(), "planning failed while recovering 1 retained result") ||
		!strings.Contains(err.Error(), e.Dir) ||
		!strings.Contains(err.Error(), "execution plan needs an executable verification command") {
		t.Fatalf("the recovery error is not actionable: %v", err)
	}
	if e.State.Plan != nil {
		t.Fatal("an invalid plan was substituted for the missing one")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("the retained result was lost: %+v", e.State.Attempts)
	}
	if task := e.State.Backlog.Task("T001"); task.Status != "doing" {
		t.Fatalf("the retained task changed status: %+v", task)
	}
	if n := workerRuns(t, log); n != 0 {
		t.Fatalf("a failed recovery dispatched %d workers", n)
	}
}

// TestNilPlanRecoverySurvivesCancellationAndRestart cancels the recovery
// planning pass and proves a restart verifies the retained result without
// repeating implementation.
func TestNilPlanRecoverySurvivesCancellationAndRestart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verify-ran")
	c := retainedPlanRun(t, "printf planned-verify > "+marker)
	// Hold the planner so the cancellation lands inside the recovery pass.
	t.Setenv("LOOPER_FAKE_SLEEP", "30")
	e, shutdown := retainedEngine(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	code, err := e.Run(ctx)
	cancel()
	if code != 130 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled recovery: %d %v", code, err)
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("cancellation lost the retained result: %+v", e.State.Attempts)
	}
	shutdown()

	t.Setenv("LOOPER_FAKE_SLEEP", "")
	log := agentLogPath(t)
	e, shutdown = retainedEngine(t, c)
	// The restart verifies the retained result and finishes the project;
	// those durable steps are fsync-bound, so the budget must scale with
	// host pressure.
	ctx, cancel = context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	code, err = e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("restart after cancellation: %d %v", code, err)
	}
	if task := e.State.Backlog.Task("T001"); task == nil || task.Status != "done" {
		t.Fatalf("the retained task did not finish after restart: %+v", task)
	}
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("restart repeated implementation: %d worker runs", n)
	}
	shutdown()
}
