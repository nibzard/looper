package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The OW-07 and OW-08 acceptance fixtures. A fresh requeue retires every
// reusable attempt as evidence, keeps the task identity and history, and
// makes the next dispatch start clean under the current worker role. Every
// refusal preserves the attempts, and a keyed retry retires nothing twice.

// freshRequeueEngine saves one run with a blocked task and one reusable
// blocked attempt, with the task file matching the accepted export.
func freshRequeueEngine(t *testing.T) (*Engine, *Attempt, io.Closer) {
	t.Helper()
	bin, _ := fakeAgent(t, `{"task_id":"T001","status":"done","summary":"finished","files":[],"blockers":[],"followups":[],"unblock_when":""}`)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	b := Backlog{Version: 1, Sources: []string{}, Goal: "fresh requeue", Tasks: []Task{
		{ID: "T001", Title: "blocked work", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, b); err != nil {
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
	work, err := e.Repo.Workspace(identifier("task"), e.State.Tip, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Repo.Remove(work) })
	id := identifier("task")
	dir := filepath.Join(e.Dir, id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task("T001")
	task.Status = "blocked"
	task.Blockers = []string{"needs an operator decision"}
	snapshot := *task
	a := &Attempt{ID: id, Task: snapshot, Agent: "codex", Base: e.State.Tip, Worktree: work, Directory: dir,
		Ref: "refs/looper-tasks/" + id, Phase: "blocked", Worktrees: []string{work},
		Summary: &Summary{TaskID: "T001", Status: "blocked", Summary: "Work is retained for recovery.", Files: []string{}, Blockers: []string{"needs an operator decision"}}}
	e.State.Attempts = append(e.State.Attempts, a)
	if err = e.export(); err != nil {
		t.Fatal(err)
	}
	return e, a, lock
}

// freshRequeueRequest builds one fresh requeue against the current task and
// attempt set.
func freshRequeueRequest(e *Engine, id string) *SettingsRequest {
	info, err := readController(e.Dir)
	if err != nil {
		panic(err)
	}
	if id == "" {
		id = identifier("settings")
	}
	t := e.State.Backlog.Task("T001")
	return &SettingsRequest{Version: SettingsRequestVersion, ID: id, Type: RequestTypeRequeueFresh,
		Session: info.Session, TaskID: "T001", TaskRevision: hashJSON(*t),
		AttemptsRevision: attemptSetRevision(e.State, "T001"),
		CreatedAt:        time.Now().UTC().Format(time.RFC3339Nano)}
}

// A fresh requeue retires the reusable attempt, keeps its evidence, and
// returns the task to todo with its identity and history intact.
func TestRequeueFreshRetiresReusableAttempts(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	before := a.Worktree
	ack := e.applyRequeueFreshRequest(freshRequeueRequest(e, "fresh-1"))
	if ack.Status != AckApplied {
		t.Fatalf("ack: %+v", ack)
	}
	if a.Phase != "blocked" || a.Retired == nil {
		t.Fatalf("the attempt must stay blocked as retired evidence: %+v", a.Retired)
	}
	if a.Retired.RequestID != "fresh-1" || a.Retired.Reason != "fresh_requeue" || a.Retired.At == "" {
		t.Fatalf("retirement metadata: %+v", a.Retired)
	}
	task := e.State.Backlog.Task("T001")
	if task.Status != "todo" || len(task.Blockers) != 0 {
		t.Fatalf("task after fresh requeue: %+v", task)
	}
	if task.ID != "T001" || len(task.DependsOn) != 0 {
		t.Fatalf("the task identity changed: %+v", task)
	}
	if _, err := os.Stat(before); err != nil {
		t.Fatal("retirement removed the attempt worktree")
	}
	// The export wrote the reopened task to the accepted file.
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		t.Fatal(err)
	}
	var exported Backlog
	if err = json.Unmarshal(data, &exported); err != nil {
		t.Fatal(err)
	}
	if exported.Tasks[0].Status != "todo" {
		t.Fatalf("the accepted export still holds %q", exported.Tasks[0].Status)
	}
}

// After a fresh requeue, the next dispatch creates a new attempt instead of
// resuming the retired one, and the retired attempt names its successor.
func TestRequeueFreshNextDispatchStartsClean(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	if ack := e.applyRequeueFreshRequest(freshRequeueRequest(e, "fresh-2")); ack.Status != AckApplied {
		t.Fatalf("ack: %+v", ack)
	}
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T001", Paths: []string{"."}}}}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{[]string{e.State.Backlog.Task("T001").Contract()}, e.Config.Setup, e.Config.Verify})
	results := make(chan workerResult, 1)
	ctx, cancel := runCtx(t, 20*time.Second)
	defer cancel()
	next, err := e.start(ctx, *e.State.Backlog.Task("T001"), results)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if next == a {
		t.Fatal("the dispatch resumed the retired attempt")
	}
	if next.ID == a.ID || next.Worktree == a.Worktree {
		t.Fatalf("the dispatch reused the retired attempt's identity or worktree")
	}
	if a.Retired == nil || a.Retired.Successor != next.ID {
		t.Fatalf("the retired attempt must name its successor: %+v", a.Retired)
	}
	select {
	case result := <-results:
		if result.Err != nil {
			t.Fatalf("worker: %v", result.Err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not finish")
	}
	e.wg.Wait()
}

// Every refusal preserves the attempts and the task, and records why.
func TestRequeueFreshRefusals(t *testing.T) {
	t.Run("stale task revision", func(t *testing.T) {
		e, _, _ := freshRequeueEngine(t)
		req := freshRequeueRequest(e, "fresh-rev")
		req.TaskRevision = strings.Repeat("0", 8)
		if ack := e.applyRequeueFreshRequest(req); ack.Status != AckConflict {
			t.Fatalf("ack: %+v", ack)
		}
		if e.State.Backlog.Task("T001").Status != "blocked" {
			t.Fatal("the refusal reopened the task")
		}
	})
	t.Run("active work holds the run", func(t *testing.T) {
		e, a, _ := freshRequeueEngine(t)
		a.Phase = "running"
		if ack := e.applyRequeueFreshRequest(freshRequeueRequest(e, "fresh-active")); ack.Status != AckConflict {
			t.Fatalf("ack: %+v", ack)
		}
	})
	t.Run("deferred task", func(t *testing.T) {
		e, _, _ := freshRequeueEngine(t)
		e.State.Backlog.Task("T001").Tags = []string{deferredTaskTag}
		if ack := e.applyRequeueFreshRequest(freshRequeueRequest(e, "fresh-defer")); ack.Status != AckConflict {
			t.Fatalf("ack: %+v", ack)
		}
	})
	t.Run("attempt set changed", func(t *testing.T) {
		e, _, _ := freshRequeueEngine(t)
		req := freshRequeueRequest(e, "fresh-attempts")
		req.AttemptsRevision = strings.Repeat("1", 64)
		if ack := e.applyRequeueFreshRequest(req); ack.Status != AckConflict {
			t.Fatalf("ack: %+v", ack)
		}
		if e.State.Attempts[0].Retired != nil {
			t.Fatal("the refusal retired an attempt")
		}
	})
	t.Run("stale session", func(t *testing.T) {
		e, _, _ := freshRequeueEngine(t)
		req := freshRequeueRequest(e, "fresh-session")
		req.Session = "run-older"
		if ack := e.applyRequeueFreshRequest(req); ack.Status != AckStale {
			t.Fatalf("ack: %+v", ack)
		}
	})
}

// An export failure after the mutation rolls the retirement back and
// records the failed receipt.
func TestRequeueFreshRollsBackWhenExportFails(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	// The export cannot write the accepted file while the project root
	// rejects writes.
	if err := os.Chmod(e.Config.Root, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(e.Config.Root, 0700)
	ack := e.applyRequeueFreshRequest(freshRequeueRequest(e, "fresh-rollback"))
	if ack.Status != AckError {
		t.Fatalf("ack: %+v", ack)
	}
	if a.Retired != nil {
		t.Fatal("the rollback kept the retirement")
	}
	if e.State.Backlog.Task("T001").Status != "blocked" {
		t.Fatal("the rollback lost the blocked status")
	}
}

// A caller key replays the original outcome and retires nothing twice.
func TestRequeueFreshIdempotencyKey(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	req := freshRequeueRequest(e, "fresh-key")
	req.IdempotencyKey = "requeue-once"
	if ack := e.applyRequeueFreshRequest(req); ack.Status != AckApplied {
		t.Fatalf("ack: %+v", ack)
	}
	// The retry carries the original payload; only the identity differs.
	retry := *req
	retry.ID = "fresh-key-retry"

	replay := e.applyRequeueFreshRequest(&retry)
	if replay.Status != AckApplied {
		t.Fatalf("replay ack: %+v", replay)
	}
	if a.Retired == nil || a.Retired.RequestID != req.ID {
		t.Fatalf("the replay must not retire again: %+v", a.Retired)
	}
	if len(e.State.Attempts) != 1 {
		t.Fatalf("the replay added attempts: %d", len(e.State.Attempts))
	}
}

// The controller loop claims a requeue-fresh envelope and answers it.
func TestRequeueFreshRequestLoopClaim(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	req := freshRequeueRequest(e, "")
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), *req); err != nil {
		t.Fatal(err)
	}
	e.processRequeueFreshRequests()
	if e.err != nil {
		t.Fatalf("claim failed: %v", e.err)
	}
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Status != AckApplied {
		t.Fatalf("ack: %+v", ack)
	}
	if a.Retired == nil {
		t.Fatal("the claimed application retired nothing")
	}
	if _, statErr := os.Stat(filepath.Join(processingDir(e.Dir), req.ID+".json")); !os.IsNotExist(statErr) {
		t.Fatal("the envelope stayed in processing")
	}
}

// The preview reports the retirement read-only, and the stopped application
// retires through the same checked path as the live controller.
func TestRequeueCLIStoppedRun(t *testing.T) {
	e, a, lock := freshRequeueEngine(t)
	// The CLI runs on a stopped run: release the engine first.
	work := a.Worktree
	if err := e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	cc := v2Config(e.Config, "requeue", false)
	cc.RequeueTask, cc.RequeueFresh, cc.Preview = "T001", true, true
	if code := runRequeue(cc, &out, &stderr); code != exitOK {
		t.Fatalf("preview exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "retired as evidence") || !strings.Contains(out.String(), "read-only") {
		t.Fatalf("preview text: %s", out.String())
	}
	if a.Retired != nil {
		t.Fatal("the preview retired an attempt")
	}
	out.Reset()
	stderr.Reset()
	if code := runRequeue(cc, &out, &stderr); code != exitOK {
		t.Fatalf("preview is not idempotent: %d", code)
	}
	cc.Preview = false
	if code := runRequeue(cc, &out, &stderr); code != exitOK {
		t.Fatalf("apply exit %d: %s%s", code, out.String(), stderr.String())
	}
	// The stopped application retired the attempt the store reloaded, not
	// the engine's in-memory copy.
	_, dir := projectPaths(e.Config)
	saved := savedState(t, dir)
	retired := false
	for _, sa := range saved.Attempts {
		if sa.ID == a.ID && sa.Retired != nil && sa.Retired.Reason == "fresh_requeue" {
			retired = true
		}
	}
	if !retired {
		t.Fatalf("stopped application retired nothing: attempts=%+v", saved.Attempts)
	}
	if saved.Backlog.Task("T001").Status != "todo" {
		t.Fatalf("the stopped application did not reopen the task: %s", saved.Backlog.Task("T001").Status)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal("the stopped application removed the worktree")
	}
}

// The ordinary requeue keeps the retained attempt for reuse; the fresh
// requeue retires it. The two never mix.
func TestRequeueOrdinaryKeepsRetainedWork(t *testing.T) {
	e, a, _ := freshRequeueEngine(t)
	info, err := readController(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	tt := e.State.Backlog.Task("T001")
	req := &SettingsRequest{Version: SettingsRequestVersion, ID: "plain-requeue", Type: RequestTypeRequeue,
		Session: info.Session, TaskID: "T001", TaskRevision: hashJSON(*tt),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ack := e.applyRequeueRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("ack: %+v", ack)
	}
	if a.Retired != nil {
		t.Fatal("the ordinary requeue retired the attempt")
	}
	if e.State.Backlog.Task("T001").Status != "todo" {
		t.Fatal("the ordinary requeue did not reopen the task")
	}
}

// A controller that does not advertise requeue-fresh is refused before any
// envelope is published.
func TestRequeueFreshCapabilityRefused(t *testing.T) {
	e, _, _ := freshRequeueEngine(t)
	older := &ControllerInfo{Version: 1, Session: "run-old", PID: os.Getpid()}
	if err := validateControllerRequest(older, PublicRequestRequeueFresh); err == nil {
		t.Fatal("an older controller must refuse requeue-fresh")
	} else if _, ok := err.(*unsupportedRequestError); !ok {
		t.Fatalf("refusal type: %T", err)
	}
	current := &ControllerInfo{Version: 1, Session: "run-new", PID: os.Getpid(),
		Capabilities: &ControllerCapabilities{ContractVersions: []int{1, 2}, RequestTypes: controllerRequestTypes()}}
	if err := validateControllerRequest(current, PublicRequestRequeueFresh); err != nil {
		t.Fatalf("a current controller must accept requeue-fresh: %v", err)
	}
	_ = e
}
