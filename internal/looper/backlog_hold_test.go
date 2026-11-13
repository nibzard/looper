package looper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holdVerify blocks each verification until the test releases it, so an
// external task-file edit can land while the check is in flight. Every check
// writes a marker into its own artifact directory, which lets tests count
// verifications. The quotes are JSON-escaped because the command travels
// inside the fake planner's JSON result.
const holdVerify = `touch \"$LOOPER_ARTIFACT_DIR/started\"; while [ ! -e \"$LOOPER_TEST_RELEASE\" ]; do sleep 0.05; done`

// holdRunEngine starts a continuous one-task run around the fake agent. The
// returned release path unblocks holdVerify; the test owns it outside the
// project root so it never changes the accepted tree.
func holdRunEngine(t *testing.T, verify string) (*Engine, Config, chan holdOutcome, string) {
	t.Helper()
	return holdRunEngineConfig(t, verify, nil)
}

// holdRunEngineConfig is holdRunEngine with a hook over the configuration, so
// a test can flip a mode flag without copying the run harness.
func holdRunEngineConfig(t *testing.T, verify string, configure func(*Config)) (*Engine, Config, chan holdOutcome, string) {
	t.Helper()
	bin, _ := fakeAgent(t, "{}")
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"`+verify+`","tasks":[{"task_id":"T1","write_paths":["."],"resources":[]}]}`)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T1","status":"done","summary":"implemented","files":["T1.txt"],"blockers":[]}`)
	t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("LOOPER_TEST_RELEASE", release)
	c := repoConfig(t, "run")
	c.Poll = 20 * time.Millisecond
	c.CodexBin = bin
	c.ClaudeBin = bin
	if configure != nil {
		configure(&c)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "hold", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan holdOutcome, 1)
	exited := make(chan struct{})
	go func() {
		code, err := e.Run(ctx)
		done <- holdOutcome{code, err}
		close(exited)
	}()
	joined := false
	t.Cleanup(func() {
		cancel()
		if !joined {
			select {
			case <-exited:
			case <-time.After(testBudget(15 * time.Second)):
				t.Error("controller did not stop")
			}
		}
		e.Store.Close()
		lock.Close()
	})
	return e, c, done, release
}

type holdOutcome struct {
	code int
	err  error
}

// editedBacklog adds one blocked task and one task depending on it, so the
// edit is valid but never dispatches.
func editedBacklog() Backlog {
	return Backlog{Version: 1, Sources: []string{}, Goal: "hold", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 2, Status: "todo", DependsOn: []string{"T3"}},
		{ID: "T3", Title: "three", Priority: 2, Status: "blocked", Blockers: []string{"missing input"}},
	}}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// checkMarkers counts verification markers across all check artifact
// directories.
func checkMarkers(t *testing.T, dir string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "task-*", "checks", "*", "started"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func countKindEvent(t *testing.T, dir, kind, substr string) int {
	t.Helper()
	s, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.RecentEvents(-1, 500)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.Kind == kind && strings.Contains(ev.Message, substr) {
			count++
		}
	}
	return count
}

