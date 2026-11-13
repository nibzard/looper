package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
	"github.com/nibzard/looper/internal/ui"
)

// The AX-010 acceptance fixtures. Every fixture drives the public CLI: the
// request queries must answer from durable receipts while changing nothing,
// and the missing-identifier, unknown-cursor, stopped-controller, and large
// history cases must stay bounded, precise, and actionable.

// requestRecordContractSchema reads the published record schema, anchored at
// this file's directory because fixtures chdir into temporary projects.
func requestRecordContractSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "request-record.schema.json"))
	if err != nil {
		t.Skipf("request-record schema is absent: %v", err)
	}
	return b
}

// doctorContractSchema reads the published version 2 doctor schema.
func doctorContractSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "doctor.schema.json"))
	if err != nil {
		t.Skipf("doctor schema is absent: %v", err)
	}
	return b
}

// requestsProject seeds one stopped run whose state carries the receipts the
// fixtures need. A stopped run has no controller metadata, exactly like a
// controller that shut down and removed its own record.
func requestsProject(t *testing.T, mutate func(*State)) Config {
	t.Helper()
	base := runsBase(t)
	root := filepath.Join(filepath.Dir(base), "req-"+strings.ToLower(strings.TrimPrefix(t.Name(), "Test")))
	c := runsProjectAt(t, root)
	seedSavedRun(t, c, mutate)
	return c
}

// runRequestsCLI drives `looper requests` through Main and returns the exit
// code with the decoded document. The JSON form must validate against the
// published schema, carry no human text, and stay within its byte budget.
func runRequestsCLI(t *testing.T, args ...string) (int, *requestRecordDocument, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Main(append([]string{"requests"}, args...), &out, &stderr)
	jsonForm := false
	for _, a := range args {
		if a == "--json" {
			jsonForm = true
		}
	}
	if !jsonForm || code != 0 {
		return code, nil, out.String() + stderr.String()
	}
	if stderr.Len() > 0 {
		t.Fatalf("the record JSON mixes human text into stderr: %q", stderr.String())
	}
	var doc requestRecordDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one record document: %v\n%s", err, out.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	if err := contractschema.ValidateJSON(requestRecordContractSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the emitted record document violates its schema: %v\n%s", err, out.String())
	}
	if out.Len() > requestsBytesBudget {
		t.Fatalf("record document is %d bytes, budget %d", out.Len(), requestsBytesBudget)
	}
	return code, &doc, out.String()
}

// requestErrorDoc decodes one emitted error document.
func requestErrorDoc(t *testing.T, out string) *errorDocument {
	t.Helper()
	var doc errorDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out)
	}
	return &doc
}

// ago formats one timestamp the given seconds back.
func ago(seconds int) string {
	return time.Now().UTC().Add(-time.Duration(seconds) * time.Second).Format(time.RFC3339Nano)
}

// seedRequestReceipts builds the fixture receipts: one waiting plan import
// with its coverage record, one applied settings change whose verify command
// applies on the next controller run, one refused submission with its error,
// and one terminal add-task result.
func seedRequestReceipts(s *State) {
	applied := ago(400)
	waiting := ago(90)
	s.Imports = []*ImportRecord{{
		RequestID: "import-wait", Status: ImportWaiting, Enqueued: waiting,
		SourcePaths: []string{"plan.md"},
	}}
	s.Requests = []*RequestRecord{
		{
			ID: "import-wait", Type: PublicRequestAddPlan, State: RequestWaiting,
			Session: "run-reqseed00001", SubmittedAt: waiting,
			WaitReason: "active_work", WaitSince: ago(40),
			WaitDetail: "The import applies at the next boundary after T015 finishes.",
		},
		{
			ID: "settings-1", Type: PublicRequestSettings, State: RequestApplied,
			Session: "run-reqseed00001", TargetSession: "run-reqseed00001",
			SubmittedAt: applied, DecidedAt: ago(398),
			Fields:  []string{"agent", "verify_command"},
			Summary: "applied: Saved the profile and applied live settings.",
			Ack: &SettingsAck{
				Version: SettingsRequestVersion, ID: "settings-1", Status: AckApplied,
				Outcomes: map[string]string{
					"agent":          "saved; applied",
					"verify_command": "saved; applies on the next controller run",
				},
			},
		},
		{
			ID: "add-refused", Type: PublicRequestAddTask, State: RequestRejected,
			Session: "run-reqseed00001", SubmittedAt: ago(60), DecidedAt: ago(59),
			Error: "the request targets an earlier controller session; resubmit it after reloading state",
		},
		{
			ID: "add-done", Type: PublicRequestAddTask, State: RequestApplied,
			Session: "run-reqseed00001", SubmittedAt: ago(30), DecidedAt: ago(29),
			AffectedTasks: []string{"T070"}, Summary: "Added 1 task(s): T070",
		},
	}
}

