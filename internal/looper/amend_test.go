package looper

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// amendEngine mirrors cancelEngine: a protocol engine with one exported
// backlog, so amendment tests exercise the real export guard and receipt
// store.
func amendEngine(t *testing.T, tasks []Task, attempts ...*Attempt) *Engine {
	t.Helper()
	e := protocolEngine(t)
	e.Config.Todo = filepath.Join(e.Dir, "to-do.json")
	e.State = &State{Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: tasks}, Attempts: attempts}
	if err := writeJSON(e.Config.Todo, e.State.Backlog); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		t.Fatal(err)
	}
	e.State.ExportHash = digest(data)
	return e
}

// amendRequest builds one checked amend envelope for the amendment the
// caller's task file implies: the revision the file held, the live task it
// read, and the amended task itself.
func amendRequest(e *Engine, amended Task) *SettingsRequest {
	revision := ""
	if t := e.State.Backlog.Task(amended.ID); t != nil {
		revision = hashJSON(*t)
	}
	return &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
		Session: e.controller.Session, TaskID: amended.ID, TaskRevision: revision, Tasks: []Task{amended}}
}

func submitAmendClaim(t *testing.T, e *Engine, req *SettingsRequest) SettingsAck {
	t.Helper()
	if err := writeJSON(filepath.Join(requestDir(e.Dir), req.ID+".json"), req); err != nil {
		t.Fatal(err)
	}
	e.processAmendRequests()
	if e.err != nil {
		t.Fatalf("amend pass failed: %v", e.err)
	}
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), req.ID+".json"), &ack); err != nil {
		t.Fatalf("ack: %v", err)
	}
	return ack
}

