package looper

import (
	"context"
	"os"
	"os/exec"
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

// failingMergeGit puts a git shim on PATH whose merge subcommand fails with
// a distinctive storage error, while every other git invocation runs the
// real binary. The merge target worktree stays clean, so the failure is a
// verified non-conflict rather than an unavailable classification.
func failingMergeGit(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "git")
	script := `#!/bin/sh
skip=0
for a in "$@"; do
  if [ "$skip" = 1 ]; then skip=0; continue; fi
  case "$a" in
    -C|-c) skip=1;;
    -*) ;;
    merge) echo "fatal: simulated storage failure during merge" >&2; exit 128;;
    *) break;;
  esac
done
exec ` + real + ` "$@"
`
	if err := os.WriteFile(shim, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A merge failure that is not a verified content conflict must hold the
// attempt with the operation, the original Git error, and the retained
// recovery state intact. No worker may start from the failed merge.
func TestRetainedMergeFailureWithoutConflict(t *testing.T) {
	for _, path := range []string{"reopen", "repair"} {
		t.Run(path, func(t *testing.T) {
			e, a := retainedMergeEngine(t, false)
			original, base := a.Worktree, a.Base
			resultCommit := ""
			if path == "repair" {
				tree, err := e.Repo.Snapshot(a.Worktree)
				if err != nil {
					t.Fatal(err)
				}
				a.ResultCommit, err = e.Repo.Commit(tree, []string{a.Base}, "Retain worker result")
				if err != nil {
					t.Fatal(err)
				}
				resultCommit = a.ResultCommit
			} else {
				a.Phase = "blocked"
				a.Summary = &Summary{TaskID: a.Task.ID, Status: "blocked", Blockers: []string{"earlier blocker"}}
			}
			failingMergeGit(t)
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
			if a.Phase != "blocked" || e.State.Backlog.Task(a.Task.ID).Status != "blocked" || a.Summary.Status != "blocked" {
				t.Fatalf("merge failure did not hold the attempt: %+v", a)
			}
			for _, want := range []string{
				"Merging the ", "failed without a content conflict",
				"simulated storage failure during merge",
			} {
				if !strings.Contains(a.Feedback, want) {
					t.Fatalf("feedback lacks %q: %s", want, a.Feedback)
				}
			}
			if strings.Contains(a.Feedback, "conflicts with the integrated project") ||
				strings.Contains(a.Feedback, "Resolve the content conflicts") {
				t.Fatalf("feedback directs the operator to nonexistent conflicts: %s", a.Feedback)
			}
			if path == "repair" {
				if !strings.Contains(a.Feedback, "Original verification failure") {
					t.Fatal("lost check feedback")
				}
				if a.ResultCommit != resultCommit {
					t.Fatal("merge failure lost the retained result commit")
				}
			}
			if a.Worktree != original || a.Base != base || len(a.Worktrees) != 1 {
				t.Fatal("merge failure replaced retained recovery state")
			}
			select {
			case r := <-results:
				t.Fatalf("worker ran after a failed merge: %+v", r)
			default:
			}
			for _, event := range storedEvents(t, e) {
				if event.Kind == "start" && strings.HasPrefix(event.Message, a.Task.ID+"  ") {
					t.Fatalf("launched worker after a failed merge: %s", event.Message)
				}
			}
			saved, err := e.Store.Load()
			if err != nil || saved == nil {
				t.Fatalf("load state: %v", err)
			}
			if saved.Attempts[0].Phase != "blocked" || saved.Attempts[0].Feedback != a.Feedback {
				t.Fatal("failure feedback was not saved")
			}
			for name, want := range map[string]string{
				"retained.txt":    "retained implementation\n",
				"worker-only.txt": "worker edits\n",
			} {
				data, err := os.ReadFile(filepath.Join(a.Worktree, name))
				if err != nil || string(data) != want {
					t.Fatalf("%s lost: %q %v", name, data, err)
				}
			}
			worktrees, err := os.ReadDir(filepath.Dir(original))
			if err != nil {
				t.Fatal(err)
			}
			if len(worktrees) != 1 {
				t.Fatalf("unexpected retained worktrees: %d", len(worktrees))
			}
		})
	}
}

// Prepare shares the merge classification: a real conflict keeps the
// conflict wording with its unmerged path, and a Git failure that leaves no
// unmerged paths names the operation and the actual error instead.
func TestPrepareMergeFailureFeedback(t *testing.T) {
	retain := func(e *Engine, a *Attempt) string {
		tree, err := e.Repo.Snapshot(a.Worktree)
		if err != nil {
			t.Fatal(err)
		}
		a.ResultCommit, err = e.Repo.Commit(tree, []string{a.Base}, "Retain worker result")
		if err != nil {
			t.Fatal(err)
		}
		return a.ResultCommit
	}

	e, a := retainedMergeEngine(t, true)
	retain(e, a)
	c, err := e.prepare(a, e.State.Tip)
	if err != nil {
		t.Fatal(err)
	}
	if c.OK {
		t.Fatal("a conflicting result must not produce a candidate")
	}
	for _, want := range []string{"conflicts with the integrated project", "retained.txt", "CONFLICT"} {
		if !strings.Contains(c.Feedback, want) {
			t.Fatalf("conflict feedback lacks %q: %s", want, c.Feedback)
		}
	}

	e, a = retainedMergeEngine(t, false)
	missing := strings.Repeat("0123456789", 4)
	a.ResultCommit = missing
	c, err = e.prepare(a, e.State.Tip)
	if err != nil {
		t.Fatal(err)
	}
	if c.OK {
		t.Fatal("a failed merge must not produce a candidate")
	}
	if !strings.Contains(c.Feedback, "failed without a content conflict") || !strings.Contains(c.Feedback, missing) {
		t.Fatalf("failure feedback must name the operation and the actual error: %s", c.Feedback)
	}
	if strings.Contains(c.Feedback, "conflicts with the integrated project") {
		t.Fatalf("a non-conflict prepare failure reads as a conflict: %s", c.Feedback)
	}
}

// The run loop must also survive a non-conflict merge failure on a reopened
// attempt: the attempt holds, no worker starts, and the loop exits blocked.
func TestRunReopenedMergeFailureDoesNotWaitForWorker(t *testing.T) {
	e, a := retainedMergeEngine(t, false)
	a.Phase = "blocked"
	e.Config.Once = true
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	failingMergeGit(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if err != nil || code != 2 {
		t.Fatalf("run returned %d, %v; want blocked idle exit", code, err)
	}
	a = e.State.Attempts[0]
	if a.Phase != "blocked" || !strings.Contains(a.Feedback, "failed without a content conflict") {
		t.Fatalf("missing non-conflict feedback: %+v", a)
	}
	if strings.Contains(a.Feedback, "conflicts with the integrated project") {
		t.Fatalf("feedback claims a content conflict: %s", a.Feedback)
	}
	for _, event := range storedEvents(t, e) {
		if event.Kind == "start" && strings.HasPrefix(event.Message, a.Task.ID+"  ") {
			t.Fatal("launched worker after a failed merge")
		}
	}
}
