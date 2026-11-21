package looper

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/ui"
)

func reasonIs(t *testing.T, r *BlockReason, class, code, retry string) {
	t.Helper()
	if r == nil {
		t.Fatal("reason is nil")
	}
	if r.Class != class || r.Code != code || r.Retry != retry {
		t.Fatalf("reason: %+v", r)
	}
}

func TestControlTagsProduceStructuredReasons(t *testing.T) {
	task := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo"}
	holdForOperator(&task)
	reasonIs(t, task.BlockReason, blockClassOperator, blockCodeOperatorPrerequisite, blockRetryAfterChange)
	if task.BlockReason.Source != blockSourceController {
		t.Fatalf("source: %+v", task.BlockReason)
	}

	task = Task{ID: "T2", Title: "two", Priority: 1, Status: "todo"}
	deferTask(&task, "the release ships")
	reasonIs(t, task.BlockReason, blockClassDeferred, blockCodeDeferredCondition, blockRetryAfterChange)

	// Both tags present: deferred is the primary class; the operator gate
	// stays a separate blocker.
	both := Task{ID: "T3", Title: "three", Priority: 1, Status: "blocked",
		Tags: []string{operatorTaskTag, deferredTaskTag}}
	reasonIs(t, classifyBlockReason(&both, nil, blockSourceWorker), blockClassDeferred, blockCodeDeferredCondition, blockRetryAfterChange)

	// A reported reason replaces prior metadata; without one the class stays
	// unknown instead of guessing from blocker text.
	plain := Task{ID: "T4", Title: "four", Priority: 1, Status: "blocked", Blockers: []string{"quota exceeded"}}
	reasonIs(t, classifyBlockReason(&plain, nil, blockSourceWorker), blockClassUnknown, blockCodeUnknown, blockRetryUnknown)
	typed := classifyBlockReason(&plain, failureBlockReason(blockCodeRepairLimit, blockRetryNo, "a1"), blockSourceWorker)
	reasonIs(t, typed, blockClassFailure, blockCodeRepairLimit, blockRetryNo)
}

func TestWorkerFailureReasonUsesTypedEvidence(t *testing.T) {
	a := &Attempt{ID: "a-timeout"}
	s := &FailureSummary{Operation: opWorker, Outcome: outcomeTimeout}
	r := workerFailureBlockReason(a, s)
	if r.Retry != blockRetryYes || r.Class != blockClassFailure || r.AttemptID != "a-timeout" {
		t.Fatalf("timeout reason: %+v", r)
	}
	s = &FailureSummary{Operation: opWorker, Outcome: outcomeExit}
	r = workerFailureBlockReason(a, s)
	if r.Retry != blockRetryUnknown || r.Code != outcomeExit {
		t.Fatalf("exit reason: %+v", r)
	}
	// A non-worker operation keeps its operation name in the code.
	s = &FailureSummary{Operation: opVerify, Outcome: outcomeExit}
	if code := workerFailureBlockReason(a, s).Code; code != "verify_"+outcomeExit {
		t.Fatalf("verify code: %q", code)
	}
}

func TestApplyResultManagesCurrentReason(t *testing.T) {
	e, _ := exportTestEngine(t)
	b := e.State.Backlog
	b.Tasks[0].Status = "doing"
	a := &Attempt{ID: "task-1", Task: b.Tasks[0], Directory: e.Config.Root}
	e.State.Attempts = append(e.State.Attempts, a)

	// A blocked worker report without typed evidence stays unknown.
	a.Summary = &Summary{TaskID: b.Tasks[0].ID, Status: "blocked", Summary: "cannot proceed", Files: []string{}, Blockers: []string{"needs credentials"}}
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	reasonIs(t, e.State.Backlog.Tasks[0].BlockReason, blockClassUnknown, blockCodeUnknown, blockRetryUnknown)

	// A typed hold replaces the current reason.
	if err := e.holdReason(a, "Repair limit reached.", failureBlockReason(blockCodeRepairLimit, blockRetryNo, a.ID)); err != nil {
		t.Fatal(err)
	}
	r := e.State.Backlog.Tasks[0].BlockReason
	reasonIs(t, r, blockClassFailure, blockCodeRepairLimit, blockRetryNo)
	if r.AttemptID != a.ID || r.Source != blockSourceController {
		t.Fatalf("typed reason: %+v", r)
	}

	// Successful completion clears the current reason; blockers stay as
	// history.
	a.Summary = &Summary{TaskID: b.Tasks[0].ID, Status: "done", Summary: "done", Files: []string{}, Blockers: []string{}}
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if e.State.Backlog.Tasks[0].BlockReason != nil {
		t.Fatalf("completion must clear the reason: %+v", e.State.Backlog.Tasks[0].BlockReason)
	}
}

