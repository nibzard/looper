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

func TestFindingKeyDeduplicatesRenamedBlocker(t *testing.T) {
	e := snapshotEngine(t)
	before := len(e.State.Backlog.Tasks)
	e.addFinding(reportedFinding(t, `{"title":"Obtain benchmark access","details":"Operator supplies access","evidence":"401","owner":"operator","key":"benchmark-access","priority":1}`))
	e.addFinding(reportedFinding(t, `{"title":"Unblock dedicated pair provisioning","details":"Same access is missing","evidence":"second 401","owner":"operator","key":"benchmark-access","priority":1}`))
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
	e.addFinding(reportedFinding(t, `{"title":"Access still unavailable","details":"Same prerequisite","evidence":"401 again","owner":"operator","key":"access","existing_task_id":"T001","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 {
		t.Fatal("existing task was not updated")
	}
	assertFindingEvidence(t, e, "T001", "401 again")
	// The stable key must survive a save/reload and another renamed finding.
	data, _ := json.Marshal(e.State.Backlog)
	if err := json.Unmarshal(data, &e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	e.addFinding(reportedFinding(t, `{"title":"Retry access","details":"Still blocked","evidence":"401 third time","owner":"operator","key":"access","priority":1}`))
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
		e.addFinding(reportedFinding(t, body))
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
	e.addFinding(reportedFinding(t, `{"title":"Authorize host","details":"Operator grants access","evidence":"401","owner":"operator","key":"access","priority":1}`))
	e.State.Backlog.Tasks[0].Status = "done"
	e.addFinding(reportedFinding(t, `{"title":"Authorize the host again","details":"Old report","evidence":"old 401","owner":"agent","key":"access","priority":1}`))
	if len(e.State.Backlog.Tasks) != 1 || e.State.Backlog.Tasks[0].Status != "done" {
		t.Fatal("duplicate reopened completed work")
	}
}

func TestFindingRejectsInvalidOwnershipAndReference(t *testing.T) {
	for _, extra := range []string{`"owner":"robot"`, `"existing_task_id":"missing"`, `"key":" "`} {
		e := snapshotEngine(t)
		before := len(e.State.Backlog.Tasks)
		e.addFinding(reportedFinding(t, `{"title":"Fix code","details":"Acceptance","evidence":"Failure","priority":1,`+extra+`}`))
		if len(e.State.Backlog.Tasks) != before {
			t.Fatal("invalid finding created executable work")
		}
	}
}

func TestDistinctFindingsRemainRunnable(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = nil
	for _, key := range []string{"input-validation", "output-validation"} {
		e.addFinding(reportedFinding(t, `{"title":"Fix `+key+`","details":"Document operator workflow","evidence":"Test fails","owner":"agent","key":"`+key+`","priority":1}`))
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
	e.addFinding(reportedFinding(t, `{"title":"Authorize host","details":"Supply access","evidence":"401","owner":"operator","key":"access","priority":1}`))
	e.addFinding(reportedFinding(t, `{"title":"Retry host","details":"Still blocked","evidence":"401 again","owner":"agent","key":"access","priority":1}`))
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
	e.addFinding(reportedFinding(t, `{"title":"Same validation defect","details":"Do not replace acceptance criteria","evidence":"Another failed test","owner":"agent","key":"validation","existing_task_id":"T001","priority":1}`))
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
	e.addFinding(Finding{Title: "Fix operator workflow", Details: "An operator must be able to manage sessions through the UI.", Evidence: "The reset button fails.", Priority: 1})
	if len(e.State.Backlog.Ready()) != 1 {
		t.Fatal("a product requirement was mistaken for an operator action")
	}
}

func TestOperatorFindingPreservesActiveContract(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Backlog.Tasks = []Task{{ID: "T001", Title: "Fix validation", Details: "Reject invalid input", Status: "doing", Priority: 1}}
	before := e.State.Backlog.Tasks[0].Contract()
	e.addFinding(Finding{Title: "Grant validation access", Details: "Supply credentials", Evidence: "401", Owner: "operator", Key: "validation", ExistingTaskID: "T001"})
	if e.State.Backlog.Tasks[0].Contract() != before || e.State.Backlog.Tasks[0].Status != "doing" {
		t.Fatal("finding changed an active contract")
	}
	assertFindingEvidence(t, e, "T001", "401")
}