// A valid amendment of an open, unheld task applies once: the amended fields
// commit through the export, the plan drops its stale scope, the receipt
// names the task and its changed fields, and a replay returns the recorded
// answer without a second mutation.
func TestAmendAppliesCheckedRequest(t *testing.T) {
	e := amendEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 2, Status: "todo", Details: "old details", CreatedAt: "2026-11-01T00:00:00Z"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	})
	e.State.Plan, e.State.PlanSignature = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T1", Paths: []string{"one/"}}, {TaskID: "T2", Paths: []string{"two/"}},
	}}, "stale"
	amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo", Details: "new details",
		Files: []string{"src/one/"}, DependsOn: []string{"T2"}, CreatedAt: "2026-11-01T00:00:00Z"}
	req := amendRequest(e, amended)
	ack := submitAmendClaim(t, e, req)
	if ack.Status != AckApplied {
		t.Fatalf("amend: %+v", ack)
	}
	task := e.State.Backlog.Task("T1")
	if task.Title != "one revised" || task.Priority != 1 || task.Details != "new details" ||
		strings.Join(task.Files, ",") != "src/one/" || strings.Join(task.DependsOn, ",") != "T2" {
		t.Fatalf("task not amended: %+v", task)
	}
	if task.CreatedAt != "2026-11-01T00:00:00Z" || task.UpdatedAt == "" {
		t.Fatalf("history anchors: %+v", task)
	}
	if e.State.Backlog.Task("T2").Title != "two" {
		t.Fatal("an unrelated task changed")
	}
	var exported Backlog
	if err := readJSON(e.Config.Todo, &exported); err != nil || exported.Tasks[0].Title != "one revised" {
		t.Fatalf("export: %v", err)
	}
	if e.State.Plan == nil || e.State.PlanSignature != "" {
		t.Fatal("amend discarded the live plan instead of its signature")
	}
	if scope := e.State.Plan.Scope("T1"); strings.Join(scope.Paths, ",") != "." {
		t.Fatalf("the stale plan scope survived: %+v", scope)
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil || rec.State != RequestApplied || strings.Join(rec.AffectedTasks, ",") != "T1" {
		t.Fatalf("receipt: %+v", rec)
	}
	if !strings.Contains(rec.Summary, "files") || !strings.Contains(rec.Summary, "depends_on") {
		t.Fatalf("summary hides the changed fields: %s", rec.Summary)
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range events {
		if events[i].Kind != "amend" {
			continue
		}
		found = true
		var payload map[string]any
		if err = json.Unmarshal(events[i].Data, &payload); err != nil || payload["request_id"] != req.ID {
			t.Fatalf("amend event identity: %s", events[i].Data)
		}
	}
	if !found {
		t.Fatal("no amend event")
	}
	if repeated := e.applyAmendRequest(req); repeated.Status != AckApplied || repeated.Message != ack.Message {
		t.Fatalf("replay: %+v", repeated)
	}
	if e.State.Backlog.Task("T1").Title != "one revised" {
		t.Fatal("replay mutated twice")
	}
}

// Every guard refuses without touching saved work: a changed task, an
// unknown task, a smuggled status change, a renamed task, a control-tag
// change, a stale session, a held external edit, a cancelled task, and an
// amendment that changes nothing.
func TestAmendGuardsRefuseWithoutMutation(t *testing.T) {
	for _, mode := range []string{"stale-revision", "unknown-task", "status-change", "renamed",
		"control-tag", "stale-session", "held-edit", "cancelled-task", "no-op", "empty-payload"} {
		t.Run(mode, func(t *testing.T) {
			e := amendEngine(t, []Task{
				{ID: "T1", Title: "one", Priority: 1, Status: "todo", Tags: []string{"work"}},
				{ID: "T8", Title: "cancelled work", Priority: 1, Status: "cancelled"},
			})
			var req *SettingsRequest
			switch mode {
			case "stale-revision":
				amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}
				req = amendRequest(e, amended)
				req.TaskRevision = "changed"
			case "unknown-task":
				amended := Task{ID: "T404", Title: "missing", Priority: 1, Status: "todo"}
				req = &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
					Session: e.controller.Session, TaskID: "T404", TaskRevision: "any", Tasks: []Task{amended}}
			case "status-change":
				amended := Task{ID: "T1", Title: "one", Priority: 1, Status: "done"}
				req = amendRequest(e, amended)
				// The revision covers the original task; the payload smuggles
				// a status the engine never granted.
				req.Tasks = []Task{amended}
			case "renamed":
				amended := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo"}
				req = amendRequest(e, amended)
				req.TaskID = "T2"
			case "control-tag":
				amended := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", Tags: []string{"work", "project-done"}}
				req = amendRequest(e, amended)
			case "stale-session":
				amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}
				req = amendRequest(e, amended)
				req.Session = "run-old"
			case "held-edit":
				amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}
				req = amendRequest(e, amended)
				edited := clone(e.State.Backlog)
				edited.Tasks[0].Title = "edited outside"
				if err := writeJSON(e.Config.Todo, edited); err != nil {
					t.Fatal(err)
				}
			case "cancelled-task":
				amended := Task{ID: "T8", Title: "cancelled revised", Priority: 1, Status: "cancelled"}
				req = amendRequest(e, amended)
			case "no-op":
				req = amendRequest(e, Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", Tags: []string{"work"}})
			case "empty-payload":
				req = &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
					Session: e.controller.Session, TaskID: "T1"}
			}
			before := hashJSON(e.State.Backlog)
			ack := e.applyAmendRequest(req)
			if ack.Status != AckConflict && ack.Status != AckStale && ack.Status != AckError {
				t.Fatalf("%s: %+v", mode, ack)
			}
			if ack.Message == "" {
				t.Fatalf("%s: refusal without a reason", mode)
			}
			if hashJSON(e.State.Backlog) != before {
				t.Fatalf("%s: a refusal mutated the backlog", mode)
			}
			rec := e.requestReceipt(req.ID)
			if rec == nil || rec.State != RequestRejected {
				t.Fatalf("%s: receipt %+v", mode, rec)
			}
		})
	}
}

// Dependency cycles and unknown dependencies are refused before anything
// commits, and the refusal names the cycle.
func TestAmendValidatesDependencies(t *testing.T) {
	e := amendEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo", DependsOn: []string{"T1"}},
	})
	cycle := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", DependsOn: []string{"T2"}}
	if ack := submitAmendClaim(t, e, amendRequest(e, cycle)); ack.Status != AckConflict ||
		!strings.Contains(ack.Message, "cycle") {
		t.Fatalf("cycle: %+v", ack)
	}
	unknown := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", DependsOn: []string{"T404"}}
	if ack := submitAmendClaim(t, e, amendRequest(e, unknown)); ack.Status != AckConflict ||
		!strings.Contains(ack.Message, "T404") {
		t.Fatalf("unknown dependency: %+v", ack)
	}
	if e.State.Backlog.Task("T1").DependsOn != nil {
		t.Fatal("a refused amendment changed dependencies")
	}
	// A self-dependency is a cycle too.
	self := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", DependsOn: []string{"T1"}}
	if ack := submitAmendClaim(t, e, amendRequest(e, self)); ack.Status != AckConflict {
		t.Fatalf("self dependency: %+v", ack)
	}
}