// TestBacklogEditDuringCheckHoldsAndImports reproduces the archived-session
// incident: an external to-do.json edit that lands while a check is in flight
// must hold dispatch, import the edit at the boundary, and keep the run alive
// instead of ending it at the publication guard.
func TestBacklogEditDuringCheckHoldsAndImports(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The hold is reported once, then the edit imports and the verified work
	// re-verifies against the moved baseline.
	waitFor(t, "the hold to be reported", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "Dispatch is paused") >= 1
	})
	waitFor(t, "the edit to import", func() bool {
		return countKindEvent(t, e.Dir, "refresh", "Imported changed project") >= 1
	})
	waitFor(t, "the re-verification", func() bool { return checkMarkers(t, e.Dir) >= 2 })
	waitFor(t, "T1 to finish", func() bool {
		var b Backlog
		return readJSON(c.Todo, &b) == nil && b.Task("T1") != nil && b.Task("T1").Status == "done" && b.Task("T2") != nil
	})
	select {
	case <-done:
		t.Fatal("the run must stay alive while runnable work remains")
	default:
	}
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop after the held import must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
	if markers := checkMarkers(t, e.Dir); markers != 2 {
		t.Fatalf("the check must run once before and once after the import, got %d", markers)
	}
	var final Backlog
	if err := readJSON(c.Todo, &final); err != nil {
		t.Fatal(err)
	}
	if final.Task("T1") == nil || final.Task("T1").Status != "done" {
		t.Fatalf("the verified task did not complete: %+v", final.Task("T1"))
	}
	if final.Task("T2") == nil || final.Task("T2").Status != "todo" {
		t.Fatalf("the imported edit must survive the status export: %+v", final.Task("T2"))
	}
	if e.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the exported file must match the accepted export hash")
	}
}

// TestBacklogEditChangingRetainedContractHoldsForCorrection reproduces the
// live incident behind T119: an external edit that changes the contract of the
// task whose work the hold retains must refuse at the drained boundary instead
// of importing and stranding that work at the next contract check. The hold
// stays open, the diagnostic names the retained task once, and a corrected
// edit — the same change outside the retained contract — imports and the run
// continues.
func TestBacklogEditChangingRetainedContractHoldsForCorrection(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	breaking := editedBacklog()
	breaking.Task("T1").Details = "changed acceptance criteria"
	if err := writeJSON(c.Todo, breaking); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hold to be reported", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "Dispatch is paused") >= 1
	})
	waitFor(t, "the refusal to be reported", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "changes the contract of retained task(s) T1") >= 1
	})
	select {
	case out := <-done:
		t.Fatalf("the refused import must keep the run alive: %d %v", out.code, out.err)
	default:
	}
	if count := countKindEvent(t, e.Dir, "refresh", "Imported changed project"); count != 0 {
		t.Fatalf("a contract-breaking edit must not import, got %d import events", count)
	}
	if e.State.ExportHash == digestFile(c.Todo) {
		t.Fatal("the refused import must not accept the edited file")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("the interrupted work must stay retained with its commit: %+v", e.State.Attempts)
	}
	// The correction keeps every added task and restores the retained
	// contract, so the same boundary imports it.
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the corrected edit to import", func() bool {
		return countKindEvent(t, e.Dir, "refresh", "Imported changed project") >= 1
	})
	if count := countKindEvent(t, e.Dir, "waiting", "changes the contract of retained task(s) T1"); count != 1 {
		t.Fatalf("the refusal diagnostic must fire once per problem, got %d", count)
	}
	waitFor(t, "T1 to finish", func() bool {
		var b Backlog
		return readJSON(c.Todo, &b) == nil && b.Task("T1") != nil && b.Task("T1").Status == "done" && b.Task("T2") != nil
	})
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop after the corrected import must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
}

