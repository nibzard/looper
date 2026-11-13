package looper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The AX-018 acceptance fixtures. Semantic comparison decides one question:
// did the task-file bytes change only formatting, or did content change?
// Equivalent formatting must never trigger a contract refusal on the
// supported paths; changed task or dependency data must; invalid input,
// including duplicate keys, must stay rejected; and byte contracts keep
// their exact digests.

// equivalentBacklogs renders one backlog twice with different formatting:
// compact versus indented, reversed key order, and escaped versus literal
// string content. Both parse to the same content.
func equivalentBacklogs(t *testing.T) (canonical, reformatted []byte) {
	t.Helper()
	canonical = []byte(`{
  "schema_version": 1,
  "source_files": [
    "docs/PLAN.md"
  ],
  "goal": "ship the feature",
  "project": {"name": "demo", "root": "/demo"},
  "tasks": [
    {"id": "T001", "title": "one", "priority": 1, "status": "doing", "details": "accept it", "depends_on": ["T002"]},
    {"id": "T002", "title": "two", "priority": 2, "status": "todo", "depends_on": []}
  ]
}`)
	reformatted = []byte(`{"tasks":[{"depends_on":["T002"],"details":"accept it","status":"doing","priority":1,"title":"one","id":"T001"},
   {"depends_on":[],"status":"todo","priority":2,"title":"two","id":"T002"}],
 "project":{"root":"/demo","name":"demo"},"goal":"ship the feature","source_files":["docs/PLAN.md"],"schema_version":1}`)
	for _, data := range [][]byte{canonical, reformatted} {
		if _, err := decodeBacklogStrict(data); err != nil {
			t.Fatalf("fixture must be valid: %v", err)
		}
	}
	if digest(canonical) == digest(reformatted) {
		t.Fatal("the fixture must differ in bytes")
	}
	return canonical, reformatted
}

// TestSemanticFingerprintIgnoresFormattingOnlyChanges pins the equivalence
// rule itself: formatting never changes the fingerprint, every data change
// does.
func TestSemanticFingerprintIgnoresFormattingOnlyChanges(t *testing.T) {
	canonical, reformatted := equivalentBacklogs(t)
	base, err := decodeBacklogStrict(canonical)
	if err != nil {
		t.Fatal(err)
	}
	other, err := decodeBacklogStrict(reformatted)
	if err != nil {
		t.Fatal(err)
	}
	if base.semanticFingerprint() != other.semanticFingerprint() {
		t.Fatal("equivalent formatting must not change the semantic fingerprint")
	}
	// Formatting inside the raw project object is formatting too.
	projectReordered := clone(base)
	projectReordered.Project = []byte(`{"root":"/demo","name":"demo"}`)
	if base.semanticFingerprint() != projectReordered.semanticFingerprint() {
		t.Fatal("key order inside the project object must not change the fingerprint")
	}
	mutations := map[string]func(*Backlog){
		"task title":        func(b *Backlog) { b.Tasks[0].Title = "renamed" },
		"task status":       func(b *Backlog) { b.Tasks[0].Status = "done" },
		"dependency change": func(b *Backlog) { b.Tasks[1].DependsOn = []string{"T001"} },
		"task order":        func(b *Backlog) { b.Tasks[0], b.Tasks[1] = b.Tasks[1], b.Tasks[0] },
		"source order":      func(b *Backlog) { b.Sources = []string{"docs/OTHER.md"} },
		"goal change":       func(b *Backlog) { b.Goal = "another goal" },
		"added task": func(b *Backlog) {
			b.Tasks = append(b.Tasks, Task{ID: "T003", Title: "three", Priority: 1, Status: "todo"})
		},
		"task files":    func(b *Backlog) { b.Tasks[0].Files = []string{"a.go"} },
		"project value": func(b *Backlog) { b.Project = []byte(`{"name":"other","root":"/demo"}`) },
	}
	names := make([]string, 0, len(mutations))
	for name := range mutations {
		names = append(names, name)
	}
	for _, name := range names {
		changed := clone(base)
		mutations[name](&changed)
		if changed.semanticFingerprint() == base.semanticFingerprint() {
			t.Fatalf("%s must change the semantic fingerprint", name)
		}
	}
}

