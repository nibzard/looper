package looper

import (
	"encoding/json"
	"strings"
	"testing"
)

func reportedFinding(t *testing.T, body string) Finding {
	t.Helper()
	var f Finding
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// applyFinding drives one finding through the staged protocol exactly as a
// committed result or review does: mutate first, report afterwards.
func applyFinding(t *testing.T, e *Engine, f Finding) {
	t.Helper()
	s := e.beginFindingStage()
	s.add(f)
	s.emit()
}

func TestFindingKeyDeduplicatesRenamedBlocker(t *testing.T) {
	e := snapshotEngine(t)
	before := len(e.State.Backlog.Tasks)
	applyFinding(t, e, reportedFinding(t, `{"title":"Obtain benchmark access","details":"Operator supplies access","evidence":"401","owner":"operator","key":"benchmark-access","priority":1}`))
	applyFinding(t, e, reportedFinding(t, `{"title":"Unblock dedicated pair provisioning","details":"Same access is missing","evidence":"second 401","owner":"operator","key":"benchmark-access","priority":1}`))
	if len(e.State.Backlog.Tasks) != before+1 {
		t.Fatal("renamed blocker created another task")
	}
	task := e.State.Backlog.Tasks[before]
	assertFindingEvidence(t, e, task.ID, "second 401")
	if task.Status != "blocked" {
		t.Fatal("operator prerequisite is executable")
	}
}

func TestFindingReferencesExistingTask(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = []Task{{ID: "T001", Title: "Await access", Status: "blocked", Priority: 1}}
	applyFinding(t, e, reportedFinding(t, `{"title":"Access still unavailable","details":"Same prerequisite","evidence":"401 again","owner":"operator","key":"access","existing_task_id":"T001","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 {
		t.Fatal("existing task was not updated")
	}
	assertFindingEvidence(t, e, "T001", "401 again")
	// The stable key must survive a save/reload and another renamed finding.
	data, _ := json.Marshal(e.State.Backlog)
	if err := json.Unmarshal(data, &e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	applyFinding(t, e, reportedFinding(t, `{"title":"Retry access","details":"Still blocked","evidence":"401 third time","owner":"operator","key":"access","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 {
		t.Fatal("saved identity was lost")
	}
}

func TestOperatorFindingNeverEntersReadyQueue(t *testing.T) {
	for _, body := range []string{
		`{"title":"Grant access","details":"Supply the token","evidence":"401","owner":"operator","priority":1}`,
		`{"title":"Operator issues the API token","details":"Supply the token","evidence":"401","priority":1}`,
		`{"title":"Push provisioning patch","details":"An operator must git push the branch","evidence":"local commit","priority":1}`,
	} {
		e := snapshotEngine(t)
		e.State.Backlog.Tasks = nil
		applyFinding(t, e, reportedFinding(t, body))
		if len(e.State.Backlog.Tasks) != 1 {
			t.Fatal("operator work was lost")
		}
		task := &e.State.Backlog.Tasks[0]
		if task.Status != "blocked" || len(task.Blockers) == 0 || task.UnblockWhen == "" {
			t.Fatal("missing operator blocker")
		}
		task.Status = "todo" // A generic retry must not dispatch operator work.
		if len(e.State.Backlog.Ready()) != 0 {
			t.Fatal("operator task entered worker queue")
		}
	}
}

func TestAgentFindingDoesNotReopenCompletedOperatorTask(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = nil
	applyFinding(t, e, reportedFinding(t, `{"title":"Authorize host","details":"Operator grants access","evidence":"401","owner":"operator","key":"access","priority":1}`))
	e.State.Backlog.Tasks[0].Status = "done"
	applyFinding(t, e, reportedFinding(t, `{"title":"Authorize the host again","details":"Old report","evidence":"old 401","owner":"agent","key":"access","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 || e.State.Backlog.Tasks[0].Status != "done" {
		t.Fatal("duplicate reopened completed work")
	}
}

func TestFindingRejectsInvalidOwnershipAndReference(t *testing.T) {
	for _, extra := range []string{`"owner":"robot"`, `"existing_task_id":"missing"`, `"key":" "`} {
		e := snapshotEngine(t)
		before := len(e.State.Backlog.Tasks)
		applyFinding(t, e, reportedFinding(t, `{"title":"Fix code","details":"Acceptance","evidence":"Failure","priority":1,`+extra+`}`))
		if len(e.State.Backlog.Tasks) != before {
			t.Fatal("invalid finding created executable work")
		}
	}
}

func TestDistinctFindingsRemainRunnable(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = nil
	for _, key := range []string{"input-validation", "output-validation"} {
		applyFinding(t, e, reportedFinding(t, `{"title":"Fix `+key+`","details":"Document operator workflow","evidence":"Test fails","owner":"agent","key":"`+key+`","priority":1}`))
	}
	if len(e.State.Backlog.Ready()) != 2 {
		t.Fatal("distinct code fixes were merged or blocked")
	}
}

func TestLegacyOperatorTaskNeverEntersReadyQueue(t *testing.T) {
	b := Backlog{Tasks: []Task{{ID: "T001", Title: "Operator issues the API token", Status: "todo", Priority: 1}}}
	if len(b.Ready()) != 0 {
		t.Fatal("legacy operator task entered worker queue")
	}
}

func TestFindingDoesNotEraseOperatorOwnership(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = nil
	applyFinding(t, e, reportedFinding(t, `{"title":"Authorize host","details":"Supply access","evidence":"401","owner":"operator","key":"access","priority":1}`))
	applyFinding(t, e, reportedFinding(t, `{"title":"Retry host","details":"Still blocked","evidence":"401 again","owner":"agent","key":"access","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 || len(e.State.Backlog.Ready()) != 0 {
		t.Fatal("duplicate changed operator ownership")
	}
}

func assertFindingEvidence(t *testing.T, e *Engine, id, evidence string) {
	t.Helper()
	events, err := e.Store.RecentEvents(-1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Kind == "finding" && strings.Contains(event.Message, id) && strings.Contains(event.Message, evidence) {
			return
		}
	}
	t.Fatal("duplicate discarded linked evidence")
}

func TestDuplicateEvidencePreservesRunningTaskContract(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = []Task{{ID: "T001", Title: "Fix validation", Details: "Reject invalid input", Status: "doing", Priority: 1, Tags: []string{"P0", "M0"}}}
	contract := e.State.Backlog.Tasks[0].Contract()
	applyFinding(t, e, reportedFinding(t, `{"title":"Same validation defect","details":"Do not replace acceptance criteria","evidence":"Another failed test","owner":"agent","key":"validation","existing_task_id":"T001","priority":1}`))
	if e.State.Backlog.Tasks[0].Contract() != contract {
		t.Fatal("new evidence invalidated a running worker contract")
	}
	if e.State.Backlog.Tasks[0].Details != "Reject invalid input" {
		t.Fatal("evidence changed acceptance criteria")
	}
}

func TestOperatorCapabilityRequirementRemainsAgentWork(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = nil
	applyFinding(t, e, Finding{Title: "Fix operator workflow", Details: "An operator must be able to manage sessions through the UI.", Evidence: "The reset button fails.", Priority: 1})
	if len(e.State.Backlog.Ready()) != 1 {
		t.Fatal("a product requirement was mistaken for an operator action")
	}
}

func TestOperatorFindingPreservesActiveContract(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = []Task{{ID: "T001", Title: "Fix validation", Details: "Reject invalid input", Status: "doing", Priority: 1}}
	before := e.State.Backlog.Tasks[0].Contract()
	applyFinding(t, e, Finding{Title: "Grant validation access", Details: "Supply credentials", Evidence: "401", Owner: "operator", Key: "validation", ExistingTaskID: "T001"})
	if e.State.Backlog.Tasks[0].Contract() != before || e.State.Backlog.Tasks[0].Status != "doing" {
		t.Fatal("finding changed an active contract")
	}
	assertFindingEvidence(t, e, "T001", "401")
}

func TestFindingReopensCompletedAgentTask(t *testing.T) {
	for _, match := range []string{"id", "key", "title"} {
		t.Run(match, func(t *testing.T) {
			e := snapshotEngine(t)
			original := Task{ID: "T001", Title: "Implement storage", Details: "Preserve archive safety", Status: "done", Priority: 1, Tags: []string{findingKeyPrefix + "storage"}}
			e.State.Backlog.Tasks = []Task{original}
			e.State.Attempts = []*Attempt{{ID: "old", Task: original, Phase: "done"}}
			e.State.Finished = true
			e.State.PlanSignature = "old-plan"
			finding := Finding{Title: "Repair storage", Details: "Publish durable B2 archives", Evidence: "B2 protection rejects conditional creation", Owner: "agent"}
			switch match {
			case "id":
				finding.ExistingTaskID = "T001"
			case "key":
				finding.Key = "storage"
			case "title":
				finding.Title = original.Title
			}
			applyFinding(t, e, finding)
			task := e.State.Backlog.Tasks[0]
			if len(e.State.Backlog.Tasks) != 1 || task.Status != "todo" || len(e.State.Backlog.Ready()) != 1 {
				t.Fatal("completed defect has no runnable repair")
			}
			if !strings.Contains(task.Details, original.Details) || !strings.Contains(task.Details, finding.Details) || !strings.Contains(task.Details, finding.Evidence) {
				t.Fatal("repair lost its contract or evidence")
			}
			if e.State.Finished || e.State.PlanSignature != "" {
				t.Fatal("completion or plan remains stale")
			}
			if e.State.Attempts[0].Task.Contract() != original.Contract() || e.State.Attempts[0].Phase != "done" {
				t.Fatal("completed attempt history changed")
			}
			applyFinding(t, e, finding)
			if len(e.State.Backlog.Tasks) != 1 || e.State.Backlog.Tasks[0].Details != task.Details {
				t.Fatal("repeated finding duplicated repair")
			}
		})
	}
}

func TestDistinctFindingsRetainRepairCriteria(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = []Task{{ID: "T001", Title: "Implement storage", Status: "done", Priority: 1}}
	for _, key := range []string{"readback", "large-pack"} {
		applyFinding(t, e, Finding{Title: key, Details: "Repair " + key, Evidence: key + " fails", Owner: "agent", Key: key, ExistingTaskID: "T001"})
	}
	task := e.State.Backlog.Tasks[0]
	if len(e.State.Backlog.Ready()) != 1 || !strings.Contains(task.Details, "Repair readback") || !strings.Contains(task.Details, "Repair large-pack") {
		t.Fatal("distinct repair criteria were lost")
	}
}

func TestFindingPreservesRetainedAttemptContract(t *testing.T) {
	for _, phase := range []string{"running", "ready", "verifying", "resume"} {
		t.Run(phase, func(t *testing.T) {
			e := snapshotEngine(t)
			task := Task{ID: "T001", Title: "Implement storage", Status: "todo", Priority: 1}
			e.State.Backlog.Tasks = []Task{task}
			e.State.Attempts = []*Attempt{{Task: task, Phase: phase}}
			applyFinding(t, e, Finding{Title: "Repair storage", Details: "Fix readback", Evidence: "Readback fails", Owner: "agent", ExistingTaskID: "T001"})
			if e.State.Backlog.Tasks[0].Contract() != task.Contract() {
				t.Fatal("finding invalidated retained work")
			}
		})
	}
}

func TestOperatorFollowupPreservesIdleAgentOwnership(t *testing.T) {
	for _, status := range []string{"todo", "blocked", "done"} {
		t.Run(status, func(t *testing.T) {
			e := snapshotEngine(t)
			original := Task{ID: "T001", Title: "Implement layout", Details: "Automate layout checks", Status: status, Priority: 1}
			e.State.Backlog.Tasks = []Task{original}
			applyFinding(t, e, Finding{Title: "Wire the layout gate", Details: "Operator adds the gate dependency", Evidence: "Dependency missing", Owner: "operator", ExistingTaskID: "T001", Key: "layout"})
			task := e.State.Backlog.Tasks[0]
			if task.OperatorOnly() || task.Status != status || task.UnblockWhen != "" || len(task.Blockers) != 0 {
				t.Fatalf("operator follow-up changed implementation ownership: %+v", task)
			}
			if task.Details != original.Details {
				t.Fatal("planning report replaced implementation scope")
			}
			assertFindingEvidence(t, e, "T001", "Dependency missing")
		})
	}
}