// TestBacklogEditChangingRetainedContractEndsFiniteRun pins the finite-run
// side of the refusal: a --once run has no continuous boundary to hold open,
// so the contract-breaking edit ends the run with the refusal, the work
// retained, and the edit untouched — never an import that strands it.
func TestBacklogEditChangingRetainedContractEndsFiniteRun(t *testing.T) {
	e, c, done, release := holdRunEngineConfig(t, holdVerify, func(cfg *Config) { cfg.Once = true })
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	breaking := editedBacklog()
	breaking.Task("T1").Details = "changed acceptance criteria"
	if err := writeJSON(c.Todo, breaking); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 1 || out.err == nil || !strings.Contains(out.err.Error(), "changes the contract of retained task(s) T1") {
			t.Fatalf("a finite run must end with the refusal: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("the finite run stayed alive on the refused import")
	}
	if count := countKindEvent(t, e.Dir, "refresh", "Imported changed project"); count != 0 {
		t.Fatalf("a contract-breaking edit must not import, got %d import events", count)
	}
	if e.State.ExportHash == digestFile(c.Todo) {
		t.Fatal("the refused import must not accept the edited file")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("the interrupted work must stay retained with its commit: %+v", e.State.Attempts)
	}
	var b Backlog
	if err := readJSON(c.Todo, &b); err != nil || b.Task("T1").Details != "changed acceptance criteria" {
		t.Fatal("the refused finite run must leave the edit untouched")
	}
}

// TestBacklogEditDuringWorkerBuffersResult edits the task file while only a
// worker is in flight. The finished report must wait for the boundary import
// and replay against the imported baseline exactly once.
func TestBacklogEditDuringWorkerBuffersResult(t *testing.T) {
	t.Setenv("LOOPER_FAKE_SLEEP", "2")
	workerLog := filepath.Join(t.TempDir(), "workers")
	t.Setenv("LOOPER_FAKE_LOG", workerLog)
	e, c, done, _ := holdRunEngine(t, "true")
	waitFor(t, "the worker to start", func() bool {
		return countKindEvent(t, e.Dir, "start", "T1  ") >= 1
	})
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the hold to be reported", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "Dispatch is paused") >= 1
	})
	waitFor(t, "the edit to import", func() bool {
		return countKindEvent(t, e.Dir, "refresh", "Imported changed project") >= 1
	})
	waitFor(t, "T1 to finish", func() bool {
		var b Backlog
		return readJSON(c.Todo, &b) == nil && b.Task("T1") != nil && b.Task("T1").Status == "done" && b.Task("T2") != nil
	})
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop after the held import must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
	workers, err := os.ReadFile(workerLog)
	if err != nil || strings.Count(string(workers), "worker") != 1 {
		t.Fatalf("the buffered worker must run exactly once: %q %v", workers, err)
	}
}

// enterHoldCorrectionWait drives a run into the backlog-hold correction
// wait: an invalid task-file edit lands while a check is in flight, the check
// finishes against the released gate, and the controller asks for a
// correction while the verified result stays retained.
func enterHoldCorrectionWait(t *testing.T, e *Engine, c Config, release string) {
	t.Helper()
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the correction request", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "needs correction") >= 1
	})
}

// TestBacklogHoldWaitsForCorrectedEdit drives an invalid edit through the
// hold: the run must stay alive waiting for a correction, then import the
// corrected edit and resume, mirroring the idle input wait.
func TestBacklogHoldWaitsForCorrectedEdit(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	enterHoldCorrectionWait(t, e, c, release)
	select {
	case out := <-done:
		t.Fatalf("the run died on an invalid edit during the hold: %d %v", out.code, out.err)
	default:
	}
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the corrected edit to import", func() bool {
		return countKindEvent(t, e.Dir, "refresh", "Imported changed project") >= 1
	})
	waitFor(t, "T1 to finish", func() bool {
		var b Backlog
		return readJSON(c.Todo, &b) == nil && b.Task("T1") != nil && b.Task("T1").Status == "done" && b.Task("T2") != nil
	})
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop after the corrected import must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
}

// TestBacklogHoldStopDuringActiveWorkDrainsAtBoundary reproduces the live
// incident: a stop requested while a gated check still runs must complete the
// drain at the first boundary after that work finishes, even though the held
// edit is invalid and would otherwise hold the loop for a correction.
func TestBacklogHoldStopDuringActiveWorkDrainsAtBoundary(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the first check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
		t.Fatal(err)
	}
	// The stop request lands while the check is still in flight, matching the
	// reported reproduction where looper stop answered "Active work will
	// drain."
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop during a held invalid edit must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run stayed alive in the correction wait instead of draining")
	}
	assertHeldDrainRetainedWork(t, e, c)
}

