package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
)

// missingPlan returns a path that never exists, for rejected-input fixtures.
func missingPlan(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "missing-plan.md")
}

// TestInitializeRejectsInvalidStartupPlans pins the T045 startup gate:
// unreadable startup plans fail initialization before any state work,
// import queueing, planning, or dispatch can start.
func TestInitializeRejectsInvalidStartupPlans(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{missingPlan(t)}
	err := e.initialize(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot read plan") {
		t.Fatalf("invalid startup plans must fail initialization: %v", err)
	}
	var n int
	if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE kind='import-error' AND message LIKE '%cannot read plan%'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the rejection must record one import-error event, got %d", n)
	}
}

// TestStartupImportCaptureFailurePropagates pins the capture boundary in
// startupImportPlans: a failure there becomes the controller error instead of
// a log line, and nothing is queued.
func TestStartupImportCaptureFailurePropagates(t *testing.T) {
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{missingPlan(t)}
	e.startupImportPlans()
	if e.err == nil || !strings.Contains(e.err.Error(), "cannot read plan") {
		t.Fatalf("capture failure must become the controller error: %v", e.err)
	}
	if len(e.State.Imports) != 0 {
		t.Fatal("a failed capture must not queue an import")
	}
}

// TestStartupPlanValidationCoversEveryFile pins that one invalid path rejects
// the whole invocation even when other supplied files are valid.
func TestStartupPlanValidationCoversEveryFile(t *testing.T) {
	valid := planFile(t, "Add telemetry")
	missing := missingPlan(t)
	if err := validatePlanFiles([]string{valid, missing}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("one invalid file must reject the invocation: %v", err)
	}
	e, _ := importEngine(t, additionProposal)
	e.Config.Plans = []string{valid, missing}
	if err := e.initialize(context.Background()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("mixed startup plans must fail initialization: %v", err)
	}
	if len(e.State.Imports) != 0 {
		t.Fatal("a rejected invocation must not queue an import")
	}
	if _, err := os.Stat(filepath.Join(importsDir(e.Dir))); !os.IsNotExist(err) {
		t.Fatal("a rejected invocation must not capture artifacts")
	}
}

