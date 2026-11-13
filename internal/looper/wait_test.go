package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// The AX-013 acceptance fixtures. Every fixture drives the public command
// path: each predicate answers completion before invocation, a transition
// during the wait, a timeout, a controller exit or restart, and a client
// cancellation — without a busy loop, without a missed transition, and
// without cancelling anything it observed.

// waitContractSchema reads the published wait schema, anchored at this file's
// directory because fixtures chdir into temporary projects.
func waitContractSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "wait.schema.json"))
	if err != nil {
		t.Skipf("wait schema is absent: %v", err)
	}
	return b
}

// waitProject seeds one stopped run and returns the configuration the wait
// command reads. A stopped run has no controller metadata, exactly like a
// controller that shut down and removed its own record.
func waitProject(t *testing.T, mutate func(*State)) Config {
	t.Helper()
	base := runsBase(t)
	root := filepath.Join(filepath.Dir(base), "wait-"+strings.ToLower(strings.TrimPrefix(t.Name(), "Test")))
	c := runsProjectAt(t, root)
	seedSavedRun(t, c, mutate)
	return c
}

// runWaitCLI drives `looper wait` through Main and returns the exit code with
// the decoded document. The JSON form must validate against the published
// schema and keep standard error empty.
func runWaitCLI(t *testing.T, args ...string) (int, *waitDocument, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Main(append([]string{"wait"}, args...), &out, &stderr)
	jsonForm := false
	for _, a := range args {
		if a == "--json" {
			jsonForm = true
		}
	}
	if !jsonForm {
		return code, nil, out.String() + stderr.String()
	}
	if code != exitOK && code != exitWaitTimeout && code != exitCancelled {
		// Every wait answers with its document, including the timeout and
		// cancellation outcomes. Another exit is a defect, and reporting the
		// raw output here — instead of returning a nil document the caller
		// dereferences — is what makes the failure readable.
		t.Fatalf("the wait must answer with its document, exit %d:\n%s%s", code, out.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("a JSON wait must keep standard error empty, found:\n%s", stderr.String())
	}
	var doc waitDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("the wait document does not parse: %v\n%s", err, out.String())
	}
	var schema, instance any
	if err := json.Unmarshal(waitContractSchema(t), &schema); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(out.Bytes(), &instance)
	if err := contractschema.Validate(schema, instance); err != nil {
		t.Fatalf("the wait document must validate against the published schema: %v\n%s", err, out.String())
	}
	return code, &doc, out.String()
}

// waitTaskSeed adds one task with the given status to a seeded state.
func waitTaskSeed(id, status string) func(*State) {
	return func(s *State) {
		s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: id, Title: "seeded " + id, Priority: 2, Status: status})
	}
}

// waitMutateState opens the seeded store for writing, applies one mutation,
// and closes it again — the shape a controller's own commit takes.
// mutateSavedState is waitMutateState without the testing hooks, so a timer
// goroutine can run it: testing's Fatal and Cleanup belong to the test
// goroutine alone, and a mutation that lands after the wait answered must
// still report its error there.
func mutateSavedState(c Config, mutate func(*Store, *State)) error {
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	s, err := store.Load()
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("load seeded state: no state")
	}
	mutate(store, s)
	return store.Save(s)
}

func waitMutateState(t *testing.T, c Config, mutate func(*Store, *State)) {
	t.Helper()
	if err := mutateSavedState(c, mutate); err != nil {
		t.Fatal(err)
	}
}

// mutateDuringTheWait schedules one mutation past the wait's first observation
// and returns the channel that carries its outcome. The test goroutine drains
// the channel after its assertions: a mutation the wait observed is complete,
// so the receive returns at once, and a failed mutation reports on the test
// goroutine instead of a timer goroutine where Fatal is forbidden.
func mutateDuringTheWait(d time.Duration, c Config, mutate func(*Store, *State)) <-chan error {
	done := make(chan error, 1)
	time.AfterFunc(d, func() { done <- mutateSavedState(c, mutate) })
	return done
}

// drainMutation receives a scheduled mutation's outcome. A mutation the wait
// observed has committed, so only its close can remain; the budget covers the
// close under load.
func drainMutation(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the scheduled mutation did not finish")
	}
}

