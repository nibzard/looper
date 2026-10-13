package looper

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

func TestSnapshotAllTasksAndRecoveryAreIndependent(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks[2].UnblockWhen = "Configure credentials"
	e.State.Backlog.Tasks[3].DependsOn = []string{"T3"}
	v := e.snapshot(false)
	if len(v.Tasks) != 4 || v.Tasks[0].Status != "done" || v.Tasks[2].UnblockWhen != "Configure credentials" || len(v.Tasks[3].WaitingOn) != 1 {
		t.Fatalf("missing inspector data: %+v", v.Tasks)
	}
	e.State.Backlog.Tasks[2].Blockers[0] = "changed"
	e.State.Backlog.Tasks[3].DependsOn[0] = "T1"
	e.State.Attempts[0].Repairs = 12
	if v.Tasks[2].Blockers[0] != "no credentials" || v.Blocked[0].Reasons[0] != "no credentials" || v.Tasks[3].DependsOn[0] != "T3" || v.Tasks[1].Attempt.Repairs != 0 {
		t.Fatal("presentation snapshot aliases owner state")
	}
}

func TestObserverViewReportsBootstrapBeforeFirstSnapshot(t *testing.T) {
	dir := t.TempDir()
	info := ControllerInfo{Version: ControllerVersion, Session: "run-live", PID: os.Getpid(), Mode: ControllerModeForeground, BootstrapAgent: "claude"}
	if err := writeJSON(controllerPath(dir), &info); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	o := &observer{store: store}
	if v := o.view(dir, "run-live"); v == nil || !v.Bootstrapping || v.ActivityLog != "" || v.ActivityAgent != "claude" {
		t.Fatalf("streamless bootstrap must still report the phase: %+v", v)
	}
	stream := filepath.Join(dir, "bootstrap-1", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(stream), 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("{\"type\":\"assistant\"}\n")
	if err := os.WriteFile(stream, content, 0600); err != nil {
		t.Fatal(err)
	}
	v := o.view(dir, "run-live")
	if v == nil || !v.Bootstrapping || v.Session != "run-live" {
		t.Fatalf("missing bootstrap snapshot: %+v", v)
	}
	if v.ActivityLog != stream || v.BootstrapBytes != int64(len(content)) || v.BootstrapIdleSeconds < 0 {
		t.Fatalf("missing stream evidence: %+v", v)
	}
	// A stale view from an earlier session must not mask the live bootstrap.
	stale := &ui.ViewSnapshot{Version: ui.ViewVersion, Session: "run-old"}
	if err := writeJSON(viewPath(dir), stale); err != nil {
		t.Fatal(err)
	}
	if v = o.view(dir, "run-live"); v == nil || !v.Bootstrapping {
		t.Fatalf("stale view hid the bootstrap: %+v", v)
	}
	// The current view still wins once the controller publishes it.
	current := &ui.ViewSnapshot{Version: ui.ViewVersion, Session: "run-live", State: "planning"}
	if err := writeJSON(viewPath(dir), current); err != nil {
		t.Fatal(err)
	}
	if v = o.view(dir, "run-live"); v == nil || v.Bootstrapping || v.State != "planning" {
		t.Fatalf("published view not served: %+v", v)
	}
	dead := exec.Command("true")
	if err := dead.Start(); err != nil {
		t.Fatal(err)
	}
	gone := dead.Process.Pid
	if err := dead.Wait(); err != nil {
		t.Fatal(err)
	}
	info.PID = gone
	if err := writeJSON(controllerPath(dir), &info); err != nil {
		t.Fatal(err)
	}
	if v = o.view(dir, "run-live"); v != nil {
		t.Fatalf("dead owner produced a snapshot: %+v", v)
	}
}

func TestObserverIntegrationPaceUsesCompleteJournal(t *testing.T) {
	e := protocolEngine(t)
	now := time.Now().UTC()
	insert := func(at time.Time, kind, msg string) {
		t.Helper()
		_, err := e.Store.db.Exec("INSERT INTO events(time,kind,message,data) VALUES(?,?,?,'null')", at.Format(time.RFC3339Nano), kind, msg)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert(now.Add(-30*time.Hour), "result", "T001  done | old integration")
	insert(now.Add(-2*time.Hour), "result", "T002  done | earlier integration")
	insert(now.Add(-10*time.Minute), "result", "T003  done | recent integration")
	insert(now.Add(-5*time.Minute), "result", "T004  blocked | a summary containing  done | must not count")
	for i := 0; i < 600; i++ {
		insert(now, "worker_activity", "Agent returned a result")
	}
	o := &observer{store: e.Store}
	p, err := o.integrationPace(now.Add(-3*time.Hour).Format(time.RFC3339), now)
	if err != nil || p.Session != 2 || p.Hour != 1 || p.Day != 2 || p.LastIntegration != now.Add(-10*time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("incorrect publication pace: %+v %v", p, err)
	}
	outcomes, err := o.recentOutcomes(now.Add(-3 * time.Hour).Format(time.RFC3339))
	if err != nil || len(outcomes) != 3 || !strings.Contains(outcomes[0].Message, "T002") {
		t.Fatalf("busy workers hid recorded outcomes: %+v %v", outcomes, err)
	}
}

func TestObserverInspectorIncludesChecksAndRetainedWork(t *testing.T) {
	e := snapshotEngine(t)
	a := e.State.Attempts[1]
	a.Summary = &Summary{Summary: "Module layout verified"}
	a.Worktree = "retained/worktree"
	e.State.Backlog.Tasks[0].Files = []string{"main.go"}
	path := filepath.Join(e.Dir, "checks", "revision", "verify.log")
	if err := writeJSON(filepath.Join(filepath.Dir(path), "verify-command.json"), map[string]string{"command": "go test ./..."}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.Check(a.ID, "revision", "executable", "pass", path); err != nil {
		t.Fatal(err)
	}
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	o := &observer{store: e.Store}
	d, err := o.inspect("T1")
	if err != nil || d.Task.Status != "done" || d.Summary != "Module layout verified" || len(d.Checks) != 1 || d.VerifyCommand != "go test ./..." || d.Task.Attempt.Worktree != "retained/worktree" || d.Files[0] != "main.go" {
		t.Fatalf("inspector lost evidence: %+v %v", d, err)
	}
	if _, err = o.inspect("missing"); err == nil {
		t.Fatal("unknown task accepted")
	}
}

func TestObserverViewRejectsDifferentOrStoppedController(t *testing.T) {
	e := snapshotEngine(t)
	e.publishView(false)
	if err := writeJSON(controllerPath(e.Dir), e.controller); err != nil {
		t.Fatal(err)
	}
	o := &observer{store: e.Store}
	if v := o.view(e.Dir, e.controller.Session); v == nil || len(v.Tasks) != 4 {
		t.Fatal("matching view unavailable")
	}
	if v := o.view(e.Dir, "other"); v != nil {
		t.Fatal("observer switched controller sessions")
	}
	os.Remove(controllerPath(e.Dir))
	if v := o.view(e.Dir, e.controller.Session); v != nil {
		t.Fatal("stopped controller still shown as live")
	}
}

func TestDashboardEvidenceDoesNotInterpretQuietAsFailure(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "work-00", "events-00.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("output"), 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	os.Chtimes(path, at, at)
	a := ui.AttemptView{ID: "attempt", LogDir: root, Phase: "running"}
	enrichAttempt(&a)
	if a.LastOutput == "" || a.ProcessState != "" {
		t.Fatalf("invented health from quiet output: %+v", a)
	}
}

func requeueEngine(t *testing.T) *Engine {
	e := protocolEngine(t)
	e.Config.Todo = filepath.Join(e.Dir, "to-do.json")
	e.State = &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T1", Title: "Check Redis", Status: "blocked", Priority: 1, Blockers: []string{"Redis missing"}, UnblockWhen: "Install Redis"}}}, Attempts: []*Attempt{{ID: "a1", Task: Task{ID: "T1", Title: "Check Redis", Priority: 1}, Phase: "blocked", Worktree: "retained"}}}
	if err := writeJSON(e.Config.Todo, e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(e.Config.Todo)
	e.State.ExportHash = digest(data)
	return e
}

func requeueRequest(e *Engine) *SettingsRequest {
	return &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("requeue"), Type: RequestTypeRequeue, Session: e.controller.Session, TaskID: "T1", TaskRevision: hashJSON(e.State.Backlog.Tasks[0])}
}

func TestRequeuePreservesContractAndRetainedAttempt(t *testing.T) {
	e := requeueEngine(t)
	req := requeueRequest(e)
	contract := e.State.Backlog.Tasks[0].Contract()
	ack := e.applyRequeueRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("requeue: %+v", ack)
	}
	task := e.State.Backlog.Tasks[0]
	if task.Status != "todo" || task.Contract() != contract || len(task.Blockers) != 0 || e.State.Attempts[0].Worktree != "retained" || e.State.Attempts[0].Phase != "blocked" {
		t.Fatal("requeue changed contract or discarded work")
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || exported.Tasks[0].Status != "todo" {
		t.Fatal("requeue not exported")
	}
	saved, err := e.Store.Load()
	if err != nil || saved.Backlog.Tasks[0].Status != "todo" {
		t.Fatal("requeue not persisted")
	}
	if repeated := e.applyRequeueRequest(req); repeated.Status != AckApplied {
		t.Fatal("duplicate request did not reuse acknowledgement")
	}
}

func TestRequeueRejectsConflictsWithoutMutatingState(t *testing.T) {
	for _, condition := range []string{"session", "revision", "active", "external-edit"} {
		t.Run(condition, func(t *testing.T) {
			e := requeueEngine(t)
			req := requeueRequest(e)
			switch condition {
			case "session":
				req.Session = "old"
			case "revision":
				req.TaskRevision = "old"
			case "active":
				e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "busy", Phase: "running"})
			case "external-edit":
				os.WriteFile(e.Config.Todo, []byte("external edit"), 0600)
			}
			ack := e.applyRequeueRequest(req)
			if ack.Status == AckApplied || e.State.Backlog.Tasks[0].Status != "blocked" || e.err != nil {
				t.Fatalf("conflict mutated execution state: %+v", ack)
			}
		})
	}
}

func TestRequeueUsesRequestTransport(t *testing.T) {
	e := requeueEngine(t)
	req := requeueRequest(e)
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processRequeueRequests()
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil || ack.Status != AckApplied {
		t.Fatalf("missing acknowledgement: %+v %v", ack, err)
	}
	events, _ := e.Store.RecentEvents(-1, 20)
	if len(events) != 1 || !strings.Contains(events[0].Message, "T1 requeued") {
		t.Fatal("requeue outcome missing from journal")
	}
}

func TestDashboardEvidenceFollowsCurrentPhase(t *testing.T) {
	root := t.TempDir()
	worker := filepath.Join(root, "work-00", "events-00.jsonl")
	check := filepath.Join(root, "checks", "revision", "verify.log")
	for _, p := range []string{worker, check} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("output"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	earlier := time.Now().Add(-time.Minute)
	os.Chtimes(worker, earlier, earlier)
	a := &Attempt{ID: "a", Directory: root, Phase: "running"}
	if w := observeWorker(a); w.Log != worker {
		t.Fatal("repair shows a prior check as worker output")
	}
	a.Phase = "verifying"
	if w := observeWorker(a); w.Log != check {
		t.Fatal("verification shows worker output")
	}
	os.Remove(check)
	if w := observeWorker(a); w.LastOutput != "" {
		t.Fatal("verification invents output before a check starts")
	}
}

func TestDashboardControllerAlive(t *testing.T) {
	if !dashboardControllerAlive(&ControllerInfo{PID: os.Getpid()}) {
		t.Fatal("current process is not alive")
	}
	for _, info := range []*ControllerInfo{nil, {PID: 0}, {PID: 1}} {
		if dashboardControllerAlive(info) {
			t.Fatal("invalid controller is alive")
		}
	}
}
