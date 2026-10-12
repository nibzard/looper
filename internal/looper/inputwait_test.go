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

const malformedBacklog = `{"schema_version":1,`

func invalidStatusBacklog() []byte {
	return []byte(`{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","title":"one","priority":1,"status":"bogus"}]}`)
}

// idleEngine builds an initialized engine around one done task with a
// finished, fully reviewed state. Tests drive its methods directly; no Run
// loop exists, so engine state is safe to touch.
func idleEngine(t *testing.T, reviewResult string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_REVIEW", reviewResult)
	c := repoConfig(t, "run")
	c.Poll = 40 * time.Millisecond
	c.CodexBin = bin
	c.ClaudeBin = bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "wait", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
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
	e.State.Finished = true
	e.State.ReviewedTip = e.State.Tip
	e.State.ReviewedAt = 1
	e.State.Integrated = 1
	if err = e.save(); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	return e, c
}

// runEngine starts the real Run loop around an idle engine and returns its
// exit channel plus the cancel function for the interrupt path.
func runEngine(t *testing.T, reviewResult string) (*Engine, Config, chan int, context.CancelFunc) {
	t.Helper()
	e, c := idleEngine(t, reviewResult)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	exited := make(chan struct{})
	go func() {
		code, _ := e.Run(ctx)
		done <- code
		close(exited)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("engine did not exit after cancel")
		}
		e.Store.Close()
	})
	return e, c, done, cancel
}

// waitEvent blocks until the journal holds at least count waiting events whose
// message contains substr.
func waitEvent(t *testing.T, dir, substr string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if countEvent(t, dir, substr) >= count {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d waiting events containing %q", count, substr)
}

func countEvent(t *testing.T, dir, substr string) int {
	t.Helper()
	s, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.RecentEvents(-1, 300)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Kind == "waiting" && strings.Contains(ev.Message, substr) {
			count++
		}
	}
	return count
}

func TestRefreshIdleClassifiesInvalidInput(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	err := e.refreshIdle()
	var bad *invalidBacklogError
	if !errors.As(err, &bad) {
		t.Fatalf("malformed JSON must classify as invalid input: %v", err)
	}
	if err = os.WriteFile(c.Todo, invalidStatusBacklog(), 0600); err != nil {
		t.Fatal(err)
	}
	if err = e.refreshIdle(); !errors.As(err, &bad) {
		t.Fatalf("invalid task status must classify as invalid input: %v", err)
	}
	// A missing file stays a fatal access error.
	if err = os.Remove(c.Todo); err != nil {
		t.Fatal(err)
	}
	if err = e.refreshIdle(); errors.As(err, &bad) {
		t.Fatal("file access errors must not classify as invalid input")
	}
	// Git failures keep their fatal behavior as well.
	todo := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
	}}
	if err = writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e.Repo.Root = filepath.Join(e.Repo.Root, "gone")
	err = e.refreshIdle()
	if errors.As(err, &bad) {
		t.Fatal("git failures must not classify as invalid input")
	}
	if err == nil {
		t.Fatal("a broken repository must still fail the refresh")
	}
}

func TestIdleInputWaitDecision(t *testing.T) {
	e, _ := idleEngine(t, reviewComplete)
	typed := &invalidBacklogError{errors.New("bad")}
	if !e.idleInputWait(typed) {
		t.Fatal("a continuous run must wait for corrected input")
	}
	if e.idleInputWait(errors.New("unrelated")) {
		t.Fatal("only invalid input waits")
	}
	e.Config.Once = true
	if e.idleInputWait(typed) {
		t.Fatal("a finite run must fail on invalid idle input")
	}
}

func TestInputWaitPublishesImmediately(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := "completed goal"
		if ready {
			name = "ready task"
		}
		t.Run(name, func(t *testing.T) {
			e, c := idleEngine(t, reviewComplete)
			if ready {
				e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, Task{ID: "T2", Title: "two", Priority: 1, Status: "todo"})
				e.State.Finished = false
			}
			before := hashJSON(e.State)
			e.publishView(false)
			e.waitForInput(errors.New(c.Todo + ": invalid status"))
			v := LoadView(e.Dir)
			if v == nil || v.Finished || v.State != "waiting" || !strings.Contains(v.ActivityDetail, c.Todo) || !strings.Contains(v.ActivityDetail, "Correct the file") {
				t.Fatalf("input wait was not published before returning: %+v", v)
			}
			if ready && v.Counters.Ready != 1 {
				t.Fatal("the snapshot must retain the accepted ready task")
			}
			e.waitForInput(errors.New(c.Todo + ": unknown dependency"))
			if v = LoadView(e.Dir); v == nil || !strings.Contains(v.ActivityDetail, "unknown dependency") {
				t.Fatalf("changed input error was not published immediately: %+v", v)
			}
			e.resumeFromInputWait()
			v = LoadView(e.Dir)
			if v == nil || v.State != "" || v.ActivityDetail != "" || v.Finished != !ready {
				t.Fatalf("correction was not published before returning: %+v", v)
			}
			if hashJSON(e.State) != before {
				t.Fatal("presentation must not change the accepted state")
			}
		})
	}
}