// TestRequestRecordQueriesByIdentifier covers the single-record query: the
// state, age, wait reason, coverage, outcome, and error evidence all come
// from the durable receipts of a stopped run, with no acknowledgement
// directory and no direct state access.
func TestRequestRecordQueriesByIdentifier(t *testing.T) {
	requestsProject(t, seedRequestReceipts)

	t.Run("waiting import names its prerequisite and coverage source", func(t *testing.T) {
		code, doc, _ := runRequestsCLI(t, "import-wait", "--contract", "v2", "--json")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		rec := doc.Request
		if rec == nil || rec.State != RequestWaiting || rec.Type != PublicRequestAddPlan {
			t.Fatalf("waiting record: %+v", rec)
		}
		if rec.WaitingOn == nil || rec.WaitingOn.Reason != "active_work" || rec.WaitingOn.AgeSeconds == nil || *rec.WaitingOn.AgeSeconds < 30 {
			t.Fatalf("wait evidence: %+v", rec.WaitingOn)
		}
		if rec.AgeSeconds < 60 || rec.ControllerSession != "run-reqseed00001" {
			t.Fatalf("age/session: %+v", rec)
		}
		if doc.Continuation.Command != "looper requests import-wait --contract v2" {
			t.Fatalf("continuation: %+v", doc.Continuation)
		}
		if doc.List != nil {
			t.Fatalf("a single record carries no list summary: %+v", doc.List)
		}
	})

	t.Run("applied settings names fields and restart boundary", func(t *testing.T) {
		code, doc, _ := runRequestsCLI(t, "settings-1", "--contract", "v2", "--json")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		rec := doc.Request
		if rec.State != RequestApplied || rec.Error != nil {
			t.Fatalf("applied record: %+v", rec)
		}
		if rec.Result == nil || len(rec.Result.AppliedFields) != 2 || len(rec.Result.RestartRequiredFields) != 1 || rec.Result.RestartRequiredFields[0] != "verify_command" {
			t.Fatalf("applied fields: %+v", rec.Result)
		}
	})

	t.Run("rejected request carries safe error evidence", func(t *testing.T) {
		code, doc, _ := runRequestsCLI(t, "add-refused", "--contract", "v2", "--json")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		rec := doc.Request
		if rec.State != RequestRejected || rec.Error == nil {
			t.Fatalf("rejected record: %+v", rec)
		}
		if rec.Error.Code != "request_rejected" || rec.Error.Retry != retryAfterChange {
			t.Fatalf("error evidence: %+v", rec.Error)
		}
		if len(rec.Error.Evidence) == 0 || rec.Error.Evidence[0].Command == "" {
			t.Fatalf("the error must name its read-only route: %+v", rec.Error.Evidence)
		}
	})

	t.Run("applied add-task names its added tasks", func(t *testing.T) {
		_, doc, _ := runRequestsCLI(t, "add-done", "--contract", "v2", "--json")
		if doc.Request.Result == nil || len(doc.Request.Result.AddedTaskIDs) != 1 || doc.Request.Result.AddedTaskIDs[0] != "T070" {
			t.Fatalf("added tasks: %+v", doc.Request.Result)
		}
		if len(doc.Request.AffectedTasks) != 1 {
			t.Fatalf("affected tasks: %+v", doc.Request.AffectedTasks)
		}
	})

	t.Run("plain rendering answers the same facts", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Main([]string{"requests", "import-wait", "--contract", "v2"}, &out, &stderr)
		if code != exitOK {
			t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
		}
		for _, want := range []string{"import-wait", "waiting", "active_work", "add-plan"} {
			if !strings.Contains(out.String(), want) {
				t.Fatalf("plain record lost %q:\n%s", want, out.String())
			}
		}
	})
}