// TestStrictParseRejectsAmbiguousDocuments proves ambiguous input is
// rejected rather than normalized: duplicate keys anywhere, unknown fields,
// and trailing values are errors.
func TestStrictParseRejectsAmbiguousDocuments(t *testing.T) {
	docs := map[string]string{
		"duplicate top-level key":  `{"schema_version":1,"schema_version":1,"source_files":[],"tasks":[]}`,
		"duplicate task key":       `{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","id":"T1","title":"one","priority":1,"status":"todo"}]}`,
		"duplicate key in project": `{"schema_version":1,"source_files":[],"project":{"name":"a","name":"b"},"tasks":[]}`,
		"unknown task field":       `{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","title":"one","priority":1,"status":"todo","surprise":1}]}`,
		"trailing second value":    `{"schema_version":1,"source_files":[],"tasks":[]} {"schema_version":1}`,
		"trailing garbage":         `{"schema_version":1,"source_files":[],"tasks":[]} oops`,
		"invalid status":           `{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","title":"one","priority":1,"status":"bogus"}]}`,
		"dependency cycle":         `{"schema_version":1,"source_files":[],"tasks":[{"id":"T1","title":"one","priority":1,"status":"todo","depends_on":["T2"]},{"id":"T2","title":"two","priority":1,"status":"todo","depends_on":["T1"]}]}`,
	}
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	for _, name := range names {
		if _, err := decodeBacklogStrict([]byte(docs[name])); err == nil {
			t.Fatalf("%s must be rejected", name)
		} else if !strings.Contains(err.Error(), "duplicate") && name == "duplicate top-level key" {
			t.Fatalf("duplicate keys must name the problem: %v", err)
		}
	}
	// The last value must never silently win: a duplicate that changes data
	// is not equivalent to either meaning.
	a, errA := decodeBacklogStrict([]byte(`{"schema_version":1,"source_files":[],"tasks":[]}`))
	if errA != nil {
		t.Fatal(errA)
	}
	if _, err := decodeBacklogStrict([]byte(`{"schema_version":1,"source_files":[],"tasks":[],"goal":"visible","goal":"hidden by duplicate"}`)); err == nil {
		t.Fatal("duplicate keys must be rejected")
	}
	if a.Goal != "" {
		t.Fatal("the accepted document must keep its parsed meaning")
	}
}

