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

// resumeEngine builds an engine that resumes two retained attempts whose
// planned write scopes overlap: the state a controller restart leaves when
// both tasks were mid-run and a replan assigned them one shared write path.
// The saved state round-trips through the store, so the engine under test
// reloads it exactly the way a restarted controller does.
func resumeEngine(t *testing.T, result string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, result)
	t.Setenv("LOOPER_FAKE_SLEEP", "30")
	c := repoConfig(t, "run")
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
	// A plan whose two scopes share one write path, with the signature the
	// run loop expects, so it never launches a planning agent.
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T001", Paths: []string{"README.md"}},
		{TaskID: "T002", Paths: []string{"README.md"}},
	}}
	contracts := []string{}
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	for _, id := range []string{"T001", "T002"} {
		work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(e.Dir, "task-"+strings.ToLower(id))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		e.State.Attempts = append(e.State.Attempts, &Attempt{
			ID: "task-" + strings.ToLower(id), Task: *e.State.Backlog.Task(id), Agent: "codex",
			Base: e.State.Tip, Worktree: work, Directory: dir,
			Ref: "refs/looper-tasks/task-" + strings.ToLower(id), Phase: "resume", Worktrees: []string{work},
		})
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e, c
}

func TestCompatibleIgnoresRetainedAttempts(t *testing.T) {
	e, _ := resumeEngine(t, "")
	for _, id := range []string{"T001", "T002"} {
		task := e.State.Backlog.Task(id)
		if task == nil || !e.compatible(*task) {
			t.Fatalf("%s must stay dispatchable while both attempts are retained", id)
		}
	}
}

// Two retained attempts with overlapping scopes used to veto each other, so
// the run loop dispatched nothing and waited forever without an error.
func TestRunResumesOneOfTwoOverlappingRetainedAttempts(t *testing.T) {
	result := `{"task_id":"T001","status":"done","summary":"finished","files":[],"blockers":[],"followups":[],"unblock_when":""}`
	e, _ := resumeEngine(t, result)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 130 || err != context.DeadlineExceeded {
		t.Fatalf("engine exited %d (%v); want a live loop cancelled at the deadline", code, err)
	}
	running, retained := 0, 0
	for _, a := range e.State.Attempts {
		switch a.Phase {
		case "running":
			running++
		case "resume":
			retained++
		}
	}
	var started []string
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Kind == "start" && (strings.HasPrefix(ev.Message, "T001") || strings.HasPrefix(ev.Message, "T002")) {
			started = append(started, ev.Message)
		}
	}
	if len(started) != 1 || running != 1 || retained != 1 {
		t.Fatalf("one retained attempt must relaunch while the other stays retained; started=%v running=%d retained=%d", started, running, retained)
	}
}
