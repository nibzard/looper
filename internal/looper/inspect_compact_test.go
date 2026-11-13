package looper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nibzard/looper/internal/contractschema"
	"github.com/nibzard/looper/internal/ui"
)

// inspectCompactSchema reads the published compact inspection schema so the
// documents the CLI really produces are checked against it, not only the
// recorded examples.
func inspectCompactSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "inspect-compact.schema.json"))
	if err != nil {
		t.Skipf("compact inspection schema is absent: %v", err)
	}
	return b
}

// inspectTaskState builds one saved run whose target task carries the history
// a compact inspection must bound: many attempts, blockers, and a details
// text far over the excerpt bound.
func inspectTaskState(f *statusFixture, target Task, attempts []*Attempt) *State {
	tasks := []Task{target, {ID: "T9", Title: "Unrelated work", Priority: 3, Status: "todo"}}
	for _, a := range attempts {
		for i := range tasks {
			if tasks[i].ID == a.Task.ID {
				tasks[i].Status = a.Task.Status
			}
		}
	}
	return &State{
		Version: 2, Root: f.root, Todo: f.c.Todo, Head: "head", Branch: "main",
		Ref: "refs/looper/run-test0001", Tip: "tip", Published: "published", ExportHash: "export-hash",
		Backlog:  Backlog{Version: 1, Sources: []string{}, Tasks: tasks},
		Attempts: attempts,
	}
}

// inspectAttemptWithLogs builds one attempt with the write sessions a heavily
// repaired attempt leaves behind.
func inspectAttemptWithLogs(t *testing.T, id string, ordinal int, dir string, sessions int) *Attempt {
	t.Helper()
	for i := 0; i < sessions; i++ {
		work := filepath.Join(dir, fmt.Sprintf("work-%02d-session%d", i, ordinal))
		if err := os.MkdirAll(work, 0700); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(work, "events-00.jsonl")
		if err := writeFixtureFile(log, []byte("{\"type\":\"assistant\"}\n")); err != nil {
			t.Fatal(err)
		}
	}
	return &Attempt{
		ID: id, Task: Task{ID: "T1", Title: "Bound task inspection", Priority: 2, Status: "doing"},
		Agent: "claude", Base: fmt.Sprintf("base-%02d", ordinal), Directory: dir,
		Ref: "ref", Phase: "blocked", Repairs: sessions, Worktrees: []string{},
	}
}