// TestBacklogHoldStopDuringCorrectionWaitDrains stops the controller after the
// correction wait is already published, mirroring the idle input wait where a
// stop request ends the wait instead of waiting for the next refresh.
func TestBacklogHoldStopDuringCorrectionWaitDrains(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	enterHoldCorrectionWait(t, e, c, release)
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop during the held correction wait must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run stayed alive in the correction wait instead of draining")
	}
	assertHeldDrainRetainedWork(t, e, c)
}

// TestBacklogHoldCorrectionWaitAnswersQueuedRequests pins the request
// contract while the hold waits for a corrected edit: every request kind is
// claimed and answered exactly as at the loop boundary, none of them touches
// the invalid task file, and a stop request still drains the run.
func TestBacklogHoldCorrectionWaitAnswersQueuedRequests(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	enterHoldCorrectionWait(t, e, c, release)
	info, err := readController(e.Dir)
	if err != nil {
		t.Fatal(err)
	}

	// A settings request applies without touching the task file.
	SaveProfile(c.ProfilePath, &Profile{Version: 1})
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "hold-settings.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "hold-settings", Session: info.Session,
		ProfileDigest: profileDigest(c.ProfilePath), Fields: map[string]json.RawMessage{"jobs": json.RawMessage("3")},
	}); err != nil {
		t.Fatal(err)
	}
	// Add-task and requeue conflict instead of exporting over the edit.
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "hold-add.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "hold-add", Type: RequestTypeAddTask, Session: info.Session,
		Tasks: []Task{{ID: "T9", Title: "nine", Priority: 1, Status: "todo"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "hold-requeue.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "hold-requeue", Type: RequestTypeRequeue, Session: info.Session,
		TaskID: "T1", TaskRevision: hashJSON(Task{ID: "T1", Title: "one", Priority: 1, Status: "todo"}),
	}); err != nil {
		t.Fatal(err)
	}
	// A plan import is claimed and queued, not applied against the edit.
	plan := planFile(t, "# Held requirements\nAdd one telemetry counter.\n")
	importID, err := submitPlanImport(e.Dir, info.Session, "", []string{plan})
	if err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the settings request to apply", func() bool {
		ack, err := LoadAck(e.Dir, "hold-settings")
		return err == nil && ack.Status == AckApplied
	})
	waitFor(t, "add-task to conflict", func() bool {
		ack, err := LoadAck(e.Dir, "hold-add")
		return err == nil && ack.Status == AckConflict
	})
	waitFor(t, "requeue to conflict", func() bool {
		ack, err := LoadAck(e.Dir, "hold-requeue")
		return err == nil && ack.Status == AckConflict
	})
	waitFor(t, "the plan import to queue", func() bool {
		var ack planImportAck
		return readJSON(filepath.Join(ackDir(e.Dir), importID+".json"), &ack) == nil && ack.Status == ImportQueued
	})

	// Every envelope left the queue, the diagnostic stays at one event, and
	// the invalid edit is preserved byte for byte.
	if left, _ := filepath.Glob(filepath.Join(requestDir(e.Dir), "*.json")); len(left) != 0 {
		t.Fatalf("queued requests were never claimed: %v", left)
	}
	if count := countKindEvent(t, e.Dir, "waiting", "needs correction"); count != 1 {
		t.Fatalf("the diagnostic must fire once per problem, got %d", count)
	}
	if raw, err := os.ReadFile(c.Todo); err != nil || string(raw) != malformedBacklog {
		t.Fatalf("requests must not touch the invalid file: %q %v", raw, err)
	}
	select {
	case out := <-done:
		t.Fatalf("the correction wait must keep the run alive: %d %v", out.code, out.err)
	default:
	}

	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop with queued requests answered must drain cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
	assertHeldDrainRetainedWork(t, e, c)
	if e.Config.Jobs != 3 {
		t.Fatalf("the applied settings request must change the live configuration: %d", e.Config.Jobs)
	}
	for _, r := range e.State.Imports {
		if r.RequestID == importID && r.Status != ImportQueued {
			t.Fatalf("the held import must stay queued through the drain: %s", r.Status)
		}
	}
}

