package looper

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// retainedMergeEngine creates real divergent commits and retains uncommitted
// worker edits. The project has already accepted the other side of the merge.
func retainedMergeEngine(t *testing.T, conflict bool) (*Engine, *Attempt) {
	t.Helper()
	e, a := diagnosisEngine(t, `{"task_id":"T001","status":"done","summary":"finished","files":[],"blockers":[],"followups":[],"unblock_when":""}`)
	write := func(work, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(a.Worktree, "retained.txt", "retained implementation\n")
	write(a.Worktree, "worker-only.txt", "worker edits\n")
	other, err := e.Repo.Workspace(identifier("other"), a.Base, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Repo.Remove(other)
	name := "integrated.txt"
	if conflict {
		name = "retained.txt"
	}
	write(other, name, "integrated implementation\n")
	tree, err := e.Repo.Snapshot(other)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := e.Repo.Commit(tree, []string{a.Base}, "Accept independent work")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Repo.Apply(e.State.Published, tree); err != nil {
		t.Fatal(err)
	}
	e.State.Tip, e.State.Published = tip, tree
	e.State.ReviewedTip = tip
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: a.Task.ID, Paths: []string{"."}}}}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{[]string{a.Task.Contract()}, e.Config.Setup, e.Config.Verify})
	return e, a
}

func TestRetainedMerge(t *testing.T) {
	for _, path := range []string{"reopen", "repair"} {
		for _, conflict := range []bool{true, false} {
			name := "clean"
			if conflict {
				name = "conflict"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				e, a := retainedMergeEngine(t, conflict)
				original, base := a.Worktree, a.Base
				if path == "repair" {
					tree, err := e.Repo.Snapshot(a.Worktree)
					if err != nil {
						t.Fatal(err)
					}
					a.ResultCommit, err = e.Repo.Commit(tree, []string{a.Base}, "Retain worker result")
					if err != nil {
						t.Fatal(err)
					}
				} else {
					a.Phase = "blocked"
					a.Summary = &Summary{TaskID: a.Task.ID, Status: "blocked", Blockers: []string{"earlier blocker"}}
				}
				resultCommit := a.ResultCommit
				results := make(chan workerResult, 1)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if path == "reopen" {
					got, err := e.start(ctx, *e.State.Backlog.Task(a.Task.ID), results)
					if err != nil {
						t.Fatal(err)
					}
					if got != a {
						t.Fatal("reopen replaced the retained attempt")
					}
				} else if err := e.repair(ctx, a, "Original verification failure", results); err != nil {
					t.Fatal(err)
				}
				if !conflict {
					select {
					case result := <-results:
						if result.Err != nil {
							t.Fatal(result.Err)
						}
					case <-ctx.Done():
						t.Fatal("worker did not finish")
					}
				}
				e.wg.Wait()
				if conflict {
					if a.Phase != "blocked" || e.State.Backlog.Task(a.Task.ID).Status != "blocked" || a.Summary.Status != "blocked" {
						t.Fatalf("conflict did not fail the attempt: %+v", a)
					}
					for _, want := range []string{"conflicts", "CONFLICT", "retained.txt"} {
						if !strings.Contains(a.Feedback, want) {
							t.Fatalf("feedback lacks %q: %s", want, a.Feedback)
						}
					}
					if path == "repair" && !strings.Contains(a.Feedback, "Original verification failure") {
						t.Fatal("lost check feedback")
					}
					if a.Worktree != original || a.Base != base || a.ResultCommit != resultCommit || len(a.Worktrees) != 1 {
						t.Fatal("conflict replaced retained recovery state")
					}
					select {
					case r := <-results:
						t.Fatalf("worker ran after conflict: %+v", r)
					default:
					}
					saved, err := e.Store.Load()
					if err != nil || saved == nil {
						t.Fatalf("load state: %v", err)
					}
					if saved.Attempts[0].Phase != "blocked" || saved.Attempts[0].Feedback != a.Feedback {
						t.Fatal("failure feedback was not saved")
					}
				} else {
					if a.Phase != "running" || a.Base != e.State.Tip {
						t.Fatal("clean merge did not launch")
					}
					data, err := os.ReadFile(filepath.Join(a.Worktree, "integrated.txt"))
					if err != nil || string(data) != "integrated implementation\n" {
						t.Fatalf("integrated work lost: %q %v", data, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(a.Worktree, "retained.txt"))
				if err != nil || string(data) != "retained implementation\n" {
					t.Fatalf("retained edits lost: %q %v", data, err)
				}
				data, err = os.ReadFile(filepath.Join(a.Worktree, "worker-only.txt"))
				if err != nil || string(data) != "worker edits\n" {
					t.Fatalf("worker edits lost: %q %v", data, err)
				}
				worktrees, err := os.ReadDir(filepath.Dir(original))
				if err != nil {
					t.Fatal(err)
				}
				wantCount := 1
				if !conflict {
					wantCount = 2
				}
				if len(worktrees) != wantCount {
					t.Fatalf("unexpected retained worktrees: %d", len(worktrees))
				}
			})
		}
	}
}

func TestRunReopenedMergeConflictDoesNotWaitForWorker(t *testing.T) {
	e, a := retainedMergeEngine(t, true)
	a.Phase = "blocked"
	e.Config.Once = true
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if err != nil || code != 2 {
		t.Fatalf("run returned %d, %v; want blocked idle exit", code, err)
	}
	a = e.State.Attempts[0]
	if a.Phase != "blocked" || !strings.Contains(a.Feedback, "CONFLICT") {
		t.Fatalf("missing conflict feedback: %+v", a)
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "start" && strings.HasPrefix(event.Message, a.Task.ID+"  ") {
			t.Fatal("launched worker after merge conflict")
		}
	}
}
