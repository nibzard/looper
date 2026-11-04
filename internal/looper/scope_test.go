package looper

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The competing task must carry its own narrow scope. Without it the plan
// falls back to a whole-tree scope for T2, which overlaps anything, and the
// observed expansion cannot be told apart from the fallback.
func TestObserveScopeBlocksCompetingNarrowScope(t *testing.T) {
	b := testBacklog()
	plan := &Plan{Verify: "make test", Tasks: []Scope{
		{TaskID: "T1", Paths: []string{"src/auth"}},
		{TaskID: "T2", Paths: []string{"src/db"}},
	}}
	s, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e := &Engine{Repo: &Repository{Root: "/repo", Excluded: []string{"to-do.json"}}, Store: s, State: &State{Backlog: b, Plan: plan}, Output: io.Discard}
	a := &Attempt{Task: b.Tasks[0], Phase: "running", Summary: &Summary{Files: []string{"src/db/config.go"}}}
	e.State.Attempts = append(e.State.Attempts, a)
	if !e.compatible(b.Tasks[1]) {
		t.Fatal("disjoint planned scopes must allow parallel dispatch before observation")
	}
	e.observeScope(a)
	if strings.Join(plan.Tasks[0].Paths, ",") != "src/auth,src/db/config.go" {
		t.Fatalf("observed path not appended: %v", plan.Tasks[0].Paths)
	}
	if e.compatible(b.Tasks[1]) {
		t.Fatal("an observed path inside the competing scope must block parallel dispatch")
	}
}

// resumeReceiptEngine seeds one retained attempt whose worker finished but
// whose controller died before consuming the result: a running attempt with a
// matching receipt.json on disk. The state round-trips through the store, so
// the engine under test reloads it exactly the way a restarted controller does.
func resumeReceiptEngine(t *testing.T) (*Engine, *Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, "")
	c := repoConfig(t, "run", "--jobs", "2")
	c.CodexBin, c.ClaudeBin = bin, bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "auth work", Priority: 1, Status: "doing"},
		{ID: "T002", Title: "database work", Priority: 1, Status: "doing"},
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
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T001", Paths: []string{"src/auth"}},
		{TaskID: "T002", Paths: []string{"src/db"}},
	}}
	contracts := []string{}
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	work, err := e.Repo.Workspace("task-receipt", e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(e.Dir, "task-receipt")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	task := *e.State.Backlog.Task("T001")
	a := &Attempt{ID: "task-receipt", Task: task, Agent: "codex", Base: e.State.Tip, Worktree: work, Directory: dir, Ref: "refs/looper-tasks/task-receipt", Phase: "running", Worktrees: []string{work}}
	e.State.Attempts = append(e.State.Attempts, a)
	// The receipt records a worker that edited outside its planned scope.
	receipt := resultReceipt{
		Base: a.Base, Contract: task.Contract(), Repairs: a.Repairs,
		Summary: Summary{TaskID: "T001", Status: "done", Summary: "touched the shared schema", Files: []string{"src/auth/session.go", "src/db/config.go"}, Blockers: []string{}},
		Commit:  e.State.Tip,
	}
	if err := writeJSON(filepath.Join(dir, "receipt.json"), receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	lock.Close()

	resumed, lock2, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.initialize(context.Background()); err != nil {
		lock2.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { resumed.Store.Close(); lock2.Close() })
	return e, resumed, c
}

// Receipt recovery must expand the plan scope before the next dispatch
// decision, so a restarted controller cannot dispatch competing work over
// files the dead worker already touched.
func TestResumeReceiptExpandsScopeAndBlocksDispatch(t *testing.T) {
	before, resumed, _ := resumeReceiptEngine(t)
	competing := *before.State.Backlog.Task("T002")
	if !before.compatible(competing) {
		t.Fatal("fixture must start with disjoint scopes")
	}
	scope := resumed.State.Plan.Scope("T001")
	if strings.Join(scope.Paths, ",") != "src/auth,src/db/config.go" {
		t.Fatalf("recovery must expand the scope with observed paths, got %v", scope.Paths)
	}
	a := resumed.attempt("task-receipt")
	if a == nil || a.Phase != "ready" {
		t.Fatalf("a matching receipt must leave the attempt ready to check, got %+v", a)
	}
	if a.Summary == nil || a.Summary.TaskID != "T001" {
		t.Fatalf("the receipt summary must load into the attempt, got %+v", a.Summary)
	}
	if resumed.compatible(competing) {
		t.Fatal("the recovered scope must block dispatch of the competing task")
	}
	var expanded bool
	for _, ev := range storedEvents(t, resumed) {
		if ev.Kind == "scope" && strings.Contains(ev.Message, "src/db/config.go") {
			expanded = true
		}
	}
	if !expanded {
		t.Fatal("scope expansion during recovery must be recorded as an event")
	}
}
