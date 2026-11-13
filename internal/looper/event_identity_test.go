package looper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// identityOf decodes the structured identity an event recorded. Absent
// fields decode to their zero values, exactly as a historical record
// without them reads.
func identityOf(t *testing.T, ev Event) eventIdentity {
	t.Helper()
	var id eventIdentity
	if len(ev.Data) == 0 || string(ev.Data) == "null" {
		return id
	}
	if err := json.Unmarshal(ev.Data, &id); err != nil {
		t.Fatalf("event %d (%s) payload is not an identity document: %v (%s)", ev.ID, ev.Kind, err, ev.Data)
	}
	return id
}

// kindsOf lists the kinds a page's items cover.
func kindsOf(items []Event) map[string]bool {
	kinds := map[string]bool{}
	for _, ev := range items {
		kinds[ev.Kind] = true
	}
	return kinds
}

// TestTaskFilteredQueriesRetrieveLifecycleEvents runs the whole pipeline for
// one task and covers the AX-007 acceptance: a task-filtered query retrieves
// the task's start, check, and finish events with their structured
// identities, session-level events stay out of the filter, and the messages
// keep their human text.
func TestTaskFilteredQueriesRetrieveLifecycleEvents(t *testing.T) {
	// The harness changes the working directory for its repository; read the
	// published schema first, while the source tree is still reachable.
	schema, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "v2", "events-page.schema.json"))
	if err != nil {
		t.Skipf("events-page schema is absent: %v", err)
	}
	e, c, _ := assessRunEngine(t, "", 0)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 0 || err != nil {
		t.Fatalf("run: %d %v", code, err)
	}
	attempt := ""
	for _, a := range e.State.Attempts {
		if a.Task.ID == "T1" {
			attempt = a.ID
		}
	}
	if attempt == "" {
		t.Fatal("the run recorded no attempt for T1")
	}
	s, err := OpenStoreReadonly(filepath.Join(mustDir(t, c), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.EventPage(EventQuery{Task: "T1", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	wantPhase := map[string]string{"start": "running", "check": "verifying", "result": "done"}
	messages := map[string]string{
		"start":  "T1  one",
		"check":  "Checking T1 with integrated work...",
		"result": "T1  done | implemented",
	}
	for _, ev := range page.Items {
		id := identityOf(t, ev)
		if id.TaskID != "T1" {
			t.Fatalf("event %d (%s) matched the filter with task %q", ev.ID, ev.Kind, id.TaskID)
		}
		if id.AttemptID != attempt {
			t.Fatalf("event %d (%s) names attempt %q, want %s", ev.ID, ev.Kind, id.AttemptID, attempt)
		}
		if id.Session != e.controller.Session {
			t.Fatalf("event %d (%s) names session %q, want %s", ev.ID, ev.Kind, id.Session, e.controller.Session)
		}
		if phase, ok := wantPhase[ev.Kind]; ok {
			if id.Phase != phase {
				t.Fatalf("event %d (%s) records phase %q, want %q", ev.ID, ev.Kind, id.Phase, phase)
			}
			if ev.Message != messages[ev.Kind] {
				t.Fatalf("event %d (%s) message changed: %q", ev.ID, ev.Kind, ev.Message)
			}
		}
	}
	for kind := range wantPhase {
		if !kindsOf(page.Items)[kind] {
			t.Fatalf("task-filtered query missing the %s event; got kinds %v", kind, kindsOf(page.Items))
		}
	}
	// Session-level events stay readable without matching the task filter:
	// the run banner names no task, and the finish event closes the run.
	all, err := s.EventPage(EventQuery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	banner, finish := false, false
	for _, ev := range all.Items {
		id := identityOf(t, ev)
		if ev.Kind == "start" && strings.Contains(ev.Message, "Looper | up to") {
			banner = true
			if id.TaskID != "" || id.Session != e.controller.Session {
				t.Fatalf("run banner identity wrong: %+v", id)
			}
		}
		if ev.Kind == "finish" {
			finish = true
			if id.TaskID != "" || id.Session != e.controller.Session {
				t.Fatalf("finish identity wrong: %+v", id)
			}
		}
		if id.Session == "" {
			t.Fatalf("event %d (%s) records no controller session", ev.ID, ev.Kind)
		}
	}
	if !banner || !finish {
		t.Fatalf("run-level events missing: banner %t finish %t", banner, finish)
	}
	// The version 2 finite query surfaces the same identities and validates
	// against its published schema.
	cc := c
	cc.Command, cc.JSON, cc.Contract, cc.EventTask = "events", true, ContractVersionV2, "T1"
	doc := runEventsCommand(t, cc)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(schema, raw); err != nil {
		t.Fatalf("produced page violates its schema: %v\n%s", err, raw)
	}
	kinds := map[string]bool{}
	for _, item := range doc.Items {
		if item.TaskID != "T1" || item.ControllerSession != e.controller.Session || item.AttemptID != attempt {
			t.Fatalf("surfaced identity wrong: %+v", item)
		}
		kinds[item.Kind] = true
	}
	for kind := range wantPhase {
		if !kinds[kind] {
			t.Fatalf("finite query missing the %s event for T1: %v", kind, kinds)
		}
	}
}

// TestRepairDiagnosisAndFailureEventsCarryTaskIdentity covers the failure
// side of the AX-007 acceptance: repair, diagnosis, and the blocked result
// with its blockers all carry the task identity a filter matches.
func TestRepairDiagnosisAndFailureEventsCarryTaskIdentity(t *testing.T) {
	summary := `{"task_id":"T001","status":"blocked","summary":"held","files":[],"blockers":["waiting"],"followups":[],"unblock_when":"retry"}`
	e, a := diagnosisEngine(t, summary)
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", diagnosisProposal)
	e.State.Plan = &Plan{}
	feedback := "check failed: T001.txt missing"
	a.FailureHash = digest([]byte(feedback))
	a.Repeats = 1
	results := make(chan workerResult, 1)
	if err := e.repair(context.Background(), a, feedback, results); err != nil {
		t.Fatal(err)
	}
	r := <-results
	e.wg.Wait()
	pending := []*Attempt{}
	if err := e.applyFinishedWorker(context.Background(), r, &pending); err != nil {
		t.Fatal(err)
	}
	page, err := e.Store.EventPage(EventQuery{Task: "T001", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"start", "repair", "diagnosis", "result", "blocker"} {
		if !kindsOf(page.Items)[kind] {
			t.Fatalf("task-filtered query missing the %s event; got kinds %v", kind, kindsOf(page.Items))
		}
	}
	phases := map[string]string{"repair": "running", "result": "blocked"}
	for _, ev := range page.Items {
		id := identityOf(t, ev)
		if id.TaskID != "T001" || id.AttemptID != a.ID || id.Session != e.controller.Session {
			t.Fatalf("event %d (%s) identity wrong: %+v", ev.ID, ev.Kind, id)
		}
		if phase, ok := phases[ev.Kind]; ok && id.Phase != phase {
			t.Fatalf("event %d (%s) records phase %q, want %q", ev.ID, ev.Kind, id.Phase, phase)
		}
		switch ev.Kind {
		case "repair":
			if ev.Message != "Repairing T001 with the recorded failure evidence." {
				t.Fatalf("repair message changed: %q", ev.Message)
			}
		case "result":
			if ev.Message != "T001  blocked | held" {
				t.Fatalf("result message changed: %q", ev.Message)
			}
		}
	}
}

// TestRequestOutcomeEventsCorrelateWithReceipts covers the request side of
// the AX-007 acceptance: add-task, requeue, and settings outcomes carry the
// request identity their acknowledgements answer under.
func TestRequestOutcomeEventsCorrelateWithReceipts(t *testing.T) {
	e := requeueEngine(t)
	// Requeue one task through the durable request path.
	req := requeueRequest(e)
	if ack := e.applyRequeueRequest(req); ack.Status != AckApplied {
		t.Fatalf("requeue: %+v", ack)
	}
	// Add one task through the claim loop.
	add := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("add"), Type: RequestTypeAddTask, Session: e.controller.Session, Tasks: []Task{
		{ID: "T2", Title: "New work", Priority: 2},
	}}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), add.ID+".json"), add); err != nil {
		t.Fatal(err)
	}
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	var addAck SettingsAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), add.ID+".json"), &addAck); err != nil || addAck.Status != AckApplied {
		t.Fatalf("add-task ack: %+v %v", addAck, err)
	}
	// A settings request that changes no fields still reports its outcome.
	settings := &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("settings"), Type: RequestTypeSettings, Session: e.controller.Session}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), settings.ID+".json"), settings); err != nil {
		t.Fatal(err)
	}
	e.processSettingsRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	if _, err := os.Stat(filepath.Join(ackDir(e.Dir), settings.ID+".json")); err != nil {
		t.Fatalf("settings ack missing: %v", err)
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]eventIdentity{}
	for _, ev := range events {
		switch ev.Kind {
		case "requeue", "add-task", "settings":
			outcomes[ev.Kind] = identityOf(t, ev)
		}
	}
	if id := outcomes["requeue"]; id.RequestID != req.ID || id.TaskID != "T1" || id.Session != e.controller.Session {
		t.Fatalf("requeue outcome identity wrong: %+v", id)
	}
	if id := outcomes["add-task"]; id.RequestID != add.ID || id.TaskID != "" || id.Session != e.controller.Session {
		t.Fatalf("add-task outcome identity wrong: %+v", id)
	}
	var added struct {
		Tasks []string `json:"tasks"`
	}
	for _, ev := range events {
		if ev.Kind == "add-task" && json.Unmarshal(ev.Data, &added) == nil {
			if len(added.Tasks) != 1 || added.Tasks[0] != "T2" {
				t.Fatalf("add-task outcome must list the added task, got %v", added.Tasks)
			}
		}
	}
	if id := outcomes["settings"]; id.RequestID != settings.ID || id.Session != e.controller.Session {
		t.Fatalf("settings outcome identity wrong: %+v", id)
	}
}