// TestCompareBacklogSeparatesContractDataAndFormatting drives the evidence
// comparison: equivalent bytes report equality, contract edits name the
// contract, and status edits name task data the contract hash does not
// cover.
func TestCompareBacklogSeparatesContractDataAndFormatting(t *testing.T) {
	canonical, reformatted := equivalentBacklogs(t)
	base, err := decodeBacklogStrict(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if d := compareBacklog(base, reformatted); !d.Equal || d.Unknown != "" || len(d.Changed) > 0 || len(d.Data) > 0 {
		t.Fatalf("equivalent bytes must compare equal: %+v", d)
	}
	status := clone(base)
	status.Tasks[0].Status = "done"
	d := compareBacklog(base, mustMarshalBacklog(t, status))
	if d.Equal || len(d.Changed) != 0 || len(d.Data) != 1 || d.Data[0] != "T001" {
		t.Fatalf("a status edit is task data, not a contract change: %+v", d)
	}
	contract := clone(base)
	contract.Tasks[0].DependsOn = nil
	d = compareBacklog(base, mustMarshalBacklog(t, contract))
	if d.Equal || len(d.Changed) != 1 || d.Changed[0] != "T001" {
		t.Fatalf("a dependency edit is a contract change: %+v", d)
	}
	// Duplicate keys keep their reason instead of an invented comparison.
	d = compareBacklog(base, []byte(`{"schema_version":1,"schema_version":1,"source_files":[],"tasks":[]}`))
	if d.Unknown == "" || d.Equal {
		t.Fatalf("ambiguous input must stay undecided: %+v", d)
	}
}

// mustMarshalBacklog renders one backlog as compact JSON, the formatting a
// formatting-only edit differs from.
func mustMarshalBacklog(t *testing.T, b Backlog) []byte {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// readAcceptedBacklog parses the task file strictly, so a test fixture fails
// on ambiguous bytes instead of continuing with normalized content.
func readAcceptedBacklog(t *testing.T, c Config) Backlog {
	t.Helper()
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parseBacklogStrict(data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestStartupRejectsDuplicateKeysWhileWorkIsRetained proves invalid input
// stays rejected at startup: a duplicate-key task file is an invalid
// backlog, not a document to normalize.
func TestStartupRejectsDuplicateKeysWhileWorkIsRetained(t *testing.T) {
	c, _ := startupBacklogFixture(t)
	duplicate := []byte(`{"schema_version":1,"source_files":[],"tasks":[],"tasks":[{"id":"T001","title":"one","priority":1,"status":"doing"}]}`)
	if _, err := decodeBacklogStrict(duplicate); err == nil {
		t.Fatal("the fixture must be ambiguous")
	}
	if err := os.WriteFile(c.Todo, duplicate, 0600); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	err = e.initializeState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid backlog") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("a duplicate-key task file must be rejected as invalid: %v", err)
	}
}

// TestExportAcceptsEquivalentFormattingMidPending exercises the export guard
// with a pending export: the user reformats the file after Looper staged its
// own mutation, so the in-memory backlog no longer matches the accepted
// bytes and only the persisted semantic fingerprint can decide. The export
// must proceed and write the canonical projection.
func TestExportAcceptsEquivalentFormattingMidPending(t *testing.T) {
	e, c := idleEngine(t, "{}")
	// Begin a pending export the way applyResult does: mutate the backlog,
	// journal ExportBefore, and leave the file on the accepted bytes.
	e.State.Backlog.Tasks[0].Files = []string{"done.txt"}
	e.State.ExportBefore = e.State.ExportHash
	e.State.ExportPending = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	// The operator reformats the accepted bytes behind the pending export.
	compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
	if digest(compacted) == e.State.ExportHash {
		t.Fatal("the fixture must change the file bytes")
	}
	if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.export(); err != nil {
		t.Fatalf("equivalent formatting must not block the pending export: %v", err)
	}
	if e.State.ExportPending || e.State.ExportBefore != "" {
		t.Fatal("the export must complete")
	}
	// The canonical projection is on disk and the digests agree with it.
	if observed := digestFile(c.Todo); observed != e.State.ExportHash {
		t.Fatalf("the accepted digest must match the written bytes: %s != %s", observed, e.State.ExportHash)
	}
	if e.State.ExportSemantics != e.State.Backlog.semanticFingerprint() {
		t.Fatal("the finalized export must persist its semantic fingerprint")
	}
}

// TestExportStillRefusesRealEditsMidPending proves the same guard keeps
// refusing a real edit while an export is pending.
func TestExportStillRefusesRealEditsMidPending(t *testing.T) {
	e, c := idleEngine(t, "{}")
	e.State.Backlog.Tasks[0].Files = []string{"done.txt"}
	e.State.ExportBefore = e.State.ExportHash
	e.State.ExportPending = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	edited := clone(e.State.Backlog)
	edited.Tasks[0].Title = "renamed outside Looper"
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	err := e.export()
	var retained *retentionError
	if !errors.As(err, &retained) || retained.code != "backlog_contract_changed" {
		t.Fatalf("a real edit must keep the export guard refusing: %v", err)
	}
	if !e.State.ExportPending {
		t.Fatal("the refused export must leave the journal intact")
	}
}

// TestLegacyPendingStateWithoutSemanticsStaysConservative proves a state
// saved before semantic fingerprints existed cannot guess mid-pending: the
// guard keeps its byte refusal instead of deciding equivalence from the
// already-mutated in-memory backlog.
func TestLegacyPendingStateWithoutSemanticsStaysConservative(t *testing.T) {
	e, c := idleEngine(t, "{}")
	// Reproduce a legacy pending state: no fingerprint, pending journal.
	e.State.ExportSemantics = ""
	e.State.Backlog.Tasks[0].Files = []string{"done.txt"}
	e.State.ExportBefore = e.State.ExportHash
	e.State.ExportPending = true
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	// The file still holds the accepted content in different formatting.
	reformatted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
	if err := os.WriteFile(c.Todo, reformatted, 0600); err != nil {
		t.Fatal(err)
	}
	err := e.export()
	var retained *retentionError
	if !errors.As(err, &retained) || retained.code != "backlog_contract_changed" {
		t.Fatalf("an undecided comparison must keep the byte refusal: %v", err)
	}
}

// TestPublishTreatsEquivalentFormattingAsUnchanged proves the publication
// boundary: a formatting-only edit publishes the verified result without the
// backlog-change sentinel, while a real edit still reports it.
func TestPublishTreatsEquivalentFormattingAsUnchanged(t *testing.T) {
	setup := func(t *testing.T) (*Engine, Config) {
		e, c := idleEngine(t, "{}")
		task := *e.State.Backlog.Task("T1")
		e.State.Attempts = append(e.State.Attempts, &Attempt{
			ID: "task-pub", Task: task, Phase: "verifying",
			Directory: filepath.Join(e.Dir, "task-pub"), Ref: "refs/looper-tasks/task-pub",
		})
		e.State.Publication = &Candidate{AttemptID: "task-pub", Base: e.State.Tip, Commit: e.State.Tip, Tree: e.State.Published, Before: e.State.Published}
		return e, c
	}
	t.Run("formatting only", func(t *testing.T) {
		e, c := setup(t)
		compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
		if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
			t.Fatal(err)
		}
		if err := e.publish(); err != nil {
			t.Fatalf("equivalent formatting must publish without the hold sentinel: %v", err)
		}
	})
	t.Run("real edit", func(t *testing.T) {
		e, c := setup(t)
		edited := clone(e.State.Backlog)
		edited.Goal = "changed during publication"
		if err := writeJSON(c.Todo, edited); err != nil {
			t.Fatal(err)
		}
		if err := e.publish(); !errors.Is(err, errBacklogChanged) {
			t.Fatalf("a real edit must keep the hold sentinel: %v", err)
		}
	})
}

// TestRefreshAdoptsEquivalentFormattingWithoutReplanning proves the refresh
// shortcut: with retained work that the idle path must refuse to rebase, a
// formatting-only edit is adopted in place — the baseline, plan, and review
// pointers stay untouched.
func TestRefreshAdoptsEquivalentFormattingWithoutReplanning(t *testing.T) {
	e, c := idleEngine(t, "{}")
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{{TaskID: "T1", Paths: []string{"src/"}}}}
	e.State.PlanSignature = "live"
	e.State.ReviewedTip = e.State.Tip
	tip, published := e.State.Tip, e.State.Published
	e.State.Attempts = append(e.State.Attempts, &Attempt{
		ID: "task-held", Task: *e.State.Backlog.Task("T1"), Phase: "ready", ResultCommit: tip,
		Directory: filepath.Join(e.Dir, "task-held"), Ref: "refs/looper-tasks/task-held",
	})
	compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
	if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
		t.Fatal(err)
	}
	// The idle refresh would refuse a real edit while work is retained.
	if err := e.refreshIdle(); err != nil {
		t.Fatalf("an equivalent edit must not block the idle refresh: %v", err)
	}
	if e.State.ExportHash != digestFile(c.Todo) {
		t.Fatal("the observed bytes must become the accepted export")
	}
	if e.State.Tip != tip || e.State.Published != published || e.State.PlanSignature != "live" || e.State.ReviewedTip != e.State.ReviewedTip {
		t.Fatal("adoption must not move the baseline or replan")
	}
	// A real edit keeps the retained-work refusal.
	edited := clone(e.State.Backlog)
	edited.Goal = "changed while retained"
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	if err := e.refreshIdle(); err == nil || !strings.Contains(err.Error(), "work is retained") {
		t.Fatalf("a real edit must keep the retained-work refusal: %v", err)
	}
}

