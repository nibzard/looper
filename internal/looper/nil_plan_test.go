package looper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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

// TestRunWithNilPlanDefersWork resumes the state a crashed pre-T049
// controller could leave: one finished worker waiting for its check, ready
// work, and no plan. The run loop must complete its iterations without the
// controller panic the unguarded sites produced, and the work must stay
// deferred while no planning pass can run.
func TestRunWithNilPlanDefersWork(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	called := filepath.Join(dir, "agent-called")
	script := `#!/bin/bash
touch "` + called + `"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	b := Backlog{Version: 1, Sources: []string{}, Goal: "defer work without a plan", Tasks: []Task{
		{ID: "T001", Title: "finished worker", Priority: 1, Status: "doing"},
		{ID: "T002", Title: "ready work", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	// Seed the project baseline once, then persist the crafted state: the
	// pending attempt keeps its result commit so startup resumes it as ready
	// work, and the plan stays nil.
	if err = e.initializeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	work := e.State.Tip
	e.State.Attempts = []*Attempt{{
		ID: "a1", Task: *e.State.Backlog.Task("T001"), Agent: e.Config.Agent,
		Base: work, ResultCommit: work,
		Worktree: filepath.Join(e.Dir, "task-a1"), Directory: filepath.Join(e.Dir, "task-a1"),
		Ref: "refs/looper-tasks/a1", Phase: "ready", Worktrees: []string{},
	}}
	e.State.Plan = nil
	e.State.PlanSignature = ""
	if err = e.Store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	e.State = nil

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("controller panic: %v", r)
			}
		}()
		_, err := e.Run(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			done <- err
			return
		}
		done <- nil
	}()
	// The deferral is reported once; stop the controller after it is on record.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var n int
		if err = e.Store.db.QueryRow(`SELECT count(*) FROM events WHERE kind='defer' AND message LIKE 'Execution plan is missing%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("run ended before the deferral was reported: %v", err)
			}
			t.Fatal("the nil plan never deferred the waiting work")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatalf("run with a nil plan: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if _, err = os.Stat(called); err == nil {
		t.Fatal("an agent ran although all work stayed deferred")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" {
		t.Fatalf("the pending attempt did not stay deferred: %+v", e.State.Attempts)
	}
	if e.State.Plan != nil {
		t.Fatal("the plan was rebuilt while work was in flight")
	}
	if t2 := e.State.Backlog.Task("T002"); t2 == nil || t2.Status != "todo" {
		t.Fatalf("the ready work did not stay deferred: %+v", t2)
	}
	var spawned int
	if err = e.Store.db.QueryRow(`SELECT count(*) FROM checks`).Scan(&spawned); err != nil {
		t.Fatal(err)
	}
	if spawned != 0 {
		t.Fatal("a check was spawned against the nil plan")
	}
	var defers int
	if err = e.Store.db.QueryRow(`SELECT count(*) FROM events WHERE kind='defer' AND message LIKE 'Execution plan is missing%'`).Scan(&defers); err != nil {
		t.Fatal(err)
	}
	if defers != 1 {
		t.Fatalf("the deferral was reported %d times, not once", defers)
	}
}
