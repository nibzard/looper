package looper

// The durable request lifecycle (AX-009). One receipt store covers add-task,
// add-plan, settings, requeue, and stop; crash injection at every transport
// boundary — envelope, claim, acceptance, mutation, export, receipt, and
// cleanup — leaves a queryable outcome with no silent deletion and no false
// success.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRequestStateRegistryMatchesContract keeps the lifecycle states the
// engine writes identical to the registry the request-record contract
// enumerates: the same names, the same terminal flags.
func TestRequestStateRegistryMatchesContract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "contracts", "request-states.json"))
	if err != nil {
		t.Fatalf("request state registry is absent: %v", err)
	}
	var doc struct {
		States []struct {
			Name     string `json:"name"`
			Terminal bool   `json:"terminal"`
		} `json:"states"`
	}
	if err = json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.States) != len(requestStateRegistry) {
		t.Fatalf("the registry lists %d states, the engine knows %d", len(doc.States), len(requestStateRegistry))
	}
	for _, st := range doc.States {
		terminal, listed := requestStateRegistry[st.Name]
		if !listed {
			t.Errorf("the engine does not know request state %q", st.Name)
			continue
		}
		if terminal != st.Terminal {
			t.Errorf("request state %q terminal=%v in the engine, %v in the registry", st.Name, terminal, st.Terminal)
		}
	}
}