// Changed write scopes are validated before committing: an absolute path, a
// parent escape, and a glob are refused, in place and against done work.
func TestAmendValidatesWriteScopes(t *testing.T) {
	for _, path := range []string{"/etc/passwd", "../outside", "src/*.go"} {
		e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}})
		amended := Task{ID: "T1", Title: "one", Priority: 1, Status: "todo", Files: []string{path}}
		ack := submitAmendClaim(t, e, amendRequest(e, amended))
		if ack.Status != AckConflict || !strings.Contains(ack.Message, "write paths") {
			t.Fatalf("%s: %+v", path, ack)
		}
		if e.State.Backlog.Task("T1").Files != nil {
			t.Fatalf("%s: the refused scope committed", path)
		}
	}
}

// Work that holds the task's contract refuses with the boundary named:
// running, verifying, ready, and resume attempts all own it, because the
// next startup compares them against the contract.
func TestAmendActiveWorkExplainsBoundary(t *testing.T) {
	for _, phase := range []string{"running", "verifying", "ready", "resume", "integrated"} {
		t.Run(phase, func(t *testing.T) {
			attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: phase}
			e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
			amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "doing"}
			ack := submitAmendClaim(t, e, amendRequest(e, amended))
			if ack.Status != AckConflict {
				t.Fatalf("%s: %+v", phase, ack)
			}
			if !strings.Contains(ack.Message, phase) || !strings.Contains(ack.Message, "cannot change under a worker") {
				t.Fatalf("%s: the boundary is not explained: %s", phase, ack.Message)
			}
			if e.State.Backlog.Task("T1").Title != "one" {
				t.Fatalf("%s: the task changed under its worker", phase)
			}
			if rec := e.requestReceipt(amendRequest(e, amended).ID); rec != nil {
				t.Fatalf("%s: unexpected receipt %+v", phase, rec)
			}
		})
	}
	// The work drains: the same amendment applies at the open boundary.
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "running"}
	e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "doing"}}, attempt)
	amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "doing"}
	if ack := submitAmendClaim(t, e, amendRequest(e, amended)); ack.Status != AckConflict {
		t.Fatalf("active amend: %+v", ack)
	}
	attempt.Phase = "done"
	// The result applied: the task is done now, and completed work takes the
	// follow-up path rather than an in-place rewrite.
	e.State.Backlog.Task("T1").Status = "done"
	amended.Status = "done"
	ack := submitAmendClaim(t, e, amendRequest(e, amended))
	if ack.Status != AckApplied || !strings.Contains(ack.Message, "follow-up") {
		t.Fatalf("drained amend: %+v", ack)
	}
}

// A blocked attempt keeps its retained result for inspection and does not
// hold the contract: the amendment applies, the attempt record stays, and a
// later resume does not adopt the moved contract.
func TestAmendKeepsRetainedBlockedResult(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T1", Title: "one", Priority: 1}, Phase: "blocked",
		ResultCommit: "commit-1", Summary: &Summary{TaskID: "T1", Status: "blocked", Summary: "retained", Files: []string{}, Blockers: []string{"external"}}}
	e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "blocked", Blockers: []string{"external"}}}, attempt)
	amended := Task{ID: "T1", Title: "one revised", Priority: 1, Status: "blocked", Blockers: []string{"external"}}
	if ack := submitAmendClaim(t, e, amendRequest(e, amended)); ack.Status != AckApplied {
		t.Fatalf("blocked amend: %+v", ack)
	}
	if attempt.Phase != "blocked" || attempt.ResultCommit != "commit-1" || attempt.Summary == nil {
		t.Fatal("the amendment discarded a retained result")
	}
	if e.State.Backlog.Task("T1").Title != "one revised" {
		t.Fatal("the blocked task was not amended")
	}
	if e.State.Backlog.Task("T1").Contract() == attempt.Task.Contract() {
		t.Fatal("the amended contract still matches the retained attempt")
	}
}

