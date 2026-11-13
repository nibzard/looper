package looper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
	"github.com/nibzard/looper/internal/ui"
)

// statusCompactSchema reads the published compact status schema so the
// documents the CLI really produces are checked against it, not only the
// recorded examples. The path is anchored at this file because fixtures chdir
// into a temporary project before the document is validated.
func statusCompactSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "status-compact.schema.json"))
	if err != nil {
		t.Skipf("compact status schema is absent: %v", err)
	}
	return b
}

// statusFixture owns one temporary project with a saved run. Tests publish
// controller metadata, a view, or a stop request on top of it to shape the
// case the compact contract must distinguish.
type statusFixture struct {
	c    Config
	root string
	dir  string
	lock *os.File
}

func newStatusFixture(t *testing.T, args ...string) *statusFixture {
	t.Helper()
	clearSettingEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "base"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	if _, err = gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(append([]string{"status"}, args...))
	if err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	f := &statusFixture{c: c, root: root, dir: dir}
	t.Cleanup(f.release)
	return f
}

func (f *statusFixture) release() {
	if f.lock != nil {
		f.lock.Close()
		f.lock = nil
	}
}

func (f *statusFixture) storePath() string { return filepath.Join(f.dir, "state.db") }

func (f *statusFixture) openStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(f.storePath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func (f *statusFixture) save(t *testing.T, s *State) {
	t.Helper()
	store := f.openStore(t)
	if err := store.Save(s); err != nil {
		t.Fatal(err)
	}
}

func (f *statusFixture) event(t *testing.T, kind, message string, data any) {
	t.Helper()
	store := f.openStore(t)
	if _, err := store.Event(kind, message, data); err != nil {
		t.Fatal(err)
	}
}

// ownRepository holds the repository lock and publishes controller metadata
// for a live controller session, so ownership evidence comes from the lock
// and the recorded process is this test process.
func (f *statusFixture) ownRepository(t *testing.T, session string, started time.Time) {
	t.Helper()
	common, err := gitText(f.root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(f.root, common)
	}
	lock, err := lockRepository(common)
	if err != nil {
		t.Fatal(err)
	}
	f.lock = lock
	info := &ControllerInfo{
		Version: ControllerVersion, Session: session, PID: os.Getpid(), Root: f.root,
		Mode: ControllerModeDetached, Started: started.UTC().Format(time.RFC3339Nano),
		Build:        &BuildInfo{Release: "test-release", Revision: "testrevision", Modified: false},
		Capabilities: newControllerCapabilities(),
	}
	if err = writeJSON(controllerPath(f.dir), info); err != nil {
		t.Fatal(err)
	}
}

func (f *statusFixture) publishView(t *testing.T, v ui.ViewSnapshot) {
	t.Helper()
	v.Version, v.Now = ui.ViewVersion, time.Now().UTC().Format(time.RFC3339)
	if err := writeJSON(viewPath(f.dir), &v); err != nil {
		t.Fatal(err)
	}
}

// attemptOutput writes one agent output stream for an attempt directory and
// returns the path. age controls how stale the output reads.
func attemptOutput(t *testing.T, dir string, age time.Duration, bytes int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "work-00"), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "work-00", "events-00.jsonl")
	body := strings.Repeat("{\"type\":\"assistant\"}\n", max(1, bytes/22))
	if err := atomicWrite(log, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(log, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return log
}

// attemptCheckOutput writes one verification log for an attempt directory.
// A verifying attempt reads its check logs, not its implementation stream.
func attemptCheckOutput(t *testing.T, dir string, age time.Duration, bytes int) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "checks", "abc123"), 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "checks", "abc123", "verify.log")
	if err := atomicWrite(log, []byte(strings.Repeat("ok\n", max(1, bytes/3))), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-age)
	if err := os.Chtimes(log, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	return log
}

// statusState builds one saved run: a done task, a blocked task, a ready
// task, and the described active attempts.
func statusState(f *statusFixture, attempts []*Attempt, integrated int) *State {
	tasks := []Task{
		{ID: "T1", Title: "Done work", Priority: 1, Status: "done"},
		{ID: "T2", Title: "Blocked work", Priority: 2, Status: "blocked", Blockers: []string{"needs an operator decision"}, UnblockWhen: "The operator decides."},
		{ID: "T3", Title: "Ready work", Priority: 1, Status: "todo"},
	}
	for _, a := range attempts {
		for i := range tasks {
			if tasks[i].ID == a.Task.ID {
				tasks[i].Status = "doing"
			}
		}
	}
	return &State{
		Version: 2, Root: f.root, Todo: f.c.Todo, Head: "head", Branch: "main",
		Ref: "refs/looper/run-test0001", Tip: "tip", Published: "published", ExportHash: "export-hash",
		Backlog:    Backlog{Version: 1, Sources: []string{}, Tasks: tasks},
		Attempts:   attempts,
		Integrated: integrated,
	}
}

// statusActiveAttempt builds an attempt whose output evidence lives in dir.
func statusActiveAttempt(id, taskID, phase, dir string) *Attempt {
	return &Attempt{ID: id, Task: Task{ID: taskID, Title: taskID, Priority: 1, Status: "doing"}, Agent: "claude",
		Base: "base", Directory: dir, Ref: "ref", Phase: phase, Worktrees: []string{}}
}