func TestContractExcludesReasonWhileRevisionTracksIt(t *testing.T) {
	base := Task{ID: "T1", Title: "one", Priority: 1, Status: "blocked", Blockers: []string{"x"}}
	withReason := base
	withReason.BlockReason = failureBlockReason(blockCodeMergeFailed, blockRetryAfterChange, "a1")
	if base.Contract() != withReason.Contract() {
		t.Fatal("the reason must stay outside the acceptance contract")
	}
	if hashJSON(base) == hashJSON(withReason) {
		t.Fatal("the mutation revision must track reason edits")
	}
	// A reason-only edit changes task data, not the contract.
	saved := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{base}}
	observed := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{withReason}}
	d := compareBacklog(saved, marshalBacklog(t, observed))
	if len(d.Data) != 1 || d.Data[0] != "T1" || len(d.Changed) != 0 {
		t.Fatalf("comparison: %+v", d)
	}
}

func marshalBacklog(t *testing.T, b Backlog) []byte {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEffectiveBlockReasonDerivesLegacyState(t *testing.T) {
	operator := Task{Status: "blocked", Tags: []string{operatorTaskTag}}
	reasonIs(t, operator.effectiveBlockReason(), blockClassOperator, blockCodeOperatorPrerequisite, blockRetryAfterChange)
	deferred := Task{Status: "blocked", Tags: []string{operatorTaskTag, deferredTaskTag}}
	reasonIs(t, deferred.effectiveBlockReason(), blockClassDeferred, blockCodeDeferredCondition, blockRetryAfterChange)
	legacy := Task{Status: "blocked", Blockers: []string{"unclear"}}
	reasonIs(t, legacy.effectiveBlockReason(), blockClassUnknown, blockCodeUnknown, blockRetryUnknown)
	if (Task{Status: "todo"}).effectiveBlockReason() != nil {
		t.Fatal("an unblocked task carries no reason")
	}
	// Invalid stored metadata degrades to unknown for readers.
	broken := Task{Status: "blocked", BlockReason: &BlockReason{Class: "sideways", Code: strings.Repeat("x", 10)}}
	reasonIs(t, broken.effectiveBlockReason(), blockClassUnknown, blockCodeUnknown, blockRetryUnknown)
	// Stored reasons pass through unchanged.
	stored := Task{Status: "blocked", BlockReason: failureBlockReason(blockCodeRepeatedFailure, blockRetryNo, "a2")}
	if *stored.effectiveBlockReason() != *stored.BlockReason {
		t.Fatalf("stored reason: %+v", stored.effectiveBlockReason())
	}
}

func TestNormalizeImportedBlockReasons(t *testing.T) {
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "blocked"},
		{ID: "T2", Title: "two", Priority: 1, Status: "blocked", Tags: []string{deferredTaskTag}},
		{ID: "T3", Title: "three", Priority: 1, Status: "todo"},
	}}
	normalizeImportedBlockReasons(&b)
	reasonIs(t, b.Tasks[0].BlockReason, blockClassUnknown, blockCodeUnknown, blockRetryUnknown)
	if b.Tasks[0].BlockReason.Source != blockSourceImport {
		t.Fatalf("import source: %+v", b.Tasks[0].BlockReason)
	}
	reasonIs(t, b.Tasks[1].BlockReason, blockClassDeferred, blockCodeDeferredCondition, blockRetryAfterChange)
	if b.Tasks[2].BlockReason != nil {
		t.Fatal("an open task must not gain a reason")
	}
}