func TestRunWaitsForCorrectedInputAndResumes(t *testing.T) {
	e, c, done, _ := runEngine(t, reviewComplete)
	waitEvent(t, e.Dir, "Waiting for changed requirements", 1)

	// Malformed input keeps the run alive without touching the file.
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "needs correction", 1)
	time.Sleep(300 * time.Millisecond)
	select {
	case code := <-done:
		t.Fatalf("run died on invalid input: %d", code)
	default:
	}
	raw, err := os.ReadFile(c.Todo)
	if err != nil || string(raw) != malformedBacklog {
		t.Fatal("the invalid file must be preserved byte for byte")
	}
	if countEvent(t, e.Dir, "needs correction") != 1 {
		t.Fatal("the diagnostic must be emitted once per problem, not per poll")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if v := LoadView(e.Dir); v != nil && !v.Finished {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v := LoadView(e.Dir); v == nil || v.Finished {
		t.Fatal("the completion banner must stay suppressed while input needs correction")
	}

	// A different problem is reported again; the wait continues.
	if err = os.WriteFile(c.Todo, invalidStatusBacklog(), 0600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "needs correction", 2)
	select {
	case code := <-done:
		t.Fatalf("run died on a changed problem: %d", code)
	default:
	}

	// A corrected backlog resumes in the same process: the baseline accepts
	// the new done task and the final review finishes the goal again.
	corrected := Backlog{Version: 1, Sources: []string{}, Goal: "wait", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
		{ID: "T2", Title: "two", Priority: 1, Status: "done"},
	}}
	if err = writeJSON(c.Todo, corrected); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "accepted again", 1)
	// The completion banner returns once the input is accepted again.
	bannerDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(bannerDeadline) {
		if v := LoadView(e.Dir); v != nil && v.Finished {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if v := LoadView(e.Dir); v == nil || !v.Finished {
		t.Fatal("the completion banner must return after correction")
	}
	if err = os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("stop after correction must drain with code 0, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not drain after stop")
	}
	if !e.State.Finished {
		t.Fatal("the corrected backlog must resume toward completion")
	}
	if len(e.State.Attempts) != 0 {
		t.Fatal("the input wait must not launch agents")
	}
}

func TestRunStopDuringInputWaitDrainsWithZero(t *testing.T) {
	e, c, done, _ := runEngine(t, reviewComplete)
	waitEvent(t, e.Dir, "Waiting for changed requirements", 1)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "needs correction", 1)
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("stop during the input wait must drain with code 0, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not drain after stop")
	}
}

func TestRunInterruptDuringInputWaitReturns130(t *testing.T) {
	e, c, done, cancel := runEngine(t, reviewComplete)
	waitEvent(t, e.Dir, "Waiting for changed requirements", 1)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "needs correction", 1)
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatalf("interrupt during the input wait must return 130, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit after interrupt")
	}
}

func TestRequeueDuringInputWaitConflictsWithHint(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	e.inputPending = true

	// A blocked task in the accepted backlog is a valid requeue target.
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, Task{ID: "T2", Title: "two", Priority: 1, Status: "blocked", Blockers: []string{"missing input"}})
	revision := hashJSON(e.State.Backlog.Task("T2"))
	req := SettingsRequest{Version: SettingsRequestVersion, ID: "req-wait", Type: RequestTypeRequeue,
		Session: e.controller.Session, TaskID: "T2", TaskRevision: revision}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processRequeueRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Status != AckConflict {
		t.Fatalf("requeue during the wait must conflict: %+v", ack)
	}
	if !strings.Contains(ack.Message, "correction") {
		t.Fatalf("conflict must carry the correction hint: %q", ack.Message)
	}
	if raw, err := os.ReadFile(c.Todo); err != nil || string(raw) != malformedBacklog {
		t.Fatal("a requeue must never overwrite the invalid file")
	}
}

