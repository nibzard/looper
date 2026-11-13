package looper

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lastShutdown(t *testing.T, s *Store) sessionExit {
	t.Helper()
	var raw string
	if err := s.db.QueryRow("SELECT data FROM events WHERE kind='shutdown' ORDER BY id DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var exit sessionExit
	if err := json.Unmarshal([]byte(raw), &exit); err != nil {
		t.Fatal(err)
	}
	return exit
}

func TestStartupDrainAndResume(t *testing.T) {
	for _, beforeEngine := range []bool{true, false} {
		t.Run(fmt.Sprintf("before_engine=%t", beforeEngine), func(t *testing.T) {
			e, c, closeEngine := assessRunEngine(t, "", 0)
			closeEngine()
			stopPath := filepath.Join(e.Dir, "stop")
			requestStop := func() {
				t.Helper()
				if err := atomicWrite(stopPath, []byte("drain\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if beforeEngine {
				requestStop()
			}
			startEngine := func() (*Engine, func()) {
				t.Helper()
				e, lock, err := NewEngine(c, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				closed := false
				close := func() {
					if !closed {
						closed = true
						e.Store.Close()
						lock.Close()
					}
				}
				t.Cleanup(close)
				return e, close
			}
			e, closeEngine = startEngine()
			if !beforeEngine {
				requestStop()
			}
			// The budget spans two full engine runs, drain and resume; both
			// are fsync-bound, so it must scale with host pressure.
			ctx, cancel := context.WithTimeout(context.Background(), testBudget(20*time.Second))
			defer cancel()
			code, err := e.Run(ctx)
			if code != 0 || err != nil {
				t.Fatalf("startup drain: %d %v", code, err)
			}
			if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
				t.Fatalf("startup request was lost: %+v", exit)
			}
			if len(e.State.Attempts) != 0 || e.State.Plan != nil || e.State.Backlog.Tasks[0].Status != "todo" {
				t.Fatal("startup drain must stop before planning or dispatch")
			}
			if _, err := os.Stat(stopPath); !os.IsNotExist(err) {
				t.Fatalf("completed drain left a stop request: %v", err)
			}
			closeEngine()
			e, _ = startEngine()
			code, err = e.Run(ctx)
			if code != 0 || err != nil {
				t.Fatalf("resume after drain: %d %v", code, err)
			}
			if exit := lastShutdown(t, e.Store); exit.Reason != "complete" {
				t.Fatalf("resume did not complete: %+v", exit)
			}
			if e.State.Backlog.Tasks[0].Status != "done" {
				t.Fatal("resume must execute the pending task")
			}
		})
	}
}

// runFiniteEngine starts a controller on a prepared finite project and
// returns its exit record and run result.
func runFiniteEngine(t *testing.T, c Config) (sessionExit, int, error) {
	t.Helper()
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	close := func() {
		if !closed {
			closed = true
			e.Store.Close()
			lock.Close()
		}
	}
	t.Cleanup(close)
	code, err := e.Run(context.Background())
	return lastShutdown(t, e.Store), code, err
}

// TestStopWithoutOwnerCannotAffectFutureRun covers the defect source: `looper
// stop` with no controller owning the repository must neither write nor
// delete a request, and the next start must run normally instead of draining
// on a stale request.
func TestStopWithoutOwnerCannotAffectFutureRun(t *testing.T) {
	t.Run("writes nothing", func(t *testing.T) {
		c := preparedRepo(t, "--once")
		_, dir := projectPaths(c)
		var out, stderr bytes.Buffer
		if code := runStop(c, dir, &out, &stderr); code != 1 {
			t.Fatalf("stop without a controller must not claim success: %d %s %s", code, out.String(), stderr.String())
		}
		if !strings.Contains(out.String(), "No controller is running") {
			t.Fatalf("stop must report the missing owner: %q", out.String())
		}
		if _, err := os.Stat(stopRequestPath(dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("stop without an owner must not write a request")
		}
		exit, code, err := runFiniteEngine(t, c)
		if code != 0 || err != nil {
			t.Fatalf("a later start must run normally: %d %v", code, err)
		}
		if exit.Reason != "complete" {
			t.Fatalf("a stop without an owner drained the next run: %+v", exit)
		}
	})
	t.Run("keeps an existing request", func(t *testing.T) {
		c := preparedRepo(t, "--once")
		_, dir := projectPaths(c)
		// A pending request may belong to a controller that is still starting
		// and has not acquired the lock yet, so stop must not delete it.
		if err := writeStopRequest(dir, stopRequest{Session: "run-starting"}); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		if code := runStop(c, dir, &out, &stderr); code != 1 {
			t.Fatalf("stop without a controller must not claim success: %d %s", code, out.String())
		}
		if _, err := os.Stat(stopRequestPath(dir)); err != nil {
			t.Fatal("stop without an owner must leave an existing request untouched")
		}
		// The next start discards the unanswerable request and completes.
		exit, code, err := runFiniteEngine(t, c)
		if code != 0 || err != nil {
			t.Fatalf("a later start must run normally: %d %v", code, err)
		}
		if exit.Reason != "complete" {
			t.Fatalf("the stale request drained the next run: %+v", exit)
		}
	})
}

// TestStopBindsRequestToLiveController pins the ownership binding: while a
// controller holds the repository lock, stop writes a request scoped to that
// controller's session, the controller drains on it, and the completed drain
// consumes it.
func TestStopBindsRequestToLiveController(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	var out, stderr bytes.Buffer
	code := runStop(c, e.Dir, &out, &stderr)
	if code != 0 {
		t.Fatalf("stop with a live controller must succeed: %d %s %s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), e.controller.Session) {
		t.Fatalf("stop must name the owning session: %q", out.String())
	}
	req, _, err := readStopRequest(e.Dir)
	if err != nil || req == nil || req.Session != e.controller.Session {
		t.Fatalf("the request must bind to the owning session: %+v %v", req, err)
	}
	code, err = e.Run(context.Background())
	if code != 0 || err != nil {
		t.Fatalf("scoped drain: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the scoped request was ignored: %+v", exit)
	}
	if _, err = os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(err) {
		t.Fatal("the completed drain left its request behind")
	}
}

// TestStartupSweepDiscardsStaleStopRequests covers shutdown overlap and
// legacy files: a request that cannot bind to the starting controller is
// discarded at startup, reported, and never drains the run.
func TestStartupSweepDiscardsStaleStopRequests(t *testing.T) {
	backdate := func(t *testing.T, dir string) {
		t.Helper()
		if err := atomicWrite(stopRequestPath(dir), []byte("drain\n"), 0600); err != nil {
			t.Fatal(err)
		}
		past := processStartedAt.Add(-time.Minute)
		if err := os.Chtimes(stopRequestPath(dir), past, past); err != nil {
			t.Fatal(err)
		}
	}
	wrongIdentity := func(t *testing.T, dir string) {
		t.Helper()
		if id, err := runIdentityCommand(os.Getpid()); err != nil {
			t.Skip("this platform provides no process identity")
		} else if id == "wrong-identity" {
			t.Skip("identity collision with the test process")
		}
		if err := writeStopRequest(dir, stopRequest{PID: os.Getpid(), Identity: "wrong-identity"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		write  func(t *testing.T, dir string)
		report string
	}{
		{"foreign session", func(t *testing.T, dir string) {
			if err := writeStopRequest(dir, stopRequest{Session: "run-earlier", PID: 4194304}); err != nil {
				t.Fatal(err)
			}
		}, "run-earlier"},
		{"foreign process", func(t *testing.T, dir string) {
			if err := writeStopRequest(dir, stopRequest{PID: 4194304}); err != nil {
				t.Fatal(err)
			}
		}, "4194304"},
		{"wrong process identity", wrongIdentity, "cannot affect this run"},
		{"legacy request predating the controller", backdate, "predates this controller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := preparedRepo(t, "--once")
			_, dir := projectPaths(c)
			tc.write(t, dir)
			exit, code, err := runFiniteEngine(t, c)
			if code != 0 || err != nil {
				t.Fatalf("the stale request must not fail the run: %d %v", code, err)
			}
			if exit.Reason != "complete" {
				t.Fatalf("the stale request drained the run: %+v", exit)
			}
			if _, err = os.Stat(stopRequestPath(dir)); !os.IsNotExist(err) {
				t.Fatal("the stale request must be discarded")
			}
			s, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			events, err := s.RecentEvents(-1, 500)
			if err != nil {
				t.Fatal(err)
			}
			reported := false
			for _, ev := range events {
				if ev.Kind == "recovery" && strings.Contains(ev.Message, "Discarded a stop request") && strings.Contains(ev.Message, tc.report) {
					reported = true
				}
			}
			if !reported {
				t.Fatalf("the discarded request was not reported (want %q)", tc.report)
			}
		})
	}
}

// TestStartupDrainScopedRequests extends the T028 coverage to scoped
// requests: a request bound to the starting controller's session or process
// drains it before planning or dispatch, and a resume executes the work.
func TestStartupDrainScopedRequests(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request func(session string) stopRequest
	}{
		{"session", func(session string) stopRequest { return stopRequest{Session: session} }},
		{"process", func(string) stopRequest { return stopRequest{PID: os.Getpid()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, c, closeEngine := assessRunEngine(t, "", 0)
			closeEngine()
			startEngine := func() (*Engine, func()) {
				t.Helper()
				e, lock, err := NewEngine(c, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				closed := false
				close := func() {
					if !closed {
						closed = true
						e.Store.Close()
						lock.Close()
					}
				}
				t.Cleanup(close)
				return e, close
			}
			e, closeEngine = startEngine()
			if err := writeStopRequest(e.Dir, tc.request(e.controller.Session)); err != nil {
				t.Fatal(err)
			}
			// The budget spans the scoped drain and the resume that
			// executes the task; both are fsync-bound, so it must scale
			// with host pressure.
			ctx, cancel := context.WithTimeout(context.Background(), testBudget(20*time.Second))
			defer cancel()
			code, err := e.Run(ctx)
			if code != 0 || err != nil {
				t.Fatalf("scoped startup drain: %d %v", code, err)
			}
			if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
				t.Fatalf("the scoped startup request was lost: %+v", exit)
			}
			if len(e.State.Attempts) != 0 || e.State.Plan != nil || e.State.Backlog.Tasks[0].Status != "todo" {
				t.Fatal("startup drain must stop before planning or dispatch")
			}
			if _, err := os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(err) {
				t.Fatal("the completed drain left its request behind")
			}
			closeEngine()
			e, _ = startEngine()
			code, err = e.Run(ctx)
			if code != 0 || err != nil {
				t.Fatalf("resume after scoped drain: %d %v", code, err)
			}
			if exit := lastShutdown(t, e.Store); exit.Reason != "complete" {
				t.Fatalf("resume did not complete: %+v", exit)
			}
			if e.State.Backlog.Tasks[0].Status != "done" {
				t.Fatal("resume must execute the pending task")
			}
		})
	}
}

// TestShutdownReleasesPendingStopRequests pins the shutdown side of the
// synchronization: a request that arrives after the final boundary can no
// longer be honored, so the exiting controller removes requests bound to
// itself. Requests for other controllers stay for the next startup sweep.
func TestShutdownReleasesPendingStopRequests(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = writeStopRequest(e.Dir, stopRequest{Session: e.controller.Session}); err != nil {
		t.Fatal(err)
	}
	e.releaseStopRequests()
	if _, err = os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(err) {
		t.Fatal("shutdown must release a request bound to this controller")
	}
	if err = writeStopRequest(e.Dir, stopRequest{Session: "run-earlier"}); err != nil {
		t.Fatal(err)
	}
	e.releaseStopRequests()
	if _, err = os.Stat(stopRequestPath(e.Dir)); err != nil {
		t.Fatal("shutdown must not remove a request bound to another controller")
	}
	os.Remove(stopRequestPath(e.Dir))
}

// waitForEvent blocks until the engine records one event of the given kind.
func waitForEvent(t *testing.T, e *Engine, kind string) {
	t.Helper()
	deadline := time.Now().Add(testBudget(30 * time.Second))
	for {
		var n int
		if err := e.Store.db.QueryRow(`SELECT count(*) FROM events WHERE kind=?`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no %s event was recorded", kind)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastEventMessage returns the newest recorded message for one event kind.
func lastEventMessage(t *testing.T, e *Engine, kind string) string {
	t.Helper()
	var message string
	if err := e.Store.db.QueryRow(`SELECT message FROM events WHERE kind=? ORDER BY id DESC LIMIT 1`, kind).Scan(&message); err != nil {
		t.Fatal(err)
	}
	return message
}

// TestStartupDrainWithRetainedNilPlanWork proves a drain request that meets
// retained ready work without a plan is honored: the unverified results are
// saved, the request is consumed only after that, and the exit explains the
// retained work. No planning, check, or worker runs for the drain.
func TestStartupDrainWithRetainedNilPlanWork(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verify-ran")
	c := retainedPlanRun(t, "printf planned-verify > "+marker)
	log := agentLogPath(t)
	e, shutdown := retainedEngine(t, c)
	stopPath := filepath.Join(e.Dir, "stop")
	if err := atomicWrite(stopPath, []byte("drain\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The budget spans the retained-work drain and the resume that verifies
	// and completes the project; both are fsync-bound, so it must scale
	// with host pressure.
	ctx, cancel := context.WithTimeout(context.Background(), testBudget(20*time.Second))
	defer cancel()
	code, err := e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("retained-work drain: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the drain request was ignored: %+v", exit)
	}
	if _, statErr := os.Stat(stopPath); !os.IsNotExist(statErr) {
		t.Fatal("the completed drain left its stop request behind")
	}
	if message := lastEventMessage(t, e, "stop"); !strings.Contains(message, "1 unverified result") || !strings.Contains(message, e.Dir) {
		t.Fatalf("the drain exit does not explain the retained work: %q", message)
	}
	if e.State.Plan != nil {
		t.Fatal("the drain started a planning pass")
	}
	if len(e.State.Attempts) != 1 || e.State.Attempts[0].Phase != "ready" || e.State.Attempts[0].ResultCommit == "" {
		t.Fatalf("the drain consumed the retained result: %+v", e.State.Attempts)
	}
	if task := e.State.Backlog.Task("T001"); task.Status != "doing" {
		t.Fatalf("the retained task changed status: %+v", task)
	}
	if n := workerRuns(t, log); n != 0 {
		t.Fatalf("the drain dispatched %d workers", n)
	}
	if _, err = os.Stat(marker); err == nil {
		t.Fatal("the drain verified retained work")
	}

	// The next controller plans and verifies the retained result without
	// repeating implementation.
	shutdown()
	log = agentLogPath(t)
	e, shutdown = retainedResumeEngine(t, c)
	code, err = e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("resume after retained-work drain: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "complete" {
		t.Fatalf("the resumed run did not complete: %+v", exit)
	}
	for _, id := range []string{"T001", "T002"} {
		if task := e.State.Backlog.Task(id); task == nil || task.Status != "done" {
			t.Fatalf("%s did not finish after resume: %+v", id, task)
		}
	}
	if n := workerRuns(t, log); n != 1 {
		t.Fatalf("resume repeated implementation: %d worker runs", n)
	}
}

// TestDrainDuringNilPlanRecovery requests the drain while the recovery
// planning pass runs. The request is honored at the next boundary: the
// retained result still verifies against the rebuilt plan, no new task
// dispatches, and the run drains instead of wedging.
func TestDrainDuringNilPlanRecovery(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "verify-ran")
	c := retainedPlanRun(t, "printf planned-verify > "+marker)
	// Hold the planning agent so the request lands during recovery.
	t.Setenv("LOOPER_FAKE_SLEEP", "3")
	e, _ := retainedEngine(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		code, err := e.Run(ctx)
		if err != nil {
			t.Errorf("run: %v", err)
		}
		done <- code
	}()
	waitForEvent(t, e, "plan")
	if err := atomicWrite(filepath.Join(e.Dir, "stop"), []byte("drain\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("drain during recovery exited %d", code)
		}
	case <-time.After(testBudget(30 * time.Second)):
		t.Fatal("the drain request was never honored")
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("drain during recovery was ignored: %+v", exit)
	}
	if task := e.State.Backlog.Task("T001"); task == nil || task.Status != "done" {
		t.Fatalf("the retained result was not verified before the drain: %+v", task)
	}
	if task := e.State.Backlog.Task("T002"); task == nil || task.Status != "todo" {
		t.Fatalf("the drain dispatched queued work: %+v", task)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the planner-selected verification never ran: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "stop")); !os.IsNotExist(err) {
		t.Fatal("the completed drain left its stop request behind")
	}
}

func TestDrainReportsRequestCleanupFailure(t *testing.T) {
	c := preparedRepo(t, "--once")
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	stopPath := filepath.Join(e.Dir, "stop")
	if err := os.Mkdir(stopPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stopPath, "child"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	code, err := e.Run(context.Background())
	if code != 1 || err == nil || !strings.Contains(err.Error(), "remove completed drain request") {
		t.Fatalf("cleanup failure must surface: %d %v", code, err)
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "error" {
		t.Fatalf("failed cleanup recorded as a completed drain: %+v", exit)
	}
}

func TestControllerRecordsCompletedAndFailedExits(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			e, c, _ := assessRunEngine(t, "", 0)
			if fail {
				if err := os.WriteFile(c.Todo, []byte(malformedBacklog), 0600); err != nil {
					t.Fatal(err)
				}
			}
			code, err := e.Run(context.Background())
			exit := lastShutdown(t, e.Store)
			reason, outcome := "complete", "stopped"
			if fail {
				reason, outcome = "error", "failed"
				if code != 1 || err == nil {
					t.Fatalf("bad input exit: %d %v", code, err)
				}
			} else if code != 0 || err != nil {
				t.Fatalf("completed exit: %d %v", code, err)
			}
			if exit.Session != e.controller.Session || exit.Reason != reason || exit.ExitCode != code || (fail && exit.Error == "") {
				t.Fatalf("exit record: %+v", exit)
			}
			var got string
			var ended sql.NullString
			if err := e.Store.db.QueryRow("SELECT outcome,ended FROM sessions WHERE id=?", exit.Session).Scan(&got, &ended); err != nil || got != outcome || !ended.Valid {
				t.Fatalf("session exit: %s %v %v", got, ended, err)
			}
		})
	}
}

type panicOutput struct{}

func (panicOutput) Write([]byte) (int, error) { panic("test controller panic") }

func TestControllerRecordsPanicAndRethrows(t *testing.T) {
	e, _, _ := assessRunEngine(t, "", 0)
	e.Output, e.Render = panicOutput{}, nil
	var crash any
	func() { defer func() { crash = recover() }(); _, _ = e.Run(context.Background()) }()
	if crash != "test controller panic" {
		t.Fatalf("panic was lost: %v", crash)
	}
	exit := lastShutdown(t, e.Store)
	if exit.Reason != "panic" || exit.ExitCode != 2 || !strings.Contains(exit.Error, "test controller panic") {
		t.Fatalf("panic record: %+v", exit)
	}
}

func TestExitRecordAndSessionEndAreAtomic(t *testing.T) {
	s := timingStore(t)
	info := &ControllerInfo{Session: "exit-test", PID: os.Getpid(), Started: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.beginSession(info); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER reject_shutdown BEFORE INSERT ON events WHEN NEW.kind='shutdown' BEGIN SELECT RAISE(ABORT,'test write failure'); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.endSession(sessionExit{Reason: "error", ExitCode: 1}); err == nil {
		t.Fatal("exit event write succeeded unexpectedly")
	}
	path := filepath.Join(s.dir, "state.db")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	read, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	var end sql.NullString
	if err := read.db.QueryRow("SELECT ended FROM sessions WHERE id=?", info.Session).Scan(&end); err != nil || end.Valid {
		t.Fatalf("failed recording invented a clean end: %v %v", end, err)
	}
}

func TestRestartIdentifiesPreviousExit(t *testing.T) {
	for _, clean := range []bool{false, true} {
		t.Run(fmt.Sprint(clean), func(t *testing.T) {
			s := timingStore(t)
			info := &ControllerInfo{Session: "old", PID: os.Getpid(), Started: timingAt(0)}
			if err := s.beginSession(info); err != nil {
				t.Fatal(err)
			}
			id, err := s.Event("start", "last activity", nil)
			if err != nil {
				t.Fatal(err)
			}
			if clean {
				if err := s.endSession(sessionExit{Reason: "cancelled", ExitCode: 130}); err != nil {
					t.Fatal(err)
				}
			}
			info.Session = "new"
			if err := s.beginSession(info); err != nil {
				t.Fatal(err)
			}
			e := &Engine{Store: s}
			message := e.resumeMessage()
			if !strings.Contains(message, "old") {
				t.Fatal(message)
			}
			if clean {
				if !strings.Contains(message, "cancelled") || !strings.Contains(message, "Exit record: event") {
					t.Fatal(message)
				}
			} else if !strings.Contains(message, "uncleanly") || s.previous.LastEventID != id {
				t.Fatal(message)
			}
		})
	}
}

func TestRejectedFindingsIdentifyTheirSubject(t *testing.T) {
	e := snapshotEngine(t)
	for _, finding := range []Finding{
		{Title: "Fix output", Details: "criteria", Evidence: "failure", DependsOn: []string{"missing"}},
		{Title: "Fix input", Details: "criteria", Evidence: "failure", DependsOn: []string{"missing"}},
		{Evidence: "first untitled report"},
		{Evidence: "second untitled report"},
	} {
		applyFinding(t, e, finding)
	}
	events, err := e.Store.RecentEvents(-1, 10)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	count := 0
	for _, event := range events {
		if event.Kind == "finding" {
			if seen[event.Message] {
				t.Fatal("different findings have identical messages")
			}
			seen[event.Message] = true
			count++
		}
	}
	if count != 4 {
		t.Fatalf("finding count %d", count)
	}
}