// TestRequestRecordPlanCoverage covers the coverage an applied import
// reports: the added tasks, the covered requirements, and the conflicts a
// failed analysis named.
func TestRequestRecordPlanCoverage(t *testing.T) {
	requestsProject(t, func(s *State) {
		seedRequestReceipts(s)
		s.Imports = append(s.Imports, &ImportRecord{
			RequestID: "import-covered", Status: ImportApplied,
			Enqueued: ago(120), Applied: ago(100),
			AddedTasks:  []string{"T070", "T071"},
			Covered:     []CoveredRef{{Requirement: "Keep plain status readable when piped", TaskID: "T012", Evidence: "docs/UI-PLAN.md Part 1"}},
			Adjustments: []string{"T012 also covers the piped liveness line"},
		})
		s.Requests = append(s.Requests, &RequestRecord{
			ID: "import-covered", Type: PublicRequestAddPlan, State: RequestApplied,
			Session: "run-reqseed00001", SubmittedAt: ago(120), DecidedAt: ago(100),
			AffectedTasks: []string{"T070", "T071"}, Summary: "2 task(s) added, 1 covered (T070, T071)",
		})
	})
	_, doc, _ := runRequestsCLI(t, "import-covered", "--contract", "v2", "--json")
	rec := doc.Request
	if rec.PlanCoverage == nil {
		t.Fatalf("applied import must report coverage: %+v", rec)
	}
	if len(rec.PlanCoverage.AddedTasks) != 2 || len(rec.PlanCoverage.Covered) != 1 {
		t.Fatalf("coverage: %+v", rec.PlanCoverage)
	}
	if ref := rec.PlanCoverage.Covered[0]; ref.TaskID != "T012" || ref.Requirement == "" {
		t.Fatalf("covered reference: %+v", ref)
	}
	if len(rec.Result.AddedTaskIDs) != 2 {
		t.Fatalf("added ids: %+v", rec.Result)
	}
}

