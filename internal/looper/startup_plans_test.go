package looper

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