// TestImportOutcomeEventsCorrelateWithReceipts covers the import side of the
// AX-007 acceptance: queued and applied import events carry the request
// identity their receipts own, and an applied import lists its task
// mutations beside it.
func TestImportOutcomeEventsCorrelateWithReceipts(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "# New requirements\nAdd telemetry to the service.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record := e.State.Imports[0]
	if record.Status != ImportApplied {
		t.Fatalf("import status %s: %s", record.Status, record.Error)
	}
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	queued, applied := false, false
	for _, ev := range events {
		if ev.Kind != "import" {
			continue
		}
		id := identityOf(t, ev)
		if id.RequestID != record.RequestID || id.Session != e.controller.Session {
			t.Fatalf("import event %d identity wrong: %+v", ev.ID, id)
		}
		var payload struct {
			Tasks []string `json:"tasks"`
		}
		_ = json.Unmarshal(ev.Data, &payload)
		if strings.Contains(ev.Message, "Queued startup plan import") {
			queued = true
		}
		if strings.Contains(ev.Message, "Applied plan import") {
			applied = true
			if len(payload.Tasks) != 1 || payload.Tasks[0] != "T002" {
				t.Fatalf("applied import must list its task mutations, got %v", payload.Tasks)
			}
		}
	}
	if !queued || !applied {
		t.Fatalf("import outcomes missing: queued %t applied %t", queued, applied)
	}
	var ack planImportAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), record.RequestID+".json"), &ack); err != nil || ack.ID != record.RequestID {
		t.Fatalf("receipt does not correlate: %+v %v", ack, err)
	}
}