// TestRequestRecordListPagesAndBounds covers the bounded list: one page of
// the durable records, the omitted count, the continuation, and paging
// through a history larger than one page with no gaps or duplicates.
func TestRequestRecordListPagesAndBounds(t *testing.T) {
	seed := func(s *State) {
		seedRequestReceipts(s)
	}
	requestsProject(t, seed)

	t.Run("first page lists the retained records", func(t *testing.T) {
		code, doc, _ := runRequestsCLI(t, "--contract", "v2", "--json")
		if code != exitOK {
			t.Fatalf("exit %d", code)
		}
		if doc.Request != nil || len(doc.Requests) != 4 {
			t.Fatalf("list form: %d records, single %v", len(doc.Requests), doc.Request != nil)
		}
		if doc.List == nil || doc.List.Returned != 4 || doc.List.HasMore || doc.List.Omitted["records"] != 0 {
			t.Fatalf("list summary: %+v", doc.List)
		}
		// Receipt order is the order the decisions committed: oldest first.
		if doc.Requests[0].ID != "import-wait" || doc.Requests[3].ID != "add-done" {
			t.Fatalf("page order: %s, %s", doc.Requests[0].ID, doc.Requests[3].ID)
		}
		if doc.Requests[0].WaitingOn == nil || doc.Requests[0].WaitingOn.Reason != "active_work" {
			t.Fatalf("waiting evidence in list: %+v", doc.Requests[0])
		}
	})

	t.Run("paging reaches every record exactly once", func(t *testing.T) {
		seen := map[string]bool{}
		pages := 0
		after := ""
		for {
			args := []string{"--contract", "v2", "--json", "--limit", "1"}
			if after != "" {
				args = append(args, "--after", after)
			}
			code, doc, out := runRequestsCLI(t, args...)
			if code != exitOK {
				t.Fatalf("exit %d: %s", code, out)
			}
			pages++
			for _, rec := range doc.Requests {
				if seen[rec.ID] {
					t.Fatalf("record %s returned twice", rec.ID)
				}
				seen[rec.ID] = true
			}
			if !doc.List.HasMore {
				break
			}
			if doc.Continuation.List == "" {
				t.Fatal("a page with more records must name the next call")
			}
			after = doc.Requests[len(doc.Requests)-1].ID
			if pages > 10 {
				t.Fatal("paging did not terminate")
			}
		}
		if len(seen) != 4 || pages != 4 {
			t.Fatalf("paged %d pages over %d records", pages, len(seen))
		}
	})
}

// TestRequestRecordListLargeHistoryBounds covers a receipt history far past
// the retention bound: every page stays within the byte budget, the omitted
// count names what the page left out, and paging reaches every record.
func TestRequestRecordListLargeHistoryBounds(t *testing.T) {
	total := 250
	requestsProject(t, func(s *State) {
		seedRequestReceipts(s)
		for i := 0; i < total; i++ {
			s.Requests = append(s.Requests, &RequestRecord{
				ID: fmt.Sprintf("bulk-%03d&of-%d", i, total), Type: PublicRequestAddTask,
				State: RequestApplied, Session: "run-reqseed00001",
				SubmittedAt: ago(1000 + i), DecidedAt: ago(999 + i),
				AffectedTasks: []string{fmt.Sprintf("T%03d-with-a-long-identifier-%d", i, i)},
				Summary:       strings.Repeat("a long recorded summary line ", 12),
			})
		}
	})
	limit := 100
	code, doc, out := runRequestsCLI(t, "--contract", "v2", "--json", "--limit", fmt.Sprint(limit))
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if doc.List == nil {
		t.Fatal("the list form must carry its summary")
	}
	if doc.List.Returned <= 0 {
		t.Fatalf("the page must return records: %+v", doc.List)
	}
	if doc.List.Omitted["records"] != total+4-doc.List.Returned {
		t.Fatalf("omitted %d, want %d", doc.List.Omitted["records"], total+4-doc.List.Returned)
	}
	if !doc.List.HasMore || doc.Continuation.List == "" {
		t.Fatalf("more records must name their continuation: %+v", doc.List)
	}
	// Page through the whole history; every record is reachable.
	seen := map[string]bool{}
	after := ""
	pages := 0
	for {
		args := []string{"--contract", "v2", "--json", "--limit", "100"}
		if after != "" {
			args = append(args, "--after", after)
		}
		code, doc, out := runRequestsCLI(t, args...)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, out)
		}
		pages++
		for _, rec := range doc.Requests {
			seen[rec.ID] = true
		}
		if !doc.List.HasMore {
			break
		}
		after = doc.Requests[len(doc.Requests)-1].ID
		// The byte budget may shrink each page well below the requested
		// limit; only termination and coverage are pinned here.
		if pages > 400 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != total+4 {
		t.Fatalf("paging reached %d of %d records", len(seen), total+4)
	}
}