// TestBacklogHoldCorrectionWaitSleepsDespiteUnclaimedRequest pins the sleep
// itself: an envelope no processor claims (an unknown type) sits in the queue
// for the whole wait, and the correction wait still sleeps between passes
// instead of spinning on the failed parse and the view republication. Every
// pass republishes the view, so a reader that keeps the notify channel empty
// receives one snapshot per pass: the sleeping wait runs one pass per poll
// interval, a sleepless loop republishes as fast as it can.
func TestBacklogHoldCorrectionWaitSleepsDespiteUnclaimedRequest(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	enterHoldCorrectionWait(t, e, c, release)
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "hold-watchdog.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "hold-watchdog", Type: "watchdog", Session: "nobody",
	}); err != nil {
		t.Fatal(err)
	}

	window := 800 * time.Millisecond
	publications := make(chan int, 1)
	go func() {
		n := 0
		deadline := time.After(window)
		for {
			select {
			case <-e.viewNotify:
				n++
			case <-deadline:
				publications <- n
				return
			}
		}
	}()
	count := <-publications
	expected := int(window / c.Poll)
	if count > 2*expected+10 {
		t.Fatalf("the correction wait spun without sleep: %d view publications in %v, about %d expected", count, window, expected)
	}

	// Passes still run at poll cadence, so a real request is answered.
	info, err := readController(e.Dir)
	if err != nil {
		t.Fatal(err)
	}
	SaveProfile(c.ProfilePath, &Profile{Version: 1})
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "hold-live.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "hold-live", Session: info.Session,
		ProfileDigest: profileDigest(c.ProfilePath), Fields: map[string]json.RawMessage{"jobs": json.RawMessage("2")},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the settings request to apply during the wait", func() bool {
		ack, err := LoadAck(e.Dir, "hold-live")
		return err == nil && ack.Status == AckApplied
	})
	if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.code != 0 || out.err != nil {
			t.Fatalf("stop must still drain the sleeping wait: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(20 * time.Second)):
		t.Fatal("run did not drain after stop")
	}
}

// assertHeldDrainRetainedWork pins the parts the drain must not change: the
// stop request is consumed, the stop event names the retained work, the
// interrupted check stays queued with its result commit, and the invalid edit
// stays on disk byte for byte.
func assertHeldDrainRetainedWork(t *testing.T, e *Engine, c Config) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.Dir, "stop")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the drain must consume the stop file")
	}
	if countKindEvent(t, e.Dir, "stop", "draining active work") != 1 {
		t.Fatal("the drain must record one stop event for the held boundary")
	}
	if countKindEvent(t, e.Dir, "stop", "retained in") != 1 {
		t.Fatal("the stop event must name where the retained work lives")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("the interrupted check must stay retained as a ready result: %+v", e.State.Attempts)
	}
	if raw, err := os.ReadFile(c.Todo); err != nil || string(raw) != malformedBacklog {
		t.Fatalf("the invalid edit must be preserved byte for byte: %q %v", raw, err)
	}
}

