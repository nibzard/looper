package looper

// The AX-022 acceptance fixtures. A detached start reports success only after
// the saved run state is publicly queryable — including the bootstrap window
// and immediately completed once runs — and a failed startup returns
// structured evidence through the readiness pipe. `looper stop --wait` waits
// boundedly for the durable stop result of the request it published, bound to
// the controller session it stopped, through active work, held corrections,
// startup, exit, and replacement controllers, without stopping a new run.
// Every hold is a stub barrier file, so each phase is deterministic.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// ax022Backlog is the backlog a stub bootstrap returns: one task that is done
// from the start, so a once run needs no worker.
const ax022Backlog = `{"schema_version":1,"goal":"fixture","source_files":[],"tasks":[{"id":"T001","title":"one","priority":1,"status":"done","details":"done from the start","depends_on":[],"tags":["project-done"]}]}`

// ax022Plan is the plan a stub returns for the planning prompt.
const ax022Plan = `{"setup":"","verify":"true","tasks":[]}`

// ax022Agent writes a stub agent whose answers are baked in: the backlog for
// bootstrap prompts, the plan for planning prompts, the review report for
// review prompts. When release names a file, the stub's first invocation
// blocks until that file exists, so a test can hold a controller in one
// startup phase and release it deterministically. The once marker lives in the
// stub's own directory, so two stubs in one test gate independently; the
// returned once path exists exactly while a gated invocation holds, so a test
// can wait for the hold to engage before it acts.
func ax022Agent(t *testing.T, release string) (bin, once string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent.sh")
	once = filepath.Join(dir, "once")
	gate := ""
	if release != "" {
		gate = `if [ ! -e "` + filepath.Join(dir, "once") + `" ]; then
  touch "` + filepath.Join(dir, "once") + `"
  while [ ! -e "` + release + `" ]; do sleep 0.05; done
fi
`
	}
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
` + gate + `result='` + ax022Backlog + `'
if printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result='` + ax022Plan + `'
fi
if printf '%s' "$p" | grep -q "review pass for Looper"; then
  result='` + reviewComplete + `'
fi
if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
printf '{"structured_output":%s}\n' "$result"
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin, once
}

// ax022DetachedStart points the daemon spawn at the built executable and
// returns the restore for the package-level override.
func ax022DetachedStart(t *testing.T, bin string) func() {
	t.Helper()
	orig := startExecutable
	startExecutable = func() (string, error) { return bin, nil }
	return func() { startExecutable = orig }
}

