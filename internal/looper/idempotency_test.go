package looper

// The AX-012 acceptance fixtures. A caller-supplied idempotency key scoped to
// the project and operation makes retries safe: concurrent submissions and
// retries after lost client output or a controller restart apply one
// mutation, changed content under the same key is rejected, intentional
// same-content submissions with distinct keys remain possible, key input is
// bounded, and every answer is queryable from the durable receipts.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// keyedProject builds one project a live-looking controller owns: git root,
// controller metadata, the repository lock, and a saved state whose task file
// and export hash agree, so a keyed add-task can submit through the CLI and a
// protocol engine on the same directory can claim and apply it.
func keyedProject(t *testing.T, command string) (Config, string) {
	t.Helper()
	c := repoConfig(t, command)
	b := testBacklog()
	if err := writeJSON(filepath.Join(c.Root, "to-do.json"), b); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)
	t.Cleanup(release)
	original, err := os.ReadFile(filepath.Join(c.Root, "to-do.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = store.Save(&State{
		Version: 2, Root: c.Root, Todo: "to-do.json", Backlog: b,
		ExportHash: digest(original), Head: "head", Branch: "main", Tip: "tip",
	}); err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// keyedTaskFile writes one task batch the CLI can submit.
func keyedTaskFile(t *testing.T, id, title string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	tasks := []Task{{ID: id, Title: title, Priority: 1, Status: "todo", Details: "Acceptance: appears once."}}
	if err := writeJSON(path, tasks); err != nil {
		t.Fatal(err)
	}
	return path
}

// keyedEngineOn loads the saved state of one keyed project into a protocol
// engine that answers as the named controller session.
func keyedEngineOn(t *testing.T, dir, session string) *Engine {
	t.Helper()
	e := protocolEngineOn(t, dir, session)
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil {
		t.Fatal("the keyed project saved no state")
	}
	e.Config.Root = saved.Root
	e.Config.Todo = filepath.Join(saved.Root, saved.Todo)
	e.Repo = &Repository{Root: saved.Root}
	e.State = saved
	return e
}

// submitKeyedAddTask drives `looper add-task` through Main and returns the
// exit code with stdout and stderr.
func submitKeyedAddTask(t *testing.T, c Config, file, key string, v2 bool) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	args := []string{"add-task", file, "--json"}
	if key != "" {
		args = append(args, "--idempotency-key", key)
	}
	if v2 {
		args = append(args, "--contract", "v2")
	}
	code := Main(args, &out, &stderr)
	return code, out.String(), stderr.String()
}

// The key option belongs to the mutation commands, and its value is bounded
// at parse time so nothing out of range reaches an envelope or a receipt.
func TestIdempotencyKeyOptionParsing(t *testing.T) {
	// Each command carries its own argument shape; `config` takes no
	// positional at all.
	for command, args := range map[string][]string{
		"add-task": {"tasks.json"},
		"add-plan": {"plan.md"},
		"config":   nil,
	} {
		c, err := Parse(append([]string{command}, append(args, "--idempotency-key", "release-1")...))
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		if c.IdempotencyKey != "release-1" {
			t.Fatalf("%s parsed key %q", command, c.IdempotencyKey)
		}
	}
	for _, command := range []string{"status", "stop", "run", "requests"} {
		_, err := Parse([]string{command, "--idempotency-key", "release-1"})
		if err == nil {
			t.Fatalf("%s accepted --idempotency-key", command)
		}
		p, ok := err.(*PublicError)
		if !ok || p.Code != "option_command_mismatch" {
			t.Fatalf("%s refusal: %+v", command, err)
		}
	}
	for name, key := range map[string]string{
		"empty":     "",
		"oversized": strings.Repeat("k", maxIdempotencyKeyBytes+1),
		"control":   "key\x01",
		"not-utf8":  "key\xff",
	} {
		if _, err := Parse([]string{"add-task", "tasks.json", "--idempotency-key", key}); err == nil {
			t.Fatalf("%s key parsed", name)
		}
	}
	// The documented bound itself parses.
	if _, err := Parse([]string{"add-task", "tasks.json", "--idempotency-key", strings.Repeat("k", maxIdempotencyKeyBytes)}); err != nil {
		t.Fatalf("a 128-byte key must parse: %v", err)
	}
}