// TestWaitCursorChangeSatisfiedBeforeInvocation covers the completion that
// already happened: a cursor below the committed high-water mark answers at
// once, without sleeping, and the document carries the position it reached.
func TestWaitCursorChangeSatisfiedBeforeInvocation(t *testing.T) {
	c := waitProject(t, nil)
	waitMutateState(t, c, func(store *Store, s *State) {
		for i := 0; i < 3; i++ {
			if _, err := store.Event("progress", fmt.Sprintf("seed event %d", i), nil); err != nil {
				t.Fatal(err)
			}
		}
	})
	high := waitHighWater(t, c)
	start := time.Now()
	code, doc, _ := runWaitCLI(t, "--cursor", fmt.Sprint(high-1), "--timeout", "5s", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("an already-satisfied cursor wait must exit 0, found %d", code)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a satisfied wait must return at once, took %s", time.Since(start))
	}
	if doc.Outcome != waitSatisfied || doc.Condition.Kind != WaitCursorChange || doc.Condition.Cursor != high-1 {
		t.Fatalf("condition: %+v", doc.Condition)
	}
	if doc.Observed.HighWater != high || doc.Cursor.EventID != high {
		t.Fatalf("cursor: observed %d, cursor %d, want %d", doc.Observed.HighWater, doc.Cursor.EventID, high)
	}
	if doc.Wait.TimeoutSeconds != 5 {
		t.Fatalf("the bound must be reported: %+v", doc.Wait)
	}
	if !strings.Contains(doc.Detail.Command, fmt.Sprintf("--cursor %d", high)) {
		t.Fatalf("the continuation must name the reached position: %s", doc.Detail.Command)
	}
	if !strings.Contains(doc.Detail.Follow, "--follow") {
		t.Fatalf("the streaming alternative must stay named: %s", doc.Detail.Follow)
	}
	if doc.Store == nil || doc.Cursor.StoreID == "" {
		t.Fatalf("the cursor must carry the store binding: %+v", doc.Cursor)
	}
}

// waitHighWater reads the committed high-water mark of one seeded project.
func waitHighWater(t *testing.T, c Config) int64 {
	t.Helper()
	_, dir := projectPaths(c)
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	high, err := store.highWater()
	if err != nil {
		t.Fatal(err)
	}
	return high
}

// TestWaitCursorChangeDuringTheWait covers the transition that lands while the
// wait runs: another writer commits an event, the wait returns satisfied with
// the new position, and no transition is missed.
func TestWaitCursorChangeDuringTheWait(t *testing.T) {
	c := waitProject(t, nil)
	high := waitHighWater(t, c)
	var eventErr error
	mutation := mutateDuringTheWait(600*time.Millisecond, c, func(store *Store, s *State) {
		_, eventErr = store.Event("result", "T1 finished", map[string]string{"task_id": "T1"})
	})
	code, doc, _ := runWaitCLI(t, "--cursor", fmt.Sprint(high), "--timeout", "10s", "--contract", "v2", "--json")
	if code != exitOK || doc.Outcome != waitSatisfied {
		t.Fatalf("outcome %q (exit %d) for a transition during the wait", doc.Outcome, code)
	}
	if doc.Observed.HighWater <= high {
		t.Fatalf("the wait must report the new position %d, reported %d", high, doc.Observed.HighWater)
	}
	drainMutation(t, mutation)
	if eventErr != nil {
		// The channel receive above ordered this write before the read.
		t.Fatal(eventErr)
	}
}

// TestWaitCursorChangeAcrossAControllerStarting covers the attach discipline:
// the wait starts against a cleanly stopped run, which attaches immutable and
// creates nothing, and a controller then starts, commits, and stops. The wait
// must re-attach and report the new position — a controller that starts after
// the caller cannot strand a wait on the snapshot it began with.
func TestWaitCursorChangeAcrossAControllerStarting(t *testing.T) {
	c := waitProject(t, nil)
	_, dir := projectPaths(c)
	high := waitHighWater(t, c)
	controller := make(chan error, 1)
	time.AfterFunc(600*time.Millisecond, func() {
		// The controller's whole life — open, commit, close — can fall inside
		// one poll interval, so the wait may see only a changed database.
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			controller <- err
			return
		}
		if _, err = store.Event("result", "T1 finished under a new controller", map[string]string{"task_id": "T1"}); err == nil {
			err = store.Close()
		} else {
			store.Close()
		}
		controller <- err
	})
	code, doc, raw := runWaitCLI(t, "--cursor", fmt.Sprint(high), "--timeout", "10s", "--contract", "v2", "--json")
	if code != exitOK || doc.Outcome != waitSatisfied {
		t.Fatalf("outcome %q exit %d:\n%s", doc.Outcome, code, raw)
	}
	if doc.Observed.HighWater <= high {
		t.Fatalf("the wait must see the new controller's commit %d, reported %d", high, doc.Observed.HighWater)
	}
	select {
	case err := <-controller:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the short-lived controller never finished")
	}
	// The wait on a run that stayed stopped leaves nothing beside its
	// database — that case is pinned by the observation-command test. Here a
	// writer appeared, so the wait's through-WAL attach keeps the sidecars
	// that writer's own close could not fold away; the wait never removes a
	// run's files.
	if doc.Store == nil || doc.Store.ID == "" {
		t.Fatalf("the store binding must ride the document: %+v", doc.Store)
	}
}