// runStatusCompact runs the public CLI and returns the exit code with the
// decoded compact document. It fails unless standard output carries exactly
// one JSON document that validates against the published schema and stays
// within the byte budget.
func runStatusCompact(t *testing.T, args ...string) (int, *statusCompactDocument, []byte) {
	t.Helper()
	var out, stderr strings.Builder
	code := Main(append([]string{"status"}, args...), &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: stderr=%s stdout=%s", code, stderr.String(), out.String())
	}
	data := []byte(out.String())
	if len(data) > statusBytesBudget {
		t.Fatalf("compact status is %d bytes, over the %d byte budget:\n%s", len(data), statusBytesBudget, data)
	}
	var doc statusCompactDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("stdout is not one compact status document: %v\n%s", err, data)
	}
	remarshaled, err := json.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(statusCompactSchema(t), remarshaled); err != nil {
		t.Fatalf("the produced document violates the compact status schema: %v\n%s", err, data)
	}
	return code, &doc, data
}

// TestStatusCompactDocumentMatchesSchema serves the compact document from a
// run with live ownership, active work, and a journaled transition, and
// checks the facts a routine observation needs.
func TestStatusCompactDocumentMatchesSchema(t *testing.T) {
	f := newStatusFixture(t)
	work := t.TempDir()
	attemptOutput(t, work, 2*time.Second, 4096)
	quiet := t.TempDir()
	attemptCheckOutput(t, quiet, 20*time.Minute, 8192)
	attempts := []*Attempt{
		statusActiveAttempt("task-0001", "T3", "running", work),
		statusActiveAttempt("task-0002", "T4", "verifying", quiet),
	}
	attempts[1].Task.ID, attempts[1].Task.Title = "T4", "Checking work"
	s := statusState(f, attempts, 7)
	s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T4", Title: "Checking work", Priority: 1, Status: "doing"})
	f.save(t, s)
	f.event(t, "start", "Session started", nil)
	f.event(t, "progress", "2 implementing | project 1/4 complete", nil)
	f.event(t, "worker_activity", "T3 Tool started: Read", map[string]string{"task_id": "T3"})
	f.event(t, "check", "Checking T4 with integrated work...", map[string]string{"task_id": "T4", "attempt_id": "task-0002"})
	session := "run-test0001"
	started := time.Now().Add(-31 * time.Minute)
	f.ownRepository(t, session, started)
	f.publishView(t, ui.ViewSnapshot{Session: session, State: "waiting", ActivityDetail: "task file input needs correction",
		ActivityUpdated: time.Now().Add(-3 * time.Minute).UTC().Format(time.RFC3339), Draining: false})

	_, doc, data := runStatusCompact(t, "--contract", "v2", "--json")

	if doc.Kind != statusCompactKind || doc.ContractVersion != ContractVersionV2 {
		t.Fatalf("document identity: %+v", doc)
	}
	if doc.Controller.State != "controlling" || !doc.Controller.Owned || doc.Controller.OwnershipEvidence != "lock" {
		t.Fatalf("controller: %+v", doc.Controller)
	}
	if doc.Controller.OwnershipAgeSeconds == nil || *doc.Controller.OwnershipAgeSeconds < 1800 {
		t.Fatalf("ownership age: %+v", doc.Controller)
	}
	if doc.Controller.ViewMatches == nil || !*doc.Controller.ViewMatches {
		t.Fatalf("view evidence: %+v", doc.Controller)
	}
	if doc.Run.ID != "run-test0001" || doc.Run.ControllerSession != session || doc.Run.Mode != "detached" {
		t.Fatalf("run identity: %+v", doc.Run)
	}
	if doc.Builds.Client.Release == "" || doc.Builds.Controller == nil {
		t.Fatalf("builds: %+v", doc.Builds)
	}
	// The published controller build differs from this executable, and the
	// document says so instead of hiding the difference.
	if doc.Builds.Match == nil || *doc.Builds.Match || doc.Builds.Note == "" {
		t.Fatalf("a differing controller build must be reported: %+v", doc.Builds)
	}
	want := statusCounts{Done: 1, Todo: 2, Blocked: 1, Ready: 0, InFlight: 1, Integrated: 7}
	if doc.Work.Counts != want {
		t.Fatalf("counts: got %+v want %+v", doc.Work.Counts, want)
	}
	if len(doc.Work.Active) != 2 {
		t.Fatalf("active work: %+v", doc.Work.Active)
	}
	// The newest attempt renders first.
	if doc.Work.Active[0].TaskID != "T4" || doc.Work.Active[0].Phase != "verifying" {
		t.Fatalf("active order: %+v", doc.Work.Active)
	}
	if doc.Work.Active[0].Progress != "quiet" || doc.Work.Active[0].OutputAgeSeconds == nil || *doc.Work.Active[0].OutputAgeSeconds < 600 {
		t.Fatalf("quiet output must be distinguishable: %+v", doc.Work.Active[0])
	}
	if doc.Work.Active[1].Progress != "advancing" {
		t.Fatalf("fresh output must read as advancing: %+v", doc.Work.Active[1])
	}
	if doc.Work.Active[1].Agent != "claude" {
		t.Fatalf("agent identity: %+v", doc.Work.Active[1])
	}
	if doc.Waiting.Reason != "input_correction" {
		t.Fatalf("waiting reason: %+v", doc.Waiting)
	}
	if doc.LastTransition == nil || doc.LastTransition.Kind != "check" || doc.LastTransition.EventID != 4 {
		t.Fatalf("last transition must be the durable check, not the activity tick: %+v", doc.LastTransition)
	}
	if doc.Cursor.EventID != 4 || doc.Cursor.Scope != "store" || doc.Cursor.StoreID == "" || doc.Cursor.StoreEpoch != 1 {
		t.Fatalf("cursor: %+v", doc.Cursor)
	}
	if doc.Limits.BytesBudget != statusBytesBudget || doc.Limits.StringMax == 0 || doc.Limits.Omitted == nil {
		t.Fatalf("limits: %+v", doc.Limits)
	}
	if doc.Detail.Full != "looper status --contract v2 --full" || doc.Detail.Events == "" {
		t.Fatalf("detail routes: %+v", doc.Detail)
	}
	if strings.Contains(string(data), "statistics") || strings.Contains(string(data), "timing") {
		t.Fatalf("a compact read must not carry historical statistics:\n%s", data)
	}
}