// A hand-written envelope cannot smuggle an unbounded key past the loader,
// and publication refuses one before any envelope is written.
func TestEnvelopeBoundsIdempotencyKey(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(requestDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("k", maxIdempotencyKeyBytes+1)
	if err := writeJSON(filepath.Join(requestDir(dir), "wide.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "wide", Type: RequestTypeAddTask,
		Session: "run-live", IdempotencyKey: oversized,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettingsRequest(filepath.Join(requestDir(dir), "wide.json")); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("an oversized key must fail the load: %v", err)
	}
	req := SettingsRequest{Version: SettingsRequestVersion, ID: "pub", Type: RequestTypeAddTask, IdempotencyKey: "bad\x02key"}
	if err := publishSettingsRequest(dir, &req); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("publication must refuse an unusable key: %v", err)
	}
	if names := queuedJSONNames(t, dir); len(names) != 1 {
		t.Fatalf("only the seeded envelope may exist, got %v", names)
	}
}

// The acceptance core: submit with a key, lose nothing, apply once, and get
// the original request back for the same key and payload — with no second
// envelope published. The emitted receipts validate against the published
// schema, and the retained record exposes the key.
func TestAddTaskKeyReplayAppliesOneMutation(t *testing.T) {
	c, dir := keyedProject(t, "add-task")
	file := keyedTaskFile(t, "T101", "keyed task")
	code, out, stderr := submitKeyedAddTask(t, c, file, "release-1", true)
	if code != 0 {
		t.Fatalf("submission failed: %d %s %s", code, out, stderr)
	}
	first := decodeReceipt(t, out, stderr)
	if first.Request.IdempotencyKey != "release-1" || first.Request.Duplicate {
		t.Fatalf("first receipt: %+v", first.Request)
	}
	if first.Request.PayloadDigest == "" {
		t.Fatal("the first receipt must carry the canonical payload digest")
	}
	// The controller claims and applies the envelope.
	e := keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	if task := e.State.Backlog.Task("T101"); task == nil {
		t.Fatal("the keyed add-task never applied")
	}
	if rec := e.requestReceipt(first.Request.ID); rec == nil || rec.IdempotencyKey != "release-1" ||
		rec.State != RequestApplied || rec.PayloadDigest == "" {
		t.Fatalf("applied receipt: %+v", rec)
	}
	// A retry after lost output: the same key and payload returns the
	// original request, publishes nothing, and applies nothing again.
	code, out, stderr = submitKeyedAddTask(t, c, file, "release-1", true)
	if code != 0 {
		t.Fatalf("retry failed: %d %s %s", code, out, stderr)
	}
	second := decodeReceipt(t, out, stderr)
	if !second.Request.Duplicate || second.Request.ID != first.Request.ID || second.Request.State != RequestApplied {
		t.Fatalf("duplicate receipt: %+v", second.Request)
	}
	if second.Request.IdempotencyKey != "release-1" || second.Request.PayloadDigest != first.Request.PayloadDigest {
		t.Fatalf("duplicate receipt key material: %+v", second.Request)
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("the retry published a second envelope: %v", names)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, task := range saved.Backlog.Tasks {
		if task.ID == "T101" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the keyed retry applied the task %d times", count)
	}
	// The retained record a caller queries names the key.
	code, doc, recordOut := runRequestsCLI(t, first.Request.ID, "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("record query failed: %d %s", code, recordOut)
	}
	if doc == nil || doc.Request == nil || doc.Request.IdempotencyKey != "release-1" {
		t.Fatalf("record key: %+v", doc)
	}
}

// Changed content under the same key is rejected before anything is
// published, with the structured conflict the version 2 contract promises.
func TestAddTaskKeyConflictRejectsChangedPayload(t *testing.T) {
	c, dir := keyedProject(t, "add-task")
	first := keyedTaskFile(t, "T101", "keyed task")
	code, out, _ := submitKeyedAddTask(t, c, first, "release-1", true)
	if code != 0 {
		t.Fatalf("submission failed: %d %s", code, out)
	}
	e := keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	changed := keyedTaskFile(t, "T102", "different task")
	code, out, stderr := submitKeyedAddTask(t, c, changed, "release-1", true)
	if code != 1 {
		t.Fatalf("a changed payload under the same key must fail the transport: %d %s %s", code, out, stderr)
	}
	doc := requestErrorDoc(t, out)
	if doc.Error.Code != "idempotency_key_conflict" {
		t.Fatalf("error code: %s", doc.Error.Code)
	}
	if doc.Error.Retry != retryAfterChange {
		t.Fatalf("retry guidance: %s", doc.Error.Retry)
	}
	if original, _ := doc.Error.Expected["original_request"].(string); !strings.HasPrefix(original, "settings-") && !strings.HasPrefix(original, "add-") {
		t.Fatalf("the conflict must name the original request: %v", doc.Error.Expected)
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("the conflicting retry published an envelope: %v", names)
	}
	// The legacy contract keeps its one-line refusal and exit status.
	var plain bytes.Buffer
	code = Main([]string{"add-task", changed, "--idempotency-key", "release-1"}, &plain, &plain)
	if code != 1 || !strings.Contains(plain.String(), "release-1") {
		t.Fatalf("legacy conflict: %d %q", code, plain.String())
	}
}

// Keys deduplicate retries, never task content: distinct keys carrying
// distinct batches both apply, and the same batch under a new key reaches
// backlog validation instead of the key layer's duplicate answer.
func TestAddTaskDistinctKeysApplyTwice(t *testing.T) {
	c, dir := keyedProject(t, "add-task")
	first := keyedTaskFile(t, "T101", "same wording")
	second := keyedTaskFile(t, "T102", "same wording")
	for _, tc := range []struct{ key, file string }{
		{"release-1", first},
		{"release-2", second},
	} {
		code, out, stderr := submitKeyedAddTask(t, c, tc.file, tc.key, true)
		if code != 0 {
			t.Fatalf("key %s submission failed: %d %s %s", tc.key, code, out, stderr)
		}
		e := keyedEngineOn(t, dir, "run-live")
		e.processAddTaskRequests()
		if e.err != nil {
			t.Fatal(e.err)
		}
	}
	e := keyedEngineOn(t, dir, "run-live")
	for _, id := range []string{"T101", "T102"} {
		if e.State.Backlog.Task(id) == nil {
			t.Fatalf("distinct keys must add both tasks; %s missing", id)
		}
	}
	// The first batch again under a third key: nothing replays, nothing
	// conflicts — the backlog's own duplicate-identifier validation refuses
	// it, which is the documented no-global-dedup behavior.
	code, out, _ := submitKeyedAddTask(t, c, first, "release-3", true)
	if code != 0 {
		t.Fatalf("the third submission must publish: %d %s", code, out)
	}
	e = keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, task := range saved.Backlog.Tasks {
		if task.ID == "T101" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the duplicate task batch applied %d times", count)
	}
	if rec := e.requestByIdempotencyKey(PublicRequestAddTask, "release-3"); rec == nil || rec.State != RequestRejected {
		t.Fatalf("the duplicate batch must be refused by validation, not the key: %+v", rec)
	}
}

// Two submitters that both passed the pre-check before either envelope was
// claimed still cause exactly one mutation: the claim-time resolution answers
// the second envelope from the receipt the first committed.
func TestAddTaskKeyConcurrentSubmissionsApplyOnce(t *testing.T) {
	_, dir := keyedProject(t, "add-task")
	tasks := []Task{{ID: "T101", Title: "keyed task", Priority: 1, Status: "todo", Details: "Acceptance: appears once."}}
	for _, id := range []string{"settings-race1", "settings-race2"} {
		req := SettingsRequest{Version: SettingsRequestVersion, ID: id, Type: RequestTypeAddTask,
			Session: "run-live", Tasks: tasks, IdempotencyKey: "release-1"}
		if err := publishSettingsRequest(dir, &req); err != nil {
			t.Fatal(err)
		}
	}
	e := keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, task := range saved.Backlog.Tasks {
		if task.ID == "T101" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent same-key submissions applied the task %d times", count)
	}
	for _, id := range []string{"settings-race1", "settings-race2"} {
		ack, ok := readRequestAck(dir, id)
		if !ok || ack.Status != AckApplied {
			t.Fatalf("the racing submission %s must answer from the receipt: %+v, %v", id, ack, ok)
		}
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("both envelopes must be consumed: %v", names)
	}
}

// A retry that reaches the controller after a restart targets a new session.
// The key resolves before the session check, so the recorded outcome answers
// instead of a stale-session refusal and a second mutation.
func TestAddTaskKeyRestartReplay(t *testing.T) {
	c, dir := keyedProject(t, "add-task")
	file := keyedTaskFile(t, "T101", "keyed task")
	code, out, _ := submitKeyedAddTask(t, c, file, "release-1", true)
	if code != 0 {
		t.Fatalf("submission failed: %d %s", code, out)
	}
	first := decodeReceipt(t, out, "")
	e := keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	e.Store.Close()
	// The restart: a new controller session, and a retry envelope that
	// targets the old session, exactly as a submitter that never saw the
	// first answer would write it.
	retry := SettingsRequest{Version: SettingsRequestVersion, ID: "add-after-restart", Type: RequestTypeAddTask,
		Session: "run-live", Tasks: []Task{{ID: "T101", Title: "keyed task", Priority: 1, Status: "todo", Details: "Acceptance: appears once."}},
		IdempotencyKey: "release-1"}
	if err := writeJSON(filepath.Join(requestDir(dir), retry.ID+".json"), retry); err != nil {
		t.Fatal(err)
	}
	next := keyedEngineOn(t, dir, "run-next")
	next.processAddTaskRequests()
	if next.err != nil {
		t.Fatal(next.err)
	}
	ack, ok := readRequestAck(dir, retry.ID)
	if !ok || ack.Status != AckApplied {
		t.Fatalf("the restart must replay the recorded outcome: %+v, %v", ack, ok)
	}
	saved, err := next.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, task := range saved.Backlog.Tasks {
		if task.ID == "T101" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the restart replay applied the task %d times", count)
	}
	if rec := next.requestReceipt(retry.ID); rec == nil || !strings.Contains(rec.Summary, first.Request.ID) {
		t.Fatalf("the replay must mirror the original request: %+v", rec)
	}
}

// Settings keys resolve the same way: the applied receipt carries the key and
// digest, the same key and patch replays it, and a changed patch conflicts.
func TestSettingsKeyReplayAndConflict(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{Version: 2}
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	first := request(e, map[string]any{"agent": "claude"}, profileDigest(e.Config.ProfilePath))
	first.IdempotencyKey = "tune-agent"
	ack := e.applySettingsRequest(first)
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	rec := e.requestReceipt(first.ID)
	if rec == nil || rec.IdempotencyKey != "tune-agent" || rec.PayloadDigest == "" || rec.State != RequestApplied {
		t.Fatalf("applied settings receipt: %+v", rec)
	}
	// The same key and payload under a new identifier replays the answer.
	second := request(e, map[string]any{"agent": "claude"}, profileDigest(e.Config.ProfilePath))
	second.IdempotencyKey = "tune-agent"
	replay := e.applySettingsRequest(second)
	if replay.Status != AckApplied || replay.ID != second.ID {
		t.Fatalf("replay: %+v", replay)
	}
	if mirror := e.requestReceipt(second.ID); mirror == nil || !strings.Contains(mirror.Summary, first.ID) {
		t.Fatalf("the replay must mirror the original: %+v", mirror)
	}
	// A changed payload under the same key is a conflict.
	third := request(e, map[string]any{"agent": "codex"}, profileDigest(e.Config.ProfilePath))
	third.IdempotencyKey = "tune-agent"
	conflict := e.applySettingsRequest(third)
	if conflict.Status != AckConflict || !strings.Contains(conflict.Message, first.ID) {
		t.Fatalf("conflict: %+v", conflict)
	}
	if ref := e.requestReceipt(third.ID); ref == nil || ref.State != RequestRejected || ref.IdempotencyKey != "tune-agent" {
		t.Fatalf("conflict receipt: %+v", ref)
	}
	// The profile still holds the first answer only.
	p, err := LoadProfile(e.Config.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != "claude" {
		t.Fatalf("the conflicting patch must not apply: %q", p.Agent)
	}
}

// Plan-import keys address content, not paths: a queued keyed import answers
// a same-key retry from its receipt, an applied one replays its result
// wherever the same bytes came from, and changed content under the key is
// refused.
func TestAddPlanKeyLifecycle(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	plan := planFile(t, "# New requirements\nAdd telemetry to the service.")
	first := &SettingsRequest{ID: "import-key-1", Type: RequestTypePlanImport,
		Session: e.controllerSessionID(), Paths: []string{plan}, IdempotencyKey: "import-telemetry"}
	e.acceptPlanImport(first, false)
	if e.err != nil {
		t.Fatal(e.err)
	}
	if rec := e.importReceipt("import-key-1"); rec == nil || rec.IdempotencyKey != "import-telemetry" || rec.PayloadDigest == "" {
		t.Fatalf("queued keyed import: %+v", rec)
	}
	if common := e.requestReceipt("import-key-1"); common == nil || common.IdempotencyKey != "import-telemetry" {
		t.Fatalf("mirrored receipt: %+v", common)
	}
	// The same key and content while the original is queued: refused, naming
	// the original, with no second import queued.
	second := &SettingsRequest{ID: "import-key-2", Type: RequestTypePlanImport,
		Session: e.controllerSessionID(), Paths: []string{plan}, IdempotencyKey: "import-telemetry"}
	e.acceptPlanImport(second, false)
	if e.err != nil {
		t.Fatal(e.err)
	}
	if len(e.State.Imports) != 1 {
		t.Fatalf("a same-key retry against a queued import must not queue a second import: %+v", e.State.Imports)
	}
	if rec := e.requestReceipt("import-key-2"); rec == nil || rec.State != RequestRejected || !strings.Contains(rec.Error, "import-key-1") {
		t.Fatalf("in-flight retry receipt: %+v", rec)
	}
	// Apply the original, then retry the same key with the same bytes from a
	// different path: the keyed replay returns the applied result and adds no
	// task twice.
	e.processPlanImports(context.Background())
	if e.err != nil {
		t.Fatal(e.err)
	}
	sameBytes := planFile(t, "# New requirements\nAdd telemetry to the service.")
	third := &SettingsRequest{ID: "import-key-3", Type: RequestTypePlanImport,
		Session: e.controllerSessionID(), Paths: []string{sameBytes}, IdempotencyKey: "import-telemetry"}
	e.acceptPlanImport(third, false)
	if e.err != nil {
		t.Fatal(e.err)
	}
	ack, err := readPlanAck(e.Dir, "import-key-3")
	if err != nil || ack.Status != ImportApplied {
		t.Fatalf("keyed replay must return the applied result: %+v, %v", ack, err)
	}
	count := 0
	for _, task := range e.State.Backlog.Tasks {
		if strings.Contains(task.Title, "telemetry") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("the keyed import added the telemetry task %d times", count)
	}
	// Changed content under the same key is rejected.
	changed := planFile(t, "# Other requirements\nReplace the storage layer.")
	fourth := &SettingsRequest{ID: "import-key-4", Type: RequestTypePlanImport,
		Session: e.controllerSessionID(), Paths: []string{changed}, IdempotencyKey: "import-telemetry"}
	e.acceptPlanImport(fourth, false)
	if e.err != nil {
		t.Fatal(e.err)
	}
	if rec := e.requestReceipt("import-key-4"); rec == nil || rec.State != RequestRejected ||
		!strings.Contains(rec.Error, "different content") || rec.IdempotencyKey != "import-telemetry" {
		t.Fatalf("conflict receipt: %+v", rec)
	}
	if len(e.State.Imports) != 1 {
		t.Fatalf("the conflicting import must queue nothing: %+v", e.State.Imports)
	}
}

// readPlanAck loads one plan-import acknowledgement.
func readPlanAck(dir, id string) (*planImportAck, error) {
	var ack planImportAck
	if err := readJSON(filepath.Join(ackDir(dir), id+".json"), &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// The add-plan CLI carries the key on its envelope and its receipt. Two
// submissions that race ahead of any claim both publish, and the controller's
// claim-time resolution still queues one import; after a receipt exists, the
// retained-key pre-check answers a same-key resubmission with the original
// request and refuses changed content.
func TestAddPlanKeySubmissionAndResubmission(t *testing.T) {
	_, dir := keyedProject(t, "add-plan")
	plan := planFile(t, "# Requirements\nAdd one feature.")
	submit := func(path, key string) (int, string, string) {
		var out, stderr bytes.Buffer
		args := []string{"add-plan", path, "--json"}
		if key != "" {
			args = append(args, "--idempotency-key", key)
		}
		code := Main(args, &out, &stderr)
		return code, out.String(), stderr.String()
	}
	code, out, stderr := submit(plan, "plan-1")
	if code != 0 {
		t.Fatalf("submission failed: %d %s %s", code, out, stderr)
	}
	first := decodeReceipt(t, out, stderr)
	if first.Request.IdempotencyKey != "plan-1" || first.Request.Duplicate || first.Request.PayloadDigest == "" {
		t.Fatalf("first receipt: %+v", first.Request)
	}
	// A racing second submission finds no receipt yet, so its envelope also
	// publishes; one import must still queue.
	code, out, stderr = submit(plan, "plan-1")
	if code != 0 {
		t.Fatalf("racing submission failed: %d %s %s", code, out, stderr)
	}
	second := decodeReceipt(t, out, stderr)
	if second.Request.Duplicate || second.Request.ID == first.Request.ID {
		t.Fatalf("the racing receipt names its own durable envelope: %+v", second.Request)
	}
	e := keyedEngineOn(t, dir, "run-live")
	e.claimPlanImports()
	if e.err != nil {
		t.Fatal(e.err)
	}
	if len(e.State.Imports) != 1 {
		t.Fatalf("one keyed import must queue, got %+v", e.State.Imports)
	}
	// Whichever envelope claims first owns the key; the other answers from
	// its receipt. Claim order follows the envelope names, so both orders are
	// correct as long as exactly one import queued and exactly one envelope
	// was refused with the other named.
	originalID, refusedID := "", ""
	for _, id := range []string{first.Request.ID, second.Request.ID} {
		rec := e.requestReceipt(id)
		if rec == nil {
			t.Fatalf("request %s left no receipt", id)
		}
		switch rec.State {
		case RequestAccepted, RequestWaiting:
			originalID = id
		case RequestRejected:
			refusedID, _ = id, rec.Error
			if !strings.Contains(rec.Error, "query that request") && !strings.Contains(rec.Error, "belongs to request") {
				t.Fatalf("the refused envelope must name the original: %+v", rec)
			}
		default:
			t.Fatalf("request %s resolved to %q", id, rec.State)
		}
	}
	if originalID == "" || refusedID == "" || originalID == refusedID {
		t.Fatalf("one original (%q) and one refusal (%q) must resolve", originalID, refusedID)
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("both envelopes must be consumed: %v", names)
	}
	// With the queued receipt retained, a same-key resubmission returns the
	// original request and publishes nothing.
	code, out, stderr = submit(plan, "plan-1")
	if code != 0 {
		t.Fatalf("resubmission failed: %d %s %s", code, out, stderr)
	}
	duplicate := decodeReceipt(t, out, stderr)
	if !duplicate.Request.Duplicate || duplicate.Request.ID != originalID {
		t.Fatalf("duplicate receipt: %+v", duplicate.Request)
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("the resubmission must not publish: %v", names)
	}
	// Changed content under the same key is a conflict: the structured code
	// under version 2, the plain refusal under the default contract.
	other := planFile(t, "# Requirements\nAdd a different feature.")
	var v2out bytes.Buffer
	code = Main([]string{"add-plan", other, "--json", "--contract", "v2", "--idempotency-key", "plan-1"}, &v2out, &v2out)
	if code != 1 || !strings.Contains(v2out.String(), "idempotency_key_conflict") {
		t.Fatalf("conflicting add-plan: %d %s", code, v2out.String())
	}
	var plainOut bytes.Buffer
	code = Main([]string{"add-plan", other, "--idempotency-key", "plan-1"}, &plainOut, &plainOut)
	if code != 1 || !strings.Contains(plainOut.String(), "already used by request") {
		t.Fatalf("plain conflicting add-plan: %d %s", code, plainOut.String())
	}
}

// A key resolves inside its own operation: the same key on another request
// type is a different key, so both submissions proceed.
func TestKeyScopedToOperation(t *testing.T) {
	c, dir := keyedProject(t, "add-task")
	file := keyedTaskFile(t, "T101", "keyed task")
	code, out, _ := submitKeyedAddTask(t, c, file, "shared-key", true)
	if code != 0 {
		t.Fatalf("add-task submission failed: %d %s", code, out)
	}
	e := keyedEngineOn(t, dir, "run-live")
	e.processAddTaskRequests()
	if e.err != nil {
		t.Fatal(e.err)
	}
	// The same key on add-plan is untouched by the add-task receipt.
	plan := planFile(t, "# Requirements\nAdd one feature.")
	var out2, stderr2 bytes.Buffer
	code = Main([]string{"add-plan", plan, "--json", "--idempotency-key", "shared-key"}, &out2, &stderr2)
	if code != 0 {
		t.Fatalf("an add-plan submission under a key used by add-task must submit: %d %s %s", code, out2.String(), stderr2.String())
	}
	receipt := decodeReceipt(t, out2.String(), stderr2.String())
	if receipt.Request.Duplicate {
		t.Fatal("the add-plan receipt must not report the add-task request as its original")
	}
}

// The retained-record query names the key, in both the JSON document and the
// plain rendering.
func TestRequestRecordNamesKey(t *testing.T) {
	requestsProject(t, func(s *State) {
		seedRequestReceipts(s)
		s.Requests[3].IdempotencyKey = "release-1"
	})
	code, doc, out := runRequestsCLI(t, "add-done", "--contract", "v2", "--json")
	if code != 0 {
		t.Fatalf("query failed: %d %s", code, out)
	}
	if doc.Request == nil || doc.Request.IdempotencyKey != "release-1" {
		t.Fatalf("record key: %+v", doc.Request)
	}
	var plain bytes.Buffer
	if code := Main([]string{"requests", "add-done"}, &plain, &plain); code != 0 || !strings.Contains(plain.String(), "release-1") {
		t.Fatalf("plain record must name the key: %d %s", code, plain.String())
	}
}