func TestHoldQuarantinedRecordsInvalidWritePath(t *testing.T) {
	e := snapshotEngine(t)
	before := e.State.Backlog.Tasks[2].BlockReason
	if !e.holdQuarantined("T3", "write path invalid: ../outside") {
		t.Fatal("the quarantine must hold the task")
	}
	reasonIs(t, e.State.Backlog.Tasks[2].BlockReason, blockClassFailure, blockCodeInvalidWritePath, blockRetryAfterChange)
	if before != nil {
		t.Fatal("fixture must start without a reason")
	}
}

func TestDoctorFindingViewsCarryReasonAndSeverity(t *testing.T) {
	s := &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "blocked", Tags: []string{operatorTaskTag}},
		{ID: "T2", Title: "two", Priority: 1, Status: "blocked", BlockReason: failureBlockReason(blockCodeRepairLimit, blockRetryNo, "a1")},
	}}}
	findings := []HealthFinding{
		{Code: "task_blocked", Severity: "warning", TaskID: "T1", Message: "m1", Action: "a1"},
		{Code: "task_blocked", Severity: "warning", TaskID: "T2", Message: "m2", Action: "a2"},
		{Code: "task_deferred", Severity: "info", TaskID: "T3", Message: "m3", Action: "a3"},
	}
	views := doctorFindingViews(findings, s)
	if views[0].Severity != "info" || views[0].ReasonClass != blockClassOperator {
		t.Fatalf("operator view: %+v", views[0])
	}
	if views[1].Severity != "warning" || views[1].ReasonClass != blockClassFailure || views[1].Retry != blockRetryNo || views[1].AttemptID != "a1" {
		t.Fatalf("failure view: %+v", views[1])
	}
	if views[2].ReasonClass != "" {
		t.Fatalf("a finding without a task keeps no reason: %+v", views[2])
	}
	// The legacy document keeps the shared finding shape: no reason fields
	// exist on it.
	legacy, err := json.Marshal(findings[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(legacy), "reason_") {
		t.Fatal("the shared finding must stay reason-free")
	}
}

func TestInspectTaskContractCarriesReason(t *testing.T) {
	task := Task{ID: "T1", Title: "one", Priority: 1, Status: "blocked",
		Blockers: []string{"quota"}, BlockReason: failureBlockReason(blockCodeRepairLimit, blockRetryNo, "task-T1-9")}
	c := inspectTaskContractOf(&task, inspectFits[0])
	if c.ReasonClass != blockClassFailure || c.ReasonCode != blockCodeRepairLimit || c.Retry != blockRetryNo || c.AttemptID != "task-T1-9" {
		t.Fatalf("inspect reason: %+v", c)
	}
	open := Task{ID: "T2", Title: "two", Priority: 1, Status: "todo"}
	if c := inspectTaskContractOf(&open, inspectFits[0]); c.ReasonClass != "" || c.AttemptID != "" {
		t.Fatalf("open task reason: %+v", c)
	}
}