// TestRequestRecordListIncludesSubmittedEnvelopes covers requests whose
// envelope exists but that no receipt has answered: the public record is the
// envelope itself, with an explicit submitted state.
func TestRequestRecordListIncludesSubmittedEnvelopes(t *testing.T) {
	c := requestsProject(t, seedRequestReceipts)
	_, runtime := projectPaths(c)
	if err := os.MkdirAll(requestDir(runtime), 0700); err != nil {
		t.Fatal(err)
	}
	envelope := SettingsRequest{
		Version: SettingsRequestVersion, ID: "env-queued", Type: RequestTypeAddTask,
		Session: "run-reqseed00001", CreatedAt: ago(25),
		Tasks: []Task{{ID: "T080", Title: "queued", Priority: 1, Status: "todo"}},
	}
	if err := writeJSON(filepath.Join(requestDir(runtime), "env-queued.json"), envelope); err != nil {
		t.Fatal(err)
	}
	code, doc, _ := runRequestsCLI(t, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	var submitted *requestRecordView
	for i := range doc.Requests {
		if doc.Requests[i].ID == "env-queued" {
			submitted = &doc.Requests[i]
		}
	}
	if submitted == nil || submitted.State != RequestSubmitted || submitted.Type != PublicRequestAddTask {
		t.Fatalf("submitted envelope record: %+v", submitted)
	}
	if submitted.AgeSeconds < 20 {
		t.Fatalf("the envelope's own age must surface: %+v", submitted)
	}
	// The single-record query answers for the envelope too.
	code, single, _ := runRequestsCLI(t, "env-queued", "--contract", "v2", "--json")
	if code != exitOK || single.Request.State != RequestSubmitted {
		t.Fatalf("single envelope query: exit %d, %+v", code, single.Request)
	}
}

// TestRequestQueryPreciseFailures covers the precise results the contract
// names: a missing identifier, an unknown continuation identifier, a missing
// run, and the legacy contract refusal.
func TestRequestQueryPreciseFailures(t *testing.T) {
	requestsProject(t, seedRequestReceipts)

	t.Run("missing identifier", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Main([]string{"requests", "never-submitted", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d, want %d", code, exitOperationFailed)
		}
		doc := requestErrorDoc(t, out.String())
		if doc.Error.Code != "request_not_found" {
			t.Fatalf("code %q", doc.Error.Code)
		}
		if doc.Error.Observed["retained_records"].(float64) != 4 {
			t.Fatalf("the error must name what the run holds: %v", doc.Error.Observed)
		}
		found := false
		for _, a := range doc.Error.NextActions {
			if strings.Contains(a.Command, "looper requests --contract v2 --json") {
				found = true
			}
		}
		if !found {
			t.Fatalf("the error must route to the list: %+v", doc.Error.NextActions)
		}
	})

	t.Run("unknown continuation identifier", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Main([]string{"requests", "--contract", "v2", "--json", "--after", "pruned-id"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d, want %d", code, exitOperationFailed)
		}
		doc := requestErrorDoc(t, out.String())
		if doc.Error.Code != "request_after_unknown" {
			t.Fatalf("code %q", doc.Error.Code)
		}
	})

	t.Run("no saved run", func(t *testing.T) {
		base := runsBase(t)
		c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "empty"))
		_ = c
		var out, stderr bytes.Buffer
		code := Main([]string{"requests", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("exit %d, want %d", code, exitUnavailable)
		}
		doc := requestErrorDoc(t, out.String())
		if doc.Error.Code != "run_not_found" {
			t.Fatalf("code %q", doc.Error.Code)
		}
	})

	t.Run("legacy contract refuses the json form", func(t *testing.T) {
		var out, stderr bytes.Buffer
		code := Main([]string{"requests", "--json"}, &out, &stderr)
		// The legacy contract keeps its exit meanings: the refusal lands in
		// exit 1 with the versioned document named, exactly as `runs --json`
		// refuses.
		if code != exitOperationFailed {
			t.Fatalf("exit %d, want %d", code, exitOperationFailed)
		}
		if !strings.Contains(stderr.String(), "contract version 2") {
			t.Fatalf("the refusal must name the versioned document: %q", stderr.String())
		}
	})

	t.Run("option contradictions are usage failures", func(t *testing.T) {
		for _, args := range [][]string{
			{"requests", "import-wait", "--after", "add-done", "--contract", "v2"},
			{"requests", "import-wait", "--limit", "5", "--contract", "v2"},
			{"requests", "--limit", "101", "--contract", "v2"},
			{"requests", "--full", "--contract", "v2"},
			{"requests", "one", "two", "--contract", "v2"},
		} {
			var out, stderr bytes.Buffer
			code := Main(args, &out, &stderr)
			if code != exitUsage {
				t.Fatalf("%v exit %d, want %d", args, code, exitUsage)
			}
		}
	})
}