// addTaskRequest builds one claimed add-task request for the protocol engine.
func addTaskRequest(e *Engine, id string, tasks ...Task) *SettingsRequest {
	return &SettingsRequest{
		Version: SettingsRequestVersion, ID: id, Type: RequestTypeAddTask,
		Session: e.controllerSessionID(), Tasks: tasks,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
}

func crashTask() Task {
	return Task{ID: "T009", Title: "crash boundary", Priority: 1, Status: "todo", Details: "Acceptance: appears exactly once."}
}

// A failed export must leave neither an in-memory-only task nor a staged
// receipt: the rolled-back backlog and the failed receipt persist together
// before the poisoned engine exits, whatever the export's own journaling
// save had already written. This pins the add-task fold of the
// stagedBacklogRequest pattern the review repair names; the write failure
// after the journaling save is the same injection the import fixture uses.
func TestAddTaskExportFailureStagesReceiptAndBacklog(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not stop the root user")
	}
	for _, scenario := range []string{"first-export", "pending-journal"} {
		t.Run(scenario, func(t *testing.T) {
			e, original := exportTestEngine(t)
			// A successful export records the accepted bytes first.
			if err := e.export(); err != nil {
				t.Fatal(err)
			}
			if scenario == "pending-journal" {
				// An earlier export left its journal: the failing export below
				// saves nothing on its own, so the rollback persist is the
				// only thing that can record the outcome.
				e.State.ExportPending = true
				e.State.ExportBefore = digest(original)
			}
			// The task-file write fails after the journaling save, as an
			// unwritable project directory makes it.
			if err := os.Chmod(e.Config.Root, 0555); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(e.Config.Root, 0755)
			req := addTaskRequest(e, "add-crash", crashTask())
			ack := e.applyAddTaskRequest(req)
			if err := os.Chmod(e.Config.Root, 0755); err != nil {
				t.Fatal(err)
			}
			if ack.Status != AckError || ack.Message == "" {
				t.Fatalf("the failed export must answer with an error: %+v", ack)
			}
			if e.err == nil {
				t.Fatal("the export failure must poison the engine")
			}
			if task := e.State.Backlog.Task("T009"); task != nil {
				t.Fatalf("failed export kept an in-memory-only task: %+v", task)
			}
			for _, event := range journal(t, e) {
				if strings.Contains(event.Message, "Added 1 task(s)") {
					t.Fatalf("uncommitted add-task reported success: %+v", event)
				}
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if saved == nil {
				t.Fatal("the rollback never persisted")
			}
			if task := saved.Backlog.Task("T009"); task != nil {
				t.Fatalf("failed export persisted a staged task: %+v", task)
			}
			state, receipt := requestLifecycleState(e.Dir, saved, "add-crash")
			if state != RequestFailed || receipt == nil || receipt.Error == "" {
				t.Fatalf("receipt after failed export: state %q, %+v", state, receipt)
			}
			if receipt.Ack == nil || receipt.Ack.Status != AckError {
				t.Fatalf("the receipt must record the honest error ack: %+v", receipt.Ack)
			}
			// The recorded outcome never applies later: a replay of the same
			// identifier returns the recorded answer and adds no task.
			e2 := protocolEngineOn(t, e.Dir, e.controllerSessionID())
			e2.State = saved
			replayed := e2.applyAddTaskRequest(req)
			if replayed.Status != AckError {
				t.Fatalf("a decided request must replay its recorded outcome, got %+v", replayed)
			}
			if task := e2.State.Backlog.Task("T009"); task != nil {
				t.Fatalf("the replay applied the refused tasks: %+v", task)
			}
		})
	}
}

// A controller restart between the export commit and the acknowledgement
// write replays the recorded receipt instead of applying the tasks twice.
func TestAddTaskReceiptReplaysAfterRestart(t *testing.T) {
	e, _ := exportTestEngine(t)
	envelope := addTaskRequest(e, "add-replay", crashTask())
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "add-replay.json"), envelope); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := claimNextRequest(e.Dir, RequestTypeAddTask)
	if err != nil || claimed == nil || claimed.ID != "add-replay" {
		t.Fatalf("claim: %+v, %v", claimed, err)
	}
	req := claimed
	ack := e.applyAddTaskRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	// The crash: the acknowledgement and the claim removal never happened.
	if err := os.Remove(filepath.Join(ackDir(e.Dir), "add-replay.json")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	// The restart re-claims the stranded envelope and must return the
	// recorded answer with the tasks exactly once in the saved backlog. The
	// receipt answers before any session check, so the restarted controller
	// needs no session identity to replay it.
	e2 := protocolEngineOn(t, e.Dir, e.controllerSessionID())
	e2.State = saved
	if err = os.Rename(filepath.Join(processingDir(e.Dir), "add-replay.json"), filepath.Join(requestDir(e.Dir), "add-replay.json")); err != nil {
		t.Fatal(err)
	}
	e2.processAddTaskRequests()
	if e2.err != nil {
		t.Fatal(e2.err)
	}
	count := 0
	for _, task := range e2.State.Backlog.Tasks {
		if task.ID == "T009" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the replay applied the tasks %d times", count)
	}
	replayed, ok := readRequestAck(e2.Dir, "add-replay")
	if !ok || replayed.Status != AckApplied {
		t.Fatalf("the restart must answer from the receipt: %+v, %v", replayed, ok)
	}
	if state, _ := requestLifecycleState(e2.Dir, e2.State, "add-replay"); state != RequestApplied {
		t.Fatalf("receipt state after replay: %q", state)
	}
	if _, statErr := os.Stat(filepath.Join(processingDir(e.Dir), "add-replay.json")); !os.IsNotExist(statErr) {
		t.Fatal("the answered claim must be removed")
	}
}

// Settings decisions record receipts beside their acknowledgements, and a
// restart replays the recorded acknowledgement without re-saving the profile.
func TestSettingsReceiptSurvivesRestart(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	req := request(e, map[string]any{"agent": "claude"}, profileDigest(e.Config.ProfilePath))
	ack := e.applySettingsRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, receipt := requestLifecycleState(e.Dir, saved, req.ID)
	if state != RequestApplied || receipt == nil {
		t.Fatalf("applied settings must hold a receipt: %q, %+v", state, receipt)
	}
	if len(receipt.Fields) != 1 || receipt.Fields[0] != "agent" {
		t.Fatalf("the receipt must name the applied fields: %v", receipt.Fields)
	}
	// The restart replays the recorded acknowledgement; the profile keeps
	// the values the first controller saved.
	e2 := protocolEngineOn(t, e.Dir, e.controller.Session)
	e2.State = saved
	before := profileDigest(e.Config.ProfilePath)
	replayed := e2.applySettingsRequest(req)
	if replayed.Status != AckApplied {
		t.Fatalf("a decided settings request must replay its outcome: %+v", replayed)
	}
	if after := profileDigest(e.Config.ProfilePath); after != before {
		t.Fatal("the replay re-saved the profile")
	}
}