// Changed requirements against completed work create follow-up work: the
// done task keeps its identity, status, and attempts byte-for-byte, and the
// follow-up carries the amended requirements as open work.
func TestAmendDoneTaskCreatesFollowUp(t *testing.T) {
	attempt := &Attempt{ID: "a1", Task: Task{ID: "T9", Title: "done work", Priority: 1}, Phase: "done",
		ResultCommit: "commit-9", Summary: &Summary{TaskID: "T9", Status: "done", Summary: "finished", Files: []string{"a.go"}, Blockers: []string{}}}
	e := amendEngine(t, []Task{
		{ID: "T9", Title: "done work", Priority: 1, Status: "done", CreatedAt: "2026-11-01T00:00:00Z"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	}, attempt)
	before := clone(*e.State.Backlog.Task("T9"))
	amended := Task{ID: "T9", Title: "done work revised", Priority: 3, Status: "done",
		Details: "more work needed", Files: []string{"b.go"}, DependsOn: []string{"T2"}, CreatedAt: "2026-11-01T00:00:00Z"}
	ack := submitAmendClaim(t, e, amendRequest(e, amended))
	if ack.Status != AckApplied {
		t.Fatalf("done amend: %+v", ack)
	}
	done := e.State.Backlog.Task("T9")
	if hashJSON(*done) != hashJSON(before) {
		t.Fatalf("the done task was rewritten: %+v", done)
	}
	follow := e.State.Backlog.Task("T9-amend-1")
	if follow == nil || follow.Status != "todo" || follow.Title != "done work revised" ||
		follow.Priority != 3 || strings.Join(follow.Files, ",") != "b.go" ||
		strings.Join(follow.DependsOn, ",") != "T2" {
		t.Fatalf("follow-up: %+v", follow)
	}
	if follow.CreatedAt == before.CreatedAt || follow.UpdatedAt == "" {
		t.Fatalf("follow-up history anchors: %+v", follow)
	}
	if attempt.Phase != "done" || attempt.ResultCommit != "commit-9" {
		t.Fatal("the accepted result was disturbed")
	}
	rec := lastReceipt(e)
	if rec.State != RequestApplied || strings.Join(rec.AffectedTasks, ",") != "T9-amend-1" {
		t.Fatalf("receipt: %+v", rec)
	}
	if !strings.Contains(rec.Summary, "stays as accepted") {
		t.Fatalf("summary: %s", rec.Summary)
	}
	// A second amendment of the same accepted work takes the next identity.
	amended.Title = "done work revised again"
	ack = submitAmendClaim(t, e, amendRequest(e, Task{ID: "T9", Title: "done work revised again", Priority: 3, Status: "done", DependsOn: []string{"T2"}}))
	if ack.Status != AckApplied || e.State.Backlog.Task("T9-amend-2") == nil {
		t.Fatalf("second follow-up: %+v", ack)
	}
}

// Concurrent amendments conflict without loss: the first applies, the second
// one's revision no longer matches, both outcomes stay queryable, and the
// first one's application is intact.
func TestAmendConcurrentEditsConflictWithoutLoss(t *testing.T) {
	e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}})
	first := amendRequest(e, Task{ID: "T1", Title: "first edit", Priority: 1, Status: "todo"})
	// Both callers read the same revision.
	second := amendRequest(e, Task{ID: "T1", Title: "second edit", Priority: 2, Status: "todo"})
	if ack := submitAmendClaim(t, e, first); ack.Status != AckApplied {
		t.Fatalf("first: %+v", ack)
	}
	ack := submitAmendClaim(t, e, second)
	if ack.Status != AckConflict || !strings.Contains(ack.Message, "changed") {
		t.Fatalf("concurrent: %+v", ack)
	}
	if e.State.Backlog.Task("T1").Title != "first edit" {
		t.Fatal("the conflict overwrote the first application")
	}
	firstRec, secondRec := e.requestReceipt(first.ID), e.requestReceipt(second.ID)
	if firstRec.State != RequestApplied || secondRec.State != RequestRejected {
		t.Fatalf("receipts: %+v %+v", firstRec, secondRec)
	}
	if !strings.Contains(secondRec.Error, "changed") {
		t.Fatalf("the rejected receipt lost its reason: %+v", secondRec)
	}
}