// A finite run must fail, not wait, when idle input turns invalid mid-run.
// The result event is the last read of the task file before the idle refresh,
// so corrupting after it lands in the idle branch deterministically.
func TestOnceInvalidIdleInputExitsOne(t *testing.T) {
	e, _, shutdown := assessRunEngine(t, "", 0)
	defer shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		code int
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		code, err := e.Run(ctx)
		done <- outcome{code, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, openErr := OpenStoreReadonly(filepath.Join(e.Dir, "state.db"))
		if openErr == nil {
			events, _ := s.RecentEvents(-1, 20)
			s.Close()
			for _, ev := range events {
				if ev.Kind == "result" {
					if err := os.WriteFile(e.Config.Todo, []byte(malformedBacklog), 0600); err != nil {
						t.Fatal(err)
					}
					select {
					case out := <-done:
						if out.code != 1 {
							t.Fatalf("invalid idle input under --once must exit 1, got %d (%v)", out.code, out.err)
						}
						var bad *invalidBacklogError
						if !errors.As(out.err, &bad) {
							t.Fatalf("the exit must come from the input branch: %v", out.err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("run did not exit on invalid idle input")
					}
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("the worker result never landed")
}

func TestPlanImportWaitsForCorrectedInput(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	e.inputPending = true

	plan := planFile(t, "# New requirements\nAdd telemetry to the service.\n")
	// The proposal covers no existing task; the backlog here uses id T1.
	t.Setenv("LOOPER_FAKE_RESULT", `{"additions":[{"id":"P1","title":"Add telemetry","priority":2,"status":"todo","details":"Acceptance: telemetry endpoint returns counters.","depends_on":[]}],"covered":[],"conflicts":[]}`)
	id, err := submitPlanImport(e.Dir, e.controller.Session, []string{plan})
	if err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record := e.importReceipt(id)
	if record == nil {
		t.Fatal("import was not claimed and queued")
	}
	if record.Status != ImportQueued {
		t.Fatalf("import must stay queued while input is invalid: %s (%s)", record.Status, record.Error)
	}
	if raw, err := os.ReadFile(c.Todo); err != nil || string(raw) != malformedBacklog {
		t.Fatal("a queued import must never overwrite the invalid file")
	}

	// After correction the same receipt applies without resubmission.
	corrected := Backlog{Version: 1, Sources: []string{}, Goal: "wait", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
	}}
	if err = writeJSON(c.Todo, corrected); err != nil {
		t.Fatal(err)
	}
	if err = e.refreshIdle(); err != nil {
		t.Fatal(err)
	}
	e.inputPending = false
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record = e.importReceipt(id)
	if record.Status != ImportApplied {
		t.Fatalf("import must apply after correction: %s (%s)", record.Status, record.Error)
	}
	if e.State.Backlog.Task("T002") == nil {
		t.Fatal("the applied import must add its task")
	}
}

func TestCorrectionRunsNewTaskWithoutRepeatingCompletedWork(t *testing.T) {
	e, c, _ := assessRunEngine(t, "", 0)
	e.Config.Once = false
	e.Config.Poll = 40 * time.Millisecond
	log := filepath.Join(t.TempDir(), "workers")
	t.Setenv("LOOPER_TEST_WORKERS", log)
	bin := filepath.Join(t.TempDir(), "agent.sh")
	script := `#!/bin/bash
out=""; prev=""
for arg in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out=$arg; fi
  prev=$arg
done
p=$(cat)
case "$p" in
  'Complete task '*)
    id=${p#Complete task }; id=${id%%:*}
    printf '%s\n' "$id" >> "$LOOPER_TEST_WORKERS"
    printf 'implemented %s\n' "$id" > "$id.txt"
    result="{\"task_id\":\"$id\",\"status\":\"done\",\"summary\":\"implemented\",\"files\":[\"$id.txt\"],\"blockers\":[],\"followups\":[],\"unblock_when\":\"\"}"
    ;;
  'Plan execution for Looper'*)
    id=T1
    if grep -q '"id": "T2"' "$LOOPER_ARTIFACT_DIR/context/backlog.json"; then id=T2; fi
    result="{\"setup\":\"\",\"verify\":\"test -s $id.txt\",\"tasks\":[{\"task_id\":\"$id\",\"write_paths\":[\"$id.txt\"],\"resources\":[]}]}"
    ;;
  *) result='{"status":"reviewed","summary":"checked","findings":[],"complete":true}' ;;
esac
printf '%s' "$result" > "$out"
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	e.Config.CodexBin = bin
	e.Config.Agent = "codex"
	e.Config.ReviewAgent = "codex"
	session := e.controller.Session
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() { code, _ := e.Run(ctx); done <- code }()
	// Join before closing the store, including when an assertion fails.
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("controller did not stop")
			}
		}
	})
	waitEvent(t, e.Dir, "Waiting for changed requirements", 1)
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "needs correction", 1)
	workersBefore, err := os.ReadFile(log)
	if err != nil || string(workersBefore) != "T1\n" {
		t.Fatalf("initial worker: %q %v", workersBefore, err)
	}
	time.Sleep(3 * e.Config.Poll)
	workersWaiting, _ := os.ReadFile(log)
	if string(workersWaiting) != string(workersBefore) {
		t.Fatal("worker ran during input wait")
	}
	corrected := Backlog{Version: 1, Sources: []string{}, Goal: "assess", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, corrected); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, e.Dir, "accepted again", 1)
	deadline := time.Now().Add(10 * time.Second)
	for {
		var b Backlog
		if err := readJSON(c.Todo, &b); err == nil && b.Task("T2") != nil && b.Task("T2").Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new runnable task did not complete")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		joined = true
		if code != 0 {
			t.Fatalf("stop code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stop timed out")
	}
	workers, err := os.ReadFile(log)
	if err != nil || string(workers) != "T1\nT2\n" {
		t.Fatalf("completed work repeated: %q %v", workers, err)
	}
	if e.controller.Session != session {
		t.Fatal("controller session changed")
	}
	if data, err := os.ReadFile(filepath.Join(c.Root, "T2.txt")); err != nil || string(data) != "implemented T2\n" {
		t.Fatalf("new task did not publish: %q %v", data, err)
	}
}