// TestStatusCompactCountsCancelledWork serves the compact document for a run
// holding cancelled work (AX-019): the counts name the cancellations, the
// produced document validates against the published schema, and the plain
// rendering states the withdrawn work instead of hiding it in the todo count.
func TestStatusCompactCountsCancelledWork(t *testing.T) {
	f := newStatusFixture(t)
	s := statusState(f, nil, 0)
	s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T4", Title: "Cancelled work", Priority: 1, Status: "cancelled"})
	f.save(t, s)
	_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
	want := statusCounts{Done: 1, Todo: 1, Blocked: 1, Ready: 1, Cancelled: 1}
	if doc.Work.Counts != want {
		t.Fatalf("counts: got %+v want %+v", doc.Work.Counts, want)
	}
	var out, stderr strings.Builder
	if code := Main([]string{"status", "--contract", "v2"}, &out, &stderr); code != exitOK {
		t.Fatalf("plain status exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "1 cancelled") {
		t.Fatalf("the plain rendering must state the cancelled work:\n%s", out.String())
	}
}

// TestStatusCompactUnknownProgressWithoutOutput pins the distinction between
// unknown progress and output activity: an attempt with no worker output at
// all reports unknown, never quiet.
func TestStatusCompactUnknownProgressWithoutOutput(t *testing.T) {
	f := newStatusFixture(t)
	missing := filepath.Join(f.dir, "task-absent")
	attempts := []*Attempt{statusActiveAttempt("task-0001", "T3", "running", missing)}
	f.save(t, statusState(f, attempts, 0))
	_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
	if len(doc.Work.Active) != 1 || doc.Work.Active[0].Progress != "unknown" {
		t.Fatalf("progress: %+v", doc.Work.Active)
	}
	if doc.Work.Active[0].OutputAgeSeconds != nil {
		t.Fatalf("no output means no output age: %+v", doc.Work.Active[0])
	}
	if doc.Work.Active[0].OutputBytes != 0 {
		t.Fatalf("no output means no bytes: %+v", doc.Work.Active[0])
	}
}

// TestStatusCompactStaysWithinBudgetOnLargeBacklog exercises the byte budget
// with thousands of tasks, very long strings, and many attempts.
func TestStatusCompactStaysWithinBudgetOnLargeBacklog(t *testing.T) {
	f := newStatusFixture(t)
	long := strings.Repeat("x", 4000)
	tasks := []Task{}
	for i := 0; i < 3000; i++ {
		status := "todo"
		switch {
		case i%3 == 0:
			status = "done"
		case i%7 == 0:
			status = "blocked"
		}
		tasks = append(tasks, Task{
			ID: fmt.Sprintf("T%04d", i), Title: long, Priority: 1, Status: status,
			Details: strings.Repeat("d", 8000), Blockers: []string{long, long}, UnblockWhen: long,
		})
	}
	attempts := []*Attempt{}
	for i := 0; i < 40; i++ {
		a := statusActiveAttempt(fmt.Sprintf("task-%04d", i), fmt.Sprintf("T%04d", i), "running", filepath.Join(f.dir, "missing"))
		a.Feedback = strings.Repeat("f", 50000)
		attempts = append(attempts, a)
	}
	s := statusState(f, attempts, 3)
	s.Backlog.Tasks = tasks
	s.Backlog.Goal = strings.Repeat("g", 10000)
	f.save(t, s)
	// Two hundred events seeded through one store handle and one durable
	// commit; f.event would reopen the store per event and commit per row.
	seedEvents(t, f.openStore(t), 200, func(i int) (string, string, any) {
		return "result", fmt.Sprintf("T%04d  done | %s", i, long), map[string]string{"task_id": fmt.Sprintf("T%04d", i)}
	})

	_, doc, data := runStatusCompact(t, "--contract", "v2", "--json")
	if len(data) > statusBytesBudget {
		t.Fatalf("document is %d bytes", len(data))
	}
	var done, blocked int
	for _, task := range tasks {
		if task.Status == "done" {
			done++
		}
		if task.Status == "blocked" {
			blocked++
		}
	}
	if doc.Work.Counts.Done != done || doc.Work.Counts.Blocked != blocked {
		t.Fatalf("counts: got %+v want done=%d blocked=%d", doc.Work.Counts, done, blocked)
	}
	if doc.Work.Counts.Todo+doc.Work.Counts.Done+doc.Work.Counts.Blocked != len(tasks) {
		t.Fatalf("counts must cover the whole backlog: %+v", doc.Work.Counts)
	}
	if len(doc.Work.Active) == 0 || doc.Limits.Omitted["active_work"] != 40-len(doc.Work.Active) {
		t.Fatalf("bounded active list must report what it omitted: %+v %+v", doc.Work.Active, doc.Limits)
	}
	if len(doc.Findings) == 0 || doc.Limits.Omitted["findings"] < 100 {
		t.Fatalf("bounded findings must report what they omitted: %+v", doc.Limits)
	}
	if doc.LastTransition == nil || doc.LastTransition.EventID != 200 {
		t.Fatalf("last transition: %+v", doc.LastTransition)
	}
	if !json.Valid(data) {
		t.Fatal("the bounded document must stay valid JSON")
	}
	// Long strings never pass through unbounded.
	for _, finding := range doc.Findings {
		if len(finding.Message) > doc.Limits.StringMax+3 {
			t.Fatalf("finding message exceeds the stated string bound: %d", len(finding.Message))
		}
	}
}

// TestStatusCompactDistinguishesControllerCases covers the states the
// acceptance requires: stale, missing, idle, bootstrapping, draining,
// stopped, and quiet.
func TestStatusCompactDistinguishesControllerCases(t *testing.T) {
	session := "run-cases0001"

	t.Run("stopped without owner", func(t *testing.T) {
		f := newStatusFixture(t)
		f.save(t, statusState(f, nil, 0))
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "stopped" || doc.Controller.Owned || doc.Controller.OwnershipEvidence != "none" {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Waiting.Reason != "controller_stopped" {
			t.Fatalf("waiting: %+v", doc.Waiting)
		}
		if doc.Run.Mode != "unknown" || doc.Builds.Controller != nil {
			t.Fatalf("run and builds without a controller: %+v %+v", doc.Run, doc.Builds)
		}
		if doc.Cursor.EventID != 0 || doc.Cursor.Scope != "store" {
			t.Fatalf("a stopped run still reports its cursor: %+v", doc.Cursor)
		}
	})

	t.Run("stale controller metadata", func(t *testing.T) {
		f := newStatusFixture(t)
		f.save(t, statusState(f, nil, 0))
		// Metadata a dead controller left behind: the lock is free, so the
		// run is stopped and the record is stale evidence.
		info := &ControllerInfo{Version: ControllerVersion, Session: session, PID: 999999, Root: f.root,
			Mode: ControllerModeDetached, Started: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}
		if err := writeJSON(controllerPath(f.dir), info); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "stopped" || doc.Controller.Owned || doc.Controller.OwnershipEvidence != "metadata" {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Run.ControllerSession != session {
			t.Fatalf("the stale record still names its session: %+v", doc.Run)
		}
	})

	t.Run("stale published view", func(t *testing.T) {
		f := newStatusFixture(t)
		f.save(t, statusState(f, nil, 0))
		started := time.Now().Add(-time.Minute)
		f.ownRepository(t, session, started)
		f.publishView(t, ui.ViewSnapshot{Session: "run-earlier01"})
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "controlling" || doc.Controller.ViewMatches == nil || *doc.Controller.ViewMatches {
			t.Fatalf("a view from another session must not read as current: %+v", doc.Controller)
		}
		codes := map[string]bool{}
		for _, finding := range doc.Findings {
			codes[finding.Code] = true
		}
		if !codes["view_unavailable"] {
			t.Fatalf("the mismatched view must be reported: %+v", doc.Findings)
		}
	})

	t.Run("bootstrapping before the first backlog", func(t *testing.T) {
		f := newStatusFixture(t)
		// The store exists with no saved state; the live controller is
		// reading project requirements.
		_ = f.openStore(t)
		f.ownRepository(t, session, time.Now().Add(-40*time.Second))
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "starting" {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Waiting.Reason != "bootstrap" {
			t.Fatalf("waiting: %+v", doc.Waiting)
		}
		if doc.Run.ID != session || doc.Run.ControllerSession != session {
			t.Fatalf("run identity: %+v", doc.Run)
		}
		if doc.Work.Counts != (statusCounts{}) || len(doc.Work.Active) != 0 {
			t.Fatalf("work before a backlog: %+v", doc.Work)
		}
		if len(doc.Work.Phases) != 1 || doc.Work.Phases[0].Phase != "bootstrap" {
			t.Fatalf("controller phases: %+v", doc.Work.Phases)
		}
	})

	t.Run("bootstrapping on a detached startup seed", func(t *testing.T) {
		f := newStatusFixture(t)
		// A detached start seeds a placeholder state so the run is queryable
		// before readiness (AX-022). The seed marks the same bootstrap window
		// as no state at all: observation must not read it as an empty
		// backlog with no runnable work.
		f.ownRepository(t, session, time.Now().Add(-40*time.Second))
		f.save(t, &State{
			Version: 2, Root: f.root, Todo: f.c.Todo, Head: "head", Branch: "main",
			Ref: "refs/looper/" + session, Tip: "tip", Published: "published",
			Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}},
			Attempts: []*Attempt{}, StartupSeed: true,
		})
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "starting" {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Waiting.Reason != "bootstrap" {
			t.Fatalf("waiting: %+v", doc.Waiting)
		}
		if doc.Run.ID != session || doc.Run.ControllerSession != session {
			t.Fatalf("run identity: %+v", doc.Run)
		}
		if len(doc.Work.Phases) != 1 || doc.Work.Phases[0].Phase != "bootstrap" {
			t.Fatalf("controller phases: %+v", doc.Work.Phases)
		}
		for _, finding := range doc.Findings {
			if finding.Code == "view_unavailable" {
				t.Fatalf("a bootstrapping controller has no view yet: %+v", doc.Findings)
			}
		}
	})

	t.Run("draining on a stop request", func(t *testing.T) {
		f := newStatusFixture(t)
		work := t.TempDir()
		attemptOutput(t, work, time.Second, 128)
		attempts := []*Attempt{statusActiveAttempt("task-0001", "T3", "running", work)}
		f.save(t, statusState(f, attempts, 0))
		f.ownRepository(t, session, time.Now().Add(-time.Minute))
		f.publishView(t, ui.ViewSnapshot{Session: session})
		if err := writeStopRequest(f.dir, stopRequest{Session: session}); err != nil {
			t.Fatal(err)
		}
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "draining" {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Waiting.Reason != "draining" {
			t.Fatalf("waiting: %+v", doc.Waiting)
		}
	})

	t.Run("idle with no ready task", func(t *testing.T) {
		f := newStatusFixture(t)
		s := statusState(f, nil, 0)
		// Everything is done or blocked, so nothing is ready.
		for i := range s.Backlog.Tasks {
			s.Backlog.Tasks[i].Status = "done"
		}
		s.Backlog.Tasks[1].Status = "blocked"
		f.save(t, s)
		f.ownRepository(t, session, time.Now().Add(-time.Minute))
		f.publishView(t, ui.ViewSnapshot{Session: session})
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if doc.Controller.State != "controlling" || doc.Waiting.Reason != "no_ready_task" {
			t.Fatalf("controller/waiting: %+v %+v", doc.Controller, doc.Waiting)
		}
		if len(doc.Work.Active) != 0 {
			t.Fatalf("idle work: %+v", doc.Work.Active)
		}
	})

	t.Run("quiet worker", func(t *testing.T) {
		f := newStatusFixture(t)
		work := t.TempDir()
		attemptOutput(t, work, 12*time.Minute, 2048)
		attempts := []*Attempt{statusActiveAttempt("task-0001", "T3", "running", work)}
		f.save(t, statusState(f, attempts, 0))
		f.ownRepository(t, session, time.Now().Add(-time.Minute))
		f.publishView(t, ui.ViewSnapshot{Session: session})
		_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
		if len(doc.Work.Active) != 1 || doc.Work.Active[0].Progress != "quiet" {
			t.Fatalf("quiet work: %+v", doc.Work.Active)
		}
		found := false
		for _, finding := range doc.Findings {
			if finding.Code == "output_quiet" && finding.TaskID == "T3" {
				found = true
			}
		}
		if !found {
			t.Fatalf("quiet output must be a finding: %+v", doc.Findings)
		}
	})

	t.Run("missing run", func(t *testing.T) {
		newStatusFixture(t)
		var out, stderr strings.Builder
		code := Main([]string{"status", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("exit %d, want %d", code, exitUnavailable)
		}
		var failure errorDocument
		if err := json.Unmarshal([]byte(out.String()), &failure); err != nil {
			t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
		}
		if failure.Error.Code != "run_not_found" {
			t.Fatalf("code %q", failure.Error.Code)
		}
	})
}

// TestStatusCompactReportsHeldBacklogAndRequests covers the scheduling
// obstructions the contract names: an external task-file edit and pending
// request envelopes.
func TestStatusCompactReportsHeldBacklogAndRequests(t *testing.T) {
	f := newStatusFixture(t)
	s := statusState(f, nil, 0)
	f.save(t, s)
	// The saved export digest no longer matches the file on disk.
	if err := os.WriteFile(f.c.Todo, []byte("{\"schema_version\":1,\"source_files\":[],\"tasks\":[]}"), 0600); err != nil {
		t.Fatal(err)
	}
	f.ownRepository(t, "run-hold00001", time.Now().Add(-time.Minute))
	f.publishView(t, ui.ViewSnapshot{Session: "run-hold00001"})
	if err := os.MkdirAll(requestDir(f.dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(requestDir(f.dir), "req-1.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
	if doc.Waiting.Reason != "backlog_hold" {
		t.Fatalf("waiting: %+v", doc.Waiting)
	}
	if doc.Requests.Pending != 1 {
		t.Fatalf("requests: %+v", doc.Requests)
	}
}

// TestStatusFullRemainsExplicitlyAvailable pins the route to the complete
// state: --full under version 2 serves the complete document, identical to
// the legacy document the default contract keeps.
func TestStatusFullRemainsExplicitlyAvailable(t *testing.T) {
	f := newStatusFixture(t)
	work := t.TempDir()
	attemptOutput(t, work, time.Second, 256)
	attempts := []*Attempt{statusActiveAttempt("task-0001", "T3", "running", work)}
	f.save(t, statusState(f, attempts, 2))
	f.event(t, "check", "Checking T3 with integrated work...", nil)

	var full, legacy strings.Builder
	if code := Main([]string{"status", "--contract", "v2", "--full", "--json"}, &full, &legacy); code != exitOK {
		t.Fatalf("exit %d: %s", code, legacy.String())
	}
	if code := Main([]string{"status", "--json"}, &legacy, &full); code != exitOK {
		t.Fatalf("exit %d: %s", code, full.String())
	}
	// The two documents carry the same complete state; only the observation
	// timestamp differs between the two invocations.
	stripVolatile := func(s string) string {
		var doc map[string]any
		if err := json.Unmarshal([]byte(s), &doc); err != nil {
			t.Fatalf("the complete document did not parse: %v", err)
		}
		delete(doc, "observation")
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if stripVolatile(full.String()) != stripVolatile(legacy.String()) {
		t.Fatalf("the complete document must stay the same document")
	}
	var report struct {
		Version  int `json:"version"`
		Attempts []struct {
			ID string `json:"id"`
		} `json:"attempts"`
		Backlog struct {
			Tasks []Task `json:"tasks"`
		} `json:"backlog"`
		Timing     map[string]any `json:"timing"`
		Statistics map[string]any `json:"statistics"`
	}
	if err := json.Unmarshal([]byte(full.String()), &report); err != nil {
		t.Fatalf("the complete document did not parse: %v", err)
	}
	if report.Version != 2 || len(report.Backlog.Tasks) != 3 || len(report.Attempts) != 1 || report.Timing == nil || report.Statistics == nil {
		t.Fatalf("the complete document must carry the whole state: %+v", report)
	}
	if strings.Contains(full.String(), "contract_version") {
		t.Fatalf("the complete document is the complete state, not a second contract:\n%s", full.String())
	}
}

// TestStatusPlainPipedShowsLiveness drives the plain piped status and checks
// the three facts a caller without a dashboard needs: controller state,
// in-flight work, and the reason for waiting.
func TestStatusPlainPipedShowsLiveness(t *testing.T) {
	f := newStatusFixture(t)
	work := t.TempDir()
	attemptOutput(t, work, 3*time.Second, 512)
	quiet := t.TempDir()
	attemptCheckOutput(t, quiet, 9*time.Minute, 512)
	attempts := []*Attempt{
		statusActiveAttempt("task-0001", "T3", "running", work),
		statusActiveAttempt("task-0002", "T4", "verifying", quiet),
	}
	s := statusState(f, attempts, 0)
	s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T4", Title: "Checking work", Priority: 1, Status: "doing"})
	f.save(t, s)
	f.event(t, "check", "Checking T4 with integrated work...", nil)
	f.ownRepository(t, "run-plain0001", time.Now().Add(-5*time.Minute))
	f.publishView(t, ui.ViewSnapshot{Session: "run-plain0001", State: "reviewing", ActivityDetail: "Waiting for review agent"})

	var out, stderr strings.Builder
	if code := Main([]string{"status"}, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	text := out.String()
	for _, want := range []string{
		"Controller: controlling",
		"Active:",
		"T3 running",
		"progress advancing",
		"progress quiet",
		"Controller phase: reviewing",
		"Waiting:",
		"Cursor:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain piped status is missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Timing:") {
		t.Fatalf("a piped status must not build historical statistics:\n%s", text)
	}
}

// TestStatusPlainPipedStaysBoundedWithLargeBacklog checks that piping plain
// status does not reintroduce an unbounded listing.
func TestStatusPlainPipedStaysBoundedWithLargeBacklog(t *testing.T) {
	f := newStatusFixture(t)
	tasks := []Task{}
	for i := 0; i < 2000; i++ {
		tasks = append(tasks, Task{ID: fmt.Sprintf("T%04d", i), Title: strings.Repeat("t", 300), Priority: 1, Status: "blocked", Blockers: []string{strings.Repeat("b", 300)}})
	}
	s := statusState(f, nil, 0)
	s.Backlog.Tasks = tasks
	f.save(t, s)
	var out, stderr strings.Builder
	if code := Main([]string{"status"}, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := len(out.String()); got > statusBytesBudget {
		t.Fatalf("plain piped status is %d bytes, over the %d byte budget", got, statusBytesBudget)
	}
	if !strings.Contains(out.String(), "2000") {
		t.Fatalf("the blocked count must still appear:\n%s", out.String())
	}
}

// TestStatusCompactNeedsNoProcessHistory proves a compact read never builds
// the timing or usage statistics: a store whose process history tables are
// gone still serves the compact document, while the complete document needs
// them and reports exactly that.
func TestStatusCompactNeedsNoProcessHistory(t *testing.T) {
	f := newStatusFixture(t)
	work := t.TempDir()
	attemptOutput(t, work, time.Second, 128)
	f.save(t, statusState(f, []*Attempt{statusActiveAttempt("task-0001", "T3", "running", work)}, 0))
	f.event(t, "check", "Checking T3...", nil)
	store := f.openStore(t)
	// The complete document reads the process history; a history whose rows
	// cannot be scanned fails it. The compact document must not notice.
	for _, broken := range []string{"processes", "sessions", "usage_reports"} {
		if _, err := store.db.Exec("DROP TABLE IF EXISTS " + broken); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec("CREATE VIEW " + broken + " AS SELECT 1"); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()
	runStatusCompact(t, "--contract", "v2", "--json")
	var out, stderr strings.Builder
	code := Main([]string{"status", "--contract", "v2", "--full", "--json"}, &out, &stderr)
	if code != exitUnavailable {
		t.Fatalf("the complete document needs the process history: exit %d, want %d\n%s", code, exitUnavailable, out.String())
	}
	var failure errorDocument
	if err := json.Unmarshal([]byte(out.String()), &failure); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if failure.Error.Code != "state_unreadable" {
		t.Fatalf("code %q", failure.Error.Code)
	}
}

// TestStatusFullFlagScope pins which commands accept --full and which query
// flags stay with the event and inspection commands.
func TestStatusFullFlagScope(t *testing.T) {
	if _, err := Parse([]string{"status", "--full", "--json"}); err != nil {
		t.Fatalf("status must accept --full: %v", err)
	}
	if _, err := Parse([]string{"inspect", "T1", "--full"}); err != nil {
		t.Fatalf("inspect must accept --full since AX-005: %v", err)
	}
	if _, err := Parse([]string{"status", "--task", "T1"}); err == nil {
		t.Fatal("status must refuse --task")
	}
	if _, err := Parse([]string{"status", "--limit", "10"}); err == nil {
		t.Fatal("status must refuse --limit")
	}
	if _, err := Parse([]string{"tail", "--full"}); err == nil {
		t.Fatal("tail must keep refusing --full")
	}
}

// newStatusEscapeFixture saves one run whose every recorded string the compact
// document renders is escape-heavy, with the given number of active attempts,
// a controller record, a published view, and a journaled transition: every
// channel that can carry content into the document carries text far over
// every bound the ladder states.
func newStatusEscapeFixture(t *testing.T, workers int) *statusFixture {
	t.Helper()
	f := newStatusFixture(t)
	esc := preflightEscapeHeavy
	tasks := []Task{{ID: "T9", Title: "unrelated work", Priority: 3, Status: "todo"}}
	attempts := []*Attempt{}
	for i := 0; i < workers; i++ {
		task := Task{ID: fmt.Sprintf("%04d", i) + esc(30), Title: esc(60), Priority: 1, Status: "doing"}
		attempts = append(attempts, &Attempt{
			ID: esc(20) + fmt.Sprintf("-%04d", i), Task: task, Agent: esc(20), Base: esc(20),
			Directory: filepath.Join(f.dir, "taskwork", fmt.Sprintf("t%04d", i)+esc(10)),
			Ref:       "ref", Phase: "running", Worktrees: []string{},
		})
		tasks = append(tasks, task)
	}
	f.save(t, &State{
		Version: 2, Root: esc(30), Todo: esc(30), Head: esc(20), Branch: esc(10),
		Ref: "refs/looper/" + esc(10), Tip: esc(20), Published: esc(20), ExportHash: esc(30),
		Backlog:  Backlog{Version: 1, Sources: []string{}, Tasks: tasks},
		Attempts: attempts,
	})
	seedControllerRecord(t, f.dir, &ControllerInfo{
		Version: ControllerVersion, Session: esc(30), PID: os.Getpid(), Root: esc(30),
		Mode: ControllerModeDetached, Started: esc(40),
		Build: &BuildInfo{Release: esc(40), Revision: esc(40), Modified: true},
	})
	f.publishView(t, ui.ViewSnapshot{Session: esc(30), Mode: ControllerModeDetached,
		State: esc(10), ActivityDetail: esc(200)})
	f.event(t, esc(10), esc(200), map[string]any{"task_id": esc(60)})
	return f
}

// assertStatusStringsBounded checks the fit's structural discipline: every
// content-derived string obeys the bound the document states, measured in the
// bytes the JSON encoder writes. Route commands obey the route bound that
// gated their identifier instead, because a truncated command would name a
// task that does not exist.
func assertStatusStringsBounded(t *testing.T, doc *statusCompactDocument, max int) {
	t.Helper()
	bounded := func(label, s string) {
		t.Helper()
		if jsonEscapedLen(s) > max {
			t.Errorf("%s exceeds the stated %d byte bound: %d emitted", label, max, jsonEscapedLen(s))
		}
	}
	bounded("project root", doc.Project.Root)
	bounded("project name", doc.Project.Name)
	bounded("project todo path", doc.Project.TodoPath)
	bounded("run id", doc.Run.ID)
	bounded("controller session", doc.Run.ControllerSession)
	bounded("run started at", doc.Run.StartedAt)
	bounded("client build release", doc.Builds.Client.Release)
	bounded("client build revision", doc.Builds.Client.Revision)
	if doc.Builds.Controller != nil {
		bounded("controller build release", doc.Builds.Controller.Release)
		bounded("controller build revision", doc.Builds.Controller.Revision)
	}
	for i, a := range doc.Work.Active {
		bounded(fmt.Sprintf("active %d attempt id", i), a.AttemptID)
		bounded(fmt.Sprintf("active %d task id", i), a.TaskID)
		bounded(fmt.Sprintf("active %d phase", i), a.Phase)
		bounded(fmt.Sprintf("active %d agent", i), a.Agent)
	}
	for i, p := range doc.Work.Phases {
		bounded(fmt.Sprintf("phase %d name", i), p.Phase)
		bounded(fmt.Sprintf("phase %d detail", i), p.Detail)
	}
	bounded("waiting detail", doc.Waiting.Detail)
	for i, f := range doc.Findings {
		bounded(fmt.Sprintf("finding %d code", i), f.Code)
		bounded(fmt.Sprintf("finding %d task id", i), f.TaskID)
		bounded(fmt.Sprintf("finding %d message", i), f.Message)
		bounded(fmt.Sprintf("finding %d action", i), f.Action)
		if f.Evidence != nil {
			bounded(fmt.Sprintf("finding %d reference", i), f.Evidence.Reference)
		}
	}
	if doc.LastTransition != nil {
		bounded("transition kind", doc.LastTransition.Kind)
		bounded("transition time", doc.LastTransition.Time)
		bounded("transition task id", doc.LastTransition.TaskID)
		bounded("transition message", doc.LastTransition.Message)
	}
	bounded("cursor store id", doc.Cursor.StoreID)
}

// TestStatusCompactBoundsEscapeExpandedContent pins the published budget
// against saved records the JSON encoder expands sixfold: the same run shape
// the ordinary fixtures use, with every recorded string escape-heavy, must
// still answer one schema-valid document at or under the budget its own
// limits report.
func TestStatusCompactBoundsEscapeExpandedContent(t *testing.T) {
	newStatusEscapeFixture(t, 12)
	_, doc, data := runStatusCompact(t, "--contract", "v2", "--json")
	if doc.Limits.BytesBudget < len(data) {
		t.Fatalf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(data))
	}
	assertStatusStringsBounded(t, doc, doc.Limits.StringMax)
	// The document stays useful, not merely small: identity, counts, and the
	// waiting reason survive every fit the ladder can choose.
	if doc.Project.Root == "" || doc.Run.ID == "" || doc.Waiting.Reason == "" {
		t.Fatalf("the bounded document lost its identity or waiting reason: %+v", doc)
	}
	if doc.Work.Counts.Todo == 0 || doc.Work.Counts.InFlight == 0 {
		t.Fatalf("the bounded document lost its counts: %+v", doc.Work.Counts)
	}
	if len(doc.Work.Active) == 0 && doc.Limits.ActiveMax > 0 {
		t.Fatalf("a fit that keeps active work must show it: %+v", doc.Limits)
	}
	// The plain rendering reads the same structure and stays bounded too.
	var plain, plainErr strings.Builder
	if code := Main([]string{"status", "--contract", "v2"}, &plain, &plainErr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, plain.String(), plainErr.String())
	}
	if text := plain.String(); !strings.Contains(text, "Controller:") || len(text) > 4*statusBytesBudget {
		t.Fatalf("plain rendering of the bounded document is not readable or bounded: %d bytes", len(text))
	}
}

// TestStatusCompactTerminalFitBoundsAnyContent pins the structural guarantee
// behind the budget: the ladder's terminal fit bounds every string to one
// rune at worst and drops both collections, so the document it renders is
// identity, counts, and cursor alone. Escape-heavy saved records — every
// field populated, every worker present — must render under the budget at
// that fit, and so must any content, because no recorded byte can reach past
// it. Every earlier fit obeys the string bound it states in emitted bytes.
func TestStatusCompactTerminalFitBoundsAnyContent(t *testing.T) {
	for _, workers := range []int{2, 40, 200} {
		f := newStatusEscapeFixture(t, workers)
		store := f.openStore(t)
		snap, err := store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		ev, err := collectStatusEvidence(f.c, store, snap, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, fit := range statusFits {
			assertStatusStringsBounded(t, renderStatusDocument(ev, fit), fit.stringMax)
		}
		terminal := renderStatusDocument(ev, statusTerminalFit)
		if size := statusEncoded(terminal); size > statusBytesBudget {
			t.Fatalf("%d workers: the terminal fit renders %d bytes, over the %d byte budget",
				workers, size, statusBytesBudget)
		}
		remarshaled, err := json.Marshal(terminal)
		if err != nil {
			t.Fatal(err)
		}
		if err = contractschema.ValidateJSON(statusCompactSchema(t), remarshaled); err != nil {
			t.Fatalf("%d workers: the terminal document violates its schema: %v", workers, err)
		}
		// The ladder as a whole never returns a larger document than its
		// terminal fit can justify: whatever the earlier fits measured, the
		// chosen document obeys the budget it reports.
		chosen, err := compactStatus(f.c, store, snap)
		if err != nil {
			t.Fatal(err)
		}
		if size := statusEncoded(chosen); size > statusBytesBudget {
			t.Fatalf("%d workers: the chosen document exceeds the budget it reports: %d bytes", workers, size)
		}
	}
}
