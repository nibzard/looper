package looper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDecodeNewTasks(t *testing.T) {
	for _, input := range []string{`{}`, `[]`, `null`, `{"id":"T2","typo":true}`, `{} {}`, `{"status":"done"}`} {
		tasks, err := decodeNewTasks([]byte(input))
		if err == nil {
			b := Backlog{Version: 1, Sources: []string{}, Tasks: tasks}
			err = b.Validate()
		}
		if err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	tasks, err := decodeNewTasks([]byte(`{"id":"T2","title":"New work","priority":2}`))
	if err != nil || len(tasks) != 1 || tasks[0].Status != "todo" {
		t.Fatalf("decode: %+v %v", tasks, err)
	}
}

func TestAddTaskTransportPreservesActiveWork(t *testing.T) {
	e := requeueEngine(t)
	e.State.Attempts[0].Phase = "implementing"
	before := hashJSON(e.State.Attempts)
	original := e.State.Backlog.Tasks[0]
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("add"), Type: RequestTypeAddTask, Session: e.controller.Session, Tasks: []Task{
		{ID: "T2", Title: "New work", Priority: 2},
		{ID: "T3", Title: "Dependent work", Priority: 3, DependsOn: []string{"T2"}},
	}}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processAddTaskRequests()
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil || ack.Status != AckApplied {
		t.Fatalf("ack: %+v %v", ack, err)
	}
	if len(e.State.Backlog.Tasks) != 3 || !reflect.DeepEqual(original, e.State.Backlog.Tasks[0]) || before != hashJSON(e.State.Attempts) {
		t.Fatal("existing work changed")
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || len(exported.Tasks) != 3 {
		t.Fatalf("export: %v", err)
	}
	if ack = e.applyAddTaskRequest(req); ack.Status != AckApplied || len(e.State.Backlog.Tasks) != 3 {
		t.Fatal("replay duplicated tasks")
	}
}

// A request can apply while a worker implements. The dispatch loop reads
// scopes from the live plan on every tick, so an applied request must keep
// that plan and invalidate only its signature. Tasks missing from the
// retained plan scope to the whole tree, which blocks parallel dispatch
// until the next planning pass rebuilds with them.
func TestAddTaskWhileRunningKeepsLivePlan(t *testing.T) {
	e := requeueEngine(t)
	e.State.Backlog.Tasks[0].Status = "doing"
	e.State.Attempts[0].Phase = "running"
	plan := &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"one/"}}}}
	e.State.Plan = plan
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{[]string{e.State.Backlog.Tasks[0].Contract()}, "", "true"})
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("add"), Type: RequestTypeAddTask, Session: e.controller.Session, Tasks: []Task{
		{ID: "T2", Title: "New work", Priority: 2},
	}}
	if ack := e.applyAddTaskRequest(req); ack.Status != AckApplied {
		t.Fatalf("add-task while running: %+v", ack)
	}
	if e.State.Plan != plan {
		t.Fatal("an applied add-task request discarded the live plan")
	}
	if e.State.PlanSignature != "" {
		t.Fatal("the stale plan signature must force a replan")
	}
	if !e.compatible(*e.State.Backlog.Task("T1")) {
		t.Fatal("the retained plan must keep gating the running task")
	}
	if e.compatible(*e.State.Backlog.Task("T2")) {
		t.Fatal("a task outside the retained plan must not dispatch beside running work")
	}
}

func TestAddTaskRejectsWholeBatch(t *testing.T) {
	for _, mode := range []string{"duplicate", "dependency", "cycle", "stale", "external", "status"} {
		t.Run(mode, func(t *testing.T) {
			e := requeueEngine(t)
			before := hashJSON(e.State.Backlog)
			req := &SettingsRequest{Version: 1, ID: identifier("add"), Session: e.controller.Session, Tasks: []Task{{ID: "T2", Title: "Work", Priority: 2}, {ID: "T3", Title: "Other", Priority: 2}}}
			switch mode {
			case "duplicate":
				req.Tasks[1].ID = "T1"
			case "dependency":
				req.Tasks[1].DependsOn = []string{"missing"}
			case "cycle":
				req.Tasks[0].DependsOn = []string{"T3"}
				req.Tasks[1].DependsOn = []string{"T2"}
			case "stale":
				req.Session = "old"
			case "external":
				if err := os.WriteFile(e.Config.Todo, []byte("external edit"), 0600); err != nil {
					t.Fatal(err)
				}
			case "status":
				req.Tasks[1].Status = "doing"
			}
			if ack := e.applyAddTaskRequest(req); ack.Status == AckApplied {
				t.Fatal("accepted invalid batch")
			}
			if hashJSON(e.State.Backlog) != before {
				t.Fatal("partially applied batch")
			}
		})
	}
}

func TestParseAddTask(t *testing.T) {
	c, err := Parse([]string{"add-task", "tasks.json", "--json"})
	if err != nil || c.Command != "add-task" || len(c.Plans) != 1 || !c.JSON {
		t.Fatalf("parse: %+v %v", c, err)
	}
}