// ax022Release opens one stub barrier.
func ax022Release(t *testing.T, release string) {
	t.Helper()
	if err := os.WriteFile(release, []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
}

// ax022StopController interrupts a live controller by pid and waits for its
// metadata to disappear. A controller that already exited is a no-op.
func ax022StopController(t *testing.T, dir string, sig syscall.Signal) {
	t.Helper()
	info, err := readController(dir)
	if err != nil {
		return
	}
	_ = syscall.Kill(info.PID, sig)
	deadline := time.Now().Add(testBudget(20 * time.Second))
	for time.Now().Before(deadline) {
		if _, statErr := os.Stat(controllerPath(dir)); os.IsNotExist(statErr) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("controller %s did not exit after signal %v", info.Session, sig)
}

// ax022SavedState reads the saved state through the observer route a status
// query takes.
func ax022SavedState(t *testing.T, dir string) *State {
	t.Helper()
	store, err := openObservedStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snap.State
}

// The recorded race, pinned at the process level: a detached start on a fresh
// project reported success while bootstrap still ran, and the status call in
// the next moment answered "no saved run" (exit 1). The startup seed makes the
// run queryable before the readiness pipe reports success, so the same call
// finds the starting run at once, and the completed bootstrap replaces the
// seed with the real baseline.
func TestStartDetachedStatusFindsSavedRunDuringBootstrap(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start", "--once")
	release := filepath.Join(t.TempDir(), "release")
	stub, _ := ax022Agent(t, release)
	c.ClaudeBin, c.CodexBin = stub, stub
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		ax022Release(t, release)
		ax022StopController(t, dir, syscall.SIGINT)
	})
	var out, stderr strings.Builder
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	// Bootstrap is still blocked on the barrier: the state a status query
	// reads must exist anyway, because readiness was reported after it.
	if s := ax022SavedState(t, dir); s == nil {
		t.Fatal("start reported success before the saved run was queryable")
	} else if !s.StartupSeed {
		t.Fatalf("the bootstrap-window state must be the startup seed, got %+v", s)
	}
	// The public CLI, exactly as the recorded reproduction invoked it.
	cmd := exec.Command(looperBin, "status", "--json")
	cmd.Dir = c.Root
	var statusOut, statusErr strings.Builder
	cmd.Stdout, cmd.Stderr = &statusOut, &statusErr
	if err := cmd.Run(); err != nil {
		t.Fatalf("status right after start must find the run: %v\nstdout: %s\nstderr: %s", err, statusOut.String(), statusErr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(statusOut.String()), &doc); err != nil {
		t.Fatalf("status output is not JSON: %v\n%s", err, statusOut.String())
	}
	// The seed the query just read is saved-state machinery: the frozen
	// legacy document admits no startup_seed key, so the window a detached
	// start holds must not leak the marker (T122).
	if _, ok := doc["startup_seed"]; ok {
		t.Fatalf("status during the bootstrap window emits startup_seed; the key stays out of the frozen document: %s", statusOut.String())
	}
	// The version 2 compact status reads the same seed through its own route.
	v2 := exec.Command(looperBin, "status", "--contract", "v2", "--json")
	v2.Dir = c.Root
	var v2Out, v2Err strings.Builder
	v2.Stdout, v2.Stderr = &v2Out, &v2Err
	if err := v2.Run(); err != nil {
		t.Fatalf("v2 status right after start must find the run: %v\nstdout: %s\nstderr: %s", err, v2Out.String(), v2Err.String())
	}
	// Releasing the barrier finishes bootstrap; the once run completes and the
	// real baseline replaces the seed. The baseline must land before the
	// interrupt: a stop that races bootstrap leaves the seed behind by design,
	// covered by the rebootstrap case below.
	ax022Release(t, release)
	waitFor(t, "the bootstrapped baseline to replace the seed", func() bool {
		s := ax022SavedState(t, dir)
		return s != nil && !s.StartupSeed && s.ExportHash != ""
	})
	ax022StopController(t, dir, syscall.SIGINT)
	s := ax022SavedState(t, dir)
	if s == nil || s.StartupSeed {
		t.Fatalf("the completed run must keep a real saved state, got %+v", s)
	}
	if s.ExportHash == "" || len(s.Backlog.Tasks) != 1 || s.Backlog.Tasks[0].ID != "T001" {
		t.Fatalf("the bootstrapped backlog must be the saved baseline, got %+v", s.Backlog.Tasks)
	}
}

// Success stays defined by queryability for a once run that finishes before
// the parent reads the readiness pipe: the exited branch keeps its guarantee
// because the seed precedes readiness.
func TestStartDetachedOnceRunStaysQueryable(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start", "--once")
	stub, _ := ax022Agent(t, "")
	c.ClaudeBin, c.CodexBin = stub, stub
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	t.Cleanup(func() { ax022StopController(t, dir, syscall.SIGINT) })
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "fixture", Tasks: []Task{
		{ID: "T001", Title: "one", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	if s := ax022SavedState(t, dir); s == nil {
		t.Fatal("a successful once start must leave a queryable saved run at once")
	}
	ax022StopController(t, dir, syscall.SIGINT)
	if s := ax022SavedState(t, dir); s == nil {
		t.Fatal("the completed once run must keep its saved state")
	}
}

// A startup the seed rejects — an invalid task file — is a startup failure:
// the parent reports the structured evidence instead of a detached controller
// that dies after success was printed, and no controller metadata survives.
func TestStartDetachedInvalidBacklogReportsStructuredStartupFailure(t *testing.T) {
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start")
	if err := os.WriteFile(c.Todo, []byte(`{"schema_version":1,`), 0600); err != nil {
		t.Fatal(err)
	}
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	var out, stderr strings.Builder
	code := StartDetached(v2Config(c, "start", true), &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("an invalid task file must fail the start: %d %s %s", code, out.String(), stderr.String())
	}
	var doc struct {
		Kind  string `json:"kind"`
		Error struct {
			Code     string `json:"code"`
			Message  string `json:"message"`
			Evidence []struct {
				Kind string `json:"kind"`
			} `json:"evidence"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("the failure is not one error document: %v\n%s", err, out.String())
	}
	if doc.Kind != "looper.error" || doc.Error.Code != "startup_failed" {
		t.Fatalf("unexpected failure document: %+v", doc)
	}
	if !strings.Contains(doc.Error.Message, "invalid backlog") {
		t.Fatalf("the failure must name the invalid backlog: %q", doc.Error.Message)
	}
	if len(doc.Error.Evidence) == 0 || doc.Error.Evidence[0].Kind != "log" {
		t.Fatalf("the failure must reference the retained log: %+v", doc.Error.Evidence)
	}
	if _, err := os.Stat(controllerPath(dir)); !os.IsNotExist(err) {
		t.Fatal("a failed startup must leave no controller metadata")
	}
	// The legacy rendering keeps its frozen startup-failure lines.
	var plain, plainErr strings.Builder
	if code := StartDetached(c, &plain, &plainErr); code != exitOperationFailed {
		t.Fatalf("legacy start must fail too: %d", code)
	}
	if !strings.Contains(plain.String()+plainErr.String(), "did not start") {
		t.Fatalf("legacy startup failure line missing: %q %q", plain.String(), plainErr.String())
	}
}

// The startup seed is not a baseline to resume: initialization treats it as
// absent, adopts its snapshot when nothing moved (one run ref, no duplicate
// commit), and re-bootstraps after a crash that left only the seed behind.
func TestStartupSeedAdoptedAndRebootstrap(t *testing.T) {
	t.Run("adopt", func(t *testing.T) {
		c := repoConfig(t, "run")
		todo := Backlog{Version: 1, Sources: []string{}, Goal: "fixture", Tasks: []Task{
			{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
		}}
		if err := writeJSON(c.Todo, todo); err != nil {
			t.Fatal(err)
		}
		e, lock, err := NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		defer e.Store.Close()
		if err = e.seedStartupState(); err != nil {
			t.Fatal(err)
		}
		refs := func() int {
			text, err := gitText(c.Root, "for-each-ref", "refs/looper", "--format=%(refname)")
			if err != nil {
				t.Fatal(err)
			}
			return len(strings.Fields(text))
		}
		if n := refs(); n != 1 {
			t.Fatalf("the startup seed must create one run ref, got %d", n)
		}
		if s, _ := e.Store.Load(); s == nil || !s.StartupSeed {
			t.Fatalf("the seed must be saved marked, got %+v", s)
		}
		if err = e.initialize(context.Background()); err != nil {
			t.Fatal(err)
		}
		s, _ := e.Store.Load()
		if s == nil || s.StartupSeed {
			t.Fatalf("initialization must replace the seed with the real baseline, got %+v", s)
		}
		if s.ExportHash == "" {
			t.Fatal("the adopted baseline must keep the accepted export digest")
		}
		if n := refs(); n != 1 {
			t.Fatalf("adopting the seed must not add a second run ref, got %d", n)
		}
	})
	t.Run("rebootstrap", func(t *testing.T) {
		c := repoConfig(t, "run")
		bin, _ := fakeAgent(t, ax022Backlog)
		t.Setenv("LOOPER_FAKE_PLAN", ax022Plan)
		t.Setenv("LOOPER_FAKE_REVIEW", reviewComplete)
		c.CodexBin, c.ClaudeBin = bin, bin
		e, lock, err := NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if err = e.seedStartupState(); err != nil {
			t.Fatal(err)
		}
		e.Store.Close()
		lock.Close()
		// A fresh controller finds only the seed; bootstrap must run again and
		// leave the real baseline.
		e2, lock2, err := NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		defer lock2.Close()
		defer e2.Store.Close()
		ctx, cancel := context.WithTimeout(context.Background(), testBudget(30*time.Second))
		defer cancel()
		if err = e2.initialize(ctx); err != nil {
			t.Fatal(err)
		}
		s, err := e2.Store.Load()
		if err != nil {
			t.Fatal(err)
		}
		if s == nil || s.StartupSeed {
			t.Fatalf("the restarted controller must replace the seed, got %+v", s)
		}
		if s.ExportHash == "" || len(s.Backlog.Tasks) != 1 {
			t.Fatalf("the restarted controller must bootstrap the real backlog, got %+v", s.Backlog)
		}
		// Bootstrap changed the tree, so the fresh baseline could not adopt the
		// seed's snapshot; the seed's ref must still be the one ref, advanced.
		text, err := gitText(c.Root, "for-each-ref", "refs/looper", "--format=%(refname)")
		if err != nil {
			t.Fatal(err)
		}
		if n := len(strings.Fields(text)); n != 1 {
			t.Fatalf("a bootstrapped run must keep one run ref, got %d", n)
		}
	})
}

// ax022StopWait runs one `looper stop --wait` in a goroutine and reports its
// result, so a test can keep acting while the wait observes.
type ax022StopWaitResult struct {
	code   int
	out    string
	stderr string
	done   chan struct{}
}

func ax022StopWaitGo(c Config, v2 bool, timeout time.Duration) *ax022StopWaitResult {
	cc := c
	cc.Command, cc.Wait, cc.WaitTimeout, cc.WaitTimeoutSet = "stop", true, timeout, true
	if v2 {
		cc = v2Config(cc, "stop", true)
	}
	_, dir := projectPaths(cc)
	r := &ax022StopWaitResult{done: make(chan struct{})}
	go func() {
		var out, stderr strings.Builder
		r.code = runStop(cc, dir, &out, &stderr)
		r.out, r.stderr = out.String(), stderr.String()
		close(r.done)
	}()
	return r
}

// ax022Record parses the one request-record document a satisfied version 2
// stop wait prints.
func ax022Record(t *testing.T, out string) requestRecordDocument {
	t.Helper()
	var doc requestRecordDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("the stop wait printed no record document: %v\n%s", err, out)
	}
	if doc.Kind != "looper.request.record" || doc.Request == nil {
		t.Fatalf("unexpected stop wait output: %s", out)
	}
	return doc
}

// ax022WaitForStopRequest waits until the running stop wait published its
// request. A stop that returned without publishing one answers with its own
// failure instead of a timeout.
func ax022WaitForStopRequest(t *testing.T, dir string, r *ax022StopWaitResult) {
	t.Helper()
	waitFor(t, "the stop request to land", func() bool {
		req, _, readErr := readStopRequest(dir)
		if readErr == nil && req != nil {
			return true
		}
		select {
		case <-r.done:
			t.Fatalf("stop --wait returned %d without a request: %s %s", r.code, r.out, r.stderr)
		default:
		}
		return false
	})
}

// The controller's own exit decides a stop it never honored: an interrupt
// cancels the run mid-wait, the shutdown release records the rejection, and
// the wait ends on that durable outcome without waiting for a replacement.
func TestStopWaitEndsWhenControllerExitsWithoutHonoring(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start")
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "fixture", Tasks: []Task{
		{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(t.TempDir(), "release")
	claude, claudeOnce := ax022Agent(t, release)
	codex, _ := ax022Agent(t, release)
	c.ClaudeBin, c.CodexBin = claude, codex
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		ax022Release(t, release)
		ax022StopController(t, dir, syscall.SIGINT)
	})
	var out, stderr strings.Builder
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	original, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to adopt its baseline", func() bool {
		s := ax022SavedState(t, dir)
		return s != nil && !s.StartupSeed
	})
	waitFor(t, "the planning hold to engage", func() bool {
		_, statErr := os.Stat(claudeOnce)
		return !os.IsNotExist(statErr)
	})
	r := ax022StopWaitGo(c, true, testBudget(30*time.Second))
	ax022WaitForStopRequest(t, dir, r)
	// The controller is interrupted mid-wait without honoring the request.
	if err = syscall.Kill(original.PID, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the stop wait never returned after the controller exited")
	}
	if r.code != exitOK {
		t.Fatalf("the exited controller's durable rejection must answer the wait: %d %s %s", r.code, r.out, r.stderr)
	}
	doc := ax022Record(t, r.out)
	if doc.Request.State != RequestRejected {
		t.Fatalf("the un-honored request must end rejected: %+v", doc.Request)
	}
	if !strings.Contains(doc.Request.ID, original.Session) {
		t.Fatalf("the record must name the exited session's request: %q vs %q", doc.Request.ID, original.Session)
	}
	if doc.Request.Error == nil || !strings.Contains(doc.Request.Error.Message, "ended before it could honor") {
		t.Fatalf("the rejection must name the un-honored drain: %+v", doc.Request)
	}
	if _, err = os.Stat(stopRequestPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the released stop request must be gone")
	}
}

// The drain wait observes active work: the stop request lands while a check
// holds the loop, the finished work drains at the boundary, and the wait
// returns the durable applied record naming the stopped session.
func TestStopWaitReportsDurableDrainResult(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	r := ax022StopWaitGo(c, true, testBudget(30*time.Second))
	// The held check is the active work the drain waits for.
	ax022Release(t, release)
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the stop wait never returned")
	}
	if r.code != exitOK {
		t.Fatalf("a satisfied stop wait exits 0, got %d: %s %s", r.code, r.out, r.stderr)
	}
	doc := ax022Record(t, r.out)
	if doc.Request.State != RequestApplied {
		t.Fatalf("the honored drain must record an applied stop: %+v", doc.Request)
	}
	if doc.Request.ID != "stop-"+e.controller.Session {
		t.Fatalf("the record must name the stopped session's request: %q vs %q", doc.Request.ID, e.controller.Session)
	}
	select {
	case out := <-done:
		if out.code != exitOK || out.err != nil {
			t.Fatalf("the drained controller must exit cleanly: %d %v", out.code, out.err)
		}
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the controller never drained")
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the session must record a drained exit: %+v", exit)
	}
}

// The drain wait outranks the held correction wait: a stop that arrives while
// an invalid task-file edit holds the boundary ends the run there, and the
// wait returns the applied record instead of following the correction.
func TestStopWaitDrainsDuringHeldCorrectionWait(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	// The invalid edit lands while the check is in flight, then the check
	// finishes into a boundary that holds for a corrected task file.
	if err := os.WriteFile(c.Todo, []byte("not a backlog"), 0600); err != nil {
		t.Fatal(err)
	}
	ax022Release(t, release)
	waitFor(t, "the correction wait", func() bool {
		return countKindEvent(t, e.Dir, "waiting", "needs correction") >= 1
	})
	r := ax022StopWaitGo(c, true, testBudget(30*time.Second))
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the stop wait never returned from the correction hold")
	}
	if r.code != exitOK {
		t.Fatalf("a stop during the correction hold must drain: %d %s %s", r.code, r.out, r.stderr)
	}
	doc := ax022Record(t, r.out)
	if doc.Request.State != RequestApplied {
		t.Fatalf("the drain must outrank the correction wait: %+v", doc.Request)
	}
	select {
	case <-done:
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the controller never drained from the hold")
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "drained" {
		t.Fatalf("the held session must record a drained exit: %+v", exit)
	}
}

// The legacy wait keeps the frozen submission line, then reports the durable
// outcome the bounded wait observed in plain text.
func TestStopWaitLegacyReportsDrainOutcome(t *testing.T) {
	e, c, done, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	r := ax022StopWaitGo(c, false, testBudget(30*time.Second))
	ax022Release(t, release)
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the legacy stop wait never returned")
	}
	if r.code != exitOK {
		t.Fatalf("a satisfied legacy stop wait exits 0, got %d: %s %s", r.code, r.out, r.stderr)
	}
	lines := strings.SplitN(r.out, "\n", 2)
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "Stop requested for session ") ||
		!strings.HasSuffix(lines[0], "Active work will drain.") {
		t.Fatalf("the frozen legacy submission line must stay first: %q", r.out)
	}
	if !strings.Contains(r.out, "Stopped after draining active work") {
		t.Fatalf("the legacy wait must report the durable drain summary: %q", r.out)
	}
	select {
	case <-done:
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the controller never drained")
	}
}

// A startup the guard refuses streams no journal line under --json: the
// unclean previous session's recovery note waits for a successful start, so
// the stream holds exactly the launch event and the error document
// (looper:finding:ax022-run-stream-error-document-position). The store still
// journals the note for `looper events`.
func TestStartupGuardFailureStreamsNoJournalLine(t *testing.T) {
	c := repoConfig(t, "run")
	tasks := []Task{{ID: "T001", Title: "one", Priority: 1, Status: "doing"}}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	saved := digestFile(c.Todo)
	if err = store.Save(&State{
		Version: 2, Root: c.Root, Todo: c.Todo, Head: "head", Branch: "main",
		Ref: "refs/heads/main", Tip: "tip", ExportHash: saved,
		Backlog:  Backlog{Version: 1, Tasks: tasks},
		Attempts: []*Attempt{{ID: "a1", Task: tasks[0], Phase: "running", Worktrees: []string{}}},
	}); err != nil {
		t.Fatal(err)
	}
	// A session row that never ended: the next start sees an unclean exit.
	if _, err = store.db.Exec("INSERT INTO sessions(id,pid,identity,started,outcome) VALUES('run-dead',1,'x',?,'running')",
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	store.Close()
	// An external edit invalidates the saved contract while work is retained.
	tasks[0].Title = "edited outside Looper"
	if err = writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	code := Main([]string{"run", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("the guarded restart must fail: %d %s %s", code, out.String(), stderr.String())
	}
	var kinds []string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var doc struct {
			Kind  string `json:"kind"`
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err = json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("a stream value is not JSON: %v\n%s", err, out.String())
		}
		kinds = append(kinds, doc.Kind)
		if doc.Kind == "looper.error" && doc.Error.Code != "backlog_contract_changed" {
			t.Fatalf("unexpected failure code: %s", line)
		}
	}
	if len(kinds) != 2 || kinds[0] != "launch" || kinds[1] != "looper.error" {
		t.Fatalf("a guarded startup must stream only the launch event and the error document, got %v:\n%s", kinds, out.String())
	}
	// The unclean end still journals store-side, so events can explain it.
	saved2, err := openObservedStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer saved2.Close()
	events, err := saved2.RecentEvents(-1, 50)
	if err != nil {
		t.Fatal(err)
	}
	noted := false
	for _, ev := range events {
		if ev.Kind == "recovery" && strings.Contains(ev.Message, "ended uncleanly") {
			noted = true
		}
	}
	if !noted {
		t.Fatal("the unclean previous session must journal to the store")
	}
}

// A bounded wait that ends without the drain is an outcome of the wait: the
// version 2 transport reports request_pending with exit 5, the stop request
// stays durable, and the controller keeps draining.
func TestStopWaitTimeoutIsAnOutcome(t *testing.T) {
	e, c, _, release := holdRunEngine(t, holdVerify)
	waitFor(t, "the check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
	t.Cleanup(func() { ax022Release(t, release) })
	r := ax022StopWaitGo(c, true, 300*time.Millisecond)
	select {
	case <-r.done:
	case <-time.After(testBudget(10 * time.Second)):
		t.Fatal("the bounded stop wait never returned")
	}
	if r.code != exitWaitTimeout {
		t.Fatalf("a stop wait timeout exits 5, got %d: %s %s", r.code, r.out, r.stderr)
	}
	var doc struct {
		Error struct {
			Code  string `json:"code"`
			Retry string `json:"retry"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.out), &doc); err != nil {
		t.Fatalf("the timeout is not one error document: %v\n%s", err, r.out)
	}
	if doc.Error.Code != "request_pending" || doc.Error.Retry != retryYes {
		t.Fatalf("unexpected timeout document: %+v", doc.Error)
	}
	if _, err := os.Stat(stopRequestPath(e.Dir)); err != nil {
		t.Fatal("a timed-out stop wait must leave its request durable")
	}
}

// A drain requested during startup remains pending until initialization
// reaches its first boundary: the wait outlives a held bootstrap, then returns
// the applied record when the controller drains instead of running work.
func TestStopWaitDuringStartupDrainsAtFirstBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start")
	release := filepath.Join(t.TempDir(), "release")
	stub, _ := ax022Agent(t, release)
	c.ClaudeBin, c.CodexBin = stub, stub
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		ax022Release(t, release)
		ax022StopController(t, dir, syscall.SIGINT)
	})
	var out, stderr strings.Builder
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	// The controller is held inside bootstrap; the stop request binds to its
	// published session and waits.
	r := ax022StopWaitGo(c, true, testBudget(30*time.Second))
	ax022Release(t, release)
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the startup stop wait never returned")
	}
	if r.code != exitOK {
		t.Fatalf("the startup drain must satisfy the wait: %d %s %s", r.code, r.out, r.stderr)
	}
	doc := ax022Record(t, r.out)
	if doc.Request.State != RequestApplied {
		t.Fatalf("the startup request must drain at the first boundary: %+v", doc.Request)
	}
	ax022StopController(t, dir, syscall.SIGINT)
}

