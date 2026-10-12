package looper

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgent writes a canned JSON result wherever the agent protocol expects
// its final message, so import tests exercise the real agent plumbing. The
// prompt on stdin selects LOOPER_FAKE_DIAGNOSIS for diagnosis consultations,
// LOOPER_FAKE_REVIEW for review passes, LOOPER_FAKE_PLAN for planning, and
// LOOPER_FAKE_SUMMARY for worker reports; LOOPER_FAKE_RESULT answers
// everything else. With LOOPER_FAKE_TOUCH set, it also creates that file to
// simulate an agent that breaks its read-only contract. With
// LOOPER_FAKE_BACKLOG_EDIT set, it overwrites the task file in its working
// directory with that content, which simulates a review or planning agent
// that edits the private backlog. With LOOPER_FAKE_LOG set, it appends one
// line per invocation, "worker" for worker prompts and "other" otherwise, so
// tests can count implementation runs. With LOOPER_FAKE_SLEEP set, it sleeps
// that many seconds first, so a test can hold a dispatched worker in the
// running phase.
func fakeAgent(t *testing.T, proposal string) (bin string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent.sh")
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
if [ -n "$LOOPER_FAKE_SLEEP" ]; then sleep "$LOOPER_FAKE_SLEEP"; fi
kind=other
if printf '%s' "$p" | grep -q "Complete task "; then kind=worker; fi
result="$LOOPER_FAKE_RESULT"
if [ -n "$LOOPER_FAKE_DIAGNOSIS" ] && printf '%s' "$p" | grep -q "Diagnose the repeated failure"; then
  result="$LOOPER_FAKE_DIAGNOSIS"
fi
if [ -n "$LOOPER_FAKE_PLAN" ] && printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result="$LOOPER_FAKE_PLAN"
fi
if [ -n "$LOOPER_FAKE_SUMMARY" ] && [ "$kind" = worker ]; then
  result="$LOOPER_FAKE_SUMMARY"
fi
if [ -n "$LOOPER_FAKE_REVIEW" ] && printf '%s' "$p" | grep -q "review pass for Looper"; then
  result="$LOOPER_FAKE_REVIEW"
fi
if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
if [ -n "$LOOPER_FAKE_LOG" ]; then printf '%s\n' "$kind" >> "$LOOPER_FAKE_LOG"; fi
if [ -n "$LOOPER_FAKE_TOUCH" ]; then touch "$LOOPER_FAKE_TOUCH"; fi
if [ -n "$LOOPER_FAKE_BACKLOG_EDIT" ]; then
  printf '%s' "$LOOPER_FAKE_BACKLOG_EDIT" > "${LOOPER_FAKE_BACKLOG_PATH:-to-do.json}"
fi
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_RESULT", proposal)
	return bin, func() {}
}

func importEngine(t *testing.T, proposal string) (*Engine, Config) {
	t.Helper()
	bin, _ := fakeAgent(t, proposal)
	c := repoConfig(t, "run")
	c.CodexBin = bin
	c.ClaudeBin = bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "existing", Priority: 1, Status: "done", Tags: []string{"project-done"}},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	return e, c
}

func planFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

const additionProposal = `{"additions":[{"id":"P1","title":"Add telemetry","priority":2,"status":"todo","details":"Acceptance: telemetry endpoint returns counters.","depends_on":[]}],"covered":[{"requirement":"existing goal","task_id":"T001","evidence":"already implemented"}],"conflicts":[]}`