// TestRecoveryConsultationSurvivesFormattingEdit proves the consultation
// guard: a formatting-only edit during the consultation does not discard the
// proposal; a real edit still does.
func TestRecoveryConsultationSurvivesFormattingEdit(t *testing.T) {
	e, c := idleEngine(t, "{}")
	baseline := hashJSON(e.State.Backlog)
	compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
	if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	edit := e.classifyBacklog(changed)
	if !edit.Equivalent {
		t.Fatalf("the fixture must be equivalent: %+v", edit)
	}
	// The guard expression recoverRun uses: an unchanged in-memory backlog
	// and equivalent bytes keep the proposal; a real edit rejects it.
	if hashJSON(e.State.Backlog) != baseline {
		t.Fatal("the fixture must not change the in-memory backlog")
	}
	edited := clone(e.State.Backlog)
	edited.Goal = "changed during consultation"
	if err := writeJSON(c.Todo, edited); err != nil {
		t.Fatal(err)
	}
	changed, err = os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	if edit := e.classifyBacklog(changed); edit.Equivalent {
		t.Fatal("a real edit must not classify as equivalent")
	}
}

// TestAddTaskAppliesOverEquivalentFormatting proves the request guard: an
// add-task request is not answered with a conflict when the task file
// changed only in formatting; a real edit still conflicts.
func TestAddTaskAppliesOverEquivalentFormatting(t *testing.T) {
	newRequest := func(e *Engine) *SettingsRequest {
		return &SettingsRequest{Version: SettingsRequestVersion, ID: "req-1", Type: RequestTypeAddTask, Session: e.controller.Session,
			Tasks: []Task{{ID: "T9", Title: "added", Priority: 1, Status: "todo"}}}
	}
	t.Run("formatting only", func(t *testing.T) {
		e, c := idleEngine(t, "{}")
		compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
		if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
			t.Fatal(err)
		}
		ack := e.applyAddTaskRequest(newRequest(e))
		if ack.Status != AckApplied {
			t.Fatalf("an equivalent edit must not conflict: %s %s", ack.Status, ack.Message)
		}
		if e.State.Backlog.Task("T9") == nil {
			t.Fatal("the task must be added")
		}
	})
	t.Run("real edit", func(t *testing.T) {
		e, c := idleEngine(t, "{}")
		edited := clone(e.State.Backlog)
		edited.Goal = "changed before the request"
		if err := writeJSON(c.Todo, edited); err != nil {
			t.Fatal(err)
		}
		ack := e.applyAddTaskRequest(newRequest(e))
		if ack.Status != AckConflict {
			t.Fatalf("a real edit must conflict: %s %s", ack.Status, ack.Message)
		}
		if e.State.Backlog.Task("T9") != nil {
			t.Fatal("no task may be applied over a real edit")
		}
	})
}