// TestRequestQueryLeavesStoppedRunUntouched pins the read-only discipline:
// the request query on a cleanly stopped run creates no file and changes no
// record, because the records are durable evidence, not side effects.
func TestRequestQueryLeavesStoppedRunUntouched(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "untouched"))
	logs := seedSavedRun(t, c, seedRequestReceipts)
	runtime := filepath.Join(logs, "runtime")
	assertNoSidecars(t, runtime)
	before := snapshotBase(t, runtime)
	for _, args := range [][]string{
		{"requests", "--contract", "v2", "--json"},
		{"requests", "import-wait", "--contract", "v2", "--json"},
		{"doctor", "--contract", "v2", "--json"},
	} {
		var out, stderr bytes.Buffer
		code := Main(append([]string(nil), args...), &out, &stderr)
		if code != exitOK {
			t.Fatalf("%v exit %d: %s%s", args, code, out.String(), stderr.String())
		}
		assertRuntimeUntouched(t, runtime, before, args[0])
	}
	assertNoSidecars(t, runtime)
}

// TestRequestRecordCorruptAndUnroutableEvidence covers two records a healthy
// run never writes: a receipt whose state the registry does not define stays
// queryable as a failed record that names the unrecognized state, and an
// identifier too escape-heavy to embed keeps a valid continuation command.
func TestRequestRecordCorruptAndUnroutableEvidence(t *testing.T) {
	longID := "id-" + strings.Repeat("&", 40)
	requestsProject(t, func(s *State) {
		s.Requests = []*RequestRecord{
			{ID: "odd-state", Type: PublicRequestSettings, State: "vanished", Session: "run-reqseed00001", SubmittedAt: ago(10)},
			{ID: longID, Type: PublicRequestSettings, State: RequestApplied, Session: "run-reqseed00001", SubmittedAt: ago(5)},
		}
	})
	// The unregistered state renders as a failed record whose error names it.
	code, doc, _ := runRequestsCLI(t, "odd-state", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("exit %d", code)
	}
	if doc.Request.State != RequestFailed || doc.Request.Error == nil || !strings.Contains(doc.Request.Error.Message, "vanished") {
		t.Fatalf("unregistered state: %+v %+v", doc.Request, doc.Request.Error)
	}
	// The escape-heavy identifier keeps a schema-valid continuation that does
	// not embed it.
	code, doc, out := runRequestsCLI(t, longID, "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.Contains(doc.Continuation.Command, "&&") || strings.Contains(doc.Continuation.List, "&&") {
		t.Fatalf("the continuation must not embed an unroutable identifier: %+v", doc.Continuation)
	}
	if doc.Continuation.Command == "" {
		t.Fatal("the continuation command stays present")
	}
	// The list page carries the same records within its budget.
	_, list, _ := runRequestsCLI(t, "--contract", "v2", "--json")
	if len(list.Requests) != 2 || list.Requests[0].State != RequestFailed {
		t.Fatalf("list over corrupt records: %+v", list.Requests)
	}
}