func TestLegacyDocumentsOmitBlockReason(t *testing.T) {
	e, _ := exportTestEngine(t)
	e.State.Backlog.Tasks[0].Status = "blocked"
	e.State.Backlog.Tasks[0].BlockReason = failureBlockReason(blockCodeRepairLimit, blockRetryNo, "a1")
	if err := e.Store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	snap, err := e.Store.Snapshot()
	if err != nil || snap == nil || snap.State == nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, err := json.Marshal(statusReport(e.Config, snap, TimingReport{}, ui.StatisticsView{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "block_reason") {
		t.Fatal("the legacy status document must omit block_reason on nested tasks")
	}
}

func TestAmendCorrectsBlockReason(t *testing.T) {
	e := requeueEngine(t)
	e.State.Backlog.Tasks[0].BlockReason = &BlockReason{Class: blockClassUnknown, Code: blockCodeUnknown, Retry: blockRetryUnknown, Source: blockSourceWorker}
	amend := func(reason *BlockReason) SettingsAck {
		task := e.State.Backlog.Tasks[0]
		amended := task
		amended.BlockReason = reason
		return e.applyAmendRequest(&SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
			Session: e.controller.Session, TaskID: task.ID, TaskRevision: hashJSON(task), Tasks: []Task{amended}})
	}
	fixed := &BlockReason{Class: blockClassFailure, Code: "agent_quota_exhausted", Retry: blockRetryAfterChange}
	ack := amend(fixed)
	if ack.Status != AckApplied {
		t.Fatalf("correction refused: %s %s", ack.Status, ack.Message)
	}
	r := e.State.Backlog.Tasks[0].BlockReason
	reasonIs(t, r, blockClassFailure, "agent_quota_exhausted", blockRetryAfterChange)
	if r.Source != blockSourceAmend {
		t.Fatalf("amend source: %+v", r)
	}
	// The stored revision tracks the reason edit.
	e.State.Backlog.Tasks[0].BlockReason = &BlockReason{Class: blockClassUnknown, Code: blockCodeUnknown, Retry: blockRetryUnknown, Source: blockSourceWorker}
	if ack := amend(&BlockReason{Class: "waiting"}); ack.Status != AckError || !strings.Contains(ack.Message, "block_reason") {
		t.Fatalf("invalid class accepted: %+v", ack)
	}
	if ack := amend(nil); ack.Status != AckConflict || !strings.Contains(ack.Message, "removing") {
		t.Fatalf("removal accepted: %+v", ack)
	}
	if ack := amend(&BlockReason{Class: blockClassFailure, Code: "x"}); ack.Status != AckError {
		t.Fatalf("missing retry guidance accepted: %+v", ack)
	}
	// A control tag fixes the class.
	e.State.Backlog.Tasks[0].Tags = []string{deferredTaskTag}
	e.State.Backlog.Tasks[0].BlockReason = nil
	if ack := amend(&BlockReason{Class: blockClassFailure, Code: "x", Retry: blockRetryNo}); ack.Status != AckConflict || !strings.Contains(ack.Message, "control tag") {
		t.Fatalf("reclassification accepted: %+v", ack)
	}
	// An open task does not take reason metadata.
	e.State.Backlog.Tasks[0].Tags = nil
	e.State.Backlog.Tasks[0].Status = "todo"
	e.State.Backlog.Tasks[0].BlockReason = nil
	if ack := amend(&BlockReason{Class: blockClassFailure, Code: "x", Retry: blockRetryNo}); ack.Status != AckConflict || !strings.Contains(ack.Message, "blocked") {
		t.Fatalf("open-task correction accepted: %+v", ack)
	}
}

func TestRequeueClearsReasonWithBlockers(t *testing.T) {
	e := requeueEngine(t)
	e.State.Backlog.Tasks[0].BlockReason = failureBlockReason(blockCodeRepairLimit, blockRetryNo, "a1")
	ack := e.applyRequeueRequest(requeueRequest(e))
	if ack.Status != AckApplied {
		t.Fatalf("requeue refused: %s %s", ack.Status, ack.Message)
	}
	task := e.State.Backlog.Tasks[0]
	if task.Status != "todo" || len(task.Blockers) != 0 || task.BlockReason != nil {
		t.Fatalf("requeue must clear the barrier: %+v", task)
	}
}

// A worker result that closes its own dispatched task is ordinary
// progression: the recorded review shape follows it, so the next review keeps
// its focused or skip scope (T128). A shape that was already stale stays
// stale, and a follow-up task the worker reports still moves the shape.
func TestApplyResultKeepsReviewShapeForOwnTask(t *testing.T) {
	e, _ := exportTestEngine(t)
	e.State.Backlog.Tasks[0].Status = "doing"
	task := e.State.Backlog.Tasks[0]
	a := &Attempt{ID: "task-1", Task: task, Directory: e.Config.Root}
	e.State.Attempts = append(e.State.Attempts, a)
	e.State.ReviewedBacklog = backlogShape(e.State.Backlog)
	a.Summary = &Summary{TaskID: task.ID, Status: "done", Summary: "done", Files: []string{}, Blockers: []string{}}
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if e.State.ReviewedBacklog != backlogShape(e.State.Backlog) {
		t.Fatal("closing the dispatched task must not widen the next review")
	}

	stale := "stale-shape"
	e.State.Backlog.Tasks[0].Status = "doing"
	e.State.ReviewedBacklog = stale
	if err := e.applyResult(a); err != nil {
		t.Fatal(err)
	}
	if e.State.ReviewedBacklog != stale {
		t.Fatal("a shape that no longer described the backlog must stay stale")
	}
}