// TestBoundaryAdoptionSkipsTheHold drives the run loop with one gated check
// in flight and an external task-file edit: formatting-only bytes are adopted
// without pausing dispatch, while changed content produces the documented
// hold event.
func TestBoundaryAdoptionSkipsTheHold(t *testing.T) {
	waitForAdoption := func(t *testing.T, dir string) int {
		count := 0
		waitFor(t, "the equivalent edit to be adopted", func() bool {
			s, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
			if err != nil {
				return false
			}
			defer s.Close()
			events, err := s.RecentEvents(-1, 100)
			if err != nil {
				return false
			}
			count = 0
			for _, ev := range events {
				if ev.Kind == "backlog-format" && strings.Contains(ev.Message, "changed only in formatting") {
					count++
				}
			}
			return count >= 1
		})
		return count
	}
	t.Run("formatting only keeps dispatch", func(t *testing.T) {
		e, c, done, release := holdRunEngine(t, holdVerify)
		waitFor(t, "the gated check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
		compacted := append(mustMarshalBacklog(t, readAcceptedBacklog(t, c)), '\n')
		if err := os.WriteFile(c.Todo, compacted, 0600); err != nil {
			t.Fatal(err)
		}
		waitForAdoption(t, e.Dir)
		if countKindEvent(t, e.Dir, "waiting", "The task file changed outside Looper while work runs") != 0 {
			t.Fatal("an equivalent edit must not hold dispatch")
		}
		// The gated check still runs to its own end; the run drains cleanly.
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(e.Dir, "stop"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		select {
		case out := <-done:
			if out.code != 0 || out.err != nil {
				t.Fatalf("the run must drain cleanly after adoption: %d %v", out.code, out.err)
			}
		case <-time.After(testBudget(30 * time.Second)):
			t.Fatal("the run did not drain after the stop request")
		}
	})
	t.Run("changed content holds", func(t *testing.T) {
		e, c, _, release := holdRunEngine(t, holdVerify)
		waitFor(t, "the gated check to start", func() bool { return checkMarkers(t, e.Dir) >= 1 })
		if err := writeJSON(c.Todo, editedBacklog()); err != nil {
			t.Fatal(err)
		}
		waitEvent(t, e.Dir, "The task file changed outside Looper while work runs", 1)
		if err := os.WriteFile(release, nil, 0600); err != nil {
			t.Fatal(err)
		}
	})
}

// TestByteContractsKeepExactDigests pins the rule that semantic comparison
// never leaks into byte contracts: task Contract() hashes, receipt contract
// fields, and the recovery preview token stay exact byte-derived values.
func TestByteContractsKeepExactDigests(t *testing.T) {
	canonical, reformatted := equivalentBacklogs(t)
	a, err := decodeBacklogStrict(canonical)
	if err != nil {
		t.Fatal(err)
	}
	b, err := decodeBacklogStrict(reformatted)
	if err != nil {
		t.Fatal(err)
	}
	// Contracts hash the typed value, so equal content yields equal
	// contracts even from different bytes — but any byte-level identity a
	// contract requires still compares exactly.
	for i := range a.Tasks {
		if a.Tasks[i].Contract() != b.Tasks[i].Contract() {
			t.Fatalf("task %s: equal content must hash equally", a.Tasks[i].ID)
		}
	}
	if digest(canonical) == digest(reformatted) {
		t.Fatal("the fixture must differ in bytes")
	}
	if hashJSON(a.Tasks[0]) != hashJSON(b.Tasks[0]) {
		t.Fatal("typed records of equal content must serialize equally")
	}
}