// A restart preserves both the accepted history and the applied amendment
// receipts: the saved state holds the amended backlog, the done attempts,
// and the receipts a query reads after the controller returns.
func TestAmendRestartPreservesHistoryAndReceipts(t *testing.T) {
	doneAttempt := &Attempt{ID: "a1", Task: Task{ID: "T9", Title: "done work", Priority: 1}, Phase: "done",
		ResultCommit: "commit-9", Summary: &Summary{TaskID: "T9", Status: "done", Summary: "finished", Files: []string{"a.go"}, Blockers: []string{}}}
	e := amendEngine(t, []Task{
		{ID: "T9", Title: "done work", Priority: 1, Status: "done"},
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}, doneAttempt)
	inPlace := submitAmendClaim(t, e, amendRequest(e, Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}))
	if inPlace.Status != AckApplied {
		t.Fatalf("in place: %+v", inPlace)
	}
	follow := submitAmendClaim(t, e, amendRequest(e, Task{ID: "T9", Title: "done work revised", Priority: 1, Status: "done"}))
	if follow.Status != AckApplied {
		t.Fatalf("follow-up: %+v", follow)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Backlog.Task("T1").Title != "one revised" {
		t.Fatal("the amendment did not survive the save")
	}
	if saved.Backlog.Task("T9").Title != "done work" || saved.Backlog.Task("T9-amend-1") == nil {
		t.Fatal("the accepted history or the follow-up did not survive")
	}
	kept := 0
	for _, a := range saved.Attempts {
		if a.Phase == "done" && a.ResultCommit == "commit-9" {
			kept++
		}
	}
	if kept != 1 {
		t.Fatal("the done attempt did not survive")
	}
	var receipts []*RequestRecord
	for _, r := range saved.Requests {
		if r != nil && r.State == RequestApplied {
			receipts = append(receipts, r)
		}
	}
	if len(receipts) != 2 {
		t.Fatalf("applied receipts: %+v", saved.Requests)
	}
	// The loaded receipts answer a query: a replayed request returns its
	// recorded acknowledgement instead of amending twice.
	replay := amendRequest(e, Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"})
	replay.ID = inPlace.ID
	if ack := e.applyAmendRequest(replay); ack.Status != AckApplied || ack.Message != inPlace.Message {
		t.Fatalf("replay after restart: %+v", ack)
	}
}

// A failed export rolls the staged amendment back — the task, the dropped
// plan scope, and the receipt queue — and records the failure.
func TestAmendExportFailureRollsBack(t *testing.T) {
	e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}})
	e.State.Plan, e.State.PlanSignature = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"one/"}}}}, "sig"
	before := hashJSON(e.State.Backlog)
	beforeScope := clone(e.State.Plan.Scope("T1"))
	if err := e.Store.Close(); err != nil {
		t.Fatal(err)
	}
	req := amendRequest(e, Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"})
	ack := e.applyAmendRequest(req)
	if ack.Status != AckError || e.err == nil {
		t.Fatalf("export failure: %+v err=%v", ack, e.err)
	}
	if hashJSON(e.State.Backlog) != before {
		t.Fatal("the failed export kept the staged amendment")
	}
	if e.State.PlanSignature != "sig" {
		t.Fatalf("the signature was not restored: %q", e.State.PlanSignature)
	}
	if scope := e.State.Plan.Scope("T1"); hashJSON(scope) != hashJSON(beforeScope) {
		t.Fatalf("the dropped scope was not restored: %+v", scope)
	}
	rec := e.requestReceipt(req.ID)
	if rec == nil || rec.State != RequestFailed {
		t.Fatalf("failed receipt: %+v", rec)
	}
}

