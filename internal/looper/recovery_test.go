package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recoveryEngine(t *testing.T) (*Engine, *Attempt, runRepair) {
	t.Helper()
	e, a := diagnosisEngine(t, diagnosisProposal)
	source := e.Repo.contextPath() + "/bootstrap-fixture/backlog.json"
	if err := writeJSON(filepath.Join(e.Dir, "bootstrap-fixture", "context", "backlog.json"), Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}); err != nil {
		t.Fatal(err)
	}
	e.State.Backlog.Sources = []string{source}
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, Task{ID: "T002", Title: "successor", Priority: 2, Status: "todo", DependsOn: []string{"T001"}, Details: "Run the later model test"})
	a.Phase = "blocked"
	e.State.Backlog.Tasks[0].Status = "blocked"
	e.State.Backlog.Tasks[0].Blockers = []string{"private snapshot missing"}
	e.State.Plan = &Plan{Setup: "cat " + source, Verify: "cargo xtask model"}
	if err := os.WriteFile(filepath.Join(a.Worktree, "retained.txt"), []byte("useful implementation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	p := runRepair{RemoveSources: []string{source}, Setup: "", Verify: "bash -c 'test -f retained.txt'", Reopen: []string{"T001"}, DeferredTasks: []string{"T002"}}
	return e, a, p
}

func proposalJSON(t *testing.T, p runRepair) string {
	t.Helper()
	b, err := json.Marshal(diagnosis{RootCause: "private snapshot and premature gate", Evidence: "retained bootstrap and task requirements", FixApproach: "correct current-stage contracts", RunRepair: &p})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunRecoveryAppliesAndSurvivesRestart(t *testing.T) {
	e, a, p := recoveryEngine(t)
	original := a.Task.Contract()
	t.Setenv("LOOPER_FAKE_RESULT", proposalJSON(t, p))
	ok, err := e.recoverRun(context.Background())
	if err != nil || !ok {
		t.Fatalf("recover: %v %v", ok, err)
	}
	if len(e.State.Backlog.Sources) != 0 || e.State.Backlog.Task("T001").Status != "todo" || e.State.Backlog.Task("T001").Contract() != original {
		t.Fatal("requirements or reopening changed")
	}
	if b, err := os.ReadFile(filepath.Join(a.Worktree, "retained.txt")); err != nil || string(b) != "useful implementation" {
		t.Fatal("retained implementation lost")
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e.State = saved
	plan := Plan{Verify: "cargo xtask model"}
	e.recoveryCommands(&plan)
	if plan.Verify != p.Verify {
		t.Fatal("restart restored premature gate")
	}
	// Recreate the same failure after the retry. The corrected identity must
	// already be consumed even though the original private source is gone.
	e.State.Backlog.Task("T001").Status = "blocked"
	a = e.State.Attempts[0]
	a.Repeats = 2
	a.Feedback = "Combined verification failed"
	if ok, err = e.recoverRun(context.Background()); err != nil || ok {
		t.Fatalf("repeated consultation: %v %v", ok, err)
	}
	if len(e.State.Recoveries) != 2 {
		t.Fatalf("unexpected consultations: %+v", e.State.Recoveries)
	}
}

func TestRunRecoveryRejectsUnsafeProposals(t *testing.T) {
	for _, kind := range []string{"requirement", "checks", "operator", "malformed", "changed-files", "concurrent-edit", "explicit-command"} {
		t.Run(kind, func(t *testing.T) {
			e, a, p := recoveryEngine(t)
			before := hashJSON(e.State.Backlog)
			switch kind {
			case "requirement":
				p.RemoveSources = []string{"SPEC.md"}
			case "checks":
				p.Verify = "true"
			case "operator":
				e.State.Backlog.Task("T001").Tags = []string{operatorTaskTag}
				before = hashJSON(e.State.Backlog)
			case "explicit-command":
				e.Config.Verify = "cargo xtask model"
			}
			raw := proposalJSON(t, p)
			if kind == "malformed" {
				raw = "not JSON"
			}
			if kind == "changed-files" {
				t.Setenv("LOOPER_FAKE_TOUCH", "")
				t.Setenv("LOOPER_FAKE_BACKLOG_EDIT", `{}`)
			}
			if kind == "concurrent-edit" {
				t.Setenv("LOOPER_FAKE_TOUCH", e.Config.Todo)
			}
			t.Setenv("LOOPER_FAKE_RESULT", raw)
			if kind == "concurrent-edit" { // Overwrite the real export from the agent process.
				t.Setenv("LOOPER_FAKE_TOUCH", "")
				t.Setenv("LOOPER_FAKE_BACKLOG_PATH", e.Config.Todo)
				t.Setenv("LOOPER_FAKE_BACKLOG_EDIT", `{"external":"edit"}`)
			}
			ok, err := e.recoverRun(context.Background())
			if err != nil || ok {
				t.Fatalf("unsafe repair accepted: %v %v", ok, err)
			}
			if before != hashJSON(e.State.Backlog) || a.Phase != "blocked" {
				t.Fatal("rejected proposal changed state")
			}
		})
	}
}

func TestRecoveryPreservesUnknownSource(t *testing.T) {
	e, _, _ := recoveryEngine(t)
	path := filepath.Join(e.Dir, "bootstrap-fixture", "context", "backlog.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if id, _ := e.recoveryOpportunity(); id != "" {
		t.Fatal("unreadable source triggered automatic removal")
	}
}

func TestVerificationRetainsChangedPaths(t *testing.T) {
	e, a := diagnosisEngine(t, diagnosisProposal)
	tree, err := e.Repo.Snapshot(a.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{a.Base}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("check"), commit, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	result := e.verify(context.Background(), e.capture(), *a, Candidate{OK: true, Commit: commit, Tree: tree, Worktree: work}, Plan{Verify: "mkdir -p .cache; echo cache > .cache/generated"})
	if result.OK || !strings.Contains(result.Feedback, "A\t.cache/generated") || !strings.Contains(result.Feedback, "command passed") {
		t.Fatalf("missing path evidence: %s", result.Feedback)
	}
	b, err := os.ReadFile(filepath.Join(a.Directory, "checks", commit, "file-changes.txt"))
	if err != nil || !strings.Contains(string(b), ".cache/generated") {
		t.Fatalf("comparison lost: %s %v", b, err)
	}
}

func TestUnattendedRecoveryIntegratesAndDispatchesSuccessor(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprint(interrupted), func(t *testing.T) { testUnattendedRecovery(t, interrupted) })
	}
}

func testUnattendedRecovery(t *testing.T, interrupted bool) {
	e, a, p := recoveryEngine(t)
	// A real shell fixture checks retained content. The worker must keep it and
	// add an ignore rule before the ordinary integration repair can pass.
	script := `#!/bin/bash
set -eu
out=""; prev=""
for arg in "$@"; do
 if [ "$prev" = "--output-last-message" ]; then out="$arg"; fi
 prev="$arg"
done
prompt="$(cat)"
case "$prompt" in
 *"Diagnose a blocked run"*) result="$LOOPER_RECOVERY_PROPOSAL" ;;
 *"Plan execution for Looper"*) result='{"setup":"","verify":"cargo xtask model","tasks":[]}' ;;
 *"Complete task T001"*)
 test "$(cat retained.txt)" = 'useful implementation'
 if grep -R -q 'changed project files' to-do.json.context; then printf '/.cache/\n' > .gitignore; fi
 result='{"task_id":"T001","status":"done","summary":"retained work","files":["retained.txt"],"blockers":[],"followups":[]}' ;;
 *"Complete task T002"*)
 test -f retained.txt
 printf successor > successor.txt
 result='{"task_id":"T002","status":"done","summary":"successor","files":["successor.txt"],"blockers":[],"followups":[]}' ;;
 *"review pass for Looper"*) result='{"status":"reviewed","summary":"fixture complete","findings":[],"complete":true}' ;;
 *) exit 2 ;;
esac
printf '%s' "$result" > "$out"
`
	bin := filepath.Join(t.TempDir(), "agent.sh")
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e.Config.CodexBin = bin
	e.Config.ClaudeBin = bin
	e.Config.Once = true
	e.Config.ReviewEvery = 0
	e.Config.Limit = 2
	e.Agent = Agent{e.Config}
	// Use a check that generates an unignored cache. The first integration must
	// fail with paths, and the task repair must add the ignore rule.
	p.Verify = "bash -c 'test -f retained.txt; mkdir -p .cache; echo cache > .cache/generated'"
	t.Setenv("LOOPER_RECOVERY_PROPOSAL", proposalJSON(t, p))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if interrupted {
		if ok, err := e.recoverRun(ctx); err != nil || !ok {
			t.Fatalf("pre-stop repair: %v %v", ok, err)
		}
		e.State = nil // Simulate loss of all in-memory state after the journal commits.
	}
	code, err := e.Run(ctx)
	if err != nil || code != 0 {
		t.Fatalf("run: code=%d err=%v state=%+v", code, err, e.State.Backlog)
	}
	if e.State.Integrated != 2 || e.State.Backlog.Task("T001").Status != "done" || e.State.Backlog.Task("T002").Status != "done" {
		t.Fatal("no implementation progress")
	}
	if len(e.State.Attempts) != 2 || e.State.Attempts[0].ID != a.ID || e.State.Attempts[0].Repairs != 1 {
		t.Fatalf("retained attempt or ordinary repair lost: %+v", e.State.Attempts)
	}
	if _, err := os.Stat(filepath.Join(e.Config.Root, "successor.txt")); err != nil {
		t.Fatal("successor was not integrated")
	}
}

func TestDiagnosisProposalUsesOneConsultation(t *testing.T) {
	e, a, p := recoveryEngine(t)
	e.State.Backlog.Task("T001").Status = "doing"
	a.Phase = "ready"
	feedback := "Combined verification failed: future command missing"
	a.FailureHash = digest([]byte(feedback))
	a.Repeats = 1
	t.Setenv("LOOPER_FAKE_DIAGNOSIS", proposalJSON(t, p))
	log := filepath.Join(t.TempDir(), "calls")
	t.Setenv("LOOPER_FAKE_LOG", log)
	if err := e.repair(context.Background(), a, feedback, make(chan workerResult, 1)); err != nil {
		t.Fatal(err)
	}
	if a.Phase != "blocked" || e.State.PendingRunRepair == nil {
		t.Fatal("proposal was not queued at a drained boundary")
	}
	ok, err := e.recoverRun(context.Background())
	if err != nil || !ok {
		t.Fatalf("queued proposal: %v %v", ok, err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "\n") != 1 {
		t.Fatalf("extra consultation: %s", b)
	}
}

func TestRecoveryCorrectsEffectiveOverrides(t *testing.T) {
	e, _, p := recoveryEngine(t)
	e.Config.Setup = "cat " + p.RemoveSources[0]
	e.Config.Verify = "cargo xtask model"
	e.State.Backlog.Task("T002").Details = "Acceptance: cargo xtask model passes after implementation."
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_RESULT", proposalJSON(t, p))
	if ok, err := e.recoverRun(context.Background()); err != nil || !ok {
		t.Fatalf("override recovery: %v %v", ok, err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	e.State = saved
	op := e.capture()
	if op.config.Setup != p.Setup || op.config.Verify != p.Verify {
		t.Fatal("effective override restored the bad commands")
	}
	e.Config.Verify = "go test ./..."
	op = e.capture()
	if op.config.Verify != e.Config.Verify || op.config.Setup != p.Setup {
		t.Fatal("new user override did not take precedence")
	}
}

func TestAdministrativeClosureIsNotIntegration(t *testing.T) {
	e, _, p := recoveryEngine(t)
	e.State.Backlog.Tasks = append(e.State.Backlog.Tasks, Task{ID: "ADMIN", Title: "Restore private context", Priority: 2, Status: "blocked", Details: "Restore " + p.RemoveSources[0], Tags: []string{"looper:orchestration"}})
	p.Administrative = []administrativeClosure{{TaskID: "ADMIN", Source: p.RemoveSources[0], Reason: "Empty private snapshot is no longer a project source."}}
	if err := e.export(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_FAKE_RESULT", proposalJSON(t, p))
	if ok, err := e.recoverRun(context.Background()); err != nil || !ok {
		t.Fatalf("closure: %v %v", ok, err)
	}
	if e.State.Backlog.Task("ADMIN").Status != "done" || e.State.Integrated != 0 {
		t.Fatal("administrative closure counted as product progress")
	}
}

func TestExternalPrerequisiteDoesNotConsult(t *testing.T) {
	e, a, _ := recoveryEngine(t)
	e.State.Backlog.Sources = []string{}
	e.State.Backlog.Task("T001").Blockers = []string{"Provider credentials missing; owner decision needed."}
	a.Feedback = "Provider credentials missing"
	a.Repeats = 0
	t.Setenv("LOOPER_FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	for range 3 {
		if ok, err := e.recoverRun(context.Background()); err != nil || ok {
			t.Fatalf("external prerequisite: %v %v", ok, err)
		}
	}
	if len(e.State.Recoveries) != 0 || e.State.Backlog.Task("T001").Status != "blocked" {
		t.Fatal("external prerequisite was retried")
	}
}

func TestVerificationReportsSnapshotCaptureError(t *testing.T) {
	e, a := diagnosisEngine(t, diagnosisProposal)
	tree, err := e.Repo.Snapshot(a.Worktree)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := e.Repo.Commit(tree, []string{a.Base}, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	work, err := e.Repo.Workspace(identifier("check"), commit, e.State.Backlog)
	if err != nil {
		t.Fatal(err)
	}
	result := e.verify(context.Background(), e.capture(), *a, Candidate{OK: true, Commit: commit, Tree: tree, Worktree: work}, Plan{Verify: "printf invalid > .git"})
	if result.OK || !strings.Contains(result.Feedback, "snapshot capture failed:") || strings.Contains(result.Feedback, "but changed project files") {
		t.Fatalf("wrong error: %s", result.Feedback)
	}
}

func TestRecoveryIdentityIgnoresOperationAndFindingNames(t *testing.T) {
	e, _, p := recoveryEngine(t)
	before := e.recoveryIdentity()
	e.addFinding(Finding{Title: "Obtain provider token", Details: "Supply access", Evidence: "401", Owner: "operator", Key: "provider-token"})
	e.addFinding(Finding{Title: "Access remains missing", Details: "Supply access", Evidence: "401 again", Owner: "operator", Key: "provider-token"})
	old := p.RemoveSources[0]
	replacement := strings.Replace(old, "bootstrap-fixture", "bootstrap-renamed", 1)
	if err := writeJSON(filepath.Join(e.Dir, "bootstrap-renamed", "context", "backlog.json"), Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}); err != nil {
		t.Fatal(err)
	}
	e.State.Backlog.Sources = []string{replacement}
	e.State.Plan.Setup = strings.ReplaceAll(e.State.Plan.Setup, old, replacement)
	if after := e.recoveryIdentity(); after != before {
		t.Fatal("equivalent evidence received a new consultation identity")
	}
}

func TestRecoveryRecordsBlockedRetry(t *testing.T) {
	e, a, p := recoveryEngine(t)
	t.Setenv("LOOPER_FAKE_RESULT", proposalJSON(t, p))
	if ok, err := e.recoverRun(context.Background()); err != nil || !ok {
		t.Fatalf("repair: %v %v", ok, err)
	}
	if err := e.hold(a, "The corrected check still fails."); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.Recoveries[0].Outcome != "retry_blocked" || saved.Backlog.Task("T001").Status != "blocked" {
		t.Fatal("retry outcome was not saved with the task result")
	}
}
