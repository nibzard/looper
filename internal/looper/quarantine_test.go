package looper

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// quarantineRunEngine builds a finite run whose backlog carries one blocked
// task with a files entry no plan scope can use, next to ordinary todo work.
// The planner answer is injectable so a test can reproduce the incident where
// the planner copied the invalid files entry into its write paths.
func quarantineRunEngine(t *testing.T, plan string, backlog Backlog) (*Engine, Config, func()) {
	t.Helper()
	return quarantineReportEngine(t, plan, backlog, `{"task_id":"T001","status":"done","summary":"implemented","files":[],"blockers":[]}`)
}

// quarantineReportEngine is quarantineRunEngine with the worker's summary
// injectable, so a test can reproduce a report that itself carries an entry no
// plan scope can use.
func quarantineReportEngine(t *testing.T, plan string, backlog Backlog, summary string) (*Engine, Config, func()) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", plan)
	t.Setenv("LOOPER_FAKE_SUMMARY", summary)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.Once = true
	if err := writeJSON(c.Todo, backlog); err != nil {
		t.Fatal(err)
	}
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
	return e, c, shutdown
}

// quarantineMessages returns the recorded quarantine events for one task.
func quarantineMessages(t *testing.T, e *Engine) []string {
	t.Helper()
	rows, err := e.Store.db.Query(`SELECT message FROM events WHERE kind='quarantine' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var messages []string
	for rows.Next() {
		var message string
		if err = rows.Scan(&message); err != nil {
			t.Fatal(err)
		}
		messages = append(messages, message)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return messages
}

func readTodoBacklog(t *testing.T, c Config) Backlog {
	t.Helper()
	var b Backlog
	if err := readJSON(c.Todo, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

// TestWritePathErrorIdentifiesPlanTask pins the typed validation error and the
// scope drop that lets one bad task leave the rest of a plan usable.
func TestWritePathErrorIdentifiesPlanTask(t *testing.T) {
	b := testBacklog()
	p := Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T1", Paths: []string{"src/"}},
		{TaskID: "T2", Paths: []string{"/outside/abs"}},
	}}
	err := p.Validate(b, "")
	var bad *writePathError
	if !errors.As(err, &bad) {
		t.Fatalf("an invalid write path must stay a typed error, got %v", err)
	}
	if bad.TaskID != "T2" || bad.Path != "/outside/abs" {
		t.Fatalf("the error must identify the task and path: %+v", bad)
	}
	if want := `invalid write path "/outside/abs"`; bad.Error() != want {
		t.Fatalf("message changed: %q", bad.Error())
	}
	p.dropScope("T2")
	if err = p.Validate(b, ""); err != nil {
		t.Fatalf("dropping the scope must repair the plan: %v", err)
	}
	if got := p.Scope("T2"); len(got.Paths) != 1 || got.Paths[0] != "." {
		t.Fatalf("the unfinished task must return to the repository default: %+v", got)
	}
	if got := p.Scope("T1"); len(got.Paths) != 1 || got.Paths[0] != "src" {
		t.Fatalf("the healthy scope must survive: %+v", got)
	}
}

// TestQuarantineHoldsInvalidFilesTasks covers the startup scan: the task with
// a malformed files entry holds blocked with the validation error as its
// blocker, its recorded unblock condition survives, done tasks keep their
// outcome, and a repeated scan is a no-op.
func TestQuarantineHoldsInvalidFilesTasks(t *testing.T) {
	c := repoConfig(t, "run")
	b := Backlog{Version: 1, Sources: []string{}, Goal: "quarantine", Tasks: []Task{
		{ID: "T001", Title: "clean", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "malformed", Priority: 2, Status: "blocked",
			Files:       []string{"notes/report.md", "/notes/absolute.md", "../escape.md"},
			Blockers:    []string{"owner decision"},
			UnblockWhen: "The owner records the decision."},
		{ID: "T003", Title: "finished", Priority: 1, Status: "done", Tags: []string{"project-done"},
			Files: []string{"/done/absolute.md"}},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	if err = e.initializeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = e.quarantineInvalidFiles(); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T002")
	if task.Status != "blocked" {
		t.Fatalf("the malformed task must hold blocked: %+v", task)
	}
	for _, reason := range []string{`invalid write path "/notes/absolute.md"`, `invalid write path "../escape.md"`} {
		if !hasBlocker(task.Blockers, reason) {
			t.Fatalf("blocker missing %q: %v", reason, task.Blockers)
		}
	}
	if !hasBlocker(task.Blockers, "owner decision") {
		t.Fatalf("the recorded blocker must survive: %v", task.Blockers)
	}
	if task.UnblockWhen != "The owner records the decision." {
		t.Fatalf("the recorded unblock condition must survive: %q", task.UnblockWhen)
	}
	if clean := e.State.Backlog.Task("T001"); clean.Status != "todo" || len(clean.Blockers) != 0 {
		t.Fatalf("the healthy task changed: %+v", clean)
	}
	if done := e.State.Backlog.Task("T003"); done.Status != "done" || len(done.Blockers) != 0 {
		t.Fatalf("a done task must keep its outcome: %+v", done)
	}
	messages := quarantineMessages(t, e)
	if len(messages) != 2 {
		t.Fatalf("expected one quarantine event per malformed entry, got %v", messages)
	}
	for _, message := range messages {
		if !strings.Contains(message, "T002") {
			t.Fatalf("the event must name the rejected task: %q", message)
		}
	}
	// The quarantine reached the exported task file, and the file the
	// controller accepted matches the state.
	exported := readTodoBacklog(t, c)
	if exportedTask := exported.Task("T002"); !hasBlocker(exportedTask.Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("the export lacks the quarantine blocker: %v", exportedTask.Blockers)
	}
	if digestFile(c.Todo) != e.State.ExportHash {
		t.Fatal("the exported task file does not match the accepted export")
	}
	// A repeated scan changes nothing and stays silent.
	if err = e.quarantineInvalidFiles(); err != nil {
		t.Fatal(err)
	}
	if again := quarantineMessages(t, e); len(again) != len(messages) {
		t.Fatalf("a repeated quarantine repeated events: %v", again)
	}
	if task = e.State.Backlog.Task("T002"); len(task.Blockers) != 3 {
		t.Fatalf("a repeated quarantine duplicated blockers: %v", task.Blockers)
	}
}

// TestStartupQuarantinesInvalidTaskAndDispatchesOthers is the acceptance: the
// incident backlog — one blocked task whose files list carried an absolute
// path, which the planner copied into its write paths — starts the controller,
// logs the rejected task, and dispatches the others instead of dying at
// startup with "invalid write path".
func TestStartupQuarantinesInvalidTaskAndDispatchesOthers(t *testing.T) {
	log := agentLogPath(t)
	plan := `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]},{"task_id":"T002","write_paths":["/notes/absolute.md"]}]}`
	backlog := Backlog{Version: 1, Sources: []string{}, Goal: "quarantine at startup", Tasks: []Task{
		{ID: "T001", Title: "implement", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "triage", Priority: 2, Status: "blocked", Files: []string{"/notes/absolute.md"}},
	}}
	e, c, _ := quarantineRunEngine(t, plan, backlog)
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 2 || err != nil {
		t.Fatalf("the run must survive the malformed entry and stop on the blocked remainder: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "no_runnable_tasks" {
		t.Fatalf("unexpected exit: %+v", exit)
	}
	if task := e.State.Backlog.Task("T001"); task == nil || task.Status != "done" {
		t.Fatalf("the healthy task did not dispatch and finish: %+v", task)
	}
	task := e.State.Backlog.Task("T002")
	if task.Status != "blocked" || !hasBlocker(task.Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("the rejected task must hold blocked with the validation error: %+v", task)
	}
	messages := quarantineMessages(t, e)
	if len(messages) == 0 || !strings.Contains(messages[len(messages)-1], "T002") {
		t.Fatalf("the rejected task was not logged: %v", messages)
	}
	if e.State.Plan == nil {
		t.Fatal("the run must keep its plan")
	}
	if got := e.State.Plan.Scope("T002"); len(got.Paths) != 1 || got.Paths[0] != "." {
		t.Fatalf("the rejected scope must return to the repository default: %+v", got)
	}
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("expected exactly one implementation run, got %d", n)
	}
	if exported := readTodoBacklog(t, c); !hasBlocker(exported.Task("T002").Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("the export lacks the quarantine: %v", exported.Task("T002").Blockers)
	}
}

// TestPlannerInvalidScopeQuarantinesTask covers the same defect when the
// planner introduces the invalid path itself: the affected task quarantines,
// the rest of the backlog still plans and dispatches.
func TestPlannerInvalidScopeQuarantinesTask(t *testing.T) {
	plan := `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]},{"task_id":"T002","write_paths":["/outside/abs"]}]}`
	backlog := Backlog{Version: 1, Sources: []string{}, Goal: "quarantine a planner scope", Tasks: []Task{
		{ID: "T001", Title: "healthy", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "malformed scope", Priority: 1, Status: "todo"},
	}}
	e, c, _ := quarantineRunEngine(t, plan, backlog)
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 2 || err != nil {
		t.Fatalf("the run must survive the invalid planner scope: %d %v", code, err)
	}
	healthy := e.State.Backlog.Task("T001")
	if healthy == nil || healthy.Status != "done" {
		t.Fatalf("the healthy task did not finish: %+v", healthy)
	}
	task := e.State.Backlog.Task("T002")
	if task.Status != "blocked" || !hasBlocker(task.Blockers, `invalid write path "/outside/abs"`) {
		t.Fatalf("the task must hold blocked with the validation error: %+v", task)
	}
	if task.UnblockWhen == "" {
		t.Fatal("the quarantine must record how to resume the task")
	}
	messages := quarantineMessages(t, e)
	if len(messages) != 1 || !strings.Contains(messages[0], "T002") {
		t.Fatalf("the rejected task was not logged once: %v", messages)
	}
	if got := e.State.Plan.Scope("T002"); len(got.Paths) != 1 || got.Paths[0] != "." {
		t.Fatalf("the rejected scope must return to the repository default: %+v", got)
	}
	if exported := readTodoBacklog(t, c); exported.Task("T002").Status != "blocked" {
		t.Fatal("the export lacks the quarantine")
	}
}

// TestRefreshQuarantinesImportedInvalidFiles covers an external task-file edit
// that introduces a malformed files entry while the controller is idle: the
// import accepts the edit and the same quarantine holds that task.
func TestRefreshQuarantinesImportedInvalidFiles(t *testing.T) {
	c := repoConfig(t, "run")
	b := Backlog{Version: 1, Sources: []string{}, Goal: "import quarantine", Tasks: []Task{
		{ID: "T001", Title: "clean", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	if err = e.initializeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(quarantineMessages(t, e)) != 0 {
		t.Fatal("a clean backlog must not quarantine anything")
	}
	edited := Backlog{Version: 1, Sources: []string{}, Goal: "import quarantine", Tasks: []Task{
		{ID: "T001", Title: "clean", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "imported", Priority: 1, Status: "todo", Files: []string{"/notes/absolute.md"}},
	}}
	if err = writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	if err = e.refreshIdle(); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T002")
	if task.Status != "blocked" || !hasBlocker(task.Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("the imported malformed task must hold blocked: %+v", task)
	}
	if digestFile(c.Todo) != e.State.ExportHash {
		t.Fatal("the exported task file does not match the accepted export")
	}
	messages := quarantineMessages(t, e)
	if len(messages) != 1 || !strings.Contains(messages[0], "T002") {
		t.Fatalf("the imported rejection was not logged: %v", messages)
	}
}

// TestQuarantineSurvivesRestart proves the quarantined entry stays recorded
// across a restart without repeating itself, and the controller keeps
// starting.
func TestQuarantineSurvivesRestart(t *testing.T) {
	log := agentLogPath(t)
	plan := `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]}]}`
	backlog := Backlog{Version: 1, Sources: []string{}, Goal: "quarantine restart", Tasks: []Task{
		{ID: "T001", Title: "implement", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "triage", Priority: 2, Status: "blocked", Files: []string{"/notes/absolute.md"}},
	}}
	e, c, stop := quarantineRunEngine(t, plan, backlog)
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	if code, err := e.Run(ctx); code != 2 || err != nil {
		t.Fatalf("first run: %d %v", code, err)
	}
	// Release the repository lock before the restart claims it.
	stop()
	// Restart over the finished state: the quarantine must not repeat, the
	// completed task must stay done, and the rejected task stays blocked.
	restart, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restart.Store.Close(); lock.Close() })
	ctx2, cancel2 := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel2()
	restartCode, restartErr := restart.Run(ctx2)
	if restartCode != 2 || restartErr != nil {
		t.Fatalf("restart: %d %v", restartCode, restartErr)
	}
	task := restart.State.Backlog.Task("T002")
	if task.Status != "blocked" || !hasBlocker(task.Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("the quarantine did not survive the restart: %+v", task)
	}
	if done := restart.State.Backlog.Task("T001"); done == nil || done.Status != "done" {
		t.Fatalf("the completed task did not survive: %+v", done)
	}
	var total int
	if err = restart.Store.db.QueryRow(`SELECT count(*) FROM events WHERE kind='quarantine'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("the restart repeated the quarantine: %d events", total)
	}
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("the restart repeated implementation: %d runs", n)
	}
}