func TestStartupImportAppliesAdditions(t *testing.T) {
	e, c := importEngine(t, additionProposal)
	plan := planFile(t, "# New requirements\nAdd telemetry to the service.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	if len(e.State.Imports) != 1 || e.State.Imports[0].Status != ImportQueued {
		t.Fatalf("startup import not queued: %+v", e.State.Imports)
	}
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record := e.State.Imports[0]
	if record.Status != ImportApplied {
		t.Fatalf("import status %s: %s", record.Status, record.Error)
	}
	if len(record.AddedTasks) != 1 || record.AddedTasks[0] != "T002" {
		t.Fatalf("added tasks wrong: %v", record.AddedTasks)
	}
	if e.State.Backlog.Task("T002") == nil || e.State.Backlog.Task("T002").Status != "todo" {
		t.Fatal("addition not applied as a todo task")
	}
	// Scope expansion clears planning and satisfaction.
	if e.State.Plan != nil || e.State.PlanSignature != "" || e.State.Finished || e.State.ReviewedTip != "" {
		t.Fatalf("scope expansion must clear planning state: %+v", e.State)
	}
	// The task file export carries the addition.
	raw, _ := os.ReadFile(c.Todo)
	if !strings.Contains(string(raw), "Add telemetry") {
		t.Fatal("export missing the addition")
	}
	if digestFile(c.Todo) != e.State.ExportHash {
		t.Fatal("export hash mismatch after import")
	}
	// Context includes every applied document for review and workers.
	documents, err := e.appliedContext()
	if err != nil || len(documents) != 1 || !strings.Contains(string(documents[0].Text), "Add telemetry") {
		t.Fatal("imported scope missing")
	}
	// The ack names added tasks and coverage.
	var ack planImportAck
	if err := readJSON(filepath.Join(ackDir(e.Dir), record.RequestID+".json"), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Status != ImportApplied || len(ack.AddedTasks) != 1 || len(ack.Covered) != 1 {
		t.Fatalf("ack wrong: %+v", ack)
	}
}

func TestImportReplayDoesNotDuplicate(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "# Same requirements\nAdd telemetry to the service.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	before := len(e.State.Backlog.Tasks)
	// The same content arrives again as a new request.
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	if len(e.State.Backlog.Tasks) != before {
		t.Fatalf("replayed import added tasks: %d -> %d", before, len(e.State.Backlog.Tasks))
	}
	if len(e.State.Imports) != 1 {
		t.Fatalf("replay must reuse the recorded receipt: %d records", len(e.State.Imports))
	}
}

func TestImportConflictsLeaveBacklogUnchanged(t *testing.T) {
	conflict := `{"additions":[],"covered":[],"conflicts":["replaces the existing storage scope"]}`
	e, _ := importEngine(t, conflict)
	plan := planFile(t, "# Replace storage\nDrop SQLite and use flat files instead.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	before := digestFile(e.Config.Todo)
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record := e.State.Imports[0]
	if record.Status != ImportFailed || len(record.Conflicts) != 1 {
		t.Fatalf("conflict not reported: %+v", record)
	}
	if digestFile(e.Config.Todo) != before {
		t.Fatal("failed import must leave the backlog unchanged")
	}
}

func TestImportWaitsForRetainedWork(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "# More work\nAdd another feature.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	// An unfinished attempt owns the current contract.
	e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "a1", Task: Task{ID: "T001"}, Phase: "running"})
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if e.State.Imports[0].Status != ImportWaiting {
		t.Fatalf("import must wait: %+v", e.State.Imports[0])
	}
	if e.State.Backlog.Task("T002") != nil {
		t.Fatal("import must not add tasks while work is retained")
	}
	// The boundary releases after the attempt settles.
	e.State.Attempts[0].Phase = "done"
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if e.State.Imports[0].Status != ImportApplied {
		t.Fatalf("import must resume at the boundary: %+v", e.State.Imports[0])
	}
}

func TestImportRejectsInvalidProposals(t *testing.T) {
	unknownDep := `{"additions":[{"id":"P1","title":"X","priority":1,"status":"todo","details":"d","depends_on":["T999"]}],"covered":[],"conflicts":[]}`
	e, _ := importEngine(t, unknownDep)
	plan := planFile(t, "# Requirements\nSomething new.")
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	record := e.State.Imports[0]
	if record.Status != ImportFailed {
		t.Fatalf("unknown dependency must fail: %+v", record)
	}
	if e.State.Backlog.Task("T002") != nil {
		t.Fatal("invalid proposal mutated the backlog")
	}
}