// TestRefreshHeldRefusesRetainedContractChange pins the import gate at the
// refresh itself (T119): an edit that changes a retained task's contract
// refuses through the same predicate the checked application applies, keeps
// the baseline and the hold, and imports once the edit leaves the contract
// intact.
func TestRefreshHeldRefusesRetainedContractChange(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	tipBefore := e.State.Tip
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-hold", Task: *e.State.Backlog.Task("T1"), Phase: "ready", ResultCommit: tipBefore,
		Directory: filepath.Join(e.Dir, "task-hold"), Ref: "refs/looper-tasks/task-hold",
	})
	breaking := editedBacklog()
	breaking.Task("T1").Details = "changed acceptance criteria"
	if err := writeJSON(c.Todo, breaking); err != nil {
		t.Fatal(err)
	}
	err := e.refreshHeld()
	var refused *retainedContractRefusal
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "retained task(s) T1") {
		t.Fatalf("a contract-breaking edit must refuse the held import: %v", err)
	}
	if !e.idleInputWait(err) {
		t.Fatal("the refusal must be waitable input, or a live hold would end the run")
	}
	if e.State.Tip != tipBefore || e.State.ExportHash == digestFile(c.Todo) {
		t.Fatal("the refused import must not move the baseline or accept the file")
	}
	if err := e.refreshIdle(); err == nil || !strings.Contains(err.Error(), "work is retained") {
		t.Fatalf("the idle refresh keeps its own refusal while work is retained: %v", err)
	}
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshHeld(); err != nil {
		t.Fatalf("the corrected edit must import at the held boundary: %v", err)
	}
	if e.State.ExportHash != digestFile(c.Todo) || e.State.Tip == tipBefore {
		t.Fatal("the corrected edit must import and move the baseline")
	}
}

// TestPublishReportsBacklogChangeSentinel pins the publication guard's typed
// error, which the run loop uses to distinguish a held edit from fatal
// publication conflicts.
func TestPublishReportsBacklogChangeSentinel(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	task := *e.State.Backlog.Task("T1")
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-hold", Task: task, Phase: "verifying",
		Directory: filepath.Join(e.Dir, "task-hold"), Ref: "refs/looper-tasks/task-hold",
	})
	e.State.Publication = &Candidate{AttemptID: "task-hold", Base: e.State.Tip, Commit: e.State.Tip, Tree: e.State.Published}
	edited := Backlog{Version: 1, Sources: []string{}, Goal: "wait", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "done"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	err := e.publish()
	if !errors.Is(err, errBacklogChanged) {
		t.Fatalf("a changed task file must report the hold sentinel: %v", err)
	}
	if err == nil || err.Error() != "the backlog changed outside Looper before integration" {
		t.Fatalf("the sentinel keeps its message: %v", err)
	}
}

// TestRefreshHeldCarriesRetainedVerifiedWork pins the split between the two
// refresh variants: the idle refresh keeps refusing while work is retained,
// while the held boundary imports the edit, moves the baseline, and keeps the
// live plan for the queued work.
func TestRefreshHeldCarriesRetainedVerifiedWork(t *testing.T) {
	e, c := idleEngine(t, reviewComplete)
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"src/"}}}}
	e.State.PlanSignature = "live"
	tipBefore := e.State.Tip
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-hold", Task: *e.State.Backlog.Task("T1"), Phase: "ready", ResultCommit: tipBefore,
		Directory: filepath.Join(e.Dir, "task-hold"), Ref: "refs/looper-tasks/task-hold",
	})
	if err := writeJSON(c.Todo, editedBacklog()); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshIdle(); err == nil || !strings.Contains(err.Error(), "work is retained") {
		t.Fatalf("the idle refresh must keep refusing retained work: %v", err)
	}
	if e.State.Tip != tipBefore {
		t.Fatal("the refused refresh must not move the baseline")
	}
	// A changed project baseline still conflicts with the retained results.
	stray := filepath.Join(c.Root, "stray.txt")
	if err := os.WriteFile(stray, []byte("local edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshHeld(); err == nil || !strings.Contains(err.Error(), "work is retained") {
		t.Fatalf("changed project files must keep the refusal: %v", err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshHeld(); err != nil {
		t.Fatal(err)
	}
	if e.State.Tip == tipBefore {
		t.Fatal("the held import must move the baseline")
	}
	if e.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the held import must accept the edited file")
	}
	if e.State.Backlog.Task("T2") == nil {
		t.Fatal("the held import must accept the edited tasks")
	}
	if e.State.Plan == nil || e.State.PlanSignature != "" {
		t.Fatal("the held import must keep the live plan and clear only its signature")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" {
		t.Fatalf("the retained attempt must stay queued: %+v", e.State.Attempts)
	}
}
