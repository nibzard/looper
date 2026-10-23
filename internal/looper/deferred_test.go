package looper

import (
	"os"
	"strings"
	"testing"
)

func TestDeferredFindingPreservesRequiredWork(t *testing.T) {
	e := snapshotEngine(t)
	e.addFinding(Finding{Title: "Native checks", Details: "Pass native tests", Evidence: "Linux host", Owner: "agent", Key: "native", DeferUntil: "Native round and macOS host available"})
	task := &e.State.Backlog.Tasks[len(e.State.Backlog.Tasks)-1]
	if !task.Deferred() || task.Status != "blocked" || task.OperatorOnly() || task.UnblockWhen == "" {
		t.Fatalf("deferred task: %+v", task)
	}
	id := task.ID
	task.Status = "todo" // A generic retry must not override the deferral.
	for _, ready := range e.State.Backlog.Ready() {
		if ready.ID == id {
			t.Fatal("deferred task dispatched")
		}
	}
	if !taskView(*task, e.State.Backlog).Deferred {
		t.Fatal("deferral absent from view")
	}
	task.Status = "blocked"
	e.addFinding(Finding{Title: "Native checks", Details: "Still required", Evidence: "Same host", Owner: "agent", Key: "native"})
	if !e.State.Backlog.Task(id).Deferred() || e.State.Backlog.Task(id).Status != "blocked" {
		t.Fatal("review reopened deferred task")
	}
	task = e.State.Backlog.Task(id)
	task.Tags = nil
	task.Status = "todo"
	found := false
	for _, ready := range e.State.Backlog.Ready() {
		if ready.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("explicit resume failed")
	}
}

func TestDeferredTaskDiagnosis(t *testing.T) {
	e := snapshotEngine(t)
	deferTask(&e.State.Backlog.Tasks[2], "macOS host available")
	findings := diagnose(Observation{}, e.State)
	for _, f := range findings {
		if f.TaskID == "T3" {
			if f.Code != "task_deferred" || f.Severity != "info" || f.Action == "" {
				t.Fatalf("diagnosis: %+v", f)
			}
			return
		}
	}
	t.Fatal("missing deferral diagnosis")
}

func TestDeferredRequeueKeepsResumeCondition(t *testing.T) {
	e := requeueEngine(t)
	task := &e.State.Backlog.Tasks[0]
	deferTask(task, "native host available")
	if err := writeJSON(e.Config.Todo, e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		t.Fatal(err)
	}
	e.State.ExportHash = digest(data)
	before := hashJSON(e.State.Backlog)
	ack := e.applyRequeueRequest(requeueRequest(e))
	if ack.Status != AckConflict || !strings.Contains(ack.Message, "deferred") || hashJSON(e.State.Backlog) != before {
		t.Fatalf("requeue lost deferral: %+v", ack)
	}
}

func TestImportedDeferralRetainsStatusAndCondition(t *testing.T) {
	e := snapshotEngine(t)
	ids, _, err := e.applyProposal(&importProposal{Additions: []Task{{ID: "P1", Title: "Native validation", Priority: 3, Status: "blocked", Details: "Pass native tests", Tags: []string{deferredTaskTag}, UnblockWhen: "Native host available"}}})
	if err != nil {
		t.Fatal(err)
	}
	task := e.State.Backlog.Task(ids[0])
	if !task.Deferred() || task.Status != "blocked" || task.UnblockWhen != "Native host available" {
		t.Fatalf("lost import deferral: %+v", task)
	}
}