// TestDoctorCompactServesRequestSummaries covers the version 2 doctor
// document: the same findings shape plus the bounded request summaries, and
// the exit-code policy the transport registry states — findings are
// observation content, so the command exits 0.
func TestDoctorCompactServesRequestSummaries(t *testing.T) {
	requestsProject(t, func(s *State) {
		seedRequestReceipts(s)
		s.Backlog.Tasks = append(s.Backlog.Tasks, Task{ID: "T015", Title: "blocked work", Priority: 1, Status: "blocked", Blockers: []string{"T001"}})
	})
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if err := contractschema.ValidateJSON(doctorContractSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the version 2 doctor violates its schema: %v\n%s", err, out.String())
	}
	var doc doctorCompactDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc.Kind != doctorCompactKind || doc.ContractVersion != ContractVersionV2 {
		t.Fatalf("identity: %+v", doc)
	}
	if doc.Requests.Waiting != 1 || doc.Requests.Pending != 0 {
		t.Fatalf("request summary: %+v", doc.Requests)
	}
	if len(doc.Requests.WaitingOn) != 1 {
		t.Fatalf("waiting list: %+v", doc.Requests.WaitingOn)
	}
	entry := doc.Requests.WaitingOn[0]
	if entry.ID != "import-wait" || entry.State != RequestWaiting || entry.Reason != "active_work" {
		t.Fatalf("waiting entry: %+v", entry)
	}
	if doc.Detail.Requests == "" {
		t.Fatal("the summary must name its detail route")
	}
	found := false
	for _, f := range doc.Findings {
		if f.Code == "task_blocked" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the findings shape must keep its diagnoses: %+v", doc.Findings)
	}
}

// TestDoctorCompactBoundsWaitingList covers the bounded waiting list: many
// waiting requests report a bounded page of entries and the omitted count.
func TestDoctorCompactBoundsWaitingList(t *testing.T) {
	requestsProject(t, func(s *State) {
		for i := 0; i < doctorWaitingOnMax+5; i++ {
			s.Requests = append(s.Requests, &RequestRecord{
				ID: fmt.Sprintf("wait-%02d", i), Type: PublicRequestSettings,
				State: RequestWaiting, Session: "run-reqseed00001", SubmittedAt: ago(300 - i),
				WaitReason: "active_work", WaitSince: ago(100 + i),
			})
		}
	})
	var out, stderr bytes.Buffer
	if code := Main([]string{"doctor", "--contract", "v2", "--json"}, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if err := contractschema.ValidateJSON(doctorContractSchema(t), out.Bytes()); err != nil {
		t.Fatalf("schema: %v\n%s", err, out.String())
	}
	var doc doctorCompactDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Requests.WaitingOn) != doctorWaitingOnMax || doc.Requests.WaitingOmitted != 5 {
		t.Fatalf("bounded waiting list: %d shown, %d omitted", len(doc.Requests.WaitingOn), doc.Requests.WaitingOmitted)
	}
	if doc.Requests.WaitingOn[0].ID != "wait-00" {
		t.Fatalf("the oldest waiting request comes first: %+v", doc.Requests.WaitingOn[0])
	}
}