// TestFindingEventsCarryTaskIdentity covers findings as task mutations: an
// added follow-up and an updated task carry the mutated task identity, and a
// rejected report carries the session only.
func TestFindingEventsCarryTaskIdentity(t *testing.T) {
	e := requeueEngine(t)
	stage := e.beginFindingStage()
	stage.add(Finding{Title: "", Details: "incomplete", Evidence: "none"})
	stage.add(Finding{Title: "Cover logging", Details: "Add structured logs.", Evidence: "No log calls found.", DependsOn: []string{}, Priority: 1})
	stage.add(Finding{Title: "Repair the blocker", Details: "Recheck after install.", Evidence: "Retained attempt.", DependsOn: []string{}, Priority: 1, ExistingTaskID: "T1"})
	stage.emit()
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	identities := map[string]eventIdentity{}
	for _, ev := range events {
		if ev.Kind != "finding" {
			continue
		}
		switch {
		case strings.Contains(ev.Message, "Ignored follow-up"):
			identities["rejected"] = identityOf(t, ev)
		case strings.Contains(ev.Message, "Added follow-up"):
			identities["added"] = identityOf(t, ev)
		case strings.Contains(ev.Message, "Updated existing finding"):
			identities["updated"] = identityOf(t, ev)
		}
	}
	if len(identities) != 3 {
		t.Fatalf("finding outcomes missing: %v", identities)
	}
	if id := identities["added"]; id.TaskID != "T002" || id.Session != e.controller.Session {
		t.Fatalf("added follow-up identity wrong: %+v", id)
	}
	if id := identities["updated"]; id.TaskID != "T1" || id.Session != e.controller.Session {
		t.Fatalf("updated finding identity wrong: %+v", id)
	}
	if id := identities["rejected"]; id.TaskID != "" || id.Session != e.controller.Session {
		t.Fatalf("rejected report must carry the session only: %+v", id)
	}
}