// TestRunRejectsInvalidStartupPlansBeforeWork pins the whole-controller
// behavior required by docs/UI-PLAN.md Part 7: a run with invalid startup
// plan input exits nonzero before planning, worker dispatch, or publication,
// and leaves the backlog untouched.
func TestRunRejectsInvalidStartupPlansBeforeWork(t *testing.T) {
	bin, _ := fakeAgent(t, additionProposal)
	calls := filepath.Join(t.TempDir(), "agent.log")
	t.Setenv("LOOPER_FAKE_LOG", calls)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.Once = true
	if err := os.WriteFile(filepath.Join(c.Root, "seed.txt"), []byte("original\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "seed.txt"}, {"commit", "-qm", "fixture"}} {
		if _, err := gitText(c.Root, args...); err != nil {
			t.Fatal(err)
		}
	}
	head, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "write files", Tasks: []Task{
		{ID: "T1", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T2", Title: "two", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	before := digestFile(c.Todo)
	c.Plans = []string{missingPlan(t)}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	code, err := e.Run(context.Background())
	if code != 1 || err == nil {
		t.Fatalf("invalid startup plans must fail the run: code %d, err %v", code, err)
	}
	if !strings.Contains(err.Error(), "cannot read plan") {
		t.Fatalf("the failure must name the unreadable plan: %v", err)
	}
	// No agent ran: neither planning nor any worker was invoked.
	if raw, _ := os.ReadFile(calls); len(raw) != 0 {
		t.Fatalf("the run must stop before agent calls, got %q", raw)
	}
	// No publication happened: the accepted head and worktree are unchanged.
	after, err := gitText(c.Root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if after != head {
		t.Fatalf("invalid startup plans must not publish: %s -> %s", head, after)
	}
	for _, name := range []string{"T1.txt", "T2.txt"} {
		if _, err := os.Stat(filepath.Join(c.Root, name)); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist before work starts: %v", name, err)
		}
	}
	if digestFile(c.Todo) != before {
		t.Fatal("the existing backlog must be preserved")
	}
	for _, kind := range []string{"start", "plan", "check", "finish"} {
		var n int
		if err := e.Store.db.QueryRow("SELECT count(*) FROM events WHERE kind=?", kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("a %q event must not exist before work starts", kind)
		}
	}
	if exit := lastShutdown(t, e.Store); exit.Reason != "error" || exit.ExitCode != 1 {
		t.Fatalf("the session must record the startup error exit: %+v", exit)
	}
}

// TestBootstrapRejectsInvalidPlans keeps the fresh-project path pinned: an
// unreadable plan fails before bootstrap creates a backlog or calls an agent.
func TestBootstrapRejectsInvalidPlans(t *testing.T) {
	bin, _ := fakeAgent(t, additionProposal)
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin = bin, bin
	c.Plans = []string{missingPlan(t)}
	if _, err := os.Stat(c.Todo); !os.IsNotExist(err) {
		t.Fatal("test expects a fresh project")
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	defer e.Store.Close()
	if err = e.initialize(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot read plan") {
		t.Fatalf("fresh projects must reject unreadable plans: %v", err)
	}
	if _, err = os.Stat(c.Todo); !os.IsNotExist(err) {
		t.Fatal("a rejected invocation must not create a backlog")
	}
}

// TestStartDetachedRejectsInvalidPlans pins the parent-side gate: an invalid
// plan fails the detached start without spawning a child that would die at
// initialization after reporting readiness.
func TestStartDetachedRejectsInvalidPlans(t *testing.T) {
	c := preparedRepo(t, "start")
	c.Plans = []string{missingPlan(t)}
	orig := startExecutable
	startExecutable = func() (string, error) { return "/nonexistent-detached-child", nil }
	t.Cleanup(func() { startExecutable = orig })
	var out, stderr bytes.Buffer
	code := StartDetached(c, &out, &stderr)
	if code != 1 {
		t.Fatalf("invalid plans must fail the detached start: %d %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot read plan") {
		t.Fatalf("rejection missing from stderr: %q", stderr.String())
	}
	if out.Len() != 0 {
		t.Fatalf("a rejected start must not report a detached controller: %q", out.String())
	}
}

// The double-start refusal both launch paths share, byte for byte: callers
// that read the default contract learn the plans were not submitted and the
// command that submits them.
const ownedRepositoryRefusal = "A controller already owns this repository.\nSubmit the plans with: looper add-plan <path>\n"

// TestStartPlanUnderLiveControllerSuggestsAddPlan pins the T123 rule from
// docs/UI-PLAN.md: `start --plan` against a live controller refuses the
// invocation with the same add-plan suggestion the run path emits, instead of
// dropping the plans behind a detached child that cannot take the lock.
func TestStartPlanUnderLiveControllerSuggestsAddPlan(t *testing.T) {
	t.Setenv("LOOPER_NO_PROMPT", "1")
	c := preparedRepo(t, "start")
	c.Plans = []string{planFile(t, "# Needs\nAdd one counter.\n")}
	// The guard must act before a child exists, so an executable that cannot
	// run stands in for the spawn: reaching it would fail differently.
	orig := startExecutable
	startExecutable = func() (string, error) { return "/nonexistent-detached-child", nil }
	t.Cleanup(func() { startExecutable = orig })
	release := holdRepositoryLock(t, c)
	defer release()
	var out, stderr bytes.Buffer
	code := runCLIController([]string{"start", "--plan", c.Plans[0]}, c, &out, &stderr)
	if code != 1 {
		t.Fatalf("the double start must fail: %d %s %s", code, out.String(), stderr.String())
	}
	if stderr.String() != ownedRepositoryRefusal {
		t.Fatalf("the refusal must match the run path's suggestion: %q", stderr.String())
	}
	if strings.Contains(out.String(), "Controller detached") {
		t.Fatalf("no controller may be reported started: %q", out.String())
	}
	_, dir := projectPaths(c)
	if _, err := os.Stat(filepath.Join(dir, "daemon-config.json")); err == nil {
		t.Fatal("the refusal must come before the daemon child is configured")
	}
}

// TestRunPlanUnderLiveControllerSuggestsAddPlan keeps the foreground half of
// the shared refusal pinned: the same text, with the launch description still
// preceding it (AX-023).
func TestRunPlanUnderLiveControllerSuggestsAddPlan(t *testing.T) {
	t.Setenv("LOOPER_NO_PROMPT", "1")
	c := preparedRepo(t, "run")
	c.Plans = []string{planFile(t, "# Needs\nAdd one counter.\n")}
	release := holdRepositoryLock(t, c)
	defer release()
	var out, stderr bytes.Buffer
	code := runCLIController([]string{"run", "--plan", c.Plans[0]}, c, &out, &stderr)
	if code != 1 {
		t.Fatalf("the double start must fail: %d %s %s", code, out.String(), stderr.String())
	}
	if stderr.String() != ownedRepositoryRefusal {
		t.Fatalf("the run path's refusal must stay unchanged: %q", stderr.String())
	}
	if !strings.Contains(out.String(), "Launch: agent") {
		t.Fatalf("the run path keeps its launch description first: %q", out.String())
	}
}

// TestStartPlanUnderLiveControllerServesStructuredRefusal checks the same
// refusal under contract version 2: exactly one error document, valid against
// the published schema, whose action carries the add-plan command.
func TestStartPlanUnderLiveControllerServesStructuredRefusal(t *testing.T) {
	t.Setenv("LOOPER_NO_PROMPT", "1")
	c := preparedRepo(t, "start")
	plan := planFile(t, "# Needs\nAdd one counter.\n")
	orig := startExecutable
	startExecutable = func() (string, error) { return "/nonexistent-detached-child", nil }
	t.Cleanup(func() { startExecutable = orig })
	release := holdRepositoryLock(t, c)
	defer release()
	cc := v2Config(c, "start", true)
	cc.Plans = []string{plan}
	var out, stderr bytes.Buffer
	code := runCLIController([]string{"start", "--plan", plan}, cc, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("exit %d: %s %s", code, out.String(), stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("a version 2 JSON error must stay one document on stdout: %q", stderr.String())
	}
	var doc errorDocument
	if err := json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("stdout is not one error document: %v\n%s", err, out.String())
	}
	if doc.Command != "looper start --contract v2" {
		t.Fatalf("command label %q", doc.Command)
	}
	if doc.Error.Code != "controller_owns_repository" {
		t.Fatalf("error code %q", doc.Error.Code)
	}
	if len(doc.Error.NextActions) != 1 || doc.Error.NextActions[0].Command != "looper add-plan <path>" {
		t.Fatalf("the action must name the add-plan command: %+v", doc.Error.NextActions)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(errorSchema(t), data); err != nil {
		t.Fatalf("the refusal violates the error schema: %v\n%s", err, data)
	}
}

// TestStartWithoutPlansStillDispatchesToDetachedStart pins the unchanged half
// of the rule: without startup plans a double start still reaches the detached
// child, and no add-plan suggestion appears.
func TestStartWithoutPlansStillDispatchesToDetachedStart(t *testing.T) {
	t.Setenv("LOOPER_NO_PROMPT", "1")
	c := preparedRepo(t, "start")
	orig := startExecutable
	startExecutable = func() (string, error) { return "/nonexistent-detached-child", nil }
	t.Cleanup(func() { startExecutable = orig })
	release := holdRepositoryLock(t, c)
	defer release()
	var out, stderr bytes.Buffer
	code := runCLIController([]string{"start"}, c, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("the double start must keep failing: %d %s %s", code, out.String(), stderr.String())
	}
	combined := out.String() + stderr.String()
	if strings.Contains(combined, "add-plan") {
		t.Fatalf("a start without plans must not suggest add-plan: %q %q", out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "/nonexistent-detached-child") {
		t.Fatalf("the start must still dispatch to the detached child: %q", stderr.String())
	}
}