// TestDoctorExitCodePolicy covers both contracts' exit policy on the same
// unhealthy run: version 2 exits 0 because findings are observation
// content; the legacy contract keeps its frozen exit 1 on error-severity
// findings.
func TestDoctorExitCodePolicy(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "broken"))
	logs, runtime := projectPaths(c)
	_ = logs
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	// A state database that exists but cannot be read: the error-severity
	// state_unreadable finding.
	if err := os.WriteFile(filepath.Join(runtime, "state.db"), []byte("this is not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	var legacyOut, legacyErr, v2Out, v2Err bytes.Buffer
	legacyCode := Main([]string{"doctor", "--json"}, &legacyOut, &legacyErr)
	v2Code := Main([]string{"doctor", "--contract", "v2", "--json"}, &v2Out, &v2Err)
	if legacyCode != exitOperationFailed {
		t.Fatalf("legacy exit %d, want %d (frozen behavior)", legacyCode, exitOperationFailed)
	}
	if v2Code != exitOK {
		t.Fatalf("version 2 exit %d, want %d (findings are observation content)", v2Code, exitOK)
	}
	if err := contractschema.ValidateJSON(doctorContractSchema(t), v2Out.Bytes()); err != nil {
		t.Fatalf("schema: %v\n%s", err, v2Out.String())
	}
	var doc doctorCompactDocument
	if err := json.Unmarshal(v2Out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	sawError := false
	for _, f := range doc.Findings {
		if f.Severity == "error" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("the fixture must hold an error finding: %+v", doc.Findings)
	}
}

// TestDoctorPlainShowsRequestSummary covers the plain doctor's bounded
// request summary.
func TestDoctorPlainShowsRequestSummary(t *testing.T) {
	requestsProject(t, seedRequestReceipts)
	var out, stderr bytes.Buffer
	// The plain doctor needs no terminal; JSON mode forces plain rendering.
	code := Main([]string{"doctor", "--plain"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "Requests: 0 queued | 1 waiting") {
		t.Fatalf("the plain summary must appear:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "import-wait") || !strings.Contains(out.String(), "active_work") {
		t.Fatalf("the waiting entry must name its reason:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "looper requests --contract v2 --json") {
		t.Fatalf("the detail route must appear:\n%s", out.String())
	}
}

// TestDoctorCompactWithoutRun covers the version 2 doctor on a project with
// no saved run: the document still validates and reports an empty request
// summary beside its finding.
func TestDoctorCompactWithoutRun(t *testing.T) {
	base := runsBase(t)
	c := runsProjectAt(t, filepath.Join(filepath.Dir(base), "norun"))
	_ = c
	var out, stderr bytes.Buffer
	code := Main([]string{"doctor", "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if err := contractschema.ValidateJSON(doctorContractSchema(t), out.Bytes()); err != nil {
		t.Fatalf("schema: %v\n%s", err, out.String())
	}
	var doc doctorCompactDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Requests.Pending != 0 || len(doc.Requests.WaitingOn) != 0 {
		t.Fatalf("empty summary: %+v", doc.Requests)
	}
	found := false
	for _, f := range doc.Findings {
		if f.Code == "no_saved_run" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the no-run finding must stay: %+v", doc.Findings)
	}
}

// TestStatusNamesRequestDetailRoute covers the status integration: pending
// requests route to the bounded query (AX-004's detail field, served by
// AX-010), and the waiting count covers every non-terminal durable request.
func TestStatusNamesRequestDetailRoute(t *testing.T) {
	f := newStatusFixture(t)
	s := statusState(f, nil, 0)
	seedRequestReceipts(s)
	f.save(t, s)
	f.ownRepository(t, "run-route00001", time.Now().Add(-time.Minute))
	f.publishView(t, ui.ViewSnapshot{Session: "run-route00001"})
	_, doc, _ := runStatusCompact(t, "--contract", "v2", "--json")
	if doc.Requests.Waiting != 1 {
		t.Fatalf("waiting durable requests: %+v", doc.Requests)
	}
	if doc.Detail.Requests != "looper requests --contract v2 --json" {
		t.Fatalf("detail route: %q", doc.Detail.Requests)
	}
	// A run with nothing pending names no request route.
	f2 := newStatusFixture(t)
	f2.save(t, statusState(f2, nil, 0))
	f2.ownRepository(t, "run-quiet00001", time.Now().Add(-time.Minute))
	f2.publishView(t, ui.ViewSnapshot{Session: "run-quiet00001"})
	_, quiet, _ := runStatusCompact(t, "--contract", "v2", "--json")
	if quiet.Detail.Requests != "" {
		t.Fatalf("a quiet run names no request route: %q", quiet.Detail.Requests)
	}
}