// A refused settings request records a rejected receipt, so the refusal
// stays queryable after the envelope and its acknowledgement are gone.
func TestRefusedSettingsRequestRecordsReceipt(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	req := request(e, map[string]any{"agent": "claude"}, profileDigest(e.Config.ProfilePath))
	req.Session = "run-earlier"
	ack := e.applySettingsRequest(req)
	if ack.Status != AckStale {
		t.Fatalf("status %s", ack.Status)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, receipt := requestLifecycleState(e.Dir, saved, req.ID)
	if state != RequestRejected || receipt == nil || receipt.Error == "" {
		t.Fatalf("a refused request must hold a rejected receipt: %q, %+v", state, receipt)
	}
}

// The startup sweep of envelopes whose submitters are gone records one
// rejected receipt per removal: the envelope and claim boundaries leave a
// queryable outcome instead of a silent deletion.
func TestSweptEnvelopeRecordsRejectedReceipt(t *testing.T) {
	for _, scenario := range []string{"requests", "processing"} {
		t.Run(scenario, func(t *testing.T) {
			e := protocolEngine(t)
			e.State = &State{Version: 2}
			req := &SettingsRequest{
				Version: SettingsRequestVersion, ID: "swept", Type: RequestTypeSettings,
				Session: "run-earlier", SubmitterPID: 999999,
			}
			base := requestDir(e.Dir)
			if scenario == "processing" {
				base = processingDir(e.Dir)
				if err := os.MkdirAll(base, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := writeJSON(filepath.Join(base, "swept.json"), req); err != nil {
				t.Fatal(err)
			}
			removed := clearSessionFiles(e.Dir)
			if len(removed) != 1 || removed[0].id != "swept" {
				t.Fatalf("the sweep must report its removals: %+v", removed)
			}
			e.recordRefusedRequests(removed)
			if e.err != nil {
				t.Fatal(e.err)
			}
			saved, err := e.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			state, receipt := requestLifecycleState(e.Dir, saved, "swept")
			if state != RequestRejected || receipt == nil || !strings.Contains(receipt.Error, "never applied") {
				t.Fatalf("a swept envelope must hold a rejected receipt: %q, %+v", state, receipt)
			}
		})
	}
}

// A live submitter's stranded claim returns to the queue and the next
// controller answers it; the sweep records nothing because the request was
// never removed.
func TestLiveSubmitterClaimIsNotSweptOrRecorded(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	req := &SettingsRequest{
		Version: SettingsRequestVersion, ID: "live", Type: RequestTypeSettings,
		Session: "run-test", SubmitterPID: os.Getpid(),
	}
	if identity, err := runIdentityCommand(os.Getpid()); err == nil {
		req.SubmitterIdentity = identity
	}
	if err := os.MkdirAll(processingDir(e.Dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(processingDir(e.Dir), "live.json"), req); err != nil {
		t.Fatal(err)
	}
	if removed := clearSessionFiles(e.Dir); len(removed) != 0 {
		t.Fatalf("a live submitter's claim must not be swept: %+v", removed)
	}
	if _, err := os.Stat(filepath.Join(requestDir(e.Dir), "live.json")); err != nil {
		t.Fatalf("the claim must return to the queue: %v", err)
	}
}

// Stop requests record receipts on both outcomes: the honored drain commits
// an applied receipt with the state that drains, and the discarded request
// for a gone controller commits a rejected one.
func TestStopRequestReceipts(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	// A request for a gone controller is discarded with a rejected receipt.
	if err := writeStopRequest(e.Dir, stopRequest{Session: "run-gone"}); err != nil {
		t.Fatal(err)
	}
	e.sweepStopRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, receipt := requestLifecycleState(e.Dir, saved, "stop-run-gone")
	if state != RequestRejected || receipt == nil || !strings.Contains(receipt.Error, "never delivered") {
		t.Fatalf("a discarded stop request must hold a rejected receipt: %q, %+v", state, receipt)
	}
	// A request for this controller drains with an applied receipt that
	// commits before the request file is removed.
	if err = writeStopRequest(e.Dir, stopRequest{Session: e.controller.Session}); err != nil {
		t.Fatal(err)
	}
	code, err := e.drainExit("Stopped after draining active work. Run looper to resume.")
	if code != 0 || err != nil {
		t.Fatalf("drain exit %d: %v", code, err)
	}
	saved, err = e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, receipt = requestLifecycleState(e.Dir, saved, "stop-"+e.controller.Session)
	if state != RequestApplied || receipt == nil {
		t.Fatalf("an honored drain must hold an applied receipt: %q, %+v", state, receipt)
	}
	if _, statErr := os.Stat(stopRequestPath(e.Dir)); !os.IsNotExist(statErr) {
		t.Fatal("the honored stop request must be removed")
	}
}

// The export guard a blocked journal raises names the expected digest, the
// retained accepted bytes, and the restore that replays the journal — the
// recovery guidance the manual AX-002 reproduction asked for. Restoring the
// retained bytes replays; the export then completes.
func TestBlockedExportJournalNamesAcceptedBytesRecovery(t *testing.T) {
	e, original := exportTestEngine(t)
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	accepted := acceptedExportPath(e.Dir)
	if data, readErr := os.ReadFile(accepted); readErr != nil || digest(data) != digest(original) {
		t.Fatalf("the accepted-byte copy is wrong: %v", readErr)
	}
	edited := append([]byte(nil), original...)
	edited = append(edited, "  // external edit\n"...)
	if err := os.WriteFile(e.Config.Todo, edited, 0600); err != nil {
		t.Fatal(err)
	}
	e.State.ExportPending = true
	e.State.ExportBefore = digest(original)
	err := e.export()
	var guard *retentionError
	if !errors.As(err, &guard) {
		t.Fatalf("expected the export guard, got %v", err)
	}
	if !strings.Contains(guard.message, "changed outside Looper") {
		t.Fatalf("the guard message lost its cause: %q", guard.message)
	}
	// The legacy one-line text stays frozen; the structured document carries
	// the expected digest and the restore step.
	doc := retentionErrorDoc(guard)
	if doc.Expected["task_file_digest"] != digest(original) {
		t.Fatalf("the guard document must name the expected digest: %v", doc.Expected)
	}
	found := false
	for _, action := range doc.Actions {
		if action.Command == fmt.Sprintf("cp %s %s", accepted, e.Config.Todo) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the guard document must carry the restore command: %+v", doc.Actions)
	}
	// The v2 plain rendering names both the digest and the restore command.
	text := doc.plain()
	if !strings.Contains(text, digest(original)) || !strings.Contains(text, accepted) {
		t.Fatalf("the plain rendering must name the digest and the retained bytes: %q", text)
	}
	// The restore replays the pending journal exactly as the guidance says.
	if err := os.WriteFile(e.Config.Todo, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.export(); err != nil {
		t.Fatalf("the restored bytes must replay the journal: %v", err)
	}
	if e.State.ExportPending {
		t.Fatal("the replayed journal never cleared")
	}
}

// A formatting-only restore replays the journal (AX-018 equivalence), while
// a restore that changed content is refused with the same guidance. This
// pins why the guard names the retained bytes: content equality, not byte
// equality, decides, and the retained copy always satisfies it.
func TestBlockedExportJournalEquivalenceRules(t *testing.T) {
	e, original := exportTestEngine(t)
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	var backlog Backlog
	if err := json.Unmarshal(original, &backlog); err != nil {
		t.Fatal(err)
	}
	// Same content, different serialization: compact JSON with no newline.
	rewritten, err := json.Marshal(backlog)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.Config.Todo, rewritten, 0600); err != nil {
		t.Fatal(err)
	}
	// The staged mutation the journal replays over the accepted bytes.
	next := e.State.Backlog
	next.Tasks = append(append([]Task{}, next.Tasks...), crashTask())
	e.State.Backlog = next
	e.State.ExportPending = true
	e.State.ExportBefore = digest(original)
	if err = e.export(); err != nil {
		t.Fatalf("a formatting-only restore must replay the journal: %v", err)
	}
	// A restore that changed content is refused and names the recovery.
	e.State.Backlog = next
	e.State.ExportPending = true
	e.State.ExportBefore = digest(original)
	changed := append([]byte(nil), rewritten...)
	changed = append(changed, "  // external edit\n"...)
	if err = os.WriteFile(e.Config.Todo, changed, 0600); err != nil {
		t.Fatal(err)
	}
	var guard *retentionError
	if err = e.export(); !errors.As(err, &guard) {
		t.Fatalf("a content-changing restore must hit the export guard, got %v", err)
	}
	doc := retentionErrorDoc(guard)
	found := false
	for _, action := range doc.Actions {
		if action.Command == fmt.Sprintf("cp %s %s", acceptedExportPath(e.Dir), e.Config.Todo) {
			found = true
		}
	}
	if !found {
		t.Fatalf("the guard document must carry the restore command: %+v", doc.Actions)
	}
}

// The shed acknowledgement speaks the shed envelope's own transport, so a
// plan-import submitter reads its reason instead of an empty error string.
func TestShedPlanImportAcknowledgesInOwnTransport(t *testing.T) {
	dir := t.TempDir()
	queueEntries(t, dir, 100)
	if err := writeJSON(filepath.Join(requestDir(dir), "zz-import.json"), planImportRequest{
		Version: SettingsRequestVersion, ID: "zz-import", Type: RequestTypePlanImport,
		Session: "run-test", Paths: []string{"plan.md"},
	}); err != nil {
		t.Fatal(err)
	}
	shed, cleaned, over := sweepRequestQueue(dir, "run-test")
	if len(shed) != 1 || cleaned != 0 || over != 0 {
		t.Fatalf("sweep shed %d, cleaned %d, over %d", len(shed), cleaned, over)
	}
	var ack planImportAck
	if err := readJSON(filepath.Join(ackDir(dir), "zz-import.json"), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Status != ImportFailed || ack.Error == "" || ack.Type != RequestTypePlanImport {
		t.Fatalf("the shed plan import must read its reason: %+v", ack)
	}
	// The refusal receipt names the shed request's type.
	e := protocolEngineOn(t, dir, "run-test")
	e.State = &State{Version: 2}
	e.recordRefusedRequests(shed)
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state, receipt := requestLifecycleState(dir, saved, "zz-import"); state != RequestRejected || receipt == nil || receipt.Type != PublicRequestAddPlan {
		t.Fatalf("shed receipt: %q, %+v", state, receipt)
	}
}

// Terminal receipts prune to the retention bound from the oldest; every
// non-terminal receipt stays whatever the count.
func TestRequestReceiptRetentionBound(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	for i := 0; i < maxRetainedRequestReceipts+5; i++ {
		e.upsertRequest(&RequestRecord{ID: fmt.Sprintf("done-%03d", i), Type: PublicRequestAddTask, State: RequestApplied})
	}
	e.upsertRequest(&RequestRecord{ID: "waiting-forever", Type: PublicRequestAddPlan, State: RequestWaiting})
	if len(e.State.Requests) != maxRetainedRequestReceipts+1 {
		t.Fatalf("retained %d receipts, want %d", len(e.State.Requests), maxRetainedRequestReceipts+1)
	}
	if receipt := e.requestReceipt("done-000"); receipt != nil {
		t.Fatal("the oldest terminal receipt must prune")
	}
	if receipt := e.requestReceipt("done-004"); receipt != nil {
		t.Fatal("a receipt outside the bound must prune")
	}
	if receipt := e.requestReceipt("done-005"); receipt == nil {
		t.Fatal("a receipt inside the bound must stay")
	}
	if receipt := e.requestReceipt("waiting-forever"); receipt == nil {
		t.Fatal("a non-terminal receipt never prunes")
	}
}

// The import lifecycle mirrors into the common receipts with its wait
// reasons, so an import's outcome stays queryable beside every other
// request type's.
func TestImportReceiptsMirrorLifecycle(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "Add telemetry")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	id := e.State.Imports[0].RequestID
	if state, receipt := requestLifecycleState(e.Dir, e.State, id); state != RequestAccepted || receipt == nil || receipt.Type != PublicRequestAddPlan {
		t.Fatalf("queued import receipt: %q, %+v", state, receipt)
	}
	// An unfinished attempt holds the import at its boundary; the receipt
	// names the prerequisite while it waits.
	e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "active", Phase: "running"})
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	state, receipt := requestLifecycleState(e.Dir, e.State, id)
	if state != RequestWaiting || receipt == nil || receipt.WaitReason != "active_work" {
		t.Fatalf("waiting import receipt: %q, %+v", state, receipt)
	}
	e.State.Attempts = nil
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	state, receipt = requestLifecycleState(e.Dir, e.State, id)
	if state != RequestApplied || receipt == nil || len(receipt.AffectedTasks) != 1 {
		t.Fatalf("applied import receipt: %q, %+v", state, receipt)
	}
	if receipt.Summary == "" || !strings.Contains(receipt.Summary, "task(s) added") {
		t.Fatalf("the applied receipt must summarize its outcome: %q", receipt.Summary)
	}
	// The receipt survives the restart with the import record.
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state, _ := requestLifecycleState(e.Dir, saved, id); state != RequestApplied {
		t.Fatalf("the applied receipt must survive restart, got %q", state)
	}
}

// A refused import records a rejected receipt under its requesting
// identifier; a digest-deduplicated replay records the deduplicated outcome.
func TestImportRefusalAndDedupReceipts(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "Add telemetry")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	applied := e.State.Imports[0]
	// A second import of the same content deduplicates and records its own
	// outcome under the new identifier.
	dup := planFile(t, "Add telemetry")
	e.acceptPlanImport(&SettingsRequest{ID: "import-dedup", Paths: []string{dup}}, false)
	if e.err != nil {
		t.Fatal(e.err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, receipt := requestLifecycleState(e.Dir, saved, "import-dedup")
	if state != RequestApplied || receipt == nil || !strings.Contains(receipt.Summary, applied.RequestID) {
		t.Fatalf("dedup receipt: %q, %+v", state, receipt)
	}
	// A stale-session import is refused with a rejected receipt.
	e.acceptPlanImport(&SettingsRequest{ID: "import-stale", Paths: []string{dup}}, true)
	if e.err != nil {
		t.Fatal(e.err)
	}
	saved, err = e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state, receipt := requestLifecycleState(e.Dir, saved, "import-stale"); state != RequestRejected || receipt == nil || receipt.Error == "" {
		t.Fatalf("stale import receipt: %q, %+v", state, receipt)
	}
}

// TestRequestCrashHelper stops at the durable crash boundaries of one
// add-task request. The parent kills the process, so neither deferred
// cleanup nor a graceful shutdown can run. It ends itself when its parent is
// gone, exactly like the plan-import helper.
func TestRequestCrashHelper(t *testing.T) {
	raw := os.Getenv("LOOPER_REQUEST_CRASH_CONFIG")
	if raw == "" {
		return
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	mode := os.Getenv("LOOPER_REQUEST_CRASH_MODE")
	req := SettingsRequest{
		Version: SettingsRequestVersion, ID: "settings-crash", Type: RequestTypeAddTask,
		Session:   e.controller.Session,
		Tasks:     []Task{crashTask()},
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	req.SubmitterPID = os.Getpid()
	if identity, idErr := runIdentityCommand(os.Getpid()); idErr == nil {
		req.SubmitterIdentity = identity
	}
	if err = writeJSON(filepath.Join(requestDir(e.Dir), "settings-crash.json"), req); err != nil {
		t.Fatal(err)
	}
	switch mode {
	case "envelope":
		// The envelope is durable; nothing was claimed.
	case "claim":
		if err = os.MkdirAll(processingDir(e.Dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(filepath.Join(requestDir(e.Dir), "settings-crash.json"), filepath.Join(processingDir(e.Dir), "settings-crash.json")); err != nil {
			t.Fatal(err)
		}
	case "applied", "receipt", "cleanup", "export-write":
		if err = os.MkdirAll(processingDir(e.Dir), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Rename(filepath.Join(requestDir(e.Dir), "settings-crash.json"), filepath.Join(processingDir(e.Dir), "settings-crash.json")); err != nil {
			t.Fatal(err)
		}
		if mode == "export-write" {
			// The task-file write fails after the journaling save, as an
			// unwritable project directory makes it.
			if err = os.Chmod(c.Root, 0555); err != nil {
				t.Fatal(err)
			}
		}
		ack := e.applyAddTaskRequest(&req)
		if mode == "export-write" {
			if ack.Status == AckApplied {
				t.Fatal("the injected write failure must fail the application")
			}
		} else if e.err != nil || ack.Status != AckApplied {
			t.Fatalf("apply: %+v, %v", ack, e.err)
		}
		if mode == "receipt" || mode == "cleanup" {
			if err = writeJSON(filepath.Join(ackDir(e.Dir), "settings-crash.json"), ack); err != nil {
				t.Fatal(err)
			}
			if mode == "cleanup" {
				if err = os.Remove(filepath.Join(processingDir(e.Dir), "settings-crash.json")); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if e.err != nil && mode != "export-write" {
		t.Fatal(e.err)
	}
	if err = os.WriteFile(os.Getenv("LOOPER_REQUEST_CRASH_READY"), []byte("settings-crash"), 0600); err != nil {
		t.Fatal(err)
	}
	crashHelperHalt(crashHelperParent(), crashHelperDeadline())
}

// TestAddTaskCrashBoundariesSurviveRestart injects a controller kill at
// every add-task transport boundary and asserts the queryable outcome after
// each restart: no silent deletion, no false success, and no duplication on
// the replays that follow.
func TestAddTaskCrashBoundariesSurviveRestart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not stop the root user")
	}
	modes := []string{"envelope", "claim", "applied", "receipt", "cleanup", "export-write"}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			c := repoConfig(t, "run")
			bin, _ := fakeAgent(t, additionProposal)
			c.CodexBin, c.ClaudeBin = bin, bin
			b := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
				{ID: "T001", Title: "existing", Status: "done", Priority: 1, Tags: []string{"project-done"}},
			}}
			if err := writeJSON(c.Todo, b); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(t.TempDir(), "ready")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRequestCrashHelper$")
			cmd.Env = withEnvParent(os.Environ(), os.Getpid())
			cmd.Env = append(cmd.Env, "LOOPER_REQUEST_CRASH_CONFIG="+string(raw), "LOOPER_REQUEST_CRASH_MODE="+mode, "LOOPER_REQUEST_CRASH_READY="+ready, "LOOPER_REQUEST_CRASH_DEADLINE="+strconv.FormatInt(time.Now().Add(crashHelperBound).Unix(), 10))
			log, err := os.Create(filepath.Join(t.TempDir(), "child.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			cmd.Stdout, cmd.Stderr = log, log
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait() }()
			for {
				_, err = os.ReadFile(ready)
				if err == nil {
					break
				}
				select {
				case <-ctx.Done():
					output, _ := os.ReadFile(log.Name())
					t.Fatalf("helper did not reach %s: %s", mode, output)
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("expected an unclean exit")
			}
			if err = os.Chmod(c.Root, 0755); err != nil {
				t.Fatal(err)
			}
			wantState := map[string]string{
				"envelope": RequestRejected, "claim": RequestRejected,
				"applied": RequestApplied, "receipt": RequestApplied,
				"cleanup": RequestApplied, "export-write": RequestFailed,
			}[mode]
			for restart := 0; restart < 2; restart++ {
				e, lock, err := NewEngine(c, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				func() {
					defer lock.Close()
					defer e.Store.Close()
					if err = e.initialize(context.Background()); err != nil {
						t.Fatal(err)
					}
					if err = e.processControllerRequests(context.Background()); err != nil {
						t.Fatal(err)
					}
					if e.err != nil {
						t.Fatal(e.err)
					}
					saved, err := e.Store.Load()
					if err != nil {
						t.Fatal(err)
					}
					state, receipt := requestLifecycleState(e.Dir, saved, "settings-crash")
					if state != wantState {
						t.Fatalf("restart %d: receipt state %q, want %q (%+v)", restart, state, wantState, receipt)
					}
					count := 0
					for _, task := range saved.Backlog.Tasks {
						if task.ID == "T009" {
							count++
						}
					}
					wantTasks := 0
					if wantState == RequestApplied {
						wantTasks = 1
					}
					if count != wantTasks {
						t.Fatalf("restart %d: T009 appears %d time(s), want %d", restart, count, wantTasks)
					}
					if _, statErr := os.Stat(filepath.Join(processingDir(e.Dir), "settings-crash.json")); !os.IsNotExist(statErr) {
						t.Fatalf("restart %d: the claim must not strand", restart)
					}
				}()
			}
		})
	}
}