// nothing new committed: exit 5, one outcome document, the journal untouched,
// and the note that names why nothing changed.
func TestWaitCursorChangeTimeoutIsAnOutcome(t *testing.T) {
	c := waitProject(t, nil)
	_, dir := projectPaths(c)
	high := waitHighWater(t, c)
	before := waitEventCount(t, dir)
	start := time.Now()
	code, doc, raw := runWaitCLI(t, "--cursor", fmt.Sprint(high), "--timeout", "1s", "--contract", "v2", "--json")
	if code != exitWaitTimeout {
		t.Fatalf("a timed-out wait must exit 5, found %d (%s)", code, raw)
	}
	if doc.Outcome != waitTimeout {
		t.Fatalf("outcome: %s", doc.Outcome)
	}
	if doc.Observed.HighWater != high || doc.Cursor.EventID != high {
		t.Fatalf("a timeout keeps the latest cursor: %+v", doc.Observed)
	}
	if !strings.Contains(doc.Observed.Note, "no event was committed past") {
		t.Fatalf("the note must name what held: %q", doc.Observed.Note)
	}
	if !strings.Contains(doc.Observed.Note, "No controller owns the repository") {
		t.Fatalf("a stopped run must say nothing can change the records: %q", doc.Observed.Note)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("the wait must run its bound, ended after %s", elapsed)
	}
	// The wait observes only: the journal is exactly what it was.
	if after := waitEventCount(t, dir); after != before {
		t.Fatalf("a wait must not write the journal: %d events before, %d after", before, after)
	}
}