// Caller keys make amend retries return the recorded outcome, and a changed
// payload under the same key is a conflict (AX-012).
func TestAmendIdempotencyKey(t *testing.T) {
	e := amendEngine(t, []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	})
	first := amendRequest(e, Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"})
	first.IdempotencyKey = "amend-once"
	original := e.applyAmendRequest(first)
	if original.Status != AckApplied {
		t.Fatalf("keyed amend: %+v", original)
	}
	// A retry sends the payload the original sent: the revision the caller
	// read, not the post-amendment task.
	retry := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
		Session: e.controller.Session, TaskID: first.TaskID, TaskRevision: first.TaskRevision,
		Tasks: []Task{Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}}, IdempotencyKey: "amend-once"}
	ack := e.applyAmendRequest(retry)
	if ack.Status != AckApplied || !strings.Contains(ack.Message, original.Message) {
		t.Fatalf("replay: %+v", ack)
	}
	if len(e.State.Requests) < 2 {
		t.Fatalf("replay recorded nothing: %+v", e.State.Requests)
	}
	changed := amendRequest(e, Task{ID: "T2", Title: "two revised", Priority: 1, Status: "todo"})
	changed.IdempotencyKey = "amend-once"
	if ack = e.applyAmendRequest(changed); ack.Status != AckConflict {
		t.Fatalf("changed payload under the key: %+v", ack)
	}
	if e.State.Backlog.Task("T2").Title != "two" {
		t.Fatal("the conflicting retry mutated the backlog")
	}
}

// A hand-written envelope cannot smuggle what the CLI refuses: the engine
// re-checks the payload shape itself.
func TestAmendEnvelopeShape(t *testing.T) {
	e := amendEngine(t, []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}})
	tasks := func(ts ...Task) []Task { return ts }
	shapes := []struct {
		name string
		req  *SettingsRequest
	}{
		{"no tasks", &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
			Session: e.controller.Session, TaskID: "T1"}},
		{"two tasks", &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
			Session: e.controller.Session, TaskID: "T1", Tasks: tasks(
				Task{ID: "T1", Title: "a", Priority: 1, Status: "todo"}, Task{ID: "T1", Title: "b", Priority: 1, Status: "todo"})}},
		{"no task id", &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("amend"), Type: RequestTypeAmend,
			Session: e.controller.Session, Tasks: tasks(Task{ID: "T1", Title: "a", Priority: 1, Status: "todo"})}},
	}
	for _, shape := range shapes {
		if ack := e.applyAmendRequest(shape.req); ack.Status != AckError {
			t.Fatalf("%s: %+v", shape.name, ack)
		}
	}
}

