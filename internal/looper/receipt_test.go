package looper

// The AX-011 acceptance fixtures. Every mutation — add-task, add-plan,
// settings, and stop — returns a durable receipt immediately: the response
// is parseable while the controller plans or checks, the identifier it
// returns is queryable, waiting is explicit and bounded, and a timeout is an
// outcome of the wait, never a cancellation or rejection.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
	"github.com/nibzard/looper/internal/ui"
)

// receiptContractSchema reads the published receipt schema, anchored at this
// file's directory because fixtures chdir into temporary projects.
func receiptContractSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "request-receipt.schema.json"))
	if err != nil {
		t.Skipf("request-receipt schema is absent: %v", err)
	}
	return b
}

// receiptProject builds one project whose repository a live controller owns:
// controller metadata, the repository lock, and a saved state the queries can
// read. The controller itself never answers, exactly like one whose planning
// or check phase holds the loop — the condition that used to silence callers.
func receiptProject(t *testing.T, command string) (Config, string) {
	t.Helper()
	c := repoConfig(t, command)
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)
	t.Cleanup(release)
	return c, dir
}

// seedReceiptState saves one minimal state so request queries have a run.
func seedReceiptState(t *testing.T, dir string, mutate func(*State)) {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &State{Version: 2, Root: "root", Todo: "to-do.json", Backlog: testBacklog()}
	if mutate != nil {
		mutate(s)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
}

// decodeReceipt fails unless stdout carries exactly one receipt document
// that validates against the published schema.
func decodeReceipt(t *testing.T, out, stderr string) *requestReceiptDocument {
	t.Helper()
	if stderr != "" {
		t.Fatalf("the receipt mixes text into stderr: %q", stderr)
	}
	var doc requestReceiptDocument
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("stdout is not one receipt document: %v\n%s", err, out)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	if err := contractschema.ValidateJSON(receiptContractSchema(t), []byte(out)); err != nil {
		t.Fatalf("the emitted receipt violates its schema: %v\n%s", err, out)
	}
	return &doc
}

// The recorded gap: `looper add-plan <path> --json` against a controller
// whose import waits printed nothing before the caller's timeout killed it.
// The immediate receipt answers during any long phase, under both contract
// selections, and the envelope survives as the durable submission evidence.
func TestMutationReturnsImmediateReceiptDuringLongPhase(t *testing.T) {
	for _, contract := range []struct {
		name   string
		sel    func(Config) Config
		expect int
	}{
		{"legacy json", func(c Config) Config {
			c.JSON = true
			return c
		}, exitOK},
		{"version 2 json", func(c Config) Config { return v2Config(c, "add-plan", true) }, exitOK},
	} {
		t.Run(contract.name, func(t *testing.T) {
			c, dir := receiptProject(t, "add-plan")
			plan := filepath.Join(c.Root, "plan.md")
			if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cc := contract.sel(c)
			cc.Plans = []string{plan}
			var out, stderr strings.Builder
			started := time.Now()
			code := runAddPlan(cc, &out, &stderr)
			if code != contract.expect {
				t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
			}
			if elapsed := time.Since(started); elapsed > testBudget(5*time.Second) {
				t.Fatalf("the receipt took %s; the caller must not wait", elapsed)
			}
			doc := decodeReceipt(t, out.String(), stderr.String())
			if doc.Kind != requestReceiptKind || doc.Request.Type != PublicRequestAddPlan {
				t.Fatalf("receipt identity: %+v", doc)
			}
			if doc.Request.State != RequestSubmitted {
				t.Fatalf("a fresh submission reports %q", doc.Request.State)
			}
			if doc.Request.ID == "" || doc.Request.CreatedAt == "" || doc.Request.ControllerSession != "run-live" {
				t.Fatalf("receipt request evidence: %+v", doc.Request)
			}
			if want := "looper requests " + doc.Request.ID + " --contract v2"; doc.Query.Command != want {
				t.Fatalf("query route %q", doc.Query.Command)
			}
			// The envelope is the durable submission evidence: no answer was
			// consumed or invented.
			entries, _ := os.ReadDir(requestDir(dir))
			if len(entries) != 1 {
				t.Fatalf("the envelope must survive, found %d", len(entries))
			}
			if _, ok := readRequestAck(dir, doc.Request.ID); ok {
				t.Fatal("no acknowledgement may exist before the controller answers")
			}
		})
	}
}

// Every identifier a receipt returns is queryable: add-task and add-plan
// through their envelopes, and stop through the scoped stop file the stop
// transport publishes.
func TestReturnedReceiptIDIsQueryable(t *testing.T) {
	c, dir := receiptProject(t, "add-plan")
	seedReceiptState(t, dir, nil)

	plan := filepath.Join(c.Root, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	pc := v2Config(c, "add-plan", true)
	pc.Plans = []string{plan}
	var out, stderr strings.Builder
	if code := runAddPlan(pc, &out, &stderr); code != exitOK {
		t.Fatalf("add-plan exit %d: %s%s", code, out.String(), stderr.String())
	}
	planReceipt := decodeReceipt(t, out.String(), stderr.String())

	tasks := filepath.Join(c.Root, "tasks.json")
	if err := os.WriteFile(tasks, []byte(`{"id":"T9","title":"nine","priority":1,"status":"todo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	tc := v2Config(c, "add-task", true)
	tc.Plans = []string{tasks}
	out.Reset()
	stderr.Reset()
	if code := runAddTask(tc, &out, &stderr); code != exitOK {
		t.Fatalf("add-task exit %d: %s%s", code, out.String(), stderr.String())
	}
	taskReceipt := decodeReceipt(t, out.String(), stderr.String())

	out.Reset()
	stderr.Reset()
	if code := runStop(v2Config(c, "stop", true), dir, &out, &stderr); code != exitOK {
		t.Fatalf("stop exit %d: %s%s", code, out.String(), stderr.String())
	}
	stopReceipt := decodeReceipt(t, out.String(), stderr.String())
	if stopReceipt.Request.Type != PublicRequestStop || stopReceipt.Request.ID != "stop-run-live" {
		t.Fatalf("stop receipt: %+v", stopReceipt.Request)
	}

	for _, receipt := range []*requestReceiptDocument{planReceipt, taskReceipt, stopReceipt} {
		code, doc, raw := runRequestsCLI(t, receipt.Request.ID, "--contract", "v2", "--json")
		if code != exitOK {
			t.Fatalf("query %s: exit %d: %s", receipt.Request.ID, code, raw)
		}
		if doc.Request.ID != receipt.Request.ID || doc.Request.State != RequestSubmitted {
			t.Fatalf("record for %s: %+v", receipt.Request.ID, doc.Request)
		}
	}
	// The stop record names the public stop type, not an unknown transport.
	if code, doc, raw := runRequestsCLI(t, "stop-run-live", "--contract", "v2", "--json"); code != exitOK {
		t.Fatalf("stop record: exit %d: %s", code, raw)
	} else if doc.Request.Type != PublicRequestStop {
		t.Fatalf("stop record type %q", doc.Request.Type)
	}
}

// --wait is explicit and bounded: the flag parses only on the mutation
// commands that answer through the request protocol, and its bound must
// accompany the wait itself.
func TestMutationWaitFlagValidation(t *testing.T) {
	cases := []struct {
		args []string
		code string
		want string
	}{
		{[]string{"add-plan", "plan.md", "--wait"}, "", ""},
		{[]string{"add-task", "tasks.json", "--wait", "--wait-timeout", "5"}, "", ""},
		{[]string{"config", "--reset", "--wait"}, "", ""},
		// Stop's wait joined the family in AX-022: the durable, session-bound
		// drain wait parses like every other mutation wait.
		{[]string{"stop", "--wait"}, "", ""},
		{[]string{"stop", "--wait", "--wait-timeout", "5"}, "", ""},
		{[]string{"status", "--wait"}, "option_command_mismatch", "--wait is supported by"},
		{[]string{"events", "--wait-timeout", "5"}, "option_command_mismatch", "--wait is supported by"},
		{[]string{"add-plan", "--wait-timeout", "5"}, "option_conflict", "--wait-timeout bounds a --wait"},
		{[]string{"add-plan", "plan.md", "--wait", "--wait-timeout", "0"}, "option_value_invalid", "positive number of seconds"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			receiptProject(t, "status")
			_, err := Parse(tc.args)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("must parse: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("must refuse")
			}
			p, ok := err.(*PublicError)
			if !ok || p.Code != tc.code {
				t.Fatalf("error %v is not %s", err, tc.code)
			}
			if !strings.Contains(p.Message, tc.want) {
				t.Fatalf("message %q lacks %q", p.Message, tc.want)
			}
		})
	}
}

// A bounded wait that ends without an answer is an outcome of the wait: the
// version 2 transport reports request_pending with exit 5, and neither the
// envelope nor the request's standing changes.
func TestMutationWaitTimeoutIsAnOutcome(t *testing.T) {
	c, dir := receiptProject(t, "add-plan")
	plan := filepath.Join(c.Root, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cc := v2Config(c, "add-plan", true)
	cc.Plans = []string{plan}
	cc.Wait, cc.WaitTimeout = true, 200*time.Millisecond
	var out, stderr strings.Builder
	code := runAddPlan(cc, &out, &stderr)
	if code != exitWaitTimeout {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	doc := requestErrorDoc(t, out.String())
	if doc.Error.Code != "request_pending" || doc.Error.Retry != retryYes {
		t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
	}
	entries, _ := os.ReadDir(requestDir(dir))
	if len(entries) != 1 {
		t.Fatalf("a wait timeout must leave the envelope running, found %d", len(entries))
	}
}

// A satisfied wait reports the request record — the applied result with its
// added task identifiers — read from the run's durable evidence.
func TestMutationWaitReportsTheRecord(t *testing.T) {
	c, dir := receiptProject(t, "add-task")
	b := testBacklog()
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{
		Dir: dir, Output: io.Discard, Store: store,
		Config: c, Repo: &Repository{Root: c.Root},
		State:      &State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: b, ExportHash: digest(original)},
		controller: &ControllerInfo{Version: 1, Session: "run-live", PID: os.Getpid()},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1),
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 250; i++ {
			e.processAddTaskRequests()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	defer func() { <-done }()

	tasks := filepath.Join(c.Root, "tasks.json")
	if err = os.WriteFile(tasks, []byte(`{"id":"T9","title":"nine","priority":1,"status":"todo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cc := v2Config(c, "add-task", true)
	cc.Plans = []string{tasks}
	cc.Wait, cc.WaitTimeout = true, 30*time.Second
	var out, stderr strings.Builder
	code := runAddTask(cc, &out, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	var doc requestRecordDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one record document: %v\n%s", err, out.String())
	}
	if doc.Kind != requestRecordKind || doc.Request.State != RequestApplied {
		t.Fatalf("record: %+v", doc.Request)
	}
	if doc.Request.Result == nil || len(doc.Request.Result.AddedTaskIDs) != 1 || doc.Request.Result.AddedTaskIDs[0] != "T9" {
		t.Fatalf("applied result: %+v", doc.Request.Result)
	}
}

// The draining case explains itself at submission time: a plan submitted to
// a controller already draining stays queued for the next run, and the plain
// receipt says so.
func TestDrainingImportNoteExplainsOutcome(t *testing.T) {
	c, dir := receiptProject(t, "add-plan")
	if err := writeStopRequest(dir, stopRequest{Session: "run-live"}); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(c.Root, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cc := v2Config(c, "add-plan", false)
	cc.Plans = []string{plan}
	var out, stderr strings.Builder
	if code := runAddPlan(cc, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "draining") || !strings.Contains(out.String(), "next controller run") {
		t.Fatalf("the receipt must explain the drain: %q", out.String())
	}
}

// The held-input case explains itself at submission time: a task file whose
// content changed against the accepted export holds add-task answers until
// the boundary imports the edit. A formatting-only edit never holds, exactly
// like the controller's own equivalence rule.
func TestHeldInputNoteExplainsOutcome(t *testing.T) {
	c, dir := receiptProject(t, "add-task")
	b := testBacklog()
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	seedReceiptState(t, dir, func(s *State) {
		s.Todo = c.Todo
		// The accepted export names different content than the file holds.
		s.ExportHash = "different-digest"
		s.ExportSemantics = "different-semantics"
	})
	tasks := filepath.Join(c.Root, "tasks.json")
	if err := os.WriteFile(tasks, []byte(`{"id":"T9","title":"nine","priority":1,"status":"todo"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cc := v2Config(c, "add-task", false)
	cc.Plans = []string{tasks}
	var out, stderr strings.Builder
	if code := runAddTask(cc, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "task file changed outside Looper") {
		t.Fatalf("the receipt must explain the held input: %q", out.String())
	}

	// An equivalent reformatting of the accepted bytes adopts instead of
	// holding, so the note stays silent.
	seedReceiptState(t, dir, func(s *State) {
		s.Todo = c.Todo
		s.ExportHash = digest([]byte("  different bytes, same content  "))
		s.ExportSemantics = b.semanticFingerprint()
	})
	out.Reset()
	stderr.Reset()
	if code := runAddTask(cc, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if strings.Contains(out.String(), "task file changed outside Looper") {
		t.Fatalf("an equivalent edit must not hold: %q", out.String())
	}
}

// The request record carries an import's gates beside its other applied
// evidence, in the bounded JSON document and the plain rendering alike.
func TestRequestRecordCarriesGates(t *testing.T) {
	c := requestsProject(t, func(s *State) {
		seedRequestReceipts(s)
		for _, imp := range s.Imports {
			if imp.RequestID == "import-wait" {
				imp.Status = ImportApplied
				imp.Gates = []ImportGate{{TaskID: "T012", DependsOn: []string{"T070"}}}
			}
		}
		for _, rec := range s.Requests {
			if rec.ID == "import-wait" {
				rec.State = RequestApplied
				rec.WaitReason, rec.WaitDetail, rec.WaitSince = "", "", ""
			}
		}
	})
	code, doc, raw := runRequestsCLI(t, "import-wait", "--contract", "v2", "--json")
	if code != exitOK {
		t.Fatalf("exit %d: %s", code, raw)
	}
	if doc.Request.PlanCoverage == nil || len(doc.Request.PlanCoverage.Gates) != 1 ||
		doc.Request.PlanCoverage.Gates[0].TaskID != "T012" {
		t.Fatalf("gates missing from the record: %+v", doc.Request.PlanCoverage)
	}
	// The plain rendering states the same gate.
	var out strings.Builder
	cc := c
	cc.Command, cc.RequestID, cc.Contract, cc.JSON = "requests", "import-wait", ContractVersionV2, false
	if code := runRequestsQuery(cc, &out, &strings.Builder{}); code != exitOK {
		t.Fatalf("plain record exit %d", code)
	}
	if !strings.Contains(out.String(), "gate: T012 depends on T070") {
		t.Fatalf("plain record omits the gate: %q", out.String())
	}
}

// The stop receipt keeps the legacy plain lines byte for byte: the default
// contract's stop behavior was already immediate and explanatory.
func TestStopLegacyTextUnchanged(t *testing.T) {
	c, dir := receiptProject(t, "stop")
	seedReceiptState(t, dir, nil)
	var out, stderr strings.Builder
	if code := runStop(c, dir, &out, &stderr); code != exitOK {
		t.Fatalf("exit %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(out.String(), "Stop requested for session run-live. Active work will drain.") {
		t.Fatalf("legacy stop text changed: %q", out.String())
	}
}