func TestPlanValidation(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.md")
	if err := validatePlanFiles([]string{missing}); err == nil {
		t.Fatal("missing file accepted")
	}
	empty := planFile(t, "")
	if err := validatePlanFiles([]string{empty}); err == nil {
		t.Fatal("empty file accepted")
	}
	if err := validatePlanFiles([]string{dir}); err == nil {
		t.Fatal("directory accepted")
	}
	big := filepath.Join(dir, "big.md")
	if err := os.WriteFile(big, make([]byte, (1<<20)+1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := validatePlanFiles([]string{big}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized input accepted: %v", err)
	}
	invalid := filepath.Join(dir, "invalid.md")
	if err := os.WriteFile(invalid, []byte{0xff, 0xfe, 0xfd}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validatePlanFiles([]string{invalid}); err == nil {
		t.Fatal("invalid text accepted")
	}
}

func TestCapturedPlansAreImmutable(t *testing.T) {
	dir := t.TempDir()
	plan := planFile(t, "original content")
	plans, err := capturePlans(dir, "import-test", []string{plan})
	if err != nil || len(plans) != 1 {
		t.Fatal(err)
	}
	// Later edits to the source cannot change a queued import.
	if err = os.WriteFile(plan, []byte("changed content"), 0600); err != nil {
		t.Fatal(err)
	}
	again, err := loadCapturedPlans(&ImportRecord{Artifacts: []string{plans[0].Artifact}, SourcePaths: []string{plan}})
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Text != "original content" {
		t.Fatal("captured text changed with the source file")
	}
}

func TestBootstrapPlansImportedExactlyOnce(t *testing.T) {
	bootstrap := `{"schema_version":1,"goal":"planned goal","source_files":["plan.md"],"tasks":[{"id":"T001","title":"first","priority":1,"status":"todo","details":"Acceptance: works."}]}`
	bin, _ := fakeAgent(t, bootstrap)
	c := repoConfig(t, "run")
	c.CodexBin, c.ReviewAgent = bin, "codex"
	plan := planFile(t, "# Fresh project requirements\nBuild the first feature.")
	c.Plans = []string{plan}
	if _, err := os.Stat(c.Todo); !os.IsNotExist(err) {
		t.Fatal("test expects a fresh project")
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	defer func() { e.Store.Close(); lock.Close() }()
	if e.State.Backlog.Goal != "planned goal" {
		t.Fatalf("bootstrap ignored the plan: %q", e.State.Backlog.Goal)
	}
	if len(e.State.Imports) != 1 || e.State.Imports[0].Status != ImportApplied {
		t.Fatalf("bootstrap import not receipted: %+v", e.State.Imports)
	}
	// A restart adopts no second import from the same staging.
	e.adoptStagedImports()
	if len(e.State.Imports) != 1 {
		t.Fatalf("staging imported twice: %+v", e.State.Imports)
	}
	// Submitting the same content again replays the recorded result.
	e.Config.Plans = []string{plan}
	e.startupImportPlans()
	if len(e.State.Imports) != 1 {
		t.Fatalf("identical bootstrap plans re-imported: %+v", e.State.Imports)
	}
}

func TestStateJSONOmitsEmptyImports(t *testing.T) {
	s := &State{Version: 2, Backlog: testBacklog()}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "imports") {
		t.Fatalf("old state shape changed: %s", b)
	}
	withImports := &State{Version: 2, Backlog: testBacklog(), Imports: []*ImportRecord{{RequestID: "r", Status: ImportQueued}}}
	b, _ = json.Marshal(withImports)
	if !strings.Contains(string(b), `"imports"`) {
		t.Fatal("import records missing from state JSON")
	}
	// Old state files load without import metadata.
	var loaded State
	if err = json.Unmarshal([]byte(`{"version":2,"root":"/x","todo":"/x/to-do.json","backlog":{"schema_version":1,"source_files":[],"tasks":[]},"attempts":[]}`), &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Imports != nil {
		t.Fatal("legacy state gained phantom imports")
	}
}

func TestAddPlanNeedsActiveController(t *testing.T) {
	c := repoConfig(t, "add-plan")
	plan := planFile(t, "# Requirements\nMore work.")
	c.Plans = []string{plan}
	var out, stderr strings.Builder
	if code := runAddPlan(c, &out, &stderr); code != 1 {
		t.Fatalf("add-plan without controller must fail: %d", code)
	}
	if !strings.Contains(stderr.String(), "No active run") || !strings.Contains(stderr.String(), "looper --plan") {
		t.Fatalf("suggestion missing: %q", stderr.String())
	}
}

func TestImportMultipleAdditionsResolveDependencies(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	added, _, err := e.applyProposal(&importProposal{Additions: []Task{
		{ID: "P1", Title: "First", Details: "First acceptance", Priority: 1},
		{ID: "P2", Title: "Second", Details: "Second acceptance", Priority: 1, DependsOn: []string{"P1", "T001"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 || added[0] == added[1] {
		t.Fatalf("identifiers: %v", added)
	}
	task := e.State.Backlog.Task(added[1])
	if len(task.DependsOn) != 2 || task.DependsOn[0] != added[0] || task.DependsOn[1] != "T001" {
		t.Fatalf("dependencies: %v", task.DependsOn)
	}
}

func TestStartupPlansOnSavedState(t *testing.T) {
	for _, retained := range []bool{false, true} {
		name := "idle"
		if retained {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			e, _ := importEngine(t, additionProposal)
			if retained {
				e.State.Attempts = append(e.State.Attempts, &Attempt{ID: "retained", Task: e.State.Backlog.Tasks[0], Phase: "ready", ResultCommit: e.State.Tip})
				if err := e.save(); err != nil {
					t.Fatal(err)
				}
			}
			e.Config.Plans = []string{planFile(t, "Add telemetry")}
			if err := e.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(e.State.Imports) != 1 || e.State.Imports[0].Status != ImportQueued {
				t.Fatalf("plans not queued: %+v", e.State.Imports)
			}
			// Restarting with the same pending plan must preserve one import.
			if err := e.initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(e.State.Imports) != 1 {
				t.Fatalf("duplicate pending imports: %d", len(e.State.Imports))
			}
		})
	}
}

func TestImportResumesSavedAnalysis(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{planFile(t, "Add telemetry")}
	e.startupImportPlans()
	e.State.Imports[0].Status = ImportPlanning
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	if err := e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	if len(e.State.Imports) != 1 || e.State.Imports[0].Status != ImportApplied {
		t.Fatalf("analysis did not resume: %+v", e.State.Imports)
	}
	if e.State.Backlog.Task("T002") == nil {
		t.Fatal("resumed import did not add the task")
	}
}

func TestDuplicateImportAcknowledgesNewRequest(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plans := []string{planFile(t, "Add telemetry")}
	e.Config.Plans = plans
	e.startupImportPlans()
	e.processPlanImports(context.Background())
	for i := 0; i < 2; i++ {
		e.acceptPlanImport("second-request", plans, false)
		var ack planImportAck
		if err := readJSON(filepath.Join(ackDir(e.Dir), "second-request.json"), &ack); err != nil {
			t.Fatal(err)
		}
		if ack.ID != "second-request" || ack.Status != ImportApplied || len(ack.AddedTasks) != 1 || ack.AddedTasks[0] != "T002" {
			t.Fatalf("ack: %+v", ack)
		}
	}
	if len(e.State.Imports) != 1 || len(e.State.Backlog.Tasks) != 2 {
		t.Fatal("duplicate import changed scope")
	}
}

func TestImportUsesRefreshedBaseline(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{planFile(t, "Add telemetry")}
	e.startupImportPlans()
	before := e.State.Tip
	if err := os.WriteFile(filepath.Join(e.Config.Root, "new.txt"), []byte("outside edit"), 0600); err != nil {
		t.Fatal(err)
	}
	e.processPlanImports(context.Background())
	record := e.State.Imports[0]
	if record.Status != ImportApplied {
		t.Fatalf("import failed: %s", record.Error)
	}
	if e.State.Tip == before {
		t.Fatal("import did not use the refreshed baseline")
	}
}