// TestHistoricalEventsStayReadableWithoutIdentity covers the migration rule:
// records written before structured identities exist keep their identifier,
// kind, and message, surface no identity, and stay out of task filters that
// cannot honestly match them.
func TestHistoricalEventsStayReadableWithoutIdentity(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	s, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	legacy := []struct{ kind, message string }{
		{"start", "T015  old work"},
		{"result", "T015  done | finished by an older controller"},
		{"progress", "1 implementing"},
	}
	for _, l := range legacy {
		if _, err = s.Event(l.kind, l.message, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Event("check", "Checking T015 with integrated work...", map[string]string{
		"task_id": "T015", "attempt_id": "task-new", "session": "run-2", "phase": "verifying",
	}); err != nil {
		t.Fatal(err)
	}
	page, err := s.EventPage(EventQuery{Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 4 {
		t.Fatalf("historical records must stay readable, got %d items", len(page.Items))
	}
	for i, want := range []string{"T015  old work", "T015  done | finished by an older controller", "1 implementing", "Checking T015 with integrated work..."} {
		if page.Items[i].Message != want {
			t.Fatalf("item %d message changed: %q", i, page.Items[i].Message)
		}
	}
	for i := 0; i < 3; i++ {
		// A record written without a payload reads back with no data at all;
		// one written with an unrelated payload keeps it untouched.
		var id eventIdentity
		if len(page.Items[i].Data) > 0 {
			if err := json.Unmarshal(page.Items[i].Data, &id); err != nil {
				t.Fatalf("historical record %d payload unreadable: %v", i, err)
			}
		}
		if id != (eventIdentity{}) {
			t.Fatalf("historical record %d surfaces an identity it never wrote: %+v", i, id)
		}
	}
	filtered, err := s.EventPage(EventQuery{Task: "T015", Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Items) != 1 || filtered.Items[0].Kind != "check" {
		t.Fatalf("task filter must match only the structured record, got %d items", len(filtered.Items))
	}
	// The finite contract surfaces the same distinction.
	doc := runEventsCommand(t, c)
	if len(doc.Items) != 4 {
		t.Fatalf("plain full query returned %d items", len(doc.Items))
	}
	for _, item := range doc.Items[:3] {
		if item.TaskID != "" || item.ControllerSession != "" {
			t.Fatalf("historical item surfaces identity: %+v", item)
		}
	}
}

// TestSessionEventsCarryControllerSession keeps the identity contract honest
// for controller-level events that no task or request owns: an engine event
// names its session and nothing else.
func TestSessionEventsCarryControllerSession(t *testing.T) {
	e := requeueEngine(t)
	e.event("waiting", "Waiting for changed requirements or resolved blockers.")
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range events {
		if ev.Kind != "waiting" {
			continue
		}
		found = true
		if id := identityOf(t, ev); id != (eventIdentity{Session: e.controller.Session}) {
			t.Fatalf("session-level identity wrong: %+v", id)
		}
	}
	if !found {
		t.Fatal("waiting event missing")
	}
}