// A replacement controller ends the wait without being stopped by it: the
// original controller dies mid-wait, the next controller's startup sweep
// rejects the stranded request, the wait reports that durable rejection, and
// the new run keeps running.
func TestStopWaitEndsWhenReplacementControllerSweepsRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level daemon test")
	}
	looperBin := detachedBinary(t)
	c := repoConfig(t, "start")
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "fixture", Tasks: []Task{
		{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	// Each controller gets its own stub: the first agent call (planning)
	// blocks, so a controller holds the loop with saved state and no finished
	// agent work. The hold must engage before the stop request is written: a
	// request that beats the controller to its first boundary is honored at
	// once (the startup-drain case) and never strands for a sweep.
	releaseA := filepath.Join(t.TempDir(), "release")
	releaseB := filepath.Join(t.TempDir(), "release")
	claudeA, claudeOnceA := ax022Agent(t, releaseA)
	codexA, codexOnceA := ax022Agent(t, releaseA)
	c.ClaudeBin, c.CodexBin = claudeA, codexA
	defer ax022DetachedStart(t, looperBin)()
	_, dir := projectPaths(c)
	t.Cleanup(func() {
		ax022Release(t, releaseA)
		ax022Release(t, releaseB)
		ax022StopController(t, dir, syscall.SIGINT)
	})
	var out, stderr strings.Builder
	if code := StartDetached(c, &out, &stderr); code != 0 {
		t.Fatalf("detached start failed: %d %s %s", code, out.String(), stderr.String())
	}
	original, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the run to adopt its baseline", func() bool {
		s := ax022SavedState(t, dir)
		return s != nil && !s.StartupSeed
	})
	waitFor(t, "the planning hold to engage", func() bool {
		_, a := os.Stat(claudeOnceA)
		_, b := os.Stat(codexOnceA)
		return !os.IsNotExist(a) || !os.IsNotExist(b)
	})
	r := ax022StopWaitGo(c, true, testBudget(30*time.Second))
	ax022WaitForStopRequest(t, dir, r)
	// The original controller dies mid-wait without honoring the request.
	if err = syscall.Kill(original.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the original controller to die", func() bool {
		return syscall.Kill(original.PID, 0) != nil
	})
	// A replacement controller starts; its startup sweep is what decides the
	// stranded request, and it must survive the wait that observed it.
	claudeB, _ := ax022Agent(t, releaseB)
	codexB, _ := ax022Agent(t, releaseB)
	c.ClaudeBin, c.CodexBin = claudeB, codexB
	var out2, stderr2 strings.Builder
	if code := StartDetached(c, &out2, &stderr2); code != 0 {
		t.Fatalf("replacement start failed: %d %s %s", code, out2.String(), stderr2.String())
	}
	select {
	case <-r.done:
	case <-time.After(testBudget(40 * time.Second)):
		t.Fatal("the stop wait never returned after the replacement started")
	}
	if r.code != exitOK {
		t.Fatalf("the swept request must still answer the wait: %d %s %s", r.code, r.out, r.stderr)
	}
	doc := ax022Record(t, r.out)
	if doc.Request.State != RequestRejected {
		t.Fatalf("the replacement's sweep must reject the stranded request: %+v", doc.Request)
	}
	if !strings.Contains(doc.Request.ID, original.Session) {
		t.Fatalf("the record must name the original session's request: %q vs %q", doc.Request.ID, original.Session)
	}
	// The replacement keeps running and is not drained by the wait.
	info, err := readController(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Session == original.Session {
		t.Fatal("the replacement must publish its own session")
	}
	if err = syscall.Kill(info.PID, 0); err != nil {
		t.Fatal("the replacement controller must survive the stop wait")
	}
	if _, err = os.Stat(stopRequestPath(dir)); !os.IsNotExist(err) {
		t.Fatal("the swept stop request must be gone")
	}
}