// TestReportedInvalidFilesAreRejectedAtTheMerge covers the merge-side repair:
// a worker summary that reports a path no scope can use is filtered at the
// moment of the report, so the task's files list stays scannable, each
// rejected entry is logged once, the reported outcome survives, and the next
// startup scan finds nothing left to quarantine.
func TestReportedInvalidFilesAreRejectedAtTheMerge(t *testing.T) {
	log := agentLogPath(t)
	plan := `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["one/"]}]}`
	backlog := Backlog{Version: 1, Sources: []string{}, Goal: "merge quarantine", Tasks: []Task{
		{ID: "T001", Title: "triage", Priority: 1, Status: "todo"},
	}}
	summary := `{"task_id":"T001","status":"blocked","summary":"needs the fixture","files":["one/report.md","/notes/absolute.md","../escape.md"],"blockers":["fixture input"]}`
	e, c, stop := quarantineReportEngine(t, plan, backlog, summary)
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
	defer cancel()
	if code, err := e.Run(ctx); code != 2 || err != nil {
		t.Fatalf("the run must apply the blocked report: %d %v", code, err)
	}
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("expected exactly one implementation run, got %d", n)
	}
	task := e.State.Backlog.Task("T001")
	if task.Status != "blocked" || !hasBlocker(task.Blockers, "fixture input") {
		t.Fatalf("the reported outcome must survive the rejected entries: %+v", task)
	}
	if hasBlocker(task.Blockers, `invalid write path "/notes/absolute.md"`) {
		t.Fatalf("a rejected entry must not become a hold: %+v", task)
	}
	if len(task.Files) != 1 || task.Files[0] != "one/report.md" {
		t.Fatalf("the files list must keep only scannable entries: %v", task.Files)
	}
	messages := quarantineMessages(t, e)
	if len(messages) != 2 {
		t.Fatalf("expected one rejection per malformed entry, got %v", messages)
	}
	for _, reason := range []string{`invalid write path "/notes/absolute.md"`, `invalid write path "../escape.md"`} {
		found := false
		for _, message := range messages {
			if strings.Contains(message, "T001") && strings.Contains(message, reason) {
				found = true
			}
		}
		if !found {
			t.Fatalf("the rejection was not logged: %v", messages)
		}
	}
	// The export carries the same scannable list, so the entry never reaches
	// the task file and a restart has nothing to quarantine.
	exported := readTodoBacklog(t, c)
	if files := exported.Task("T001").Files; len(files) != 1 || files[0] != "one/report.md" {
		t.Fatalf("the export carries a rejected entry: %v", files)
	}
	stop()
	restart, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restart.Store.Close(); lock.Close() })
	if err = restart.initializeState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = restart.quarantineInvalidFiles(); err != nil {
		t.Fatal(err)
	}
	after := restart.State.Backlog.Task("T001")
	if after.Status != "blocked" || len(after.Blockers) != 1 || !hasBlocker(after.Blockers, "fixture input") {
		t.Fatalf("the startup scan quarantined a task whose report was already filtered: %+v", after)
	}
	if len(quarantineMessages(t, restart)) != len(messages) {
		t.Fatalf("the startup scan repeated the rejection: %v", quarantineMessages(t, restart))
	}
}
