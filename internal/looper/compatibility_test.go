package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
	"github.com/nibzard/looper/internal/ui"
)

// The legacy contract fixtures live in docs/contracts so other worktrees and
// agents can read them. Run `go test ./internal/looper -run
// TestLegacyContract -update-contract-fixtures` after a deliberate contract
// change to record the new shape; the change also needs a migration entry.
var updateContractFixtures = flag.Bool("update-contract-fixtures", false, "rewrite the recorded legacy contract shapes in docs/contracts/fixtures/legacy")

const legacyContractDocs = "../../docs/contracts"

// contractScenario builds one fixed saved run. The documents the CLI produces
// from it must keep the shape recorded in docs/contracts/fixtures/legacy.
func contractScenario(t *testing.T) Config {
	t.Helper()
	root, base := t.TempDir(), t.TempDir()
	c := Config{Root: root, BaseDir: base, Todo: filepath.Join(root, "to-do.json"), Command: "status", JSON: true, Agent: "claude"}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tasks := []Task{
		{ID: "T1", Title: "Hold a decision", Priority: 1, Status: "blocked", Blockers: []string{"needs an owner decision"}, Tags: []string{"looper:deferred"}, UnblockWhen: "The owner records the decision.", CreatedAt: "2026-11-01T09:00:00Z", UpdatedAt: "2026-11-02T09:00:00Z"},
		{ID: "T2", Title: "Second task", Priority: 2, Status: "doing", DependsOn: []string{"T1"}, Files: []string{"docs/PLAN.md"}, CreatedAt: "2026-11-01T09:00:00Z"},
	}
	// One attempt carries worker output evidence so the observation records a
	// log path, an output age, and a process record; the other stays bare.
	work := t.TempDir()
	log := filepath.Join(work, "work-00", "events-00.jsonl")
	if err = atomicWrite(log, []byte("{\"type\":\"assistant\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	process := `{"pid":4242,"identity":"identity-4242","started":"2026-11-05T09:41:02Z"}`
	if err = os.WriteFile(log+".process.json", []byte(process), 0600); err != nil {
		t.Fatal(err)
	}
	s := &State{
		Version: 2, Root: root, Todo: c.Todo, Head: "head", Branch: "main", Ref: "refs/heads/main",
		Tip: "tip", Published: "published", ExportHash: "export-hash",
		// Every state the engine exports since AX-018 carries the
		// fingerprint. The scenario saves it so the frozen projection has to
		// keep hiding the key from the legacy status document (T100).
		ExportSemantics: "semantics-fingerprint",
		Backlog:         Backlog{Version: 1, Sources: []string{"docs/PLAN.md"}, Goal: "ship the plan", Tasks: tasks},
		Attempts: []*Attempt{
			{ID: "task-bare", Task: tasks[0], Agent: "claude", Base: "base", Directory: root, Ref: "ref", Phase: "blocked", Repairs: 0, Worktrees: []string{},
				ResultCommit: "result-commit",
				Summary:      &Summary{TaskID: "T1", Status: "blocked", Summary: "needs the decision", Files: []string{}, Blockers: []string{"needs an owner decision"}, UnblockWhen: "The owner records the decision."},
				Verification: &Candidate{Base: "base", Commit: "commit", Tree: "tree", AttemptID: "task-bare", OK: false, Feedback: "verify failed"}},
			{ID: "task-live", Task: tasks[1], Agent: "claude", Base: "base", Directory: work, Ref: "ref", Phase: "running", Repairs: 1, Worktrees: []string{"wt-1"},
				Feedback: "verify failed", FailureHash: "hash", Repeats: 1, Diagnosed: true},
		},
		Plan:              &Plan{Setup: "make setup", Verify: "make test", Tasks: []Scope{{TaskID: "T2", Paths: []string{"docs"}, Resources: []string{}}}},
		PlanSignature:     "plan-signature",
		Publication:       &Candidate{Base: "base", Commit: "commit", Tree: "tree", Worktree: "wt-1", Before: "before", AttemptID: "task-live", OK: true, Feedback: "ok"},
		Recoveries:        []RunRecovery{{Identity: "identity", Outcome: "applied", Tasks: []string{"T1"}, Integrated: 1}},
		RecoveredCommands: &RecoveryCommands{PreviousSetup: "old", PreviousVerify: "old", BeforeSetup: "before", BeforeVerify: "before", Setup: "new", Verify: "new"},
		ReviewedTip:       "reviewed-tip",
		ReviewPending:     true,
		ExportBefore:      "export-before",
		ExportPending:     true,
		Imports:           []*ImportRecord{{RequestID: "import-01", Digests: []string{"digest"}, Artifacts: []string{"imports/import-01/plan.md"}, SourcePaths: []string{"docs/PLAN.md"}, Status: ImportApplied, AddedTasks: []string{"T070"}, Enqueued: "2026-11-05T09:41:02Z", Applied: "2026-11-05T09:44:02Z"}},
		// Receipts are the normal saved state after any mutation (AX-009) —
		// one terminal, one waiting beside the decided cancellation below —
		// so the frozen guards fail if the legacy status projection stops
		// hiding the machinery (T118).
		Requests: []*RequestRecord{
			{ID: "import-01", Type: PublicRequestAddPlan, State: RequestApplied, Session: "run-f7803453a9ce5f6b", SubmittedAt: "2026-11-05T09:41:02Z", DecidedAt: "2026-11-05T09:44:02Z", AffectedTasks: []string{"T070"}, Summary: "Imported the plan."},
			{ID: "cancel-01", Type: PublicRequestCancel, State: RequestWaiting, Session: "run-f7803453a9ce5f6b", SubmittedAt: "2026-11-05T09:50:00Z", WaitReason: "active_work", WaitDetail: "T2 holds a running attempt", WaitSince: "2026-11-05T09:50:05Z", AffectedTasks: []string{"T2"}},
		},
		// One decided cancellation waits for T2's running attempt to drain
		// (AX-019), so the scenario also carries that state machinery.
		Cancellations: []*PendingCancellation{
			{RequestID: "cancel-01", TaskIDs: []string{"T2"}, DecidedAt: "2026-11-05T09:50:05Z"},
		},
		// A detached start saves the AX-022 startup seed for the whole
		// bootstrap window, so a status query can read a state that carries
		// the marker. The scenario seeds it beside the rest of the machinery
		// — a real seed holds less, but the projection must hide the marker
		// whatever else the state carries (T122).
		StartupSeed: true,
		Finished:    false,
		Integrated:  1,
		ReviewedAt:  1,
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	if err = store.Check("task-bare", "rev-1", "verify", "failed", "verify.log"); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		kind, message string
		data          any
	}{
		{"start", "Session started", nil},
		{"check", "Checking T2 with integrated work...", map[string]string{"task_id": "T2", "attempt_id": "task-live"}},
		{"shutdown", "Stopped after draining active work", map[string]string{"reason": "drained"}},
	} {
		if _, err = store.Event(e.kind, e.message, e.data); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// populatedTimingAndStatistics fills every optional field so the recorded
// shape also pins the fields an empty store leaves out.
func populatedTimingAndStatistics() (TimingReport, ui.StatisticsView) {
	one, two := 1.5, 30.25
	timing := TimingReport{
		Tasks: []TaskTimingReport{{
			TaskID: "T2", Available: true, HistoryComplete: true, Attempts: 1, RecordedAttempts: 1,
			WorkerSeconds: &one, CheckSeconds: &two, OtherSeconds: &one, ActiveSeconds: &two, ElapsedSeconds: &two,
			UnknownProcesses: 0, RunningProcesses: 1,
		}},
		Version: 1, Available: true, RecordedSince: "2026-11-05T09:41:02Z", FirstRecorded: "2026-11-05T09:41:02Z",
		LastRecorded: "2026-11-05T10:41:02Z", HistoryComplete: true, ElapsedSeconds: &two, ActiveSeconds: &one,
		WorkerSeconds: &two, ControllerSeconds: &one, UnknownSessions: 0, UnknownProcesses: 0,
		RunningProcesses: 1, ImportedProcesses: 2,
	}
	statistics := ui.StatisticsView{
		Version: 1, UpdatedAt: "2026-11-05T10:41:02Z", FirstRecorded: "2026-11-05T09:41:02Z",
		ElapsedSeconds: &two, ActiveSeconds: &one, WorkerSeconds: &two, ControllerSeconds: &one,
		HistoryComplete: true, UnknownEnds: 0, TasksDone: 1, TasksTotal: 2, Integrations: 1, Attempts: 2,
		Repairs: 1, ChecksPassed: 1, ChecksFailed: 1, Sessions: 1, UsageReports: 3, AgentRuns: 3, PricedReports: 3,
		Tokens: 1000, Input: 400, Output: 300, CacheRead: 300, EstimatedUSD: &one, ReportedUSD: &two,
		ReportedCostBasis: "unknown", PricingDate: "2026-11-01",
		Models: []ui.ModelStatistics{{Model: "glm-5.3", Input: 400, Output: 300, CacheRead: 300, CacheWrite: 0, EstimatedUSD: &one, ReportedUSD: &two, ReportedCostBasis: "unknown", Reports: 3, Priced: 3}},
		Days:   []ui.DailyStatistics{{Date: "2026-11-05", WorkerSeconds: 30.25, Integrations: 1, Tokens: 1000, Reports: 3, EstimatedUSD: &one}},
	}
	return timing, statistics
}

// failedPlanImportAck is the acknowledgement the engine writes for an import
// whose analysis reports conflicts: writePlanAck (imports.go) copies the
// receipt's conflicts and error into it, and no task, coverage, or gate
// survives a failed import. The applied example above leaves both keys empty,
// so this variant is the only recorded document that pins them.
func failedPlanImportAck() planImportAck {
	return planImportAck{Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: "import-02", Status: ImportFailed,
		Session:   "run-f7803453a9ce5f6b",
		Conflicts: []string{"replaces the existing storage scope"},
		Error:     "the plan conflicts with existing scope: replaces the existing storage scope"}
}

// TestLegacyContractShapesAreFrozen is the executable compatibility guard for
// the contract-version-1 documents the CLI emits today. It produces each
// document through the production path, checks it against the frozen legacy
// schema, and compares its structure with the recorded fixture. A failure
// means a field was added, removed, renamed, or retyped: either restore the
// shape or record the change deliberately with -update-contract-fixtures and
func TestLegacyContractShapesAreFrozen(t *testing.T) {
	c := contractScenario(t)
	ctx := context.Background()
	timing, statistics := populatedTimingAndStatistics()

	documents := map[string]struct {
		schema  string
		produce func() ([]byte, error)
	}{
		"status": {"status", func() ([]byte, error) {
			var out bytes.Buffer
			cc := c
			cc.Command, cc.JSON = "status", true
			if err := inspect(ctx, cc, &out); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		"status-populated": {"status", func() ([]byte, error) {
			// The same report with every optional timing and statistics field
			// present, so the recorded shape pins those keys too.
			_, dir := projectPaths(c)
			store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
			if err != nil {
				return nil, err
			}
			defer store.Close()
			snap, err := store.Snapshot()
			if err != nil {
				return nil, err
			}
			if snap.State == nil {
				return nil, fmt.Errorf("no saved state")
			}
			return json.Marshal(statusReport(c, snap, timing, statistics))
		}},
		"inspect": {"inspect", func() ([]byte, error) {
			var out bytes.Buffer
			cc := c
			cc.Command, cc.InspectTask, cc.JSON = "inspect", "T2", true
			if err := inspect(ctx, cc, &out); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		"doctor": {"doctor", func() ([]byte, error) {
			var out bytes.Buffer
			doctorJSON(c, &out)
			return out.Bytes(), nil
		}},
		"settings-ack": {"settings-ack", func() ([]byte, error) {
			ack := SettingsAck{Version: SettingsRequestVersion, ID: "settings-545679dbaf4e41e2", Status: AckApplied,
				Session: "run-f7803453a9ce5f6b", ProfileDigest: "digest", Outcomes: map[string]string{"agent": "applied"}, Message: "Saved."}
			return json.Marshal(ack)
		}},
		"plan-import-ack": {"plan-import-ack", func() ([]byte, error) {
			ack := planImportAck{Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: "import-01", Status: ImportApplied,
				Session: "run-f7803453a9ce5f6b", AddedTasks: []string{"T070"},
				Covered:     []CoveredRef{{Requirement: "keep plain status readable", TaskID: "T012", Evidence: "docs/UI-PLAN.md"}},
				Adjustments: []string{"T071 now depends on T070"},
				Gates:       []ImportGate{{TaskID: "T071", DependsOn: []string{"T070"}}}}
			return json.Marshal(ack)
		}},
		"plan-import-ack-failed": {"plan-import-ack", func() ([]byte, error) {
			return json.Marshal(failedPlanImportAck())
		}},
		"version": {"version", func() ([]byte, error) { return json.Marshal(currentBuild()) }},
	}

	for name, doc := range documents {
		produced, err := doc.produce()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		compareContractDocument(t, name, doc.schema, produced)
	}

	// The event stream is newline-delimited JSON: each line is one document,
	// and the recorded fixture lists the distinct line shapes.
	var out bytes.Buffer
	cc := c
	cc.Command, cc.JSON, cc.After = "events", true, 0
	if err := inspect(ctx, cc, &out); err != nil {
		t.Fatalf("events: %v", err)
	}
	schemaBytes := readLegacySchema(t, "event-ndjson")
	shapes := []any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		if err := contractschema.ValidateJSON(schemaBytes, []byte(line)); err != nil {
			t.Errorf("event line does not match its frozen schema: %v\n%s", err, line)
		}
		shape, err := contractschema.ShapeJSON([]byte(line))
		if err != nil {
			t.Fatalf("event line does not parse: %v", err)
		}
		shapes = append(shapes, shape)
	}
	if len(shapes) == 0 {
		t.Fatal("the scenario produced no events")
	}
	recordContractShape(t, "events-ndjson", shapes)
}

func readLegacySchema(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "legacy", name+".schema.json"))
	if err != nil {
		t.Skipf("legacy contract schemas are absent: %v", err)
	}
	return b
}

func compareContractDocument(t *testing.T, name, schemaName string, produced []byte) {
	t.Helper()
	if err := contractschema.ValidateJSON(readLegacySchema(t, schemaName), produced); err != nil {
		t.Errorf("%s does not match its frozen legacy schema: %v", name, err)
	}
	shape, err := contractschema.ShapeJSON(produced)
	if err != nil {
		t.Errorf("%s is not valid JSON: %v", name, err)
		return
	}
	recordContractShape(t, name, shape)
}

func recordContractShape(t *testing.T, name string, shape any) {
	t.Helper()
	path := filepath.Join(legacyContractDocs, "fixtures", "legacy", name+".shape.json")
	if *updateContractFixtures {
		data, err := json.MarshalIndent(shape, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, append(data, '\n'), 0644); err != nil {
			t.Fatalf("record %s: %v", path, err)
		}
		return
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s has no recorded shape; run go test ./internal/looper -run TestLegacyContract -update-contract-fixtures: %v", name, err)
	}
	var recorded any
	if err = json.Unmarshal(b, &recorded); err != nil {
		t.Fatalf("%s recorded shape does not parse: %v", name, err)
	}
	if !contractschema.ShapeEqual(shape, recorded) {
		t.Errorf("%s changed shape. Restore the legacy shape, or record the change deliberately with -update-contract-fixtures and add a migration entry to docs/AGENT-CONTRACT-MIGRATION.md.\n produced: %s\n recorded: %s",
			name, marshalShape(shape), marshalShape(recorded))
	}
}

func marshalShape(shape any) string {
	b, err := json.Marshal(shape)
	if err != nil {
		return fmt.Sprintf("%v", shape)
	}
	return string(b)
}

// TestLegacyStatusOmitsSavedStateMachinery pins the T100, T118, and T122
// repairs: every state the engine exports since AX-018 carries the
// export_semantics fingerprint, every state saved after a mutation carries
// request receipts (AX-009), a decided cancellation waiting on active work
// survives a restart in the saved state (AX-019), and a detached start saves
// the startup seed for its whole bootstrap window (AX-022). The frozen
// legacy status document must keep hiding all four. A leak here fails the
// frozen schema, whose additionalProperties false admits no such keys. The
// version 2 --full route serves the same complete document, so it hides
// them too.
func TestLegacyStatusOmitsSavedStateMachinery(t *testing.T) {
	c := contractScenario(t)
	_, dir := projectPaths(c)
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.State == nil || snap.State.ExportSemantics == "" {
		t.Fatal("the scenario must save a state that carries the fingerprint")
	}
	if len(snap.State.Requests) == 0 {
		t.Fatal("the scenario must save a state that carries request receipts")
	}
	if len(snap.State.Cancellations) == 0 {
		t.Fatal("the scenario must save a state that carries a decided cancellation")
	}
	if !snap.State.StartupSeed {
		t.Fatal("the scenario must save a state that carries the startup seed marker")
	}
	for _, route := range []struct {
		name string
		c    Config
	}{
		{"status --json (the legacy default)", Config{}},
		{"status --contract v2 --full --json", Config{Contract: ContractVersionV2, Full: true}},
	} {
		var out bytes.Buffer
		cc := c
		cc.Command, cc.JSON = "status", true
		cc.Contract, cc.Full = route.c.Contract, route.c.Full
		if err := inspect(context.Background(), cc, &out); err != nil {
			t.Fatalf("%s: %v", route.name, err)
		}
		var doc map[string]json.RawMessage
		if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%s: output is not JSON: %v", route.name, err)
		}
		for _, key := range []string{"export_semantics", "requests", "cancellations", "startup_seed"} {
			if _, ok := doc[key]; ok {
				t.Errorf("%s emits %s; the key is saved-state machinery and stays out of the frozen document: %s", route.name, key, out.Bytes())
			}
		}
		if err := contractschema.ValidateJSON(readLegacySchema(t, "status"), out.Bytes()); err != nil {
			t.Errorf("%s does not match its frozen schema: %v", route.name, err)
		}
	}
}

// TestContractDocumentsStayDeterministic guards the fixture scenario itself:
// producing the documents twice from a fresh scenario must yield the same
// shape, otherwise the recorded fixtures would depend on the run.
func TestContractDocumentsStayDeterministic(t *testing.T) {
	shapes := map[string]string{}
	for i := 0; i < 2; i++ {
		c := contractScenario(t)
		var out bytes.Buffer
		cc := c
		cc.Command, cc.JSON = "status", true
		if err := inspect(context.Background(), cc, &out); err != nil {
			t.Fatal(err)
		}
		shape, err := contractschema.ShapeJSON(out.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		shapes[fmt.Sprint(i)] = marshalShape(shape)
	}
	if shapes["0"] != shapes["1"] {
		t.Fatalf("the status shape is not deterministic:\n%s\n%s", shapes["0"], shapes["1"])
	}
	if strings.Contains(shapes["0"], t.TempDir()) {
		t.Fatal("a temporary path leaked into the recorded shape")
	}
}

// TestCommandSurfaceMatchesContractRegistry keeps the command inventory
// executable. Every invocation in docs/contracts/commands.json must parse
// into the command it names, every command the parser accepts must be listed,
// and every referenced schema file must exist.
func TestCommandSurfaceMatchesContractRegistry(t *testing.T) {
	path := filepath.Join(legacyContractDocs, "commands.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("command registry is absent: %v", err)
	}
	var registry struct {
		Commands []struct {
			Name       string   `json:"name"`
			Invocation []string `json:"invocation"`
			Legacy     *string  `json:"legacy"`
			V2         *string  `json:"v2"`
			Streaming  bool     `json:"streaming"`
			OwningTask string   `json:"owning_task"`
		} `json:"commands"`
	}
	if err = json.Unmarshal(b, &registry); err != nil {
		t.Fatalf("commands.json does not parse: %v", err)
	}
	if len(registry.Commands) == 0 {
		t.Fatal("commands.json lists no commands")
	}
	t.Setenv("LOOPER_JOBS", "2")
	for _, command := range registry.Commands {
		if len(command.Invocation) == 0 {
			t.Errorf("%s has no invocation to parse", command.Name)
			continue
		}
		c, err := Parse(command.Invocation)
		if err != nil {
			t.Errorf("%s invocation %v does not parse: %v", command.Name, command.Invocation, err)
			continue
		}
		if c.Command != command.Name {
			t.Errorf("%s invocation parses as %q", command.Name, c.Command)
		}
		if command.OwningTask == "" {
			t.Errorf("%s names no owning task", command.Name)
		}
		for _, ref := range []struct {
			path   *string
			period string
		}{{command.Legacy, "legacy"}, {command.V2, "v2"}} {
			if ref.path == nil {
				continue
			}
			if _, err = os.Stat(filepath.Join(legacyContractDocs, *ref.path)); err != nil {
				t.Errorf("%s references missing %s schema %s", command.Name, ref.period, *ref.path)
			}
		}
	}
	// The parser's own command list must stay covered by the registry so a
	// new command cannot ship without a contract entry.
	source, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, command := range registry.Commands {
		listed[command.Name] = true
	}
	for _, name := range parserCommandNames(string(source)) {
		if !listed[name] {
			t.Errorf("the parser accepts %q, which commands.json does not list", name)
		}
	}
}

// parserCommandNames extracts the command names from the argument switch in
// config.go. It reads the one case line that enumerates the subcommands, so a
// renamed or moved list fails loudly instead of silently passing.
func parserCommandNames(source string) []string {
	for _, line := range strings.Split(source, "\n") {
		if !strings.Contains(line, `case "run", "status"`) {
			continue
		}
		var names []string
		for _, field := range strings.Fields(strings.SplitN(line, ":", 2)[1]) {
			if name := strings.Trim(field, `":,`); name != "" {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return nil
		}
		return names
	}
	return nil
}