// waitForAddTaskPath blocks until path exists, so a fixture can observe a
// running controller without polling shared engine fields.
func waitForAddTaskPath(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func eventID(t *testing.T, e *Engine, query string, args ...any) int {
	t.Helper()
	var id int
	if err := e.Store.db.QueryRow(query, args...).Scan(&id); err != nil {
		t.Fatalf("event lookup %q: %v", query, err)
	}
	return id
}

// TestAddTaskRequestWhileWorkerRuns reproduces the controller panics of
// 2026-11-04 (sessions run-a1084b23a3fcfca4 and run-d87d53c1fea47bba): the
// request applied while one attempt implemented, cleared the live plan, and
// the next dispatch tick read a scope from the nil plan.
func TestAddTaskRequestWhileWorkerRuns(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agent.sh")
	marker, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	// The stub answers planning and review immediately, and holds each worker
	// until the fixture releases it, so the request lands mid-flight.
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
result="$LOOPER_FAKE_RESULT"
if printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result="$LOOPER_FAKE_PLAN"
elif printf '%s' "$p" | grep -q "^Complete task "; then
  id="$(printf '%s' "$p" | sed -n 's/^Complete task \(T[0-9][0-9A-Za-z_-]*\):.*/\1/p')"
  result="$(printf '{"task_id":"%s","status":"done","summary":"implemented","files":[],"blockers":[],"followups":[]}' "$id")"
  touch "` + marker + `"
  while [ ! -f "` + release + `" ]; do sleep 0.02; done
elif [ -n "$LOOPER_FAKE_REVIEW" ] && printf '%s' "$p" | grep -q "review pass for Looper"; then
  result="$LOOPER_FAKE_REVIEW"
fi
if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
if [ -n "$result" ]; then printf '{"structured_output":%s}\n' "$result"; fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["src/"]},{"task_id":"T002","write_paths":["src/"]}]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	c := repoConfig(t, "run", "--jobs", "1")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.ReviewEvery = 100
	c.Once = true
	// The two planned tasks share one write scope and the added task depends
	// on the first, so exactly one attempt is ever in flight. That keeps the
	// fixture off the separate snapshot race between a publication and a
	// concurrent worker, which this test does not cover;
	// TestPublishOverlapsConcurrentWorkerWrites covers that overlap.
	b := Backlog{Version: 1, Sources: []string{}, Goal: "add tasks while work runs", Tasks: []Task{
		{ID: "T001", Title: "first", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "second", Priority: 1, Status: "todo"},
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
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fmt.Errorf("controller panic: %v", r)
			}
		}()
		code, err := e.Run(ctx)
		if err == nil && code != 0 {
			err = fmt.Errorf("unexpected exit %d", code)
		}
		done <- err
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(testBudget(5 * time.Second)):
			t.Error("run did not stop")
		}
	}()
	waitForAddTaskPath(t, marker, "the first worker to start")
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("add"), Type: RequestTypeAddTask, Session: e.controller.Session, Tasks: []Task{
		{ID: "T003", Title: "added mid-run", Priority: 1, DependsOn: []string{"T001"}},
	}}
	if err = writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	ackPath := filepath.Join(ackDir(e.Dir), req.ID+".json")
	waitForAddTaskPath(t, ackPath, "the add-task acknowledgement")
	var ack SettingsAck
	if err = readJSON(ackPath, &ack); err != nil || ack.Status != AckApplied {
		t.Fatalf("add-task while running: %+v %v", ack, err)
	}
	// Release only after the request applied, so it lands on a busy loop.
	if err = os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("run timed out")
	}
	added := eventID(t, e, `SELECT id FROM events WHERE kind='add-task'`)
	first := eventID(t, e, `SELECT id FROM events WHERE kind='start' AND message LIKE 'T001  %'`)
	result := eventID(t, e, `SELECT id FROM events WHERE kind='result' AND message LIKE 'T001  %'`)
	if !(first < added && added < result) {
		t.Fatalf("the request applied outside the running attempt: start %d, add %d, result %d", first, added, result)
	}
	var replans int
	if err = e.Store.db.QueryRow(`SELECT count(*) FROM events WHERE kind='plan' AND id > ?`, added).Scan(&replans); err != nil {
		t.Fatal(err)
	}
	if replans == 0 {
		t.Fatal("the plan never rebuilt after the applied request")
	}
	for _, id := range []string{"T001", "T002", "T003"} {
		if e.State.Backlog.Task(id) == nil || e.State.Backlog.Task(id).Status != "done" {
			t.Fatalf("%s did not complete: %+v", id, e.State.Backlog.Task(id))
		}
	}
	if e.State.Plan == nil {
		t.Fatal("the rebuilt plan was not retained")
	}
	if got := e.State.Plan.Scope("T003").Paths; len(got) != 1 || got[0] != "." {
		t.Fatalf("the added task kept no conservative scope: %v", got)
	}
}