// The public CLI path: the receipt returns at once naming the durable
// request, the controller decides the envelope, the record answers, an
// unknown task is a usage-side failure, and a bare amend is a usage error.
func TestAmendCLIReceiptAndQuery(t *testing.T) {
	c, dir := receiptProject(t, "amend")
	b := testBacklog()
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}}
	if err = store.Save(state); err != nil {
		t.Fatal(err)
	}
	amended := Task{ID: "T1", Title: testBacklog().Tasks[0].Title + " revised", Priority: 1, Status: testBacklog().Tasks[0].Status}
	amendPath := filepath.Join(dir, "amended.json")
	if err = writeJSON(amendPath, amended); err != nil {
		t.Fatal(err)
	}
	var out, stderr strings.Builder
	cc := v2Config(c, "amend", true)
	cc.AmendTask, cc.AmendFile = "T1", amendPath
	code := runAmend(cc, &out, &stderr)
	if code != exitOK {
		t.Fatalf("amend exit %d: %s%s", code, out.String(), stderr.String())
	}
	receipt := decodeReceipt(t, out.String(), stderr.String())
	if receipt.Request.Type != PublicRequestAmendTask || receipt.Request.State != RequestSubmitted {
		t.Fatalf("receipt: %+v", receipt.Request)
	}
	if receipt.Request.ControllerSession != "run-live" {
		t.Fatalf("receipt session: %+v", receipt.Request)
	}
	queryCode, doc, _ := runRequestsCLI(t, receipt.Request.ID, "--contract", "v2", "--json")
	if queryCode != exitOK {
		t.Fatalf("query exit %d", queryCode)
	}
	if doc.Request.Type != PublicRequestAmendTask || doc.Request.State != RequestSubmitted {
		t.Fatalf("record: %+v", doc.Request)
	}
	// The controller decides the queued envelope; the record answers.
	e := &Engine{Dir: dir, Output: io.Discard, Store: store, State: state, Config: c,
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1)}
	e.processAmendRequests()
	if state.Backlog.Task("T1").Title != amended.Title {
		t.Fatalf("queued envelope not applied: %+v", state.Backlog.Tasks)
	}
	// An unknown task is a usage-side failure with the fix in the message.
	missing := v2Config(c, "amend", true)
	missing.AmendTask, missing.AmendFile = "T404", amendPath
	out.Reset()
	stderr.Reset()
	if code = runAmend(missing, &out, &stderr); code != exitOperationFailed {
		t.Fatalf("unknown task exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "T404") {
		t.Fatalf("unknown task message: %s%s", out.String(), stderr.String())
	}
	// A status change is refused before anything is attempted.
	statusChange := v2Config(c, "amend", true)
	statusPath := filepath.Join(dir, "status-change.json")
	if err = writeJSON(statusPath, Task{ID: "T1", Title: amended.Title, Priority: 1, Status: "done"}); err != nil {
		t.Fatal(err)
	}
	statusChange.AmendTask, statusChange.AmendFile = "T1", statusPath
	out.Reset()
	stderr.Reset()
	if code = runAmend(statusChange, &out, &stderr); code != exitUsage {
		t.Fatalf("status change exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "status") {
		t.Fatalf("status change message: %s%s", out.String(), stderr.String())
	}
	// No task argument is a usage error.
	out.Reset()
	stderr.Reset()
	if code = runAmend(v2Config(c, "amend", true), &out, &stderr); code != exitUsage {
		t.Fatalf("bare amend exit %d: %s%s", code, out.String(), stderr.String())
	}
}

// --wait against a live engine reports the request record: applied for a
// decided amendment.
func TestAmendWaitReportsRecord(t *testing.T) {
	c, dir := receiptProject(t, "amend")
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T1", Title: "one", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{Dir: dir, Output: io.Discard, Store: store, Config: c,
		State:      &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(data), Attempts: []*Attempt{}},
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1),
	}
	if err = store.Save(e.State); err != nil {
		t.Fatal(err)
	}
	amendPath := filepath.Join(dir, "amended.json")
	if err = writeJSON(amendPath, Task{ID: "T1", Title: "one revised", Priority: 1, Status: "todo"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 250; i++ {
			e.processAmendRequests()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { <-done }()
	cc := v2Config(c, "amend", true)
	cc.AmendTask, cc.AmendFile, cc.Wait, cc.WaitTimeout = "T1", amendPath, true, 30*time.Second
	var out, stderr strings.Builder
	if code := runAmend(cc, &out, &stderr); code != exitOK {
		t.Fatalf("amend --wait exit %d: %s%s", code, out.String(), stderr.String())
	}
	var doc requestRecordDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one record document: %v\n%s", err, out.String())
	}
	if doc.Kind != requestRecordKind || doc.Request.State != RequestApplied {
		t.Fatalf("record: %+v", doc.Request)
	}
}

// The option surface: amend accepts the mutation wait and key options, one
// task id and one file, and the capabilities document lists the new command
// and request type.
func TestAmendOptionParsing(t *testing.T) {
	c, err := Parse([]string{"amend", "T1", "task.json", "--wait", "--idempotency-key", "k1"})
	if err != nil {
		t.Fatalf("amend options: %v", err)
	}
	if !c.Wait || c.IdempotencyKey != "k1" || c.AmendTask != "T1" || c.AmendFile != "task.json" || c.Command != "amend" {
		t.Fatalf("parsed: %+v", c)
	}
	if _, err = Parse([]string{"amend", "T1", "a.json", "b.json"}); err == nil {
		t.Fatal("amend accepted three positionals")
	}
	if _, err = Parse([]string{"status", "--wait"}); err == nil {
		t.Fatal("--wait parsed outside the mutation commands")
	}
	if _, err = Parse([]string{"amend", "T1", "task.json", "--cascade"}); err == nil {
		t.Fatal("--cascade parsed outside cancel")
	}
	// The command and its request type are part of the advertised surface.
	found := false
	for _, name := range clientCommands {
		if name == "amend" {
			found = true
		}
	}
	if !found {
		t.Fatal("the capabilities command list lost amend")
	}
	found = false
	for _, typ := range controllerRequestTypes() {
		if typ == PublicRequestAmendTask {
			found = true
		}
	}
	if !found {
		t.Fatal("controllers do not advertise amend-task requests")
	}
}