// inspectFixture builds one project whose target task T1 carries a heavy
// history: the described number of attempts, check records spread across
// them, and write sessions inside the newest attempt.
func newInspectFixture(t *testing.T, attemptCount, checksPerAttempt, sessions int) *statusFixture {
	t.Helper()
	f := newStatusFixture(t)
	target := Task{
		ID: "T1", Title: "Bound task inspection", Priority: 2, Status: "doing",
		Details: strings.Repeat("detail ", 4000), Blockers: []string{"needs the operator to grant access"},
		DependsOn: []string{"T0"}, Files: []string{"internal/looper/observe.go"}, UnblockWhen: "The operator grants access.",
	}
	attempts := []*Attempt{}
	for i := 0; i < attemptCount; i++ {
		// The attempt identifier and its directory must match, as the engine's
		// own start path guarantees, so timing classifies its artifacts.
		dir := filepath.Join(f.dir, fmt.Sprintf("task-%04d", i))
		attempts = append(attempts, inspectAttemptWithLogs(t, fmt.Sprintf("task-%04d", i), i, dir, 1))
	}
	if len(attempts) > 0 {
		current := attempts[len(attempts)-1]
		current.Phase = "running"
		current.Repairs = sessions
		current.Feedback = strings.Repeat("verify failed: ", 6000)
		current.FailureHash = "failure-hash"
		current.Repeats = 1
		current.Diagnosed = true
		current.ResultCommit = "result-commit"
		current.Summary = &Summary{TaskID: "T1", Status: "done", Summary: "implemented", Files: []string{}, Blockers: []string{}}
		current.Verification = &Candidate{Base: "base", Commit: "commit", Tree: "tree", AttemptID: current.ID, OK: false, Feedback: "verify failed"}
		// The newest attempt carries the write sessions many repairs leave.
		for i := 1; i < sessions; i++ {
			work := filepath.Join(current.Directory, fmt.Sprintf("work-%02d-sessionN", i))
			if err := os.MkdirAll(work, 0700); err != nil {
				t.Fatal(err)
			}
			if err := writeFixtureFile(filepath.Join(work, "events-00.jsonl"), []byte("{\"type\":\"assistant\"}\n")); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.save(t, inspectTaskState(f, target, attempts))
	store := f.openStore(t)
	rows := [][5]string{}
	for i, a := range attempts {
		for c := 0; c < checksPerAttempt; c++ {
			rows = append(rows, [5]string{a.ID, fmt.Sprintf("rev-%02d-%02d", i, c), "verify", "fail",
				filepath.Join(a.Directory, "checks", fmt.Sprintf("rev-%02d-%02d", i, c), "verify.log")})
		}
	}
	seedChecks(t, store, rows)
	if len(attempts) > 0 {
		// One recorded worker interval for the newest attempt, so the timing
		// provenance has coverage to report.
		current := attempts[len(attempts)-1]
		artifact := filepath.Join(current.Directory, "work-00-session0", "events-00.jsonl")
		started := time.Now().UTC().Add(-3 * time.Minute)
		if _, err := store.db.Exec(`INSERT INTO processes(id,session_id,attempt_id,kind,artifact,pid,identity,started,ended,outcome,source)
 VALUES('inspect-proc-1','',?,'events-00',?,-1,'',?,?,'exited','imported')`,
			current.ID, artifact, started.Format(time.RFC3339Nano), started.Add(time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// runInspectCompact runs the public CLI and returns the exit code with the
// decoded compact document. It fails unless standard output carries exactly
// one JSON document that validates against the published schema and stays
// within the byte budget.
func runInspectCompact(t *testing.T, args ...string) (int, *inspectCompactDocument, []byte) {
	t.Helper()
	var out, stderr strings.Builder
	code := Main(append([]string{"inspect", "T1", "--contract", "v2", "--json"}, args...), &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: stderr=%s stdout=%s", code, stderr.String(), out.String())
	}
	data := []byte(out.String())
	if len(data) > inspectBytesBudget {
		t.Fatalf("compact inspection is %d bytes, over the %d byte budget:\n%s", len(data), inspectBytesBudget, data)
	}
	var doc inspectCompactDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("stdout is not one compact inspection document: %v\n%s", err, data)
	}
	remarshaled, err := json.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(inspectCompactSchema(t), remarshaled); err != nil {
		t.Fatalf("the produced document violates the compact inspection schema: %v\n%s", err, data)
	}
	return code, &doc, data
}

// TestInspectCompactDocumentMatchesSchema serves the compact document from a
// live controller with an in-flight, heavily repaired attempt and checks the
// facts a focused inspection needs.
func TestInspectCompactDocumentMatchesSchema(t *testing.T) {
	f := newInspectFixture(t, 3, 2, 3)
	f.ownRepository(t, "run-inspect01", time.Now().Add(-2*time.Minute))
	f.publishView(t, ui.ViewSnapshot{Session: "run-inspect01"})

	_, doc, data := runInspectCompact(t)

	if doc.Kind != inspectCompactKind || doc.ContractVersion != ContractVersionV2 {
		t.Fatalf("document identity: %+v", doc)
	}
	if doc.Controller.State != "controlling" || !doc.Controller.Owned {
		t.Fatalf("controller: %+v", doc.Controller)
	}
	if doc.Task.ID != "T1" || doc.Task.Status != "doing" || doc.Task.ContractDigest == "" {
		t.Fatalf("task contract: %+v", doc.Task)
	}
	if len(doc.Task.Blockers) != 1 || doc.Task.Blockers[0] == "" {
		t.Fatalf("blockers: %+v", doc.Task)
	}
	if !doc.Task.Details.Truncated || doc.Task.Details.Bytes != len(strings.Repeat("detail ", 4000)) {
		t.Fatalf("details excerpt: %+v", doc.Task.Details)
	}
	if doc.Attempts.Total != 3 || doc.Attempts.Current == nil {
		t.Fatalf("attempts: %+v", doc.Attempts)
	}
	current := doc.Attempts.Current
	if current.Phase != "running" || !current.Active || current.Retained {
		t.Fatalf("current attempt: %+v", current)
	}
	if current.Repairs != 3 || !current.Diagnosed || current.ResultCommit != "result-commit" {
		t.Fatalf("current provenance: %+v", current)
	}
	if current.Feedback == nil || !current.Feedback.Truncated || current.Feedback.Bytes == 0 {
		t.Fatalf("feedback excerpt: %+v", current.Feedback)
	}
	if current.Output == nil || current.Output.Progress == "" {
		t.Fatalf("output evidence: %+v", current.Output)
	}
	if doc.Checks.Total != 6 || len(doc.Checks.Items) == 0 {
		t.Fatalf("checks: %+v", doc.Checks)
	}
	if doc.Timing.TaskID != "T1" || !doc.Timing.Available || doc.Timing.RecordedAttempts != 1 || doc.Timing.Attempts != 3 {
		t.Fatalf("timing provenance: %+v", doc.Timing)
	}
	if doc.Timing.WorkerSeconds == nil || *doc.Timing.WorkerSeconds <= 0 {
		t.Fatalf("the recorded worker interval must surface: %+v", doc.Timing)
	}
	if doc.Limits.BytesBudget != inspectBytesBudget || doc.Limits.Omitted == nil {
		t.Fatalf("limits: %+v", doc.Limits)
	}
	for _, route := range []string{doc.Detail.Full, doc.Detail.History, doc.Detail.Events} {
		if route == "" || !strings.Contains(route, "T1") {
			t.Fatalf("detail routes must name the task: %+v", doc.Detail)
		}
	}
	if !strings.Contains(doc.Detail.Events, "--task T1") {
		t.Fatalf("events route: %+v", doc.Detail)
	}
	// Long strings never pass through unbounded.
	for _, item := range doc.Attempts.Items {
		if item.Feedback != nil && len(item.Feedback.Text) > doc.Limits.ExcerptMax+8 {
			t.Fatalf("feedback excerpt exceeds the stated bound: %d", len(item.Feedback.Text))
		}
	}
	if strings.Contains(string(data), strings.Repeat("verify failed: ", 100)) {
		t.Fatal("the full feedback text leaked into the compact document")
	}
}

// TestInspectCompactStaysBoundedWithManyRepairsAndLargeFeedback is the AX-005
// acceptance case: a task with many repairs, many attempts, and large
// feedback returns bounded valid JSON that states what it omitted.
func TestInspectCompactStaysBoundedWithManyRepairsAndLargeFeedback(t *testing.T) {
	const attempts, checksPerAttempt, sessions = 25, 5, 15
	newInspectFixture(t, attempts, checksPerAttempt, sessions)

	_, doc, data := runInspectCompact(t)

	if !json.Valid(data) {
		t.Fatal("the bounded document must stay valid JSON")
	}
	if doc.Attempts.Total != attempts {
		t.Fatalf("the total must stay exact: %+v", doc.Attempts)
	}
	if doc.Checks.Total != attempts*checksPerAttempt {
		t.Fatalf("check total: got %d want %d", doc.Checks.Total, attempts*checksPerAttempt)
	}
	if returned := doc.Attempts.Page.Returned; returned == 0 || returned >= attempts {
		t.Fatalf("a bounded window must return some but not all attempts: %d", returned)
	}
	if doc.Limits.Omitted["attempts"] == 0 || doc.Limits.Omitted["checks"] == 0 {
		t.Fatalf("omissions must be counted: %+v", doc.Limits)
	}
	if doc.Attempts.Current.LogsTotal != sessions {
		t.Fatalf("the current attempt must count every retained log: %d want %d", doc.Attempts.Current.LogsTotal, sessions)
	}
	if len(doc.Attempts.Current.Logs) > doc.Limits.LogsMax {
		t.Fatalf("rendered logs exceed the stated bound: %d > %d", len(doc.Attempts.Current.Logs), doc.Limits.LogsMax)
	}
	if doc.Attempts.Current.Feedback == nil || !doc.Attempts.Current.Feedback.Truncated {
		t.Fatal("large feedback must arrive as a stated excerpt")
	}
}

// newEscapeFixture builds one project whose target task populates every field
// the compact document renders with text the JSON encoder expands sixfold:
// ampersands, less-than, and greater-than each escape to six bytes, so a raw
// byte bound understates the emitted size as much as six times. Every list
// carries eight entries and every free-text field is far over the tightest
// excerpt bound, which is the shape that overflowed the published budget.
func newEscapeFixture(t *testing.T, attemptCount, checksPerAttempt int) (*statusFixture, string) {
	t.Helper()
	f := newStatusFixture(t)
	esc := func(n int) string { return strings.Repeat("amp&<>", n) }
	// The identifier stays inside the route bound in emitted bytes, so the
	// detail and continuation routes render: that is the largest route cost.
	id := strings.Repeat("&", inspectRouteID)
	target := Task{
		ID: id, Title: esc(60), Priority: 2, Status: "doing",
		Details: esc(600), UnblockWhen: esc(60), CreatedAt: esc(20), UpdatedAt: esc(20),
	}
	for i := 0; i < maxInspectList+3; i++ {
		target.Tags = append(target.Tags, esc(30))
		target.Files = append(target.Files, esc(30))
		target.DependsOn = append(target.DependsOn, esc(30))
		target.Blockers = append(target.Blockers, esc(30))
		target.Steps = append(target.Steps, esc(30))
	}
	attempts := []*Attempt{}
	for i := 0; i < attemptCount; i++ {
		// The directory keeps one ASCII segment so the relative log paths
		// stay readable in failures; every other string is escape-heavy.
		dir := filepath.Join(f.dir, fmt.Sprintf("task-%04d", i)+esc(10))
		work := filepath.Join(dir, "work-00-session0")
		if err := os.MkdirAll(work, 0700); err != nil {
			t.Fatal(err)
		}
		if err := writeFixtureFile(filepath.Join(work, "events-00.jsonl"), []byte("{\"type\":\"assistant\"}\n")); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, &Attempt{
			ID: esc(20) + fmt.Sprintf("-%04d", i), Task: Task{ID: id, Title: esc(20), Priority: 2, Status: "doing"},
			Agent: esc(20), Base: esc(20), Directory: dir, Ref: "ref", Phase: "running",
			Repairs: 3, Repeats: 1, Diagnosed: true, ResultCommit: esc(20), FailureHash: esc(20),
			Feedback:     esc(600),
			Summary:      &Summary{TaskID: id, Status: "done", Summary: esc(60), Files: []string{}, Blockers: []string{}},
			Verification: &Candidate{Base: esc(20), Commit: esc(20), Tree: esc(20), AttemptID: esc(20), OK: false, Feedback: esc(200)},
		})
	}
	f.save(t, &State{
		Version: 2, Root: esc(30), Todo: esc(30), Head: esc(20), Branch: "main",
		Ref: "refs/looper/" + esc(10), Tip: esc(20), Published: esc(20), ExportHash: "export-hash",
		Backlog:  Backlog{Version: 1, Sources: []string{}, Tasks: []Task{target, {ID: "T9", Title: "Unrelated work", Priority: 3, Status: "todo"}}},
		Attempts: attempts,
	})
	store := f.openStore(t)
	rows := [][5]string{}
	for i, a := range attempts {
		for c := 0; c < checksPerAttempt; c++ {
			artifact := filepath.Join(a.Directory, "checks", fmt.Sprintf("rev-%04d-%02d", i, c), esc(10)+".log")
			rows = append(rows, [5]string{a.ID, esc(20), esc(10), esc(10), artifact})
		}
	}
	// The largest case seeds two thousand check rows; one durable commit
	// keeps that fixture from dominating the suite's fsync budget.
	seedChecks(t, store, rows)
	return f, id
}

// inspectEvidenceOfKind returns the evidence reference of one kind.
func inspectEvidenceOfKind(doc *inspectCompactDocument, kind, prefix string) string {
	for _, e := range doc.Evidence {
		if e.Kind == kind && strings.HasPrefix(e.Reference, prefix) {
			return e.Reference
		}
	}
	return ""
}

// TestInspectCompactBoundsEscapeExpandedContent pins the published budget
// against content the JSON encoder expands: the same task shape the ordinary
// fixtures use, with every string escape-heavy, must still return one valid
// document at or under the budget its own limits report.
func TestInspectCompactBoundsEscapeExpandedContent(t *testing.T) {
	_, id := newEscapeFixture(t, 4, 8)
	var out, stderr strings.Builder
	code := Main([]string{"inspect", id, "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	data := []byte(out.String())
	if len(data) > inspectBytesBudget {
		t.Fatalf("document is %d bytes, over the %d byte budget:\n%s", len(data), inspectBytesBudget, data)
	}
	var doc inspectCompactDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("stdout is not one compact inspection document: %v\n%s", err, data)
	}
	remarshaled, err := json.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(inspectCompactSchema(t), remarshaled); err != nil {
		t.Fatalf("the produced document violates the schema: %v\n%s", err, data)
	}
	// The document stays useful, not merely small: the truncation it applied
	// is stated, and every excerpt keeps the complete recorded byte count.
	if doc.Task.Title == "" || !doc.Task.Details.Truncated || doc.Task.Details.Bytes == 0 {
		t.Fatalf("task contract: %+v", doc.Task)
	}
	if current := doc.Attempts.Current; current == nil || current.Feedback == nil || !current.Feedback.Truncated {
		t.Fatalf("current attempt: %+v", doc.Attempts.Current)
	}
	if len(doc.Attempts.Items) == 0 || len(doc.Checks.Items) == 0 {
		t.Fatalf("the bounded pages must keep content: %+v %+v", doc.Attempts, doc.Checks)
	}
	// No string the document emits may exceed the bound its limits state,
	// measured in the bytes the encoder writes for it.
	for _, item := range append(doc.Attempts.Items, *doc.Attempts.Current) {
		if item.Feedback != nil && jsonEscapedLen(item.Feedback.Text) > doc.Limits.ExcerptMax {
			t.Fatalf("feedback excerpt exceeds the stated bound: %d", jsonEscapedLen(item.Feedback.Text))
		}
	}
	// The budget the document reports is a true statement about itself.
	if doc.Limits.BytesBudget < len(data) {
		t.Fatalf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(data))
	}
	// The plain rendering reads the same structure and stays bounded too.
	var plain, plainErr strings.Builder
	if code := Main([]string{"inspect", id, "--contract", "v2"}, &plain, &plainErr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, plain.String(), plainErr.String())
	}
	if text := plain.String(); !strings.Contains(text, "Task:") || len(text) > 4*inspectBytesBudget {
		t.Fatalf("plain rendering of the bounded document is not readable or bounded: %d bytes", len(text))
	}
}

// TestInspectCompactServesCancelledTask inspects a task cancelled through a
// checked request (AX-019): the task contract carries the cancelled status,
// the retained attempt keeps its recorded result in the history, and the
// produced document validates against the published schema, whose task-status
// enum accepts the status.
func TestInspectCompactServesCancelledTask(t *testing.T) {
	f := newStatusFixture(t)
	dir := filepath.Join(f.dir, "task-0000")
	retained := inspectAttemptWithLogs(t, "task-0000", 0, dir, 1)
	retained.Task.Status = "cancelled"
	retained.Summary = &Summary{TaskID: "T1", Status: "blocked", Summary: "blocked before the cancellation applied", Files: []string{}, Blockers: []string{}}
	target := Task{ID: "T1", Title: "Cancelled work", Priority: 2, Status: "cancelled"}
	f.save(t, inspectTaskState(f, target, []*Attempt{retained}))
	_, doc, _ := runInspectCompact(t)
	if doc.Task.Status != "cancelled" {
		t.Fatalf("task status: %+v", doc.Task)
	}
	if len(doc.Attempts.Items) != 1 || doc.Attempts.Items[0].Phase != "blocked" || doc.Attempts.Items[0].Summary == nil {
		t.Fatalf("the retained attempt and its result must stay inspectable: %+v", doc.Attempts)
	}
	var plain, plainErr strings.Builder
	if code := Main([]string{"inspect", "T1", "--contract", "v2"}, &plain, &plainErr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, plain.String(), plainErr.String())
	}
	if !strings.Contains(plain.String(), "cancelled") {
		t.Fatalf("the plain rendering must show the cancelled status:\n%s", plain.String())
	}
}

// TestInspectTerminalFitBoundsAnyContent pins the structural guarantee behind
// the budget: the ladder's terminal fit bounds every string to one rune at
// worst, so the document it renders is structure alone. The escape-heavy
// fixture — every field populated, every list full, every free-text field far
// over the bound — must render under the budget at that fit, and so must any
// content, because no recorded byte can reach past it.
func TestInspectTerminalFitBoundsAnyContent(t *testing.T) {
	for _, tc := range []struct {
		attempts, checks int
	}{
		{2, 8}, {25, 5}, {100, 20},
	} {
		f, id := newEscapeFixture(t, tc.attempts, tc.checks)
		store := f.openStore(t)
		snap, err := store.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		var task *Task
		for i := range snap.State.Backlog.Tasks {
			if snap.State.Backlog.Tasks[i].ID == id {
				task = &snap.State.Backlog.Tasks[i]
			}
		}
		if task == nil {
			t.Fatal("target task is absent from the saved state")
		}
		c := f.c
		c.InspectTask = id
		for _, limit := range []int{1, defaultInspectPageLimit, maxInspectPageLimit} {
			c.After, c.AfterSet = 0, false
			c.CheckAfter, c.CheckAfterSet = 0, false
			c.EventLimit = limit
			ev, err := collectInspectEvidence(c, store, snap, task, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			doc := renderInspectDocument(ev, inspectTerminalFit)
			if size := inspectEncoded(doc); size > inspectBytesBudget {
				t.Fatalf("%d attempts, %d checks, limit %d: the terminal fit renders %d bytes, over the %d byte budget",
					tc.attempts, tc.checks, limit, size, inspectBytesBudget)
			}
			// The ladder as a whole never returns a larger document than its
			// terminal fit can justify: whatever the earlier fits measured,
			// the chosen document obeys the budget it reports.
			if chosen := fitInspectDocument(ev); inspectEncoded(chosen) > inspectBytesBudget {
				t.Fatalf("the chosen document exceeds the budget it reports: %d bytes", inspectEncoded(chosen))
			}
		}
	}
}

// TestInspectEvidenceNamesNewestVerificationArtifact pins the evidence entry
// for the newest verification artifact: a forward check page can stop far
// below the newest row, and the reference must still name the task's newest
// artifact, not the newest item of the page that was fetched.
func TestInspectEvidenceNamesNewestVerificationArtifact(t *testing.T) {
	const attempts, checksPerAttempt = 3, 3
	newInspectFixture(t, attempts, checksPerAttempt, 1)

	_, oldest, _ := runInspectCompact(t, "--checks-after", "0", "--limit", "1")
	if len(oldest.Checks.Items) != 1 || !oldest.Checks.Page.HasMore {
		t.Fatalf("the forward page must hold exactly its oldest row: %+v", oldest.Checks)
	}
	// The page's single row is the oldest artifact of the whole history.
	oldestArtifact := oldest.Checks.Items[0].Artifact
	if !strings.Contains(oldestArtifact, "rev-00-00") {
		t.Fatalf("the fixture's oldest row is %q", oldestArtifact)
	}
	reference := inspectEvidenceOfKind(oldest, "log", "checks/")
	if reference == "" {
		t.Fatalf("the evidence must reference the newest verification artifact: %+v", oldest.Evidence)
	}
	if strings.Contains(reference, "rev-00-00") {
		t.Fatalf("the evidence named the fetched page's row %q, not the task's newest artifact", reference)
	}
	// The newest record belongs to the newest attempt's last check.
	newest := fmt.Sprintf("rev-%02d-%02d", attempts-1, checksPerAttempt-1)
	if !strings.Contains(reference, newest) {
		t.Fatalf("the evidence must name the task's newest artifact %q: %q", newest, reference)
	}

	// The bare query reads the newest tail, so its evidence agrees.
	_, tail, _ := runInspectCompact(t)
	if other := inspectEvidenceOfKind(tail, "log", "checks/"); other != reference {
		t.Fatalf("the newest artifact must not depend on the page read: %q vs %q", other, reference)
	}
}

// TestClampJSONStringMatchesEncoder keeps the escaped-length model honest
// against the encoder itself: every byte value, multibyte runes, the
// JavaScript line separators, and invalid UTF-8 must measure exactly what
// encoding/json emits, and the clamp must keep its promise on all of them.
func TestClampJSONStringMatchesEncoder(t *testing.T) {
	corpus := []string{"", "plain text", "amp&<>", strings.Repeat("&", 500)}
	for b := 0; b < 256; b++ {
		corpus = append(corpus, string([]byte{byte(b)}), "x"+string([]byte{byte(b)})+"y")
	}
	corpus = append(corpus,
		"héllo … 😀",                            // multibyte that never escapes
		"line\u2028sep and paragraph\u2029sep", // JavaScript separators
		string([]byte{0xff, 0xfe, 0x41}),       // invalid UTF-8 bytes
		"a\xcf\x28b",                           // invalid continuation
		strings.Repeat("mixed & < > \" \\ \x01 text", 60),
	)
	for _, s := range corpus {
		encoded, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := jsonEscapedLen(s), len(encoded)-2; got != want {
			t.Fatalf("escaped length of %q: got %d want %d", s, got, want)
		}
		for _, want := range []int{1, 4, 8, 12, 28, 64, 512} {
			clamped := clampJSONString(s, want)
			if s != "" && clamped == "" {
				t.Fatalf("clampJSONString(%q, %d) emptied the string", s, want)
			}
			encoded, err := json.Marshal(clamped)
			if err != nil {
				t.Fatal(err)
			}
			// Below its floor the clamp keeps one rune rather than emptying
			// the string, which costs at most one six-byte escape.
			bound := max(want, clampJSONStringMin)
			if got := len(encoded) - 2; got > bound {
				t.Fatalf("clampJSONString(%q, %d) emitted %d bytes", s, want, got)
			}
			if utf8.ValidString(s) && !utf8.ValidString(clamped) {
				t.Fatalf("clampJSONString(%q, %d) broke UTF-8: %q", s, want, clamped)
			}
		}
	}
	// The bound holds for raw bytes too, because escaping never shrinks.
	if got := len(clampJSONString(strings.Repeat("&", 100), 28)); got > 28 {
		t.Fatalf("the raw length must stay inside the bound: %d", got)
	}
	// Prefix and suffix walks agree with the encoder on where they cut.
	for _, s := range corpus {
		for _, budget := range []int{3, 9, 27} {
			head := s[:jsonPrefixLen(s, budget)]
			if got := jsonEscapedLen(head); got > budget {
				t.Fatalf("prefix of %q within %d emitted %d bytes", s, budget, got)
			}
			tail := s[jsonSuffixStart(s, budget):]
			if got := jsonEscapedLen(tail); got > budget {
				t.Fatalf("suffix of %q within %d emitted %d bytes", s, budget, got)
			}
		}
	}
}

// TestExcerptOfBoundsEscapedLength pins the excerpt bound: the head-plus-tail
// excerpt stays inside its budget in emitted bytes however heavily the text
// escapes, cuts on rune boundaries, and reports the complete recorded byte
// count.
func TestExcerptOfBoundsEscapedLength(t *testing.T) {
	texts := []string{
		strings.Repeat("&", 4000),
		strings.Repeat("plain detail text ", 400),
		strings.Repeat("amp&<>", 800),
		"héllo 😀 " + strings.Repeat("&", 200),
		string([]byte{0xff}) + strings.Repeat("<", 200),
	}
	for _, text := range texts {
		for _, bound := range []int{8, 16, 32, 96, 512} {
			e := excerptOf(text, bound)
			if got := jsonEscapedLen(e.Text); got > bound {
				t.Fatalf("excerpt of %q within %d emitted %d bytes: %q", text[:min(20, len(text))], bound, got, e.Text)
			}
			if utf8.ValidString(text) && !utf8.ValidString(e.Text) {
				t.Fatalf("excerpt broke UTF-8: %q", e.Text)
			}
			if e.Bytes != len(text) {
				t.Fatalf("the excerpt must report the complete byte count: %d of %d", e.Bytes, len(text))
			}
			if !e.Truncated {
				t.Fatal("text over the bound must state its truncation")
			}
			// Text whose emitted size fits passes through unchanged.
			if jsonEscapedLen(text) <= bound && e.Text != text {
				t.Fatalf("text inside the bound must pass through: %q", e.Text)
			}
		}
	}
	if e := excerptOf("short", 32); e.Truncated || e.Text != "short" || e.Bytes != 5 {
		t.Fatalf("text inside the bound passes through whole: %+v", e)
	}
}

// TestInspectCompactBoundsAnAdversarialIdentifier checks the budget against
// an identifier long enough that a route command embedding it would itself
// be unbounded: the routes are dropped, never truncated into wrong commands,
// and the document still validates.
func TestInspectCompactBoundsAnAdversarialIdentifier(t *testing.T) {
	f := newInspectFixture(t, 3, 3, 3)
	long := strings.Repeat("T", 400)
	s := f.inspectSavedState(t)
	s.Backlog.Tasks[0].ID = long
	for _, a := range s.Attempts {
		a.Task.ID = long
	}
	f.save(t, s)
	var out, stderr strings.Builder
	code := Main([]string{"inspect", long, "--contract", "v2", "--json"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if len(out.String()) > inspectBytesBudget {
		t.Fatalf("document is %d bytes, over the %d byte budget", len(out.String()), inspectBytesBudget)
	}
	var doc inspectCompactDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one compact inspection document: %v", err)
	}
	remarshaled, err := json.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(inspectCompactSchema(t), remarshaled); err != nil {
		t.Fatalf("the produced document violates the schema: %v", err)
	}
	if doc.Detail.Full != "" || doc.Detail.History != "" || doc.Detail.Events != "" {
		t.Fatalf("an over-long identifier must drop its routes, not truncate them: %+v", doc.Detail)
	}
	if doc.Task.ID != clampJSONString(long, doc.Limits.StringMax) {
		t.Fatalf("the identifier itself is clamped like any string: %d chars", len(doc.Task.ID))
	}
	if !strings.Contains(doc.Attempts.Continuation.Command, "<task-id>") {
		t.Fatalf("the continuation names a placeholder instead of an unbounded identifier: %s", doc.Attempts.Continuation.Command)
	}
}

// TestInspectCompactPagesHistoryWithoutGapsOrDuplicates walks the attempt and
// check history with the continuation commands the documents return and
// proves every record is reachable exactly once.
func TestInspectCompactPagesHistoryWithoutGapsOrDuplicates(t *testing.T) {
	// The heavy history forces the byte budget to drop records from fetched
	// windows, so the walk also proves the fit ladder keeps paging gapless.
	const attempts, checksPerAttempt = 25, 5
	newInspectFixture(t, attempts, checksPerAttempt, 15)

	// The bare query is the newest tail; its continuation routes into the
	// forward read from the beginning.
	_, tail, _ := runInspectCompact(t, "--limit", "10")
	tailOrdinals := map[int]bool{}
	for _, item := range tail.Attempts.Items {
		tailOrdinals[item.Ordinal] = true
	}
	if !tail.Attempts.Page.HasMore || tail.Attempts.Page.Order != "ascending" {
		t.Fatalf("tail page: %+v", tail.Attempts.Page)
	}
	if len(tail.Attempts.Items) < 1 || tail.Attempts.Items[0].Ordinal <= attempts-10 {
		t.Fatalf("the tail must hold the newest attempts: %+v", tail.Attempts.Items)
	}
	droppedPages := 0
	if !strings.Contains(tail.Attempts.Continuation.Command, "--after 0") {
		t.Fatalf("the tail's continuation must name the forward read: %s", tail.Attempts.Continuation.Command)
	}
	// Walk forward from the beginning until the pages run out. The walk is
	// its own gap-free history: every ordinal exactly once.
	seen := map[int]bool{}
	after := int64(0)
	pages := 0
	for {
		_, doc, _ := runInspectCompact(t, "--after", fmt.Sprint(after), "--limit", "10")
		if doc.Attempts.Page.After != after {
			t.Fatalf("page after position: %+v", doc.Attempts.Page)
		}
		if doc.Attempts.Page.Returned < 10 {
			droppedPages++
		}
		for _, item := range doc.Attempts.Items {
			if item.Ordinal <= int(after) {
				t.Fatalf("an item repeated or preceded the cursor: %+v after %d", item, after)
			}
			if seen[item.Ordinal] {
				t.Fatalf("attempt ordinal %d returned twice", item.Ordinal)
			}
			seen[item.Ordinal] = true
		}
		pages++
		if !doc.Attempts.Page.HasMore {
			break
		}
		if len(doc.Attempts.Items) == 0 {
			t.Fatal("has_more without items loops forever")
		}
		after = int64(doc.Attempts.Items[len(doc.Attempts.Items)-1].Ordinal)
		if pages > attempts {
			t.Fatal("paging did not terminate")
		}
	}
	if droppedPages == 0 {
		t.Fatal("the fixture must force the budget to drop records from a fetched window")
	}
	for ordinal := 1; ordinal <= attempts; ordinal++ {
		if !seen[ordinal] {
			t.Fatalf("attempt ordinal %d is unreachable", ordinal)
		}
		if tailOrdinals[ordinal] && !seen[ordinal] {
			t.Fatalf("the tail reported an ordinal the walk never reaches: %d", ordinal)
		}
	}
	// The same walk over the check identifiers: the tail routes forward, and
	// the walk collects every check row exactly once. The tail's newest
	// records sit at the end of the forward history, so the walk covers them
	// too; the tail is only an overlapping view, never extra records.
	tailChecks := map[int64]bool{}
	_, checkTail, _ := runInspectCompact(t, "--limit", "10")
	for _, item := range checkTail.Checks.Items {
		tailChecks[item.ID] = true
	}
	if !checkTail.Checks.Page.HasMore || !strings.Contains(checkTail.Checks.Continuation.Command, "--checks-after 0") {
		t.Fatalf("check tail page: %+v", checkTail.Checks)
	}
	checks := map[int64]bool{}
	after = 0
	for {
		_, doc, _ := runInspectCompact(t, "--checks-after", fmt.Sprint(after), "--limit", "10")
		for _, item := range doc.Checks.Items {
			if item.ID <= after {
				t.Fatalf("a check repeated or preceded the cursor: %+v after %d", item, after)
			}
			if checks[item.ID] {
				t.Fatalf("check %d returned twice", item.ID)
			}
			checks[item.ID] = true
		}
		if !doc.Checks.Page.HasMore {
			break
		}
		if len(doc.Checks.Items) == 0 {
			t.Fatal("has_more without items loops forever")
		}
		after = doc.Checks.Items[len(doc.Checks.Items)-1].ID
	}
	if len(checks) != attempts*checksPerAttempt {
		t.Fatalf("check walk collected %d of %d records", len(checks), attempts*checksPerAttempt)
	}
	for id := range tailChecks {
		if !checks[id] {
			t.Fatalf("the tail reported check %d, which the walk never reaches", id)
		}
	}
}

// TestInspectCompactDistinguishesActiveRetainedAndHistoricalWork pins the
// ownership distinction the acceptance requires: an active attempt a
// controller owns, the same attempt retained without a controller, and a
// historical attempt are three different answers.
func TestInspectCompactDistinguishesActiveRetainedAndHistoricalWork(t *testing.T) {
	t.Run("active under a live controller", func(t *testing.T) {
		f := newInspectFixture(t, 2, 1, 1)
		f.ownRepository(t, "run-live00001", time.Now().Add(-time.Minute))
		f.publishView(t, ui.ViewSnapshot{Session: "run-live00001"})
		_, doc, _ := runInspectCompact(t)
		if doc.Controller.State != "controlling" || !doc.Controller.Owned {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Attempts.Current == nil || !doc.Attempts.Current.Active || doc.Attempts.Current.Retained {
			t.Fatalf("current attempt: %+v", doc.Attempts.Current)
		}
	})
	t.Run("active without a controller is retained", func(t *testing.T) {
		newInspectFixture(t, 2, 1, 1)
		_, doc, _ := runInspectCompact(t)
		if doc.Controller.Owned {
			t.Fatalf("controller: %+v", doc.Controller)
		}
		if doc.Attempts.Current == nil || !doc.Attempts.Current.Active || !doc.Attempts.Current.Retained {
			t.Fatalf("an active attempt without an owner is retained work: %+v", doc.Attempts.Current)
		}
		for _, item := range doc.Attempts.Items {
			if item.Phase != "running" && item.Retained {
				t.Fatalf("only active work can be retained: %+v", item)
			}
		}
	})
	t.Run("finished attempt is historical", func(t *testing.T) {
		f := newInspectFixture(t, 2, 1, 1)
		f.ownRepository(t, "run-done00001", time.Now().Add(-time.Minute))
		f.publishView(t, ui.ViewSnapshot{Session: "run-done00001"})
		// Finish the whole task history by hand, as a blocked exit would.
		s := f.inspectSavedState(t)
		for _, a := range s.Attempts {
			a.Phase = "done"
		}
		s.Backlog.Tasks[0].Status = "done"
		f.save(t, s)
		_, doc, _ := runInspectCompact(t)
		if doc.Attempts.Current == nil || doc.Attempts.Current.Active || doc.Attempts.Current.Retained {
			t.Fatalf("a finished attempt is historical: %+v", doc.Attempts.Current)
		}
	})
	t.Run("task without attempts", func(t *testing.T) {
		newInspectFixture(t, 0, 0, 0)
		_, doc, _ := runInspectCompact(t)
		if doc.Attempts.Total != 0 || doc.Attempts.Current != nil || len(doc.Attempts.Items) != 0 {
			t.Fatalf("no attempts: %+v", doc.Attempts)
		}
		if doc.Checks.Total != 0 || len(doc.Checks.Items) != 0 {
			t.Fatalf("no checks: %+v", doc.Checks)
		}
	})
}

// inspectSavedState reads the fixture's saved state for mutation.
func (f *statusFixture) inspectSavedState(t *testing.T) *State {
	t.Helper()
	store, err := OpenStoreReadonly(f.storePath())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.State == nil {
		t.Fatal("no saved state")
	}
	return snap.State
}

// TestInspectFullRemainsExplicitlyAvailable pins the full path: --full under
// version 2 serves the complete unpaged document, the same document shape
// the legacy default serves.
func TestInspectFullRemainsExplicitlyAvailable(t *testing.T) {
	newInspectFixture(t, 12, 2, 2)
	var full, legacy strings.Builder
	if code := Main([]string{"inspect", "T1", "--contract", "v2", "--full", "--json"}, &full, &legacy); code != exitOK {
		t.Fatalf("exit %d: %s", code, legacy.String())
	}
	if code := Main([]string{"inspect", "T1", "--json"}, &legacy, &full); code != exitOK {
		t.Fatalf("exit %d: %s", code, full.String())
	}
	if strings.Contains(full.String(), "contract_version") {
		t.Fatalf("the complete document is the legacy document, not a second contract:\n%s", full.String())
	}
	fullShape, err := contractschema.ShapeJSON([]byte(full.String()))
	if err != nil {
		t.Fatal(err)
	}
	legacyShape, err := contractschema.ShapeJSON([]byte(legacy.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !contractschema.ShapeEqual(fullShape, legacyShape) {
		t.Fatal("--full must serve the same document shape as the legacy default")
	}
	var report struct {
		Version  int `json:"schema_version"`
		Attempts []struct {
			ID string `json:"id"`
		} `json:"attempts"`
		Logs []string `json:"logs"`
	}
	if err = json.Unmarshal([]byte(full.String()), &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != observationVersion || len(report.Attempts) != 12 {
		t.Fatalf("the complete document carries every attempt: %+v", report)
	}
}

// TestInspectCompactErrors pins the failure documents a focused inspection
// returns under version 2.
func TestInspectCompactErrors(t *testing.T) {
	t.Run("unknown task", func(t *testing.T) {
		newInspectFixture(t, 1, 1, 1)
		var out, stderr strings.Builder
		code := Main([]string{"inspect", "T404", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d, want %d", code, exitOperationFailed)
		}
		var failure errorDocument
		if err := json.Unmarshal([]byte(out.String()), &failure); err != nil {
			t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
		}
		if failure.Error.Code != "task_not_found" {
			t.Fatalf("code %q", failure.Error.Code)
		}
	})
	t.Run("no saved run", func(t *testing.T) {
		newStatusFixture(t)
		var out, stderr strings.Builder
		code := Main([]string{"inspect", "T1", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitUnavailable {
			t.Fatalf("exit %d, want %d", code, exitUnavailable)
		}
		var failure errorDocument
		if err := json.Unmarshal([]byte(out.String()), &failure); err != nil {
			t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
		}
		if failure.Error.Code != "run_not_found" {
			t.Fatalf("code %q", failure.Error.Code)
		}
	})
}

// TestInspectCompactFlagScope pins which commands accept the paging flags.
func TestInspectCompactFlagScope(t *testing.T) {
	for _, args := range [][]string{
		{"inspect", "T1", "--after", "5"},
		{"inspect", "T1", "--checks-after", "3"},
		{"inspect", "T1", "--limit", "100"},
		{"inspect", "T1", "--limit", "4", "--after", "2", "--checks-after", "1"},
		{"inspect", "T1", "--full"},
		{"events", "--limit", "500"},
	} {
		if _, err := Parse(args); err != nil {
			t.Fatalf("%v must parse: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"inspect", "T1", "--limit", "101"},
		{"inspect", "T1", "--task", "T1"},
		{"inspect", "T1", "--kind", "check"},
		{"inspect", "T1", "--checks-after", "-1"},
		{"inspect", "T1", "--full", "--after", "2"},
		{"inspect", "T1", "--full", "--checks-after", "2"},
		{"inspect", "T1", "--full", "--limit", "4"},
		{"events", "--checks-after", "2"},
		{"status", "--after", "2"},
		{"status", "--checks-after", "2"},
		{"status", "--limit", "10"},
		{"doctor", "--after", "2"},
	} {
		if _, err := Parse(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// TestInspectPlainRendersFromTheDocument checks that a plain version 2
// inspection renders the same summary as text, with the routes into the
// bounded history.
func TestInspectPlainRendersFromTheDocument(t *testing.T) {
	newInspectFixture(t, 12, 2, 2)
	var out, stderr strings.Builder
	code := Main([]string{"inspect", "T1", "--contract", "v2"}, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	text := out.String()
	for _, want := range []string{
		"Task: T1",
		"Controller:",
		"Current attempt:",
		"feedback:",
		"Attempts:",
		"Checks:",
		"Detail: looper inspect T1 --contract v2 --full --json",
		"Events: looper events --contract v2 --task T1",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain inspection is missing %q:\n%s", want, text)
		}
	}
	if got := len(text); got > 4*inspectBytesBudget {
		t.Fatalf("plain inspection is %d bytes; the rendering must stay bounded", got)
	}
}

// TestChecksPagesAcrossQueryChunks exercises the store's chunked check query
// directly: a task whose history grew past the chunk size still returns exact
// totals and a gapless forward walk.
func TestChecksPagesAcrossQueryChunks(t *testing.T) {
	clearSettingEnv(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const attempts = checksQueryChunk + 10
	ids := make([]string, attempts)
	for i := range ids {
		ids[i] = fmt.Sprintf("task-chunk-%04d", i)
	}
	total := 0
	for i, id := range ids {
		if err = store.Check(id, fmt.Sprintf("rev-%04d", i), "verify", "fail", "artifact"); err != nil {
			t.Fatal(err)
		}
		total++
	}
	tail, err := store.ChecksPage(ids, 0, 40, true)
	if err != nil {
		t.Fatal(err)
	}
	if tail.Total != total || len(tail.Items) != 40 || !tail.HasMore {
		t.Fatalf("chunked tail: total %d items %d hasMore %v", tail.Total, len(tail.Items), tail.HasMore)
	}
	newest := tail.Items[len(tail.Items)-1].ID
	if newest != int64(total) {
		t.Fatalf("the tail must hold the newest records: %d of %d", newest, total)
	}
	seen := map[int64]bool{}
	var after int64
	for {
		page, err := store.ChecksPage(ids, after, 60, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if item.ID <= after || seen[item.ID] {
				t.Fatalf("check %d repeated or preceded the cursor %d", item.ID, after)
			}
			seen[item.ID] = true
		}
		if !page.HasMore {
			break
		}
		after = page.Cursor
	}
	if len(seen) != total {
		t.Fatalf("the chunked walk collected %d of %d records", len(seen), total)
	}
}
