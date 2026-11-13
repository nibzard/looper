package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// The byte-level legacy fixtures. The shape fixtures beside them
// (docs/contracts/fixtures/legacy/*.shape.json) guard structure: a scalar
// that changes its textual form keeps its shape and passes that guard. These
// fixtures record full example documents — fixed times, fixed identifiers,
// fixed build identity — and compare the live output byte for byte, so key
// order, spacing, escaping, and the textual form of every scalar are frozen
// too.
//
// Record a deliberate change the same way as a shape change:
//
//	go test ./internal/looper -run TestLegacyContractBytes -update-contract-fixtures
//
// and add a dated entry to docs/AGENT-CONTRACT-MIGRATION.md.
func TestLegacyContractBytesAreFrozen(t *testing.T) {
	// Resolve the contract directory before the scenario changes the working
	// directory; the shared helpers read it relative to the package dir.
	docs, err := filepath.Abs(legacyContractDocs)
	if err != nil {
		t.Fatal(err)
	}
	c := byteFixtureScenario(t)
	ctx := context.Background()

	documents := []struct {
		name    string
		schema  string
		ext     string
		produce func(Config) ([]byte, error)
	}{
		{"status", "status", "json", func(c Config) ([]byte, error) {
			var out bytes.Buffer
			cc := c
			cc.Command, cc.JSON = "status", true
			if err := inspect(ctx, cc, &out); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		{"inspect", "inspect", "json", func(c Config) ([]byte, error) {
			var out bytes.Buffer
			cc := c
			cc.Command, cc.InspectTask, cc.JSON = "inspect", "T2", true
			if err := inspect(ctx, cc, &out); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		{"doctor", "doctor", "json", func(c Config) ([]byte, error) {
			var out bytes.Buffer
			doctorJSON(c, &out)
			return out.Bytes(), nil
		}},
		// The event stream is newline-delimited JSON: the recorded file holds
		// the exact stream, one document per line.
		{"events-ndjson", "event-ndjson", "jsonl", func(c Config) ([]byte, error) {
			var out bytes.Buffer
			cc := c
			cc.Command, cc.JSON, cc.After = "events", true, 0
			if err := inspect(ctx, cc, &out); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		// The two request acknowledgements take no clock and no saved state,
		// so their scenario is the fixed struct the CLI prints, encoded
		// through the same json.Encoder the CLI uses (add_task.go for the
		// settings ack, cli.go for the plan import ack). The encoder pins
		// what a marshal alone would miss: the trailing newline and the
		// encoder's HTML escaping.
		{"settings-ack", "settings-ack", "json", func(Config) ([]byte, error) {
			var out bytes.Buffer
			ack := SettingsAck{Version: SettingsRequestVersion, ID: "settings-545679dbaf4e41e2", Status: AckApplied,
				Session: "run-f7803453a9ce5f6b", ProfileDigest: "digest", Outcomes: map[string]string{"agent": "applied"}, Message: "Saved."}
			if err := json.NewEncoder(&out).Encode(ack); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		{"plan-import-ack", "plan-import-ack", "json", func(Config) ([]byte, error) {
			var out bytes.Buffer
			ack := planImportAck{Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: "import-01", Status: ImportApplied,
				Session: "run-f7803453a9ce5f6b", AddedTasks: []string{"T070"},
				Covered:     []CoveredRef{{Requirement: "keep plain status readable", TaskID: "T012", Evidence: "docs/UI-PLAN.md"}},
				Adjustments: []string{"T071 now depends on T070"},
				Gates:       []ImportGate{{TaskID: "T071", DependsOn: []string{"T070"}}}}
			if err := json.NewEncoder(&out).Encode(ack); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		// The failed acknowledgement is a second variant of the same
		// document: the applied example above leaves conflicts and error
		// empty, so this one is what freezes those two keys.
		{"plan-import-ack-failed", "plan-import-ack", "json", func(Config) ([]byte, error) {
			var out bytes.Buffer
			if err := json.NewEncoder(&out).Encode(failedPlanImportAck()); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		}},
		// The version document goes through the CLI entry point itself: the
		// command dispatch, not just the encoder, is part of its bytes.
		{"version", "version", "json", func(Config) ([]byte, error) {
			var out, stderr bytes.Buffer
			if code := Main([]string{"--version", "--json"}, &out, &stderr); code != 0 {
				return nil, fmt.Errorf("version exited %d: %s", code, stderr.String())
			}
			return out.Bytes(), nil
		}},
	}
	for _, document := range documents {
		produced, err := document.produce(c)
		if err != nil {
			t.Errorf("%s: %v", document.name, err)
			continue
		}
		compareLegacyBytes(t, docs, document.name, document.schema, document.ext, produced)
	}
}

// byteFixtureScenario builds one fixed saved run whose documents hold no
// value that varies between machines or runs. It pins the wall clock and the
// build identity, and enters a temporary directory so every path a document
// names is a short fixed relative identifier.
func byteFixtureScenario(t *testing.T) Config {
	t.Helper()
	scenario := t.TempDir()
	enterTestDirectory(t, scenario)
	// Doctor compares the retained baseline with the project HEAD through
	// git, which walks parent directories to find a repository. The ceiling
	// keeps a repository around the temporary directory from leaking its
	// commits into the recorded document.
	t.Setenv("GIT_CEILING_DIRECTORIES", scenario)

	fake := time.Date(2026, 11, 6, 11, 0, 0, 0, time.UTC)
	previousClock := wallClock
	wallClock = func() time.Time { return fake }
	t.Cleanup(func() { wallClock = previousClock })
	previousBuild := currentBuild
	currentBuild = func() BuildInfo {
		return BuildInfo{Release: "0.8.3", Revision: "0f1e2d3c4b5a69788695a4b3c2d1e0f1a2b3c4d5", Modified: true}
	}
	t.Cleanup(func() { currentBuild = previousBuild })

	for _, dir := range []string{"project", "base", filepath.Join("work", "work-00")} {
		if err := os.MkdirAll(filepath.Join(scenario, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// One attempt carries worker output evidence — log, size, age, and a
	// recorded process — so the observation pins those fields too.
	log := filepath.Join("work", "work-00", "events-00.jsonl")
	if err := atomicWrite(log, []byte("{\"type\":\"assistant\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	lastOutput := time.Date(2026, 11, 6, 11, 59, 0, 0, time.UTC)
	if err := os.Chtimes(log, lastOutput, lastOutput); err != nil {
		t.Fatal(err)
	}
	process := `{"pid":4242,"identity":"identity-4242","started":"2026-11-06T11:58:00Z"}`
	if err := os.WriteFile(log+".process.json", []byte(process), 0600); err != nil {
		t.Fatal(err)
	}

	c := Config{Root: "project", BaseDir: "base", Todo: filepath.Join("project", "to-do.json"), Command: "status", JSON: true, Agent: "claude"}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	tasks := []Task{
		{ID: "T1", Title: "Hold a decision", Priority: 1, Status: "blocked", Blockers: []string{"needs an owner decision"}, Tags: []string{"looper:deferred"}, UnblockWhen: "The owner records the decision.", CreatedAt: "2026-11-01T09:00:00Z", UpdatedAt: "2026-11-02T09:00:00Z"},
		{ID: "T2", Title: "Second task", Priority: 2, Status: "doing", DependsOn: []string{"T1"}, Files: []string{"docs/PLAN.md"}, CreatedAt: "2026-11-01T09:00:00Z"},
	}
	s := &State{
		Version: 2, Root: c.Root, Todo: c.Todo, Head: "head", Branch: "main", Ref: "refs/heads/main",
		Tip: "tip", Published: "published", ExportHash: "export-hash",
		// Every state the engine exports since AX-018 carries the
		// fingerprint; the frozen legacy document keeps hiding it (T100).
		ExportSemantics: "semantics-fingerprint",
		Backlog:         Backlog{Version: 1, Sources: []string{"docs/PLAN.md"}, Goal: "ship the plan", Tasks: tasks},
		Attempts: []*Attempt{
			{ID: "task-bare", Task: tasks[0], Agent: "claude", Base: "base", Directory: "project", Ref: "ref", Phase: "blocked", Repairs: 0, Worktrees: []string{},
				ResultCommit: "result-commit",
				Summary:      &Summary{TaskID: "T1", Status: "blocked", Summary: "needs the decision", Files: []string{}, Blockers: []string{"needs an owner decision"}, UnblockWhen: "The owner records the decision."},
				Verification: &Candidate{Base: "base", Commit: "commit", Tree: "tree", AttemptID: "task-bare", OK: false, Feedback: "verify failed"}},
			{ID: "task-live", Task: tasks[1], Agent: "claude", Base: "base", Directory: "work", Ref: "ref", Phase: "running", Repairs: 1, Worktrees: []string{"wt-1"},
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
		Imports:           []*ImportRecord{{RequestID: "import-01", Digests: []string{"digest"}, Artifacts: []string{"imports/import-01/plan.md"}, SourcePaths: []string{"docs/PLAN.md"}, Status: ImportApplied, AddedTasks: []string{"T070"}, Enqueued: "2026-11-06T11:41:02Z", Applied: "2026-11-06T11:44:02Z"}},
		// Receipts are the normal saved state after any mutation (AX-009) —
		// one terminal, one waiting beside the decided cancellation below —
		// so the byte guards fail if the legacy status projection stops
		// hiding the machinery (T118).
		Requests: []*RequestRecord{
			{ID: "import-01", Type: PublicRequestAddPlan, State: RequestApplied, Session: "run-f7803453a9ce5f6b", SubmittedAt: "2026-11-06T11:41:02Z", DecidedAt: "2026-11-06T11:44:02Z", AffectedTasks: []string{"T070"}, Summary: "Imported the plan."},
			{ID: "cancel-01", Type: PublicRequestCancel, State: RequestWaiting, Session: "run-f7803453a9ce5f6b", SubmittedAt: "2026-11-06T11:50:00Z", WaitReason: "active_work", WaitDetail: "T2 holds a running attempt", WaitSince: "2026-11-06T11:50:05Z", AffectedTasks: []string{"T2"}},
		},
		// One decided cancellation waits for T2's running attempt to drain
		// (AX-019), so the scenario also carries that state machinery.
		Cancellations: []*PendingCancellation{
			{RequestID: "cancel-01", TaskIDs: []string{"T2"}, DecidedAt: "2026-11-06T11:50:05Z"},
		},
		// A detached start saves the AX-022 startup seed for the whole
		// bootstrap window, so a status query can read a state that carries
		// the marker. The scenario seeds it beside the rest of the machinery
		// — a real seed holds less, but the byte guard must fail if the
		// projection stops hiding the marker (T122).
		StartupSeed: true,
		Finished:    false,
		Integrated:  1,
		ReviewedAt:  1,
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	fake = time.Date(2026, 11, 6, 11, 30, 0, 0, time.UTC)
	if err = store.Check("task-live", "rev-1", "verify", "failed", "work/checks/rev-1/verify.log"); err != nil {
		t.Fatal(err)
	}
	fake = time.Date(2026, 11, 6, 11, 40, 0, 0, time.UTC)
	for _, e := range []struct {
		offset  time.Duration
		kind    string
		message string
		data    any
	}{
		{0, "start", "Session started", nil},
		{30 * time.Second, "check", "Checking T2 with integrated work...", map[string]string{"task_id": "T2", "attempt_id": "task-live"}},
		{60 * time.Second, "shutdown", "Stopped after draining active work", sessionExit{Session: "run-f7803453a9ce5f6b", Reason: "drained", LastEventID: 2}},
	} {
		fake = fake.Add(e.offset)
		if _, err = store.Event(e.kind, e.message, e.data); err != nil {
			t.Fatal(err)
		}
	}
	// Every document reads the clock at one fixed observation instant, half a
	// second past the minute, so the two timestamp formats the documents use
	// (RFC 3339Nano and RFC 3339) both show their distinguishing behavior.
	fake = time.Date(2026, 11, 6, 12, 0, 0, 500000000, time.UTC)
	store.Close()
	return c
}

// enterTestDirectory changes the process working directory for one test and
// restores it on cleanup. Package tests run sequentially; none call
// t.Parallel.
func enterTestDirectory(t *testing.T, dir string) {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})
}

// readLegacySchemaFrom reads a legacy schema from an already resolved
// contract directory, so it also works while the scenario holds the working
// directory.
func readLegacySchemaFrom(t *testing.T, docs, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(docs, "schemas", "legacy", name+".schema.json"))
	if err != nil {
		t.Skipf("legacy contract schemas are absent: %v", err)
	}
	return b
}

// compareLegacyBytes validates a produced document against its frozen legacy
// schema, then compares it byte for byte with the recorded fixture. A
// formatting-only difference — indentation, key order, escaping, or the
// textual form of a scalar — fails here even when the shape is unchanged.
// The recorded file is validated too, so a hand-edited fixture cannot pass
// by accident.
func compareLegacyBytes(t *testing.T, docs, name, schemaName, ext string, produced []byte) {
	t.Helper()
	validate := func(label string, document []byte) {
		if ext == "jsonl" {
			for i, line := range strings.Split(strings.TrimRight(string(document), "\n"), "\n") {
				if line == "" {
					continue
				}
				if err := contractschema.ValidateJSON(readLegacySchemaFrom(t, docs, schemaName), []byte(line)); err != nil {
					t.Errorf("%s %s line %d does not match its frozen schema: %v", name, label, i+1, err)
				}
			}
			return
		}
		if err := contractschema.ValidateJSON(readLegacySchemaFrom(t, docs, schemaName), document); err != nil {
			t.Errorf("%s %s does not match its frozen legacy schema: %v", name, label, err)
		}
	}
	validate("output", produced)
	path := filepath.Join(docs, "fixtures", "legacy", name+".bytes."+ext)
	if *updateContractFixtures {
		if err := os.WriteFile(path, produced, 0644); err != nil {
			t.Fatalf("record %s: %v", path, err)
		}
		return
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s has no recorded bytes; run go test ./internal/looper -run TestLegacyContractBytes -update-contract-fixtures: %v", name, err)
	}
	validate("recorded fixture", recorded)
	if !bytes.Equal(recorded, produced) {
		at := firstDifference(recorded, produced)
		t.Errorf("%s changed bytes at offset %d. Restore the legacy formatting, or record the change deliberately with -update-contract-fixtures and add a migration entry to docs/AGENT-CONTRACT-MIGRATION.md.\n recorded: %s\n produced: %s",
			name, at, byteWindow(recorded, at), byteWindow(produced, at))
	}
}

// firstDifference returns the offset of the first differing byte, or the
// length of the shorter document when one is a prefix of the other.
func firstDifference(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// byteWindow quotes a bounded window around a difference so a failure stays
// readable while naming the exact bytes that moved.
func byteWindow(document []byte, at int) string {
	const radius = 80
	start, end := max(0, at-radius), min(len(document), at+radius)
	return fmt.Sprintf("%q", document[start:end])
}

// TestLegacyByteFixturesStayDeterministic guards the recording scenario
// itself: two fresh scenarios must produce identical bytes, otherwise the
// recorded fixtures would depend on the run and the byte comparison would
// fail for everyone else. The two scenarios live in different temporary
// directories, so a leaked absolute path also fails here.
func TestLegacyByteFixturesStayDeterministic(t *testing.T) {
	var previous []byte
	for i := 0; i < 2; i++ {
		c := byteFixtureScenario(t)
		var out bytes.Buffer
		cc := c
		cc.Command, cc.JSON = "status", true
		if err := inspect(context.Background(), cc, &out); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			previous = out.Bytes()
			continue
		}
		if !bytes.Equal(previous, out.Bytes()) {
			t.Fatalf("the status document is not deterministic:\n%q\n%q", previous, out.Bytes())
		}
	}
}