// waitEventCount counts the journal of one runtime directory.
func waitEventCount(t *testing.T, dir string) int {
	t.Helper()
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err := store.db.QueryRow("SELECT count(*) FROM events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestWaitTaskState covers the task predicate: a target status that already
// holds, a status change during the wait, an absent task that appears, and a
// timeout that reports the status that persisted.
func TestWaitTaskState(t *testing.T) {
	t.Run("status already held", func(t *testing.T) {
		c := waitProject(t, waitTaskSeed("T1", "done"))
		code, doc, _ := runWaitCLI(t, "--task", "T1", "--status", "done", "--timeout", "5s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Condition.From != "done" || doc.Observed.Task == nil || doc.Observed.Task.Status != "done" {
			t.Fatalf("condition %+v observed %+v", doc.Condition, doc.Observed.Task)
		}
		_ = c
	})
	t.Run("status change during the wait", func(t *testing.T) {
		c := waitProject(t, waitTaskSeed("T2", "doing"))
		mutation := mutateDuringTheWait(500*time.Millisecond, c, func(store *Store, s *State) {
			s.Backlog.Task("T2").Status = "done"
		})
		code, doc, _ := runWaitCLI(t, "--task", "T2", "--status", "done", "--timeout", "10s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Condition.From != "doing" || doc.Observed.Task.Status != "done" {
			t.Fatalf("from %s to %s", doc.Condition.From, doc.Observed.Task.Status)
		}
		drainMutation(t, mutation)
	})
	t.Run("any change without a target status", func(t *testing.T) {
		c := waitProject(t, waitTaskSeed("T3", "todo"))
		mutation := mutateDuringTheWait(500*time.Millisecond, c, func(store *Store, s *State) {
			s.Backlog.Task("T3").Status = "doing"
		})
		code, doc, _ := runWaitCLI(t, "--task", "T3", "--timeout", "10s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if !doc.Observed.Task.Changed {
			t.Fatalf("the change must be named: %+v", doc.Observed.Task)
		}
		drainMutation(t, mutation)
	})
	t.Run("a task that appears satisfies a change wait", func(t *testing.T) {
		c := waitProject(t, nil)
		mutation := mutateDuringTheWait(500*time.Millisecond, c, func(store *Store, s *State) {
			s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T9", Title: "imported", Priority: 2, Status: "todo"})
		})
		code, doc, _ := runWaitCLI(t, "--task", "T9", "--timeout", "10s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Condition.From != "absent" || doc.Observed.Task == nil || !doc.Observed.Task.Present {
			t.Fatalf("condition %+v observed %+v", doc.Condition, doc.Observed.Task)
		}
		drainMutation(t, mutation)
	})
	t.Run("timeout reports the status that held", func(t *testing.T) {
		waitProject(t, waitTaskSeed("T4", "blocked"))
		code, doc, _ := runWaitCLI(t, "--task", "T4", "--status", "done", "--timeout", "1s", "--contract", "v2", "--json")
		if code != exitWaitTimeout || doc.Outcome != waitTimeout {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Task == nil || doc.Observed.Task.Status != "blocked" || doc.Observed.Task.Changed {
			t.Fatalf("observed %+v", doc.Observed.Task)
		}
		if !strings.Contains(doc.Observed.Note, "still blocked") {
			t.Fatalf("note %q", doc.Observed.Note)
		}
	})
	t.Run("an absent task times out naming its absence", func(t *testing.T) {
		waitProject(t, nil)
		code, doc, _ := runWaitCLI(t, "--task", "T404", "--timeout", "1s", "--contract", "v2", "--json")
		if code != exitWaitTimeout {
			t.Fatalf("exit %d", code)
		}
		if doc.Observed.Task == nil || doc.Observed.Task.Present {
			t.Fatalf("observed %+v", doc.Observed.Task)
		}
		if !strings.Contains(doc.Observed.Note, "not in the saved backlog") {
			t.Fatalf("note %q", doc.Observed.Note)
		}
	})
}

// TestWaitRequestOutcome covers the request predicate: a terminal receipt that
// already decided, a decision during the wait, and a still-pending request
// whose timeout names the state and the wait reason. A rejected outcome is a
// satisfied wait, because the caller asked for the outcome, not for success.
func TestWaitRequestOutcome(t *testing.T) {
	terminal := &RequestRecord{ID: "req-done", Type: RequestTypeSettings, State: RequestApplied}
	pending := &RequestRecord{ID: "req-wait", Type: RequestTypeSettings, State: RequestWaiting,
		WaitReason: "active_work", WaitSince: time.Now().UTC().Format(time.RFC3339Nano)}
	holding := &RequestRecord{ID: "req-hold", Type: RequestTypeSettings, State: RequestWaiting,
		WaitReason: "backlog_hold", WaitSince: time.Now().UTC().Format(time.RFC3339Nano)}
	c := waitProject(t, func(s *State) {
		s.Requests = []*RequestRecord{terminal, pending, holding}
	})
	t.Run("already decided", func(t *testing.T) {
		code, doc, _ := runWaitCLI(t, "--request", "req-done", "--timeout", "5s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Request == nil || doc.Observed.Request.State != RequestApplied || doc.Observed.Request.ID != "req-done" {
			t.Fatalf("the latest receipt must ride the outcome: %+v", doc.Observed.Request)
		}
	})
	t.Run("decision during the wait", func(t *testing.T) {
		mutation := mutateDuringTheWait(500*time.Millisecond, c, func(store *Store, s *State) {
			for _, rec := range s.Requests {
				if rec.ID == "req-wait" {
					rec.State = RequestRejected
					rec.Error = "the settings were refused"
				}
			}
		})
		code, doc, raw := runWaitCLI(t, "--request", "req-wait", "--timeout", "10s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d:\n%s", doc.Outcome, code, raw)
		}
		if doc.Observed.Request == nil || doc.Observed.Request.State != RequestRejected {
			t.Fatalf("a rejected outcome satisfies the wait: %+v", doc.Observed.Request)
		}
		drainMutation(t, mutation)
	})
	t.Run("timeout names the pending state", func(t *testing.T) {
		code, doc, _ := runWaitCLI(t, "--request", "req-hold", "--timeout", "1s", "--contract", "v2", "--json")
		if code != exitWaitTimeout || doc.Outcome != waitTimeout {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Request == nil || doc.Observed.Request.State != RequestWaiting {
			t.Fatalf("observed %+v", doc.Observed.Request)
		}
		if doc.Observed.Request.WaitingOn == nil || doc.Observed.Request.WaitingOn.Reason != "backlog_hold" {
			t.Fatalf("the wait reason must ride the timeout: %+v", doc.Observed.Request.WaitingOn)
		}
		if !strings.Contains(doc.Observed.Note, "still waiting") {
			t.Fatalf("note %q", doc.Observed.Note)
		}
	})
	t.Run("an unknown request times out naming it", func(t *testing.T) {
		code, doc, _ := runWaitCLI(t, "--request", "req-nobody", "--timeout", "1s", "--contract", "v2", "--json")
		if code != exitWaitTimeout || doc.Observed.Request != nil {
			t.Fatalf("exit %d observed %+v", code, doc.Observed.Request)
		}
		if !strings.Contains(doc.Observed.Note, "no retained receipt or pending envelope names request req-nobody") {
			t.Fatalf("note %q", doc.Observed.Note)
		}
	})
}

// waitSeededController records one controller session and its live metadata,
// so a run wait binds to it the way it binds to a live owner. The returned
// writer is the session's own: only it can end the session, and the row stays
// running while the test holds it, exactly like a controller that has not shut
// down.
// seedControllerSession is waitSeededController without the testing hooks, so
// a timer goroutine can start a replacement controller: Fatal and Cleanup
// belong to the test goroutine alone, and the caller there closes the writer.
func seedControllerSession(c Config, session string, pid int) (*Store, *ControllerInfo, error) {
	_, dir := projectPaths(c)
	build := BuildInfo{Release: "0.9.1-seed", Revision: "seed1234abcd"}
	info := &ControllerInfo{Version: ControllerVersion, Session: session, PID: pid,
		Root: c.Root, Mode: ControllerModeDetached, Started: time.Now().UTC().Format(time.RFC3339Nano), Build: &build}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, nil, err
	}
	if err = store.beginSession(info); err != nil {
		store.Close()
		return nil, nil, err
	}
	if err = writeJSON(controllerPath(dir), info); err != nil {
		store.Close()
		return nil, nil, err
	}
	// The row this session owns stays running for as long as the holder keeps
	// the writer; a closing writer marks no end here.
	store.shutdownAttempted = true
	return store, info, nil
}

func waitSeededController(t *testing.T, c Config, session string, pid int) *Store {
	t.Helper()
	store, _, err := seedControllerSession(c, session, pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// TestWaitRunOutcome covers the run predicate: an already-ended session, a
// drain during the wait, a restart that marks the bound session interrupted,
// a crash that leaves no record, and a live session that holds past the bound.
func TestWaitRunOutcome(t *testing.T) {
	t.Run("already ended", func(t *testing.T) {
		c := waitProject(t, nil)
		_, dir := projectPaths(c)
		store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		latest, err := store.latestSession()
		store.Close()
		if err != nil || latest == "" {
			t.Skipf("the seeded run records no session: %v", err)
		}
		code, doc, _ := runWaitCLI(t, "--run", "--timeout", "5s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Session == nil || doc.Observed.Session.Outcome != "stopped" {
			t.Fatalf("the recorded outcome must answer: %+v", doc.Observed.Session)
		}
		if doc.Observed.Session.Reason != "drained" || doc.Observed.Session.ExitCode == nil || *doc.Observed.Session.ExitCode != 0 {
			t.Fatalf("the shutdown evidence must ride the outcome: %+v", doc.Observed.Session)
		}
		if doc.Observed.Session.ID != latest {
			t.Fatalf("the wait must bind to the newest recorded session %s: %+v", latest, doc.Observed.Session)
		}
	})
	t.Run("drain during the wait", func(t *testing.T) {
		c := waitProject(t, nil)
		_, dir := projectPaths(c)
		live := waitSeededController(t, c, "run-live", os.Getpid())
		drained := make(chan error, 1)
		time.AfterFunc(700*time.Millisecond, func() {
			// The session's own writer ends it and removes its metadata, the
			// way a controller's drain boundary does.
			drained <- live.endSession(sessionExit{Reason: "drained", ExitCode: 0})
			removeController(dir, "run-live")
		})
		code, doc, _ := runWaitCLI(t, "--run", "--timeout", "15s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Condition.Session != "run-live" {
			t.Fatalf("the wait must bind to the live session: %+v", doc.Condition)
		}
		if doc.Observed.Session == nil || doc.Observed.Session.Outcome != "stopped" || doc.Observed.Session.Reason != "drained" {
			t.Fatalf("the drain must answer: %+v", doc.Observed.Session)
		}
		select {
		case err := <-drained:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(testBudget(10 * time.Second)):
			t.Fatal("the drain never finished")
		}
	})
	t.Run("restart marks the bound session interrupted", func(t *testing.T) {
		c := waitProject(t, nil)
		waitSeededController(t, c, "run-old", os.Getpid())
		replacement := make(chan *Store, 1)
		setupErr := make(chan error, 1)
		time.AfterFunc(700*time.Millisecond, func() {
			// The replacement controller marks every open session
			// interrupted when it begins its own. Its writer stays open —
			// the test goroutine takes it and closes it.
			store, _, err := seedControllerSession(c, "run-new", os.Getpid())
			if err != nil {
				setupErr <- err
				return
			}
			replacement <- store
		})
		code, doc, _ := runWaitCLI(t, "--run", "--timeout", "15s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Condition.Session != "run-old" {
			t.Fatalf("a replacement controller must not answer for the bound session: %+v", doc.Condition)
		}
		if doc.Observed.Session == nil || doc.Observed.Session.Outcome != "interrupted" {
			t.Fatalf("the restart must satisfy the bound wait as interrupted: %+v", doc.Observed.Session)
		}
		select {
		case err := <-setupErr:
			t.Fatal(err)
		case store := <-replacement:
			t.Cleanup(func() { store.Close() })
		case <-time.After(testBudget(10 * time.Second)):
			t.Fatal("the replacement controller never finished starting")
		}
	})
	t.Run("a crashed controller ends unclean", func(t *testing.T) {
		c := waitProject(t, nil)
		waitSeededController(t, c, "run-crash", deadPID(t))
		code, doc, _ := runWaitCLI(t, "--run", "--timeout", "5s", "--contract", "v2", "--json")
		if code != exitOK || doc.Outcome != waitSatisfied {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Session == nil || doc.Observed.Session.Outcome != "unclean" || !doc.Observed.Session.Unclean {
			t.Fatalf("a dead controller must end its run unclean: %+v", doc.Observed.Session)
		}
		if doc.Controller == nil || doc.Controller.State != "stale" {
			t.Fatalf("the stale ownership must be named: %+v", doc.Controller)
		}
	})
	t.Run("a live session holds past the bound", func(t *testing.T) {
		c := waitProject(t, nil)
		waitSeededController(t, c, "run-holding", os.Getpid())
		code, doc, _ := runWaitCLI(t, "--run", "--timeout", "1s", "--contract", "v2", "--json")
		if code != exitWaitTimeout || doc.Outcome != waitTimeout {
			t.Fatalf("outcome %q exit %d", doc.Outcome, code)
		}
		if doc.Observed.Session == nil || doc.Observed.Session.Outcome != "running" {
			t.Fatalf("a live session must hold: %+v", doc.Observed.Session)
		}
		if !strings.Contains(doc.Observed.Note, "still holds the run") {
			t.Fatalf("note %q", doc.Observed.Note)
		}
	})
}

// TestWaitClientCancellation covers the caller that interrupts the wait: exit
// 6, one cancelled document carrying the cursor the observation reached, the
// store released, and nothing cancelled underneath.
func TestWaitClientCancellation(t *testing.T) {
	c := waitProject(t, nil)
	_, dir := projectPaths(c)
	high := waitHighWater(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(700*time.Millisecond, cancel)
	cc, err := Parse([]string{"wait", "--cursor", fmt.Sprint(high), "--timeout", "30s", "--contract", "v2", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runWait(ctx, cc, &out, &stderr) }()
	select {
	case code := <-done:
		if code != exitCancelled {
			t.Fatalf("an interrupted wait must exit 6, found %d:\n%s%s", code, out.String(), stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a cancelled wait must return")
	}
	var doc waitDocument
	if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("the cancelled document must parse: %v\n%s", err, out.String())
	}
	if doc.Outcome != waitCancelled || doc.Observed.HighWater != high {
		t.Fatalf("outcome %q cursor %d", doc.Outcome, doc.Observed.HighWater)
	}
	// The wait released its connection: a writer can open the store at once.
	writer, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("the cancelled wait must release the store: %v", err)
	}
	writer.Close()
}

// TestWaitPollIsNotABusyLoop pins the cadence: a condition that never holds is
// observed a bounded number of times, not once per scheduler tick, and the
// first observation precedes any sleep.
func TestWaitPollIsNotABusyLoop(t *testing.T) {
	c := waitProject(t, waitTaskSeed("T1", "todo"))
	_, dir := projectPaths(c)
	cc, err := Parse([]string{"wait", "--task", "T1", "--status", "done", "--timeout", "3s", "--contract", "v2"})
	if err != nil {
		t.Fatal(err)
	}
	observations := 0
	w := &waitRun{c: cc, dir: dir, started: wallClock()}
	defer w.close()
	if err = w.attach(); err != nil {
		t.Fatal(err)
	}
	if err = w.resolveTarget(); err != nil {
		t.Fatal(err)
	}
	w.probe = func() { observations++ }
	outcome, err := w.observeUntil(context.Background(), 1200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != waitTimeout {
		t.Fatalf("outcome %s", outcome)
	}
	// One observation before the first sleep, then roughly one per 250 ms
	// interval inside a 1.2 s bound, plus the final read after it.
	if observations < 2 || observations > 12 {
		t.Fatalf("a 1.2s wait at a 250ms cadence observes a handful of times, observed %d", observations)
	}
}

// TestWaitFlagValidation covers the parse refusals: the wait options belong to
// the wait command, exactly one predicate is allowed, a status needs its task,
// the bound is required, and the streaming flag names its own command.
func TestWaitFlagValidation(t *testing.T) {
	cases := []struct {
		args []string
		code string
		want string
	}{
		{[]string{"wait", "--timeout", "5s"}, "usage_invalid", "one predicate"},
		{[]string{"wait", "--cursor", "5", "--task", "T1", "--timeout", "5s"}, "usage_invalid", "one predicate"},
		{[]string{"wait", "--task", "T1"}, "usage_invalid", "--timeout"},
		{[]string{"wait", "--task", "T1", "--status", "done"}, "usage_invalid", "--timeout"},
		{[]string{"wait", "--status", "done", "--timeout", "5s"}, "option_conflict", "--status names the target"},
		{[]string{"wait", "--task", "T1", "--status", "archived", "--timeout", "5s"}, "option_value_invalid", "todo, doing, blocked, done, or cancelled"},
		{[]string{"wait", "--cursor", "5", "--timeout", "1ms"}, "option_value_invalid", "at least one second"},
		{[]string{"wait", "--cursor", "-1", "--timeout", "5s"}, "option_value_invalid", "nonnegative event identifier"},
		{[]string{"wait", "--cursor", "5", "--timeout", "abc"}, "option_value_invalid", "at least one second"},
		{[]string{"wait", "--cursor", "5", "--follow", "--timeout", "5s"}, "option_conflict", "--follow streams events"},
		{[]string{"wait", "T1", "--timeout", "5s"}, "usage_invalid", "no positional"},
		{[]string{"wait", "--run", "--kind", "result", "--timeout", "5s"}, "option_command_mismatch", "--kind is supported by"},
		{[]string{"status", "--timeout", "5s"}, "option_command_mismatch", "--timeout is supported by looper wait"},
		{[]string{"events", "--run"}, "option_command_mismatch", "--run is supported by looper wait"},
		{[]string{"requests", "--status", "done"}, "option_command_mismatch", "--status is supported by looper wait"},
		{[]string{"inspect", "T1", "--cursor", "5"}, "option_command_mismatch", "--cursor is supported by looper wait"},
		{[]string{"wait", "--wait", "--run", "--timeout", "5s"}, "option_command_mismatch", "--wait is supported by"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.args)
		if err == nil {
			t.Errorf("%v parsed", tc.args)
			continue
		}
		p := publicErrorFor(err)
		if p.Code != tc.code {
			t.Errorf("%v: code %s, want %s (%s)", tc.args, p.Code, tc.code, p.Message)
		}
		if !strings.Contains(p.Message, tc.want) {
			t.Errorf("%v: message %q must contain %q", tc.args, p.Message, tc.want)
		}
	}
	for _, args := range [][]string{
		{"wait", "--cursor", "5", "--timeout", "30s"},
		{"wait", "--cursor", "5", "--timeout", "30"},
		{"wait", "--cursor", "0", "--timeout", "2m"},
		{"wait", "--task", "T1", "--timeout", "2m"},
		{"wait", "--task", "T1", "--status", "done", "--timeout", "5s"},
		{"wait", "--task", "T1", "--status", "cancelled", "--timeout", "5s"},
		{"wait", "--request", "req-1", "--timeout", "5s"},
		{"wait", "--run", "--timeout", "5s"},
		{"events", "--task", "T1"},
	} {
		if _, err := Parse(args); err != nil {
			t.Errorf("%v must parse: %v", args, err)
		}
	}
}

// TestWaitRequiresVersionTwoForJSON covers the contract negotiation: the wait
// document is a version 2 document, and a legacy JSON caller is refused the
// same way the runs and requests documents refuse it.
func TestWaitRequiresVersionTwoForJSON(t *testing.T) {
	waitProject(t, nil)
	var out, stderr bytes.Buffer
	code := Main([]string{"wait", "--cursor", "0", "--timeout", "5s", "--json"}, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "contract version 2") {
		t.Fatalf("the refusal must name the contract:\n%s", stderr.String())
	}
	// The version 2 form names the exact invocation to retry with.
	var v2out, v2err bytes.Buffer
	if code := Main([]string{"wait", "--cursor", "0", "--timeout", "5s", "--contract", "v2", "--json"}, &v2out, &v2err); code != exitOK {
		t.Fatalf("the named invocation must work, exit %d:\n%s%s", code, v2out.String(), v2err.String())
	}
	if !strings.Contains(v2out.String(), `"kind":"looper.wait"`) {
		t.Fatalf("the version 2 form must return the wait document:\n%s", v2out.String())
	}
}

// TestWaitPlainRendering covers the text form: the same facts the JSON
// document carries, under both contract selections, with the streaming
// alternative named.
func TestWaitPlainRendering(t *testing.T) {
	waitProject(t, waitTaskSeed("T1", "done"))
	for _, contract := range []string{"legacy", "v2"} {
		var out, stderr bytes.Buffer
		args := []string{"wait", "--task", "T1", "--status", "done", "--timeout", "5s"}
		if contract == "v2" {
			args = append(args, "--contract", "v2")
		}
		code := Main(args, &out, &stderr)
		if code != exitOK {
			t.Fatalf("%s: exit %d:\n%s%s", contract, code, out.String(), stderr.String())
		}
		text := out.String()
		for _, want := range []string{"task T1 reaches done", "satisfied", "cursor:", "looper events --contract v2 --follow"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s rendering must contain %q:\n%s", contract, want, text)
			}
		}
	}
}

// TestWaitNeedsASavedRun covers the project with nothing to observe.
func TestWaitNeedsASavedRun(t *testing.T) {
	base := runsBase(t)
	root := filepath.Join(filepath.Dir(base), "wait-empty")
	runsProjectAt(t, root)
	var out, stderr bytes.Buffer
	code := Main([]string{"wait", "--run", "--timeout", "5s", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitUnavailable {
		t.Fatalf("exit %d:\n%s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "run_not_found") {
		t.Fatalf("the refusal must name the missing run:\n%s", out.String())
	}
}

// TestMutationReceiptNamesTheFiniteWait covers the wait object every mutation
// receipt carries (AX-013): the command names `looper wait` with the request
// identifier, the bound matches the command family's default, and the schema
// that defined the object since AX-001 accepts the document.
func TestMutationReceiptNamesTheFiniteWait(t *testing.T) {
	c, dir := receiptProject(t, "add-task")
	seedReceiptState(t, dir, nil)
	tasks := filepath.Join(c.Root, "tasks.json")
	if err := os.WriteFile(tasks, []byte(`{"id":"T9","title":"nine","priority":1,"status":"todo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	cc := v2Config(c, "add-task", true)
	cc.Plans = []string{tasks}
	if code := runAddTask(cc, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	doc := decodeReceipt(t, out.String(), stderr.String())
	if doc.Wait == nil {
		t.Fatalf("the receipt must carry the wait object:\n%s", out.String())
	}
	if doc.Wait.Command != "looper wait --contract v2 --request "+doc.Request.ID+" --timeout 30s" {
		t.Fatalf("the wait object must name the command and its bound: %+v", doc.Wait)
	}
	if doc.Wait.TimeoutSeconds != 30 {
		t.Fatalf("the bound must match the settings-family default: %+v", doc.Wait)
	}
	if !strings.Contains(doc.Wait.Note, "never cancels the request") {
		t.Fatalf("the note must state the rule: %+v", doc.Wait)
	}
	var schema, instance any
	if err := json.Unmarshal(receiptContractSchema(t), &schema); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal([]byte(out.String()), &instance)
	if err := contractschema.Validate(schema, instance); err != nil {
		t.Fatalf("the receipt must validate with the wait object: %v", err)
	}

	// The add-plan family suggests its own longer bound, and the plain
	// receipt prints the same route.
	plan := filepath.Join(c.Root, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pc := v2Config(c, "add-plan", true)
	pc.Plans = []string{plan}
	out.Reset()
	stderr.Reset()
	if code := runAddPlan(pc, &out, &stderr); code != exitOK {
		t.Fatalf("add-plan exit %d: %s%s", code, out.String(), stderr.String())
	}
	planReceipt := decodeReceipt(t, out.String(), stderr.String())
	if planReceipt.Wait == nil || planReceipt.Wait.TimeoutSeconds != 60 {
		t.Fatalf("add-plan must suggest its own bound: %+v", planReceipt.Wait)
	}
	plain := v2Config(c, "add-task", false)
	plain.Plans = []string{tasks}
	out.Reset()
	stderr.Reset()
	if code := runAddTask(plain, &out, &stderr); code != exitOK {
		t.Fatalf("plain exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "Wait for the outcome with: looper wait --contract v2 --request ") {
		t.Fatalf("the plain receipt must name the wait route:\n%s", out.String())
	}
}

// TestCapabilitiesAdvertiseWaits covers the discovery surface: the client
// command list gains `wait` and the waits list names the four predicates the
// schema describes.
func TestCapabilitiesAdvertiseWaits(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Main([]string{"version", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	var doc capabilitiesDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range doc.Client.Commands {
		if name == "wait" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the client command list lost wait: %v", doc.Client.Commands)
	}
	want := []string{WaitCursorChange, WaitTaskState, WaitRequestOutcome, WaitRunOutcome}
	if len(doc.Client.Waits) != len(want) {
		t.Fatalf("the waits list must name the four predicates: %v", doc.Client.Waits)
	}
	for i, name := range want {
		if doc.Client.Waits[i] != name {
			t.Fatalf("waits list %v", doc.Client.Waits)
		}
	}
}

// TestStatusNamesTheFiniteWait covers the observation workflow: the compact
// status document carries the wait route bound to its own cursor, so a caller
// stops polling status.
func TestStatusNamesTheFiniteWait(t *testing.T) {
	c := waitProject(t, nil)
	high := waitHighWater(t, c)
	var out bytes.Buffer
	cc := v2Config(c, "status", true)
	if err := inspect(context.Background(), cc, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("\"wait\":\"looper wait --contract v2 --cursor %d --timeout 30s\"", high)) {
		t.Fatalf("the compact status must name the finite wait from its cursor:\n%s", out.String())
	}
	var plain bytes.Buffer
	pc := v2Config(c, "status", false)
	pc.Plain = true
	if err := inspect(context.Background(), pc, &plain); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plain.String(), "Wait for the next change with: looper wait") {
		t.Fatalf("the plain status must name the wait route:\n%s", plain.String())
	}
}
