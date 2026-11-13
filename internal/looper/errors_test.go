package looper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// errorSchema reads the published error schema so the documents the CLI
// really produces are checked against it, not just the examples. The path is
// anchored at this file's directory, because most fixtures chdir into a
// temporary project before the document is validated.
func errorSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "error.schema.json"))
	if err != nil {
		t.Skipf("error schema is absent: %v", err)
	}
	return b
}

// errorProject prepares an empty git project the CLI can run in and chdirs
// there, mirroring repoConfig without parsing a specific command.
func errorProject(t *testing.T) Config {
	t.Helper()
	clearSettingEnv(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "base"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	if _, err = gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	c, err := Parse([]string{"status"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runV2Error runs Main with contract version 2 selected and returns the exit
// code with the decoded error document. It fails unless standard output
// carries exactly one JSON document that validates against the published
// error schema and standard output carries no human text.
func runV2Error(t *testing.T, args []string) (int, *errorDocument, string) {
	t.Helper()
	var out, stderr strings.Builder
	code := Main(args, &out, &stderr)
	if stderr.Len() > 0 {
		t.Fatalf("version 2 JSON errors mix human text into the failure: stderr=%q", stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	// A second JSON value on stdout would make the stream unparseable.
	dec := json.NewDecoder(strings.NewReader(out.String()))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(errorSchema(t), data); err != nil {
		t.Fatalf("the emitted error violates its schema: %v\n%s", err, data)
	}
	return code, &doc, out.String()
}

// TestV2ErrorsCoverRepresentativeFailures drives one failure per required
// class through the public CLI and checks the code, the retry guidance, and
// the exit code a caller needs without parsing English text.
func TestV2ErrorsCoverRepresentativeFailures(t *testing.T) {
	t.Run("missing run status", func(t *testing.T) {
		c := errorProject(t)
		code, doc, _ := runV2Error(t, []string{"status", "--contract", "v2", "--json"})
		if code != exitUnavailable {
			t.Fatalf("exit %d, want %d", code, exitUnavailable)
		}
		if doc.Error.Code != "run_not_found" || doc.Error.Retry != retryAfterChange {
			t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
		}
		if doc.Command != "looper status --contract v2" {
			t.Fatalf("command label %q", doc.Command)
		}
		_, dir := projectPaths(c)
		// A no-run error names every searched location (AX-025): the runtime
		// state database, the resolved base directory, the project root, and
		// the task path.
		paths := strings.Join(doc.Error.Paths, "\n")
		for _, want := range []string{
			filepath.Join(dir, "state.db"),
			c.BaseDir, c.Root, c.Todo,
		} {
			if !strings.Contains(paths, want) {
				t.Fatalf("the no-run error must name %s; paths were %v", want, doc.Error.Paths)
			}
		}
	})

	t.Run("missing run stop", func(t *testing.T) {
		errorProject(t)
		code, doc, _ := runV2Error(t, []string{"stop", "--contract", "v2", "--json"})
		if code != exitUnavailable || doc.Error.Code != "run_not_found" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
	})

	t.Run("missing run add-plan", func(t *testing.T) {
		c := errorProject(t)
		plan := filepath.Join(c.Root, "plan.md")
		if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		code, doc, _ := runV2Error(t, []string{"add-plan", plan, "--contract", "v2", "--json"})
		if code != exitUnavailable || doc.Error.Code != "run_not_found" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
	})

	t.Run("malformed input unknown option", func(t *testing.T) {
		errorProject(t)
		code, doc, _ := runV2Error(t, []string{"status", "--contract", "v2", "--json", "--frobnicate"})
		if code != exitUsage || doc.Error.Code != "unknown_option" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
		if doc.Error.Retry != retryAfterChange {
			t.Fatalf("retry %q", doc.Error.Retry)
		}
	})

	t.Run("malformed input option value", func(t *testing.T) {
		errorProject(t)
		code, doc, _ := runV2Error(t, []string{"events", "--after", "-7", "--contract", "v2", "--json"})
		if code != exitUsage || doc.Error.Code != "option_value_invalid" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
		if doc.Error.Observed["--after"] != "-7" {
			t.Fatalf("observed %v", doc.Error.Observed)
		}
	})

	t.Run("malformed input negotiated by environment", func(t *testing.T) {
		errorProject(t)
		t.Setenv("LOOPER_CONTRACT", "v2")
		code, doc, _ := runV2Error(t, []string{"status", "--json", "--frobnicate"})
		if code != exitUsage || doc.Error.Code != "unknown_option" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
	})

	t.Run("unsupported contract version stays plain", func(t *testing.T) {
		errorProject(t)
		var out, stderr strings.Builder
		code := Main([]string{"status", "--contract", "v3", "--json"}, &out, &stderr)
		if code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		if out.Len() > 0 {
			t.Fatalf("no contract was negotiated, so no document may print: %q", out.String())
		}
		if !strings.Contains(stderr.String(), "unsupported contract version") || !strings.Contains(stderr.String(), "legacy") {
			t.Fatalf("stderr must name the supported range: %q", stderr.String())
		}
	})

	t.Run("persistence unreadable store", func(t *testing.T) {
		c := errorProject(t)
		_, dir := projectPaths(c)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("this is not a sqlite database at all"), 0600); err != nil {
			t.Fatal(err)
		}
		code, doc, _ := runV2Error(t, []string{"status", "--contract", "v2", "--json"})
		if code != exitUnavailable || doc.Error.Code != "state_unreadable" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
	})

	t.Run("persistence unreadable profile", func(t *testing.T) {
		errorProject(t)
		// A config command loads the profile during parsing.
		cc, err := Parse([]string{"config"})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.MkdirAll(filepath.Dir(cc.ProfilePath), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(cc.ProfilePath, []byte("{not json"), 0600); err != nil {
			t.Fatal(err)
		}
		code, doc, _ := runV2Error(t, []string{"config", "--contract", "v2", "--json"})
		if code != exitUnavailable || doc.Error.Code != "profile_unreadable" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
	})

	t.Run("cursor invalid", func(t *testing.T) {
		c := errorProject(t)
		_, dir := projectPaths(c)
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Event("note", "one event", nil); err != nil {
			t.Fatal(err)
		}
		if err = store.Save(&State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: Backlog{Version: 1}}); err != nil {
			t.Fatal(err)
		}
		store.Close()
		code, doc, _ := runV2Error(t, []string{"events", "--after", "9", "--contract", "v2", "--json"})
		if code != exitOperationFailed || doc.Error.Code != "cursor_invalid" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
		if doc.Error.Retry != retryAfterChange {
			t.Fatalf("retry %q", doc.Error.Retry)
		}
		if doc.Error.Expected["event_id_at_most"] == nil {
			t.Fatalf("expected values must name the committed bound: %v", doc.Error.Expected)
		}
	})

	t.Run("unknown task is changed input", func(t *testing.T) {
		c := errorProject(t)
		_, dir := projectPaths(c)
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err = store.Save(&State{Version: 2, Root: c.Root, Todo: c.Todo, Backlog: Backlog{Version: 1, Tasks: []Task{
			{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
		}}}); err != nil {
			t.Fatal(err)
		}
		store.Close()
		code, doc, _ := runV2Error(t, []string{"inspect", "T999", "--contract", "v2", "--json"})
		if code != exitOperationFailed || doc.Error.Code != "task_not_found" {
			t.Fatalf("exit %d code %q", code, doc.Error.Code)
		}
		if doc.Error.Retry != retryAfterChange {
			t.Fatalf("retry %q", doc.Error.Retry)
		}
	})
}

// TestV2CapabilityErrors covers refusals that happen before any request is
// queued: the caller learns the supported set and that nothing was written.
func TestV2CapabilityErrors(t *testing.T) {
	withoutStop := &ControllerCapabilities{ContractVersions: []int{1}, RequestTypes: []string{
		PublicRequestAddTask, PublicRequestAddPlan, PublicRequestSettings, PublicRequestRequeue,
	}}
	withoutAddPlan := &ControllerCapabilities{ContractVersions: []int{1}, RequestTypes: []string{
		PublicRequestAddTask, PublicRequestSettings, PublicRequestRequeue, PublicRequestStop,
	}}

	t.Run("stop against a controller without stop", func(t *testing.T) {
		c := errorProject(t)
		_, dir := projectPaths(c)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "state.db"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		writeControllerMetadata(t, dir, "run-old", withoutStop)
		release := holdRepositoryLock(t, c)
		defer release()
		var out, stderr strings.Builder
		code := runStop(v2Config(c, "stop", true), dir, &out, &stderr)
		if code != exitIncompatible {
			t.Fatalf("exit %d", code)
		}
		var doc errorDocument
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
			t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
		}
		if doc.Error.Code != "capability_unsupported" || doc.Error.Retry != retryAfterChange {
			t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
		}
		if doc.Error.Expected["controller_request_types"] == nil || doc.Error.Observed["requested_request_type"] != PublicRequestStop {
			t.Fatalf("expected %v observed %v", doc.Error.Expected, doc.Error.Observed)
		}
		if _, err := os.Stat(stopRequestPath(dir)); err == nil {
			t.Fatal("no stop request may be written")
		}
	})

	t.Run("add-plan against a controller without add-plan", func(t *testing.T) {
		c := errorProject(t)
		_, dir := projectPaths(c)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		writeControllerMetadata(t, dir, "run-old", withoutAddPlan)
		release := holdRepositoryLock(t, c)
		defer release()
		plan := filepath.Join(c.Root, "plan.md")
		if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr strings.Builder
		cc := v2Config(c, "add-plan", true)
		cc.Plans = []string{plan}
		code := runAddPlan(cc, &out, &stderr)
		if code != exitIncompatible {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		var doc errorDocument
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
			t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
		}
		if doc.Error.Code != "capability_unsupported" {
			t.Fatalf("code %q", doc.Error.Code)
		}
		entries, _ := os.ReadDir(requestDir(dir))
		if len(entries) > 0 {
			t.Fatalf("no envelope may be queued, found %d", len(entries))
		}
	})
}

// TestV2InputErrorsOmitInputContents proves malformed and unreadable input
// is rejected without echoing file contents into the error document.
func TestV2InputErrorsOmitInputContents(t *testing.T) {
	c := errorProject(t)
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)
	defer release()

	// A malformed tasks file whose contents must not leak into the failure.
	tasks := filepath.Join(c.Root, "tasks.json")
	marker := "MARKER-VALUE-must-not-leak-9q7"
	if err := os.WriteFile(tasks, []byte(`{"id":"`+marker+`","title":"nine" "broken"`), 0600); err != nil {
		t.Fatal(err)
	}
	cc := v2Config(c, "add-task", true)
	cc.Plans = []string{tasks}
	var out, stderr strings.Builder
	code := runAddTask(cc, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "input_malformed" || doc.Error.Retry != retryAfterChange {
		t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
	}
	if strings.Contains(out.String(), marker) {
		t.Fatalf("the error document echoes input contents: %s", out.String())
	}
	if doc.Error.Paths[0] != tasks {
		t.Fatalf("paths %v", doc.Error.Paths)
	}

	// An oversized plan reports the bound without its contents.
	big := filepath.Join(c.Root, "big.md")
	payload := strings.Repeat("a", maxPlanBytes+1)
	if err := os.WriteFile(big, []byte(payload), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	stderr.Reset()
	pc := v2Config(c, "add-plan", true)
	pc.Plans = []string{big}
	code = runAddPlan(pc, &out, &stderr)
	if code != exitUsage {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	doc = errorDocument{}
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "input_malformed" {
		t.Fatalf("code %q", doc.Error.Code)
	}
	if doc.Error.Expected["combined_plan_bytes"] != float64(maxPlanBytes) {
		t.Fatalf("expected %v", doc.Error.Expected)
	}
	if strings.Contains(out.String(), "aaaa") {
		t.Fatal("the error document echoes plan contents")
	}
}

// TestV2ConflictError covers a controller refusal the caller can only fix by
// changing the world: the request identity and decision stay attached.
func TestV2ConflictError(t *testing.T) {
	c := errorProject(t)
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
	}}); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	// The saved export digest no longer matches the file, so the controller
	// answers every add-task request with a conflict.
	e.State = &State{Version: 2, Root: c.Root, Todo: c.Todo, Head: "h", Branch: "main",
		Tip: "t", ExportHash: "stale-digest", Backlog: Backlog{Version: 1, Tasks: []Task{
			{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		}}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
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
	cc.Wait = true
	var out, stderr strings.Builder
	code := runAddTask(cc, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc errorDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "request_conflict" || doc.Error.Retry != retryAfterChange {
		t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
	}
	if doc.Error.Observed["controller_decision"] != AckConflict || doc.Error.Observed["request_id"] == nil {
		t.Fatalf("observed %v", doc.Error.Observed)
	}
}

// TestV2PendingRequestExit covers a bounded wait that ends without an
// answer: the transport reports a timeout, never a cancellation.
func TestV2PendingRequestExit(t *testing.T) {
	c := errorProject(t)
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeControllerMetadata(t, dir, "run-live", newControllerCapabilities())
	release := holdRepositoryLock(t, c)
	defer release()
	plan := filepath.Join(c.Root, "plan.md")
	if err := os.WriteFile(plan, []byte("# Needs\nAdd one counter.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldWait := planImportWaitJSON
	planImportWaitJSON = 200 * time.Millisecond
	defer func() { planImportWaitJSON = oldWait }()

	cc := v2Config(c, "add-plan", true)
	cc.Plans = []string{plan}
	// The wait is explicit (AX-011): the immediate default returns the
	// receipt, and --wait is what can time out.
	cc.Wait = true
	var out, stderr strings.Builder
	code := runAddPlan(cc, &out, &stderr)
	if code != exitWaitTimeout {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "request_pending" || doc.Error.Retry != retryYes {
		t.Fatalf("code %q retry %q", doc.Error.Code, doc.Error.Retry)
	}
	if doc.Error.Observed["request_id"] == nil {
		t.Fatalf("observed %v", doc.Error.Observed)
	}
	// The envelope survives: a timeout must not cancel or reject the import.
	entries, _ := os.ReadDir(requestDir(dir))
	if len(entries) != 1 {
		t.Fatalf("the import envelope must survive the timeout, found %d", len(entries))
	}
}

// TestV2RetentionError covers the retained-work guard at startup: the error
// carries the saved and observed digests, names the retained work, and says
// which evidence is unknown.
func TestV2RetentionError(t *testing.T) {
	c := errorProject(t)
	tasks := []Task{{ID: "T001", Title: "one", Priority: 1, Status: "doing"}}
	if err := writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	saved := digestFile(c.Todo)
	if err = store.Save(&State{
		Version: 2, Root: c.Root, Todo: c.Todo, Head: "head", Branch: "main",
		Ref: "refs/heads/main", Tip: "tip", ExportHash: saved,
		Backlog:  Backlog{Version: 1, Tasks: tasks},
		Attempts: []*Attempt{{ID: "a1", Task: tasks[0], Phase: "running", Worktrees: []string{}}},
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	// An external edit invalidates the saved contract while work is retained.
	tasks[0].Title = "edited outside Looper"
	if err = writeJSON(c.Todo, Backlog{Version: 1, Sources: []string{}, Tasks: tasks}); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	err = e.initializeState(context.Background())
	if err == nil {
		t.Fatal("the changed backlog must stop startup")
	}
	var retained *retentionError
	if !errors.As(err, &retained) {
		t.Fatalf("the guard must be a retention error: %v", err)
	}
	p := publicErrorFor(err)
	if p.Code != "backlog_contract_changed" || p.Retry != retryAfterChange {
		t.Fatalf("code %q retry %q", p.Code, p.Retry)
	}
	if p.Expected["task_file_digest"] != saved {
		t.Fatalf("expected %v", p.Expected)
	}
	if p.Observed["task_file_digest"] == saved {
		t.Fatalf("observed %v", p.Observed)
	}
	if len(p.UnknownEvidence) == 0 {
		t.Fatal("the guard must name which evidence is unknown")
	}
	// The same failure renders through the CLI emission path.
	var out, stderr strings.Builder
	cc := v2Config(c, "run", true)
	if code := emitError(cc, err, &out, &stderr); code != exitOperationFailed {
		t.Fatalf("exit %d", code)
	}
	var doc errorDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Error.Code != "backlog_contract_changed" || doc.Command != "looper run --contract v2" {
		t.Fatalf("code %q command %q", doc.Error.Code, doc.Command)
	}
	data, _ := json.Marshal(doc)
	if err = contractschema.ValidateJSON(errorSchema(t), data); err != nil {
		t.Fatalf("the emitted error violates its schema: %v\n%s", err, data)
	}
}

// TestV2PlainErrorsRenderFromTheSameData checks the plain rendering a
// terminal caller sees: code, retry guidance, and the next action, with the
// machine exit code.
func TestV2PlainErrorsRenderFromTheSameData(t *testing.T) {
	errorProject(t)
	var out, stderr strings.Builder
	code := Main([]string{"status", "--contract", "v2"}, &out, &stderr)
	if code != exitUnavailable {
		t.Fatalf("exit %d", code)
	}
	if out.Len() > 0 {
		t.Fatalf("a plain failure writes no machine document: %q", out.String())
	}
	for _, want := range []string{"Error:", "Code: run_not_found", "retry: after-change", "Next:"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("plain rendering lacks %q:\n%s", want, stderr.String())
		}
	}
}

// TestLegacyFailureBehaviorIsUnchanged pins the pre-contract rendering and
// exit codes for representative failures: plain stderr text and exit codes 1
// and 2 only.
func TestLegacyFailureBehaviorIsUnchanged(t *testing.T) {
	t.Run("status without a run", func(t *testing.T) {
		errorProject(t)
		var out, stderr strings.Builder
		code := Main([]string{"status"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d", code)
		}
		if out.Len() > 0 {
			t.Fatalf("stdout must stay empty: %q", out.String())
		}
		if !strings.Contains(stderr.String(), "Error: no saved run; start looper first") {
			t.Fatalf("stderr %q", stderr.String())
		}
	})
	t.Run("stop without a run", func(t *testing.T) {
		errorProject(t)
		var out, stderr strings.Builder
		code := Main([]string{"stop"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d", code)
		}
		if !strings.Contains(stderr.String(), "No saved Looper run exists in this project.") {
			t.Fatalf("stderr %q", stderr.String())
		}
	})
	t.Run("add-plan without a run keeps its guidance", func(t *testing.T) {
		errorProject(t)
		var out, stderr strings.Builder
		code := Main([]string{"add-plan", "plan.md"}, &out, &stderr)
		if code != exitOperationFailed {
			t.Fatalf("exit %d", code)
		}
		for _, want := range []string{"No active run", "looper --plan"} {
			if !strings.Contains(stderr.String(), want) {
				t.Fatalf("stderr lacks %q: %q", want, stderr.String())
			}
		}
	})
	t.Run("usage errors keep exit 2", func(t *testing.T) {
		errorProject(t)
		var out, stderr strings.Builder
		if code := Main([]string{"status", "--frobnicate"}, &out, &stderr); code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		if code := Main([]string{"add-plan"}, &out, &stderr); code != exitUsage {
			t.Fatalf("exit %d", code)
		}
		if code := Main([]string{"events", "--after", "x"}, &out, &stderr); code != exitUsage {
			t.Fatalf("exit %d", code)
		}
	})
}

// TestV2SuccessKeepsLegacyDocument pins the documented transition: version 2
// success documents ship per command, and a command that opts in serves its
// own document while the default contract keeps the legacy one byte for byte.
// Inspect served its version 2 document in AX-005 and doctor in AX-010; the
// default contract keeps every legacy document.
func TestV2SuccessKeepsLegacyDocument(t *testing.T) {
	preparedRepo(t, "--once")
	t.Run("inspect serves its version 2 document", func(t *testing.T) {
		var out, stderr strings.Builder
		code := Main([]string{"inspect", "T1", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		var doc struct {
			ContractVersion int    `json:"contract_version"`
			Kind            string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
			t.Fatalf("stdout is not one document: %v\n%s", err, out.String())
		}
		if doc.ContractVersion != ContractVersionV2 || doc.Kind != inspectCompactKind {
			t.Fatalf("document identity: %+v\n%s", doc, out.String())
		}
	})
	t.Run("doctor serves its version 2 document", func(t *testing.T) {
		var out, stderr strings.Builder
		code := Main([]string{"doctor", "--contract", "v2", "--json"}, &out, &stderr)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		var doc struct {
			ContractVersion int    `json:"contract_version"`
			Kind            string `json:"kind"`
		}
		if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
			t.Fatalf("stdout is not one document: %v\n%s", err, out.String())
		}
		if doc.ContractVersion != ContractVersionV2 || doc.Kind != doctorCompactKind {
			t.Fatalf("document identity: %+v\n%s", doc, out.String())
		}
	})
	t.Run("the default contract keeps the legacy doctor document", func(t *testing.T) {
		var out, stderr strings.Builder
		code := Main([]string{"doctor", "--json"}, &out, &stderr)
		if code != exitOK {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if strings.Contains(out.String(), "contract_version") {
			t.Fatalf("the default contract must keep the legacy document:\n%s", out.String())
		}
	})
}

// TestPublicErrorConversionChecks covers the typed failures the engine and
// store return: each converts to the stable code, retry guidance, and exit
// code its callers depend on.
func TestPublicErrorConversionChecks(t *testing.T) {
	t.Run("unsupported request", func(t *testing.T) {
		err := error(&unsupportedRequestError{
			requestType: "cancel-task", session: "run-old",
			supported: controllerRequestTypes(), absentMetadata: true,
		})
		p := publicErrorFor(err)
		if p.Code != "capability_unsupported" || p.ExitCode != exitIncompatible {
			t.Fatalf("code %q exit %d", p.Code, p.ExitCode)
		}
		if len(p.UnknownEvidence) == 0 {
			t.Fatal("absent metadata must name the unknown evidence")
		}
		if p.Error() != err.Error() {
			t.Fatalf("the legacy message changed: %q", p.Error())
		}
	})
	t.Run("stale session", func(t *testing.T) {
		err := &staleSessionError{requestType: PublicRequestSettings, targeted: "run-a", current: "run-b"}
		p := publicErrorFor(err)
		if p.Code != "controller_session_stale" || p.ExitCode != exitIncompatible {
			t.Fatalf("code %q exit %d", p.Code, p.ExitCode)
		}
		if p.Expected["controller_session"] != "run-a" || p.Observed["controller_session"] != "run-b" {
			t.Fatalf("expected %v observed %v", p.Expected, p.Observed)
		}
	})
	t.Run("pending wait", func(t *testing.T) {
		err := &pendingError{message: "request r-1 is still pending", request: "r-1", waitKind: "controller acknowledgement"}
		p := publicErrorFor(err)
		if p.Code != "request_pending" || p.ExitCode != exitWaitTimeout || p.Retry != retryYes {
			t.Fatalf("code %q exit %d retry %q", p.Code, p.ExitCode, p.Retry)
		}
		// The classifier routes a timeout to the same document in every
		// output mode, not only under --json.
		var pending *pendingError
		if classified := requestSubmitError(err); !errors.As(classified, &pending) {
			t.Fatalf("a wait timeout must stay a pending result, got %v", classified)
		}
	})
	t.Run("unrecognized failures keep a code", func(t *testing.T) {
		p := publicErrorFor(fmt.Errorf("something unexpected"))
		if p.Code != "operation_failed" || p.Retry != retryNo || p.ExitCode != exitOperationFailed {
			t.Fatalf("code %q retry %q exit %d", p.Code, p.Retry, p.ExitCode)
		}
	})
	t.Run("bounds are clamped", func(t *testing.T) {
		p := operationFailure("bounded", "message")
		for i := 0; i < maxErrorPaths+10; i++ {
			p.Paths = append(p.Paths, fmt.Sprintf("/long/path/%d", i))
		}
		p.Message = strings.Repeat("x", maxErrorMessage+100)
		doc := p.document("looper status --contract v2")
		if len(doc.Error.Paths) != maxErrorPaths {
			t.Fatalf("paths %d", len(doc.Error.Paths))
		}
		if len(doc.Error.Message) > maxErrorMessage+3 {
			t.Fatalf("message %d", len(doc.Error.Message))
		}
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err = contractschema.ValidateJSON(errorSchema(t), data); err != nil {
			t.Fatalf("the clamped document violates its schema: %v", err)
		}
	})
}

// v2Config rebuilds one parsed config with version 2 selected for a command
// whose failure is under test.
func v2Config(c Config, command string, jsonOutput bool) Config {
	cc := c
	cc.Command = command
	cc.Contract = ContractVersionV2
	cc.JSON = jsonOutput
	return cc
}

// holdRepositoryLock fakes a live controller's repository ownership for
// request paths that validate ownership before acting.
func holdRepositoryLock(t *testing.T, c Config) func() {
	t.Helper()
	common := filepath.Join(c.Root, ".git")
	f, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
