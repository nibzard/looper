package looper

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// reviewEngine builds an initialized engine whose backlog decides one final
// review. The fake review agent answers with result and, when edit is not
// empty, first overwrites the private backlog in its worktree with those
// bytes.
func reviewEngine(t *testing.T, result string, edit []byte, backlog Backlog) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_REVIEW", result)
	c := repoConfig(t, "run")
	if edit != nil {
		t.Setenv("LOOPER_FAKE_BACKLOG_EDIT", string(edit))
		// The agent works in a review worktree; only the file name identifies
		// the private backlog copy beside it.
		t.Setenv("LOOPER_FAKE_BACKLOG_PATH", filepath.Base(c.Todo))
	}
	c.CodexBin = bin
	c.ClaudeBin = bin
	if err := writeJSON(c.Todo, backlog); err != nil {
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
	return e, c
}

func doneBacklog(extra ...Task) Backlog {
	tasks := append([]Task{{ID: "T1", Title: "one", Priority: 1, Status: "done"}}, extra...)
	return Backlog{Version: 1, Sources: []string{}, Goal: "ship it", Tasks: tasks}
}

const reviewComplete = `{"status":"reviewed","summary":"checked","findings":[],"complete":true}`
const reviewIncomplete = `{"status":"reviewed","summary":"checked","findings":[],"complete":false}`

func TestFinalReviewNeedsExplicitCompletion(t *testing.T) {
	e, _ := reviewEngine(t, reviewIncomplete, nil, doneBacklog())
	if err := e.review(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e.State.Finished {
		t.Fatal("complete:false must leave the goal unfinished")
	}
}

func TestFinalReviewCompletesFromResponse(t *testing.T) {
	e, _ := reviewEngine(t, reviewComplete, nil, doneBacklog())
	if err := e.review(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !e.State.Finished {
		t.Fatal("complete:true with every ordinary task done must finish the goal")
	}
}

func TestHistoricalMarkerCannotCompleteAGoal(t *testing.T) {
	marker := Task{ID: "T9", Title: "Project done", Priority: 5, Status: "done", Tags: []string{"project-done"}}
	e, _ := reviewEngine(t, reviewIncomplete, nil, doneBacklog(marker))
	if err := e.review(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e.State.Finished {
		t.Fatal("a historical marker must not authorize completion")
	}
	if e.State.Backlog.Task("T9") == nil {
		t.Fatal("loading must preserve the historical marker")
	}
}

func TestBlockedTaskPreventsCompletion(t *testing.T) {
	blocked := Task{ID: "T2", Title: "two", Priority: 1, Status: "blocked", Blockers: []string{"missing input"}}
	e, _ := reviewEngine(t, reviewComplete, nil, doneBacklog(blocked))
	if err := e.review(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e.State.Finished {
		t.Fatal("a blocked task must prevent goal completion")
	}
}

// An accepted finding must also block completion until it is implemented.
func TestOpenFindingPreventsCompletion(t *testing.T) {
	result := `{"status":"reviewed","summary":"checked","findings":[{"title":"Fix observed behavior","details":"Check added output","evidence":"Observed fixture failure","depends_on":[],"priority":1}],"complete":true}`
	e, _ := reviewEngine(t, result, nil, doneBacklog())
	if err := e.review(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if e.State.Finished {
		t.Fatal("an accepted open finding must prevent goal completion")
	}
	added := e.State.Backlog.Task("T002")
	if added == nil || added.Status != "todo" || added.Title != "Fix observed behavior" {
		t.Fatalf("finding did not create work: %+v", e.State.Backlog.Tasks)
	}
}

func TestReviewBacklogEditsAreRejected(t *testing.T) {
	cases := []struct {
		name string
		edit func(Backlog) Backlog
	}{
		{"marker task", func(b Backlog) Backlog {
			b.Tasks = append(b.Tasks, Task{ID: "T9", Title: "Project done", Priority: 5, Status: "done", Tags: []string{"project-done"}})
			return b
		}},
		{"new task", func(b Backlog) Backlog {
			b.Tasks = append(b.Tasks, Task{ID: "T2", Title: "extra work", Priority: 1, Status: "todo"})
			return b
		}},
		{"changed goal", func(b Backlog) Backlog {
			b.Goal = "a different goal"
			return b
		}},
		{"changed sources", func(b Backlog) Backlog {
			b.Sources = []string{"other.md"}
			return b
		}},
		{"changed task", func(b Backlog) Backlog {
			b.Tasks[0].Title = "rewritten"
			return b
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The edit function works on its own copy; the accepted backlog
			// stays the pristine one.
			edited, err := json.Marshal(tc.edit(doneBacklog()))
			if err != nil {
				t.Fatal(err)
			}
			e, _ := reviewEngine(t, reviewComplete, edited, doneBacklog())
			before := clone(e.State.Backlog)
			err = e.review(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), "private backlog") {
				t.Fatalf("review must be rejected for a private backlog edit: %v", err)
			}
			if e.State.Finished {
				t.Fatal("a rejected review must not finish the goal")
			}
			if hashJSON(e.State.Backlog) != hashJSON(before) {
				t.Fatal("a rejected review must not change the accepted backlog")
			}
			if e.State.ReviewedTip != "" {
				t.Fatal("a rejected review must not advance the reviewed revision")
			}
			// The controller's task file in the project root keeps the
			// accepted backlog.
			var onDisk Backlog
			if err = readJSON(e.Config.Todo, &onDisk); err != nil {
				t.Fatal(err)
			}
			if hashJSON(onDisk) != hashJSON(before) {
				t.Fatal("a rejected review must not write the task file")
			}
		})
	}
}
